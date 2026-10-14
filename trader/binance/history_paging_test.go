package binance

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/adshao/go-binance/v2/futures"
	"net/http"
	"net/http/httptest"
	"nofx/store"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

func TestHistoryPagesInclusiveBoundaryAndRestart(t *testing.T) {
	st := historyStore(t)
	at := time.Now().Add(-time.Hour).UnixMilli()
	var userCalls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			t.Errorf("exchange mutation %s", r.Method)
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/fapi/v1/income":
			fmt.Fprintf(w, `[{"symbol":"BTCUSDT","time":%d,"income":"0.1"}]`, at)
		case "/fapi/v3/positionRisk", "/fapi/v2/positionRisk":
			fmt.Fprint(w, `[]`)
		case "/fapi/v1/userTrades":
			userCalls.Add(1)
			from, _ := strconv.Atoi(r.URL.Query().Get("fromId"))
			if from == 0 {
				from = 1
			}
			if from == 1 && r.URL.Query().Get("startTime") == "" {
				t.Error("first page lacks inclusive time")
			}
			rows := []map[string]interface{}{}
			for id := from; id <= 501 && id < from+500; id++ {
				rows = append(rows, map[string]interface{}{"id": id, "symbol": "BTCUSDT", "side": "BUY", "positionSide": "LONG", "price": "100", "qty": "1", "commission": "0", "realizedPnl": "0", "time": at})
			}
			json.NewEncoder(w).Encode(rows)
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
	if err := v.SyncOrdersFromBinance("t", "a", "binance", st); err != nil {
		t.Fatal(err)
	}
	if userCalls.Load() != 2 {
		t.Fatalf("did not paginate: %d", userCalls.Load())
	}
	// A new instance must use the durable inclusive cursor and numeric trade IDs.
	v = &FuturesTrader{client: client}
	if err := v.SyncOrdersFromBinance("t", "a", "binance", st); err != nil {
		t.Fatal(err)
	}
	var n int64
	st.GormDB().Model(&store.TraderFill{}).Count(&n)
	if n != 501 {
		t.Fatalf("lost/duplicated same-ms fills: %d", n)
	}
	status, err := st.Order().HistorySyncState("a")
	if err != nil || status.LastSuccess == nil || status.CursorMS != at {
		t.Fatalf("bad cursor %+v %v", status, err)
	}
}
func TestHistoryQueryFailureNotSuccessful(t *testing.T) {
	st := historyStore(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, `{"code":-1000,"msg":"offline"}`, 500) }))
	defer server.Close()
	client := futures.NewClient("test", "test")
	client.BaseURL = server.URL
	client.HTTPClient = server.Client()
	v := &FuturesTrader{client: client}
	if err := v.SyncOrdersFromBinance("t", "a", "binance", st); err == nil {
		t.Fatal("query failure reported success")
	}
	state, _ := st.Order().HistorySyncState("a")
	if state.LastSuccess != nil || state.LastError == "" {
		t.Fatalf("false successful state %+v", state)
	}
}

func TestHistorySyncCoalescesAndCancels(t *testing.T) {
	st := historyStore(t)
	started, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			close(started)
		}
		select {
		case <-release:
			fmt.Fprint(w, `[]`)
		case <-r.Context().Done():
		}
	}))
	defer server.Close()
	client := futures.NewClient("test", "test")
	client.BaseURL = server.URL
	client.HTTPClient = server.Client()
	v := &FuturesTrader{client: client}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- v.syncOrdersContext(ctx, "t", "coalesced-account", "binance", st) }()
	<-started
	follower, cancelFollower := context.WithCancel(context.Background())
	cancelFollower()
	if err := v.syncOrdersContext(follower, "t", "coalesced-account", "binance", st); !errors.Is(err, context.Canceled) {
		t.Fatalf("follower %v", err)
	}
	if calls.Load() != 1 {
		t.Fatal("concurrent request was not coalesced")
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancelled sync succeeded")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("stop did not cancel query")
	}
	close(release)
}
func TestHistoryCursorSigningAndMalformedNumbers(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		signature := q.Get("signature")
		q.Del("signature")
		mac := hmac.New(sha256.New, []byte("test-secret"))
		mac.Write([]byte(q.Encode()))
		if signature != hex.EncodeToString(mac.Sum(nil)) || q.Get("fromId") != "12" || q.Get("fromID") != "" || q.Get("startTime") != "" || r.Header.Get("X-MBX-APIKEY") != "test-key" {
			t.Error("invalid signed history request")
		}
		fmt.Fprint(w, `[]`)
	}))
	defer server.Close()
	client := futures.NewClient("test-key", "test-secret")
	client.BaseURL = server.URL
	client.HTTPClient = server.Client()
	v := &FuturesTrader{client: client}
	if _, err := v.accountTradesFromID(context.Background(), "BTCUSDT", 12, 500); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"fee", "pnl", "price", "qty"} {
		trade := &futures.AccountTrade{Price: "1", Quantity: "1", Commission: "0", RealizedPnl: "0"}
		switch field {
		case "fee":
			trade.Commission = "bad"
		case "pnl":
			trade.RealizedPnl = "NaN"
		case "price":
			trade.Price = "bad"
		case "qty":
			trade.Quantity = "Inf"
		}
		if _, err := decodeAccountTrades([]*futures.AccountTrade{trade}); err == nil {
			t.Fatalf("accepted malformed %s", field)
		}
	}
}
