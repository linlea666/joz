package copytrader

// Regression tests for execution/recovery convergence:
//   - 2.3 exit intents never pin a trade in CLOSE_PENDING (rejected / residual
//     exits resubmit with a new identity; a new full close supersedes a stuck
//     partial; TP replacement pre-checks and rolls back; repeated emergency
//     closes never collide on the order primary key)
//   - 2.4 unlanded entry legs converge to ABANDONED; a stuck entry cancel does
//     not block closing or re-stopping the known position
//   - 2.5 no resurrection on fresh-read venues; growth is never adopted
//   - 2.7 pause keeps PLANNED legs unsubmitted
//   - 2.8 deferred market first legs are re-priced before submission
//   - 2.9 one flat read never closes a trade from UPDATE_SL
//   - 2.11 legacy venues close before cleanup; only provably unsent exits retry

import (
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"nofx/store"
	"nofx/trader/types"
)

// convergenceVenue adds deterministic failure injection to managedVenue.
type convergenceVenue struct {
	*managedVenue
	rejectReduce int    // definite rejections for the next N reduce-only MARKET orders
	dropEntry    bool   // next entry submit times out WITHOUT reaching the venue
	cancelDown   string // client ID whose cancellation keeps failing
	afterReduce  func(*types.ManagedOrderResult)
}

func (v *convergenceVenue) SubmitManagedOrder(r *types.ManagedOrderRequest) (*types.ManagedOrderResult, error) {
	if r.ReduceOnly && r.Type == "MARKET" && v.rejectReduce > 0 {
		v.rejectReduce--
		v.calls = append(v.calls, *r)
		return nil, types.ErrManagedOrderRejected
	}
	if !r.ReduceOnly && v.dropEntry {
		v.dropEntry = false
		v.calls = append(v.calls, *r)
		return nil, errors.New("gateway timeout before acceptance")
	}
	o, err := v.managedVenue.SubmitManagedOrder(r)
	if err == nil && r.ReduceOnly && r.Type == "MARKET" && v.afterReduce != nil {
		v.afterReduce(o)
	}
	return o, err
}

func (v *convergenceVenue) CancelManagedOrder(symbol, id, client string) (*types.ManagedOrderResult, error) {
	if v.cancelDown != "" && client == v.cancelDown {
		return nil, errors.New("cancel endpoint timeout")
	}
	return v.managedVenue.CancelManagedOrder(symbol, id, client)
}

func convergenceExecutor(t *testing.T) (*Executor, *convergenceVenue, *store.Store) {
	t.Helper()
	st := newTestStore(t)
	v := &convergenceVenue{managedVenue: newManagedVenue()}
	return NewExecutor("trader-1", v, st, NewEventLogger(st, "trader-1", "chan-1")), v, st
}

func reduceCalls(v *convergenceVenue) []types.ManagedOrderRequest {
	var out []types.ManagedOrderRequest
	for _, c := range v.calls {
		if c.ReduceOnly && c.Type == "MARKET" {
			out = append(out, c)
		}
	}
	return out
}

func singleLegPlan() *OpenPlan {
	p := splitPlan()
	p.SplitReference = 0
	return p
}

func TestRejectedCloseResubmitsWithNewIdentity(t *testing.T) {
	x, v, st := convergenceExecutor(t)
	ctx, err := x.ExecuteOpen("open", "open", singleLegPlan())
	if err != nil {
		t.Fatal(err)
	}
	v.rejectReduce = 1
	if _, err = x.ExecuteClose("close", "close", ctx, 100); err == nil {
		t.Fatal("definite rejection must surface")
	}
	if ctx.ClosePendingJSON == "" || v.qty <= 0 {
		t.Fatalf("rejected exit lost its intent: %+v", ctx)
	}
	if err = x.reconcileManagedClose("reconcile", ctx); err != nil {
		t.Fatal(err)
	}
	calls := reduceCalls(v)
	if len(calls) != 2 || calls[0].ClientID == calls[1].ClientID {
		t.Fatalf("resubmission reused identity or did not happen: %+v", calls)
	}
	stored, _ := st.CopyTrade().GetContext(ctx.ID)
	if v.qty != 0 || stored.State != string(StateClosed) || stored.ClosePendingJSON != "" {
		t.Fatalf("exit did not converge: qty=%g %+v", v.qty, stored)
	}
	rows, _ := x.exitRows(ctx, "close")
	if len(rows) != 2 || rows[0].ID == rows[1].ID || rows[0].Role != rows[1].Role {
		t.Fatalf("exit generations not recorded under one role: %+v", rows)
	}
}

