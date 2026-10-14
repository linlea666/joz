package binance

import (
	"fmt"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"nofx/store"
	"nofx/trader/types"
	"testing"
	"time"
)

func historyStore(t *testing.T) *store.Store {
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
	if err = db.AutoMigrate(&store.TraderPosition{}, &store.TradeHistorySyncState{}); err != nil {
		t.Fatal(err)
	}
	return st
}
func TestAtomicHistoryIdentityAndZeroPnLClose(t *testing.T) {
	st := historyStore(t)
	v := &FuturesTrader{}
	now := time.Now()
	fill := types.TradeRecord{TradeID: "1", Symbol: "BTCUSDT", Side: "BUY", PositionSide: "LONG", Price: 100, Quantity: 2, Time: now}
	for _, symbol := range []string{"BTCUSDT", "ETHUSDT"} {
		fill.Symbol = symbol
		if err := v.persistHistoryTrade(st, "trader", "account", "binance", fill); err != nil {
			t.Fatal(err)
		}
		if err := v.persistHistoryTrade(st, "trader", "account", "binance", fill); err != nil {
			t.Fatal(err)
		}
	}
	var count int64
	st.GormDB().Model(&store.TraderFill{}).Count(&count)
	if count != 2 {
		t.Fatalf("identity collision or duplicate: %d", count)
	}
	fill.Symbol = "BTCUSDT"
	fill.TradeID = "2"
	fill.Side = "SELL"
	if err := v.persistHistoryTrade(st, "trader", "account", "binance", fill); err != nil {
		t.Fatal(err)
	}
	var p store.TraderPosition
	if err := st.GormDB().Where("symbol = ?", "BTCUSDT").First(&p).Error; err != nil {
		t.Fatal(err)
	}
	if p.Status != "CLOSED" {
		t.Fatalf("zero-PnL close misclassified %+v", p)
	}
}
func TestHistoryTransactionRollback(t *testing.T) {
	st := historyStore(t)
	v := &FuturesTrader{}
	st.GormDB().Callback().Create().Before("gorm:create").Register("fail_position", func(tx *gorm.DB) {
		if tx.Statement.Table == "trader_positions" {
			tx.AddError(fmt.Errorf("position disk failure"))
		}
	})
	f := types.TradeRecord{TradeID: "9", Symbol: "BTCUSDT", Side: "BUY", PositionSide: "LONG", Price: 100, Quantity: 2, Time: time.Now()}
	if err := v.persistHistoryTrade(st, "trader", "account", "binance", f); err == nil {
		t.Fatal("failed transaction reported success")
	}
	for _, model := range []interface{}{&store.TraderFill{}, &store.TraderOrder{}} {
		var n int64
		st.GormDB().Model(model).Count(&n)
		if n != 0 {
			t.Fatal("partial transaction committed")
		}
	}
	st.GormDB().Callback().Create().Remove("fail_position")
	if err := v.persistHistoryTrade(st, "trader", "account", "binance", f); err != nil {
		t.Fatal(err)
	}
}
func TestHedgeSideActions(t *testing.T) {
	v := &FuturesTrader{}
	for _, c := range []struct{ side, pos, want string }{{"SELL", "LONG", "close_long"}, {"BUY", "SHORT", "close_short"}, {"BUY", "LONG", "open_long"}, {"SELL", "SHORT", "open_short"}} {
		if got := v.determineOrderAction(c.side, c.pos, 0); got != c.want {
			t.Fatal(got)
		}
	}
}
