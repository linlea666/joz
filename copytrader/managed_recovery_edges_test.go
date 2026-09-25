package copytrader

import (
	"encoding/json"
	"math"
	"testing"
	"time"

	"nofx/store"
	"nofx/trader/types"
)

func TestRecoveredFirstLegExitPreventsSecondLeg(t *testing.T) {
	for _, tpFill := range []bool{true, false} {
		name := "manual_reduction"
		if tpFill {
			name = "tp_fill"
		}
		t.Run(name, func(t *testing.T) {
			x, v, st := managedExecutor(t)
			p := splitPlan()
			q1, q2, err := splitQuantities(p, &v.rules)
			if err != nil {
				t.Fatal(err)
			}
			b, err := json.Marshal(managedEntryPlan{Version: 1, RiskBudget: p.RiskBudget, StopLoss: p.StopLoss, MaxNotional: p.MaxNotional, MarginBudget: p.AvailableMargin * .9, SignalExpiresAt: p.SignalExpiresAt, Rules: v.rules})
			if err != nil {
				t.Fatal(err)
			}
			// The plan was committed, but the process stopped before first submit.
			ctx := &store.CopyTradeContext{ID: "interrupted-plan", TraderID: "trader-1", ChannelID: p.ChannelID, RootMessageID: p.RootMsgID, Symbol: p.Symbol, Direction: string(p.Direction), State: string(StateEntryPending), ExecutionVersion: 1, EntryPolicy: EntryPolicySplit, EntryPlanJSON: string(b), StopLossPrice: p.StopLoss, Leverage: p.Leverage, TPRecipeJSON: planRecipeJSON(p), EntryWorking: true}
			first := x.newManagedOrder(ctx, "signal", "ENTRY_1", "MARKET", p.EntryPrice, q1)
			second := x.newManagedOrder(ctx, "signal", "ENTRY_2", "LIMIT", p.SplitReference, q2)
			if err = st.CopyTrade().CreateManagedPlan(ctx, []*store.CopyTradeOrder{first, second}); err != nil {
				t.Fatal(err)
			}
			v.onSubmit = func(o *types.ManagedOrderResult) {
				if o.Type != "LIMIT" || o.Side != "SELL" {
					return
				}
				v.onSubmit = nil
				if tpFill {
					v.fill(o, o.Quantity/2, o.Price)
				} else {
					v.qty /= 2
				}
			}
			if err = x.reconcileManagedEntry("restart", ctx); err != nil {
				t.Fatal(err)
			}
			if len(entryCalls(v)) != 1 || !ctx.EntryDisabled {
				t.Fatal("recovery added exposure after the first leg had already reduced")
			}
			rows, _ := x.entryOrders(ctx)
			if rows[1].Status != "ABANDONED" || confirmedTPLevel(ctx, 1) != tpFill {
				t.Fatal("second leg remained eligible or manual reduction fabricated TP evidence")
			}
			remaining := 0.0
			for _, tp := range readTPPlan(ctx) {
				if !tp.Filled {
					remaining += tp.Quantity - tp.FilledQuantity
				}
			}
			if remaining > v.qty+1e-9 || remaining <= 0 {
				t.Fatalf("remaining TP quantity %g vs position %g", remaining, v.qty)
			}
			if err = x.reconcileManagedEntry("next-cycle", ctx); err != nil {
				t.Fatal(err)
			}
			if len(entryCalls(v)) != 1 {
				t.Fatal("disabled second leg reappeared on the next cycle")
			}
		})
	}
}

func TestRiskTrimHonorsMarketStepAndMinimum(t *testing.T) {
	x, v, _ := managedExecutor(t)
	v.rules.MarketQuantityStep, v.rules.MarketMinQuantity = .01, .02
	v.market = 112.3 // first fill: .9 * 22.3 = 20.07 risk; excess is below one market lot
	p := splitPlan()
	p.TPPrices, p.TPRatios = nil, nil
	ctx, err := x.ExecuteOpen("risk", "signal", p)
	if err != nil {
		t.Fatal(err)
	}
	if v.qty*22.3 > p.RiskBudget+1e-8 || !ctx.EntryDisabled {
		t.Fatalf("excess risk remained: qty=%g", v.qty)
	}
	var reduction float64
	for _, call := range v.calls {
		if call.ReduceOnly {
			reduction += call.Quantity
		}
	}
	if math.Abs(reduction-.02) > 1e-10 {
		t.Fatalf("expected minimum-size protective reduction, got %g", reduction)
	}
}

