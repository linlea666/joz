package binance

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/adshao/go-binance/v2/futures"
	"nofx/trader/types"
)

func TestManagedOrderUsesStableIDAndPreciseCancel(t *testing.T) {
	var calls []map[string]string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.Method == "DELETE" {
			body, _ := io.ReadAll(r.Body)
			values, _ := url.ParseQuery(string(body))
			for k, v := range values {
				r.Form[k] = v
			}
		}
		form := map[string]string{"method": r.Method, "path": r.URL.Path}
		for k := range r.Form {
			form[k] = r.FormValue(k)
		}
		calls = append(calls, form)
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/fapi/v1/exchangeInfo" {
			_, _ = w.Write([]byte(`{"symbols":[{"symbol":"SNDKUSDT","status":"TRADING","contractType":"PERPETUAL","quoteAsset":"USDT","filters":[{"filterType":"LOT_SIZE","minQty":"0.005","maxQty":"10000","stepSize":"0.005"},{"filterType":"MARKET_LOT_SIZE","minQty":"0.005","maxQty":"10000","stepSize":"0.005"},{"filterType":"PRICE_FILTER","minPrice":"0.01","maxPrice":"100000","tickSize":"0.01"},{"filterType":"MIN_NOTIONAL","notional":"5"}]}]}`))
			return
		}
		if r.FormValue("origClientOrderId") == "missing" {
			w.WriteHeader(400)
			_, _ = w.Write([]byte(`{"code":-2013,"msg":"Order does not exist."}`))
			return
		}
		clientID := r.FormValue("newClientOrderId")
		if clientID == "" {
			clientID = r.FormValue("origClientOrderId")
		}
		status := "NEW"
		if r.Method == "DELETE" {
			status = "CANCELED"
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"symbol": "SNDKUSDT", "orderId": 123, "clientOrderId": clientID, "status": status, "type": "LIMIT", "side": "SELL", "positionSide": "LONG", "origQty": "0.015", "executedQty": "0.005", "avgPrice": "1821.35", "price": "1821.35"})
	}))
	defer server.Close()
	client := futures.NewClient("test-key", "test-secret")
	client.BaseURL = server.URL
	client.HTTPClient = server.Client()
	ex := &FuturesTrader{client: client}
	rules, err := ex.MarketRules("SNDKUSDT")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = ex.MarketRules("UNKNOWNUSDT"); err == nil {
		t.Fatal("unlisted symbol accepted")
	}
	r, err := ex.SubmitManagedOrder(&types.ManagedOrderRequest{Symbol: "SNDKUSDT", Type: "LIMIT", Side: "SELL", PositionSide: "LONG", ClientID: "ct-stable-id", Price: 1821.359, Quantity: .019, ReduceOnly: true, Rules: rules})
	if err != nil {
		t.Fatal(err)
	}
	if r.OrderID != "123" || r.ExecutedQty != .005 {
		t.Fatalf("bad response %+v", r)
	}
	submit := calls[len(calls)-1]
	if submit["newClientOrderId"] != "ct-stable-id" || submit["quantity"] != "0.015" || submit["price"] != "1821.35" || submit["reduceOnly"] != "" || submit["positionSide"] != "LONG" {
		t.Fatalf("bad managed submit: %+v", submit)
	}
	if _, err = ex.GetManagedOrder("SNDKUSDT", "", "ct-stable-id"); err != nil {
		t.Fatal(err)
	}
	if calls[len(calls)-1]["origClientOrderId"] != "ct-stable-id" {
		t.Fatal("query ignored client identity")
	}
	if _, err = ex.CancelManagedOrder("SNDKUSDT", "", "ct-stable-id"); err != nil {
		t.Fatal(err)
	}
	deleteCall := calls[len(calls)-2]
	if deleteCall["method"] != "DELETE" || deleteCall["origClientOrderId"] != "ct-stable-id" {
		t.Fatalf("imprecise cancel %+v", deleteCall)
	}
	if _, err = ex.GetManagedOrder("SNDKUSDT", "", "missing"); !errors.Is(err, types.ErrManagedOrderNotFound) {
		t.Fatalf("not-found sentinel lost: %v", err)
	}
	for _, c := range calls {
		if c["path"] != "/fapi/v1/order" && c["path"] != "/fapi/v1/exchangeInfo" {
			t.Fatalf("managed order unexpectedly mutated another endpoint %+v", c)
		}
	}
}
