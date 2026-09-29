package main

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestMigrationPostgres(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	name := fmt.Sprintf("migration_%x", randomID())
	quoted := pgx.Identifier{name}.Sanitize()
	if _, err = admin.Exec(ctx, "CREATE DATABASE "+quoted); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if _, err := admin.Exec(ctx, "DROP DATABASE "+quoted+" WITH (FORCE)"); err != nil {
			t.Error(err)
		}
	}()
	config, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	config.ConnConfig.Database = name
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	run := func(path string) {
		t.Helper()
		sql, err := os.ReadFile("/schema/" + path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = pool.Exec(ctx, string(sql)); err != nil {
			t.Fatal(err)
		}
	}
	run("init/001_products.sql")
	run("migrations/002_orders.sql")
	_, err = pool.Exec(ctx, `INSERT INTO orders(id,product_id,product_name,quantity,price_yen,status,payment_status,shipping_status,created_at) VALUES('11111111111111111111111111111111',1,'historical name',2,123,'shipping_requested','succeeded','requested','2026-01-02T03:04:05Z')`)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		run("migrations/002_orders.sql")
		run("migrations/003_cart_order_items.sql")
		run("migrations/004_order_progress.sql")
		run("migrations/005_shipping_progress.sql")
		run("migrations/006_inventory_progress.sql")
	}
	// 配送待ちと移行前の取消待ちを残したまま、全migrationを再適用できる。
	for i, state := range []struct{ status, payment, shipping string }{
		{"processing", "pending", "not_started"},
		{"processing", "succeeded", "pending"},
		{"cancel_pending", "succeeded", "failed"},
	} {
		id := fmt.Sprintf("%032x", i+2)
		if _, err = pool.Exec(ctx, `INSERT INTO orders(id,status,payment_status,shipping_status) VALUES($1,$2,$3,$4)`, id, state.status, state.payment, state.shipping); err != nil {
			t.Fatal(err)
		}
	}
	run("migrations/002_orders.sql")
	run("migrations/003_cart_order_items.sql")
	run("migrations/004_order_progress.sql")
	run("migrations/005_shipping_progress.sql")
	run("migrations/006_inventory_progress.sql")
	var preserved int
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM orders WHERE status IN ('processing','cancel_pending')`).Scan(&preserved); err != nil || preserved != 3 {
		t.Fatal(preserved, err)
	}
	var count int
	err = pool.QueryRow(ctx, `SELECT count(*) FROM orders o JOIN order_items i ON o.id=i.order_id JOIN products p ON p.id=i.product_id WHERE o.id='11111111111111111111111111111111' AND o.created_at='2026-01-02T03:04:05Z' AND o.status='shipping_requested' AND i.product_name='historical name' AND i.quantity=2 AND i.price_yen=123 AND p.stock=10`).Scan(&count)
	if err != nil || count != 1 {
		t.Fatalf("preserved=%d err=%v", count, err)
	}
	// 新規DB相当の空注文からの構築も検証する。
	_, err = pool.Exec(ctx, `DROP TABLE order_progress,carts,cart_items,order_items,orders CASCADE`)
	if err != nil {
		t.Fatal(err)
	}
	run("migrations/002_orders.sql")
	run("migrations/003_cart_order_items.sql")
	run("migrations/004_order_progress.sql")
	run("migrations/005_shipping_progress.sql")
	run("migrations/006_inventory_progress.sql")
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM order_items`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("fresh=%d err=%v", count, err)
	}
}
