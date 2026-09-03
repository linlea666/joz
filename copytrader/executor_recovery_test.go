package copytrader

// Regression tests for the audit fixes:
//   - cancel failure / fill race never marks a context terminal
//   - SL placement + emergency close double-failure keeps the trade OPEN
//   - the reconciler's SL guard re-places a missing stop loss
//   - hasStopLossOrder distinguishes stop orders from take profits

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"nofx/store"
	"nofx/trader/types"
)

func init() {
	retrySleep = func(time.Duration) {} // no real backoff waits in tests
}

// mockExchange implements types.Trader + types.GridTrader with overridable hooks.
type mockExchange struct {
	positions      []map[string]interface{}
	openOrders     []types.OpenOrder
	openOrdersErr  error
	orderStatus    map[string]interface{}
	orderStatusErr error
	cancelOrderErr error
	setStopLossErr error
	closeLongErr   error
	setStopLossLog []float64 // stop prices passed to SetStopLoss
	cancelOrderLog []string
	closeCalled    int
}

func (m *mockExchange) GetBalance() (map[string]interface{}, error) { return nil, nil }
func (m *mockExchange) GetPositions() ([]map[string]interface{}, error) {
	return m.positions, nil
}
func (m *mockExchange) OpenLong(string, float64, int) (map[string]interface{}, error) {
	return map[string]interface{}{"orderId": "o1"}, nil
}
func (m *mockExchange) OpenShort(string, float64, int) (map[string]interface{}, error) {
	return map[string]interface{}{"orderId": "o1"}, nil
}
func (m *mockExchange) CloseLong(string, float64) (map[string]interface{}, error) {
	m.closeCalled++
	return nil, m.closeLongErr
}
func (m *mockExchange) CloseShort(string, float64) (map[string]interface{}, error) {
	m.closeCalled++
	return nil, m.closeLongErr
}
func (m *mockExchange) SetLeverage(string, int) error          { return nil }
func (m *mockExchange) SetMarginMode(string, bool) error       { return nil }
func (m *mockExchange) GetMarketPrice(string) (float64, error) { return 100, nil }
func (m *mockExchange) SetStopLoss(_ string, _ string, _ float64, stopPrice float64) error {
	if m.setStopLossErr != nil {
		return m.setStopLossErr
	}
	m.setStopLossLog = append(m.setStopLossLog, stopPrice)
	return nil
}
func (m *mockExchange) SetTakeProfit(string, string, float64, float64) error { return nil }
func (m *mockExchange) CancelStopLossOrders(string) error                    { return nil }
func (m *mockExchange) CancelTakeProfitOrders(string) error                  { return nil }
func (m *mockExchange) CancelAllOrders(string) error                         { return nil }
func (m *mockExchange) CancelStopOrders(string) error                        { return nil }
func (m *mockExchange) FormatQuantity(_ string, q float64) (string, error) {
	return fmt.Sprintf("%.3f", q), nil // mimic a 3-decimal step exchange
}
func (m *mockExchange) GetOrderStatus(string, string) (map[string]interface{}, error) {
	return m.orderStatus, m.orderStatusErr
}
func (m *mockExchange) GetClosedPnL(time.Time, int) ([]types.ClosedPnLRecord, error) {
	return nil, nil
}
func (m *mockExchange) GetOpenOrders(string) ([]types.OpenOrder, error) {
	return m.openOrders, m.openOrdersErr
}
func (m *mockExchange) PlaceLimitOrder(*types.LimitOrderRequest) (*types.LimitOrderResult, error) {
	return &types.LimitOrderResult{OrderID: "lo1"}, nil
}
func (m *mockExchange) CancelOrder(_ string, orderID string) error {
	m.cancelOrderLog = append(m.cancelOrderLog, orderID)
	return m.cancelOrderErr
}
func (m *mockExchange) GetOrderBook(string, int) ([][]float64, [][]float64, error) {
	return nil, nil, nil
}

// newTestStore builds an in-memory store with the copy-trading tables.
func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	gdb, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := gdb.AutoMigrate(
		&store.CopyTradeContext{}, &store.CopyTradeEvent{},
		&store.CopyTradeSignal{}, &store.CopyTradeAIRun{},
	); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	st, err := store.NewFromGorm(gdb)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	return st
}

