package main

import (
	"context"
	"crypto/rand"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type Product struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	PriceYen   int64  `json:"priceYen"`
	Stock      int    `json:"stock"`
	StockKnown bool   `json:"stockKnown"`
}

type productStore interface {
	ListProducts(context.Context) ([]Product, error)
	CreateOrder(context.Context, string, OrderInput) (Order, error)
	GetOrder(context.Context, string) (Order, error)
}

type postgresStore struct {
	pool      *pgxpool.Pool
	payments  paymentGateway
	shipping  shippingGateway
	inventory inventoryGateway
}

func (s postgresStore) ListProducts(ctx context.Context) ([]Product, error) {
	// 商品データの読み取りはGoが担当する。金額は整数の円で扱う。
	rows, err := s.pool.Query(ctx, `SELECT id, name, price_yen FROM products ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	products := make([]Product, 0)
	for rows.Next() {
		var p Product
		if err := rows.Scan(&p.ID, &p.Name, &p.PriceYen); err != nil {
			return nil, err
		}
		products = append(products, p)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	ids := make([]int64, len(products))
	for i, p := range products {
		ids[i] = p.ID
	}
	if s.inventory != nil {
		if stocks, e := s.inventory.Stocks(ctx, ids); e == nil {
			for i := range products {
				products[i].Stock = stocks[products[i].ID]
				products[i].StockKnown = true
			}
		}
	}
	return products, nil
}

func randomID() []byte {
	// crypto/rand.Readは要求したバイト数を満たす。失敗時はプロセスを停止する。
	id := make([]byte, 16)
	_, _ = rand.Read(id)
	return id
}

func run(logger *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	shutdownTracing, err := initTracing(ctx, "order")
	if err != nil {
		return fmt.Errorf("initialize tracing: %w", err)
	}
	defer shutdownTracing()
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		return fmt.Errorf("DATABASE_URL is required")
	}
	payments, err := newHTTPPayments(os.Getenv("PAYMENT_BASE_URL"))
	if err != nil {
		return err
	}
	shipping, err := newHTTPShipping(os.Getenv("SHIPPING_BASE_URL"))
	if err != nil {
		return err
	}
	inventory, err := newHTTPInventory(os.Getenv("INVENTORY_BASE_URL"))
	if err != nil {
		return err
	}
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return err
	}
	defer pool.Close()
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	err = pool.Ping(pingCtx)
	cancel()
	if err != nil {
		return fmt.Errorf("connect database: %w", err)
	}
	var cutover bool
	if err = pool.QueryRow(ctx, `SELECT singleton FROM inventory_cutover WHERE singleton`).Scan(&cutover); err != nil {
		return fmt.Errorf("inventory migration required: %w", err)
	}
	server := &http.Server{
		Addr: ":8080", Handler: newHandler(postgresStore{pool: pool, payments: payments, shipping: shipping, inventory: inventory}, logger),
		ReadHeaderTimeout: 5 * time.Second, WriteTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second,
	}
	errors := make(chan error, 1)
	go func() {
		logger.Info("api listening", "address", server.Addr)
		errors <- server.ListenAndServe()
	}()
	select {
	case err := <-errors:
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			_ = server.Close()
			return err
		}
		return nil
	}
}

func main() {
	logger := slog.New(traceLogHandler{slog.NewJSONHandler(os.Stdout, nil)})
	slog.SetDefault(logger)
	if err := run(logger); err != nil {
		logger.Error("api stopped", "error", err)
		os.Exit(1)
	}
}
