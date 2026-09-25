package copytrader

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"nofx/store"
	"nofx/trader/types"
)

// Deterministic venue simulator: cumulative fills, cancellation races and
// ambiguous acknowledgements, with no network access or real trading account.
type managedVenue struct {
	*mockExchange
	orders           map[string]*types.ManagedOrderResult
	calls            []types.ManagedOrderRequest
	qty, avg, market float64
	rules            types.ManagedMarketRules
	lostAck          bool
	queryDown        bool
	rejectLimit      bool
	failSL           bool
	cancelFill       float64
	partialMarket    float64
	reportedQty      float64 // simulate delayed position visibility after a fill
	definiteReject   bool
	direction        string
	onSubmit         func(*types.ManagedOrderResult)
}

func newManagedVenue() *managedVenue {
	return &managedVenue{mockExchange: &mockExchange{}, orders: map[string]*types.ManagedOrderResult{}, market: 101, rules: types.ManagedMarketRules{Symbol: "BTCUSDT", Status: "TRADING", ContractType: "PERPETUAL", QuoteAsset: "USDT", QuantityStep: .001, MinQuantity: .001, MaxQuantity: 10000, MarketQuantityStep: .001, MarketMinQuantity: .001, MarketMaxQuantity: 10000, PriceTick: .01, MinNotional: 5}}
}
func (v *managedVenue) MarketRules(symbol string) (*types.ManagedMarketRules, error) {
	r := v.rules
	r.Symbol = symbol
	return &r, nil
}
func (v *managedVenue) GetMarketPrice(string) (float64, error) { return v.market, nil }
func (v *managedVenue) GetBalance() (map[string]interface{}, error) {
	return map[string]interface{}{"totalEquity": 1000.0, "availableBalance": 1000.0}, nil
}
func (v *managedVenue) GetFreshPositions() ([]map[string]interface{}, error) { return v.GetPositions() }
func (v *managedVenue) GetPositions() ([]map[string]interface{}, error) {
	side := strings.ToLower(v.direction)
	if side == "" {
		side = "long"
	}
	if v.reportedQty > 0 {
		return []map[string]interface{}{{"symbol": "BTCUSDT", "side": side, "positionAmt": v.reportedQty, "entryPrice": v.avg}}, nil
	}
	if v.qty <= 1e-10 {
		return nil, nil
	}
	return []map[string]interface{}{{"symbol": "BTCUSDT", "side": side, "positionAmt": v.qty, "entryPrice": v.avg}}, nil
}
func (v *managedVenue) fill(o *types.ManagedOrderResult, quantity, price float64) {
	if quantity <= 0 {
		return
	}
	old := o.ExecutedQty
	o.ExecutedQty += quantity
	o.AvgPrice = (o.AvgPrice*old + price*quantity) / o.ExecutedQty
	o.Status = "PARTIALLY_FILLED"
	if o.ExecutedQty >= o.Quantity-1e-10 {
		o.Status = "FILLED"
	}
	if (o.PositionSide == "LONG" && o.Side == "BUY") || (o.PositionSide == "SHORT" && o.Side == "SELL") {
		v.avg = (v.qty*v.avg + quantity*price) / (v.qty + quantity)
		v.qty += quantity
	} else {
		v.qty = math.Max(0, v.qty-quantity)
	}
}
func (v *managedVenue) SubmitManagedOrder(r *types.ManagedOrderRequest) (*types.ManagedOrderResult, error) {
	v.calls = append(v.calls, *r)
	if !r.ReduceOnly {
		v.direction = r.PositionSide
	}
	if _, exists := v.orders[r.ClientID]; exists {
		return nil, errors.New("duplicate client ID")
	}
	if v.rejectLimit && r.Type == "LIMIT" && !r.ReduceOnly {
		if v.definiteReject {
			return nil, types.ErrManagedOrderRejected
		}
		return nil, errors.New("limit rejected")
	}
	o := &types.ManagedOrderResult{OrderID: fmt.Sprint(len(v.orders) + 1), ClientID: r.ClientID, Symbol: r.Symbol, Type: r.Type, Side: r.Side, PositionSide: r.PositionSide, Status: "NEW", Quantity: r.Quantity, Price: r.Price}
	v.orders[r.ClientID] = o
	if r.Type == "MARKET" {
		q := r.Quantity
		if v.partialMarket > 0 && !r.ReduceOnly {
			q *= v.partialMarket
		}
		v.fill(o, q, v.market)
	}
	if v.onSubmit != nil {
		v.onSubmit(o)
	}
	if v.lostAck {
		v.lostAck = false
		return nil, errors.New("timeout after acceptance")
	}
	copy := *o
	return &copy, nil
}
func (v *managedVenue) GetManagedOrder(symbol, id, client string) (*types.ManagedOrderResult, error) {
	if v.queryDown {
		return nil, errors.New("query timeout")
	}
	for _, o := range v.orders {
		if (client != "" && o.ClientID == client) || (id != "" && o.OrderID == id) {
			c := *o
			return &c, nil
		}
	}
	return nil, types.ErrManagedOrderNotFound
}
func (v *managedVenue) CancelManagedOrder(symbol, id, client string) (*types.ManagedOrderResult, error) {
	o, err := v.GetManagedOrder(symbol, id, client)
	if err != nil {
		return nil, err
	}
	live := v.orders[o.ClientID]
	if live.Status != "FILLED" {
		if v.cancelFill > 0 && live.Side == "BUY" {
			v.fill(live, math.Min(v.cancelFill, live.Quantity-live.ExecutedQty), live.Price)
			v.cancelFill = 0
		}
		if live.Status != "FILLED" {
			live.Status = "CANCELED"
		}
	}
	c := *live
	return &c, nil
}
func (v *managedVenue) GetOrderStatus(symbol, id string) (map[string]interface{}, error) {
	o, err := v.GetManagedOrder(symbol, id, "")
	if err != nil {
		return nil, err
	}
	return map[string]interface{}{"status": o.Status, "executedQty": o.ExecutedQty, "avgPrice": o.AvgPrice}, nil
}
func (v *managedVenue) CancelOrder(symbol, id string) error {
	_, err := v.CancelManagedOrder(symbol, id, "")
	return err
}
func (v *managedVenue) SetStopLoss(symbol, side string, qty, price float64) error {
	if v.failSL {
		return errors.New("SL rejected")
	}
	v.setStopLossLog = append(v.setStopLossLog, price)
	v.openOrders = []types.OpenOrder{{OrderID: "stop", Symbol: symbol, PositionSide: side, Type: "STOP_MARKET", StopPrice: price, ClosePosition: true}}
	return nil
}
func (v *managedVenue) CancelStopLossOrders(string) error { v.openOrders = nil; return nil }
func (v *managedVenue) CancelOrdersBySide(symbol, side string) error {
	for _, o := range v.orders {
		if o.Symbol == symbol && o.PositionSide == side && !orderTerminal(o.Status) {
			o.Status = "CANCELED"
		}
	}
	v.openOrders = nil
	return nil
}
func splitPlan() *OpenPlan {
	return &OpenPlan{Symbol: "BTCUSDT", Direction: DirectionLong, EntryType: EntryPlanMarket, EntryPrice: 101, SplitReference: 100, Quantity: 1, StopLoss: 90, Leverage: 10, RiskBudget: 20, MaxNotional: 30000, AvailableMargin: 1000, TPPrices: []float64{110, 120}, TPRatios: []float64{50, 50}, RootMsgID: "split-root", ChannelID: "chan-1", EntryTimeout: 240 * time.Minute, SignalExpiresAt: time.Now().Add(5 * time.Minute)}
}
func managedExecutor(t *testing.T) (*Executor, *managedVenue, *store.Store) {
	t.Helper()
	st := newTestStore(t)
	v := newManagedVenue()
	return NewExecutor("trader-1", v, st, NewEventLogger(st, "trader-1", "chan-1")), v, st
}
func entryCalls(v *managedVenue) []types.ManagedOrderRequest {
	var calls []types.ManagedOrderRequest
	for _, c := range v.calls {
		if !c.ReduceOnly {
			calls = append(calls, c)
		}
	}
	return calls
}

