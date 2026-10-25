package binance

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/adshao/go-binance/v2/common"
	"github.com/adshao/go-binance/v2/futures"
	"nofx/trader/types"
)

func TestClassifyManagedRejectionCodeTable(t *testing.T) {
	cases := []struct {
		code      int64
		rejected  bool
		retryable bool
	}{
		// Existing definite rejections.
		{-1100, true, false}, {-1102, true, false}, {-1111, true, false}, {-1116, true, false},
		{-1117, true, false}, {-1121, true, false}, {-2019, true, false}, {-2020, true, false},
		{-2021, true, false}, {-2022, true, false}, {-2027, true, false}, {-4003, true, false},
		{-4004, true, false}, {-4005, true, false}, {-4014, true, false}, {-4023, true, false},
		{-4164, true, false}, {-4509, true, false},
		// Added: request never executed.
		{-4061, true, false}, // position side mismatch
		{-4130, true, false}, // closePosition stop already exists
		{-4045, true, false}, // max stop orders
		{-4016, true, false}, // above multiplier cap
		{-4024, true, false}, // below multiplier floor
		{-4131, true, false}, // PERCENT_PRICE counterparty protection
		{-1003, true, true},  // rate limit (HTTP 429/418 body)
		{-1021, true, true},  // timestamp
		{-1022, true, true},  // signature
		{-2015, true, true},  // key / IP / permission
		// Outcome unknown: must stay unclassified.
		{-1007, false, false}, // backend timeout, send status unknown
		{-1006, false, false},
		{-1001, false, false},
		{0, false, false}, // unparsable 5xx / HTML body
	}
	for _, c := range cases {
		t.Run(fmt.Sprint(c.code), func(t *testing.T) {
			raw := fmt.Errorf("wrapped: %w", &common.APIError{Code: c.code, Message: "x"})
			err := classifyManagedRejection(raw)
			if got := errors.Is(err, types.ErrManagedOrderRejected); got != c.rejected {
				t.Fatalf("code %d rejected=%v want %v", c.code, got, c.rejected)
			}
			if got := errors.Is(err, types.ErrManagedOrderRetryable); got != c.retryable {
				t.Fatalf("code %d retryable=%v want %v", c.code, got, c.retryable)
			}
			var rejection *types.ManagedRejection
			if c.rejected && (!errors.As(err, &rejection) || rejection.Code != c.code) {
				t.Fatalf("code %d not preserved: %v", c.code, err)
			}
			var apiErr *common.APIError
			if !errors.As(err, &apiErr) || apiErr.Code != c.code {
				t.Fatal("venue error cause lost")
			}
		})
	}
	if err := classifyManagedRejection(errors.New("net/http: timeout")); errors.Is(err, types.ErrManagedOrderRejected) || errors.Is(err, types.ErrManagedOrderRetryable) {
		t.Fatal("transport error classified as definite")
	}
}

func TestSetManagedStopLossMarketRulesFailureIsNotSent(t *testing.T) {
	for _, status := range []int{500, 429} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			algo := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/fapi/v1/exchangeInfo":
					w.WriteHeader(status)
					_, _ = w.Write([]byte(`{"code":-1003,"msg":"Too many requests"}`))
				case "/fapi/v1/algoOrder":
					algo++
					_, _ = w.Write([]byte(`{"algoId":1}`))
				}
			}))
			defer server.Close()
			client := futures.NewClient("k", "s")
			client.BaseURL = server.URL
			client.HTTPClient = server.Client()
			ex := &FuturesTrader{client: client}
			err := ex.SetManagedStopLoss("BTCUSDT", "LONG", 1, 90000, "ct-test")
			if !errors.Is(err, types.ErrManagedOrderNotSent) || !errors.Is(err, types.ErrManagedOrderRetryable) {
				t.Fatalf("pre-send failure not recognisable: %v", err)
			}
			if algo != 0 {
				t.Fatal("stop request was sent")
			}
		})
	}
}

func TestSetManagedStopLossVenueTimeoutStaysUnknown(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/fapi/v1/exchangeInfo":
			_, _ = w.Write([]byte(`{"symbols":[{"symbol":"BTCUSDT","status":"TRADING","contractType":"PERPETUAL","quoteAsset":"USDT","filters":[{"filterType":"LOT_SIZE","minQty":"0.001","maxQty":"1000","stepSize":"0.001"},{"filterType":"PRICE_FILTER","minPrice":"556.80","maxPrice":"4529764","tickSize":"0.10"},{"filterType":"PERCENT_PRICE","multiplierUp":"1.0500","multiplierDown":"0.9500","multiplierDecimal":"4"}]}]}`))
		case "/fapi/v1/algoOrder":
			w.WriteHeader(503)
			_, _ = w.Write([]byte(`{"code":-1007,"msg":"Timeout waiting for response from backend server. Send status unknown; execution status unknown."}`))
		}
	}))
	defer server.Close()
	client := futures.NewClient("k", "s")
	client.BaseURL = server.URL
	client.HTTPClient = server.Client()
	ex := &FuturesTrader{client: client}
	err := ex.SetManagedStopLoss("BTCUSDT", "LONG", 1, 90000, "ct-test")
	if err == nil || errors.Is(err, types.ErrManagedOrderRejected) || errors.Is(err, types.ErrManagedOrderNotSent) || errors.Is(err, types.ErrManagedOrderRetryable) {
		t.Fatalf("-1007 must remain outcome-unknown: %v", err)
	}
	rules, err := ex.MarketRules("BTCUSDT")
	if err != nil {
		t.Fatal(err)
	}
	if rules.PercentPriceUp != 1.05 || rules.PercentPriceDown != .95 || rules.MinPrice != 556.8 || rules.MaxPrice != 4529764 {
		t.Fatalf("price filters not parsed: %+v", rules)
	}
}

func TestSubmitManagedOrderLocalValidationIsNotSent(t *testing.T) {
	ex := &FuturesTrader{client: futures.NewClient("k", "s")}
	rules := &types.ManagedMarketRules{Symbol: "BTCUSDT", Status: "TRADING", QuantityStep: .001, MinQuantity: .001, PriceTick: .1}
	_, err := ex.SubmitManagedOrder(&types.ManagedOrderRequest{Symbol: "BTCUSDT", Type: "LIMIT", Side: "SELL", PositionSide: "LONG", ClientID: "ct-x", Quantity: 0, Price: 100, ReduceOnly: true, Rules: rules})
	if !errors.Is(err, types.ErrManagedOrderNotSent) || !errors.Is(err, types.ErrManagedOrderRejected) {
		t.Fatalf("local validation failure must be recognisably not sent: %v", err)
	}
}