func TestRejectedExitExhaustsThenReleasesForLaterFullClose(t *testing.T) {
	x, v, st := convergenceExecutor(t)
	ctx, err := x.ExecuteOpen("open", "open", singleLegPlan())
	if err != nil {
		t.Fatal(err)
	}
	action := &store.CopyTradeAction{ID: "reduce-action", TraderID: "trader-1", ContextID: ctx.ID, SignalID: "reduce", Action: "REDUCE", Status: "executing"}
	if _, err = st.CopyTrade().ClaimAction(action); err != nil {
		t.Fatal(err)
	}
	v.rejectReduce = 100
	if _, err = x.ExecuteClose("reduce", "reduce", ctx, 50); err == nil {
		t.Fatal("rejection hidden")
	}
	for i := 0; i < maxExitResubmits; i++ {
		_ = x.reconcileManagedClose("reconcile", ctx)
		if ctx.ClosePendingJSON == "" {
			t.Fatalf("released before retries were exhausted (pass %d)", i)
		}
	}
	if err = x.reconcileManagedClose("reconcile", ctx); err == nil || !strings.Contains(err.Error(), "EXIT_ABANDONED") {
		t.Fatalf("exhaustion not reported: %v", err)
	}
	if ctx.ClosePendingJSON != "" || ctx.State != string(StateOpen) || !strings.HasPrefix(ctx.LastError, "EXIT_ABANDONED") {
		t.Fatalf("stuck exit still blocks the trade: %+v", ctx)
	}
	if got := len(reduceCalls(v)); got != 1+maxExitResubmits {
		t.Fatalf("unbounded resubmissions: %d", got)
	}
	saved, _ := st.CopyTrade().GetAction(action.ID)
	if saved.Status != "rejected" {
		t.Fatalf("abandoned action left for blind retry: %+v", saved)
	}
	v.rejectReduce = 0
	if _, err = x.ExecuteClose("close", "close-later", ctx, 100); err != nil {
		t.Fatal(err)
	}
	if v.qty != 0 || ctx.State != string(StateClosed) {
		t.Fatalf("later full close blocked: qty=%g %+v", v.qty, ctx)
	}
}

func TestNewFullCloseSupersedesStuckPartialExit(t *testing.T) {
	x, v, _ := convergenceExecutor(t)
	ctx, err := x.ExecuteOpen("open", "open", singleLegPlan())
	if err != nil {
		t.Fatal(err)
	}
	v.rejectReduce = 1
	if _, err = x.ExecuteClose("reduce", "reduce", ctx, 50); err == nil {
		t.Fatal("rejection hidden")
	}
	before := v.qty
	skip, err := x.ExecuteClose("close", "close", ctx, 100)
	if err != nil || skip != SkipNone {
		t.Fatalf("full close blocked by stuck partial: %s %v", skip, err)
	}
	calls := reduceCalls(v)
	if len(calls) != 2 || math.Abs(calls[1].Quantity-before) > 1e-9 {
		t.Fatalf("full close must use the fresh position and must not first replay the partial: %+v", calls)
	}
	if v.qty != 0 || ctx.State != string(StateClosed) || ctx.ClosePendingJSON != "" {
		t.Fatalf("not closed: qty=%g %+v", v.qty, ctx)
	}
}

func TestFilledFullCloseWithResidualResubmitsAfterConfirmation(t *testing.T) {
	old := exitResidualConfirmAfter
	exitResidualConfirmAfter = 0
	t.Cleanup(func() { exitResidualConfirmAfter = old })
	x, v, _ := convergenceExecutor(t)
	ctx, err := x.ExecuteOpen("open", "open", singleLegPlan())
	if err != nil {
		t.Fatal(err)
	}
	once := true
	v.afterReduce = func(*types.ManagedOrderResult) {
		if once {
			once = false
			v.qty += .1 // a late fill leaves a residual after the full close
		}
	}
	if _, err = x.ExecuteClose("close", "close", ctx, 100); err == nil {
		t.Fatal("single residual sighting must wait for confirmation")
	}
	if len(reduceCalls(v)) != 1 {
		t.Fatal("resubmitted on the first residual sighting")
	}
	if err = x.reconcileManagedClose("confirm", ctx); err != nil {
		t.Fatal(err)
	}
	calls := reduceCalls(v)
	if len(calls) != 2 || math.Abs(calls[1].Quantity-.1) > 1e-9 || calls[0].ClientID == calls[1].ClientID {
		t.Fatalf("residual not closed with fresh size and identity: %+v", calls)
	}
	if v.qty != 0 || ctx.State != string(StateClosed) {
		t.Fatalf("not closed: %g %+v", v.qty, ctx)
	}
}

