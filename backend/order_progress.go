package main

import (
	"context"
	"errors"
	"fmt"
	"github.com/jackc/pgx/v5"
	"slices"
	"sort"
)

// 通信前に注文明細と予約要求を永続化する。InventoryへのHTTPはCOMMIT後。
func reserveOrderTx(ctx context.Context, tx pgx.Tx, id string, items []ItemInput, payment, shipping string, cartHash string) (out Order, resultErr error) {
	ctx, span := startOperation(ctx, "order", "order.reserve", id)
	defer func() { finishOperation(span, resultErr) }()
	if len(items) == 0 || len(items) > 100 {
		return Order{}, errInvalid
	}
	items = append([]ItemInput(nil), items...)
	sort.Slice(items, func(i, j int) bool { return items[i].ProductID < items[j].ProductID })
	o := Order{ID: id, Items: []OrderItem{}, Status: "processing", PaymentStatus: "not_started", ShippingStatus: "not_started", InventoryStatus: "pending"}
	ids := make([]int64, len(items))
	for i, in := range items {
		if in.ProductID <= 0 || in.Quantity <= 0 || in.Quantity > 2147483647 || (i > 0 && items[i-1].ProductID == in.ProductID) {
			return Order{}, errInvalid
		}
		ids[i] = in.ProductID
	}
	// 全商品を一度で取得する。行ロックは従来と同じ商品ID昇順で取得する。
	rows, err := tx.Query(ctx, `SELECT id,name,price_yen FROM products WHERE id=ANY($1::bigint[]) ORDER BY id FOR UPDATE`, ids)
	if err != nil {
		return Order{}, err
	}
	products := make([]Product, 0, len(items))
	for rows.Next() {
		var p Product
		if err := rows.Scan(&p.ID, &p.Name, &p.PriceYen); err != nil {
			rows.Close()
			return Order{}, err
		}
		products = append(products, p)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return Order{}, err
	}
	if len(products) != len(items) {
		return Order{}, errNotFound
	}
	for i, p := range products {
		in := items[i]
		item := OrderItem{ProductID: p.ID, ProductName: p.Name, Quantity: in.Quantity, PriceYen: p.PriceYen}
		item.SubtotalYen, err = addAmount(&o.TotalYen, p.PriceYen, in.Quantity)
		if err != nil {
			return Order{}, err
		}
		o.Items = append(o.Items, item)
	}
	orderStep(ctx, id, "all_products_locked")
	err = tx.QueryRow(ctx, `INSERT INTO orders(id,status,failure_reason,payment_status,shipping_status,inventory_status) VALUES($1,$2,$3,$4,$5,'pending') RETURNING created_at`, id, o.Status, o.FailureReason, o.PaymentStatus, o.ShippingStatus).Scan(&o.CreatedAt)
	if err != nil {
		return Order{}, err
	}
	names := make([]string, len(o.Items))
	quantities := make([]int32, len(o.Items))
	prices := make([]int64, len(o.Items))
	shortages := make([]bool, len(o.Items))
	for i, item := range o.Items {
		names[i], quantities[i], prices[i], shortages[i] = item.ProductName, int32(item.Quantity), item.PriceYen, item.StockShortage
	}
	_, err = tx.Exec(ctx, `INSERT INTO order_items(order_id,product_id,product_name,quantity,price_yen,stock_shortage)
		SELECT $1,product_id,product_name,quantity,price_yen,stock_shortage
		FROM unnest($2::bigint[],$3::text[],$4::integer[],$5::bigint[],$6::boolean[])
		AS item(product_id,product_name,quantity,price_yen,stock_shortage) ORDER BY product_id`,
		id, ids, names, quantities, prices, shortages)
	if err != nil {
		return o, err
	}
	if o.Status == "processing" {
		_, err = tx.Exec(ctx, `INSERT INTO order_progress(order_id,payment_mode,shipping_mode,cart_hash,stock_held,phase) VALUES($1,$2,$3,NULLIF($4,''),false,'inventory_pending')`, id, payment, shipping, cartHash)
	}
	return o, err
}

