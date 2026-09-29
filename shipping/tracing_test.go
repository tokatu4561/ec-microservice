package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http/httptest"
	"testing"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func TestShipmentIncomingTraceAndLog(t *testing.T) {
	rec := tracetest.NewSpanRecorder()
	old := otel.GetTracerProvider()
	p := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	otel.SetTracerProvider(p)
	defer func() { _ = p.Shutdown(context.Background()); otel.SetTracerProvider(old) }()
	var logs bytes.Buffer
	// GETの不存在も、正規化した経路名と親のtraceで記録する。
	h := newHandler(&stubStore{err: errNotFound}, slog.New(traceLogHandler{slog.NewJSONHandler(&logs, nil)}))
	r := httptest.NewRequest("GET", "/shipments/0123456789abcdef0123456789abcdef", nil)
	r.Header.Set("traceparent", "00-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-bbbbbbbbbbbbbbbb-01")
	r.Header.Set("X-Request-ID", "trace-test")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	spans := rec.Ended()
	if w.Code != 404 || len(spans) != 1 {
		t.Fatalf("status=%d spans=%d", w.Code, len(spans))
	}
	s := spans[0]
	if s.Name() != "GET /shipments/{orderId}" || s.Parent().SpanID().String() != "bbbbbbbbbbbbbbbb" || s.SpanContext().TraceID().String() != "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" {
		t.Fatal("invalid server trace")
	}
	var log map[string]any
	if err := json.Unmarshal(logs.Bytes(), &log); err != nil {
		t.Fatal(err)
	}
	if log["trace_id"] != s.SpanContext().TraceID().String() || log["span_id"] != s.SpanContext().SpanID().String() || log["request_id"] != "trace-test" {
		t.Fatal(log)
	}
}
