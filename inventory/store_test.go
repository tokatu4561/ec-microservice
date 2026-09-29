package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func uniqueID() string { var b [16]byte; _, _ = rand.Read(b[:]); return hex.EncodeToString(b[:]) }
func fixture(t *testing.T) (postgresStore, context.Context, int64, int64) {
	t.Helper()
	if os.Getenv("TEST_DATABASE_URL") == "" {
		t.Skip("run inventory-test for DB tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	pool, e := pgxpool.New(ctx, os.Getenv("TEST_DATABASE_URL"))
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(pool.Close)
	if _, e = pool.Exec(ctx, schema); e != nil {
		t.Fatal(e)
	}
	if _, e = pool.Exec(ctx, `CREATE SEQUENCE IF NOT EXISTS test_stock_ids START 1000000`); e != nil {
		t.Fatal(e)
	}
	var a, b int64
	for _, id := range []*int64{&a, &b} {
		if e = pool.QueryRow(ctx, `SELECT nextval('test_stock_ids')`).Scan(id); e != nil {
			t.Fatal(e)
		}
		if _, e = pool.Exec(ctx, `INSERT INTO stocks(product_id,available) VALUES($1,5)`, *id); e != nil {
			t.Fatal(e)
		}
	}
	return postgresStore{pool}, ctx, a, b
}
func stock(t *testing.T, s postgresStore, ctx context.Context, id int64, want int64) {
	t.Helper()
	var n int64
	if e := s.pool.QueryRow(ctx, `SELECT available FROM stocks WHERE product_id=$1`, id).Scan(&n); e != nil || n != want {
		t.Fatalf("stock=%d want=%d err=%v", n, want, e)
	}
}
func TestReservationLifecyclePostgres(t *testing.T) {
	s, ctx, a, b := fixture(t)
	in := ReservationInput{uniqueID(), []ReservationItem{{b, 2}, {a, 1}}}
	r, e := s.Create(ctx, in)
	if e != nil || r.Status != "reserved" {
		t.Fatal(r, e)
	}
	stock(t, s, ctx, a, 4)
	stock(t, s, ctx, b, 3)
	in.Items = []ReservationItem{{a, 1}, {b, 2}}
	again, e := s.Create(ctx, in)
	if e != nil || !reflect.DeepEqual(r, again) {
		t.Fatal(again, e)
	}
	other := in
	other.Items = []ReservationItem{{a, 2}, {b, 2}}
	if _, e = s.Create(ctx, other); !errors.Is(e, errConflict) {
		t.Fatal(e)
	}
	c, e := s.Commit(ctx, in.OrderID)
	if e != nil || c.Status != "committed" {
		t.Fatal(c, e)
	}
	stock(t, s, ctx, a, 4)
	again, e = s.Commit(ctx, in.OrderID)
	if e != nil || !reflect.DeepEqual(c, again) {
		t.Fatal(again, e)
	}
	if _, e = s.Release(ctx, in.OrderID); !errors.Is(e, errConflict) {
		t.Fatal("committed reservation released", e)
	}
	r, e = s.Create(ctx, in)
	if e != nil || r.Status != "committed" {
		t.Fatal(r, e)
	}
	other = ReservationInput{uniqueID(), []ReservationItem{{a, 1}, {b, 4}}}
	r, e = s.Create(ctx, other)
	if e != nil || r.Status != "rejected" || !reflect.DeepEqual(r.Shortages, []int64{b}) {
		t.Fatal(r, e)
	}
	stock(t, s, ctx, a, 4)
	stock(t, s, ctx, b, 3)
	if _, e = s.pool.Exec(ctx, `UPDATE stocks SET available=10 WHERE product_id=$1`, b); e != nil {
		t.Fatal(e)
	}
	again, e = s.Create(ctx, other)
	if e != nil || !reflect.DeepEqual(r, again) {
		t.Fatal("rejected request was retried after restock", again, e)
	}
	other = ReservationInput{uniqueID(), []ReservationItem{{a, 2}}}
	if _, e = s.Create(ctx, other); e != nil {
		t.Fatal(e)
	}
	released, e := s.Release(ctx, other.OrderID)
	if e != nil || released.Status != "released" {
		t.Fatal(released, e)
	}
	stock(t, s, ctx, a, 4)
	again, e = s.Release(ctx, other.OrderID)
	if e != nil || !reflect.DeepEqual(released, again) {
		t.Fatal(again, e)
	}
	again, e = s.Create(ctx, other)
	if e != nil || !reflect.DeepEqual(released, again) {
		t.Fatal("resurrected", again, e)
	}
	if _, e = s.Commit(ctx, other.OrderID); !errors.Is(e, errConflict) {
		t.Fatal(e)
	}
	if _, e = s.Create(ctx, ReservationInput{uniqueID(), []ReservationItem{{999999999, 1}}}); !errors.Is(e, errConflict) {
		t.Fatal("missing stock must not look like shortage", e)
	}
}
func TestConcurrentReservationPostgres(t *testing.T) {
	s, ctx, a, b := fixture(t)
	if _, e := s.pool.Exec(ctx, `UPDATE stocks SET available=1 WHERE product_id=ANY($1::bigint[])`, []int64{a, b}); e != nil {
		t.Fatal(e)
	}
	start := make(chan struct{})
	results := make(chan Reservation, 20)
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			r, e := s.Create(ctx, ReservationInput{uniqueID(), []ReservationItem{{b, 1}, {a, 1}}})
			if e != nil {
				t.Error(e)
			}
			results <- r
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	reserved, rejected := 0, 0
	var id string
	for r := range results {
		switch r.Status {
		case "reserved":
			reserved++
			id = r.OrderID
		case "rejected":
			rejected++
		}
	}
	if reserved != 1 || rejected != 19 {
		t.Fatal(reserved, rejected)
	}
	stock(t, s, ctx, a, 0)
	stock(t, s, ctx, b, 0)
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, e := s.Release(ctx, id); e != nil {
				t.Error(e)
			}
		}()
	}
	wg.Wait()
	stock(t, s, ctx, a, 1)
	stock(t, s, ctx, b, 1)
	// 同じ要求の並行作成は一回だけ控除し、同じ予約を返す。
	id = uniqueID()
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, e := s.Create(ctx, ReservationInput{id, []ReservationItem{{a, 1}}})
			if e != nil || r.Status != "reserved" {
				t.Error(r, e)
			}
		}()
	}
	wg.Wait()
	stock(t, s, ctx, a, 0)
	// 確定と解除の競合は一方だけ成立し、最終状態に一致する在庫になる。
	goResults := make(chan error, 2)
	go func() { _, e := s.Commit(ctx, id); goResults <- e }()
	go func() { _, e := s.Release(ctx, id); goResults <- e }()
	e1, e2 := <-goResults, <-goResults
	if !((e1 == nil && errors.Is(e2, errConflict)) || (e2 == nil && errors.Is(e1, errConflict))) {
		t.Fatal(e1, e2)
	}
	r, e := s.Get(ctx, id)
	if e != nil {
		t.Fatal(e)
	}
	n := int64(0)
	if r.Status == "released" {
		n = 1
	}
	stock(t, s, ctx, a, n)
}
func TestInventoryHTTPPostgres(t *testing.T) {
	s, ctx, a, _ := fixture(t)
	h := newHandler(s, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	id := uniqueID()
	call := func(method, path, body string, want int) {
		t.Helper()
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != want {
			t.Fatalf("%s %s: %d %s", method, path, w.Code, w.Body)
		}
	}
	for _, body := range []string{`{}`, `null`, `{"orderId":"bad","items":[]}`, fmt.Sprintf(`{"orderId":%q,"items":[{"productId":%d,"quantity":0}]}`, id, a), strings.Repeat(" ", 16385) + `{}`} {
		call("POST", "/reservations", body, 400)
	}
	body, _ := json.Marshal(ReservationInput{id, []ReservationItem{{a, 1}}})
	call("POST", "/reservations", string(body), 201)
	call("GET", "/reservations/"+id, "", 200)
	call("POST", "/reservations/"+id+"/commit", "{}", 400)
	call("POST", "/reservations/"+id+"/commit", "", 200)
	call("POST", "/reservations/"+id+"/release", "", 409)
	call("GET", "/stocks?ids="+fmt.Sprint(a), "", 200)
	call("GET", "/stocks?ids=bad", "", 400)
	stock(t, s, ctx, a, 4)
}
func newDatabase(t *testing.T, url string) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()
	admin, e := pgxpool.New(ctx, url)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(admin.Close)
	name := "inv_test_" + uniqueID()
	quoted := pgx.Identifier{name}.Sanitize()
	if _, e = admin.Exec(ctx, "CREATE DATABASE "+quoted); e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() {
		if _, e := admin.Exec(ctx, "DROP DATABASE "+quoted+" WITH (FORCE)"); e != nil {
			t.Error(e)
		}
	})
	cfg, e := pgxpool.ParseConfig(url)
	if e != nil {
		t.Fatal(e)
	}
	cfg.ConnConfig.Database = name
	p, e := pgxpool.NewWithConfig(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(p.Close)
	return p
}
func TestOfflineImportPostgres(t *testing.T) {
	if os.Getenv("TEST_SOURCE_DATABASE_URL") == "" {
		t.Skip("run inventory-test")
	}
	ctx := context.Background()
	src := newDatabase(t, os.Getenv("TEST_SOURCE_DATABASE_URL"))
	dst := newDatabase(t, os.Getenv("TEST_DATABASE_URL"))
	for _, file := range []string{"init/001_products.sql", "migrations/002_orders.sql", "migrations/003_cart_order_items.sql", "migrations/004_order_progress.sql", "migrations/005_shipping_progress.sql", "migrations/006_inventory_progress.sql"} {
		raw, e := os.ReadFile("/schema/" + file)
		if e != nil {
			t.Fatal(e)
		}
		if _, e = src.Exec(ctx, string(raw)); e != nil {
			t.Fatal(e)
		}
	}
	id := uniqueID()
	_, e := src.Exec(ctx, `UPDATE products SET stock=8 WHERE id=1;
 INSERT INTO orders(id,status,payment_status,shipping_status) VALUES('`+id+`','processing','pending','not_started');
 INSERT INTO order_items(order_id,product_id,product_name,quantity,price_yen,stock_shortage) VALUES('`+id+`',1,'historical',2,980,false);
 INSERT INTO order_progress(order_id,payment_mode,shipping_mode,stock_held) VALUES('`+id+`','success','success',true);`)
	if e != nil {
		t.Fatal(e)
	}
	if e = importFromOrder(ctx, src, dst); e != nil {
		t.Fatal(e)
	}
	s := postgresStore{dst}
	stock(t, s, ctx, 1, 8)
	r, e := s.Get(ctx, id)
	if e != nil || r.Status != "reserved" || r.Items[0].Quantity != 2 {
		t.Fatal(r, e)
	}
	var phase string
	if e = src.QueryRow(ctx, `SELECT phase FROM order_progress WHERE order_id=$1`, id).Scan(&phase); e != nil || phase != "payment_pending" {
		t.Fatal(phase, e)
	}
	if _, e = s.Release(ctx, id); e != nil {
		t.Fatal(e)
	}
	stock(t, s, ctx, 1, 10)
	if e = importFromOrder(ctx, src, dst); e != nil {
		t.Fatal(e)
	}
	stock(t, s, ctx, 1, 10)
	if _, e = dst.Exec(ctx, `UPDATE inventory_import SET fingerprint='wrong'`); e != nil {
		t.Fatal(e)
	}
	if e = importFromOrder(ctx, src, dst); e == nil {
		t.Fatal("accepted different destination")
	}
	// Inventory保存後・Orderマーカー前で停止した場合も、同一snapshotのみ再開可能。
	dst2 := newDatabase(t, os.Getenv("TEST_DATABASE_URL"))
	if _, e = dst2.Exec(ctx, schema); e != nil {
		t.Fatal(e)
	}
	snap := importSnapshot{Stocks: []Stock{{1, 8}}, Reservations: []ReservationInput{{uniqueID(), []ReservationItem{{1, 2}}}}}
	if e = importSnapshotData(ctx, dst2, snap, "same"); e != nil {
		t.Fatal(e)
	}
	if e = importSnapshotData(ctx, dst2, snap, "same"); e != nil {
		t.Fatal(e)
	}
	stock(t, postgresStore{dst2}, ctx, 1, 8)
	if e = importSnapshotData(ctx, dst2, snap, "other"); e == nil {
		t.Fatal("overwrote prior import")
	}
}
