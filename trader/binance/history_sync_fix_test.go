package binance

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/adshao/go-binance/v2/futures"
	"nofx/store"
	"nofx/trader/types"
)

func TestHistorySignatureIsLastAndCoversRawQuery(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw := r.URL.RawQuery
		i := strings.LastIndex(raw, "&signature=")
		if i < 0 || strings.Contains(raw[i+1:], "&") {
			t.Errorf("signature not last: %q", raw)
		} else {
			mac := hmac.New(sha256.New, []byte("test-secret"))
			mac.Write([]byte(raw[:i]))
			if raw[i+len("&signature="):] != hex.EncodeToString(mac.Sum(nil)) {
				t.Errorf("signature does not cover the transmitted query: %q", raw)
			}
		}
		fmt.Fprint(w, `[{"id":5,"orderId":900,"symbol":"BTCUSDT","side":"BUY","positionSide":"LONG","price":"1","qty":"1","commission":"0.01","commissionAsset":"bnb","realizedPnl":"0","time":1}]`)
	}))
	defer server.Close()
	client := futures.NewClient("test-key", "test-secret")
	client.BaseURL = server.URL
	client.HTTPClient = server.Client()
	v := &FuturesTrader{client: client}
	trades, err := v.getTradesFromID(context.Background(), "BTCUSDT", 3, 500)
	if err != nil {
		t.Fatal(err)
	}
	if len(trades) != 1 || trades[0].OrderID != "900" || trades[0].TradeID != "5" || trades[0].FeeAsset != "BNB" {
		t.Fatalf("decoded identity/fee asset lost: %+v", trades)
	}
}

func historyFills(t *testing.T, st *store.Store) []store.TraderFill {
	t.Helper()
	var fills []store.TraderFill
	if err := st.GormDB().Order("id").Find(&fills).Error; err != nil {
		t.Fatal(err)
	}
	return fills
}

func historyPositions(t *testing.T, st *store.Store) []store.TraderPosition {
	t.Helper()
	var ps []store.TraderPosition
	if err := st.GormDB().Order("id").Find(&ps).Error; err != nil {
		t.Fatal(err)
	}
	return ps
}

func TestHistoryNonUSDTCommissionKeptRawNotCountedAsUSDT(t *testing.T) {
	st := historyStore(t)
	v := &FuturesTrader{}
	now := time.Now()
	note, err := v.persistHistoryTradeNoted(st, "t", "a", "binance", types.TradeRecord{TradeID: "1", OrderID: "10", Symbol: "BTCUSDT", Side: "BUY", PositionSide: "LONG", Price: 100, Quantity: 1, Fee: 0.002, FeeAsset: "BNB", Time: now})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(note, "BNB") {
		t.Fatalf("non-USDT fee not noted: %q", note)
	}
	fills := historyFills(t, st)
	if len(fills) != 1 || fills[0].Commission != 0.002 || fills[0].CommissionAsset != "BNB" {
		t.Fatalf("fill lost raw fee/asset: %+v", fills)
	}
	var order store.TraderOrder
	st.GormDB().First(&order)
	if order.Commission != 0 || order.CommissionAsset != "USDT" {
		t.Fatalf("BNB fee summed as USDT on order: %+v", order)
	}
	if ps := historyPositions(t, st); len(ps) != 1 || ps[0].Fee != 0 {
		t.Fatalf("BNB fee summed as USDT on position: %+v", ps)
	}
	// USDT fee still counts everywhere, without a note.
	note, err = v.persistHistoryTradeNoted(st, "t", "a", "binance", types.TradeRecord{TradeID: "2", OrderID: "11", Symbol: "BTCUSDT", Side: "BUY", PositionSide: "LONG", Price: 100, Quantity: 1, Fee: 0.04, FeeAsset: "USDT", Time: now})
	if err != nil || note != "" {
		t.Fatalf("usdt fee: %q %v", note, err)
	}
	if ps := historyPositions(t, st); ps[0].Fee != 0.04 {
		t.Fatalf("USDT fee not counted: %+v", ps)
	}
}

