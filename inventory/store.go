package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"regexp"
	"slices"
	"sort"
	"time"
)

//go:embed migrations/001_inventory.sql
var schema string
var (
	errInvalid     = errors.New("invalid inventory input")
	errNotFound    = errors.New("reservation not found")
	errConflict    = errors.New("reservation conflict")
	orderIDPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)
)

type ReservationItem struct {
	ProductID int64 `json:"productId"`
	Quantity  int   `json:"quantity"`
}
type ReservationInput struct {
	OrderID string            `json:"orderId"`
	Items   []ReservationItem `json:"items"`
}
type Reservation struct {
	OrderID   string            `json:"orderId"`
	Items     []ReservationItem `json:"items"`
	Status    string            `json:"status"`
	Shortages []int64           `json:"shortages"`
	CreatedAt time.Time         `json:"createdAt"`
	UpdatedAt time.Time         `json:"updatedAt"`
}
type Stock struct {
	ProductID int64 `json:"productId"`
	Available int64 `json:"available"`
}

func (in *ReservationInput) validate() error {
	if !orderIDPattern.MatchString(in.OrderID) || len(in.Items) == 0 || len(in.Items) > 100 {
		return errInvalid
	}
	in.Items = append([]ReservationItem(nil), in.Items...)
	sort.Slice(in.Items, func(i, j int) bool { return in.Items[i].ProductID < in.Items[j].ProductID })
	for i, item := range in.Items {
		if item.ProductID <= 0 || item.Quantity <= 0 || item.Quantity > 2147483647 || (i > 0 && in.Items[i-1].ProductID == item.ProductID) {
			return errInvalid
		}
	}
	return nil
}

type reservationStore interface {
	Create(context.Context, ReservationInput) (Reservation, error)
	Get(context.Context, string) (Reservation, error)
	Commit(context.Context, string) (Reservation, error)
	Release(context.Context, string) (Reservation, error)
	Stocks(context.Context, []int64) ([]Stock, error)
	Ping(context.Context) error
}
type postgresStore struct{ pool *pgxpool.Pool }

const reservationColumns = `order_id,items,status,shortages,created_at,updated_at`

