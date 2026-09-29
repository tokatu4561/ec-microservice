package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// 実Shipping・Paymentと専用DBを使い、応答喪失後も同じ注文を並行再開する。
func TestShippingRecoveryPostgres(t *testing.T) {
	for _, scenario := range []string{"success", "failed", "stopped", "lost after commit", "malformed after commit", "timeout", "cancel unavailable", "legacy cancel pending"} {
		t.Run(scenario, func(t *testing.T) {
			s, product := orderFixture(t, 5)
			payURL, shipURL := os.Getenv("TEST_PAYMENT_URL"), os.Getenv("TEST_SHIPPING_URL")
			if payURL == "" || shipURL == "" {
				t.Fatal("TEST_PAYMENT_URL and TEST_SHIPPING_URL required")
			}
			pay, err := newHTTPPayments(payURL)
			if err != nil {
				t.Fatal(err)
			}
			s.payments = pay
			direct, err := newHTTPShipping(shipURL)
			if err != nil {
				t.Fatal(err)
			}
			client := http.Client{Timeout: 2 * time.Second}
			proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if scenario == "stopped" {
					w.WriteHeader(503)
					return
				}
				if scenario == "timeout" {
					<-r.Context().Done()
					return
				}
				req, e := http.NewRequestWithContext(r.Context(), r.Method, shipURL+r.URL.Path, r.Body)
				if e != nil {
					t.Error(e)
					w.WriteHeader(502)
					return
				}
				req.Header = r.Header.Clone()
				resp, e := client.Do(req)
				if e != nil {
					t.Error(e)
					w.WriteHeader(502)
					return
				}
				defer resp.Body.Close()
				if r.Method == "POST" && (scenario == "lost after commit" || scenario == "malformed after commit") {
					if scenario == "lost after commit" {
						conn, _, e := w.(http.Hijacker).Hijack()
						if e != nil {
							t.Error(e)
							return
						}
						_ = conn.Close()
						return
					}
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(201)
					_, _ = w.Write([]byte(`{"shipment":`))
					return
				}
				w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
				w.WriteHeader(resp.StatusCode)
				_, _ = io.Copy(w, resp.Body)
			}))
			defer proxy.Close()
			s.shipping, err = newHTTPShipping(proxy.URL)
			if err != nil {
				t.Fatal(err)
			}
			// 決済取消だけが失敗する場合、Shippingへの依頼は繰り返さない。
			if scenario == "cancel unavailable" {
				paymentProxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if strings.HasSuffix(r.URL.Path, "/cancel") {
						w.WriteHeader(503)
						return
					}
					req, e := http.NewRequestWithContext(r.Context(), r.Method, payURL+r.URL.Path, r.Body)
					if e != nil {
						t.Error(e)
						return
					}
					req.Header = r.Header.Clone()
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
				defer paymentProxy.Close()
				s.payments, err = newHTTPPayments(paymentProxy.URL)
				if err != nil {
					t.Fatal(err)
				}
			}
			ctx := context.Background()
			id := fmt.Sprintf("%x", randomID())
			hash := fmt.Sprintf("%x%x", randomID(), randomID())
			t.Cleanup(func() {
				if _, e := s.pool.Exec(context.Background(), `DELETE FROM carts WHERE token_hash=$1`, hash); e != nil {
					t.Error(e)
				}
			})
			cart, err := s.NewCart(ctx, hash)
			if err != nil {
				t.Fatal(err)
			}
			cart, err = s.SetCartItem(ctx, hash, cart.Version, product, 1, false)
			if err != nil {
				t.Fatal(err)
			}
			mode := "success"
			if scenario == "failed" || scenario == "cancel unavailable" || scenario == "legacy cancel pending" {
				mode = "fail"
			}
			if scenario == "legacy cancel pending" {
				tx, e := s.pool.Begin(ctx)
				if e != nil {
					t.Fatal(e)
				}
				_, e = reserveOrderTx(ctx, tx, id, []ItemInput{{ProductID: product, Quantity: 1}}, "success", mode, hash)
				if e != nil {
					rollback(tx)
					t.Fatal(e)
				}
				if _, e = tx.Exec(ctx, `UPDATE orders SET status='cancel_pending',payment_status='succeeded',shipping_status='failed',inventory_status='reserved' WHERE id=$1`, id); e != nil {
					rollback(tx)
					t.Fatal(e)
				}
				if e = tx.Commit(ctx); e != nil {
					t.Fatal(e)
				}
				if _, e = s.pool.Exec(ctx, `UPDATE order_progress SET phase='payment_cancel_pending',stock_held=true WHERE order_id=$1`, id); e != nil {
					t.Fatal(e)
				}
				if _, e = s.inventory.Reserve(ctx, id, []shipmentItem{{product, 1}}); e != nil {
					t.Fatal(e)
				}
				o, e := s.GetOrder(ctx, id)
				if e != nil {
					t.Fatal(e)
				}
				if _, e = pay.Create(ctx, id, o.TotalYen, "success"); e != nil {
					t.Fatal(e)
				}
			} else {
				_, err = s.Checkout(ctx, hash, cart.Version, id, "success", mode)
				if scenario == "success" || scenario == "failed" {
					if err != nil {
						t.Fatal(err)
					}
				} else {
					if err == nil {
						t.Fatal("expected uncertain result")
					}
					pending, e := s.GetOrder(ctx, id)
					if e != nil {
						t.Fatal(e)
					}
					status, ship := "processing", "pending"
					if scenario == "cancel unavailable" {
						status, ship = "cancel_pending", "failed"
					}
					if pending.Status != status || pending.PaymentStatus != "succeeded" || pending.ShippingStatus != ship {
						t.Fatalf("pending %+v", pending)
					}
					checkStock(t, s, product, 4)
					c, e := s.GetCart(ctx, hash)
					if e != nil || c.PendingOrderID == nil || *c.PendingOrderID != id {
						t.Fatal(c, e)
					}
					if result, e := pay.Get(ctx, id, pending.TotalYen); e != nil || result != "succeeded" {
						t.Fatal(result, e)
					}
				}
			}
			// 新しい依存クライアントで復旧。進行状態はDBだけに残る。
			recovered := postgresStore{pool: s.pool, payments: pay, shipping: direct, inventory: s.inventory}
			var wg sync.WaitGroup
			for range 8 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					_, e := recovered.ResumeOrder(ctx, id)
					if e != nil {
						t.Error(e)
					}
				}()
			}
			wg.Wait()
			o, err := recovered.GetOrder(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			want, stock := "shipping_requested", 4
			if mode == "fail" {
				want, stock = "failed", 5
			}
			if o.Status != want {
				t.Fatalf("final %+v", o)
			}
			checkStock(t, s, product, stock)
			if scenario == "legacy cancel pending" {
				_, err = direct.Get(ctx, id, []shipmentItem{{product, 1}}, mode)
				if err != errShippingNotFound {
					t.Fatal("legacy cancellation created shipment", err)
				}
			} else {
				result, e := direct.Get(ctx, id, []shipmentItem{{product, 1}}, mode)
				want := "requested"
				if mode == "fail" {
					want = "failed"
				}
				if e != nil || result != want {
					t.Fatal(result, e)
				}
				c, e := s.GetCart(ctx, hash)
				n := 0
				if mode == "fail" {
					n = 1
				}
				if e != nil || c.PendingOrderID != nil || len(c.Items) != n {
					t.Fatal(c, e)
				}
			}
			t.Logf("scenario=%s order=%s state=%s concurrent_resumes=8 stock=%d", scenario, id, o.Status, stock)
		})
	}
}