func TestHistoryOrderRowUsesRealOrderIDAndAggregatesFills(t *testing.T) {
	st := historyStore(t)
	v := &FuturesTrader{}
	now := time.Now()
	// A legacy fill/order pair keyed by trade ID (pre-fix data) for the same symbol.
	legacy := store.TraderOrder{TraderID: "t", ExchangeID: "a", ExchangeType: "binance", ExchangeOrderID: "7", Symbol: "BTCUSDT", Side: "BUY", PositionSide: "LONG", Type: "MARKET", OrderAction: "open_long", Quantity: 1, Price: 100, Status: "FILLED", FilledQuantity: 1, AvgFillPrice: 100, CreatedAt: now.UnixMilli()}
	if err := st.GormDB().Create(&legacy).Error; err != nil {
		t.Fatal(err)
	}
	if err := st.GormDB().Create(&store.TraderFill{TraderID: "t", ExchangeID: "a", OrderID: legacy.ID, ExchangeOrderID: "7", ExchangeTradeID: "7", Symbol: "BTCUSDT", Side: "BUY", Price: 100, Quantity: 1, CommissionAsset: "USDT"}).Error; err != nil {
		t.Fatal(err)
	}
	for i, tr := range []types.TradeRecord{
		{TradeID: "7", OrderID: "500", Symbol: "BTCUSDT", Side: "BUY", PositionSide: "LONG", Price: 100, Quantity: 1, Time: now}, // already stored (legacy)
		{TradeID: "8", OrderID: "500", Symbol: "BTCUSDT", Side: "BUY", PositionSide: "LONG", Price: 100, Quantity: 1, Fee: 0.1, Time: now},
		{TradeID: "9", OrderID: "500", Symbol: "BTCUSDT", Side: "BUY", PositionSide: "LONG", Price: 110, Quantity: 1, Fee: 0.1, Time: now.Add(time.Second)},
		{TradeID: "9", OrderID: "500", Symbol: "BTCUSDT", Side: "BUY", PositionSide: "LONG", Price: 110, Quantity: 1, Fee: 0.1, Time: now.Add(time.Second)}, // duplicate
	} {
		if err := v.persistHistoryTrade(st, "t", "a", "binance", tr); err != nil {
			t.Fatalf("fill %d: %v", i, err)
		}
	}
	var orders []store.TraderOrder
	st.GormDB().Order("id").Find(&orders)
	if len(orders) != 2 {
		t.Fatalf("orders %+v", orders)
	}
	agg := orders[1]
	if agg.ExchangeOrderID != "500" || agg.FilledQuantity != 2 || agg.AvgFillPrice != 105 || agg.Commission < 0.2-1e-12 || agg.Commission > 0.2+1e-12 {
		t.Fatalf("order not keyed/aggregated by real orderId: %+v", agg)
	}
	fills := historyFills(t, st)
	if len(fills) != 3 || fills[1].ExchangeOrderID != "500" || fills[1].ExchangeTradeID != "8" || fills[2].OrderID != agg.ID {
		t.Fatalf("fills %+v", fills)
	}
	if ps := historyPositions(t, st); len(ps) != 1 || ps[0].Quantity != 2 {
		t.Fatalf("positions %+v", ps)
	}
}

