package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const paymentTestID = "0123456789abcdef0123456789abcdef"

func TestPaymentClientValidation(t *testing.T) {
	for _, base := range []string{"", "payment:8080", "ftp://payment", "http://user:pw@payment", "http://payment/path", "http://payment?q=1", "http://payment/#fragment"} {
		if _, err := newHTTPPayments(base); err == nil {
			t.Fatalf("accepted %q", base)
		}
	}
}

func TestPaymentHTTPContract(t *testing.T) {
	for _, tc := range []struct {
		name        string
		cancel      bool
		status      int
		media, body string
		want        string
		bad         bool
	}{
		{name: "success", status: 201, want: "succeeded"},
		{name: "decline", status: 201, want: "failed"},
		{name: "cancel", cancel: true, status: 200, want: "cancelled"},
		{name: "conflict is uncertain", status: 409, bad: true},
		{name: "unavailable is uncertain", status: 503, bad: true},
		{name: "wrong success HTTP", status: 200, bad: true},
		{name: "redirect not followed", status: 307, bad: true},
		{name: "HTML", status: 201, media: "text/html", bad: true},
		{name: "malformed", status: 201, body: `{`, bad: true},
		{name: "multiple JSON", status: 201, body: `{} {}`, bad: true},
		{name: "oversized", status: 201, body: strings.Repeat(" ", 4097), bad: true},
		{name: "missing payment", status: 201, body: `{}`, bad: true},
		{name: "missing amount", status: 201, body: `{"payment":{"orderId":"` + paymentTestID + `","status":"succeeded"}}`, bad: true},
		{name: "wrong order", status: 201, body: `{"payment":{"orderId":"bad","amountYen":980,"status":"succeeded"}}`, bad: true},
		{name: "wrong amount", status: 201, body: `{"payment":{"orderId":"` + paymentTestID + `","amountYen":981,"status":"succeeded"}}`, bad: true},
		{name: "unknown status", status: 201, want: "pending", bad: true},
		{name: "already cancelled on create", status: 201, want: "cancelled"},
		{name: "cancel not confirmed", cancel: true, status: 200, want: "succeeded", bad: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Method != "POST" || r.Header.Get("X-Request-ID") != "order-request-1" {
					t.Error("method or request ID not propagated")
				}
				if tc.cancel {
					if r.URL.Path != "/payments/"+paymentTestID+"/cancel" || r.ContentLength != 0 {
						t.Error("invalid cancel request")
					}
				} else {
					var in struct {
						OrderID   string `json:"orderId"`
						AmountYen int64  `json:"amountYen"`
						Mode      string `json:"mode"`
					}
					if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.OrderID != paymentTestID || in.AmountYen != 980 || in.Mode != "success" || r.URL.Path != "/payments" {
						t.Errorf("input=%+v err=%v", in, err)
					}
				}
				media := tc.media
				if media == "" {
					media = "application/json"
				}
				w.Header().Set("Content-Type", media)
				w.Header().Set("Location", "/redirect-target")
				w.WriteHeader(tc.status)
				body := tc.body
				if body == "" {
					body = fmt.Sprintf(`{"payment":{"orderId":%q,"amountYen":980,"status":%q}}`, paymentTestID, tc.want)
				}
				_, _ = w.Write([]byte(body))
			}))
			defer server.Close()
			client, err := newHTTPPayments(server.URL)
			if err != nil {
				t.Fatal(err)
			}
			ctx := context.WithValue(context.Background(), requestIDKey{}, "order-request-1")
			var result string
			if tc.cancel {
				err = client.Cancel(ctx, paymentTestID, 980)
			} else {
				result, err = client.Create(ctx, paymentTestID, 980, "success")
			}
			if tc.bad {
				if !errors.Is(err, errPaymentUncertain) {
					t.Fatalf("expected uncertain error: %v", err)
				}
			} else if err != nil || (!tc.cancel && result != tc.want) {
				t.Fatalf("result=%s err=%v", result, err)
			}
			if calls.Load() != 1 {
				t.Fatalf("automatic resend: %d", calls.Load())
			}
		})
	}
}

func TestPaymentHTTPDeadlineAndCancellation(t *testing.T) {
	for _, stage := range []string{"headers", "body", "parent cancellation"} {
		t.Run(stage, func(t *testing.T) {
			var calls atomic.Int32
			release := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if stage == "body" {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(201)
					_, _ = w.Write([]byte(`{"payment":`))
					w.(http.Flusher).Flush()
				}
				select {
				case <-r.Context().Done():
				case <-release:
				}
			}))
			defer server.Close()
			defer close(release)
			client, _ := newHTTPPayments(server.URL)
			ctx := context.Background()
			if stage == "parent cancellation" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, 40*time.Millisecond)
				defer cancel()
			}
			start := time.Now()
			_, err := client.Create(ctx, paymentTestID, 980, "success")
			elapsed := time.Since(start)
			if !errors.Is(err, errPaymentUncertain) || calls.Load() != 1 {
				t.Fatalf("err=%v calls=%d", err, calls.Load())
			}
			// スケジューラの余裕を持たせながら、1秒の期限が本文読取にも適用されることを確認。
			if elapsed > 2*time.Second || (stage == "parent cancellation" && elapsed > 500*time.Millisecond) {
				t.Fatalf("deadline ignored: %v", elapsed)
			}
		})
	}
}
