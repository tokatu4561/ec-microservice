package main

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
)

var errNotFound = errors.New("not found")
var errInvalid = errors.New("invalid input")

const maxSafeYen int64 = 9007199254740991

type OrderInput struct {
	ProductID    int64  `json:"productId"`
	Quantity     int    `json:"quantity"`
	PaymentMode  string `json:"paymentMode"`
	ShippingMode string `json:"shippingMode"`
}

func validateModes(payment, shipping *string) error {
	if *payment == "" {
		*payment = "success"
	}
	if *shipping == "" {
		*shipping = "success"
	}
	if (*payment != "success" && *payment != "fail") || (*shipping != "success" && *shipping != "fail") {
		return errInvalid
	}
	return nil
}
func (in *OrderInput) validate() error {
	if in.ProductID <= 0 || in.Quantity <= 0 || in.Quantity > 2147483647 {
		return errInvalid
	}
	return validateModes(&in.PaymentMode, &in.ShippingMode)
}

type ItemInput struct {
	ProductID int64
	Quantity  int
}
type OrderItem struct {
	ProductID     int64  `json:"productId"`
	ProductName   string `json:"productName"`
	Quantity      int    `json:"quantity"`
	PriceYen      int64  `json:"priceYen"`
	StockShortage bool   `json:"stockShortage"`
	SubtotalYen   int64  `json:"subtotalYen"`
}
type Order struct {
	ID              string      `json:"id"`
	Items           []OrderItem `json:"items"`
	TotalYen        int64       `json:"totalYen"`
	Status          string      `json:"status"`
	FailureReason   *string     `json:"failureReason"`
	PaymentStatus   string      `json:"paymentStatus"`
	InventoryStatus string      `json:"inventoryStatus"`
	ShippingStatus  string      `json:"shippingStatus"`
	CreatedAt       time.Time   `json:"createdAt"`
}

const orderColumns = `id,status,failure_reason,payment_status,shipping_status,inventory_status,created_at`

func scanOrder(row pgx.Row) (Order, error) {
	o := Order{Items: []OrderItem{}}
	err := row.Scan(&o.ID, &o.Status, &o.FailureReason, &o.PaymentStatus, &o.ShippingStatus, &o.InventoryStatus, &o.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return o, errNotFound
	}
	return o, err
}
func addAmount(total *int64, price int64, quantity int) (int64, error) {
	if price < 0 || quantity <= 0 || price > maxSafeYen/int64(quantity) {
		return 0, errInvalid
	}
	subtotal := price * int64(quantity)
	if *total > maxSafeYen-subtotal {
		return 0, errInvalid
	}
	*total += subtotal
	return subtotal, nil
}
func (s postgresStore) GetOrder(ctx context.Context, id string) (Order, error) {
	o, err := scanOrder(s.pool.QueryRow(ctx, `SELECT `+orderColumns+` FROM orders WHERE id=$1`, id))
	if err != nil {
		return o, err
	}
	rows, err := s.pool.Query(ctx, `SELECT product_id,product_name,quantity,price_yen,stock_shortage FROM order_items WHERE order_id=$1 ORDER BY product_id`, id)
	if err != nil {
		return o, err
	}
	defer rows.Close()
	for rows.Next() {
		var item OrderItem
		if err = rows.Scan(&item.ProductID, &item.ProductName, &item.Quantity, &item.PriceYen, &item.StockShortage); err != nil {
			return o, err
		}
		item.SubtotalYen, err = addAmount(&o.TotalYen, item.PriceYen, item.Quantity)
		if err != nil {
			return o, err
		}
		o.Items = append(o.Items, item)
	}
	return o, rows.Err()
}
func orderStep(ctx context.Context, id, event string) {
	requestID, _ := ctx.Value(requestIDKey{}).(string)
	slog.InfoContext(ctx, event, "service", "order", "request_id", requestID, "order_id", id)
}
func rollback(tx pgx.Tx) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = tx.Rollback(ctx)
}
func (s postgresStore) CreateOrder(ctx context.Context, id string, in OrderInput) (out Order, resultErr error) {
	ctx, span := startOperation(ctx, "order", "order.accept", id)
	defer func() { finishOperation(span, resultErr) }()
	if err := in.validate(); err != nil {
		return Order{}, err
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return Order{}, err
	}
	defer rollback(tx)
	o, err := reserveOrderTx(ctx, tx, id, []ItemInput{{in.ProductID, in.Quantity}}, in.PaymentMode, in.ShippingMode, "")
	if err != nil {
		return Order{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return Order{}, err
	}
	orderStep(ctx, id, "order_committed")
	if o.Status == "failed" {
		return o, nil
	}
	return s.ResumeOrder(ctx, id)
}