func TestTPReductionPrecheckKeepsTPAndWritesNoIntent(t *testing.T) {
	x, v, st := convergenceExecutor(t)
	ctx, err := x.ExecuteOpen("open", "open", splitPlan())
	if err != nil {
		t.Fatal(err)
	}
	first := readTPPlan(ctx)[0]
	v.rules.MarketMinQuantity = 100 // the TP remainder can never be a market order
	skip, err := x.ExecuteTPReduction("reduce", "reduce-tp1", ctx, 1)
	if err == nil || skip != SkipNeedsContext {
		t.Fatalf("precheck must refuse: %s %v", skip, err)
	}
	stored, _ := st.CopyTrade().GetContext(ctx.ID)
	if stored.ClosePendingJSON != "" || stored.State != string(StateOpen) {
		t.Fatalf("intent written despite failed precheck: %+v", stored)
	}
	if v.orders[first.ClientID].Status != "NEW" || len(reduceCalls(v)) != 0 {
		t.Fatal("TP cancelled or exit sent despite failed precheck")
	}
}

type tpCancelFillVenue struct {
	*convergenceVenue
	target string
	leave  float64
}

func (v *tpCancelFillVenue) CancelOrder(symbol, id string) error {
	if id == v.target {
		for _, o := range v.orders {
			if o.OrderID == id {
				v.fill(o, o.Quantity-o.ExecutedQty-v.leave, o.Price)
			}
		}
		v.target = ""
	}
	return v.convergenceVenue.CancelOrder(symbol, id)
}

func TestTPReductionRollsBackWhenCancelRaceLeavesUnsendableRemainder(t *testing.T) {
	x, base, st := convergenceExecutor(t)
	ctx, err := x.ExecuteOpen("open", "open", splitPlan())
	if err != nil {
		t.Fatal(err)
	}
	first := readTPPlan(ctx)[0]
	base.rules.MarketMinQuantity = .01
	v := &tpCancelFillVenue{convergenceVenue: base, target: first.OrderID, leave: .005}
	x = NewExecutor(x.traderID, v, st, x.events)
	if _, err = x.ExecuteTPReduction("reduce", "reduce-tp1", ctx, 1); err == nil {
		t.Fatal("unsendable remainder must be reported")
	}
	stored, _ := st.CopyTrade().GetContext(ctx.ID)
	if stored.ClosePendingJSON != "" || stored.State == string(StateClosePending) {
		t.Fatalf("intent not rolled back: %+v", stored)
	}
	if len(reduceCalls(base)) != 0 {
		t.Fatal("market exit sent below minimum")
	}
	for _, tp := range readTPPlan(stored) {
		if tp.ReplacedBy != "" {
			t.Fatal("rolled back TP still marked replaced")
		}
	}
}

func TestRepeatedEmergencyCloseUsesNewGeneration(t *testing.T) {
	x, v, _ := convergenceExecutor(t)
	ctx, err := x.ExecuteOpen("open", "open", singleLegPlan())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = x.executeManagedClose("emergency", "emergency-"+ctx.ID, ctx, 100); err != nil {
		t.Fatal(err)
	}
	v.qty, v.avg = .3, 101 // a later fill reopened exposure on the same trade
	if _, err = x.executeManagedClose("emergency", "emergency-"+ctx.ID, ctx, 100); err != nil {
		t.Fatalf("second emergency close collided: %v", err)
	}
	calls := reduceCalls(v)
	if len(calls) != 2 || calls[0].ClientID == calls[1].ClientID || v.qty != 0 {
		t.Fatalf("second emergency close not executed independently: %+v qty=%g", calls, v.qty)
	}
}

