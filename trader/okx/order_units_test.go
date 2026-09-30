package okx

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

type orderUnitsTransport struct{ mode string }

func (rt orderUnitsTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	body := `{"code":"0","data":[]}`
	switch r.URL.Path {
	case okxInstrumentsPath:
		body = `{"code":"0","data":[{"instId":"BTC-USDT-SWAP","ctVal":"0.01","lotSz":"0.1","minSz":"0.1","tickSz":"0.1","ctType":"linear"}]}`
	case okxPendingOrdersPath:
		body = `{"code":"0","data":[{"ordId":"entry","clOrdId":"stable-entry","sz":"4.6","px":"80000","side":"buy","posSide":"long","ordType":"limit"}]}`
		if rt.mode == "normal-error" {
			body = `{"code":"50011","msg":"rate limited"}`
		}
	case okxAlgoPendingPath:
		body = `{"code":"0","data":[{"algoId":"stop","algoClOrdId":"stable-stop","sz":"4.6","slTriggerPx":"79000","side":"sell","posSide":"long"}]}`
		switch rt.mode {
		case "algo-error":
			body = `{"code":"50011","msg":"rate limited"}`
		case "malformed":
			body = `{"code":"0","data":{}}`
		case "nan":
			body = `{"code":"0","data":[{"sz":"NaN"}]}`
		case "close-all":
			body = `{"code":"0","data":[{"algoId":"stop","sz":"-1","closeFraction":"1","slTriggerPx":"79000","side":"sell","posSide":"long"}]}`
		}
	}
	return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
}
func TestOpenOrderQuantitiesUseBaseUnitsAndFailClosed(t *testing.T) {
	for _, mode := range []string{"ok", "normal-error", "algo-error", "malformed", "nan", "close-all"} {
		t.Run(mode, func(t *testing.T) {
			ex := newTestOKXTrader(&recordingTransport{}, true)
			ex.httpClient.Transport = orderUnitsTransport{mode}
			orders, err := ex.GetOpenOrders("BTCUSDT")
			if mode != "ok" && mode != "close-all" {
				if err == nil || len(orders) != 0 {
					t.Fatalf("partial/invalid query returned success: %+v %v", orders, err)
				}
				return
			}
			if err != nil || len(orders) != 2 {
				t.Fatalf("orders=%+v err=%v", orders, err)
			}
			if orders[0].Quantity != .046 || orders[0].ClientID != "stable-entry" {
				t.Fatalf("normal order units/identity: %+v", orders[0])
			}
			if mode == "ok" && (orders[1].Quantity != .046 || orders[1].ClientID != "stable-stop" || orders[1].OrderKind != "ALGO") {
				t.Fatalf("stop units/identity: %+v", orders[1])
			}
			if mode == "close-all" && !orders[1].ClosePosition {
				t.Fatal("whole position stop lost coverage")
			}
		})
	}
}
