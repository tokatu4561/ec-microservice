package main

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// 既存の2件競合と学習2の20件競合を、同じ実際の注文処理で確認する。
func TestConcurrentOrdersPostgres(t *testing.T) {
	for _, tc := range []struct{ stock, orders int }{{1, 2}, {10, 20}} {
		t.Run(fmt.Sprintf("stock_%d_orders_%d", tc.stock, tc.orders), func(t *testing.T) {
			s, productID := orderFixture(t, tc.stock)
			start := make(chan struct{})
			var ready sync.WaitGroup
			ready.Add(tc.orders)
			type result struct {
				order    Order
				err      error
				duration time.Duration
			}
			results := make(chan result, tc.orders)
			for range tc.orders {
				go func() {
					orderID := fmt.Sprintf("%x", randomID())
					ready.Done()
					<-start
					ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
					defer cancel()
					began := time.Now()
					o, err := s.CreateOrder(ctx, orderID, OrderInput{ProductID: productID, Quantity: 1})
					results <- result{o, err, time.Since(began)}
				}()
			}
			ready.Wait()
			began := time.Now()
			close(start)
			success, failed := 0, 0
			var minDuration, maxDuration time.Duration
			seen := make(map[string]bool)
			// 途中のエラーでも全結果を回収し、DBの後片付けと注文処理を競合させない。
			for i := range tc.orders {
				r := <-results
				if i == 0 || r.duration < minDuration {
					minDuration = r.duration
				}
				maxDuration = max(maxDuration, r.duration)
				if r.err != nil {
					t.Errorf("CreateOrder: %v", r.err)
					continue
				}
				if seen[r.order.ID] || !orderIDPattern.MatchString(r.order.ID) {
					t.Errorf("duplicate or invalid order ID: %q", r.order.ID)
				}
				seen[r.order.ID] = true
				switch {
				case r.order.Status == "shipping_requested" && r.order.FailureReason == nil:
					success++
				case r.order.Status == "failed" && r.order.FailureReason != nil && *r.order.FailureReason == "out_of_stock":
					failed++
				default:
					t.Errorf("unexpected order outcome: %+v", r.order)
				}
			}
			elapsed := time.Since(began)
			if t.Failed() {
				t.FailNow()
			}
			if success != tc.stock || failed != tc.orders-tc.stock {
				t.Fatalf("success=%d out_of_stock=%d; want %d/%d", success, failed, tc.stock, tc.orders-tc.stock)
			}
			checkStock(t, s, productID, 0)
			// 戻り値だけでなく、保存済みの注文・明細・成功数量をDBで照合する。
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			var saved, items, savedSuccess, savedFailed, sold int
			err := s.pool.QueryRow(ctx, `
				SELECT count(DISTINCT o.id), count(*),
				       count(*) FILTER (WHERE o.status='shipping_requested' AND o.failure_reason IS NULL),
				       count(*) FILTER (WHERE o.status='failed' AND o.failure_reason='out_of_stock'),
				       COALESCE(sum(i.quantity) FILTER (WHERE o.status='shipping_requested'),0)
				FROM orders o JOIN order_items i ON i.order_id=o.id WHERE i.product_id=$1`, productID).
				Scan(&saved, &items, &savedSuccess, &savedFailed, &sold)
			if err != nil {
				t.Fatal(err)
			}
			if saved != tc.orders || items != tc.orders || savedSuccess != success || savedFailed != failed || sold != tc.stock {
				t.Fatalf("saved=%d items=%d success=%d out_of_stock=%d sold=%d", saved, items, savedSuccess, savedFailed, sold)
			}
			t.Logf("initial_stock=%d concurrent_orders=%d quantity=1 pool_max_conns=%d success=%d out_of_stock=%d remaining_stock=0 saved_orders=%d sold_quantity=%d batch_elapsed=%s order_total_min=%s order_total_max=%s",
				tc.stock, tc.orders, s.pool.Config().MaxConns, success, failed, saved, sold, elapsed, minDuration, maxDuration)
		})
	}
}

func TestOrderLockWaitPostgres(t *testing.T) {
	s, productID := orderFixture(t, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// 制御用と観測用は注文プールの外に接続し、観測がプール待ちで止まるのを避ける。
	holder, err := pgx.ConnectConfig(ctx, s.pool.Config().ConnConfig.Copy())
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close(ctx)
	observer, err := pgx.ConnectConfig(ctx, s.pool.Config().ConnConfig.Copy())
	if err != nil {
		t.Fatal(err)
	}
	defer observer.Close(ctx)
	tx, err := holder.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer rollback(tx)
	var stock int
	if err := tx.QueryRow(ctx, `SELECT stock FROM products WHERE id=$1 FOR UPDATE`, productID).Scan(&stock); err != nil {
		t.Fatal(err)
	}
	holderPID := holder.PgConn().PID()
	orderID := fmt.Sprintf("%x", randomID())
	type result struct {
		order    Order
		err      error
		duration time.Duration
	}
	results := make(chan result, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		began := time.Now()
		o, err := s.CreateOrder(ctx, orderID, OrderInput{ProductID: productID, Quantity: 1})
		results <- result{o, err, time.Since(began)}
	}()
	defer func() {
		cancel()
		<-done
	}()

	// 「一定時間寝たから待っているはず」と推測せず、DBの待機状態と阻害元を確認する。
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		var waitingPID int
		var waitEvent, query string
		err := observer.QueryRow(ctx, `
			SELECT pid, wait_event, query FROM pg_stat_activity
			WHERE datname=current_database() AND wait_event_type='Lock'
			  AND $1::integer = ANY(pg_blocking_pids(pid))
			ORDER BY pid LIMIT 1`, holderPID).Scan(&waitingPID, &waitEvent, &query)
		if err == nil {
			t.Logf("lock_wait_observed=true holder_pid=%d waiting_pid=%d wait_event_type=Lock wait_event=%s query=%q", holderPID, waitingPID, waitEvent, query)
			break
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			t.Fatal(err)
		}
		select {
		case r := <-results:
			t.Fatalf("order finished before lock release: order=%+v err=%v", r.order, r.err)
		case <-ctx.Done():
			t.Fatal("lock wait was not observed: ", ctx.Err())
		case <-ticker.C:
		}
	}

	const holdAfterObservation = 200 * time.Millisecond
	timer := time.NewTimer(holdAfterObservation)
	defer timer.Stop()
	select {
	case r := <-results:
		t.Fatalf("order finished while lock was held: order=%+v err=%v", r.order, r.err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	case <-timer.C:
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	r := <-results
	if r.err != nil || r.order.ID != orderID || r.order.Status != "shipping_requested" {
		t.Fatalf("order after lock release: %+v err=%v", r.order, r.err)
	}
	checkStock(t, s, productID, 0)
	saved, err := s.GetOrder(ctx, orderID)
	if err != nil || saved.Status != "shipping_requested" || len(saved.Items) != 1 || saved.Items[0].Quantity != 1 {
		t.Fatalf("saved order=%+v err=%v", saved, err)
	}
	t.Logf("hold_after_observation=%s order_total=%s success=1 remaining_stock=0 saved_orders=1 (order_total includes pool acquisition, SQL, lock wait and commit)", holdAfterObservation, r.duration)
}