func TestUnlandedEntryLegAbandonedAfterGrace(t *testing.T) {
	x, v, st := convergenceExecutor(t)
	v.dropEntry = true
	ctx, err := x.ExecuteOpen("open", "open", splitPlan())
	if err == nil {
		t.Fatal("uncertain submit must surface")
	}
	rows, _ := x.entryOrders(ctx)
	if rows[0].Status != "UNKNOWN" {
		t.Fatalf("precondition: %s", rows[0].Status)
	}
	if err = x.reconcileManagedEntry("within-grace", ctx); err == nil {
		t.Fatal("not-found inside the grace period must stay unresolved")
	}
	rows, _ = x.entryOrders(ctx)
	if rows[0].Status != "UNKNOWN" {
		t.Fatal("abandoned before the grace period elapsed")
	}
	old := time.Now().Add(-managedNotFoundGrace - time.Second)
	if err = st.GormDB().Exec("UPDATE copytrade_orders SET updated_at = ? WHERE id = ?", old, rows[0].ID).Error; err != nil {
		t.Fatal(err)
	}
	if err = x.reconcileManagedEntry("after-grace", ctx); err != nil {
		t.Fatal(err)
	}
	rows, _ = x.entryOrders(ctx)
	if rows[0].Status != "ABANDONED" || rows[1].Status != "ABANDONED" || ctx.State != string(StateCancelled) {
		t.Fatalf("unlanded entry did not converge: %s/%s %s", rows[0].Status, rows[1].Status, ctx.State)
	}
	if len(entryCalls(v.managedVenue)) != 1 {
		t.Fatal("abandoned leg was resubmitted")
	}
}

func TestEntryCancelFailureDoesNotBlockCloseOrStopUpdate(t *testing.T) {
	for _, op := range []string{"close", "stop"} {
		t.Run(op, func(t *testing.T) {
			x, v, _ := convergenceExecutor(t)
			ctx, err := x.ExecuteOpen("open", "open", splitPlan())
			if err != nil {
				t.Fatal(err)
			}
			entries, _ := x.entryOrders(ctx)
			v.cancelDown = entries[1].ClientID // resting second leg cannot be cancelled
			if op == "close" {
				_, err = x.ExecuteClose("close", "close", ctx, 100)
				if len(reduceCalls(v)) != 1 || v.qty != 0 {
					t.Fatalf("known position not closed: %v qty=%g", err, v.qty)
				}
				if ctx.State == string(StateClosed) {
					t.Fatal("trade finalized while its entry order may still be live")
				}
			} else {
				if _, err = x.ExecuteUpdateSL("stop", "stop", ctx, 95); err != nil {
					t.Fatal(err)
				}
				if ctx.StopLossPrice != 95 {
					t.Fatalf("stop not moved: %g", ctx.StopLossPrice)
				}
			}
			if !ctx.EntryDisabled {
				t.Fatal("entry latch not closed")
			}
		})
	}
}

func TestManagedVenueNeverResurrectsClosedTrade(t *testing.T) {
	st := newTestStore(t)
	v := newManagedVenue()
	e := newReconcileEngine(st, v)
	ctx := newTestContext(t, st, StateOpen)
	closeTwice(e, ctx)
	stored, _ := st.CopyTrade().GetContext(ctx.ID)
	if stored.State != string(StateClosed) {
		t.Fatalf("precondition: %s", stored.State)
	}
	v.qty, v.avg = .5, 100 // identical-looking position appears later
	e.recheckClosedTrades()
	stored, _ = st.CopyTrade().GetContext(ctx.ID)
	if stored.State != string(StateClosed) || len(v.setStopLossLog) != 0 {
		t.Fatalf("managed venue resurrected/claimed a new position: %+v", stored)
	}
}

func TestLegacyResurrectionRequiresExactMatchAndNoOwner(t *testing.T) {
	cases := []struct {
		name      string
		qty, px   float64
		avg       float64
		otherOpen bool
		want      TradeState
	}{
		{"matching", .5, 100, 100, false, StateOpen},
		{"quantity_differs", .8, 100, 100, false, StateClosed},
		{"entry_differs", .5, 110, 100, false, StateClosed},
		{"other_owner", .5, 100, 100, true, StateClosed},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			st := newTestStore(t)
			mock := &mockExchange{}
			e := newReconcileEngine(st, mock)
			ctx := newTestContext(t, st, StateOpen)
			e.exec.updateContext(ctx, map[string]interface{}{"avg_fill_price": c.avg})
			closeTwice(e, ctx)
			if c.otherOpen {
				other := newTestContext(t, st, StateOpen)
				other.RootMessageID = "other"
			}
			mock.positions = []map[string]interface{}{{"symbol": "BTCUSDT", "side": "long", "positionAmt": c.qty, "entryPrice": c.px}}
			e.recheckClosedTrades()
			stored, _ := st.CopyTrade().GetContext(ctx.ID)
			if stored.State != string(c.want) {
				t.Fatalf("state %s want %s", stored.State, c.want)
			}
		})
	}
}

