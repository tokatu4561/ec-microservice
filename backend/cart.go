package main

import (
	"context"
	"errors"
	"github.com/jackc/pgx/v5"
	"time"
)

var errConflict = errors.New("cart version conflict")
var errEmptyCart = errors.New("empty cart")

type CartItem struct {
	ProductID   int64  `json:"productId"`
	ProductName string `json:"productName"`
	PriceYen    int64  `json:"priceYen"`
	Quantity    int    `json:"quantity"`
	Stock       int    `json:"stock"`
	SubtotalYen int64  `json:"subtotalYen"`
}
type Cart struct {
	Version     int64      `json:"version"`
	Items       []CartItem `json:"items"`
	TotalYen    int64      `json:"totalYen"`
	ExpiresAt   time.Time  `json:"expiresAt"`
	LastOrderID *string    `json:"lastOrderId"`
}
type cartStore interface {
	NewCart(context.Context, string) (Cart, error)
	GetCart(context.Context, string) (Cart, error)
	SetCartItem(context.Context, string, int64, int64, int, bool) (Cart, error)
	Checkout(context.Context, string, int64, string, string, string) (Order, error)
}

func cartHeader(ctx context.Context, tx pgx.Tx, hash, lock string) (Cart, error) {
	c := Cart{Items: []CartItem{}}
	err := tx.QueryRow(ctx, `SELECT version,expires_at,last_order_id FROM carts WHERE token_hash=$1 AND expires_at>CURRENT_TIMESTAMP `+lock, hash).Scan(&c.Version, &c.ExpiresAt, &c.LastOrderID)
	if errors.Is(err, pgx.ErrNoRows) {
		return c, errNotFound
	}
	return c, err
}
func cartItems(ctx context.Context, tx pgx.Tx, hash string, c *Cart) error {
	rows, err := tx.Query(ctx, `SELECT p.id,p.name,p.price_yen,ci.quantity,p.stock FROM cart_items ci JOIN products p ON p.id=ci.product_id WHERE cart_hash=$1 ORDER BY product_id`, hash)
	if err != nil {
		return err
	}
	defer rows.Close()
	c.Items = []CartItem{}
	c.TotalYen = 0
	for rows.Next() {
		var item CartItem
		if err = rows.Scan(&item.ProductID, &item.ProductName, &item.PriceYen, &item.Quantity, &item.Stock); err != nil {
			return err
		}
		item.SubtotalYen, err = addAmount(&c.TotalYen, item.PriceYen, item.Quantity)
		if err != nil {
			return err
		}
		c.Items = append(c.Items, item)
	}
	return rows.Err()
}
func (s postgresStore) NewCart(ctx context.Context, hash string) (Cart, error) {
	c := Cart{Items: []CartItem{}}
	err := s.pool.QueryRow(ctx, `INSERT INTO carts(token_hash) VALUES($1) RETURNING version,expires_at`, hash).Scan(&c.Version, &c.ExpiresAt)
	return c, err
}
func (s postgresStore) GetCart(ctx context.Context, hash string) (Cart, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Cart{}, err
	}
	defer rollback(tx)
	c, err := cartHeader(ctx, tx, hash, "FOR SHARE")
	if err != nil {
		return c, err
	}
	if err = cartItems(ctx, tx, hash, &c); err != nil {
		return c, err
	}
	return c, tx.Commit(ctx)
}
func (s postgresStore) SetCartItem(ctx context.Context, hash string, version, productID int64, quantity int, remove bool) (Cart, error) {
	if version < 0 || productID <= 0 || (!remove && (quantity <= 0 || quantity > 2147483647)) {
		return Cart{}, errInvalid
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Cart{}, err
	}
	defer rollback(tx)
	c, err := cartHeader(ctx, tx, hash, "FOR UPDATE")
	if err != nil {
		return c, err
	}
	if c.Version != version {
		return c, errConflict
	}
	if remove {
		if _, err = tx.Exec(ctx, `DELETE FROM cart_items WHERE cart_hash=$1 AND product_id=$2`, hash, productID); err != nil {
			return c, err
		}
	} else {
		var exists bool
		if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM products WHERE id=$1)`, productID).Scan(&exists); err != nil {
			return c, err
		}
		if !exists {
			return c, errNotFound
		}
		if _, err = tx.Exec(ctx, `INSERT INTO cart_items(cart_hash,product_id,quantity) VALUES($1,$2,$3) ON CONFLICT(cart_hash,product_id) DO UPDATE SET quantity=EXCLUDED.quantity`, hash, productID, quantity); err != nil {
			return c, err
		}
	}
	if err = cartItems(ctx, tx, hash, &c); err != nil {
		return c, err
	}
	if len(c.Items) > 100 {
		return c, errInvalid
	}
	c.Version++
	if _, err = tx.Exec(ctx, `UPDATE carts SET version=$2 WHERE token_hash=$1`, hash, c.Version); err != nil {
		return c, err
	}
	return c, tx.Commit(ctx)
}
func (s postgresStore) Checkout(ctx context.Context, hash string, version int64, id, payment, shipping string) (Order, error) {
	if version < 0 {
		return Order{}, errInvalid
	}
	if err := validateModes(&payment, &shipping); err != nil {
		return Order{}, err
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return Order{}, err
	}
	defer rollback(tx)
	c, err := cartHeader(ctx, tx, hash, "FOR UPDATE")
	if err != nil {
		return Order{}, err
	}
	if c.Version != version {
		return Order{}, errConflict
	}
	rows, err := tx.Query(ctx, `SELECT product_id,quantity FROM cart_items WHERE cart_hash=$1 ORDER BY product_id`, hash)
	if err != nil {
		return Order{}, err
	}
	items := []ItemInput{}
	for rows.Next() {
		var item ItemInput
		if err = rows.Scan(&item.ProductID, &item.Quantity); err != nil {
			rows.Close()
			return Order{}, err
		}
		items = append(items, item)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return Order{}, err
	}
	if len(items) == 0 {
		return Order{}, errEmptyCart
	}
	o, err := createOrderTx(ctx, tx, id, items, payment, shipping)
	if err != nil {
		return Order{}, err
	}
	if o.Status == "shipping_requested" {
		if _, err = tx.Exec(ctx, `DELETE FROM cart_items WHERE cart_hash=$1`, hash); err != nil {
			return Order{}, err
		}
	}
	// 失敗注文も処理済みの版とする。同じ版での二重送信を受け付けない。
	if _, err = tx.Exec(ctx, `UPDATE carts SET version=version+1,last_order_id=$2 WHERE token_hash=$1`, hash, id); err != nil {
		return Order{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return Order{}, err
	}
	orderStep(ctx, id, "order_and_cart_committed")
	return o, nil
}
