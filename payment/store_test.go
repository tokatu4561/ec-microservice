package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func uniqueID() string { var b [16]byte; _, _ = rand.Read(b[:]); return hex.EncodeToString(b[:]) }

func fixture(t *testing.T) (postgresStore, context.Context) {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL unset; run Compose payment-test for DB integration tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err = pool.Exec(ctx, schema); err != nil {
		t.Fatal(err)
	}
	return postgresStore{pool}, ctx
}

func decodePayment(t *testing.T, h http.Handler, method, path, body string, code int) Payment {
	t.Helper()
	w := request(h, method, path, body)
	if w.Code != code {
		t.Fatalf("%s %s: status=%d body=%s", method, path, w.Code, w.Body)
	}
	var result struct {
		Payment Payment `json:"payment"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	return result.Payment
}

func TestPaymentLifecyclePostgres(t *testing.T) {
	s, ctx := fixture(t)
	h := newHandler(s, quietLogger())
	for _, mode := range []string{"success", "fail"} {
		id := uniqueID()
		status := "succeeded"
		if mode == "fail" {
			status = "failed"
		}
		p := decodePayment(t, h, "POST", "/payments", fmt.Sprintf(`{"orderId":%q,"amountYen":980,"mode":%q}`, id, mode), 201)
		if p.OrderID != id || p.AmountYen != 980 || p.Status != status || p.CreatedAt.IsZero() || p.UpdatedAt.Before(p.CreatedAt) {
			t.Fatalf("payment=%+v", p)
		}
		replay := decodePayment(t, h, "POST", "/payments", fmt.Sprintf(`{"orderId":%q,"amountYen":980,"mode":%q}`, id, mode), 201)
		if replay != p {
			t.Fatal("identical request changed payment")
		}
		otherMode := "success"
		if mode == "success" {
			otherMode = "fail"
		}
		decodePayment(t, h, "POST", "/payments", fmt.Sprintf(`{"orderId":%q,"amountYen":980,"mode":%q}`, id, otherMode), 409)
		saved := decodePayment(t, h, "GET", "/payments/"+id, "", 200)
		if saved != p {
			t.Fatalf("lookup differs: %+v vs %+v", saved, p)
		}
		decodePayment(t, h, "POST", "/payments", fmt.Sprintf(`{"orderId":%q,"amountYen":1,"mode":"fail"}`, id), 409)
		if saved = decodePayment(t, h, "GET", "/payments/"+id, "", 200); saved != p {
			t.Fatal("duplicate overwrote payment")
		}
		if mode == "fail" {
			decodePayment(t, h, "POST", "/payments/"+id+"/cancel", "", 409)
			if saved = decodePayment(t, h, "GET", "/payments/"+id, "", 200); saved != p {
				t.Fatal("failed payment changed")
			}
		} else {
			cancelled := decodePayment(t, h, "POST", "/payments/"+id+"/cancel", "", 200)
			if cancelled.Status != "cancelled" || cancelled.AmountYen != p.AmountYen || !cancelled.CreatedAt.Equal(p.CreatedAt) || cancelled.UpdatedAt.Before(p.UpdatedAt) {
				t.Fatalf("cancelled=%+v", cancelled)
			}
			repeated := decodePayment(t, h, "POST", "/payments/"+id+"/cancel", "", 200)
			if repeated != cancelled {
				t.Fatal("repeat cancellation changed record")
			}
			if replay := decodePayment(t, h, "POST", "/payments", fmt.Sprintf(`{"orderId":%q,"amountYen":980}`, id), 201); replay != cancelled {
				t.Fatal("replay resurrected cancelled payment")
			}
		}
	}
	missing := uniqueID()
	decodePayment(t, h, "GET", "/payments/"+missing, "", 404)
	decodePayment(t, h, "POST", "/payments/"+missing+"/cancel", "", 404)
	// 起動時SQLの再実行で決済記録が変わらない。
	amount := int64(0)
	p, err := s.Create(ctx, PaymentInput{uniqueID(), &amount, "success"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.pool.Exec(ctx, schema); err != nil {
		t.Fatal(err)
	}
	saved, err := s.Get(ctx, p.OrderID)
	if err != nil || saved != p {
		t.Fatalf("schema reapplication changed data: %+v %v", saved, err)
	}
}

func TestConcurrentCreateAndCancelPostgres(t *testing.T) {
	s, ctx := fixture(t)
	id := uniqueID()
	amount := int64(980)
	const count = 20
	start := make(chan struct{})
	results := make(chan error, count)
	var wg sync.WaitGroup
	for range count {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := s.Create(ctx, PaymentInput{id, &amount, "success"})
			results <- err
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	successes, conflicts := 0, 0
	for err := range results {
		switch {
		case err == nil:
			successes++
		case errors.Is(err, errConflict):
			conflicts++
		default:
			t.Fatal(err)
		}
	}
	if successes != count || conflicts != 0 {
		t.Fatalf("success=%d conflict=%d", successes, conflicts)
	}
	var records int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM payments WHERE order_id=$1`, id).Scan(&records); err != nil || records != 1 {
		t.Fatalf("records=%d err=%v", records, err)
	}
	start = make(chan struct{})
	payments := make(chan Payment, count)
	results = make(chan error, count)
	for range count {
		wg.Add(1)
		go func() { defer wg.Done(); <-start; p, err := s.Cancel(ctx, id); payments <- p; results <- err }()
	}
	close(start)
	wg.Wait()
	close(results)
	close(payments)
	for err := range results {
		if err != nil {
			t.Fatal(err)
		}
	}
	var first Payment
	for p := range payments {
		if p.Status != "cancelled" {
			t.Fatalf("payment=%+v", p)
		}
		if first.OrderID == "" {
			first = p
		} else if first != p {
			t.Fatal("concurrent cancellations changed timestamp")
		}
	}
}

func TestDatabaseFailureHTTPPostgres(t *testing.T) {
	s, _ := fixture(t)
	s.pool.Close()
	h := newHandler(s, quietLogger())
	for _, route := range []struct{ method, path, body string }{
		{"GET", "/healthz", ""}, {"GET", "/payments/" + uniqueID(), ""},
		{"POST", "/payments/" + uniqueID() + "/cancel", ""},
		{"POST", "/payments", fmt.Sprintf(`{"orderId":%q,"amountYen":100}`, uniqueID())},
	} {
		if w := request(h, route.method, route.path, route.body); w.Code != 503 {
			t.Fatalf("status=%d body=%s", w.Code, w.Body)
		}
	}
}