func TestPositionGrowthOnlyWarns(t *testing.T) {
	st := newTestStore(t)
	mock := &mockExchange{
		positions:  []map[string]interface{}{{"symbol": "BTCUSDT", "side": "long", "positionAmt": 0.8, "entryPrice": 100.0}},
		openOrders: []types.OpenOrder{{Type: "STOP_MARKET", StopPrice: 95, PositionSide: "LONG", Quantity: 0.5}},
	}
	e := newReconcileEngine(st, mock)
	ctx := newTestContext(t, st, StateOpen)
	e.reconcileOpenTrade(ctx)
	stored, _ := st.CopyTrade().GetContext(ctx.ID)
	if stored.Quantity != .5 || stored.LastAction != "POSITION_GROWTH_NOT_ADOPTED" {
		t.Fatalf("growth adopted: %+v", stored)
	}
	for _, q := range mock.setStopLossQtyLog {
		if q > .5 {
			t.Fatalf("stop enlarged to un-adopted growth: %v", mock.setStopLossQtyLog)
		}
	}
	var n int64
	st.GormDB().Model(&store.CopyTradeEvent{}).Where("level = ? AND message LIKE ?", "warn", "%not adopted%").Count(&n)
	if n != 1 {
		t.Fatalf("growth warning count %d", n)
	}
	e.reconcileOpenTrade(ctx)
	st.GormDB().Model(&store.CopyTradeEvent{}).Where("level = ? AND message LIKE ?", "warn", "%not adopted%").Count(&n)
	if n != 1 {
		t.Fatalf("growth warning repeated: %d", n)
	}
}

// plannedEntry stores a managed plan whose legs were never submitted.
func plannedEntry(t *testing.T, x *Executor, st *store.Store, p *OpenPlan, decision *entryDecision, q1, q2 float64) *store.CopyTradeContext {
	t.Helper()
	rules := x.ex.(*convergenceVenue).rules
	plan, _ := json.Marshal(managedEntryPlan{Version: 1, Decision: decision, SplitReference: p.SplitReference, RiskBudget: p.RiskBudget, StopLoss: p.StopLoss, MaxNotional: p.MaxNotional, MarginBudget: p.AvailableMargin * .9, SignalExpiresAt: p.SignalExpiresAt, Rules: rules})
	ctx := &store.CopyTradeContext{ID: "planned-" + t.Name(), TraderID: "trader-1", Symbol: p.Symbol, Direction: string(p.Direction), State: "ENTRY_PENDING", ExecutionVersion: 1, EntryPlanJSON: string(plan), StopLossPrice: p.StopLoss, Leverage: p.Leverage, TPRecipeJSON: planRecipeJSON(p), EntryWorking: true}
	orders := []*store.CopyTradeOrder{x.newManagedOrder(ctx, "open", "ENTRY_1", "MARKET", p.EntryPrice, q1)}
	if q2 > 0 {
		ctx.EntryPolicy = EntryPolicySplit
		orders = append(orders, x.newManagedOrder(ctx, "open", "ENTRY_2", "LIMIT", p.SplitReference, q2))
	}
	if err := st.CopyTrade().CreateManagedPlan(ctx, orders); err != nil {
		t.Fatal(err)
	}
	return ctx
}

func TestPausedEngineKeepsPlannedEntryUnsubmitted(t *testing.T) {
	x, v, st := convergenceExecutor(t)
	p := singleLegPlan()
	ctx := plannedEntry(t, x, st, p, nil, .909, 0)
	cfg := DefaultCopyTradingConfig()
	cfg.Paused = true
	x.beforeEntrySubmit = (&Engine{cfg: &cfg}).admitEntrySubmit
	if err := x.reconcileManagedEntry("paused", ctx); !errors.Is(err, errEntryDeferred) {
		t.Fatalf("paused admission not deferred: %v", err)
	}
	rows, _ := x.entryOrders(ctx)
	if len(v.calls) != 0 || rows[0].Status != "PLANNED" {
		t.Fatal("paused engine submitted an entry")
	}
	cfg.Paused = false
	x.beforeEntrySubmit = nil
	if err := x.reconcileManagedEntry("resumed", ctx); err != nil {
		t.Fatal(err)
	}
	if len(entryCalls(v.managedVenue)) != 1 {
		t.Fatal("resumed entry not submitted exactly once")
	}
}