func TestEntryStopResolvesAfterCancelRace(t *testing.T) {
	x, v, _ := managedExecutor(t)
	ctx, err := x.ExecuteOpen("open", "signal", splitPlan())
	if err != nil {
		t.Fatal(err)
	}
	before := ctx.AvgFillPrice
	v.cancelFill = .4
	if _, err = x.ExecuteUpdateSLSpec("cost", "cost-signal", ctx, PriceSpec{Type: PriceEntry}); err != nil {
		t.Fatal(err)
	}
	if ctx.AvgFillPrice == before || math.Abs(ctx.StopLossPrice-v.avg) > 1e-9 || math.Abs(ctx.Quantity-v.qty) > 1e-9 {
		t.Fatalf("stop used pre-cancel position: stop=%g avg=%g qty=%g/%g", ctx.StopLossPrice, v.avg, ctx.Quantity, v.qty)
	}
	if !ctx.EntryDisabled || !ctx.BreakevenApplied {
		t.Fatal("cost stop did not permanently disable additions")
	}
}

func TestFullCloseWaitsForPositionAndFinalizesJournal(t *testing.T) {
	x, v, st := managedExecutor(t)
	ctx, err := x.ExecuteOpen("open", "signal", splitPlan())
	if err != nil {
		t.Fatal(err)
	}
	action := &store.CopyTradeAction{ID: "exit-action", TraderID: "trader-1", ContextID: ctx.ID, SignalID: "exit", Action: "CLOSE", Status: "executing"}
	if _, err = st.CopyTrade().ClaimAction(action); err != nil {
		t.Fatal(err)
	}
	v.reportedQty = v.qty
	if _, err = x.ExecuteClose("exit", "exit", ctx, 100); err == nil {
		t.Fatal("stale visible position prematurely marked closed")
	}
	if ctx.ClosePendingJSON == "" || ctx.State != "CLOSE_PENDING" {
		t.Fatalf("lost pending exit: %+v", ctx)
	}
	count := len(v.calls)
	v.reportedQty = 0
	restored, _ := st.CopyTrade().GetContext(ctx.ID)
	if err = x.reconcileManagedClose("restart", restored); err != nil {
		t.Fatal(err)
	}
	saved, _ := st.CopyTrade().GetAction(action.ID)
	if len(v.calls) != count || restored.State != "CLOSED" || saved.Status != "done" {
		t.Fatal("full close replayed or journal not finalized")
	}
}

func TestDefinitelyRejectedSecondLegNeverReopens(t *testing.T) {
	x, v, _ := managedExecutor(t)
	v.rejectLimit, v.definiteReject = true, true
	ctx, err := x.ExecuteOpen("open", "signal", splitPlan())
	if err == nil {
		t.Fatal("rejection missing")
	}
	if err = x.reconcileManagedEntry("recover", ctx); err != nil {
		t.Fatal(err)
	}
	rows, _ := x.entryOrders(ctx)
	if !ctx.EntryDisabled || rows[1].Status != "REJECTED" || len(entryCalls(v)) != 2 || v.qty <= 0 || len(v.openOrders) == 0 {
		t.Fatal("rejected add was retried or first leg lost protection")
	}
}

func TestProtectionWriteConflictPreventsNewOrders(t *testing.T) {
	x, v, st := managedExecutor(t)
	ctx, err := x.ExecuteOpen("open", "signal", splitPlan())
	if err != nil {
		t.Fatal(err)
	}
	stale := *ctx
	if err = x.persistContext(ctx, map[string]interface{}{"last_action": "newer"}); err != nil {
		t.Fatal(err)
	}
	count := len(v.calls)
	if err = x.restoreRemainingProtections("stale", "", &stale, v.qty/2); err == nil {
		t.Fatal("stale TP repair submitted after persistence conflict")
	}
	if len(v.calls) != count {
		t.Fatal("persistence failure still created orders")
	}
	stored, _ := st.CopyTrade().GetContext(ctx.ID)
	if stored.LastAction != "newer" {
		t.Fatal("new state was overwritten")
	}
}