func TestSplitBudgetAndProtectedOrdering(t *testing.T) {
	x, v, st := managedExecutor(t)
	ctx, err := x.ExecuteOpen("trace", "signal", splitPlan())
	if err != nil {
		t.Fatal(err)
	}
	calls := entryCalls(v)
	if len(calls) != 2 || calls[0].Type != "MARKET" || calls[1].Type != "LIMIT" {
		t.Fatalf("entries: %+v", calls)
	}
	if calls[0].Quantity != .909 || calls[1].Quantity != 1 {
		t.Fatalf("risk halves must differ in coins: %+v", calls)
	}
	if len(v.setStopLossLog) == 0 {
		t.Fatal("missing stop")
	}
	if ctx.State != "OPEN" || !ctx.EntryWorking {
		t.Fatalf("context %+v", ctx)
	}
	rows, _ := st.CopyTrade().GetOrdersForContext("trader-1", ctx.ID)
	if len(rows) != 4 {
		t.Fatalf("durable entries and TPs: %d", len(rows))
	}
	n, _ := st.CopyTrade().CountActiveByTrader("trader-1")
	if n != 1 {
		t.Fatal("split consumes more than one context")
	}
}

func TestSplitFloorsAggregateCapsAndRejectsSmallLeg(t *testing.T) {
	rules := newManagedVenue().rules
	for _, cap := range []float64{30000, 100} {
		p := splitPlan()
		p.MaxNotional = cap
		q1, q2, err := splitQuantities(p, &rules)
		if err != nil {
			t.Fatal(err)
		}
		if q1*11+q2*10 > 20+1e-9 || q1*101+q2*100 > cap+1e-9 {
			t.Fatal("budget exceeded")
		}
	}
	p := splitPlan()
	p.RiskBudget = .01
	if _, _, err := splitQuantities(p, &rules); err == nil {
		t.Fatal("small split must be rejected before first submit")
	}
}

