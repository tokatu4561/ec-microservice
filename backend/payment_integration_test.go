package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func lookupRemotePayment(t *testing.T, base, id string, expected int) (string, int64) {
	t.Helper()
	client := http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(base + "/payments/" + id)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != expected {
		t.Fatalf("Payment lookup=%d want=%d", resp.StatusCode, expected)
	}
	if expected != 200 {
		return "", 0
	}
	var result struct {
		Payment struct {
			Status string `json:"status"`
			Amount int64  `json:"amountYen"`
		} `json:"payment"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	return result.Payment.Status, result.Payment.Amount
}

// Orderの実DBと別DBを持つ実Paymentサービスを使用する。
// 障害はテスト内HTTPプロキシ／Order側DBトリガーで注入し、通常APIには故障用入口を作らない。
func TestOrderPaymentAcrossServicesPostgres(t *testing.T) {
	for _, kind := range []string{"single", "cart"} {
		for _, scenario := range []string{"success", "declined", "shipping failed", "shortage", "cancel unavailable", "response lost after commit", "malformed after commit", "order insert failure", "order finalize failure", "cancel response lost", "payment stopped"} {
			t.Run(kind+"/"+scenario, func(t *testing.T) {
				s, product := orderFixture(t, 5)
				base := os.Getenv("TEST_PAYMENT_URL")
				if base == "" {
					t.Fatal("TEST_PAYMENT_URL required with TEST_DATABASE_URL; use Compose api-test")
				}
				var calls atomic.Int32
				var requestID atomic.Value
				proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					requestID.Store(r.Header.Get("X-Request-ID"))
					if scenario == "cancel unavailable" && strings.HasSuffix(r.URL.Path, "/cancel") {
						w.WriteHeader(503)
						return
					}
					req, err := http.NewRequestWithContext(r.Context(), r.Method, base+r.URL.RequestURI(), r.Body)
					if err != nil {
						t.Error(err)
						w.WriteHeader(502)
						return
					}
					req.Header = r.Header.Clone()
					upstream := http.Client{Timeout: 3 * time.Second}
					resp, err := upstream.Do(req)
					if err != nil {
						t.Error(err)
						w.WriteHeader(502)
						return
					}
					defer resp.Body.Close()
					body, err := io.ReadAll(resp.Body)
					if err != nil {
						t.Error(err)
						w.WriteHeader(502)
						return
					}
					if (scenario == "response lost after commit" && r.Method == "POST") || (scenario == "cancel response lost" && strings.HasSuffix(r.URL.Path, "/cancel")) {
						<-r.Context().Done()
						return
					}
					if scenario == "malformed after commit" && r.Method == "POST" {
						body = []byte(`{"payment":`)
					}
					w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
					w.WriteHeader(resp.StatusCode)
					_, _ = w.Write(body)
				}))
				t.Cleanup(proxy.Close)
				if scenario == "payment stopped" {
					proxy.Close()
				}
				client, err := newHTTPPayments(proxy.URL)
				if err != nil {
					t.Fatal(err)
				}
				s.payments = client
				if scenario == "order insert failure" {
					_, err = s.pool.Exec(context.Background(), fmt.Sprintf(`CREATE FUNCTION test_payment_order_failure() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.product_id=%d THEN RAISE EXCEPTION 'injected post-payment failure'; END IF; RETURN NEW; END $$; CREATE TRIGGER payment_order_failure BEFORE INSERT ON order_items FOR EACH ROW EXECUTE FUNCTION test_payment_order_failure();`, product))
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() {
						if _, err := s.pool.Exec(context.Background(), `DROP TRIGGER payment_order_failure ON order_items; DROP FUNCTION test_payment_order_failure();`); err != nil {
							t.Error(err)
						}
					})
				}
				if scenario == "order finalize failure" {
					_, err = s.pool.Exec(context.Background(), fmt.Sprintf(`CREATE FUNCTION test_final_failure() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.status IN ('failed','shipping_requested') AND NEW.id IN (SELECT order_id FROM order_items WHERE product_id=%d) THEN RAISE EXCEPTION 'injected final failure'; END IF; RETURN NEW; END $$; CREATE TRIGGER final_failure BEFORE UPDATE ON orders FOR EACH ROW EXECUTE FUNCTION test_final_failure();`, product))
					if err != nil {
						t.Fatal(err)
					}
				}
				quantity := 1
				if scenario == "shortage" {
					quantity = 6
				}
				pay, ship := "success", "success"
				if scenario == "declined" {
					pay = "fail"
				}
				if scenario == "shipping failed" || scenario == "cancel unavailable" || scenario == "cancel response lost" {
					ship = "fail"
				}
				h := newHandler(s, slog.New(slog.NewTextHandler(io.Discard, nil)))
				var w *httptest.ResponseRecorder
				var hash string
				var cartBefore Cart
				if kind == "cart" {
					token := fmt.Sprintf("%x%x", randomID(), randomID())
					hash = tokenHash(token)
					cartBefore, err = s.NewCart(context.Background(), hash)
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() {
						if _, err := s.pool.Exec(context.Background(), `DELETE FROM carts WHERE token_hash=$1`, hash); err != nil {
							t.Error(err)
						}
					})
					cartBefore = putTestItem(t, s, hash, cartBefore, product, quantity)
					w = cartRequest(h, "POST", "/api/cart/checkout", token, fmt.Sprintf(`{"version":%d,"paymentMode":%q,"shippingMode":%q}`, cartBefore.Version, pay, ship))
				} else {
					w = request(h, "POST", "/api/orders", fmt.Sprintf(`{"productId":%d,"quantity":%d,"paymentMode":%q,"shippingMode":%q}`, product, quantity, pay, ship))
				}
				wantCode, remoteStatus, stock, wantCalls := 201, "succeeded", 5, 2
				switch scenario {
				case "success":
					stock = 4
				case "declined":
					remoteStatus = "failed"
				case "shipping failed":
					remoteStatus = "cancelled"
					wantCalls = 3
				case "shortage":
					remoteStatus = ""
					wantCalls = 0
				case "cancel unavailable":
					wantCode = 503
					wantCalls = 3
				case "response lost after commit", "malformed after commit", "order finalize failure":
					wantCode = 503
				case "order insert failure":
					wantCode, remoteStatus, wantCalls = 503, "", 0
				case "cancel response lost":
					wantCode, remoteStatus, wantCalls = 503, "cancelled", 3
				case "payment stopped":
					wantCode = 503
					remoteStatus = ""
					wantCalls = 0
				}
				if wantCode == 503 && scenario != "order insert failure" {
					stock = 4
				}
				if w.Code != wantCode {
					t.Fatalf("status=%d body=%s", w.Code, w.Body)
				}
				var response struct {
					Order     Order  `json:"order"`
					OrderID   string `json:"orderId"`
					RequestID string `json:"requestId"`
				}
				if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
					t.Fatal(err)
				}
				id := response.Order.ID
				if wantCode == 503 {
					id = response.OrderID
					if response.RequestID != w.Header().Get("X-Request-ID") || response.RequestID == "" {
						t.Fatal("missing error correlation")
					}
					wantGet := 200
					if scenario == "order insert failure" {
						wantGet = 404
					}
					if get := request(h, "GET", "/api/orders/"+id, ""); get.Code != wantGet {
						t.Fatalf("partial order remained: %s", get.Body)
					}
				} else {
					get := request(h, "GET", "/api/orders/"+id, "")
					if get.Code != 200 {
						t.Fatalf("order not saved: %s", get.Body)
					}
					if scenario == "success" && response.Order.Status != "shipping_requested" {
						t.Fatal("success not reflected")
					}
					if scenario != "success" && response.Order.Status != "failed" {
						t.Fatal("business failure not saved")
					}
				}
				if !orderIDPattern.MatchString(id) {
					t.Fatal("order ID missing")
				}
				wantLookup := 200
				if remoteStatus == "" {
					wantLookup = 404
				}
				status, amount := lookupRemotePayment(t, base, id, wantLookup)
				if status != remoteStatus || (status != "" && amount != 500) {
					t.Fatalf("remote status=%s amount=%d", status, amount)
				}
				if int(calls.Load()) != wantCalls {
					t.Fatalf("unexpected retry/cancel: calls=%d want=%d", calls.Load(), wantCalls)
				}
				if wantCalls > 0 && requestID.Load() != w.Header().Get("X-Request-ID") {
					t.Fatal("cross-service request ID differs")
				}
				checkStock(t, s, product, stock)
				if kind == "cart" {
					after, err := s.GetCart(context.Background(), hash)
					if err != nil {
						t.Fatal(err)
					}
					if scenario == "order insert failure" {
						if after.Version != cartBefore.Version || len(after.Items) != 1 || after.LastOrderID != nil {
							t.Fatalf("cart changed on unknown outcome: %+v", after)
						}
					} else {
						if after.Version != cartBefore.Version+1 || after.LastOrderID == nil || *after.LastOrderID != id {
							t.Fatalf("cart outcome not saved: %+v", after)
						}
						wantItems := 1
						if scenario == "success" {
							wantItems = 0
						}
						if len(after.Items) != wantItems {
							t.Fatalf("cart items=%d", len(after.Items))
						}
					}
				}
				if scenario == "order finalize failure" {
					if _, err = s.pool.Exec(context.Background(), `DROP TRIGGER final_failure ON orders; DROP FUNCTION test_final_failure();`); err != nil {
						t.Fatal(err)
					}
				}
				if wantCode == 503 && scenario != "order insert failure" {
					if kind == "cart" {
						c, e := s.GetCart(context.Background(), hash)
						if e != nil || c.PendingOrderID == nil || *c.PendingOrderID != id {
							t.Fatalf("pending cart=%+v err=%v", c, e)
						}
						if _, e = s.SetCartItem(context.Background(), hash, c.Version, product, 2, false); e != errConflict {
							t.Fatalf("pending cart editable: %v", e)
						}
						if _, e = s.Checkout(context.Background(), hash, c.Version, fmt.Sprintf("%x", randomID()), pay, ship); e != errConflict {
							t.Fatalf("pending cart checked out twice: %v", e)
						}
					}
					// 新しいstoreとHTTP clientを構築。プロセス内の進行状態に依存しない。
					direct, e := newHTTPPayments(base)
					if e != nil {
						t.Fatal(e)
					}
					resumed := postgresStore{pool: s.pool, payments: direct, shipping: simulatedShipping{}, inventory: s.inventory}
					const count = 8
					var wg sync.WaitGroup
					results := make(chan error, count)
					for range count {
						wg.Add(1)
						go func() { defer wg.Done(); _, e := resumed.ResumeOrder(context.Background(), id); results <- e }()
					}
					wg.Wait()
					close(results)
					for e := range results {
						if e != nil {
							t.Fatal(e)
						}
					}
					final, e := resumed.GetOrder(context.Background(), id)
					if e != nil {
						t.Fatal(e)
					}
					expectedStatus, expectedStock := "shipping_requested", 4
					if ship == "fail" {
						expectedStatus, expectedStock = "failed", 5
					}
					if final.Status != expectedStatus {
						t.Fatalf("recovery=%+v", final)
					}
					checkStock(t, s, product, expectedStock)
					if kind == "cart" {
						c, e := s.GetCart(context.Background(), hash)
						expectedItems := 0
						if ship == "fail" {
							expectedItems = 1
						}
						if e != nil || c.PendingOrderID != nil || c.Version != cartBefore.Version+1 || len(c.Items) != expectedItems {
							t.Fatalf("recovered cart=%+v err=%v", c, e)
						}
					}
					t.Logf("recovered order=%s state=%s concurrent_resumes=%d", id, final.Status, count)
				}

			})
		}
	}
}