func TestHistoryOneWayBothFillsAreClassifiedNotOpened(t *testing.T) {
	st := historyStore(t)
	v := &FuturesTrader{}
	now := time.Now()
	step := 0
	put := func(side string, qty, pnl float64) string {
		t.Helper()
		step++
		note, err := v.persistHistoryTradeNoted(st, "t", "a", "binance", types.TradeRecord{TradeID: strconv.Itoa(step), OrderID: strconv.Itoa(1000 + step), Symbol: "ETHUSDT", Side: side, PositionSide: "BOTH", Price: 100, Quantity: qty, RealizedPnL: pnl, Time: now.Add(time.Duration(step) * time.Second)})
		if err != nil {
			t.Fatal(err)
		}
		return note
	}
	lastAction := func() string {
		var o store.TraderOrder
		st.GormDB().Order("id DESC").First(&o)
		return o.OrderAction
	}
	put("BUY", 1, 0) // flat -> open long
	if a := lastAction(); a != "open_long" {
		t.Fatalf("open: %s", a)
	}
	put("SELL", 0.4, 0) // break-even partial close against the long
	if a := lastAction(); a != "close_long" {
		t.Fatalf("zero-PnL one-way close recorded as %s", a)
	}
	put("SELL", 2.6, 3) // closes 0.6 and reverses into a 2.0 short
	if a := lastAction(); a != "close_long" {
		t.Fatalf("reversal: %s", a)
	}
	ps := historyPositions(t, st)
	if len(ps) != 2 || ps[0].Side != "LONG" || ps[0].Status != "CLOSED" || ps[1].Side != "SHORT" || ps[1].Status != "OPEN" || ps[1].Quantity < 2-1e-9 || ps[1].Quantity > 2+1e-9 {
		t.Fatalf("reversal ledger %+v", ps)
	}
	// PnL while only the same direction is open cannot be classified.
	if note := put("SELL", 1, 5); !strings.Contains(note, "review") {
		t.Fatalf("contradiction not flagged: %q", note)
	}
	if a := lastAction(); a != historyOrderActionReview {
		t.Fatalf("contradiction recorded as %s", a)
	}
	if ps := historyPositions(t, st); ps[1].Quantity < 2-1e-9 || ps[1].Quantity > 2+1e-9 {
		t.Fatalf("review fill moved positions %+v", ps)
	}
	// Realized PnL proves a close even for a position the ledger never saw.
	st2 := historyStore(t)
	if err := v.persistHistoryTrade(st2, "t", "a", "binance", types.TradeRecord{TradeID: "1", OrderID: "2", Symbol: "ETHUSDT", Side: "BUY", PositionSide: "BOTH", Price: 100, Quantity: 1, RealizedPnL: -2, Time: now}); err != nil {
		t.Fatal(err)
	}
	var o store.TraderOrder
	st2.GormDB().First(&o)
	if o.OrderAction != "close_short" {
		t.Fatalf("untracked one-way close recorded as %s", o.OrderAction)
	}
	if ps := historyPositions(t, st2); len(ps) != 0 {
		t.Fatalf("close of unseen position opened %+v", ps)
	}
	if got := v.determineOrderAction("BUY", "BOTH", 0); got != "" {
		t.Fatalf("one-way fallback guessed %q", got)
	}
}

func TestHistoryDiscoveryLooksBehindCrossSymbolCursor(t *testing.T) {
	st := historyStore(t)
	cursor := time.Now().Add(-time.Hour).UnixMilli()
	late := cursor - 5*60*1000 // ETH fill whose income appeared after BTC advanced the cursor
	if err := st.GormDB().Create(&store.TradeHistorySyncState{ExchangeID: "a", CursorMS: cursor}).Error; err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		q := r.URL.Query()
		switch r.URL.Path {
		case "/fapi/v1/income":
			start, _ := strconv.ParseInt(q.Get("startTime"), 10, 64)
			if start <= late {
				fmt.Fprintf(w, `[{"symbol":"ETHUSDT","time":%d,"income":"-0.1","incomeType":%q}]`, late, q.Get("incomeType"))
				return
			}
			fmt.Fprint(w, `[]`)
		case "/fapi/v3/positionRisk", "/fapi/v2/positionRisk":
			fmt.Fprint(w, `[]`)
		case "/fapi/v1/userTrades":
			start, _ := strconv.ParseInt(q.Get("startTime"), 10, 64)
			if q.Get("symbol") != "ETHUSDT" || start > late {
				fmt.Fprint(w, `[]`)
				return
			}
			json.NewEncoder(w).Encode([]map[string]interface{}{{"id": 42, "orderId": 4200, "symbol": "ETHUSDT", "side": "BUY", "positionSide": "LONG", "price": "2000", "qty": "1", "commission": "0.1", "commissionAsset": "USDT", "realizedPnl": "0", "time": late}})
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
			http.Error(w, "unexpected", 500)
		}
	}))
	defer server.Close()
	client := futures.NewClient("test", "test")
	client.BaseURL = server.URL
	client.HTTPClient = server.Client()
	v := &FuturesTrader{client: client}
	for i := 0; i < 2; i++ { // second run re-reads the window and deduplicates
		if err := v.SyncOrdersFromBinance("t", "a", "binance", st); err != nil {
			t.Fatal(err)
		}
	}
	fills := historyFills(t, st)
	if len(fills) != 1 || fills[0].ExchangeTradeID != "42" || fills[0].ExchangeOrderID != "4200" {
		t.Fatalf("fast in/out symbol missed or duplicated: %+v", fills)
	}
	state, _ := st.Order().HistorySyncState("a")
	if state.CursorMS != cursor || state.LastSuccess == nil || state.LastError != "" {
		t.Fatalf("cursor moved backwards or bad state %+v", state)
	}
}