func TestSplitActualSlippageLimitsSecondLeg(t *testing.T) {
	x, v, _ := managedExecutor(t)
	v.market = 102
	ctx, err := x.ExecuteOpen("trace", "signal", splitPlan())
	if err != nil {
		t.Fatal(err)
	}
	legs, _ := x.entryOrders(ctx)
	risk := legs[0].ExecutedQty*12 + legs[1].Quantity*10
	if risk > 20+1e-9 {
		t.Fatalf("risk %v", risk)
	}
	if legs[1].Quantity >= 1 {
		t.Fatal("second leg did not shrink after slippage")
	}
}

func TestSplitOvershootCancelsSecondAndTrims(t *testing.T) {
	x, v, _ := managedExecutor(t)
	v.market = 120
	ctx, err := x.ExecuteOpen("trace", "signal", splitPlan())
	if err != nil {
		t.Fatal(err)
	}
	if v.qty*30 > 20+1e-7 {
		t.Fatalf("remaining risk %v", v.qty*30)
	}
	if !ctx.EntryDisabled || len(entryCalls(v)) != 1 {
		t.Fatal("overshoot must close replenishment")
	}
}

func TestSplitPartialFillProtectedAndTimeoutSettled(t *testing.T) {
	x, v, _ := managedExecutor(t)
	v.partialMarket = .5
	ctx, err := x.ExecuteOpen("trace", "signal", splitPlan())
	if err != nil {
		t.Fatal(err)
	}
	if len(entryCalls(v)) != 1 || len(v.setStopLossLog) == 0 {
		t.Fatal("partial first leg must be protected before second")
	}
	deadline := time.Now().Add(-time.Second)
	x.updateContext(ctx, map[string]interface{}{"entry_deadline": &deadline})
	v.cancelFill = .1
	if err = x.reconcileManagedEntry("timeout", ctx); err != nil {
		t.Fatal(err)
	}
	if ctx.State != "OPEN" || !ctx.EntryDisabled || math.Abs(ctx.Quantity-(.909*.5+.1)) > 1e-9 {
		t.Fatalf("partial cancel race orphaned: %+v", ctx)
	}
	if len(entryCalls(v)) != 1 {
		t.Fatal("second leg appeared after cancellation")
	}
}

func TestSplitRestartDoesNotResubmitFilledMarket(t *testing.T) {
	x, v, st := managedExecutor(t)
	ctx, err := x.ExecuteOpen("trace", "signal", splitPlan())
	if err != nil {
		t.Fatal(err)
	}
	entries := len(entryCalls(v))
	restored, _ := st.CopyTrade().GetContext(ctx.ID)
	restarted := NewExecutor("trader-1", v, st, NewEventLogger(st, "trader-1", "chan-1"))
	if err = restarted.reconcileManagedEntry("restart", restored); err != nil {
		t.Fatal(err)
	}
	if len(entryCalls(v)) != entries {
		t.Fatal("restart duplicated entry")
	}
}

func TestLostAckQueriesClientIDWithoutResubmission(t *testing.T) {
	x, v, _ := managedExecutor(t)
	v.lostAck = true
	ctx, err := x.ExecuteOpen("trace", "signal", splitPlan())
	if err != nil {
		t.Fatal(err)
	}
	if len(entryCalls(v)) != 2 || ctx.Quantity <= 0 {
		t.Fatal("lost acknowledgment not recovered")
	}
	for _, c := range v.calls {
		if !strings.HasPrefix(c.ClientID, "ct-") {
			t.Fatal("missing stable client ID")
		}
	}
}

