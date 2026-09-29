package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"regexp"
	"time"
)

type requestIDKey struct{}

var orderIDPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

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

func newHandler(store productStore, logger *slog.Logger) http.Handler {
	logger = logger.With("service", "order")
	mux := http.NewServeMux()
	wrap := func(handler func(http.ResponseWriter, *http.Request, *string)) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			requestID := fmt.Sprintf("%x", randomID())
			orderID := ""
			started := time.Now()
			rw := &responseWriter{w, http.StatusOK}
			w.Header().Set("X-Request-ID", requestID)
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.Header().Set("Cache-Control", "no-store")
			ctx, cancel := context.WithTimeout(context.WithValue(r.Context(), requestIDKey{}, requestID), 3*time.Second)
			defer cancel()
			handler(rw, r.WithContext(ctx), &orderID)
			trace.SpanFromContext(ctx).SetAttributes(attribute.String("order_id", orderID), attribute.String("request_id", requestID))
			logger.InfoContext(ctx, "http request", "request_id", requestID, "order_id", orderID,
				"method", r.Method, "path", r.URL.Path, "status", rw.status, "duration_ms", time.Since(started).Milliseconds())
		}
	}
	fail := func(w http.ResponseWriter, r *http.Request, status int, message, orderID string) {
		body := map[string]string{"error": message, "requestId": w.Header().Get("X-Request-ID")}
		if orderID != "" {
			body["orderId"] = orderID
		}
		writeJSON(w, status, body)
	}
	mux.HandleFunc("GET /api/products", wrap(func(w http.ResponseWriter, r *http.Request, _ *string) {
		products, err := store.ListProducts(r.Context())
		if err != nil {
			logger.ErrorContext(r.Context(), "products query failed", "request_id", w.Header().Get("X-Request-ID"), "error", err)
			fail(w, r, http.StatusServiceUnavailable, "商品を取得できませんでした。", "")
			return
		}
		if products == nil {
			products = []Product{}
		}
		writeJSON(w, http.StatusOK, map[string]any{"products": products})
	}))
	mux.HandleFunc("POST /api/orders", wrap(func(w http.ResponseWriter, r *http.Request, orderID *string) {
		media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || media != "application/json" {
			fail(w, r, http.StatusUnsupportedMediaType, "Content-Typeはapplication/jsonを指定してください。", "")
			return
		}
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
		decoder.DisallowUnknownFields()
		var in OrderInput
		if err := decoder.Decode(&in); err != nil {
			fail(w, r, http.StatusBadRequest, "注文のJSONを確認してください。", "")
			return
		}
		if err := decoder.Decode(&struct{}{}); err != io.EOF {
			fail(w, r, http.StatusBadRequest, "JSONは1件だけ指定してください。", "")
			return
		}
		if err := in.validate(); err != nil {
			fail(w, r, http.StatusBadRequest, err.Error(), "")
			return
		}
		*orderID = fmt.Sprintf("%x", randomID())
		order, err := store.CreateOrder(r.Context(), *orderID, in)
		if errors.Is(err, errNotFound) {
			fail(w, r, http.StatusNotFound, "商品が見つかりません。", "")
			return
		}
		if errors.Is(err, errInvalid) {
			fail(w, r, http.StatusBadRequest, "数量または金額が範囲外です。", "")
			return
		}
		if err != nil {
			logger.ErrorContext(r.Context(), "create order failed", "request_id", w.Header().Get("X-Request-ID"), "order_id", *orderID, "error", err)
			fail(w, r, http.StatusServiceUnavailable, "注文・決済・配送の結果を確認できません。再注文せず、注文IDで状況を確認して処理を再開してください。", *orderID)
			return
		}
		w.Header().Set("Location", "/api/orders/"+order.ID)
		writeJSON(w, http.StatusCreated, map[string]any{"order": order})
	}))
	mux.HandleFunc("GET /api/orders/{id}", wrap(func(w http.ResponseWriter, r *http.Request, orderID *string) {
		*orderID = r.PathValue("id")
		if !orderIDPattern.MatchString(*orderID) {
			fail(w, r, http.StatusBadRequest, "注文IDは32文字の小文字の16進数です。", "")
			return
		}
		order, err := store.GetOrder(r.Context(), *orderID)
		if errors.Is(err, errNotFound) {
			fail(w, r, http.StatusNotFound, "注文が見つかりません。", *orderID)
			return
		}
		if err != nil {
			logger.ErrorContext(r.Context(), "get order failed", "request_id", w.Header().Get("X-Request-ID"), "order_id", *orderID, "error", err)
			fail(w, r, http.StatusServiceUnavailable, "注文を取得できませんでした。", *orderID)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"order": order})
	}))
	if rs, ok := store.(interface {
		ResumeOrder(context.Context, string) (Order, error)
	}); ok {
		mux.HandleFunc("POST /api/orders/{id}/resume", wrap(func(w http.ResponseWriter, r *http.Request, orderID *string) {
			*orderID = r.PathValue("id")
			var body struct{}
			if !orderIDPattern.MatchString(*orderID) || decodeCart(w, r, &body) != nil {
				fail(w, r, 400, "注文IDとリクエスト形式を確認してください。", "")
				return
			}
			o, err := rs.ResumeOrder(r.Context(), *orderID)
			if errors.Is(err, errNotFound) {
				fail(w, r, 404, "注文が見つかりません。", *orderID)
				return
			}
			if err != nil {
				fail(w, r, 503, "結果を確認できません。在庫予約を保持しています。接続復旧後に同じ注文の処理を再開してください。", *orderID)
				return
			}
			writeJSON(w, 200, map[string]any{"order": o})
		}))
	}
	if cs, ok := store.(cartStore); ok {
		registerCartHandlers(mux, cs, logger, wrap)
	}
	return tracedHTTP("order", mux)
}
