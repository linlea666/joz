package copytrader

// Regression tests for the post-close self-healing and protection-size
// alignment (2026-09-12 incidents):
//
//   - a trade closed by RECONCILE_CLOSED whose position reappears within the
//     recheck window must be resurrected to OPEN and re-protected (BTCUSDT
//     0.007 / SNDKUSDT 0.15: spurious closes left live naked positions)
//   - a position larger than the tracked quantity (merged entries) must have
//     its quantity adopted and its SL resized to the full size (SNDKUSDT:
//     0.27 position guarded by a 0.12 stop)
//   - stop-loss cancellation must be side-filtered on capable exchanges so a
//     hedge-mode sibling's stop survives, with a symbol-wide fallback

import (
	"errors"
	"testing"

	"nofx/store"
	"nofx/trader/types"
)

// closeTwice drives reconcileOpenTrade through both misses so the trade ends
// up CLOSED with last_action=RECONCILE_CLOSED and on the recheck watch list.
func closeTwice(e *Engine, ctx *store.CopyTradeContext) {
	e.reconcileOpenTrade(ctx)
	e.reconcileOpenTrade(ctx)
}

func TestRecheckResurrectsSpuriouslyClosedTrade(t *testing.T) {
	st := newTestStore(t)
	mock := &mockExchange{positions: nil}
	e := newReconcileEngine(st, mock)
	ctx := newTestContext(t, st, StateOpen)

	closeTwice(e, ctx)
	stored, _ := st.CopyTrade().GetContext(ctx.ID)
	if stored.State != string(StateClosed) {
		t.Fatalf("precondition: state = %s, want CLOSED", stored.State)
	}
	if e.closedRecheck[ctx.ID] != closedRecheckCycles {
		t.Fatalf("closed trade must be on the recheck list, got %v", e.closedRecheck)
	}

	// The position shows up again: the close verdict was based on stale data.
	mock.positions = []map[string]interface{}{{
		"symbol": "BTCUSDT", "side": "long", "positionAmt": 0.5, "entryPrice": 100.0,
	}}
	e.recheckClosedTrades()

	stored, _ = st.CopyTrade().GetContext(ctx.ID)
	if stored.State != string(StateOpen) {
		t.Fatalf("state = %s, want OPEN (spurious close must be resurrected)", stored.State)
	}
	if stored.LastAction != "RECOVERED_SPURIOUS_CLOSE" {
		t.Fatalf("last_action = %s, want RECOVERED_SPURIOUS_CLOSE", stored.LastAction)
	}
	if stored.ClosedAt != nil {
		t.Fatal("closed_at must be cleared on resurrection")
	}
	// No live SL order on the exchange (the close cleanup cancelled it): the
	// SL guard must restore protection on the same pass.
	if len(mock.setStopLossLog) == 0 || mock.setStopLossLog[len(mock.setStopLossLog)-1] != 95 {
		t.Fatalf("SL must be re-placed at the tracked price 95, log=%v", mock.setStopLossLog)
	}
	if _, watching := e.closedRecheck[ctx.ID]; watching {
		t.Fatal("resurrected trade must leave the recheck list")
	}
}

func TestRecheckExpiresAfterConfirmedClose(t *testing.T) {
	st := newTestStore(t)
	mock := &mockExchange{positions: nil}
	e := newReconcileEngine(st, mock)
	ctx := newTestContext(t, st, StateOpen)

	closeTwice(e, ctx)
	for i := 0; i < closedRecheckCycles; i++ {
		e.recheckClosedTrades()
	}

	if _, watching := e.closedRecheck[ctx.ID]; watching {
		t.Fatalf("recheck entry must expire after %d empty cycles", closedRecheckCycles)
	}
	stored, _ := st.CopyTrade().GetContext(ctx.ID)
	if stored.State != string(StateClosed) {
		t.Fatalf("state = %s, want CLOSED (a genuinely closed trade stays closed)", stored.State)
	}
	if len(mock.setStopLossLog) != 0 {
		t.Fatalf("no SL may be placed for a confirmed close, log=%v", mock.setStopLossLog)
	}
}