func newTestContext(t *testing.T, st *store.Store, state TradeState) *store.CopyTradeContext {
	t.Helper()
	ctx := &store.CopyTradeContext{
		ID:            uuid.NewString(),
		TraderID:      "trader-1",
		ChannelID:     "chan-1",
		RootMessageID: "msg-1",
		Symbol:        "BTCUSDT",
		Direction:     "LONG",
		State:         string(state),
		Quantity:      0.5,
		StopLossPrice: 95,
		EntryOrderID:  "entry-1",
	}
	if err := st.CopyTrade().CreateContext(ctx); err != nil {
		t.Fatalf("create context: %v", err)
	}
	return ctx
}

func TestExecuteCancelFailureKeepsEntryPending(t *testing.T) {
	st := newTestStore(t)
	mock := &mockExchange{
		cancelOrderErr: errors.New("exchange 502"),
		orderStatus:    map[string]interface{}{"status": "NEW"},
	}
	x := NewExecutor("trader-1", mock, st, NewEventLogger(st, "trader-1", "chan-1"))
	ctx := newTestContext(t, st, StateEntryPending)

	_, err := x.ExecuteCancel("t", "s", ctx)
	if err == nil {
		t.Fatal("cancel failure must surface an error (retry later), got nil")
	}
	stored, _ := st.CopyTrade().GetContext(ctx.ID)
	if stored.State != string(StateEntryPending) {
		t.Fatalf("state = %s, want ENTRY_PENDING (cancel unconfirmed)", stored.State)
	}
}

func TestExecuteCancelFilledRaceKeepsTradeActive(t *testing.T) {
	st := newTestStore(t)
	mock := &mockExchange{
		orderStatus: map[string]interface{}{"status": "FILLED"},
	}
	x := NewExecutor("trader-1", mock, st, NewEventLogger(st, "trader-1", "chan-1"))
	ctx := newTestContext(t, st, StateEntryPending)

	_, err := x.ExecuteCancel("t", "s", ctx)
	if err == nil {
		t.Fatal("filled-before-cancel must surface an error, got nil")
	}
	stored, _ := st.CopyTrade().GetContext(ctx.ID)
	if stored.State != string(StateEntryPending) {
		t.Fatalf("state = %s, want ENTRY_PENDING (reconciler converts the fill)", stored.State)
	}
	if len(mock.cancelOrderLog) != 0 {
		t.Fatal("must not attempt to cancel a filled order")
	}
}

func TestExecuteCancelConfirmedMarksCancelled(t *testing.T) {
	st := newTestStore(t)
	mock := &mockExchange{orderStatus: map[string]interface{}{"status": "NEW"}}
	x := NewExecutor("trader-1", mock, st, NewEventLogger(st, "trader-1", "chan-1"))
	ctx := newTestContext(t, st, StateEntryPending)

	if _, err := x.ExecuteCancel("t", "s", ctx); err != nil {
		t.Fatalf("successful cancel errored: %v", err)
	}
	stored, _ := st.CopyTrade().GetContext(ctx.ID)
	if stored.State != string(StateCancelled) {
		t.Fatalf("state = %s, want CANCELLED", stored.State)
	}
}

func TestProtectionDoubleFailureKeepsOpen(t *testing.T) {
	st := newTestStore(t)
	mock := &mockExchange{
		setStopLossErr: errors.New("SL rejected"),
		closeLongErr:   errors.New("close rejected"),
	}
	x := NewExecutor("trader-1", mock, st, NewEventLogger(st, "trader-1", "chan-1"))
	ctx := newTestContext(t, st, StateOpen)

	plan := &OpenPlan{Symbol: "BTCUSDT", Direction: DirectionLong, StopLoss: 95}
	err := x.placeProtections("t", "s", plan, ctx, 0.5, 100)
	if err == nil {
		t.Fatal("SL+close double failure must return an error")
	}
	stored, _ := st.CopyTrade().GetContext(ctx.ID)
	if stored.State != string(StateOpen) {
		t.Fatalf("state = %s, want OPEN (never terminal while a naked position may live)", stored.State)
	}
	if stored.LastError == "" {
		t.Fatal("last_error must record the unprotected condition")
	}
}

