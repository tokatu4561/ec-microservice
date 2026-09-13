package main

import (
	"context"
	"encoding/json"
	"errors"
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

type fakeStore struct {
	products []Product
	err      error
}

func (f fakeStore) ListProducts(context.Context) ([]Product, error) { return f.products, f.err }
func (f fakeStore) CreateOrder(context.Context, string, OrderInput) (Order, error) {
	return Order{}, f.err
}
func (f fakeStore) GetOrder(context.Context, string) (Order, error) { return Order{}, f.err }

func TestProducts(t *testing.T) {
	for _, tc := range []struct {
		name   string
		store  fakeStore
		status int
	}{
		{"success", fakeStore{products: []Product{{1, "野菜セット", 980, 10}}}, http.StatusOK},
		{"empty", fakeStore{}, http.StatusOK},
		{"database failure", fakeStore{err: errors.New("private database details")}, http.StatusServiceUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			newHandler(tc.store, slog.New(slog.NewTextHandler(io.Discard, nil))).ServeHTTP(w, httptest.NewRequest("GET", "/api/products", nil))
			if w.Code != tc.status {
				t.Fatalf("status = %d, want %d", w.Code, tc.status)
			}
			if w.Header().Get("X-Request-ID") == "" {
				t.Fatal("missing request ID")
			}
			if w.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("product inventory must not be cached")
			}
			if strings.Contains(w.Body.String(), "private database details") {
				t.Fatal("database error leaked")
			}
			if tc.status == http.StatusOK {
				var body struct {
					Products []Product `json:"products"`
				}
				if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
					t.Fatal(err)
				}
				if body.Products == nil || len(body.Products) != len(tc.store.products) {
					t.Fatalf("unexpected products: %s", w.Body.String())
				}
				if len(body.Products) > 0 && body.Products[0] != tc.store.products[0] {
					t.Fatalf("unexpected product: %+v", body.Products[0])
				}
			}
		})
	}
}

func TestPostgresProducts(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL is unset; use the Compose api-test service for integration tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	products, err := (postgresStore{pool}).ListProducts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := []Product{{1, "旬の野菜セット", 980, 10}, {2, "北海道ミルク", 320, 20}, {3, "焼きたて食パン", 480, 0}}
	if len(products) != len(want) {
		t.Fatalf("products = %+v", products)
	}
	for i := range want {
		if products[i] != want[i] {
			t.Fatalf("product %d = %+v, want %+v", i, products[i], want[i])
		}
	}
}
