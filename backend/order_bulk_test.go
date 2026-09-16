package main

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// まとめたSQLでも明細の対応・価格・各業務結果が旧実装と等しいことを検証する。
func TestBulkOrderParityPostgres(t *testing.T) {
	for _, n := range []int{1, 20, 100} {
		t.Run(fmt.Sprintf("items_%d", n), func(t *testing.T) {
			s, first := orderFixture(t, 100)
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			ids := []int64{first}
			for i := 1; i < n; i++ {
				var id int64
				if err := s.pool.QueryRow(ctx, `INSERT INTO products(name,price_yen,stock) VALUES($1,$2,100) RETURNING id`, fmt.Sprintf("商品%d", i), 500+i).Scan(&id); err != nil {
					t.Fatal(err)
				}
				ids = append(ids, id)
			}
			t.Cleanup(func() {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				if _, err := s.pool.Exec(ctx, `DELETE FROM orders WHERE id IN (SELECT order_id FROM order_items WHERE product_id=ANY($1::bigint[]))`, ids); err != nil {
					t.Error(err)
				}
				if _, err := s.pool.Exec(ctx, `DELETE FROM products WHERE id=ANY($1::bigint[])`, ids[1:]); err != nil {
					t.Error(err)
				}
			})
			for _, mode := range []string{"success", "shortage", "payment", "shipping"} {
				t.Run(mode, func(t *testing.T) {
					items := make([]ItemInput, n)
					for i, id := range ids {
						items[n-1-i] = ItemInput{id, i%3 + 1}
					}
					if mode == "shortage" {
						items[0].Quantity = 101
					}
					payment, shipping := "success", "success"
					if mode == "payment" {
						payment = "fail"
					}
					if mode == "shipping" {
						shipping = "fail"
					}
					var want Order
					var wantStocks []int
					for i, create := range []func(context.Context, pgx.Tx, string, []ItemInput, string, string) (Order, error){createOrderTxBaseline, createOrderTx} {
						if _, err := s.pool.Exec(ctx, `UPDATE products SET stock=100 WHERE id=ANY($1::bigint[])`, ids); err != nil {
							t.Fatal(err)
						}
						tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
						if err != nil {
							t.Fatal(err)
						}
						id := fmt.Sprintf("%x", randomID())
						o, err := create(ctx, tx, id, items, payment, shipping)
						if err != nil {
							rollback(tx)
							t.Fatal(err)
						}
						if err := tx.Commit(ctx); err != nil {
							rollback(tx)
							t.Fatal(err)
						}
						saved, err := s.GetOrder(ctx, id)
						if err != nil || !reflect.DeepEqual(saved, o) {
							t.Fatalf("saved=%+v response=%+v err=%v", saved, o, err)
						}
						o.ID, o.CreatedAt = "", time.Time{}
						stocks := make([]int, n)
						for j, id := range ids {
							if err := s.pool.QueryRow(ctx, `SELECT stock FROM products WHERE id=$1`, id).Scan(&stocks[j]); err != nil {
								t.Fatal(err)
							}
						}
						if i == 0 {
							want, wantStocks = o, stocks
						} else if !reflect.DeepEqual(want, o) || !reflect.DeepEqual(wantStocks, stocks) {
							t.Fatalf("bulk behavior differs: baseline=%+v bulk=%+v stock=%v/%v", want, o, wantStocks, stocks)
						}
					}
				})
			}
		})
	}
}

func TestBulkOrderInvalidPostgres(t *testing.T) {
	s, id := orderFixture(t, 3)
	for _, tc := range []struct {
		name  string
		items []ItemInput
		want  error
	}{
		{"duplicate", []ItemInput{{id, 1}, {id, 1}}, errInvalid},
		{"missing", []ItemInput{{id, 1}, {9223372036854775807, 1}}, errNotFound},
		{"quantity_overflow", []ItemInput{{id, 2147483648}}, errInvalid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			tx, err := s.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer rollback(tx)
			_, err = createOrderTx(ctx, tx, fmt.Sprintf("%x", randomID()), tc.items, "success", "success")
			if !errors.Is(err, tc.want) {
				t.Fatalf("got %v want %v", err, tc.want)
			}
			rollback(tx)
			checkStock(t, s, id, 3)
		})
	}
}
