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
	"go.opentelemetry.io/otel/trace"
)

func TestReservationIncomingTraceAndLog(t *testing.T) {
	rec := tracetest.NewSpanRecorder()
	old := otel.GetTracerProvider()
	p := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	otel.SetTracerProvider(p)
	defer func() { _ = p.Shutdown(context.Background()); otel.SetTracerProvider(old) }()
	var logs bytes.Buffer
	// GETの不存在も、正規化した経路名と親のtraceで記録する。
	store, _, _, _ := fixture(t)
	h := newHandler(store, slog.New(traceLogHandler{slog.NewJSONHandler(&logs, nil)}))
	r := httptest.NewRequest("GET", "/reservations/0123456789abcdef0123456789abcdef", nil)
	r.Header.Set("traceparent", "00-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-bbbbbbbbbbbbbbbb-01")
	r.Header.Set("X-Request-ID", "trace-test")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	spans := rec.Ended()
	if w.Code != 404 || len(spans) != 2 {
		t.Fatalf("status=%d spans=%d", w.Code, len(spans))
	}
	var s, child sdktrace.ReadOnlySpan
	for _, candidate := range spans {
		if candidate.SpanKind() == trace.SpanKindServer {
			s = candidate
		} else {
			child = candidate
		}
	}
	if s == nil || child == nil || child.Parent().SpanID() != s.SpanContext().SpanID() || child.SpanContext().TraceID() != s.SpanContext().TraceID() {
		t.Fatal("store span must be child of server span")
	}
	if s.Name() != "GET /reservations/{orderId}" || s.Parent().SpanID().String() != "bbbbbbbbbbbbbbbb" || s.SpanContext().TraceID().String() != "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" {
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
