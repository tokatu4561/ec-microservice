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
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type orderStub struct {
	fakeStore
	calls int
	input OrderInput
}

func (s *orderStub) CreateOrder(_ context.Context, id string, in OrderInput) (Order, error) {
	s.calls++
	s.input = in
	return Order{ID: id, Status: "shipping_requested"}, s.err
}

func request(handler http.Handler, method, path, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	return w
}

func TestOrderInputHTTP(t *testing.T) {
	for _, body := range []string{
		`{}`, `null`, `{"productId":1,"quantity":0}`, `{"productId":1,"quantity":-1}`,
		`{"productId":1,"quantity":1.5}`, `{"productId":1,"quantity":2147483648}`,
		`{"productId":0,"quantity":1}`, `{"productId":1,"quantity":1,"paymentMode":"invalid"}`,
		`{"productId":1,"quantity":1,"shippingMode":"invalid"}`,
		`{"productId":1,"quantity":1,"priceYen":1}`, `{"productId":1,"quantity":1} {}`,
		`{"productId":`, strings.Repeat(" ", 4097) + `{}`,
	} {
		t.Run(body[:min(len(body), 90)], func(t *testing.T) {
			s := &orderStub{}
			h := newHandler(s, slog.New(slog.NewTextHandler(io.Discard, nil)))
			w := request(h, "POST", "/api/orders", body)
			if w.Code != 400 || s.calls != 0 {
				t.Fatalf("status=%d calls=%d body=%s", w.Code, s.calls, w.Body)
			}
		})
	}
	s := &orderStub{}
	h := newHandler(s, slog.New(slog.NewTextHandler(io.Discard, nil)))
	w := request(h, "POST", "/api/orders", `{"productId":1,"quantity":2}`)
	if w.Code != 201 || s.input.PaymentMode != "success" || s.input.ShippingMode != "success" {
		t.Fatalf("status=%d input=%+v", w.Code, s.input)
	}
	var result struct {
		Order Order `json:"order"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if w.Header().Get("Location") != "/api/orders/"+result.Order.ID || !orderIDPattern.MatchString(result.Order.ID) {
		t.Fatal("invalid created order location")
	}
	for _, tc := range []struct {
		err    error
		status int
	}{{errNotFound, 404}, {errors.New("private db error"), 503}} {
		s.err = tc.err
		w := request(h, "POST", "/api/orders", `{"productId":1,"quantity":1}`)
		if w.Code != tc.status || strings.Contains(w.Body.String(), "private db") {
			t.Fatalf("unexpected error: %d %s", w.Code, w.Body)
		}
		if tc.status == 503 {
			var body map[string]string
			_ = json.Unmarshal(w.Body.Bytes(), &body)
			if !orderIDPattern.MatchString(body["orderId"]) {
				t.Fatal("missing order ID for outcome lookup")
			}
		}
	}
	r := httptest.NewRequest("POST", "/api/orders", strings.NewReader(`{}`))
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 415 {
		t.Fatalf("missing content-type: %d", w.Code)
	}
	if w := request(h, "GET", "/api/orders/bad-id", ""); w.Code != 400 {
		t.Fatalf("invalid id: %d", w.Code)
	}
	s.err = errNotFound
	if w := request(h, "GET", "/api/orders/"+strings.Repeat("a", 32), ""); w.Code != 404 {
		t.Fatalf("missing order: %d", w.Code)
	}
}

func orderFixture(t *testing.T, stock int) (postgresStore, int64) {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL is unset; use Compose api-test")
	}
	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	var id int64
	err = pool.QueryRow(context.Background(), `INSERT INTO products(name,price_yen,stock) VALUES('テスト商品',500,$1) RETURNING id`, stock).Scan(&id)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := pool.Exec(ctx, `DELETE FROM orders WHERE id IN (SELECT order_id FROM order_items WHERE product_id=$1)`, id); err != nil {
			t.Error(err)
		}
		if _, err := pool.Exec(ctx, `DELETE FROM products WHERE id=$1`, id); err != nil {
			t.Error(err)
		}
	})
	return postgresStore{pool}, id
}

func checkStock(t *testing.T, s postgresStore, productID int64, want int) {
	t.Helper()
	var stock int
	if err := s.pool.QueryRow(context.Background(), `SELECT stock FROM products WHERE id=$1`, productID).Scan(&stock); err != nil {
		t.Fatal(err)
	}
	if stock != want {
		t.Fatalf("stock=%d want=%d", stock, want)
	}
}

func TestOrderOutcomesPostgres(t *testing.T) {
	for _, tc := range []struct {
		name, payment, shipping, status, reason, payStatus, shipStatus string
		quantity, stock                                                int
	}{
		{"success", "success", "success", "shipping_requested", "", "succeeded", "requested", 2, 1},
		{"insufficient stock", "fail", "fail", "failed", "out_of_stock", "not_started", "not_started", 4, 3},
		{"payment failure", "fail", "success", "failed", "payment_failed", "failed", "not_started", 2, 3},
		{"shipping failure", "success", "fail", "failed", "shipping_failed", "cancelled", "failed", 2, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, id := orderFixture(t, 3)
			h := newHandler(s, slog.New(slog.NewTextHandler(io.Discard, nil)))
			w := request(h, "POST", "/api/orders", fmt.Sprintf(`{"productId":%d,"quantity":%d,"paymentMode":%q,"shippingMode":%q}`, id, tc.quantity, tc.payment, tc.shipping))
			if w.Code != 201 {
				t.Fatalf("status=%d body=%s", w.Code, w.Body)
			}
			var response struct {
				Order Order `json:"order"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			o := response.Order
			reason := ""
			if o.FailureReason != nil {
				reason = *o.FailureReason
			}
			if o.Status != tc.status || reason != tc.reason || o.PaymentStatus != tc.payStatus || o.ShippingStatus != tc.shipStatus || len(o.Items) != 1 || o.Items[0].Quantity != tc.quantity || o.Items[0].PriceYen != 500 || o.CreatedAt.IsZero() {
				t.Fatalf("order=%+v", o)
			}
			checkStock(t, s, id, tc.stock)
			// 注文後の商品改定で注文時点の名称・価格が変わらないことも確認する。
			if _, err := s.pool.Exec(context.Background(), `UPDATE products SET name='改定後',price_yen=700 WHERE id=$1`, id); err != nil {
				t.Fatal(err)
			}
			get := request(h, "GET", "/api/orders/"+o.ID, "")
			if get.Code != 200 {
				t.Fatalf("lookup: %d %s", get.Code, get.Body)
			}
			var saved struct {
				Order Order `json:"order"`
			}
			if err := json.Unmarshal(get.Body.Bytes(), &saved); err != nil {
				t.Fatal(err)
			}
			if len(saved.Order.Items) != 1 || saved.Order.Items[0].PriceYen != 500 || saved.Order.Items[0].ProductName != "テスト商品" || saved.Order.Status != tc.status || saved.Order.ID != o.ID {
				t.Fatalf("saved=%+v", saved.Order)
			}
		})
	}
}

