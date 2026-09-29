package main

import (
	"context"
	_ "embed"
	"errors"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/001_payments.sql
var schema string

var (
	errInvalid     = errors.New("invalid payment input")
	errNotFound    = errors.New("payment not found")
	errConflict    = errors.New("payment already exists or cannot be cancelled")
	orderIDPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)
)

const maxSafeYen int64 = 9007199254740991

type Payment struct {
	OrderID   string    `json:"orderId"`
	AmountYen int64     `json:"amountYen"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

type PaymentInput struct {
	OrderID   string `json:"orderId"`
	AmountYen *int64 `json:"amountYen"`
	Mode      string `json:"mode"`
}

func (in *PaymentInput) validate() error {
	if !orderIDPattern.MatchString(in.OrderID) || in.AmountYen == nil || *in.AmountYen < 0 || *in.AmountYen > maxSafeYen {
		return errInvalid
	}
	if in.Mode == "" {
		in.Mode = "success"
	}
	if in.Mode != "success" && in.Mode != "fail" {
		return errInvalid
	}
	return nil
}

type paymentStore interface {
	Create(context.Context, PaymentInput) (Payment, error)
	Get(context.Context, string) (Payment, error)
	Cancel(context.Context, string) (Payment, error)
	Ping(context.Context) error
}

type postgresStore struct{ pool *pgxpool.Pool }

const paymentColumns = `order_id, amount_yen, status, created_at, updated_at`

func scanPayment(row pgx.Row) (Payment, error) {
	var p Payment
	err := row.Scan(&p.OrderID, &p.AmountYen, &p.Status, &p.CreatedAt, &p.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return p, errNotFound
	}
	return p, err
}

func (s postgresStore) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }

func (s postgresStore) Create(ctx context.Context, in PaymentInput) (out Payment, resultErr error) {
	ctx, span := startOperation(ctx, "payment", "payment.store.create", in.OrderID)
	defer func() { finishOperation(span, resultErr) }()
	if err := in.validate(); err != nil {
		return Payment{}, err
	}
	status := "succeeded"
	if in.Mode == "fail" {
		status = "failed"
	}
	// 同じ要求は現在の記録を返す。取消済みの決済を復活させない。
	p, err := scanPayment(s.pool.QueryRow(ctx, `INSERT INTO payments(order_id,amount_yen,status,request_mode) VALUES($1,$2,$3,$4)
        ON CONFLICT(order_id) DO UPDATE SET order_id=payments.order_id
        WHERE payments.amount_yen=EXCLUDED.amount_yen AND payments.request_mode=EXCLUDED.request_mode
        RETURNING `+paymentColumns, in.OrderID, *in.AmountYen, status, in.Mode))
	if errors.Is(err, errNotFound) {
		return Payment{}, errConflict
	}

	return p, err
}

func (s postgresStore) Get(ctx context.Context, id string) (out Payment, resultErr error) {
	ctx, span := startOperation(ctx, "payment", "payment.store.get", id)
	defer func() {
		if errors.Is(resultErr, errNotFound) {
			span.End()
		} else {
			finishOperation(span, resultErr)
		}
	}()
	if !orderIDPattern.MatchString(id) {
		return Payment{}, errInvalid
	}
	return scanPayment(s.pool.QueryRow(ctx, `SELECT `+paymentColumns+` FROM payments WHERE order_id=$1`, id))
}

func (s postgresStore) Cancel(ctx context.Context, id string) (out Payment, resultErr error) {
	ctx, span := startOperation(ctx, "payment", "payment.store.cancel", id)
	defer func() { finishOperation(span, resultErr) }()
	if !orderIDPattern.MatchString(id) {
		return Payment{}, errInvalid
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Payment{}, err
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = tx.Rollback(ctx)
	}()
	p, err := scanPayment(tx.QueryRow(ctx, `SELECT `+paymentColumns+` FROM payments WHERE order_id=$1 FOR UPDATE`, id))
	if err != nil {
		return Payment{}, err
	}
	if p.Status == "failed" {
		return Payment{}, errConflict
	}
	if p.Status == "succeeded" {
		p, err = scanPayment(tx.QueryRow(ctx, `UPDATE payments SET status='cancelled', updated_at=clock_timestamp() WHERE order_id=$1 RETURNING `+paymentColumns, id))
		if err != nil {
			return Payment{}, err
		}
	}
	// 再取消ではupdated_atも変更しない。並行取消は行ロックで直列化する。
	if err := tx.Commit(ctx); err != nil {
		return Payment{}, err
	}
	return p, nil
}