func TestUnknownAckStillProtectsObservedPosition(t *testing.T) {
	x, v, _ := managedExecutor(t)
	v.lostAck = true
	v.queryDown = true
	ctx, err := x.ExecuteOpen("trace", "signal", splitPlan())
	if err == nil {
		t.Fatal("unknown status must be surfaced")
	}
	if ctx == nil || len(v.setStopLossLog) == 0 || len(entryCalls(v)) != 1 {
		t.Fatal("uncertain first leg lost protection or spawned second")
	}
	count := len(v.calls)
	if err = x.reconcileManagedEntry("still-unknown", ctx); err == nil || len(v.calls) != count {
		t.Fatal("unconfirmed fill price caused an extra entry or invented risk trim")
	}
	v.queryDown = false
	if err = x.reconcileManagedEntry("recover", ctx); err != nil {
		t.Fatal(err)
	}
	if len(entryCalls(v)) != 2 {
		t.Fatal("did not resume from confirmed fill")
	}
}

func TestFirstLegSLFailureNeverSubmitsSecond(t *testing.T) {
	x, v, _ := managedExecutor(t)
	v.failSL = true
	ctx, err := x.ExecuteOpen("trace", "signal", splitPlan())
	if err == nil {
		t.Fatal("SL failure hidden")
	}
	if len(entryCalls(v)) != 1 || !ctx.EntryDisabled {
		t.Fatal("unprotected first leg added exposure")
	}
	if v.qty > 1e-10 {
		t.Fatal("emergency exit did not close first leg")
	}
}

func TestSecondLegFailureKeepsProtectedFirst(t *testing.T) {
	x, v, _ := managedExecutor(t)
	v.rejectLimit = true
	ctx, err := x.ExecuteOpen("trace", "signal", splitPlan())
	if err == nil {
		t.Fatal("failure missing")
	}
	if v.qty <= 0 || len(v.setStopLossLog) == 0 {
		t.Fatal("lost protected first leg")
	}
	if err = x.reconcileManagedEntry("retry", ctx); err == nil {
		t.Fatal("unknown failed order must remain explicit")
	}
	if len(entryCalls(v)) != 2 {
		t.Fatal("second leg converted/retried as a new order")
	}
}

func TestSplitReduceClosesReplenishmentAndRestoresTPs(t *testing.T) {
	x, v, st := managedExecutor(t)
	ctx, err := x.ExecuteOpen("trace", "signal", splitPlan())
	if err != nil {
		t.Fatal(err)
	}
	v.cancelFill = .1
	if _, err = x.ExecuteClose("reduce", "reduce-signal", ctx, 50); err != nil {
		t.Fatal(err)
	}
	if !ctx.EntryDisabled || ctx.ClosePendingJSON != "" {
		t.Fatalf("exit not settled: %+v", ctx)
	}
	before := len(entryCalls(v))
	if err = x.reconcileManagedEntry("restart", ctx); err != nil {
		t.Fatal(err)
	}
	if len(entryCalls(v)) != before {
		t.Fatal("replenished after reduce")
	}
	stored, _ := st.CopyTrade().GetContext(ctx.ID)
	if confirmedTPLevel(stored, 1) {
		t.Fatal("manual reduction fabricated TP1")
	}
	remaining := 0.0
	for _, tp := range readTPPlan(stored) {
		if !tp.Filled {
			remaining += tp.Quantity - tp.FilledQuantity
		}
	}
	if remaining > v.qty+1e-9 || remaining <= 0 {
		t.Fatalf("remaining TP coverage %g vs position %g", remaining, v.qty)
	}
}

func TestTrueTPFillStopsAddAndTriggersConfiguredLevel(t *testing.T) {
	x, v, st := managedExecutor(t)
	ctx, err := x.ExecuteOpen("trace", "signal", splitPlan())
	if err != nil {
		t.Fatal(err)
	}
	tp := readTPPlan(ctx)[0]
	for _, o := range v.orders {
		if o.OrderID == tp.OrderID {
			v.fill(o, o.Quantity, 110)
		}
	}
	e := newReconcileEngine(st, v)
	e.cfg.AutoBreakevenAfterTP = true
	if err = x.reconcileManagedEntry("TP", ctx); err != nil {
		t.Fatal(err)
	}
	e.reconcileOpenTrade(ctx)
	if !ctx.EntryDisabled || !ctx.BreakevenApplied || math.Abs(ctx.StopLossPrice-ctx.AvgFillPrice) > 1e-8 {
		t.Fatalf("TP1 did not trigger BE: %+v", ctx)
	}
	if !readTPPlan(ctx)[0].Filled {
		t.Fatal("completed TP restored")
	}
}

