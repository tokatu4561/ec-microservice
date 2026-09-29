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
	"strconv"
	"strings"
	"time"
)

var errInventoryUncertain = errors.New("inventory outcome not confirmed")
var errInventoryNotFound = errors.New("reservation not found")

type inventoryResult struct {
	OrderID   string         `json:"orderId"`
	Items     []shipmentItem `json:"items"`
	Status    string         `json:"status"`
	Shortages []int64        `json:"shortages"`
}
type inventoryGateway interface {
	Stocks(context.Context, []int64) (map[int64]int, error)
	Reserve(context.Context, string, []shipmentItem) (inventoryResult, error)
	Get(context.Context, string, []shipmentItem) (inventoryResult, error)
	Commit(context.Context, string, []shipmentItem) (inventoryResult, error)
	Release(context.Context, string, []shipmentItem) (inventoryResult, error)
}
type httpInventory struct {
	baseURL string
	client  *http.Client
}

func newHTTPInventory(base string) (*httpInventory, error) {
	u, e := url.Parse(base)
	if e != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return nil, errors.New("INVENTORY_BASE_URL must be an http(s) origin")
	}
	return &httpInventory{strings.TrimRight(base, "/"), &http.Client{Timeout: time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}
func (p *httpInventory) request(ctx context.Context, method, path, operation, id string, body []byte, expected int, validate func([]byte) error) (raw []byte, resultErr error) {
	ctx, span := otel.Tracer("order").Start(ctx, "inventory."+operation, trace.WithSpanKind(trace.SpanKindClient), trace.WithAttributes(attribute.String("order_id", id)))
	start := time.Now()
	defer func() {
		if errors.Is(resultErr, errInventoryNotFound) {
			span.End()
		} else {
			finishOperation(span, resultErr)
		}
		rid, _ := ctx.Value(requestIDKey{}).(string)
		result := "confirmed"
		if resultErr != nil {
			result = "unknown"
		}
		slog.InfoContext(ctx, "inventory_call_finished", "service", "order", "order_id", id, "request_id", rid, "operation", operation, "result", result, "duration_ms", time.Since(start).Milliseconds())
	}()
	req, e := http.NewRequestWithContext(ctx, method, p.baseURL+path, bytes.NewReader(body))
	if e != nil {
		return nil, fmt.Errorf("%w: %v", errInventoryUncertain, e)
	}
	req.Header.Set("Content-Type", "application/json")
	if rid, ok := ctx.Value(requestIDKey{}).(string); ok {
		req.Header.Set("X-Request-ID", rid)
	}
	propagation.TraceContext{}.Inject(ctx, propagation.HeaderCarrier(req.Header))
	resp, e := p.client.Do(req)
	if e != nil {
		return nil, fmt.Errorf("%w: %v", errInventoryUncertain, e)
	}
	defer resp.Body.Close()
	span.SetAttributes(attribute.Int("http.response.status_code", resp.StatusCode))
	if operation == "get" && resp.StatusCode == 404 {
		return nil, errInventoryNotFound
	}
	if resp.StatusCode != expected {
		return nil, fmt.Errorf("%w: HTTP %d", errInventoryUncertain, resp.StatusCode)
	}
	media, _, e := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if e != nil || media != "application/json" {
		return nil, errInventoryUncertain
	}
	raw, e = io.ReadAll(io.LimitReader(resp.Body, 32769))
	if e != nil || len(raw) > 32768 {
		return nil, errInventoryUncertain
	}
	return raw, validate(raw)
}
func (p *httpInventory) Stocks(ctx context.Context, ids []int64) (map[int64]int, error) {
	result := map[int64]int{}
	// 商品一覧もカートも一商品ごとのHTTPを避け、100商品単位で照会する。
	for start := 0; start < len(ids); start += 100 {
		batch := ids[start:min(start+100, len(ids))]
		parts := make([]string, len(batch))
		for i, id := range batch {
			parts[i] = strconv.FormatInt(id, 10)
		}
		_, e := p.request(ctx, "GET", "/stocks?ids="+strings.Join(parts, ","), "stocks", "", nil, 200, func(raw []byte) error {
			var response struct {
				Stocks []struct {
					ProductID int64 `json:"productId"`
					Available *int  `json:"available"`
				} `json:"stocks"`
			}
			if e := json.Unmarshal(raw, &response); e != nil {
				return errInventoryUncertain
			}
			for _, s := range response.Stocks {
				if !slices.Contains(batch, s.ProductID) || s.Available == nil || *s.Available < 0 || int64(*s.Available) > maxSafeYen {
					return errInventoryUncertain
				}
				if _, exists := result[s.ProductID]; exists {
					return errInventoryUncertain
				}
				result[s.ProductID] = *s.Available
			}
			for _, id := range batch {
				if _, ok := result[id]; !ok {
					return errInventoryUncertain
				}
			}
			return nil
		})
		if e != nil {
			return nil, e
		}
	}
	return result, nil
}
func (p *httpInventory) Reserve(ctx context.Context, id string, items []shipmentItem) (inventoryResult, error) {
	return p.call(ctx, id, items, "reserve")
}
func (p *httpInventory) Get(ctx context.Context, id string, items []shipmentItem) (inventoryResult, error) {
	return p.call(ctx, id, items, "get")
}
func (p *httpInventory) Commit(ctx context.Context, id string, items []shipmentItem) (inventoryResult, error) {
	return p.call(ctx, id, items, "commit")
}
func (p *httpInventory) Release(ctx context.Context, id string, items []shipmentItem) (inventoryResult, error) {
	return p.call(ctx, id, items, "release")
}
func (p *httpInventory) call(ctx context.Context, id string, items []shipmentItem, op string) (inventoryResult, error) {
	path, method, expected := "/reservations/"+id, "POST", 200
	var body []byte
	switch op {
	case "reserve":
		path = "/reservations"
		expected = 201
		body, _ = json.Marshal(struct {
			OrderID string         `json:"orderId"`
			Items   []shipmentItem `json:"items"`
		}{id, items})
	case "get":
		method = "GET"
	default:
		path += "/" + op
	}
	var response struct {
		Reservation inventoryResult `json:"reservation"`
	}
	_, err := p.request(ctx, method, path, op, id, body, expected, func(raw []byte) error {
		if e := json.Unmarshal(raw, &response); e != nil {
			return errInventoryUncertain
		}
		r := response.Reservation
		if r.OrderID != id || !slices.Equal(r.Items, items) || !slices.Contains([]string{"reserved", "rejected", "committed", "released"}, r.Status) {
			return errInventoryUncertain
		}
		seen := map[int64]bool{}
		for _, short := range r.Shortages {
			found := false
			for _, item := range items {
				found = found || item.ProductID == short
			}
			if !found || seen[short] {
				return errInventoryUncertain
			}
			seen[short] = true
		}
		if (r.Status == "rejected") != (len(r.Shortages) > 0) || (op == "commit" && r.Status != "committed") || (op == "release" && r.Status != "released") {
			return errInventoryUncertain
		}
		return nil
	})
	if err != nil {
		return inventoryResult{}, err
	}
	return response.Reservation, nil
}