func TestDeferredMarketEntryRevalidatedAtSubmit(t *testing.T) {
	adverse := &entryDecision{Direction: DirectionLong, SourcePriceSpec: PriceSpec{Type: PriceMarket, Price: 100}, ThresholdPct: 1, OrderType: EntryPlanMarket}
	cases := []struct {
		name     string
		market   float64
		decision *entryDecision
		split    bool
		submit   bool
		maxQty   float64
	}{
		{"through_stop", 89, nil, false, false, 0},
		{"beyond_threshold", 103, adverse, false, false, 0},
		{"split_reference_crossed", 99.5, nil, true, false, 0},
		{"repriced_within_budget", 100.5, adverse, false, true, 20 / 10.5},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			x, v, st := convergenceExecutor(t)
			p := singleLegPlan()
			p.EntryPrice = 100
			q2 := 0.0
			if c.split {
				p.SplitReference, q2 = 100, .5
			}
			ctx := plannedEntry(t, x, st, p, c.decision, 2, q2)
			v.market = c.market
			_ = x.reconcileManagedEntry("deferred", ctx)
			rows, _ := x.entryOrders(ctx)
			calls := entryCalls(v.managedVenue)
			if !c.submit {
				if len(calls) != 0 || rows[0].Status != "ABANDONED" || ctx.State != string(StateCancelled) {
					t.Fatalf("invalid deferred entry not abandoned: calls=%d status=%s state=%s", len(calls), rows[0].Status, ctx.State)
				}
				return
			}
			if len(calls) != 1 || calls[0].Quantity > c.maxQty+1e-9 {
				t.Fatalf("repriced entry exceeds risk budget: %+v", calls)
			}
		})
	}
}

func TestUpdateSLSingleFlatReadDoesNotClose(t *testing.T) {
	x, v, st := convergenceExecutor(t)
	ctx := newTestContext(t, st, StateOpen)
	v.qty = 0
	skip, err := x.ExecuteUpdateSL("stop", "stop", ctx, 96)
	if err != nil || skip != SkipNoPosition {
		t.Fatalf("got %s %v", skip, err)
	}
	stored, _ := st.CopyTrade().GetContext(ctx.ID)
	if stored.State != string(StateOpen) || stored.ClosedAt != nil {
		t.Fatalf("one flat read closed the trade: %+v", stored)
	}
}

// orderedLegacyVenue records whether protections were removed before the close.
type orderedLegacyVenue struct {
	*mockExchange
	seq       []string
	posErr    error
	staysOpen bool
}

func (m *orderedLegacyVenue) GetPositions() ([]map[string]interface{}, error) {
	if m.posErr != nil {
		return nil, m.posErr
	}
	return m.positions, nil
}
func (m *orderedLegacyVenue) CloseLong(symbol string, q float64) (map[string]interface{}, error) {
	m.seq = append(m.seq, "close")
	r, err := m.mockExchange.CloseLong(symbol, q)
	if err == nil && !m.staysOpen {
		m.positions = nil
	}
	return r, err
}
func (m *orderedLegacyVenue) CancelAllOrders(symbol string) error {
	m.seq = append(m.seq, "cancel")
	return m.mockExchange.CancelAllOrders(symbol)
}

func legacyCloseFixture(t *testing.T) (*Engine, *orderedLegacyVenue, *store.CopyTradeContext) {
	t.Helper()
	st := newTestStore(t)
	v := &orderedLegacyVenue{mockExchange: &mockExchange{positions: []map[string]interface{}{{"symbol": "BTCUSDT", "side": "long", "positionAmt": .5, "entryPrice": 100.0}}}}
	e := newReconcileEngine(st, v)
	return e, v, newTestContext(t, st, StateOpen)
}