func TestTP2ConditionAndTighterStop(t *testing.T) {
	x, v, st := managedExecutor(t)
	ctx, err := x.ExecuteOpen("trace", "signal", splitPlan())
	if err != nil {
		t.Fatal(err)
	}
	x.updateContext(ctx, map[string]interface{}{"breakeven_after_tp": true, "breakeven_tp_level": 2})
	tp := readTPPlan(ctx)[0]
	for _, o := range v.orders {
		if o.OrderID == tp.OrderID {
			v.fill(o, o.Quantity, 110)
		}
	}
	e := newReconcileEngine(st, v)
	e.cfg.AutoBreakevenAfterTP = true
	e.reconcileOpenTrade(ctx)
	if ctx.BreakevenApplied {
		t.Fatal("TP1 incorrectly satisfied TP2 condition")
	}
	ctx.BreakevenTPLevel = 1
	x.updateContext(ctx, map[string]interface{}{"breakeven_tp_level": 1})
	v.openOrders[0].StopPrice = 105
	e.reconcileOpenTrade(ctx)
	if ctx.StopLossPrice != 105 || !ctx.BreakevenApplied {
		t.Fatalf("BE loosened raised stop: %+v", ctx)
	}
}

func TestStopGuardRejectsOppositeSideAndUndersizedStop(t *testing.T) {
	x, v, st := managedExecutor(t)
	ctx := newTestContext(t, st, StateOpen)
	v.qty = .5
	v.avg = 100
	v.openOrders = []types.OpenOrder{{Symbol: "BTCUSDT", PositionSide: "SHORT", Type: "STOP_MARKET", StopPrice: 95, ClosePosition: true}}
	if err := x.ensureStopProtection("t", "s", ctx, .5); err != nil {
		t.Fatal(err)
	}
	if len(v.setStopLossLog) != 1 {
		t.Fatal("short stop was accepted for long")
	}
	v.openOrders[0].ClosePosition = false
	v.openOrders[0].Quantity = .1
	if err := x.ensureStopProtection("t", "s", ctx, .5); err != nil {
		t.Fatal(err)
	}
	if len(v.setStopLossLog) != 2 {
		t.Fatal("undersized stop accepted")
	}
}

func TestSecondLegPartialFillsImmediatelyResizeProtection(t *testing.T) {
	x, v, _ := managedExecutor(t)
	ctx, err := x.ExecuteOpen("trace", "signal", splitPlan())
	if err != nil {
		t.Fatal(err)
	}
	entries, _ := x.entryOrders(ctx)
	o := v.orders[entries[1].ClientID]
	v.fill(o, .4, 100)
	if err = x.reconcileManagedEntry("partial", ctx); err != nil {
		t.Fatal(err)
	}
	if !ctx.HasAddFill || math.Abs(ctx.Quantity-1.309) > 1e-9 || !ctx.EntryWorking {
		t.Fatalf("partial second leg missing %+v", ctx)
	}
	total := 0.0
	for _, tp := range readTPPlan(ctx) {
		if !tp.Filled {
			total += tp.Quantity - tp.FilledQuantity
		}
	}
	if total > ctx.Quantity+1e-9 || ctx.Quantity-total > .002 {
		t.Fatalf("TPs not resized: %v / %v", total, ctx.Quantity)
	}
}

func TestLimitCancellationRaceKeepsAndProtectsLegacyFill(t *testing.T) {
	st := newTestStore(t)
	v := &mockExchange{orderStatus: map[string]interface{}{"status": "PARTIALLY_FILLED", "executedQty": .2, "avgPrice": 100.0}}
	x := NewExecutor("trader-1", v, st, NewEventLogger(st, "trader-1", "chan-1"))
	ctx := newTestContext(t, st, StateEntryPending)
	ctx.PlannedEntryPrice = 100
	if _, err := x.ExecuteCancel("cancel", "signal", ctx); err != nil {
		t.Fatal(err)
	}
	if ctx.State != "OPEN" || ctx.Quantity != .2 || !ctx.EntryDisabled || len(v.setStopLossLog) == 0 {
		t.Fatalf("partial fill orphaned %+v", ctx)
	}
}
