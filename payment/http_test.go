package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const testOrderID = "0123456789abcdef0123456789abcdef"

type stubStore struct {
	input PaymentInput
	calls int
	err   error
}

func (s *stubStore) Create(_ context.Context, in PaymentInput) (Payment, error) {
	s.calls++
	s.input = in
	return Payment{OrderID: in.OrderID, AmountYen: *in.AmountYen, Status: "succeeded"}, s.err
}
func (s *stubStore) Get(_ context.Context, id string) (Payment, error) {
	s.calls++
	return Payment{OrderID: id, Status: "succeeded"}, s.err
}
func (s *stubStore) Cancel(_ context.Context, id string) (Payment, error) {
	s.calls++
	return Payment{OrderID: id, Status: "cancelled"}, s.err
}
func (s *stubStore) Ping(context.Context) error { return s.err }

func request(h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}
func quietLogger() *slog.Logger { return slog.New(slog.NewJSONHandler(io.Discard, nil)) }

func TestInputHTTP(t *testing.T) {
	for _, body := range []string{
		`{}`, `null`, `[]`, `{"orderId":"` + testOrderID + `"}`,
		`{"orderId":"` + testOrderID + `","amountYen":null}`,
		`{"orderId":"bad","amountYen":100}`,
		`{"orderId":"` + strings.Repeat("A", 32) + `","amountYen":100}`,
		`{"orderId":"` + testOrderID + `","amountYen":-1}`,
		`{"orderId":"` + testOrderID + `","amountYen":1.5}`,
		`{"orderId":"` + testOrderID + `","amountYen":"100"}`,
		`{"orderId":"` + testOrderID + `","amountYen":9007199254740992}`,
		`{"orderId":"` + testOrderID + `","amountYen":0,"mode":"bad"}`,
		`{"orderId":"` + testOrderID + `","amountYen":0,"extra":true}`,
		`{"orderId":"` + testOrderID + `","amountYen":0} {}`,
		`{"orderId":`, strings.Repeat(" ", 4097) + `{}`,
	} {
		t.Run(body[:min(100, len(body))], func(t *testing.T) {
			s := &stubStore{}
			w := request(newHandler(s, quietLogger()), "POST", "/payments", body)
			if w.Code != 400 || s.calls != 0 {
				t.Fatalf("code=%d calls=%d body=%s", w.Code, s.calls, w.Body)
			}
		})
	}
	for _, amount := range []string{"0", "9007199254740991"} {
		s := &stubStore{}
		w := request(newHandler(s, quietLogger()), "POST", "/payments", `{"orderId":"`+testOrderID+`","amountYen":`+amount+`}`)
		if w.Code != 201 || s.input.Mode != "success" || w.Header().Get("Location") != "/payments/"+testOrderID {
			t.Fatalf("code=%d input=%+v headers=%v", w.Code, s.input, w.Header())
		}
	}
	s := &stubStore{}
	h := newHandler(s, quietLogger())
	r := httptest.NewRequest("POST", "/payments", strings.NewReader(`{}`))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 415 || s.calls != 0 {
		t.Fatalf("missing content-type: %d", w.Code)
	}
	for _, path := range []string{"/payments/bad", "/payments/bad/cancel"} {
		method := "GET"
		if strings.HasSuffix(path, "cancel") {
			method = "POST"
		}
		if w := request(h, method, path, ""); w.Code != 400 {
			t.Fatalf("invalid id: %d", w.Code)
		}
	}
	if w := request(h, "POST", "/payments/"+testOrderID+"/cancel", `{"amountYen":100}`); w.Code != 400 || s.calls != 0 {
		t.Fatalf("cancel body: %d calls=%d", w.Code, s.calls)
	}
}

func TestErrorsAndHealthHTTP(t *testing.T) {
	for _, tc := range []struct {
		err    error
		status int
	}{
		{errNotFound, 404}, {errConflict, 409}, {errInvalid, 400}, {errors.New("private database error"), 503},
	} {
		for _, route := range []struct{ method, path, body string }{
			{"GET", "/payments/" + testOrderID, ""},
			{"POST", "/payments/" + testOrderID + "/cancel", ""},
			{"POST", "/payments", `{"orderId":"` + testOrderID + `","amountYen":100}`},
		} {
			h := newHandler(&stubStore{err: tc.err}, quietLogger())
			w := request(h, route.method, route.path, route.body)
			var body map[string]string
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if w.Code != tc.status || strings.Contains(w.Body.String(), "private database") || body["orderId"] != testOrderID || body["requestId"] == "" {
				t.Fatalf("status=%d body=%s", w.Code, w.Body)
			}
		}
	}
	for _, fail := range []bool{false, true} {
		s := &stubStore{}
		want := 200
		if fail {
			s.err = errors.New("database unavailable")
			want = 503
		}
		if w := request(newHandler(s, quietLogger()), "GET", "/healthz", ""); w.Code != want {
			t.Fatalf("health: %d", w.Code)
		}
	}
}

func TestRequestCorrelation(t *testing.T) {
	for _, id := range []string{"lesson3-payment-01", "", strings.Repeat("x", 129), "not valid"} {
		var logs bytes.Buffer
		h := newHandler(&stubStore{}, slog.New(slog.NewJSONHandler(&logs, nil)))
		r := httptest.NewRequest("GET", "/payments/"+testOrderID, nil)
		r.Header.Set("X-Request-ID", id)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		returned := w.Header().Get("X-Request-ID")
		if id == "lesson3-payment-01" && returned != id {
			t.Fatal("request ID was not propagated")
		}
		if id != "lesson3-payment-01" && !orderIDPattern.MatchString(returned) {
			t.Fatal("invalid generated ID")
		}
		var entry map[string]any
		if err := json.Unmarshal(logs.Bytes(), &entry); err != nil {
			t.Fatal(err)
		}
		if entry["service"] != "payment" || entry["request_id"] != returned || entry["order_id"] != testOrderID || entry["result"] != "succeeded" {
			t.Fatalf("log=%v", entry)
		}
		if w.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("payments must not be cached")
		}
	}
}
