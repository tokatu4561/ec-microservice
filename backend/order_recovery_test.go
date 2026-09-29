package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// 実HTTPが待機中でも、1接続しかないOrder poolを別注文が利用できることを確認。
func TestPaymentWaitReleasesConnectionAndProductPostgres(t *testing.T) {
	s, product := orderFixture(t, 3)
	config := s.pool.Config().Copy()
	config.MaxConns = 1
	pool, err := pgxpool.NewWithConfig(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	firstID := fmt.Sprintf("%x", randomID())
	entered, release := make(chan struct{}), make(chan struct{})
	base := os.Getenv("TEST_PAYMENT_URL")
	if base == "" {
		t.Fatal("TEST_PAYMENT_URL required")
	}
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" && strings.HasSuffix(r.URL.Path, firstID) {
			close(entered)
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
		}
		req, e := http.NewRequestWithContext(r.Context(), r.Method, base+r.URL.Path, r.Body)
		if e != nil {
			t.Error(e)
			w.WriteHeader(502)
			return
		}
		req.Header = r.Header.Clone()
		client := http.Client{Timeout: time.Second}
		resp, e := client.Do(req)
		if e != nil {
			t.Error(e)
			w.WriteHeader(502)
			return
		}
		defer resp.Body.Close()
		w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	}))
	defer proxy.Close()
	p, err := newHTTPPayments(proxy.URL)
	if err != nil {
		t.Fatal(err)
	}
	// ロックの検証用ゲートに余裕を与える。標準1秒の検証は別テストで維持。
	p.client.Timeout = 5 * time.Second
	s = postgresStore{pool: pool, payments: p, shipping: simulatedShipping{}, inventory: s.inventory}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, e := s.CreateOrder(ctx, firstID, OrderInput{ProductID: product, Quantity: 1}); done <- e }()
	released := false
	defer func() {
		if !released {
			close(release)
		}
		<-done
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if pool.Stat().AcquiredConns() != 0 {
		t.Fatal("DB connection held during HTTP")
	}
	secondCtx, stop := context.WithTimeout(ctx, time.Second)
	defer stop()
	second, err := s.CreateOrder(secondCtx, fmt.Sprintf("%x", randomID()), OrderInput{ProductID: product, Quantity: 1})
	if err != nil || second.Status != "shipping_requested" {
		t.Fatalf("second order blocked: %+v %v", second, err)
	}
	checkStock(t, s, product, 1)
	close(release)
	released = true
	// doneはdeferで回収するため、結果を同じchannelへ戻す。
	err = <-done
	done <- err
	if err != nil {
		t.Fatal(err)
	}
	t.Log("HTTP gate held; pool_max_conns=1; acquired_conns=0; second order completed before release; stock=1")
}
