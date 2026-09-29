package main

// 学習2の比較用。基準1c11b61の注文処理を保持し、通常バイナリには含めない。
import (
	"context"
	"errors"
	"github.com/jackc/pgx/v5"
	"sort"
)

func createOrderTxBaseline(ctx context.Context, tx pgx.Tx, id string, items []ItemInput, payment, shipping string) (Order, error) {
	if len(items) == 0 || len(items) > 100 {
		return Order{}, errInvalid
	}
	items = append([]ItemInput(nil), items...)
	sort.Slice(items, func(i, j int) bool { return items[i].ProductID < items[j].ProductID })
	o := Order{ID: id, Items: []OrderItem{}, Status: "failed", PaymentStatus: "not_started", ShippingStatus: "not_started", InventoryStatus: "legacy"}
	shortage := false
	for i, in := range items {
		if in.ProductID <= 0 || in.Quantity <= 0 || in.Quantity > 2147483647 || (i > 0 && items[i-1].ProductID == in.ProductID) {
			return Order{}, errInvalid
		}
		var p Product
		err := tx.QueryRow(ctx, `SELECT id,name,price_yen,stock FROM products WHERE id=$1 FOR UPDATE`, in.ProductID).Scan(&p.ID, &p.Name, &p.PriceYen, &p.Stock)
		if errors.Is(err, pgx.ErrNoRows) {
			return Order{}, errNotFound
		}
		if err != nil {
			return Order{}, err
		}
		item := OrderItem{ProductID: p.ID, ProductName: p.Name, Quantity: in.Quantity, PriceYen: p.PriceYen, StockShortage: p.Stock < in.Quantity}
		item.SubtotalYen, err = addAmount(&o.TotalYen, p.PriceYen, in.Quantity)
		if err != nil {
			return Order{}, err
		}
		shortage = shortage || item.StockShortage
		o.Items = append(o.Items, item)
	}
	orderStep(ctx, id, "all_products_locked")
	reason := ""
	if shortage {
		reason = "out_of_stock"
	} else {
		for _, item := range o.Items {
			if _, err := tx.Exec(ctx, `UPDATE products SET stock=stock-$1 WHERE id=$2`, item.Quantity, item.ProductID); err != nil {
				return Order{}, err
			}
		}
		orderStep(ctx, id, "stock_reserved_in_transaction")
		if payment == "fail" {
			o.PaymentStatus = "failed"
			reason = "payment_failed"
		} else {
			o.PaymentStatus = "succeeded"
			if shipping == "fail" {
				o.ShippingStatus = "failed"
				o.PaymentStatus = "cancelled"
				reason = "shipping_failed"
			} else {
				o.ShippingStatus = "requested"
				o.Status = "shipping_requested"
			}
		}
		if reason != "" {
			for _, item := range o.Items {
				if _, err := tx.Exec(ctx, `UPDATE products SET stock=stock+$1 WHERE id=$2`, item.Quantity, item.ProductID); err != nil {
					return Order{}, err
				}
			}
			orderStep(ctx, id, "stock_restored_in_transaction")
		}
	}
	if reason != "" {
		o.FailureReason = &reason
	}
	err := tx.QueryRow(ctx, `INSERT INTO orders(id,status,failure_reason,payment_status,shipping_status) VALUES($1,$2,$3,$4,$5) RETURNING created_at`, id, o.Status, o.FailureReason, o.PaymentStatus, o.ShippingStatus).Scan(&o.CreatedAt)
	if err != nil {
		return Order{}, err
	}
	for _, item := range o.Items {
		if _, err = tx.Exec(ctx, `INSERT INTO order_items(order_id,product_id,product_name,quantity,price_yen,stock_shortage) VALUES($1,$2,$3,$4,$5,$6)`, id, item.ProductID, item.ProductName, item.Quantity, item.PriceYen, item.StockShortage); err != nil {
			return Order{}, err
		}
	}
	return o, nil
}