func TestLegacyMultiActionCannotRepeatAfterUpgrade(t *testing.T) {
	st := newTestStore(t)
	interp, _ := json.Marshal(map[string]interface{}{"instructions": []map[string]string{{"action": "REDUCE", "symbol": "BTC/USDT"}, {"action": "UPDATE_SL", "symbol": "ETH"}}})
	sig := &store.CopyTradeSignal{ID: "legacy", TraderID: "trader-1", MessageID: "old", Status: store.SignalStatusExecuted, InterpretationJSON: string(interp)}
	if err := st.CopyTrade().CreateSignal(sig); err != nil {
		t.Fatal(err)
	}
	if done, err := st.CopyTrade().LegacyExecutedAction("trader-1", "old", "BTCUSDT", "REDUCE"); err != nil || !done {
		t.Fatalf("legacy executed child lost: %v %v", done, err)
	}
}

func TestOptInPoliciesValidateWithoutChangingLegacyDefaults(t *testing.T) {
	cfg, err := ParseCopyTradingConfig(`{"primary_channel_id":"123","risk_amount_usd":15,"altcoin_price_offset_pct":0.2,"auto_breakeven_after_tp":true}`)
	if err != nil || cfg.ValidateExchange("okx") != nil {
		t.Fatal("legacy config no longer accepted")
	}
	if cfg.EntryPolicy != "" || cfg.InterpretationProfile != "" || cfg.RiskAmountUSD != 15 || cfg.AltcoinPriceOffsetPct != .2 || !cfg.AutoBreakevenAfterTP {
		t.Fatal("legacy preferences changed")
	}
	cfg.EntryPolicy = EntryPolicySplit
	if cfg.ValidateExchange("okx") == nil || cfg.ValidateExchange("binance") != nil {
		t.Fatal("split exchange capability invalid")
	}
	cfg.RiskMode = RiskModeFixed
	if cfg.ValidateExchange("binance") == nil {
		t.Fatal("split allowed fixed-margin sizing")
	}
}

func TestNewLegacyLimitKeepsRatiosSeparateFromFilledQuantities(t *testing.T) {
	st := newTestStore(t)
	venue := &mockExchange{}
	x := NewExecutor("trader-1", venue, st, NewEventLogger(st, "trader-1", "chan-1"))
	p := splitPlan()
	p.SplitReference, p.EntryType, p.Quantity = 0, EntryPlanLimit, .8
	ctx, err := x.ExecuteOpen("open", "signal", p)
	if err != nil {
		t.Fatal(err)
	}
	if len(readTPPlan(ctx)) != 0 || ctx.TPRecipeJSON == "" {
		t.Fatal("unfilled order mixed TP ratios with quantities")
	}
	if err = x.settleLegacyEntry("partial", ctx, map[string]interface{}{"status": "PARTIALLY_FILLED", "executedQty": .2, "avgPrice": 100.0}); err != nil {
		t.Fatal(err)
	}
	plan := readTPPlan(ctx)
	if len(plan) != 2 || math.Abs(plan[0].Quantity-.1) > 1e-9 || math.Abs(plan[1].Quantity-.1) > 1e-9 {
		t.Fatalf("TP percentages became coin quantities: %+v", plan)
	}
}

