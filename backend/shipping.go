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
	"slices"
	"strings"
	"time"
)

type shipmentItem struct {
	ProductID int64 `json:"productId"`
	Quantity  int   `json:"quantity"`
}

var errShippingNotFound = errors.New("shipping not found")

var errShippingUncertain = errors.New("shipping outcome not confirmed")

// 業務失敗はCreateのfailed、通信・契約違反はerrorとして区別する。
type shippingGateway interface {
	Get(context.Context, string, []shipmentItem, string) (string, error)
	Create(context.Context, string, []shipmentItem, string) (string, error)
	Cancel(context.Context, string, []shipmentItem, string) error
}

type httpShipping struct {
	baseURL string
	client  *http.Client
}

func newHTTPShipping(base string) (*httpShipping, error) {
	u, err := url.Parse(base)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return nil, errors.New("SHIPPING_BASE_URL must be an http(s) origin without credentials, path, query or fragment")
	}
	return &httpShipping{baseURL: strings.TrimRight(base, "/"), client: &http.Client{
		Timeout: time.Second,
		// リダイレクト先への再送を行わない。自動リトライも実装しない。
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}, nil
}

func (p *httpShipping) Create(ctx context.Context, id string, items []shipmentItem, mode string) (string, error) {
	body, err := json.Marshal(struct {
		OrderID string         `json:"orderId"`
		Items   []shipmentItem `json:"items"`
		Mode    string         `json:"mode"`
	}{id, items, mode})
	if err != nil {
		return "", err
	}
	return p.call(ctx, "/shipments", id, items, mode, body, http.StatusCreated, "create")
}

func (p *httpShipping) Get(ctx context.Context, id string, items []shipmentItem, mode string) (string, error) {
	return p.call(ctx, "/shipments/"+id, id, items, mode, nil, http.StatusOK, "get")
}

func (p *httpShipping) Cancel(ctx context.Context, id string, items []shipmentItem, mode string) error {
	_, err := p.call(ctx, "/shipments/"+id+"/cancel", id, items, mode, nil, http.StatusOK, "cancel")
	return err
}

func (p *httpShipping) call(ctx context.Context, path, id string, items []shipmentItem, mode string, body []byte, expected int, operation string) (status string, resultErr error) {
	ctx, span := otel.Tracer("order").Start(ctx, "shipping."+operation, trace.WithSpanKind(trace.SpanKindClient), trace.WithAttributes(attribute.String("order_id", id), attribute.String("shipping.operation", operation)))
	defer func() {
		span.SetAttributes(attribute.String("shipping.result", status))
		if errors.Is(resultErr, errShippingNotFound) {
			span.SetAttributes(attribute.Bool("shipping.not_found", true))
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
		slog.InfoContext(ctx, "shipping_call_finished", "service", "order", "request_id", requestID,
			"order_id", id, "operation", operation, "result", result, "duration_ms", time.Since(started).Milliseconds())
	}()
	method := http.MethodPost
	if operation == "get" {
		method = http.MethodGet
	}
	req, err := http.NewRequestWithContext(ctx, method, p.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("%w: %v", errShippingUncertain, err)
	}
	req.Header.Set("Content-Type", "application/json")
	if requestID != "" {
		req.Header.Set("X-Request-ID", requestID)
	}
	propagation.TraceContext{}.Inject(ctx, propagation.HeaderCarrier(req.Header))
	resp, err := p.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("%w: %v", errShippingUncertain, err)
	}
	defer resp.Body.Close()
	span.SetAttributes(attribute.Int("http.response.status_code", resp.StatusCode))
	if operation == "get" && resp.StatusCode == http.StatusNotFound {
		return "", errShippingNotFound
	}
	if resp.StatusCode != expected {
		return "", fmt.Errorf("%w: %s HTTP %d", errShippingUncertain, operation, resp.StatusCode)
	}
	media, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil || media != "application/json" {
		return "", fmt.Errorf("%w: response is not JSON", errShippingUncertain)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 16385))
	if err != nil || len(raw) > 16384 {
		return "", fmt.Errorf("%w: incomplete or oversized response", errShippingUncertain)
	}
	var response struct {
		Shipment struct {
			OrderID string         `json:"orderId"`
			Items   []shipmentItem `json:"items"`
			Mode    string         `json:"mode"`
			Status  string         `json:"status"`
		} `json:"shipment"`
	}
	if err := json.Unmarshal(raw, &response); err != nil {
		return "", fmt.Errorf("%w: malformed response", errShippingUncertain)
	}
	shipping := response.Shipment
	if shipping.OrderID != id || shipping.Mode != mode || !slices.Equal(shipping.Items, items) {
		return "", fmt.Errorf("%w: response identity or content mismatch", errShippingUncertain)
	}
	if ((operation == "create" || operation == "get") && shipping.Status != "requested" && shipping.Status != "failed" && shipping.Status != "cancelled") || (operation == "cancel" && shipping.Status != "cancelled") {
		return "", fmt.Errorf("%w: unexpected shipping status", errShippingUncertain)
	}
	return shipping.Status, nil
}
