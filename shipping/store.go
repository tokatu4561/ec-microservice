package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"regexp"
	"sort"
	"time"
)

//go:embed migrations/001_shipments.sql
var schema string
var (
	errInvalid     = errors.New("invalid shipment input")
	errNotFound    = errors.New("shipment not found")
	errConflict    = errors.New("shipment content differs or cannot be cancelled")
	orderIDPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)
)

type ShipmentItem struct {
	ProductID int64 `json:"productId"`
	Quantity  int   `json:"quantity"`
}
type ShipmentInput struct {
	OrderID string         `json:"orderId"`
	Items   []ShipmentItem `json:"items"`
	Mode    string         `json:"mode"`
}
type Shipment struct {
	OrderID   string         `json:"orderId"`
	Items     []ShipmentItem `json:"items"`
	Mode      string         `json:"mode"`
	Status    string         `json:"status"`
	CreatedAt time.Time      `json:"createdAt"`
	UpdatedAt time.Time      `json:"updatedAt"`
}

func (in *ShipmentInput) validate() error {
	if !orderIDPattern.MatchString(in.OrderID) || len(in.Items) == 0 || len(in.Items) > 100 {
		return errInvalid
	}
	if in.Mode == "" {
		in.Mode = "success"
	}
	if in.Mode != "success" && in.Mode != "fail" {
		return errInvalid
	}
	in.Items = append([]ShipmentItem(nil), in.Items...)
	sort.Slice(in.Items, func(i, j int) bool { return in.Items[i].ProductID < in.Items[j].ProductID })
	for i, item := range in.Items {
		if item.ProductID <= 0 || item.Quantity <= 0 || item.Quantity > 2147483647 || (i > 0 && in.Items[i-1].ProductID == item.ProductID) {
			return errInvalid
		}
	}
	return nil
}

type shipmentStore interface {
	Create(context.Context, ShipmentInput) (Shipment, error)
	Get(context.Context, string) (Shipment, error)
	Cancel(context.Context, string) (Shipment, error)
	Ping(context.Context) error
}
type postgresStore struct{ pool *pgxpool.Pool }

const shipmentColumns = `order_id, items, request_mode, status, created_at, updated_at`

func scanShipment(row pgx.Row) (Shipment, error) {
	var s Shipment
	err := row.Scan(&s.OrderID, &s.Items, &s.Mode, &s.Status, &s.CreatedAt, &s.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return s, errNotFound
	}
	return s, err
}
func (s postgresStore) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }
func (s postgresStore) Create(ctx context.Context, in ShipmentInput) (out Shipment, resultErr error) {
	ctx, span := startOperation(ctx, "shipping", "shipping.store.create", in.OrderID)
	defer func() { finishOperation(span, resultErr) }()
	if err := in.validate(); err != nil {
		return Shipment{}, err
	}
	items, err := json.Marshal(in.Items)
	if err != nil {
		return Shipment{}, err
	}
	status := "requested"
	if in.Mode == "fail" {
		status = "failed"
	}
	// 一注文一配送。取消後の再送でも元の記録を返し、復活させない。
	out, err = scanShipment(s.pool.QueryRow(ctx, `INSERT INTO shipments(order_id,items,request_mode,status) VALUES($1,$2::jsonb,$3,$4)
 ON CONFLICT(order_id) DO UPDATE SET order_id=shipments.order_id
 WHERE shipments.items=EXCLUDED.items AND shipments.request_mode=EXCLUDED.request_mode
 RETURNING `+shipmentColumns, in.OrderID, string(items), in.Mode, status))
	if errors.Is(err, errNotFound) {
		return Shipment{}, errConflict
	}
	return out, err
}
func (s postgresStore) Get(ctx context.Context, id string) (out Shipment, resultErr error) {
	ctx, span := startOperation(ctx, "shipping", "shipping.store.get", id)
	defer func() {
		if errors.Is(resultErr, errNotFound) {
			span.End()
		} else {
			finishOperation(span, resultErr)
		}
	}()
	if !orderIDPattern.MatchString(id) {
		return Shipment{}, errInvalid
	}
	return scanShipment(s.pool.QueryRow(ctx, `SELECT `+shipmentColumns+` FROM shipments WHERE order_id=$1`, id))
}
func (s postgresStore) Cancel(ctx context.Context, id string) (out Shipment, resultErr error) {
	ctx, span := startOperation(ctx, "shipping", "shipping.store.cancel", id)
	defer func() { finishOperation(span, resultErr) }()
	if !orderIDPattern.MatchString(id) {
		return Shipment{}, errInvalid
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Shipment{}, err
	}
	defer func() {
		c, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = tx.Rollback(c)
	}()
	out, err = scanShipment(tx.QueryRow(ctx, `SELECT `+shipmentColumns+` FROM shipments WHERE order_id=$1 FOR UPDATE`, id))
	if err != nil {
		return Shipment{}, err
	}
	if out.Status == "failed" {
		return Shipment{}, errConflict
	}
	if out.Status == "requested" {
		out, err = scanShipment(tx.QueryRow(ctx, `UPDATE shipments SET status='cancelled',updated_at=clock_timestamp() WHERE order_id=$1 RETURNING `+shipmentColumns, id))
		if err != nil {
			return Shipment{}, err
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return Shipment{}, err
	}
	return out, nil
}