// A larger exchange position is not adopted: the extra size may belong to a
// manual trade or another trader, so only a warning is recorded and the stop
// stays sized for the tracked quantity.
func TestReconcileDoesNotAdoptGrownPosition(t *testing.T) {
	st := newTestStore(t)
	mock := &mockExchange{
		positions: []map[string]interface{}{{
			"symbol": "BTCUSDT", "side": "long", "positionAmt": 0.8, "entryPrice": 100.0,
		}},
		openOrders: []types.OpenOrder{{Type: "STOP_MARKET", StopPrice: 95, PositionSide: "LONG", Quantity: 0.5}},
	}
	e := newReconcileEngine(st, mock)
	ctx := newTestContext(t, st, StateOpen) // tracked quantity 0.5

	e.reconcileOpenTrade(ctx)

	stored, _ := st.CopyTrade().GetContext(ctx.ID)
	if stored.Quantity != 0.5 {
		t.Fatalf("quantity = %v, want 0.5 (growth must not be adopted)", stored.Quantity)
	}
	if stored.LastAction != "POSITION_GROWTH_NOT_ADOPTED" {
		t.Fatalf("last_action = %q, want POSITION_GROWTH_NOT_ADOPTED", stored.LastAction)
	}
	if len(mock.cancelSLLog) != 0 {
		t.Fatal("tracked stop must not be cancelled for unadopted growth")
	}
	for _, q := range mock.setStopLossQtyLog {
		if q == 0.8 {
			t.Fatalf("stop resized to the grown position: qtys=%v", mock.setStopLossQtyLog)
		}
	}
}

// slSideCancelMock adds side-aware stop-loss cancellation on top of mockExchange.
type slSideCancelMock struct {
	*mockExchange
	slSideCancels []string // "SYMBOL:SIDE"
	slSideErr     error
}

func (m *slSideCancelMock) CancelStopLossOrdersBySide(symbol, positionSide string) error {
	m.slSideCancels = append(m.slSideCancels, symbol+":"+positionSide)
	return m.slSideErr
}

func TestCancelStopLossOrdersFiltersBySide(t *testing.T) {
	st := newTestStore(t)
	base := &mockExchange{}
	mock := &slSideCancelMock{mockExchange: base}
	x := NewExecutor("trader-1", mock, st, NewEventLogger(st, "trader-1", "chan-1"))

	if err := x.cancelStopLossOrders("BTCUSDT", string(DirectionShort)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(mock.slSideCancels) != 1 || mock.slSideCancels[0] != "BTCUSDT:SHORT" {
		t.Fatalf("side-aware SL cancel calls = %v, want [BTCUSDT:SHORT]", mock.slSideCancels)
	}
	if len(base.cancelSLLog) != 0 {
		t.Fatal("must not fall back to symbol-wide cancel when the side cancel succeeded")
	}
}

func TestCancelStopLossOrdersFallsBackOnError(t *testing.T) {
	st := newTestStore(t)
	base := &mockExchange{}
	mock := &slSideCancelMock{mockExchange: base, slSideErr: errors.New("api down")}
	x := NewExecutor("trader-1", mock, st, NewEventLogger(st, "trader-1", "chan-1"))

	if err := x.cancelStopLossOrders("BTCUSDT", string(DirectionLong)); err != nil {
		t.Fatalf("fallback path must succeed, got %v", err)
	}
	if len(base.cancelSLLog) != 1 {
		t.Fatalf("failed side cancel must fall back to symbol-wide, log=%v", base.cancelSLLog)
	}
}

func TestCancelStopLossOrdersWithoutSideSupport(t *testing.T) {
	st := newTestStore(t)
	base := &mockExchange{}
	x := NewExecutor("trader-1", base, st, NewEventLogger(st, "trader-1", "chan-1"))

	if err := x.cancelStopLossOrders("BTCUSDT", string(DirectionLong)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(base.cancelSLLog) != 1 {
		t.Fatalf("exchanges without side filtering must use the symbol-wide cancel, log=%v", base.cancelSLLog)
	}
}
