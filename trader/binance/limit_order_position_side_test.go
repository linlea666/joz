package binance

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"nofx/trader/types"

	"github.com/adshao/go-binance/v2/futures"
)

// newLimitOrderCaptureServer returns a FuturesTrader wired to a mock server
// that records the form values of every POST /fapi/v1/order request.
func newLimitOrderCaptureServer(t *testing.T) (*FuturesTrader, *[]map[string]string, func()) {
	t.Helper()
	captured := &[]map[string]string{}

	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var respBody interface{}
		switch {
		case r.URL.Path == "/fapi/v1/exchangeInfo":
			respBody = map[string]interface{}{
				"symbols": []map[string]interface{}{
					{
						"symbol":            "JASMYUSDT",
						"status":            "TRADING",
						"baseAsset":         "JASMY",
						"quoteAsset":        "USDT",
						"pricePrecision":    7,
						"quantityPrecision": 0,
						"filters": []map[string]interface{}{
							{"filterType": "PRICE_FILTER", "minPrice": "0.0000010", "maxPrice": "100", "tickSize": "0.0000010"},
							{"filterType": "LOT_SIZE", "minQty": "1", "maxQty": "100000000", "stepSize": "1"},
						},
					},
				},
			}
		case r.URL.Path == "/fapi/v1/order" && r.Method == "POST":
			_ = r.ParseForm()
			form := map[string]string{}
			for k := range r.Form {
				form[k] = r.FormValue(k)
			}
			*captured = append(*captured, form)
			respBody = map[string]interface{}{
				"orderId":       8847141244,
				"symbol":        r.FormValue("symbol"),
				"status":        "NEW",
				"clientOrderId": r.FormValue("newClientOrderId"),
				"side":          r.FormValue("side"),
				"positionSide":  r.FormValue("positionSide"),
			}
		case r.URL.Path == "/fapi/v1/leverage":
			respBody = map[string]interface{}{"leverage": 10, "maxNotionalValue": "1000000", "symbol": r.FormValue("symbol")}
		default:
			respBody = map[string]interface{}{}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(respBody)
	}))

	client := futures.NewClient("test_api_key", "test_secret_key")
	client.BaseURL = mockServer.URL
	client.HTTPClient = mockServer.Client()

	return &FuturesTrader{client: client, cacheDuration: 0}, captured, mockServer.Close
}

// A reduce-only TP for a LONG (SELL + PositionSide=LONG) must be sent against
// the LONG position side. The legacy side-derived mapping turned it into
// SELL/SHORT — an OPEN SHORT in hedge mode (real incident: JASMY copy-trade
// TP orders showed as "限价/开空" on Binance).
func TestBinancePlaceLimitOrderHonorsExplicitPositionSide(t *testing.T) {
	trader, captured, cleanup := newLimitOrderCaptureServer(t)
	defer cleanup()

	_, err := trader.PlaceLimitOrder(&types.LimitOrderRequest{
		Symbol:       "JASMYUSDT",
		Side:         "SELL",
		PositionSide: "LONG",
		Price:        0.0051,
		Quantity:     16556,
		ReduceOnly:   true,
	})
	if err != nil {
		t.Fatalf("PlaceLimitOrder failed: %v", err)
	}

	if len(*captured) != 1 {
		t.Fatalf("expected 1 order request, got %d", len(*captured))
	}
	form := (*captured)[0]
	if form["side"] != "SELL" {
		t.Fatalf("expected side=SELL, got %q", form["side"])
	}
	if form["positionSide"] != "LONG" {
		t.Fatalf("expected positionSide=LONG (close long), got %q", form["positionSide"])
	}
	if _, ok := form["reduceOnly"]; ok {
		t.Fatalf("reduceOnly must not be sent in hedge mode, got %q", form["reduceOnly"])
	}
}

// Grid callers leave PositionSide empty and rely on the legacy side-derived
// open semantics (BUY=LONG, SELL=SHORT) — that behavior must not change.
func TestBinancePlaceLimitOrderLegacySideDerivation(t *testing.T) {
	trader, captured, cleanup := newLimitOrderCaptureServer(t)
	defer cleanup()

	for _, tc := range []struct {
		side    string
		wantPos string
	}{
		{"BUY", "LONG"},
		{"SELL", "SHORT"},
	} {
		_, err := trader.PlaceLimitOrder(&types.LimitOrderRequest{
			Symbol:   "JASMYUSDT",
			Side:     tc.side,
			Price:    0.005,
			Quantity: 1000,
		})
		if err != nil {
			t.Fatalf("PlaceLimitOrder(%s) failed: %v", tc.side, err)
		}
		form := (*captured)[len(*captured)-1]
		if form["positionSide"] != tc.wantPos {
			t.Fatalf("legacy %s: expected positionSide=%s, got %q", tc.side, tc.wantPos, form["positionSide"])
		}
	}
}
