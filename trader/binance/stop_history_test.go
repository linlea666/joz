package binance

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"nofx/trader/types"

	"github.com/adshao/go-binance/v2/futures"
)

func TestExactAlgoHistoryQuery(t *testing.T) {
	for _, tc := range []struct {
		name, status, child string
		missing, mismatch   bool
	}{
		{name: "cancelled", status: "CANCELED"}, {name: "finished", status: "FINISHED", child: "FILLED"},
		{name: "live child", status: "FINISHED", child: "NEW"}, {name: "expired history", missing: true},
		{name: "wrong identity", status: "CANCELED", mismatch: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" {
					t.Errorf("write request %s", r.Method)
				}
				if r.URL.Query().Get("signature") == "" {
					t.Error("history not authenticated")
				}
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/fapi/v1/algoOrder":
					if r.URL.Query().Get("clientAlgoId") != "durable-stop" && r.URL.Query().Get("algoId") != "42" {
						t.Error("missing exact identity")
					}
					if tc.missing {
						w.WriteHeader(400)
						w.Write([]byte(`{"code":-2013,"msg":"Order does not exist"}`))
						return
					}
					symbol := "BTCUSDT"
					if tc.mismatch {
						symbol = "ETHUSDT"
					}
					actual := ""
					if tc.child != "" {
						actual = "99"
					}
					fmt.Fprintf(w, `{"algoId":42,"clientAlgoId":"durable-stop","symbol":%q,"side":"SELL","positionSide":"LONG","orderType":"STOP_MARKET","algoStatus":%q,"actualOrderId":%q,"triggerPrice":"95","closePosition":true}`, symbol, tc.status, actual)
				case "/fapi/v1/order":
					if r.URL.Query().Get("orderId") != "99" || r.URL.Query().Get("symbol") != "BTCUSDT" {
						t.Error("wrong child query")
					}
					fmt.Fprintf(w, `{"symbol":"BTCUSDT","orderId":99,"side":"SELL","positionSide":"LONG","status":%q}`, tc.child)
				default:
					t.Errorf("unexpected endpoint %s", r.URL.Path)
				}
			}))
			defer s.Close()
			client := futures.NewClient("test", "test")
			client.BaseURL = s.URL
			client.HTTPClient = s.Client()
			v := &FuturesTrader{client: client}
			for _, id := range []string{"", "42"} {
				result, err := v.GetStopOrder("BTCUSDT", id, "durable-stop")
				if tc.missing {
					if !errors.Is(err, types.ErrManagedOrderNotFound) {
						t.Fatal(err)
					}
					continue
				}
				if tc.mismatch {
					if err == nil {
						t.Fatal("identity mismatch accepted")
					}
					continue
				}
				if err != nil {
					t.Fatal(err)
				}
				if result.Terminal() != (tc.child != "NEW") {
					t.Fatalf("incorrect terminality: %+v", result)
				}
			}
		})
	}
}

func TestStopHistoryDiagnosticDoesNotRenderSignedURL(t *testing.T) {
	original := &url.Error{Op: "Get", URL: "https://example.test?signature=private-signature", Err: errors.New("timeout")}
	err := &stopHistoryError{cause: original}
	if strings.Contains(err.Error(), "private-signature") || strings.Contains(err.Error(), "example.test") {
		t.Fatal("signed request leaked")
	}
	var got *url.Error
	if !errors.As(err, &got) {
		t.Fatal("error category lost")
	}
}
