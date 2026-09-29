package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func newTestCart(t *testing.T, s postgresStore) (string, Cart) {
	t.Helper()
	hash := tokenHash(fmt.Sprintf("%x%x", randomID(), randomID()))
	c, err := s.NewCart(context.Background(), hash)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, err := s.pool.Exec(context.Background(), `DELETE FROM carts WHERE token_hash=$1`, hash)
		if err != nil {
			t.Error(err)
		}
	})
	return hash, c
}
func putTestItem(t *testing.T, s postgresStore, hash string, c Cart, id int64, quantity int) Cart {
	t.Helper()
	next, err := s.SetCartItem(context.Background(), hash, c.Version, id, quantity, false)
	if err != nil {
		t.Fatal(err)
	}
	return next
}
func TestCartPostgres(t *testing.T) {
	s, a := orderFixture(t, 5)
	_, b := orderFixture(t, 5)
	hash, c := newTestCart(t, s)
	c = putTestItem(t, s, hash, c, a, 2)
	c = putTestItem(t, s, hash, c, b, 1)
	if len(c.Items) != 2 || c.TotalYen != 1500 {
		t.Fatalf("cart=%+v", c)
	}
	checkStock(t, s, a, 5)
	checkStock(t, s, b, 5)
	c = putTestItem(t, s, hash, c, a, 3)
	if len(c.Items) != 2 || c.TotalYen != 2000 {
		t.Fatalf("quantity update=%+v", c)
	}
	c, err := s.SetCartItem(context.Background(), hash, c.Version, b, 0, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Items) != 1 || c.Items[0].Quantity != 3 {
		t.Fatalf("delete=%+v", c)
	}
	saved, err := s.GetCart(context.Background(), hash)
	if err != nil || saved.Version != c.Version || saved.TotalYen != 1500 {
		t.Fatalf("saved=%+v err=%v", saved, err)
	}
	_, err = s.SetCartItem(context.Background(), hash, c.Version-1, a, 4, false)
	if !errors.Is(err, errConflict) {
		t.Fatalf("stale update=%v", err)
	}
	other, empty := newTestCart(t, s)
	if len(empty.Items) != 0 {
		t.Fatal("shared cart")
	}
	if _, err = s.Checkout(context.Background(), other, empty.Version, fmt.Sprintf("%x", randomID()), "success", "success"); !errors.Is(err, errEmptyCart) {
		t.Fatalf("empty=%v", err)
	}
}
func TestCartOutcomesPostgres(t *testing.T) {
	for _, tc := range []struct {
		name, payment, shipping, reason string
		quantity                        int
	}{
		{"success", "success", "success", "", 1}, {"shortage", "success", "success", "out_of_stock", 4},
		{"payment", "fail", "success", "payment_failed", 1}, {"shipping", "success", "fail", "shipping_failed", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, a := orderFixture(t, 5)
			_, b := orderFixture(t, 3)
			hash, c := newTestCart(t, s)
			c = putTestItem(t, s, hash, c, b, tc.quantity)
			c = putTestItem(t, s, hash, c, a, 2)
			id := fmt.Sprintf("%x", randomID())
			o, err := s.Checkout(context.Background(), hash, c.Version, id, tc.payment, tc.shipping)
			if err != nil {
				t.Fatal(err)
			}
			if len(o.Items) != 2 || o.TotalYen != int64(2+tc.quantity)*500 {
				t.Fatalf("order=%+v", o)
			}
			saved, err := s.GetOrder(context.Background(), id)
			if err != nil || len(saved.Items) != 2 || saved.TotalYen != o.TotalYen {
				t.Fatalf("saved=%+v err=%v", saved, err)
			}
			after, err := s.GetCart(context.Background(), hash)
			if err != nil {
				t.Fatal(err)
			}
			if after.Version != c.Version+1 || after.LastOrderID == nil || *after.LastOrderID != id {
				t.Fatalf("cart=%+v", after)
			}
			if tc.reason == "" {
				if o.Status != "shipping_requested" || len(after.Items) != 0 {
					t.Fatalf("success=%+v %+v", o, after)
				}
				checkStock(t, s, a, 3)
				checkStock(t, s, b, 2)
			} else {
				if o.FailureReason == nil || *o.FailureReason != tc.reason || len(after.Items) != 2 {
					t.Fatalf("failure=%+v %+v", o, after)
				}
				checkStock(t, s, a, 5)
				checkStock(t, s, b, 3)
				if tc.reason == "shipping_failed" && o.PaymentStatus != "cancelled" {
					t.Fatal("payment not cancelled")
				}
				if tc.reason == "out_of_stock" && (!o.Items[1].StockShortage || o.Items[0].StockShortage) {
					t.Fatalf("shortage flags=%+v", o.Items)
				}
			}
			if _, err = s.Checkout(context.Background(), hash, c.Version, fmt.Sprintf("%x", randomID()), tc.payment, tc.shipping); !errors.Is(err, errConflict) {
				t.Fatalf("duplicate=%v", err)
			}
		})
	}
}
func TestConcurrentCartsPostgres(t *testing.T) {
	s, a := orderFixture(t, 1)
	_, b := orderFixture(t, 1)
	h1, c1 := newTestCart(t, s)
	h2, c2 := newTestCart(t, s)
	c1 = putTestItem(t, s, h1, c1, a, 1)
	c1 = putTestItem(t, s, h1, c1, b, 1)
	c2 = putTestItem(t, s, h2, c2, b, 1)
	c2 = putTestItem(t, s, h2, c2, a, 1)
	start := make(chan struct{})
	type result struct {
		o   Order
		err error
	}
	results := make(chan result, 2)
	for _, c := range []struct {
		hash    string
		version int64
	}{{h1, c1.Version}, {h2, c2.Version}} {
		go func(hash string, v int64) {
			<-start
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			o, err := s.Checkout(ctx, hash, v, fmt.Sprintf("%x", randomID()), "success", "success")
			results <- result{o, err}
		}(c.hash, c.version)
	}
	close(start)
	success, failed := 0, 0
	for range 2 {
		r := <-results
		if r.err != nil {
			t.Fatal(r.err)
		}
		if r.o.Status == "shipping_requested" {
			success++
		} else if r.o.FailureReason != nil && *r.o.FailureReason == "out_of_stock" {
			failed++
		}
	}
	if success != 1 || failed != 1 {
		t.Fatalf("success=%d failed=%d", success, failed)
	}
	checkStock(t, s, a, 0)
	checkStock(t, s, b, 0)
}
func TestSameCartRacesPostgres(t *testing.T) {
	s, id := orderFixture(t, 5)
	hash, c := newTestCart(t, s)
	c = putTestItem(t, s, hash, c, id, 1)
	errs := make(chan error, 2)
	start := make(chan struct{})
	for _, q := range []int{2, 3} {
		go func(q int) {
			<-start
			_, err := s.SetCartItem(context.Background(), hash, c.Version, id, q, false)
			errs <- err
		}(q)
	}
	close(start)
	successes, conflicts := 0, 0
	for range 2 {
		err := <-errs
		if err == nil {
			successes++
		} else if errors.Is(err, errConflict) {
			conflicts++
		} else {
			t.Fatal(err)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatal("lost update protection failed")
	}
	c, err := s.GetCart(context.Background(), hash)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	start = make(chan struct{})
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := s.Checkout(context.Background(), hash, c.Version, fmt.Sprintf("%x", randomID()), "success", "success")
			errs <- err
		}()
	}
	close(start)
	wg.Wait()
	successes, conflicts = 0, 0
	for range 2 {
		err := <-errs
		if err == nil {
			successes++
		} else if errors.Is(err, errConflict) {
			conflicts++
		} else {
			t.Fatal(err)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatal("duplicate checkout")
	}
	checkStock(t, s, id, 5-c.Items[0].Quantity)
	var count int
	if err = s.pool.QueryRow(context.Background(), `SELECT count(*) FROM order_items WHERE product_id=$1`, id).Scan(&count); err != nil || count != 1 {
		t.Fatalf("orders=%d err=%v", count, err)
	}
}
func TestCartRollbackPostgres(t *testing.T) {
	for _, stage := range []string{"before_inventory_request", "during_items"} {
		t.Run(stage, func(t *testing.T) {
			s, a := orderFixture(t, 5)
			_, b := orderFixture(t, 5)
			hash, c := newTestCart(t, s)
			c = putTestItem(t, s, hash, c, a, 1)
			c = putTestItem(t, s, hash, c, b, 2)
			orderID := fmt.Sprintf("%x", randomID())
			// 前者は注文INSERT時、後者は先頭明細INSERT後に失敗させる。どちらもInventory呼出し前。
			table, condition := "orders", fmt.Sprintf("NEW.id = '%s'", orderID)
			if stage == "during_items" {
				table = "order_items"
				condition = fmt.Sprintf("NEW.order_id = '%s' AND NEW.product_id = %d", orderID, b)
			}
			_, err := s.pool.Exec(context.Background(), fmt.Sprintf(`CREATE FUNCTION test_cart_failure() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF %s THEN RAISE EXCEPTION 'injected cart failure'; END IF; RETURN NEW; END $$; CREATE TRIGGER cart_failure BEFORE INSERT ON %s FOR EACH ROW EXECUTE FUNCTION test_cart_failure();`, condition, table))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				_, err := s.pool.Exec(context.Background(), `DROP TRIGGER cart_failure ON `+table+`; DROP FUNCTION test_cart_failure();`)
				if err != nil {
					t.Error(err)
				}
			})
			_, err = s.Checkout(context.Background(), hash, c.Version, orderID, "success", "success")
			if err == nil || !strings.Contains(err.Error(), "injected cart failure") {
				t.Fatalf("failure=%v", err)
			}
			checkStock(t, s, a, 5)
			checkStock(t, s, b, 5)
			if _, err = s.GetOrder(context.Background(), orderID); !errors.Is(err, errNotFound) {
				t.Fatalf("order persisted: %v", err)
			}
			after, err := s.GetCart(context.Background(), hash)
			if err != nil || after.Version != c.Version || len(after.Items) != 2 || after.LastOrderID != nil {
				t.Fatalf("cart changed=%+v err=%v", after, err)
			}
		})
	}
}
func cartRequest(h http.Handler, method, path, token, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Cart-Request", "1")
	if token != "" {
		r.AddCookie(&http.Cookie{Name: cartCookieName, Value: token})
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}
func TestCartCookieHTTPPostgres(t *testing.T) {
	s, id := orderFixture(t, 5)
	h := newHandler(s, slog.New(slog.NewTextHandler(io.Discard, nil)))
	get := cartRequest(h, "GET", "/api/cart", "", "")
	if get.Code != 200 {
		t.Fatalf("get=%d %s", get.Code, get.Body)
	}
	cookie := get.Result().Cookies()[0]
	if !cookie.HttpOnly || cookie.SameSite != http.SameSiteLaxMode || cookie.MaxAge != 30*24*60*60 {
		t.Fatalf("cookie=%+v", cookie)
	}
	hash := tokenHash(cookie.Value)
	t.Cleanup(func() { _, _ = s.pool.Exec(context.Background(), `DELETE FROM carts WHERE token_hash=$1`, hash) })
	put := cartRequest(h, "PUT", fmt.Sprintf("/api/cart/items/%d", id), cookie.Value, `{"version":0,"quantity":2}`)
	if put.Code != 200 {
		t.Fatalf("put=%d %s", put.Code, put.Body)
	}
	var stored string
	if err := s.pool.QueryRow(context.Background(), `SELECT token_hash FROM carts WHERE token_hash=$1`, hash).Scan(&stored); err != nil || stored == cookie.Value {
		t.Fatal("token storage")
	}
	other := cartRequest(h, "GET", "/api/cart", "", "")
	otherToken := other.Result().Cookies()[0].Value
	t.Cleanup(func() {
		_, _ = s.pool.Exec(context.Background(), `DELETE FROM carts WHERE token_hash=$1`, tokenHash(otherToken))
	})
	var empty struct {
		Cart Cart `json:"cart"`
	}
	if err := json.Unmarshal(other.Body.Bytes(), &empty); err != nil || len(empty.Cart.Items) != 0 {
		t.Fatal("cookie isolation")
	}
	expired, err := s.pool.Exec(context.Background(), `UPDATE carts SET expires_at=CURRENT_TIMESTAMP-interval '1 second' WHERE token_hash=$1`, hash)
	if err != nil || expired.RowsAffected() != 1 {
		t.Fatal(err)
	}
	if w := cartRequest(h, "POST", "/api/cart/checkout", cookie.Value, `{"version":1}`); w.Code != 404 {
		t.Fatalf("expired checkout=%d", w.Code)
	}
	fresh := cartRequest(h, "GET", "/api/cart", cookie.Value, "")
	freshToken := fresh.Result().Cookies()[0].Value
	t.Cleanup(func() {
		_, _ = s.pool.Exec(context.Background(), `DELETE FROM carts WHERE token_hash=$1`, tokenHash(freshToken))
	})
	if freshToken == cookie.Value {
		t.Fatal("expired token reused")
	}
	req := httptest.NewRequest("PUT", fmt.Sprintf("/api/cart/items/%d", id), strings.NewReader(`{"version":0,"quantity":1}`))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: cartCookieName, Value: freshToken})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 400 {
		t.Fatal("missing custom header accepted")
	}
}
