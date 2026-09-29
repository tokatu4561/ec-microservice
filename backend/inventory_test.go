package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.opentelemetry.io/otel/codes"
)

func TestInventoryResponseContract(t *testing.T) {
	id := "0123456789abcdef0123456789abcdef"
	valid := fmt.Sprintf(`{"reservation":{"orderId":%q,"items":[{"productId":1,"quantity":1}],"status":"reserved","shortages":[]}}`, id)
	for _, tc := range []struct {
		name, body, op string
		status         int
		contentType    string
		uncertain      bool
	}{
		{"valid", valid, "get", 200, "application/json", false},
		{"malformed", `{`, "get", 200, "application/json", true},
		{"different order", strings.ReplaceAll(valid, id, strings.Repeat("a", 32)), "get", 200, "application/json", true},
		{"different quantity", strings.ReplaceAll(valid, `"quantity":1`, `"quantity":2`), "get", 200, "application/json", true},
		{"invalid state", strings.ReplaceAll(valid, "reserved", "missing"), "get", 200, "application/json", true},
		{"rejected without shortages", strings.ReplaceAll(valid, "reserved", "rejected"), "get", 200, "application/json", true},
		{"commit wrong state", valid, "commit", 200, "application/json", true},
		{"release wrong state", valid, "release", 200, "application/json", true},
		{"wrong type", valid, "get", 200, "text/plain", true},
		{"oversized", strings.Repeat(" ", 32769), "get", 200, "application/json", true},
		{"redirect", valid, "get", 302, "application/json", true},
		{"missing stock", `{"stocks":[]}`, "stocks", 200, "application/json", true},
		{"null stock", `{"stocks":[{"productId":1,"available":null}]}`, "stocks", 200, "application/json", true},
		{"negative stock", `{"stocks":[{"productId":1,"available":-1}]}`, "stocks", 200, "application/json", true},
		{"duplicate stock", `{"stocks":[{"productId":1,"available":1},{"productId":1,"available":1}]}`, "stocks", 200, "application/json", true},
		{"zero stock", `{"stocks":[{"productId":1,"available":0}]}`, "stocks", 200, "application/json", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recorder := tracingFixture(t)
			var logs bytes.Buffer
			old := slog.Default()
			slog.SetDefault(slog.New(traceLogHandler{slog.NewJSONHandler(&logs, nil)}))
			defer slog.SetDefault(old)
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Header.Get("traceparent") == "" || r.Header.Get("X-Request-ID") != "inventory-contract" {
					t.Error("missing correlation headers")
				}
				w.Header().Set("Content-Type", tc.contentType)
				w.Header().Set("Location", "/another")
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			client, err := newHTTPInventory(server.URL)
			if err != nil {
				t.Fatal(err)
			}
			ctx := context.WithValue(context.Background(), requestIDKey{}, "inventory-contract")
			if tc.op == "stocks" {
				_, err = client.Stocks(ctx, []int64{1})
			} else {
				_, err = client.call(ctx, id, []shipmentItem{{1, 1}}, tc.op)
			}
			if errors.Is(err, errInventoryUncertain) != tc.uncertain {
				t.Fatalf("error=%v", err)
			}
			if calls.Load() != 1 {
				t.Fatal("unexpected retry/redirect")
			}
			spans := recorder.Ended()
			if len(spans) != 1 || (spans[0].Status().Code == codes.Error) != tc.uncertain {
				t.Fatal("trace must reflect full response validation")
			}
			var log map[string]any
			if err = json.Unmarshal(logs.Bytes(), &log); err != nil {
				t.Fatal(err)
			}
			want := "confirmed"
			if tc.uncertain {
				want = "unknown"
			}
			if log["result"] != want || log["trace_id"] != spans[0].SpanContext().TraceID().String() {
				t.Fatal(log)
			}
		})
	}
}

func TestInventoryTimeoutAndNotFound(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if strings.HasSuffix(r.URL.Path, "/commit") {
			<-r.Context().Done()
			return
		}
		w.WriteHeader(404)
	}))
	defer server.Close()
	c, _ := newHTTPInventory(server.URL)
	if c.client.Timeout != time.Second {
		t.Fatal("one-second limit changed")
	}
	_, err := c.Get(context.Background(), paymentTestID, []shipmentItem{{1, 1}})
	if !errors.Is(err, errInventoryNotFound) {
		t.Fatal(err)
	}
	started := time.Now()
	_, err = c.Commit(context.Background(), paymentTestID, []shipmentItem{{1, 1}})
	if !errors.Is(err, errInventoryUncertain) || time.Since(started) > 2*time.Second || calls.Load() != 2 {
		t.Fatalf("timeout/retry: %v calls=%d", err, calls.Load())
	}
}