func (s postgresStore) finalizeOrder(ctx context.Context, o Order) (out Order, resultErr error) {
	ctx, span := startOperation(ctx, "order", "order.finalize", o.ID)
	defer func() { finishOperation(span, resultErr) }()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Order{}, err
	}
	defer rollback(tx)
	saved, err := scanOrder(tx.QueryRow(ctx, `SELECT `+orderColumns+` FROM orders WHERE id=$1 FOR UPDATE`, o.ID))
	if err != nil {
		return Order{}, err
	}
	if saved.Status == "failed" || saved.Status == "shipping_requested" {
		// 他の再開がすでに確定。再度在庫・カートを操作しない。
		if err = tx.Commit(ctx); err != nil {
			return Order{}, err
		}
		return s.GetOrder(ctx, o.ID)
	}
	var hash *string
	if err = tx.QueryRow(ctx, `SELECT cart_hash FROM order_progress WHERE order_id=$1`, o.ID).Scan(&hash); err != nil {
		return Order{}, err
	}
	if hash != nil {
		// 受付と同じくカート→商品順。期限後でも既存注文の確定を可能にする。
		if _, err = tx.Exec(ctx, `SELECT token_hash FROM carts WHERE token_hash=$1 FOR UPDATE`, *hash); err != nil {
			return Order{}, err
		}
	}
	if _, err = tx.Exec(ctx, `UPDATE orders SET status=$2,failure_reason=$3,payment_status=$4,shipping_status=$5,inventory_status=$6 WHERE id=$1`, o.ID, o.Status, o.FailureReason, o.PaymentStatus, o.ShippingStatus, o.InventoryStatus); err != nil {
		return Order{}, err
	}
	if _, err = tx.Exec(ctx, `UPDATE order_progress SET stock_held=false,phase='done' WHERE order_id=$1`, o.ID); err != nil {
		return Order{}, err
	}
	if hash != nil {
		if o.Status == "shipping_requested" {
			if _, err = tx.Exec(ctx, `DELETE FROM cart_items WHERE cart_hash=$1`, *hash); err != nil {
				return Order{}, err
			}
		}
		if _, err = tx.Exec(ctx, `UPDATE carts SET pending_order_id=NULL WHERE token_hash=$1 AND pending_order_id=$2`, *hash, o.ID); err != nil {
			return Order{}, err
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return Order{}, err
	}
	orderStep(ctx, o.ID, "order_finalized")
	return o, nil
}

