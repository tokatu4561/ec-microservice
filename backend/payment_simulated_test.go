package main

// 学習2のSQL比較と既存のOrder単体DBテストだけに使う。
// 通常バイナリには含めず、Payment障害時のフォールバックにも使わない。
import (
	"context"
	"github.com/jackc/pgx/v5"
	"sort"
)

type simulatedPayments struct{}

func (simulatedPayments) Create(_ context.Context, _ string, _ int64, mode string) (string, error) {
	if mode == "fail" {
		return "failed", nil
	}
	return "succeeded", nil
}
func (simulatedPayments) Cancel(context.Context, string, int64) error { return nil }

func createOrderTx(ctx context.Context, tx pgx.Tx, id string, items []ItemInput, payment, shipping string) (Order, error) {
	return createOrderWithPaymentTx(ctx, tx, id, items, payment, shipping, simulatedPayments{})
}

// カート経由と旧単品APIで、同じ注文処理・同じロック順を使う。
func createOrderWithPaymentTx(ctx context.Context, tx pgx.Tx, id string, items []ItemInput, payment, shipping string, payments paymentGateway) (Order, error) {
	if len(items) == 0 || len(items) > 100 {
		return Order{}, errInvalid
	}
	items = append([]ItemInput(nil), items...)
	sort.Slice(items, func(i, j int) bool { return items[i].ProductID < items[j].ProductID })
	o := Order{ID: id, Items: []OrderItem{}, Status: "failed", PaymentStatus: "not_started", ShippingStatus: "not_started", InventoryStatus: "legacy"}
	shortage := false
	ids := make([]int64, len(items))
	for i, in := range items {
		if in.ProductID <= 0 || in.Quantity <= 0 || in.Quantity > 2147483647 || (i > 0 && items[i-1].ProductID == in.ProductID) {
			return Order{}, errInvalid
		}
		ids[i] = in.ProductID
	}
	// 全商品を一度で取得する。行ロックは従来と同じ商品ID昇順で取得する。
	rows, err := tx.Query(ctx, `SELECT id,name,price_yen,stock FROM products WHERE id=ANY($1::bigint[]) ORDER BY id FOR UPDATE`, ids)
	if err != nil {
		return Order{}, err
	}
	products := make([]Product, 0, len(items))
	for rows.Next() {
		var p Product
		if err := rows.Scan(&p.ID, &p.Name, &p.PriceYen, &p.Stock); err != nil {
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
		if err := adjustOrderStock(ctx, tx, o.Items, -1); err != nil {
			return Order{}, err
		}
		orderStep(ctx, id, "stock_reserved_in_transaction")
		if payments == nil {
			return Order{}, errPaymentUncertain
		}
		paymentResult, err := payments.Create(ctx, id, o.TotalYen, payment)
		if err != nil {
			return Order{}, err
		}
		if paymentResult == "failed" {
			o.PaymentStatus = "failed"
			reason = "payment_failed"
		} else {
			o.PaymentStatus = "succeeded"
			if shipping == "fail" {
				o.ShippingStatus = "failed"
				if err := payments.Cancel(ctx, id, o.TotalYen); err != nil {
					return Order{}, err
				}
				o.PaymentStatus = "cancelled"
				reason = "shipping_failed"
			} else {
				o.ShippingStatus = "requested"
				o.Status = "shipping_requested"
			}
		}
		if reason != "" {
			if err := adjustOrderStock(ctx, tx, o.Items, 1); err != nil {
				return Order{}, err
			}
			orderStep(ctx, id, "stock_restored_in_transaction")
		}
	}
	if reason != "" {
		o.FailureReason = &reason
	}
	err = tx.QueryRow(ctx, `INSERT INTO orders(id,status,failure_reason,payment_status,shipping_status) VALUES($1,$2,$3,$4,$5) RETURNING created_at`, id, o.Status, o.FailureReason, o.PaymentStatus, o.ShippingStatus).Scan(&o.CreatedAt)
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
	return o, err
}

func (simulatedPayments) Get(context.Context, string, int64) (string, error) {
	return "", errPaymentNotFound
}
