package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

func tracingFixture(t *testing.T) *tracetest.SpanRecorder {
	t.Helper()
	recorder := tracetest.NewSpanRecorder()
	previous := otel.GetTracerProvider()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	otel.SetTracerProvider(provider)
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()); otel.SetTracerProvider(previous) })
	return recorder
}

func TestTracePropagationAndLogCorrelation(t *testing.T) {
	recorder := tracingFixture(t)
	var logs bytes.Buffer
	logger := slog.New(traceLogHandler{slog.NewJSONHandler(&logs, nil)}).With("service", "payment")
	server := httptest.NewServer(tracedHTTP("payment", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Pattern = "POST /payments"
		logger.InfoContext(r.Context(), "payment received")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(201)
		fmt.Fprintf(w, `{"payment":{"orderId":%q,"amountYen":980,"status":"succeeded"}}`, paymentTestID)
	})))
	defer server.Close()
	client, _ := newHTTPPayments(server.URL)
	ctx, root := otel.Tracer("test").Start(context.Background(), "order request")
	_, err := client.Create(ctx, paymentTestID, 980, "success")
	root.End()
	if err != nil {
		t.Fatal(err)
	}
	spans := recorder.Ended()
	if len(spans) != 3 {
		t.Fatalf("spans=%d", len(spans))
	}
	var clientSpan, serverSpan sdktrace.ReadOnlySpan
	for _, s := range spans {
		if s.SpanContext().TraceID() != root.SpanContext().TraceID() {
			t.Fatal("trace split")
		}
		switch s.SpanKind() {
		case trace.SpanKindClient:
			clientSpan = s
		case trace.SpanKindServer:
			serverSpan = s
		}
	}
	if clientSpan == nil || serverSpan == nil || serverSpan.Parent().SpanID() != clientSpan.SpanContext().SpanID() || clientSpan.Parent().SpanID() != root.SpanContext().SpanID() {
		t.Fatal("parent-child relationship broken")
	}
	var log map[string]any
	if err = json.Unmarshal(logs.Bytes(), &log); err != nil {
		t.Fatal(err)
	}
	if log["trace_id"] != root.SpanContext().TraceID().String() || log["span_id"] != serverSpan.SpanContext().SpanID().String() {
		t.Fatalf("uncorrelated log %v", log)
	}
}

func TestServerTraceNewRequestAndIncomingContext(t *testing.T) {
	recorder := tracingFixture(t)
	h := tracedHTTP("test", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Pattern = "POST /orders/{id}/resume"
		w.WriteHeader(503)
	}))
	first := request(h, "POST", "/orders/a/resume", "")
	second := request(h, "POST", "/orders/a/resume", "")
	if first.Header().Get("X-Trace-ID") == "" || first.Header().Get("X-Trace-ID") == second.Header().Get("X-Trace-ID") {
		t.Fatal("manual resume must start another trace")
	}
	r := httptest.NewRequest("POST", "/orders/a/resume", nil)
	r.Header.Set("traceparent", "00-0123456789abcdef0123456789abcdef-0123456789abcdef-01")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	spans := recorder.Ended()
	last := spans[len(spans)-1]
	if last.SpanContext().TraceID().String() != paymentTestID || last.Parent().SpanID().String() != "0123456789abcdef" || !last.Parent().IsRemote() {
		t.Fatal("incoming context lost")
	}
	for _, s := range spans {
		if s.Status().Code != codes.Error || s.Name() != "POST /orders/{id}/resume" {
			t.Fatalf("name/status=%s %v", s.Name(), s.Status())
		}
	}
}