func TestLegacyEntryRecoversNewManagedExitAfterLostAck(t *testing.T) {
	x, venue, st := managedExecutor(t)
	ctx := newTestContext(t, st, StateOpen)
	venue.qty, venue.avg = ctx.Quantity, ctx.AvgFillPrice
	venue.lostAck, venue.queryDown = true, true
	if _, err := x.ExecuteClose("reduce", "reduce-signal", ctx, 50); err == nil {
		t.Fatal("expected ambiguous acknowledgment")
	}
	if ctx.ExecutionVersion != 0 || ctx.ClosePendingJSON == "" {
		t.Fatal("legacy entry version changed or durable exit missing")
	}
	count := len(venue.calls)
	venue.queryDown = false
	e := newReconcileEngine(st, venue)
	e.reconcileOnce()
	restored, _ := st.CopyTrade().GetContext(ctx.ID)
	if restored.State != "OPEN" || restored.ClosePendingJSON != "" || len(venue.calls) != count {
		t.Fatalf("legacy managed exit not settled: %+v", restored)
	}
}

func TestAuthorTP2RuleSurvivesInterruptedOpen(t *testing.T) {
	x, venue, st := managedExecutor(t)
	venue.lostAck, venue.queryDown = true, true
	p := splitPlan()
	p.BreakevenTPLevel = 2
	ctx, err := x.ExecuteOpen("open", "signal", p)
	if err == nil || ctx == nil {
		t.Fatal("expected uncertain first submit")
	}
	stored, _ := st.CopyTrade().GetContext(ctx.ID)
	if !stored.BreakevenAfterTP || stored.BreakevenTPLevel != 2 {
		t.Fatal("open failure lost author TP2 condition")
	}
}

func TestDeferredRetryCannotExtendEntryLifetime(t *testing.T) {
	f := marketReferenceFixtures(t)[0]
	e, _, model, msg := fixtureEngine(t, f, 1)
	msg.MessageTimestamp = time.Now().Add(-6 * time.Minute)
	due := time.Now().Add(-time.Second)
	sig := &store.CopyTradeSignal{ID: "retry-expired", TraderID: e.traderID, ChannelID: msg.ChannelID, MessageID: msg.MessageID, Status: "retry_wait", ExecutionVersion: 1, RetryCount: 1, NextRetryAt: &due}
	if err := e.st.CopyTrade().CreateSignal(sig); err != nil {
		t.Fatal(err)
	}
	if err := e.HandleMessage(msg, false); err != nil {
		t.Fatal(err)
	}
	stored, _ := e.st.CopyTrade().GetSignal(sig.ID)
	if model.calls != 0 || stored.SkipReason != string(SkipExpired) {
		t.Fatal("deferred entry called model beyond entry TTL")
	}
}

func TestShortSplitAndTPPreserveTighterStop(t *testing.T) {
	x, v, st := managedExecutor(t)
	p := splitPlan()
	p.Direction, p.EntryPrice, p.StopLoss, p.TPPrices = DirectionShort, 99, 110, []float64{90, 80}
	v.market = 99
	ctx, err := x.ExecuteOpen("short", "signal", p)
	if err != nil {
		t.Fatal(err)
	}
	entries, _ := x.entryOrders(ctx)
	if entryCalls(v)[0].Side != "SELL" || entryCalls(v)[1].PositionSide != "SHORT" {
		t.Fatal("short legs have wrong side")
	}
	second := v.orders[entries[1].ClientID]
	v.fill(second, second.Quantity, 100)
	if err = x.reconcileManagedEntry("second", ctx); err != nil {
		t.Fatal(err)
	}
	if ctx.Quantity*(110-ctx.AvgFillPrice) > 20+1e-8 {
		t.Fatal("short split exceeded risk budget")
	}
	firstTP := readTPPlan(ctx)[0]
	for _, order := range v.orders {
		if order.OrderID == firstTP.OrderID {
			if order.Side != "BUY" || order.PositionSide != "SHORT" {
				t.Fatal("short TP has wrong side")
			}
			v.fill(order, order.Quantity, 90)
		}
	}
	v.openOrders[0].StopPrice = 95
	e := newReconcileEngine(st, v)
	e.cfg.AutoBreakevenAfterTP = true
	e.reconcileOpenTrade(ctx)
	if !ctx.EntryDisabled || !ctx.BreakevenApplied || ctx.StopLossPrice != 95 {
		t.Fatalf("short TP/stop reconciliation failed: %+v", ctx)
	}
}