func TestShippingClientContract(t *testing.T) {
	id := "0123456789abcdef0123456789abcdef"
	items := []shipmentItem{{1, 2}}
	for _, scenario := range []string{"ok", "wrong items", "wrong id", "wrong mode", "wrong status", "wrong media", "too large", "redirect", "missing"} {
		t.Run(scenario, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				w.Header().Set("Content-Type", "application/json")
				if scenario == "missing" {
					w.WriteHeader(404)
					return
				}
				if scenario == "redirect" {
					w.Header().Set("Location", "/elsewhere")
					w.WriteHeader(307)
					return
				}
				if scenario == "wrong media" {
					w.Header().Set("Content-Type", "text/plain")
				}
				if scenario == "too large" {
					_, _ = w.Write([]byte(strings.Repeat(" ", 16385)))
					return
				}
				result := map[string]any{"orderId": id, "items": items, "mode": "success", "status": "requested"}
				switch scenario {
				case "wrong items":
					result["items"] = []shipmentItem{{1, 3}}
				case "wrong id":
					result["orderId"] = strings.Repeat("f", 32)
				case "wrong mode":
					result["mode"] = "fail"
				case "wrong status":
					result["status"] = "delivered"
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"shipment": result})
			}))
			defer server.Close()
			p, e := newHTTPShipping(server.URL)
			if e != nil {
				t.Fatal(e)
			}
			result, e := p.Get(context.Background(), id, items, "success")
			if scenario == "ok" {
				if e != nil || result != "requested" {
					t.Fatal(result, e)
				}
			} else if scenario == "missing" {
				if e != errShippingNotFound {
					t.Fatal(e)
				}
			} else if e == nil {
				t.Fatal("accepted invalid response")
			}
			if calls != 1 {
				t.Fatal("automatic retry/redirect", calls)
			}
		})
	}
}
