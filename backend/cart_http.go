package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"regexp"
	"strconv"
)

const cartCookieName = "ec_cart"

var cartTokenPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

func tokenHash(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}
func cartHash(r *http.Request) string {
	c, err := r.Cookie(cartCookieName)
	if err != nil || !cartTokenPattern.MatchString(c.Value) {
		return ""
	}
	return tokenHash(c.Value)
}
func decodeCart(w http.ResponseWriter, r *http.Request, target any) error {
	// クロスオリジンのフォーム送信を許可しない。CORSは有効化しない。
	if r.Header.Get("X-Cart-Request") != "1" {
		return errInvalid
	}
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || media != "application/json" {
		return errInvalid
	}
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	d.DisallowUnknownFields()
	if err = d.Decode(target); err != nil {
		return errInvalid
	}
	if d.Decode(&struct{}{}) != io.EOF {
		return errInvalid
	}
	return nil
}
func registerCartHandlers(mux *http.ServeMux, s cartStore, logger *slog.Logger, wrap func(func(http.ResponseWriter, *http.Request, *string)) http.HandlerFunc) {
	failure := func(ctx context.Context, w http.ResponseWriter, err error, orderID string) {
		status, message := 503, "カート処理の結果を確認できませんでした。再取得してください。"
		switch {
		case errors.Is(err, errConflict):
			status, message = 409, "別の操作でカートが更新されました。最新の内容を確認してやり直してください。"
		case errors.Is(err, errNotFound):
			status, message = 404, "カートの有効期限が切れたか、商品が見つかりません。再取得してください。"
		case errors.Is(err, errEmptyCart):
			status, message = 400, "カートが空です。"
		case errors.Is(err, errInvalid):
			status, message = 400, "入力を確認してください。カートは100商品まで、数量は正の整数、金額は安全な整数範囲で指定してください。"
		}
		body := map[string]string{"error": message, "requestId": w.Header().Get("X-Request-ID")}
		if status == 503 {
			logger.ErrorContext(ctx, "cart operation failed", "request_id", body["requestId"], "service", "order", "order_id", orderID, "error", err)
			if orderID != "" {
				body["orderId"] = orderID
				body["error"] = "注文・決済・配送の結果を確認できません。再注文せず、注文IDで状況を確認して処理を再開してください。"
			}
		}
		writeJSON(w, status, body)
	}
	mux.HandleFunc("GET /api/cart", wrap(func(w http.ResponseWriter, r *http.Request, _ *string) {
		hash := cartHash(r)
		c, err := s.GetCart(r.Context(), hash)
		if errors.Is(err, errNotFound) {
			token := fmt.Sprintf("%x%x", randomID(), randomID())
			c, err = s.NewCart(r.Context(), tokenHash(token))
			if err == nil {
				http.SetCookie(w, &http.Cookie{Name: cartCookieName, Value: token, Path: "/api/cart", MaxAge: 30 * 24 * 60 * 60, Expires: c.ExpiresAt, HttpOnly: true, Secure: r.TLS != nil, SameSite: http.SameSiteLaxMode})
			}
		}
		if err != nil {
			failure(r.Context(), w, err, "")
			return
		}
		writeJSON(w, 200, map[string]any{"cart": c})
	}))
	update := wrap(func(w http.ResponseWriter, r *http.Request, _ *string) {
		var in struct {
			Version  *int64 `json:"version"`
			Quantity int    `json:"quantity"`
		}
		err := decodeCart(w, r, &in)
		if err != nil || in.Version == nil {
			failure(r.Context(), w, errInvalid, "")
			return
		}
		id, err := strconv.ParseInt(r.PathValue("productId"), 10, 64)
		if err != nil {
			failure(r.Context(), w, errInvalid, "")
			return
		}
		c, err := s.SetCartItem(r.Context(), cartHash(r), *in.Version, id, in.Quantity, r.Method == "DELETE")
		if err != nil {
			failure(r.Context(), w, err, "")
			return
		}
		writeJSON(w, 200, map[string]any{"cart": c})
	})
	mux.HandleFunc("PUT /api/cart/items/{productId}", update)
	mux.HandleFunc("DELETE /api/cart/items/{productId}", update)
	mux.HandleFunc("POST /api/cart/checkout", wrap(func(w http.ResponseWriter, r *http.Request, orderID *string) {
		var in struct {
			Version      *int64 `json:"version"`
			PaymentMode  string `json:"paymentMode"`
			ShippingMode string `json:"shippingMode"`
		}
		if err := decodeCart(w, r, &in); err != nil || in.Version == nil {
			failure(r.Context(), w, errInvalid, "")
			return
		}
		*orderID = fmt.Sprintf("%x", randomID())
		o, err := s.Checkout(r.Context(), cartHash(r), *in.Version, *orderID, in.PaymentMode, in.ShippingMode)
		if err != nil {
			failure(r.Context(), w, err, *orderID)
			return
		}
		w.Header().Set("Location", "/api/orders/"+o.ID)
		writeJSON(w, 201, map[string]any{"order": o})
	}))
}
