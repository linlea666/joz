package binance

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/adshao/go-binance/v2/futures"
	"nofx/trader/types"
)

const cacheTestExchangeInfo = `{"symbols":[` +
	`{"symbol":"BTCUSDT","status":"TRADING","contractType":"PERPETUAL","quoteAsset":"USDT","filters":[{"filterType":"LOT_SIZE","minQty":"0.001","maxQty":"1000","stepSize":"0.001"},{"filterType":"PRICE_FILTER","minPrice":"0.10","maxPrice":"1000000","tickSize":"0.10"}]},` +
	`{"symbol":"ETHUSDT","status":"TRADING","contractType":"PERPETUAL","quoteAsset":"USDT","filters":[{"filterType":"LOT_SIZE","minQty":"0.01","maxQty":"1000","stepSize":"0.01"},{"filterType":"PRICE_FILTER","minPrice":"0.01","maxPrice":"100000","tickSize":"0.01"}]},` +
	`{"symbol":"RAYUSDT","status":"SETTLING","contractType":"PERPETUAL","quoteAsset":"USDT","filters":[]}]}`

type rulesServer struct {
	infoCalls atomic.Int64
	fail      atomic.Bool
	algoCalls atomic.Int64
	lastAlgo  atomic.Value
}

func newRulesTrader(t *testing.T, rs *rulesServer) *FuturesTrader {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/fapi/v1/exchangeInfo":
			rs.infoCalls.Add(1)
			if rs.fail.Load() {
				w.WriteHeader(500)
				_, _ = w.Write([]byte(`{"code":-1000,"msg":"offline"}`))
				return
			}
			_, _ = w.Write([]byte(cacheTestExchangeInfo))
		case "/fapi/v1/algoOrder":
			if r.Method == http.MethodPost {
				rs.algoCalls.Add(1)
				_ = r.ParseForm()
				_, _ = w.Write([]byte(`{"algoId":777,"clientAlgoId":"` + r.FormValue("clientAlgoId") + `","symbol":"BTCUSDT"}`))
				return
			}
			rs.lastAlgo.Store(r.URL.RawQuery)
			_, _ = w.Write([]byte(`{"algoId":777,"clientAlgoId":"ct-stop","symbol":"BTCUSDT","side":"SELL","positionSide":"LONG","orderType":"STOP_MARKET","algoStatus":"NEW","triggerPrice":"90000","closePosition":true}`))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected", 500)
		}
	}))
	t.Cleanup(server.Close)
	client := futures.NewClient("k", "s")
	client.BaseURL = server.URL
	client.HTTPClient = server.Client()
	return &FuturesTrader{client: client}
}