func TestLegacyFullCloseClosesBeforeCleaningProtections(t *testing.T) {
	e, v, ctx := legacyCloseFixture(t)
	if _, err := e.exec.ExecuteClose("close", "close", ctx, 100); err != nil {
		t.Fatal(err)
	}
	if strings.Join(v.seq, ",") != "close,cancel" || ctx.State != string(StateClosed) {
		t.Fatalf("order %v state %s", v.seq, ctx.State)
	}

	e, v, ctx = legacyCloseFixture(t)
	v.staysOpen = true
	if _, err := e.exec.ExecuteClose("close", "close", ctx, 100); err != nil {
		t.Fatal(err)
	}
	if strings.Join(v.seq, ",") != "close" || ctx.State != string(StateClosePending) {
		t.Fatalf("protections removed before flat confirmation: %v %s", v.seq, ctx.State)
	}
}

func TestLegacyExitRetriesOnlyWhenProvablyUnsent(t *testing.T) {
	e, v, ctx := legacyCloseFixture(t)
	payload, _ := json.Marshal(SourceInterpretation{Action: ActionClose, CloseMode: CloseModeFull})
	action := &store.CopyTradeAction{ID: "close-action", TraderID: "trader-1", ContextID: ctx.ID, SignalID: "close", Action: "CLOSE", Status: "executing", PayloadJSON: string(payload)}
	if _, err := e.st.CopyTrade().ClaimAction(action); err != nil {
		t.Fatal(err)
	}
	v.posErr = errors.New("positions endpoint 502")
	if _, err := e.exec.ExecuteClose("close", "close", ctx, 100); !errors.Is(err, errExitNotSubmitted) {
		t.Fatalf("pre-send failure must be retryable: %v", err)
	}
	if v.closeCalled != 0 || !e.exec.legacyExitRetryable(ctx, "close") {
		t.Fatal("pre-send failure not recorded as unsent")
	}
	v.posErr = nil
	e.recoverPendingActions(ctx)
	saved, _ := e.st.CopyTrade().GetAction(action.ID)
	if v.closeCalled != 1 || saved.Status != "done" || ctx.State != string(StateClosed) {
		t.Fatalf("retry did not close exactly once: calls=%d action=%s state=%s", v.closeCalled, saved.Status, ctx.State)
	}
	e.recoverPendingActions(ctx)
	if _, err := e.exec.ExecuteClose("close", "close", ctx, 100); err != nil {
		t.Fatal(err)
	}
	if v.closeCalled != 1 {
		t.Fatal("sent exit repeated")
	}
}

func TestLegacyExitWithUnknownSendOutcomeNeverRetries(t *testing.T) {
	e, v, ctx := legacyCloseFixture(t)
	payload, _ := json.Marshal(SourceInterpretation{Action: ActionClose, CloseMode: CloseModeFull})
	action := &store.CopyTradeAction{ID: "close-action", TraderID: "trader-1", ContextID: ctx.ID, SignalID: "close", Action: "CLOSE", Status: "uncertain", PayloadJSON: string(payload)}
	if _, err := e.st.CopyTrade().ClaimAction(action); err != nil {
		t.Fatal(err)
	}
	v.closeLongErr = errors.New("read timeout after send")
	if _, err := e.exec.ExecuteClose("close", "close", ctx, 100); err == nil || errors.Is(err, errExitNotSubmitted) {
		t.Fatalf("ambiguous send must not be retryable: %v", err)
	}
	v.closeLongErr = nil
	for i := 0; i < 3; i++ {
		e.recoverPendingActions(ctx)
	}
	if v.closeCalled != 1 {
		t.Fatalf("ambiguous exit resent %d times", v.closeCalled-1)
	}
	if strings.Contains(strings.Join(v.seq, ","), "cancel") {
		t.Fatal("protections removed although the close outcome is unknown")
	}
}

func TestValidateExchangeAllowsOnlyBinanceAndOKX(t *testing.T) {
	cfg := DefaultCopyTradingConfig()
	cfg.PrimaryChannelID = "123"
	for _, ok := range []string{"binance", "okx", "Binance", "OKX"} {
		if err := cfg.ValidateExchange(ok); err != nil {
			t.Fatalf("%s rejected: %v", ok, err)
		}
	}
	for _, bad := range []string{"bybit", "gate", "hyperliquid", "bitget", ""} {
		err := cfg.ValidateExchange(bad)
		if err == nil || !strings.Contains(err.Error(), "Binance") || !strings.Contains(err.Error(), "OKX") {
			t.Fatalf("%q must be rejected with a clear reason: %v", bad, err)
		}
	}
}
