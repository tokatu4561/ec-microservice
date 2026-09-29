package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"os"
	"time"
)

type importSnapshot struct {
	Stocks       []Stock            `json:"stocks"`
	Reservations []ReservationInput `json:"reservations"`
}

// オフライン移行専用。通常のInventoryプロセスにはOrder DBの接続情報を渡さない。
func importFromOrder(ctx context.Context, source, dest *pgxpool.Pool) error {
	if _, err := dest.Exec(ctx, schema); err != nil {
		return err
	}
	tx, err := source.Begin(ctx)
	if err != nil {
		return err
	}
	defer rollback(tx)
	if _, err = tx.Exec(ctx, `LOCK TABLE products,orders,order_items,order_progress,carts IN SHARE ROW EXCLUSIVE MODE`); err != nil {
		return err
	}
	var marker string
	err = tx.QueryRow(ctx, `SELECT fingerprint FROM inventory_cutover WHERE singleton`).Scan(&marker)
	if err == nil {
		var imported string
		if err = dest.QueryRow(ctx, `SELECT fingerprint FROM inventory_import WHERE singleton`).Scan(&imported); err != nil {
			return err
		}
		if imported != marker {
			return errors.New("inventory migration identity mismatch")
		}
		return tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	snapshot := importSnapshot{Stocks: []Stock{}, Reservations: []ReservationInput{}}
	rows, err := tx.Query(ctx, `SELECT id,stock FROM products ORDER BY id`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var s Stock
		if err = rows.Scan(&s.ProductID, &s.Available); err != nil {
			rows.Close()
			return err
		}
		snapshot.Stocks = append(snapshot.Stocks, s)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	// 初回切替前に新しい注文が処理されていないことを確認する。
	var invalid int
	if err = tx.QueryRow(ctx, `SELECT count(*) FROM orders o LEFT JOIN order_progress p ON p.order_id=o.id
 WHERE o.status NOT IN ('failed','shipping_requested') AND (p.order_id IS NULL OR NOT p.stock_held OR p.phase<>'legacy')`).Scan(&invalid); err != nil {
		return err
	}
	if invalid != 0 {
		return errors.New("pending orders do not match legacy reservations; migration stopped")
	}
	rows, err = tx.Query(ctx, `SELECT p.order_id,jsonb_agg(jsonb_build_object('productId',i.product_id,'quantity',i.quantity) ORDER BY i.product_id)
 FROM order_progress p JOIN orders o ON o.id=p.order_id JOIN order_items i ON i.order_id=p.order_id
 WHERE p.stock_held AND o.status NOT IN ('failed','shipping_requested') GROUP BY p.order_id ORDER BY p.order_id`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var r ReservationInput
		if err = rows.Scan(&r.OrderID, &r.Items); err != nil {
			rows.Close()
			return err
		}
		snapshot.Reservations = append(snapshot.Reservations, r)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	raw, err := json.Marshal(snapshot)
	if err != nil {
		return err
	}
	hash := sha256.Sum256(raw)
	fingerprint := hex.EncodeToString(hash[:])
	if err = importSnapshotData(ctx, dest, snapshot, fingerprint); err != nil {
		return err
	}
	// Inventoryの保存後にOrderを切り替える。途中停止時は同じfingerprintの再実行のみ許可。
	if _, err = tx.Exec(ctx, `UPDATE order_progress p SET phase=CASE WHEN o.status='cancel_pending' THEN 'payment_cancel_pending' WHEN o.payment_status='succeeded' THEN 'shipping_pending' ELSE 'payment_pending' END
 FROM orders o WHERE p.order_id=o.id AND p.stock_held AND p.phase='legacy' AND o.status NOT IN ('failed','shipping_requested');
 UPDATE orders SET inventory_status='reserved' WHERE id IN (SELECT order_id FROM order_progress WHERE stock_held AND phase<>'legacy');`); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO inventory_cutover(singleton,fingerprint) VALUES(true,$1)`, fingerprint); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
func importSnapshotData(ctx context.Context, pool *pgxpool.Pool, snapshot importSnapshot, fingerprint string) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer rollback(tx)
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(2026092701)`); err != nil {
		return err
	}
	var prior string
	err = tx.QueryRow(ctx, `SELECT fingerprint FROM inventory_import WHERE singleton`).Scan(&prior)
	if err == nil {
		if prior != fingerprint {
			return errors.New("refusing to overwrite a different inventory import")
		}
		return tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	var count int
	if err = tx.QueryRow(ctx, `SELECT (SELECT count(*) FROM stocks)+(SELECT count(*) FROM reservations)`).Scan(&count); err != nil {
		return err
	}
	if count != 0 {
		return errors.New("inventory destination is not empty")
	}
	for _, s := range snapshot.Stocks {
		if _, err = tx.Exec(ctx, `INSERT INTO stocks(product_id,available) VALUES($1,$2)`, s.ProductID, s.Available); err != nil {
			return err
		}
	}
	for _, r := range snapshot.Reservations {
		if err = r.validate(); err != nil {
			return err
		}
		items, _ := json.Marshal(r.Items)
		// 移行元stockは予約分を既に控除済み。予約記録だけを作成する。
		if _, err = tx.Exec(ctx, `INSERT INTO reservations(order_id,items,status) VALUES($1,$2::jsonb,'reserved')`, r.OrderID, string(items)); err != nil {
			return err
		}
	}
	if _, err = tx.Exec(ctx, `INSERT INTO inventory_import(singleton,fingerprint) VALUES(true,$1)`, fingerprint); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
func runImport() error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if os.Getenv("SOURCE_DATABASE_URL") == "" || os.Getenv("DATABASE_URL") == "" {
		return fmt.Errorf("SOURCE_DATABASE_URL and DATABASE_URL required for offline import")
	}
	source, err := pgxpool.New(ctx, os.Getenv("SOURCE_DATABASE_URL"))
	if err != nil {
		return err
	}
	defer source.Close()
	dest, err := pgxpool.New(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		return err
	}
	defer dest.Close()
	return importFromOrder(ctx, source, dest)
}