func TestMarketRulesCacheHitExpiryAndFailure(t *testing.T) {
	rs := &rulesServer{}
	ex := newRulesTrader(t, rs)

	// Failure is returned and never cached.
	rs.fail.Store(true)
	if _, err := ex.MarketRules("BTCUSDT"); err == nil {
		t.Fatal("failed exchangeInfo reported success")
	}
	rs.fail.Store(false)
	r1, err := ex.MarketRules("BTCUSDT")
	if err != nil || r1.PriceTick != 0.1 {
		t.Fatalf("rules after failure: %+v %v", r1, err)
	}
	if rs.infoCalls.Load() != 2 {
		t.Fatalf("failure polluted cache: calls=%d", rs.infoCalls.Load())
	}
	// Hits for the same and other symbols from the same download.
	r1.PriceTick = 99 // callers get copies
	for _, sym := range []string{"BTCUSDT", "ETHUSDT", "BTCUSDT"} {
		r, err := ex.MarketRules(sym)
		if err != nil || r.PriceTick == 99 {
			t.Fatalf("cached rules %s: %+v %v", sym, r, err)
		}
	}
	if p, err := ex.GetSymbolPrecision("ETHUSDT"); err != nil || p != 2 {
		t.Fatalf("legacy precision %d %v", p, err)
	}
	if p, err := ex.GetSymbolPricePrecision("BTCUSDT"); err != nil || p != 1 {
		t.Fatalf("legacy price precision %d %v", p, err)
	}
	if rs.infoCalls.Load() != 2 {
		t.Fatalf("cache hit refetched: calls=%d", rs.infoCalls.Load())
	}
	// Rejected (non-trading) and unlisted symbols are never cached.
	for i := 0; i < 2; i++ {
		if r, err := ex.MarketRules("RAYUSDT"); err == nil || r == nil || r.Status != "SETTLING" {
			t.Fatalf("rejected status lost: %+v %v", r, err)
		}
		if _, err := ex.MarketRules("NOPEUSDT"); !errors.Is(err, types.ErrInstrumentUnavailable) {
			t.Fatalf("unlisted: %v", err)
		}
	}
	if rs.infoCalls.Load() != 6 {
		t.Fatalf("rejected/unlisted symbols cached: calls=%d", rs.infoCalls.Load())
	}
	// Expiry refreshes; a failed refresh does not serve stale rules.
	ex.symbolRulesTTL = time.Millisecond
	time.Sleep(5 * time.Millisecond)
	if _, err := ex.MarketRules("BTCUSDT"); err != nil {
		t.Fatal(err)
	}
	if rs.infoCalls.Load() != 7 {
		t.Fatalf("expired entry not refreshed: calls=%d", rs.infoCalls.Load())
	}
	time.Sleep(5 * time.Millisecond)
	rs.fail.Store(true)
	if _, err := ex.MarketRules("BTCUSDT"); err == nil {
		t.Fatal("expired rules served after failed refresh")
	}
}

func TestMarketRulesCacheConcurrentMissesShareOneDownload(t *testing.T) {
	rs := &rulesServer{}
	ex := newRulesTrader(t, rs)
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sym := "BTCUSDT"
			if i%2 == 1 {
				sym = "ETHUSDT"
			}
			if _, err := ex.MarketRules(sym); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	if rs.infoCalls.Load() != 1 {
		t.Fatalf("concurrent misses downloaded %d times", rs.infoCalls.Load())
	}
}

func TestManagedStopLossUsesCachedRulesAndRemembersAlgoID(t *testing.T) {
	rs := &rulesServer{}
	ex := newRulesTrader(t, rs)
	for i := 0; i < 3; i++ {
		if err := ex.SetManagedStopLoss("BTCUSDT", "LONG", 1, 90000.04, "ct-stop"); err != nil {
			t.Fatal(err)
		}
	}
	if rs.infoCalls.Load() != 1 || rs.algoCalls.Load() != 3 {
		t.Fatalf("stop placement refetched rules: info=%d algo=%d", rs.infoCalls.Load(), rs.algoCalls.Load())
	}
	res, err := ex.GetStopOrder("BTCUSDT", "", "ct-stop")
	if err != nil || res.OrderID != "777" {
		t.Fatalf("stop confirmation %+v %v", res, err)
	}
	q, _ := rs.lastAlgo.Load().(string)
	if q == "" || !containsParam(q, "algoId=777") || containsParam(q, "clientAlgoId=") {
		t.Fatalf("confirmation did not prefer the remembered algoId: %q", q)
	}
	// Another client ID still uses the exact clientAlgoId identity.
	if _, err := ex.GetStopOrder("BTCUSDT", "", "other"); err == nil {
		t.Fatal("identity mismatch accepted")
	}
	q, _ = rs.lastAlgo.Load().(string)
	if !containsParam(q, "clientAlgoId=other") {
		t.Fatalf("unknown client id not queried by clientAlgoId: %q", q)
	}
}

func containsParam(rawQuery, kv string) bool {
	for _, part := range strings.Split(rawQuery, "&") {
		if strings.HasPrefix(part, kv) {
			return true
		}
	}
	return false
}