func TestConcurrentOrdersPostgres(t *testing.T) {
	s, id := orderFixture(t, 1)
	start := make(chan struct{})
	type result struct {
		order Order
		err   error
	}
	results := make(chan result, 2)
	for range 2 {
		go func() {
			<-start
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			o, err := s.CreateOrder(ctx, fmt.Sprintf("%x", randomID()), OrderInput{ProductID: id, Quantity: 1})
			results <- result{o, err}
		}()
	}
	close(start)
	success, failed := 0, 0
	for range 2 {
		r := <-results
		if r.err != nil {
			t.Fatal(r.err)
		}
		if r.order.Status == "shipping_requested" {
			success++
		}
		if r.order.Status == "failed" && r.order.FailureReason != nil && *r.order.FailureReason == "out_of_stock" {
			failed++
		}
	}
	if success != 1 || failed != 1 {
		t.Fatalf("success=%d failed=%d", success, failed)
	}
	checkStock(t, s, id, 0)
	var count int
	if err := s.pool.QueryRow(context.Background(), `SELECT count(*) FROM order_items WHERE product_id=$1`, id).Scan(&count); err != nil || count != 2 {
		t.Fatalf("saved orders=%d err=%v", count, err)
	}
}

func TestOrderRollbackPostgres(t *testing.T) {
	s, id := orderFixture(t, 3)
	// テスト専用DBのトリガーで、在庫UPDATE後の注文INSERTを失敗させる。
	_, err := s.pool.Exec(context.Background(), fmt.Sprintf(`
	CREATE FUNCTION test_reject_order() RETURNS trigger LANGUAGE plpgsql AS $$
	BEGIN IF NEW.product_id = %d THEN RAISE EXCEPTION 'injected order insert failure'; END IF; RETURN NEW; END $$;
	CREATE TRIGGER test_order_insert_failure BEFORE INSERT ON order_items FOR EACH ROW EXECUTE FUNCTION test_reject_order();`, id))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := s.pool.Exec(context.Background(), `DROP TRIGGER test_order_insert_failure ON order_items; DROP FUNCTION test_reject_order();`); err != nil {
			t.Error(err)
		}
	})
	orderID := fmt.Sprintf("%x", randomID())
	_, err = s.CreateOrder(context.Background(), orderID, OrderInput{ProductID: id, Quantity: 2})
	if err == nil || !strings.Contains(err.Error(), "injected order insert failure") {
		t.Fatalf("expected injected DB error, got %v", err)
	}
	checkStock(t, s, id, 3)
	if _, err := s.GetOrder(context.Background(), orderID); !errors.Is(err, errNotFound) {
		t.Fatalf("partial order remained: %v", err)
	}
}
