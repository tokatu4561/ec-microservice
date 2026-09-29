package main

import (
	"context"
	"errors"
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

func TestInventoryRecoveryPostgres(t *testing.T) {
	for _, scenario := range []string{"reserve lost", "commit lost", "release unavailable", "inventory stopped", "order update failed"} {
		t.Run(scenario, func(t *testing.T) {
			s, product := orderFixture(t, 5)
			direct := inventoryTestClient(t)
			base := os.Getenv("TEST_INVENTORY_URL")
			// Payment/Shippingはこの単位ではtest double。Inventoryは実サービスと実DB。
			proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if scenario == "inventory stopped" || (scenario == "release unavailable" && strings.HasSuffix(r.URL.Path, "/release")) {
					w.WriteHeader(503)
					return
				}
				req, e := http.NewRequestWithContext(r.Context(), r.Method, base+r.URL.RequestURI(), r.Body)
				if e != nil {
					t.Error(e)
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
				if (scenario == "reserve lost" && r.Method == "POST" && r.URL.Path == "/reservations") || (scenario == "commit lost" && strings.HasSuffix(r.URL.Path, "/commit")) {
					conn, _, e := w.(http.Hijacker).Hijack()
					if e != nil {
						t.Error(e)
						return
					}
					_ = conn.Close()
					return
				}
				w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
				w.WriteHeader(resp.StatusCode)
				_, _ = io.Copy(w, resp.Body)
			}))
			defer proxy.Close()
			var err error
			s.inventory, err = newHTTPInventory(proxy.URL)
			if err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			id := fmt.Sprintf("%x", randomID())
			hash, c := newTestCart(t, s)
			c = putTestItem(t, s, hash, c, product, 1)
			if scenario == "order update failed" {
				_, err = s.pool.Exec(ctx, `CREATE FUNCTION inventory_progress_failure() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.id='`+id+`' AND NEW.inventory_status='reserved' THEN RAISE EXCEPTION 'injected inventory progress failure'; END IF; RETURN NEW; END $$; CREATE TRIGGER inventory_progress_failure BEFORE UPDATE ON orders FOR EACH ROW EXECUTE FUNCTION inventory_progress_failure();`)
				if err != nil {
					t.Fatal(err)
				}
			}
			pay := "success"
			if scenario == "release unavailable" {
				pay = "fail"
			}
			_, err = s.Checkout(ctx, hash, c.Version, id, pay, "success")
			if err == nil {
				t.Fatal("expected uncertain outcome")
			}
			pending, e := s.GetOrder(ctx, id)
			if e != nil {
				t.Fatal(e)
			}
			wantPhase, wantInv, wantRemote := "inventory_pending", "pending", "reserved"
			wantStock := 4
			switch scenario {
			case "commit lost":
				wantPhase, wantInv, wantRemote = "inventory_commit_pending", "commit_pending", "committed"
				if pending.PaymentStatus != "succeeded" || pending.ShippingStatus != "requested" {
					t.Fatal(pending)
				}
			case "release unavailable":
				wantPhase, wantInv = "inventory_release_pending", "release_pending"
				if pending.PaymentStatus != "failed" {
					t.Fatal(pending)
				}
			case "inventory stopped":
				wantStock = 5
				wantRemote = ""
				if pending.PaymentStatus != "not_started" {
					t.Fatal(pending)
				}
			}
			var phase string
			if e = s.pool.QueryRow(ctx, `SELECT phase FROM order_progress WHERE order_id=$1`, id).Scan(&phase); e != nil || phase != wantPhase || pending.InventoryStatus != wantInv {
				t.Fatal(phase, pending, e)
			}
			checkStock(t, s, product, wantStock)
			remote, e := direct.Get(ctx, id, []shipmentItem{{product, 1}})
			if wantRemote == "" {
				if !errors.Is(e, errInventoryNotFound) {
					t.Fatal(remote, e)
				}
			} else if e != nil || remote.Status != wantRemote {
				t.Fatal(remote, e)
			}
			savedCart, e := s.GetCart(ctx, hash)
			if e != nil || savedCart.PendingOrderID == nil || *savedCart.PendingOrderID != id {
				t.Fatal(savedCart, e)
			}
			if scenario == "order update failed" {
				if _, e = s.pool.Exec(ctx, `DROP TRIGGER inventory_progress_failure ON orders; DROP FUNCTION inventory_progress_failure()`); e != nil {
					t.Fatal(e)
				}
			}
			recovered := postgresStore{pool: s.pool, payments: simulatedPayments{}, shipping: simulatedShipping{}, inventory: direct}
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
			final, e := recovered.GetOrder(ctx, id)
			wantStatus, wantInventory, wantStock := "shipping_requested", "committed", 4
			if pay == "fail" {
				wantStatus, wantInventory, wantStock = "failed", "released", 5
			}
			if e != nil || final.Status != wantStatus || final.InventoryStatus != wantInventory {
				t.Fatal(final, e)
			}
			checkStock(t, s, product, wantStock)
			r, e := direct.Get(ctx, id, []shipmentItem{{product, 1}})
			if e != nil || r.Status != wantInventory {
				t.Fatal(r, e)
			}
			var legacyStock int
			if e = s.pool.QueryRow(ctx, `SELECT stock FROM products WHERE id=$1`, product).Scan(&legacyStock); e != nil || legacyStock != 5 {
				t.Fatal("Order modified legacy stock", legacyStock, e)
			}
			t.Logf("%s order=%s phase=%s -> %s stock=%d parallel_resumes=8", scenario, id, wantPhase, final.Status, wantStock)
		})
	}
}
func TestInventoryUnavailableViewPostgres(t *testing.T) {
	s, id := orderFixture(t, 5)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) }))
	defer server.Close()
	client, e := newHTTPInventory(server.URL)
	if e != nil {
		t.Fatal(e)
	}
	s.inventory = client
	products, e := s.ListProducts(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	for _, p := range products {
		if p.StockKnown {
			t.Fatal("unknown stock presented as confirmed", p)
		}
	}
	hash, c := newTestCart(t, s)
	c, e = s.SetCartItem(context.Background(), hash, c.Version, id, 1, false)
	if e != nil || len(c.Items) != 1 || c.Items[0].StockKnown {
		t.Fatal(c, e)
	}
}