// 進行段階を比較して更新する。通信後の古い結果で新しい状態を上書きしない。
func (s postgresStore) advanceOrder(ctx context.Context, from, to string, o Order, reason string, shortages []int64) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer rollback(tx)
	var status string
	if err = tx.QueryRow(ctx, `SELECT status FROM orders WHERE id=$1 FOR UPDATE`, o.ID).Scan(&status); err != nil {
		return err
	}
	var current string
	if err = tx.QueryRow(ctx, `SELECT phase FROM order_progress WHERE order_id=$1`, o.ID).Scan(&current); err != nil {
		return err
	}
	if current != from || status == "failed" || status == "shipping_requested" {
		return tx.Commit(ctx)
	}
	if _, err = tx.Exec(ctx, `UPDATE orders SET status=$2,payment_status=$3,shipping_status=$4,inventory_status=$5 WHERE id=$1`, o.ID, o.Status, o.PaymentStatus, o.ShippingStatus, o.InventoryStatus); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE order_progress SET phase=$2,failure_reason=NULLIF($3,''),stock_held=$4 WHERE order_id=$1`, o.ID, to, reason, slices.Contains([]string{"reserved", "commit_pending", "release_pending"}, o.InventoryStatus)); err != nil {
		return err
	}
	if shortages != nil {
		if _, err = tx.Exec(ctx, `UPDATE order_items SET stock_shortage=(product_id=ANY($2::bigint[])) WHERE order_id=$1`, o.ID, shortages); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}
func (s postgresStore) ResumeOrder(ctx context.Context, id string) (out Order, resultErr error) {
	ctx, span := startOperation(ctx, "order", "order.resume", id)
	defer func() { finishOperation(span, resultErr) }()
	for {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		o, err := s.GetOrder(ctx, id)
		if err != nil {
			return o, err
		}
		if o.Status == "failed" || o.Status == "shipping_requested" {
			return o, nil
		}
		var phase, pay, ship string
		var reason *string
		if err = s.pool.QueryRow(ctx, `SELECT phase,payment_mode,shipping_mode,failure_reason FROM order_progress WHERE order_id=$1`, id).Scan(&phase, &pay, &ship, &reason); err != nil {
			return o, err
		}
		// phaseに対応する状態を読み直す。次へ進む前にもDB内でphase一致を確認する。
		o, err = s.GetOrder(ctx, id)
		if err != nil {
			return o, err
		}
		if o.Status == "failed" || o.Status == "shipping_requested" {
			return o, nil
		}
		items := make([]shipmentItem, len(o.Items))
		for i, item := range o.Items {
			items[i] = shipmentItem{item.ProductID, item.Quantity}
		}
		next, why := "", ""
		if reason != nil {
			why = *reason
		}
		var shortages []int64
		switch phase {
		case "inventory_pending":
			if s.inventory == nil {
				return o, errInventoryUncertain
			}
			var r inventoryResult
			r, err = s.inventory.Get(ctx, id, items)
			if errors.Is(err, errInventoryNotFound) {
				r, err = s.inventory.Reserve(ctx, id, items)
			}
			if err == nil {
				switch r.Status {
				case "reserved":
					o.InventoryStatus = "reserved"
					o.PaymentStatus = "pending"
					next = "payment_pending"
				case "rejected":
					o.InventoryStatus = "rejected"
					next, why = "finalize_failure", "out_of_stock"
					shortages = r.Shortages
				default:
					err = errInventoryUncertain
				}
			}
		case "payment_pending":
			if s.payments == nil {
				return o, errPaymentUncertain
			}
			var r string
			r, err = s.payments.Get(ctx, id, o.TotalYen)
			if errors.Is(err, errPaymentNotFound) {
				r, err = s.payments.Create(ctx, id, o.TotalYen, pay)
			}
			if err == nil {
				switch r {
				case "succeeded":
					o.PaymentStatus = "succeeded"
					o.ShippingStatus = "pending"
					next = "shipping_pending"
				case "failed":
					o.PaymentStatus = "failed"
					o.InventoryStatus = "release_pending"
					o.Status = "release_pending"
					next, why = "inventory_release_pending", "payment_failed"
				default:
					err = errPaymentUncertain
				}
			}
		case "shipping_pending":
			if s.shipping == nil {
				return o, errShippingUncertain
			}
			var r string
			r, err = s.shipping.Get(ctx, id, items, ship)
			if errors.Is(err, errShippingNotFound) {
				r, err = s.shipping.Create(ctx, id, items, ship)
			}
			if err == nil {
				switch r {
				case "requested":
					o.ShippingStatus = "requested"
					o.InventoryStatus = "commit_pending"
					next = "inventory_commit_pending"
				case "failed":
					o.Status = "cancel_pending"
					o.ShippingStatus = "failed"
					next, why = "payment_cancel_pending", "shipping_failed"
				default:
					err = errShippingUncertain
				}
			}
		case "payment_cancel_pending":
			if s.payments == nil {
				return o, errPaymentUncertain
			}
			err = s.payments.Cancel(ctx, id, o.TotalYen)
			if err == nil {
				o.PaymentStatus = "cancelled"
				o.InventoryStatus = "release_pending"
				o.Status = "release_pending"
				next, why = "inventory_release_pending", "shipping_failed"
			}
		case "inventory_release_pending":
			if s.inventory == nil {
				return o, errInventoryUncertain
			}
			_, err = s.inventory.Release(ctx, id, items)
			if err == nil {
				o.InventoryStatus = "released"
				next = "finalize_failure"
			}
		case "inventory_commit_pending":
			if s.inventory == nil {
				return o, errInventoryUncertain
			}
			_, err = s.inventory.Commit(ctx, id, items)
			if err == nil {
				o.InventoryStatus = "committed"
				next = "finalize_success"
			}
		case "finalize_success":
			o.Status = "shipping_requested"
			return s.finalizeOrder(ctx, o)
		case "finalize_failure":
			if why == "" {
				return o, errInventoryUncertain
			}
			o.Status = "failed"
			o.FailureReason = &why
			return s.finalizeOrder(ctx, o)
		default:
			return o, fmt.Errorf("inventory cutover required: phase %q", phase)
		}
		if err != nil {
			// 他の再開が進めた場合だけ最新段階を読む。通信失敗そのものの自動再送はしない。
			var current string
			if e := s.pool.QueryRow(ctx, `SELECT phase FROM order_progress WHERE order_id=$1`, id).Scan(&current); e == nil && current != phase {
				continue
			}
			return o, err
		}
		if err = s.advanceOrder(ctx, phase, next, o, why, shortages); err != nil {
			return o, err
		}
		orderStep(ctx, id, next)
	}
}