func TestProtectionFailureWithSuccessfulCloseMarksClosed(t *testing.T) {
	st := newTestStore(t)
	mock := &mockExchange{setStopLossErr: errors.New("SL rejected")}
	x := NewExecutor("trader-1", mock, st, NewEventLogger(st, "trader-1", "chan-1"))
	ctx := newTestContext(t, st, StateOpen)

	plan := &OpenPlan{Symbol: "BTCUSDT", Direction: DirectionLong, StopLoss: 95}
	if err := x.placeProtections("t", "s", plan, ctx, 0.5, 100); err == nil {
		t.Fatal("SL failure must return an error")
	}
	stored, _ := st.CopyTrade().GetContext(ctx.ID)
	if stored.State != string(StateClosed) {
		t.Fatalf("state = %s, want CLOSED (emergency close confirmed)", stored.State)
	}
}

func TestReconcileSLGuardReplacesMissingStop(t *testing.T) {
	st := newTestStore(t)
	mock := &mockExchange{
		positions: []map[string]interface{}{{
			"symbol": "BTCUSDT", "side": "long", "positionAmt": 0.5, "entryPrice": 100.0,
		}},
		openOrders: nil, // no SL on the exchange
	}
	events := NewEventLogger(st, "trader-1", "chan-1")
	e := &Engine{
		traderID: "trader-1",
		cfg:      &CopyTradingConfig{},
		st:       st,
		exec:     NewExecutor("trader-1", mock, st, events),
		events:   events,
	}
	ctx := newTestContext(t, st, StateOpen)

	e.reconcileOpenTrade(ctx)
	if len(mock.setStopLossLog) != 1 || mock.setStopLossLog[0] != 95 {
		t.Fatalf("SL guard must re-place stop @95, got %v", mock.setStopLossLog)
	}
}

func TestReconcileSLGuardSkipsWhenStopExists(t *testing.T) {
	st := newTestStore(t)
	mock := &mockExchange{
		positions: []map[string]interface{}{{
			"symbol": "BTCUSDT", "side": "long", "positionAmt": 0.5, "entryPrice": 100.0,
		}},
		openOrders: []types.OpenOrder{{Type: "STOP_MARKET", StopPrice: 95}},
	}
	events := NewEventLogger(st, "trader-1", "chan-1")
	e := &Engine{
		traderID: "trader-1",
		cfg:      &CopyTradingConfig{},
		st:       st,
		exec:     NewExecutor("trader-1", mock, st, events),
		events:   events,
	}
	ctx := newTestContext(t, st, StateOpen)

	e.reconcileOpenTrade(ctx)
	if len(mock.setStopLossLog) != 0 {
		t.Fatalf("SL guard must not touch an existing stop, got %v", mock.setStopLossLog)
	}
}

func TestReconcileSLGuardIgnoresUnknownAnswer(t *testing.T) {
	st := newTestStore(t)
	mock := &mockExchange{
		positions: []map[string]interface{}{{
			"symbol": "BTCUSDT", "side": "long", "positionAmt": 0.5, "entryPrice": 100.0,
		}},
		openOrdersErr: errors.New("exchange down"),
	}
	events := NewEventLogger(st, "trader-1", "chan-1")
	e := &Engine{
		traderID: "trader-1",
		cfg:      &CopyTradingConfig{},
		st:       st,
		exec:     NewExecutor("trader-1", mock, st, events),
		events:   events,
	}
	ctx := newTestContext(t, st, StateOpen)

	e.reconcileOpenTrade(ctx)
	if len(mock.setStopLossLog) != 0 {
		t.Fatal("query failure means UNKNOWN, must not blindly re-place SL")
	}
}

func TestHasStopLossOrderDetection(t *testing.T) {
	st := newTestStore(t)
	mock := &mockExchange{openOrders: []types.OpenOrder{
		{Type: "TAKE_PROFIT_MARKET", StopPrice: 110},
	}}
	x := NewExecutor("trader-1", mock, st, NewEventLogger(st, "trader-1", "chan-1"))

	exists, ok := x.hasStopLossOrder("BTCUSDT")
	if !ok || exists {
		t.Fatalf("TP-only orders: exists=%v ok=%v, want exists=false ok=true", exists, ok)
	}
	mock.openOrders = append(mock.openOrders, types.OpenOrder{Type: "STOP_MARKET", StopPrice: 95})
	exists, ok = x.hasStopLossOrder("BTCUSDT")
	if !ok || !exists {
		t.Fatalf("with STOP_MARKET: exists=%v ok=%v, want both true", exists, ok)
	}
}
