package main

import (
	"context"
	"github.com/jackc/pgx/v5/pgxpool"
	"os"
	"testing"
)

func inventoryTestClient(t *testing.T) *httpInventory {
	t.Helper()
	p, e := newHTTPInventory(os.Getenv("TEST_INVENTORY_URL"))
	if e != nil {
		t.Fatal(e)
	}
	return p
}
func inventoryTestDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("TEST_INVENTORY_DATABASE_URL")
	if url == "" {
		t.Fatal("TEST_INVENTORY_DATABASE_URL required")
	}
	p, e := pgxpool.New(context.Background(), url)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(p.Close)
	return p
}
func seedInventory(t *testing.T, id int64, stock int) {
	t.Helper()
	p := inventoryTestDB(t)
	if _, e := p.Exec(context.Background(), `INSERT INTO stocks(product_id,available) VALUES($1,$2)`, id, stock); e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() {
		if _, e := p.Exec(context.Background(), `DELETE FROM stocks WHERE product_id=$1`, id); e != nil {
			t.Error(e)
		}
	})
}
