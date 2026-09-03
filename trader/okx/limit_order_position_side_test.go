package okx

import (
	"testing"

	"nofx/trader/types"
)

// A reduce-only TP for a LONG (SELL + PositionSide=LONG) must be sent as
// side=sell posSide=long WITHOUT the reduceOnly flag (illegal in long/short
// mode). The legacy side-derived mapping produced sell/short + reduceOnly,
// which OKX rejected with "Insufficient USDT margin" (real incident: HYPE
// copy-trade TP fell back to trigger orders).
func TestOKXPlaceLimitOrderHonorsExplicitPositionSide(t *testing.T) {
	rt := &recordingTransport{}
	trader := newTestOKXTrader(rt, true)

	_, err := trader.PlaceLimitOrder(&types.LimitOrderRequest{
		Symbol:       "BTCUSDT",
		Side:         "SELL",
		PositionSide: "LONG",
		Price:        98000,
		Quantity:     0.1,
		ReduceOnly:   true,
	})
	if err != nil {
		t.Fatalf("PlaceLimitOrder failed: %v", err)
	}

	orderRequests := rt.requestsForPath(okxOrderPath)
	if len(orderRequests) != 1 {
		t.Fatalf("expected 1 limit order request, got %d", len(orderRequests))
	}
	body := orderRequests[0].Body
	if body["side"] != "sell" {
		t.Fatalf("expected side=sell, got %#v", body["side"])
	}
	if body["posSide"] != "long" {
		t.Fatalf("expected posSide=long (close long), got %#v", body["posSide"])
	}
	if _, ok := body["reduceOnly"]; ok {
		t.Fatalf("reduceOnly must not be sent in long/short mode, got %#v", body["reduceOnly"])
	}
}

// Grid callers leave PositionSide empty: the legacy side-derived mapping and
// the reduceOnly passthrough must be preserved unchanged.
func TestOKXPlaceLimitOrderLegacySideDerivation(t *testing.T) {
	rt := &recordingTransport{}
	trader := newTestOKXTrader(rt, true)

	_, err := trader.PlaceLimitOrder(&types.LimitOrderRequest{
		Symbol:     "BTCUSDT",
		Side:       "SELL",
		Price:      98000,
		Quantity:   0.1,
		ReduceOnly: true,
	})
	if err != nil {
		t.Fatalf("PlaceLimitOrder failed: %v", err)
	}

	orderRequests := rt.requestsForPath(okxOrderPath)
	if len(orderRequests) != 1 {
		t.Fatalf("expected 1 limit order request, got %d", len(orderRequests))
	}
	body := orderRequests[0].Body
	if body["posSide"] != "short" {
		t.Fatalf("legacy SELL: expected posSide=short, got %#v", body["posSide"])
	}
	if body["reduceOnly"] != true {
		t.Fatalf("legacy path must pass reduceOnly through, got %#v", body["reduceOnly"])
	}
}