func scanReservation(row pgx.Row) (Reservation, error) {
	var r Reservation
	err := row.Scan(&r.OrderID, &r.Items, &r.Status, &r.Shortages, &r.CreatedAt, &r.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return r, errNotFound
	}
	return r, err
}
func rollback(tx pgx.Tx) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = tx.Rollback(ctx)
}
func (s postgresStore) Ping(ctx context.Context) error {
	var ready bool
	return s.pool.QueryRow(ctx, `SELECT singleton FROM inventory_import WHERE singleton`).Scan(&ready)
}
func (s postgresStore) Stocks(ctx context.Context, ids []int64) ([]Stock, error) {
	if len(ids) == 0 || len(ids) > 100 {
		return nil, errInvalid
	}
	for _, id := range ids {
		if id <= 0 {
			return nil, errInvalid
		}
	}
	rows, err := s.pool.Query(ctx, `SELECT product_id,available FROM stocks WHERE product_id=ANY($1::bigint[]) ORDER BY product_id`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []Stock{}
	for rows.Next() {
		var x Stock
		if err = rows.Scan(&x.ProductID, &x.Available); err != nil {
			return nil, err
		}
		result = append(result, x)
	}
	return result, rows.Err()
}
func (s postgresStore) Get(ctx context.Context, id string) (out Reservation, resultErr error) {
	ctx, span := startOperation(ctx, "inventory", "inventory.store.get", id)
	defer func() {
		if errors.Is(resultErr, errNotFound) {
			span.End()
		} else {
			finishOperation(span, resultErr)
		}
	}()
	if !orderIDPattern.MatchString(id) {
		return out, errInvalid
	}
	return scanReservation(s.pool.QueryRow(ctx, `SELECT `+reservationColumns+` FROM reservations WHERE order_id=$1`, id))
}
func (s postgresStore) Create(ctx context.Context, in ReservationInput) (out Reservation, resultErr error) {
	ctx, span := startOperation(ctx, "inventory", "inventory.store.reserve", in.OrderID)
	defer func() { finishOperation(span, resultErr) }()
	if err := in.validate(); err != nil {
		return out, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return out, err
	}
	defer rollback(tx)
	// 同一予約を直列化。ハッシュ衝突は待機を増やすだけでデータの同一性は主キーで検証する。
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, in.OrderID); err != nil {
		return out, err
	}
	prior, err := scanReservation(tx.QueryRow(ctx, `SELECT `+reservationColumns+` FROM reservations WHERE order_id=$1`, in.OrderID))
	if err == nil {
		if !slices.Equal(prior.Items, in.Items) {
			return out, errConflict
		}
		return prior, tx.Commit(ctx)
	}
	if !errors.Is(err, errNotFound) {
		return out, err
	}
	ids := make([]int64, len(in.Items))
	for i, item := range in.Items {
		ids[i] = item.ProductID
	}
	rows, err := tx.Query(ctx, `SELECT product_id,available FROM stocks WHERE product_id=ANY($1::bigint[]) ORDER BY product_id FOR UPDATE`, ids)
	if err != nil {
		return out, err
	}
	available := map[int64]int64{}
	for rows.Next() {
		var id, n int64
		if err = rows.Scan(&id, &n); err != nil {
			rows.Close()
			return out, err
		}
		available[id] = n
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return out, err
	}
	// 未登録の商品は在庫0と断定せず、設定不整合として保留する。
	if len(available) != len(ids) {
		return out, errConflict
	}
	shortage := []int64{}
	for _, item := range in.Items {
		if available[item.ProductID] < int64(item.Quantity) {
			shortage = append(shortage, item.ProductID)
		}
	}
	status := "reserved"
	if len(shortage) > 0 {
		status = "rejected"
	} else {
		for _, item := range in.Items {
			if _, err = tx.Exec(ctx, `UPDATE stocks SET available=available-$2 WHERE product_id=$1`, item.ProductID, item.Quantity); err != nil {
				return out, err
			}
		}
	}
	items, _ := json.Marshal(in.Items)
	shortages, _ := json.Marshal(shortage)
	out, err = scanReservation(tx.QueryRow(ctx, `INSERT INTO reservations(order_id,items,status,shortages) VALUES($1,$2::jsonb,$3,$4::jsonb) RETURNING `+reservationColumns, in.OrderID, string(items), status, string(shortages)))
	if err != nil {
		return out, err
	}
	return out, tx.Commit(ctx)
}
func (s postgresStore) Commit(ctx context.Context, id string) (Reservation, error) {
	return s.transition(ctx, id, "committed")
}
func (s postgresStore) Release(ctx context.Context, id string) (Reservation, error) {
	return s.transition(ctx, id, "released")
}
func (s postgresStore) transition(ctx context.Context, id, target string) (out Reservation, resultErr error) {
	ctx, span := startOperation(ctx, "inventory", "inventory.store."+target, id)
	defer func() { finishOperation(span, resultErr) }()
	if !orderIDPattern.MatchString(id) {
		return out, errInvalid
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return out, err
	}
	defer rollback(tx)
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, id); err != nil {
		return out, err
	}
	out, err = scanReservation(tx.QueryRow(ctx, `SELECT `+reservationColumns+` FROM reservations WHERE order_id=$1 FOR UPDATE`, id))
	if err != nil {
		return out, err
	}
	if out.Status == target {
		return out, tx.Commit(ctx)
	}
	if out.Status != "reserved" {
		return out, errConflict
	}
	if target == "released" {
		// Createと同じ商品ID昇順でロックし、在庫解除を一度だけ反映する。
		for _, item := range out.Items {
			tag, e := tx.Exec(ctx, `UPDATE stocks SET available=available+$2 WHERE product_id=$1`, item.ProductID, item.Quantity)
			if e != nil {
				return out, e
			}
			if tag.RowsAffected() != 1 {
				return out, errConflict
			}
		}
	}
	out, err = scanReservation(tx.QueryRow(ctx, `UPDATE reservations SET status=$2,updated_at=clock_timestamp() WHERE order_id=$1 RETURNING `+reservationColumns, id, target))
	if err != nil {
		return out, err
	}
	return out, tx.Commit(ctx)
}
