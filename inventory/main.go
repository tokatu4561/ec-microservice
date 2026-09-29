package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func run(logger *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	shutdownTracing, err := initTracing(ctx, "inventory")
	if err != nil {
		return fmt.Errorf("initialize tracing: %w", err)
	}
	defer shutdownTracing()
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		return fmt.Errorf("DATABASE_URL is required")
	}
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return err
	}
	defer pool.Close()
	startupCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := pool.Ping(startupCtx); err != nil {
		return fmt.Errorf("connect inventory database: %w", err)
	}
	// 最初の単体学習用スキーマ。既存の決済記録は起動時に書き換えない。
	if _, err := pool.Exec(startupCtx, schema); err != nil {
		return fmt.Errorf("initialize inventory schema: %w", err)
	}
	server := &http.Server{
		Addr: ":8080", Handler: newHandler(postgresStore{pool}, logger),
		ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 5 * time.Second,
		WriteTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second,
	}
	serverErrors := make(chan error, 1)
	go func() {
		logger.Info("inventory listening", "service", "inventory", "address", server.Addr)
		serverErrors <- server.ListenAndServe()
	}()
	select {
	case err := <-serverErrors:
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
	if len(os.Args) == 2 && os.Args[1] == "-import" {
		if err := runImport(); err != nil {
			slog.Error("inventory import failed", "error", err)
			os.Exit(1)
		}
		return
	}

	// scratchイメージでもComposeからHTTPの準備完了を確認できる。
	if len(os.Args) == 2 && os.Args[1] == "-healthcheck" {
		client := http.Client{Timeout: 4 * time.Second}
		r, err := client.Get("http://127.0.0.1:8080/healthz")
		if err != nil {
			os.Exit(1)
		}
		_ = r.Body.Close()
		if r.StatusCode != http.StatusOK {
			os.Exit(1)
		}
		return
	}
	logger := slog.New(traceLogHandler{slog.NewJSONHandler(os.Stdout, nil)})
	slog.SetDefault(logger)
	if err := run(logger); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Error("inventory stopped", "service", "inventory", "error", err)
		os.Exit(1)
	}
}
