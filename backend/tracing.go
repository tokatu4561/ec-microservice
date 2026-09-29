package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// 独立したGoモジュールごとに起動・終了を管理する。未設定時は従来どおり動作。
func initTracing(ctx context.Context, service string) (func(), error) {
	if os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") == "" {
		return func() {}, nil
	}
	exporter, err := otlptracehttp.New(ctx, otlptracehttp.WithTimeout(time.Second))
	if err != nil {
		return nil, err
	}
	provider := sdktrace.NewTracerProvider(
		sdktrace.WithResource(resource.NewSchemaless(attribute.String("service.name", service))),
		// ローカル教材は全件記録。収集側が停止しても業務要求を待たせない。
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
		sdktrace.WithBatcher(exporter, sdktrace.WithBatchTimeout(time.Second), sdktrace.WithMaxQueueSize(2048)),
	)
	otel.SetTracerProvider(provider)
	return func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := provider.Shutdown(shutdownCtx); err != nil {
			slog.Warn("trace exporter shutdown failed", "service", service, "error", err)
		}
	}, nil
}

// stdout JSONを維持し、現在のspanと関連付ける。OTel Logsの転送ではない。
type traceLogHandler struct{ slog.Handler }

func (h traceLogHandler) Handle(ctx context.Context, r slog.Record) error {
	sc := trace.SpanContextFromContext(ctx)
	if sc.IsValid() {
		r.AddAttrs(slog.String("trace_id", sc.TraceID().String()), slog.String("span_id", sc.SpanID().String()))
	}
	return h.Handler.Handle(ctx, r)
}
func (h traceLogHandler) WithAttrs(a []slog.Attr) slog.Handler {
	return traceLogHandler{h.Handler.WithAttrs(a)}
}
func (h traceLogHandler) WithGroup(n string) slog.Handler {
	return traceLogHandler{h.Handler.WithGroup(n)}
}

func tracedHTTP(service string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 健康確認を注文のトレース一覧から除外。
		if r.URL.Path == "/healthz" {
			next.ServeHTTP(w, r)
			return
		}
		parent := propagation.TraceContext{}.Extract(r.Context(), propagation.HeaderCarrier(r.Header))
		ctx, span := otel.Tracer(service).Start(parent, r.Method, trace.WithSpanKind(trace.SpanKindServer))
		defer span.End()
		if sc := span.SpanContext(); sc.IsValid() {
			w.Header().Set("X-Trace-ID", sc.TraceID().String())
		}
		req := r.WithContext(ctx)
		rw := &responseWriter{ResponseWriter: w, status: 200}
		next.ServeHTTP(rw, req)
		route := req.Pattern
		if route == "" {
			route = r.Method + " unmatched"
		}
		span.SetName(route)
		span.SetAttributes(attribute.String("http.request.method", r.Method), attribute.String("http.route", route), attribute.Int("http.response.status_code", rw.status))
		if rw.status >= 500 {
			span.SetStatus(codes.Error, "HTTP server error")
		}
	})
}

func startOperation(ctx context.Context, service, name, id string) (context.Context, trace.Span) {
	return otel.Tracer(service).Start(ctx, name, trace.WithAttributes(attribute.String("order_id", id)))
}
func finishOperation(span trace.Span, err error) {
	if err != nil {
		// SQL・接続文字列などをトレースへ流さない。
		span.RecordError(errors.New("operation failed"))
		span.SetStatus(codes.Error, "operation failed")
	}
	span.End()
}
