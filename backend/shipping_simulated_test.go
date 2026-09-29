package main

import "context"

// Order単体のDBテスト用。通常バイナリには組み込まない。
type simulatedShipping struct{}

func (simulatedShipping) Get(context.Context, string, []shipmentItem, string) (string, error) {
	return "", errShippingNotFound
}
func (simulatedShipping) Create(_ context.Context, _ string, _ []shipmentItem, mode string) (string, error) {
	if mode == "fail" {
		return "failed", nil
	}
	return "requested", nil
}
func (simulatedShipping) Cancel(context.Context, string, []shipmentItem, string) error { return nil }
