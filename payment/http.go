package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"regexp"
	"time"
)

var requestIDPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)

type requestInfo struct{ requestID, orderID, result string }
type requestInfoKey struct{}

type responseWriter struct {
	http.ResponseWriter
	status int
}

func (w *responseWriter) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func newHandler(store paymentStore, logger *slog.Logger) http.Handler {
	logger = logger.With("service", "payment")
	mux := http.NewServeMux()
	fail := func(w http.ResponseWriter, r *http.Request, status int, code string) {
		info := r.Context().Value(requestInfoKey{}).(*requestInfo)
		info.result = code
		body := map[string]string{"error": code, "requestId": info.requestID}
		if info.orderID != "" {
			body["orderId"] = info.orderID
		}
		writeJSON(w, status, body)
	}
	reply := func(w http.ResponseWriter, r *http.Request, p Payment, err error, status int) {
		info := r.Context().Value(requestInfoKey{}).(*requestInfo)
		switch {
		case errors.Is(err, errInvalid):
			fail(w, r, 400, "invalid_input")
		case errors.Is(err, errNotFound):
			fail(w, r, 404, "payment_not_found")
		case errors.Is(err, errConflict):
			fail(w, r, 409, "payment_conflict")
		case err != nil:
			logger.ErrorContext(r.Context(), "payment operation not confirmed", "request_id", info.requestID, "order_id", info.orderID, "error", err)
			fail(w, r, 503, "payment_unavailable_check_before_retry")
		default:
			info.result = p.Status
			if status == http.StatusCreated {
				w.Header().Set("Location", "/payments/"+p.OrderID)
			}
			writeJSON(w, status, map[string]any{"payment": p})
		}
	}
	mux.HandleFunc("POST /payments", func(w http.ResponseWriter, r *http.Request) {
		media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || media != "application/json" {
			fail(w, r, 415, "json_required")
			return
		}
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
		decoder.DisallowUnknownFields()
		var in PaymentInput
		if err := decoder.Decode(&in); err != nil {
			fail(w, r, 400, "invalid_input")
			return
		}
		if err := decoder.Decode(&struct{}{}); err != io.EOF {
			fail(w, r, 400, "invalid_input")
			return
		}
		if err := in.validate(); err != nil {
			fail(w, r, 400, "invalid_input")
			return
		}
		r.Context().Value(requestInfoKey{}).(*requestInfo).orderID = in.OrderID
		p, err := store.Create(r.Context(), in)
		reply(w, r, p, err, http.StatusCreated)
	})
	withID := func(next func(http.ResponseWriter, *http.Request, string)) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			id := r.PathValue("orderId")
			if !orderIDPattern.MatchString(id) {
				fail(w, r, 400, "invalid_input")
				return
			}
			r.Context().Value(requestInfoKey{}).(*requestInfo).orderID = id
			next(w, r, id)
		}
	}
	mux.HandleFunc("GET /payments/{orderId}", withID(func(w http.ResponseWriter, r *http.Request, id string) {
		p, err := store.Get(r.Context(), id)
		reply(w, r, p, err, http.StatusOK)
	}))
	mux.HandleFunc("POST /payments/{orderId}/cancel", withID(func(w http.ResponseWriter, r *http.Request, id string) {
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 4096))
		if err != nil || len(bytes.TrimSpace(body)) != 0 {
			fail(w, r, 400, "empty_body_required")
			return
		}
		p, err := store.Cancel(r.Context(), id)
		reply(w, r, p, err, http.StatusOK)
	}))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		if err := store.Ping(r.Context()); err != nil {
			fail(w, r, 503, "payment_database_unavailable")
			return
		}
		r.Context().Value(requestInfoKey{}).(*requestInfo).result = "healthy"
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	return tracedHTTP("payment", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-ID")
		if !requestIDPattern.MatchString(id) {
			var buf [16]byte
			_, _ = rand.Read(buf[:])
			id = hex.EncodeToString(buf[:])
		}
		info := &requestInfo{requestID: id, result: "http_error"}
		ctx, cancel := context.WithTimeout(context.WithValue(r.Context(), requestInfoKey{}, info), 3*time.Second)
		defer cancel()
		w.Header().Set("X-Request-ID", id)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		rw := &responseWriter{ResponseWriter: w, status: 200}
		started := time.Now()
		instrumented := r.WithContext(ctx)
		mux.ServeHTTP(rw, instrumented)
		r.Pattern = instrumented.Pattern
		trace.SpanFromContext(ctx).SetAttributes(attribute.String("order_id", info.orderID), attribute.String("request_id", id), attribute.String("payment.result", info.result))
		logger.InfoContext(ctx, "http request", "request_id", id, "order_id", info.orderID,
			"method", r.Method, "path", r.URL.Path, "status", rw.status,
			"result", info.result, "duration_ms", time.Since(started).Milliseconds())
	}))
}
