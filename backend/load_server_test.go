package main

// 学習専用HTTPサーバー。go test -cでのみ含まれ、通常のAPIには入口を作らない。
import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type loadTiming struct {
	AcquireMS float64 `json:"acquire_ms"`
	SQLMS     float64 `json:"sql_ms"`
	LockSQLMS float64 `json:"lock_sql_ms"`
	Queries   int     `json:"queries"`
	TotalMS   float64 `json:"total_ms"`
}
type loadTimingKey struct{}
type loadQueryKey struct{}
type loadAcquireKey struct{}
type loadQueryStart struct {
	at   time.Time
	lock bool
}
type loadTracer struct{}

func (loadTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	return context.WithValue(ctx, loadQueryKey{}, loadQueryStart{time.Now(), strings.Contains(d.SQL, "FOR UPDATE")})
}
func (loadTracer) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryEndData) {
	m, ok := ctx.Value(loadTimingKey{}).(*loadTiming)
	if !ok {
		return
	}
	q := ctx.Value(loadQueryKey{}).(loadQueryStart)
	ms := float64(time.Since(q.at)) / float64(time.Millisecond)
	m.Queries++
	m.SQLMS += ms
	if q.lock {
		m.LockSQLMS += ms
	}
}
func (loadTracer) TraceAcquireStart(ctx context.Context, _ *pgxpool.Pool, _ pgxpool.TraceAcquireStartData) context.Context {
	return context.WithValue(ctx, loadAcquireKey{}, time.Now())
}
func (loadTracer) TraceAcquireEnd(ctx context.Context, _ *pgxpool.Pool, _ pgxpool.TraceAcquireEndData) {
	if m, ok := ctx.Value(loadTimingKey{}).(*loadTiming); ok {
		m.AcquireMS += float64(time.Since(ctx.Value(loadAcquireKey{}).(time.Time))) / float64(time.Millisecond)
	}
}

func TestLoadServer(t *testing.T) {
	if os.Getenv("LEARNING_LOAD") != "1" {
		t.Skip("load experiment only")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	cfg, err := pgxpool.ParseConfig(os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ConnConfig.Database != "ec_load" {
		t.Fatal("load server requires isolated ec_load database")
	}
	cfg.ConnConfig.Tracer = loadTracer{}
	cfg.ConnConfig.RuntimeParams["application_name"] = "learning-load-api"
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	variant := os.Getenv("ORDER_VARIANT")
	if variant != "baseline" && variant != "current" {
		t.Fatal("ORDER_VARIANT must be baseline or current")
	}
	// SQL待機の比較を大量の段階ログ出力で歪めない。同じ条件を両実装に適用する。
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	var inFlight atomic.Int64
	var completed atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, r *http.Request) {
		var mem runtime.MemStats
		runtime.ReadMemStats(&mem)
		p := pool.Stat()
		writeJSON(w, 200, map[string]any{
			"variant": variant, "in_flight": inFlight.Load(), "completed": completed.Load(),
			"goroutines": runtime.NumGoroutine(), "heap_bytes": mem.HeapAlloc,
			"pool_max": p.MaxConns(), "pool_acquired": p.AcquiredConns(), "pool_idle": p.IdleConns(),
			"pool_total": p.TotalConns(), "pool_constructing": p.ConstructingConns(),
			"pool_empty_acquires": p.EmptyAcquireCount(), "pool_canceled_acquires": p.CanceledAcquireCount(),
		})
	})
	mux.HandleFunc("POST /orders", func(w http.ResponseWriter, r *http.Request) {
		inFlight.Add(1)
		defer inFlight.Add(-1)
		defer completed.Add(1)
		m := &loadTiming{}
		ctx, cancel := context.WithTimeout(context.WithValue(r.Context(), loadTimingKey{}, m), 3*time.Second)
		defer cancel()
		var in struct {
			Items []ItemInput `json:"items"`
		}
		d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16384))
		d.DisallowUnknownFields()
		if err := d.Decode(&in); err != nil {
			writeJSON(w, 400, map[string]any{"error": "invalid body"})
			return
		}
		if d.Decode(&struct{}{}) != io.EOF || len(in.Items) == 0 || len(in.Items) > 100 {
			writeJSON(w, 400, map[string]any{"error": "invalid items"})
			return
		}
		began := time.Now()
		id := fmt.Sprintf("%x", randomID())
		o, err := func() (Order, error) {
			tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
			if err != nil {
				return Order{}, err
			}
			defer rollback(tx)
			create := createOrderTx
			if variant == "baseline" {
				create = createOrderTxBaseline
			}
			o, err := create(ctx, tx, id, in.Items, "success", "success")
			if err != nil {
				return Order{}, err
			}
			return o, tx.Commit(ctx)
		}()
		m.TotalMS = float64(time.Since(began)) / float64(time.Millisecond)
		if err != nil {
			fmt.Fprintf(os.Stderr, "load order failed id=%s error=%v\n", id, err)
			writeJSON(w, 503, map[string]any{"error": "order failed", "orderId": id, "timing": m})
			return
		}
		writeJSON(w, 201, map[string]any{"order": o, "timing": m})
	})
	srv := &http.Server{Addr: ":8080", Handler: mux, ReadHeaderTimeout: 5 * time.Second, WriteTimeout: 10 * time.Second, IdleTimeout: 30 * time.Second}
	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()
	select {
	case err := <-errCh:
		if err != http.ErrServerClosed {
			t.Fatal(err)
		}
	case <-ctx.Done():
		end, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(end); err != nil {
			_ = srv.Close()
			t.Error(err)
		}
	}
}
