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
	input ShipmentInput
	calls int
	err   error
}

func (s *stubStore) Create(_ context.Context, in ShipmentInput) (Shipment, error) {
	s.calls++
	s.input = in
	return Shipment{OrderID: in.OrderID, Items: in.Items, Mode: in.Mode, Status: "requested"}, s.err
}
func (s *stubStore) Get(_ context.Context, id string) (Shipment, error) {
	s.calls++
	return Shipment{OrderID: id, Status: "requested"}, s.err
}
func (s *stubStore) Cancel(_ context.Context, id string) (Shipment, error) {
	s.calls++
	return Shipment{OrderID: id, Status: "cancelled"}, s.err
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
		`{"orderId":"bad","items":[{"productId":1,"quantity":1}]}`,
		`{"orderId":"` + testOrderID + `","items":[]}`,
		`{"orderId":"` + testOrderID + `","items":[{"productId":0,"quantity":1}]}`,
		`{"orderId":"` + testOrderID + `","items":[{"productId":1,"quantity":0}]}`,
		`{"orderId":"` + testOrderID + `","items":[{"productId":1,"quantity":2147483648}]}`,
		`{"orderId":"` + testOrderID + `","items":[{"productId":1,"quantity":1},{"productId":1,"quantity":2}]}`,
		`{"orderId":"` + testOrderID + `","items":[{"productId":1,"quantity":1}],"mode":"bad"}`,
		`{"orderId":"` + testOrderID + `","items":[{"productId":1,"quantity":1}],"extra":1}`,
		`{"orderId":"` + testOrderID + `","items":[{"productId":1,"quantity":1}]} {}`,
		strings.Repeat(" ", 16385) + `{}`,
	} {
		s := &stubStore{}
		w := request(newHandler(s, quietLogger()), "POST", "/shipments", body)
		if w.Code != 400 || s.calls != 0 {
			t.Fatalf("code=%d calls=%d body=%s", w.Code, s.calls, w.Body)
		}
	}
	{
		s := &stubStore{}
		w := request(newHandler(s, quietLogger()), "POST", "/shipments", `{"orderId":"`+testOrderID+`","items":[{"productId":2,"quantity":1},{"productId":1,"quantity":2}]}`)
		if w.Code != 201 || s.input.Mode != "success" || s.input.Items[0].ProductID != 1 || w.Header().Get("Location") != "/shipments/"+testOrderID {
			t.Fatalf("code=%d input=%+v", w.Code, s.input)
		}
	}

	s := &stubStore{}
	h := newHandler(s, quietLogger())
	r := httptest.NewRequest("POST", "/shipments", strings.NewReader(`{}`))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 415 || s.calls != 0 {
		t.Fatalf("missing content-type: %d", w.Code)
	}
	for _, path := range []string{"/shipments/bad", "/shipments/bad/cancel"} {
		method := "GET"
		if strings.HasSuffix(path, "cancel") {
			method = "POST"
		}
		if w := request(h, method, path, ""); w.Code != 400 {
			t.Fatalf("invalid id: %d", w.Code)
		}
	}
	if w := request(h, "POST", "/shipments/"+testOrderID+"/cancel", `{"items":[{"productId":1,"quantity":1}]}`); w.Code != 400 || s.calls != 0 {
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
			{"GET", "/shipments/" + testOrderID, ""},
			{"POST", "/shipments/" + testOrderID + "/cancel", ""},
			{"POST", "/shipments", `{"orderId":"` + testOrderID + `","items":[{"productId":1,"quantity":1}]}`},
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
	for _, id := range []string{"lesson3-shipping-01", "", strings.Repeat("x", 129), "not valid"} {
		var logs bytes.Buffer
		h := newHandler(&stubStore{}, slog.New(slog.NewJSONHandler(&logs, nil)))
		r := httptest.NewRequest("GET", "/shipments/"+testOrderID, nil)
		r.Header.Set("X-Request-ID", id)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		returned := w.Header().Get("X-Request-ID")
		if id == "lesson3-shipping-01" && returned != id {
			t.Fatal("request ID was not propagated")
		}
		if id != "lesson3-shipping-01" && !orderIDPattern.MatchString(returned) {
			t.Fatal("invalid generated ID")
		}
		var entry map[string]any
		if err := json.Unmarshal(logs.Bytes(), &entry); err != nil {
			t.Fatal(err)
		}
		if entry["service"] != "shipping" || entry["request_id"] != returned || entry["order_id"] != testOrderID || entry["result"] != "requested" {
			t.Fatalf("log=%v", entry)
		}
		if w.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("shippings must not be cached")
		}
	}
}
