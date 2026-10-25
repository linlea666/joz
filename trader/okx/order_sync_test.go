package okx

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"nofx/store"
)

type fillsTransport struct {
	fills      string
	instFail   bool
	instCalls  int
	fillsCalls int
}

func (rt *fillsTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	body := `{"code":"0","data":[]}`
	switch r.URL.Path {
	case okxInstrumentsPath:
		rt.instCalls++
		body = `{"code":"0","data":[{"instId":"ETH-USDT-SWAP","ctVal":"0.1","ctMult":"1","lotSz":"1","minSz":"1","tickSz":"0.01","ctType":"linear"}]}`
		if rt.instFail {
			body = `{"code":"50011","msg":"rate limited"}`
		}
	case "/api/v5/trade/fills-history":
		rt.fillsCalls++
		body = `{"code":"0","data":[` + rt.fills + `]}`
	}
	return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
}

func okxFill(id, side, posSide, sz, pnl string, ts int64) string {
	return fmt.Sprintf(`{"instId":"ETH-USDT-SWAP","tradeId":%q,"ordId":"o%s","side":%q,"posSide":%q,"fillPx":"2000","fillSz":%q,"fillPnl":%q,"fee":"-0.1","feeCcy":"USDT","ts":"%d","execType":"T"}`, id, id, side, posSide, sz, pnl, ts)
}

func okxHistoryStore(t *testing.T) *store.Store {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.NewFromGorm(db)
	if err != nil {
		t.Fatal(err)
	}
	if err = st.Order().InitTables(); err != nil {
		t.Fatal(err)
	}
	if err = db.AutoMigrate(&store.TraderPosition{}); err != nil {
		t.Fatal(err)
	}
	return st
}

func okxCount(t *testing.T, st *store.Store, model interface{}) int64 {
	t.Helper()
	var n int64
	if err := st.GormDB().Model(model).Count(&n).Error; err != nil {
		t.Fatal(err)
	}
	return n
}

func TestOKXHistoryFailsBatchWithoutContractValue(t *testing.T) {
	st := okxHistoryStore(t)
	now := time.Now().UnixMilli()
	rt := &fillsTransport{fills: okxFill("1", "buy", "long", "10", "0", now), instFail: true}
	ex := newTestOKXTrader(&recordingTransport{}, true)
	ex.httpClient.Transport = rt
	if err := ex.SyncOrdersFromOKX("t", "a", "okx", st); err == nil {
		t.Fatal("contracts recorded as coins without contract value")
	}
	if okxCount(t, st, &store.TraderOrder{}) != 0 || okxCount(t, st, &store.TraderFill{}) != 0 {
		t.Fatal("partial batch persisted")
	}
	rt.instFail = false
	if err := ex.SyncOrdersFromOKX("t", "a", "okx", st); err != nil {
		t.Fatal(err)
	}
	var fill store.TraderFill
	st.GormDB().First(&fill)
	if fill.Quantity != 1 { // 10 contracts * ctVal 0.1
		t.Fatalf("base quantity %v", fill.Quantity)
	}
}

func TestOKXHistoryTransactionRollsBackOnPositionFailure(t *testing.T) {
	st := okxHistoryStore(t)
	now := time.Now().UnixMilli()
	rt := &fillsTransport{fills: okxFill("1", "buy", "long", "10", "0", now)}
	ex := newTestOKXTrader(&recordingTransport{}, true)
	ex.httpClient.Transport = rt
	st.GormDB().Callback().Create().Before("gorm:create").Register("fail_okx_position", func(tx *gorm.DB) {
		if tx.Statement.Table == "trader_positions" {
			tx.AddError(fmt.Errorf("position disk failure"))
		}
	})
	if err := ex.SyncOrdersFromOKX("t", "a", "okx", st); err == nil {
		t.Fatal("failed position write reported success")
	}
	if okxCount(t, st, &store.TraderOrder{}) != 0 || okxCount(t, st, &store.TraderFill{}) != 0 {
		t.Fatal("order/fill committed without position (dedup would hide it)")
	}
	st.GormDB().Callback().Create().Remove("fail_okx_position")
	for i := 0; i < 2; i++ {
		if err := ex.SyncOrdersFromOKX("t", "a", "okx", st); err != nil {
			t.Fatal(err)
		}
	}
	if okxCount(t, st, &store.TraderOrder{}) != 1 || okxCount(t, st, &store.TraderFill{}) != 1 || okxCount(t, st, &store.TraderPosition{}) != 1 {
		t.Fatal("retry did not converge exactly once")
	}
}

func TestOKXHistoryNetModeClassifiesCloses(t *testing.T) {
	st := okxHistoryStore(t)
	now := time.Now().UnixMilli()
	rt := &fillsTransport{fills: strings.Join([]string{
		okxFill("1", "buy", "net", "10", "0", now),    // open long 1 ETH
		okxFill("2", "sell", "net", "4", "0", now+1),  // break-even partial close
		okxFill("3", "sell", "net", "26", "5", now+2), // close 0.6 and reverse into 2 short
		okxFill("4", "sell", "net", "1", "3", now+3),  // pnl while only short is open: review
	}, ",")}
	ex := newTestOKXTrader(&recordingTransport{}, true)
	ex.httpClient.Transport = rt
	if err := ex.SyncOrdersFromOKX("t", "a", "okx", st); err != nil {
		t.Fatal(err)
	}
	var orders []store.TraderOrder
	st.GormDB().Order("created_at").Find(&orders)
	got := []string{}
	for _, o := range orders {
		got = append(got, o.OrderAction)
	}
	if strings.Join(got, ",") != "open_long,close_long,close_long,review" {
		t.Fatalf("net-mode actions %v", got)
	}
	var ps []store.TraderPosition
	st.GormDB().Order("id").Find(&ps)
	if len(ps) != 2 || ps[0].Side != "LONG" || ps[0].Status != "CLOSED" || ps[1].Side != "SHORT" || ps[1].Status != "OPEN" || ps[1].Quantity < 2-1e-9 || ps[1].Quantity > 2+1e-9 {
		t.Fatalf("net-mode ledger %+v", ps)
	}
	if rt.instCalls != 1 {
		t.Fatalf("instrument fetched %d times for one symbol", rt.instCalls)
	}
}
