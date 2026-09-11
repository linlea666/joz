package copytrader

// Regression tests for the stale-positions-cache incident (2026-09-11):
// a reconcile cycle read a positions snapshot taken before a market short
// filled, concluded the position was gone, cancelled the fresh SL/TP and
// marked the trade CLOSED — leaving a live unprotected position.
//
//   - reconcileOpenTrade must require TWO consecutive misses before closing
//   - a reappearing position clears the miss marker
//   - trade cleanup cancels only this direction's orders (hedge mode can hold
//     both directions of a symbol; the sibling's SL must survive)

import (
	"errors"
	"testing"

	"nofx/store"
	"nofx/trader/types"
)

func newReconcileEngine(st *store.Store, mock types.Trader) *Engine {
	events := NewEventLogger(st, "trader-1", "chan-1")
	return &Engine{
		traderID: "trader-1",
		cfg:      &CopyTradingConfig{},
		st:       st,
		exec:     NewExecutor("trader-1", mock, st, events),
		events:   events,
	}
}

func TestReconcileCloseRequiresTwoConsecutiveMisses(t *testing.T) {
	st := newTestStore(t)
	mock := &mockExchange{positions: nil} // position invisible (stale cache)
	e := newReconcileEngine(st, mock)
	ctx := newTestContext(t, st, StateOpen)

	// First miss: must keep the trade OPEN and touch no orders.
	e.reconcileOpenTrade(ctx)
	stored, _ := st.CopyTrade().GetContext(ctx.ID)
	if stored.State != string(StateOpen) {
		t.Fatalf("state after first miss = %s, want OPEN (single miss can be a stale snapshot)", stored.State)
	}
	if len(mock.cancelAllLog) != 0 || len(mock.cancelOrderLog) != 0 {
		t.Fatal("first miss must not cancel any orders")
	}

	// Second consecutive miss: now it is a real close.
	e.reconcileOpenTrade(ctx)
	stored, _ = st.CopyTrade().GetContext(ctx.ID)
	if stored.State != string(StateClosed) {
		t.Fatalf("state after second miss = %s, want CLOSED", stored.State)
	}
	if len(mock.cancelAllLog) == 0 {
		t.Fatal("confirmed close must clean up remaining orders")
	}
}

func TestReconcileMissMarkerClearsWhenPositionReappears(t *testing.T) {
	st := newTestStore(t)
	mock := &mockExchange{
		positions: nil,
		// Live SL so the guard has nothing to restore once the position shows.
		openOrders: []types.OpenOrder{{Type: "STOP_MARKET", StopPrice: 95}},
	}
	e := newReconcileEngine(st, mock)
	ctx := newTestContext(t, st, StateOpen)

	e.reconcileOpenTrade(ctx) // miss #1
	mock.positions = []map[string]interface{}{{
		"symbol": "BTCUSDT", "side": "long", "positionAmt": 0.5, "entryPrice": 100.0,
	}}
	e.reconcileOpenTrade(ctx) // position visible again: marker must clear

	mock.positions = nil
	e.reconcileOpenTrade(ctx) // a NEW first miss, not a second one
	stored, _ := st.CopyTrade().GetContext(ctx.ID)
	if stored.State != string(StateOpen) {
		t.Fatalf("state = %s, want OPEN (miss counter must restart after the position reappeared)", stored.State)
	}
}

// sideCancelMock adds side-aware cancellation on top of mockExchange.
type sideCancelMock struct {
	*mockExchange
	sideCancels []string // "SYMBOL:SIDE"
	sideErr     error
}

func (m *sideCancelMock) CancelOrdersBySide(symbol, positionSide string) error {
	m.sideCancels = append(m.sideCancels, symbol+":"+positionSide)
	return m.sideErr
}

func TestCancelTradeOrdersQuietFiltersBySide(t *testing.T) {
	st := newTestStore(t)
	base := &mockExchange{}
	mock := &sideCancelMock{mockExchange: base}
	x := NewExecutor("trader-1", mock, st, NewEventLogger(st, "trader-1", "chan-1"))

	x.cancelTradeOrdersQuiet("BTCUSDT", string(DirectionShort))
	if len(mock.sideCancels) != 1 || mock.sideCancels[0] != "BTCUSDT:SHORT" {
		t.Fatalf("side-aware cancel calls = %v, want [BTCUSDT:SHORT]", mock.sideCancels)
	}
	if len(base.cancelAllLog) != 0 {
		t.Fatal("must not fall back to cancel-all when the side cancel succeeded")
	}
}

func TestCancelTradeOrdersQuietFallsBackOnError(t *testing.T) {
	st := newTestStore(t)
	base := &mockExchange{}
	mock := &sideCancelMock{mockExchange: base, sideErr: errors.New("api down")}
	x := NewExecutor("trader-1", mock, st, NewEventLogger(st, "trader-1", "chan-1"))

	x.cancelTradeOrdersQuiet("BTCUSDT", string(DirectionLong))
	if len(base.cancelAllLog) != 1 {
		t.Fatalf("failed side cancel must fall back to cancel-all, log=%v", base.cancelAllLog)
	}
}

func TestCancelTradeOrdersQuietWithoutSideSupport(t *testing.T) {
	st := newTestStore(t)
	base := &mockExchange{}
	x := NewExecutor("trader-1", base, st, NewEventLogger(st, "trader-1", "chan-1"))

	x.cancelTradeOrdersQuiet("BTCUSDT", string(DirectionLong))
	if len(base.cancelAllLog) != 1 {
		t.Fatalf("exchanges without side filtering must use cancel-all, log=%v", base.cancelAllLog)
	}
}
