package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"time"
)

var errPaymentNotFound = errors.New("payment not found")

var errPaymentUncertain = errors.New("payment outcome not confirmed")

// 業務失敗はCreateのfailed、通信・契約違反はerrorとして区別する。
type paymentGateway interface {
	Get(context.Context, string, int64) (string, error)
	Create(context.Context, string, int64, string) (string, error)
	Cancel(context.Context, string, int64) error
}

type httpPayments struct {
	baseURL string
	client  *http.Client
}

func newHTTPPayments(base string) (*httpPayments, error) {
	u, err := url.Parse(base)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return nil, errors.New("PAYMENT_BASE_URL must be an http(s) origin without credentials, path, query or fragment")
	}
	return &httpPayments{baseURL: strings.TrimRight(base, "/"), client: &http.Client{
		Timeout: time.Second,
		// リダイレクト先への再送を行わない。自動リトライも実装しない。
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}, nil
}

func (p *httpPayments) Create(ctx context.Context, id string, amount int64, mode string) (string, error) {
	body, err := json.Marshal(struct {
		OrderID   string `json:"orderId"`
		AmountYen int64  `json:"amountYen"`
		Mode      string `json:"mode"`
	}{id, amount, mode})
	if err != nil {
		return "", err
	}
	return p.call(ctx, "/payments", id, amount, body, http.StatusCreated, "create")
}

func (p *httpPayments) Get(ctx context.Context, id string, amount int64) (string, error) {
	return p.call(ctx, "/payments/"+id, id, amount, nil, http.StatusOK, "get")
}

func (p *httpPayments) Cancel(ctx context.Context, id string, amount int64) error {
	_, err := p.call(ctx, "/payments/"+id+"/cancel", id, amount, nil, http.StatusOK, "cancel")
	return err
}

func (p *httpPayments) call(ctx context.Context, path, id string, amount int64, body []byte, expected int, operation string) (status string, resultErr error) {
	ctx, span := otel.Tracer("order").Start(ctx, "payment."+operation, trace.WithSpanKind(trace.SpanKindClient), trace.WithAttributes(attribute.String("order_id", id), attribute.String("payment.operation", operation)))
	defer func() {
		span.SetAttributes(attribute.String("payment.result", status))
		if errors.Is(resultErr, errPaymentNotFound) {
			span.SetAttributes(attribute.Bool("payment.not_found", true))
			span.End()
			return
		}
		finishOperation(span, resultErr)
	}()
	started := time.Now()
	requestID, _ := ctx.Value(requestIDKey{}).(string)
	defer func() {
		result := status
		if resultErr != nil {
			result = "unknown"
		}
		slog.InfoContext(ctx, "payment_call_finished", "service", "order", "request_id", requestID,
			"order_id", id, "operation", operation, "result", result, "duration_ms", time.Since(started).Milliseconds())
	}()
	method := http.MethodPost
	if operation == "get" {
		method = http.MethodGet
	}
	req, err := http.NewRequestWithContext(ctx, method, p.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("%w: %v", errPaymentUncertain, err)
	}
	req.Header.Set("Content-Type", "application/json")
	if requestID != "" {
		req.Header.Set("X-Request-ID", requestID)
	}
	propagation.TraceContext{}.Inject(ctx, propagation.HeaderCarrier(req.Header))
	resp, err := p.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("%w: %v", errPaymentUncertain, err)
	}
	defer resp.Body.Close()
	span.SetAttributes(attribute.Int("http.response.status_code", resp.StatusCode))
	if operation == "get" && resp.StatusCode == http.StatusNotFound {
		return "", errPaymentNotFound
	}
	if resp.StatusCode != expected {
		return "", fmt.Errorf("%w: %s HTTP %d", errPaymentUncertain, operation, resp.StatusCode)
	}
	media, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil || media != "application/json" {
		return "", fmt.Errorf("%w: response is not JSON", errPaymentUncertain)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4097))
	if err != nil || len(raw) > 4096 {
		return "", fmt.Errorf("%w: incomplete or oversized response", errPaymentUncertain)
	}
	var response struct {
		Payment struct {
			OrderID   string `json:"orderId"`
			AmountYen *int64 `json:"amountYen"`
			Status    string `json:"status"`
		} `json:"payment"`
	}
	if err := json.Unmarshal(raw, &response); err != nil {
		return "", fmt.Errorf("%w: malformed response", errPaymentUncertain)
	}
	payment := response.Payment
	if payment.OrderID != id || payment.AmountYen == nil || *payment.AmountYen != amount {
		return "", fmt.Errorf("%w: response identity or amount mismatch", errPaymentUncertain)
	}
	if ((operation == "create" || operation == "get") && payment.Status != "succeeded" && payment.Status != "failed" && payment.Status != "cancelled") || (operation == "cancel" && payment.Status != "cancelled") {
		return "", fmt.Errorf("%w: unexpected payment status", errPaymentUncertain)
	}
	return payment.Status, nil
}
