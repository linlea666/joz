package copytrader

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

// --- 1. max_position_notional_usd: one meaning everywhere -------------------

func TestNotionalCapZeroRejectedAndLegacyNormalized(t *testing.T) {
	cfg := DefaultCopyTradingConfig()
	cfg.PrimaryChannelID = "123"
	cfg.MaxPositionNotionalUSD = 0
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "max_position_notional_usd") || !strings.Contains(err.Error(), "上限") {
		t.Fatalf("zero cap must be rejected with a bilingual message: %v", err)
	}
	cfg.MaxPositionNotionalUSD = -1
	if cfg.Validate() == nil {
		t.Fatal("negative cap accepted")
	}
	for _, raw := range []string{`{"max_position_notional_usd":0,"primary_channel_id":"123"}`, `{"primary_channel_id":"123"}`, ``} {
		parsed, err := ParseCopyTradingConfig(raw)
		if err != nil {
			t.Fatal(err)
		}
		if parsed.MaxPositionNotionalUSD != DefaultMaxPositionNotionalUSD || parsed.NotionalCap() != DefaultMaxPositionNotionalUSD {
			t.Fatalf("legacy %q not normalized: %v", raw, parsed.MaxPositionNotionalUSD)
		}
		if raw != "" && parsed.Validate() != nil {
			t.Fatalf("normalized legacy config must start: %v", parsed.Validate())
		}
	}
	parsed, _ := ParseCopyTradingConfig(`{"max_position_notional_usd":5000}`)
	if parsed.NotionalCap() != 5000 {
		t.Fatal("explicit cap changed")
	}
}

func TestNotionalCapZeroIsDefaultInEverySizingPath(t *testing.T) {
	// Single-leg sizing: 0 is the default cap, never "unlimited".
	res, err := ComputePositionSize(SizingInput{RiskMode: RiskModeByLoss, RiskAmountUSD: 1000, EntryPrice: 100, StopLossPrice: 99, Leverage: 100, AvailableMarginUSD: 1e9})
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(res.NotionalUSD-DefaultMaxPositionNotionalUSD) > 1e-6 || len(res.AppliedConstraints) == 0 || res.AppliedConstraints[0] != "max_position_notional" {
		t.Fatalf("zero cap not defaulted: %+v", res)
	}
	// Split sizing: identical interpretation.
	rules := newManagedVenue().rules
	p := splitPlan()
	p.RiskBudget, p.AvailableMargin, p.Leverage = 100000, 1e9, 100
	p.MaxNotional = 0
	q1, q2, err := splitQuantities(p, &rules)
	if err != nil {
		t.Fatal(err)
	}
	if n := q1*p.EntryPrice + q2*p.SplitReference; n > DefaultMaxPositionNotionalUSD+1e-6 || n < DefaultMaxPositionNotionalUSD*0.99 {
		t.Fatalf("split notional %v not capped at default", n)
	}
	if EffectiveNotionalCap(0) != DefaultMaxPositionNotionalUSD || EffectiveNotionalCap(math.NaN()) != DefaultMaxPositionNotionalUSD || EffectiveNotionalCap(7) != 7 {
		t.Fatal("EffectiveNotionalCap")
	}
}

// --- 2. default_tp_ratios ---------------------------------------------------

func TestDefaultTPRatiosPerLevelCountAndRunner(t *testing.T) {
	for _, tc := range []struct {
		name     string
		levels   int
		defaults []float64
		want     []float64
		source   string
	}{
		{"two levels use two configured values", 2, []float64{60, 40}, []float64{60, 40}, TPAllocConfigured},
		{"two configured values below 100 keep runner", 2, []float64{40, 30}, []float64{40, 30}, TPAllocConfigured},
		{"three configured values below 100 keep runner", 3, []float64{40, 30, 20}, []float64{40, 30, 20}, TPAllocConfigured},
		{"one level one value keeps runner", 1, []float64{70}, []float64{70}, TPAllocConfigured},
		{"two levels with three defaults fall back", 2, []float64{50, 30, 20}, []float64{50, 50}, TPAllocBuiltinFallback},
		{"three levels with two defaults fall back", 3, []float64{60, 40}, []float64{50, 30, 20}, TPAllocBuiltinFallback},
		{"single level with three defaults is 100", 1, []float64{50, 30, 20}, []float64{100}, TPAllocBuiltinFallback},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, source, err := AllocateTPRatiosDetailed(make([]TPLevel, tc.levels), tc.defaults)
			if err != nil || source != tc.source || !floatSliceEq(got, tc.want) {
				t.Fatalf("got %v %s %v, want %v %s", got, source, err, tc.want, tc.source)
			}
		})
	}
	if _, source, _ := AllocateTPRatiosDetailed([]TPLevel{{Ratio: ratio(30)}, {}}, []float64{60, 40}); source != TPAllocMixed {
		t.Fatal("explicit ratio must win over configured defaults")
	}
	// The runner survives into the quantity split.
	q, err := SplitTPQuantities(10, []float64{40, 30, 20}, .001, .001)
	if err != nil || math.Abs(q[0]+q[1]+q[2]-9) > 1e-9 {
		t.Fatalf("runner lost: %v %v", q, err)
	}
}

// --- 3. reduction scaling keeps the runner ----------------------------------

func stampedPlan(qtys ...float64) []TPPlanEntry {
	plan := make([]TPPlanEntry, len(qtys))
	for i, q := range qtys {
		plan[i] = TPPlanEntry{Ordinal: i + 1, Quantity: q, Price: float64(110 + i)}
	}
	stampTPSync(plan)
	return plan
}

func TestTPRescaleKeepsRunnerProportional(t *testing.T) {
	// Position 10, TPs 5+3, runner 2.
	plan := stampedPlan(5, 3)
	if s := tpRescale(10, 5, 8, plan); math.Abs(s-.5) > 1e-12 {
		t.Fatalf("external halving must scale TPs and runner by .5, got %v", s)
	}
	// TP1 fully filled (position 10 -> 5): remaining TP2 must NOT shrink.
	plan[0].Filled, plan[0].FilledQuantity = true, 5
	if s := tpRescale(10, 5, 3, plan); math.Abs(s-1) > 1e-12 {
		t.Fatalf("TP fill must not rescale remaining levels, got %v", s)
	}
	// TP1 fill plus an external halving of the rest (5 -> 2.5).
	if s := tpRescale(10, 2.5, 3, plan); math.Abs(s-.5) > 1e-12 {
		t.Fatalf("got %v", s)
	}
	// Guard: never more reduce-only TP quantity than position.
	full := stampedPlan(6, 4) // no runner
	if s := tpRescale(10, 20, 10, full); math.Abs(s-2) > 1e-12 {
		t.Fatalf("growth scale %v", s)
	}
	if s := tpRescale(10, 5, 10, full); s*10 > 5+1e-12 {
		t.Fatalf("scaled TPs exceed position: %v", s)
	}
	// Unstamped (older) plans keep the previous behaviour.
	legacy := []TPPlanEntry{{Quantity: 5}, {Quantity: 3}}
	if s := tpRescale(10, 5, 8, legacy); math.Abs(s-5.0/8) > 1e-12 {
		t.Fatalf("legacy fallback changed: %v", s)
	}
}

func TestExternalReductionScalesTPLadderAndKeepsRunner(t *testing.T) {
	x, v, st := managedExecutor(t)
	p := splitPlan()
	p.SplitReference, p.RiskBudget, p.Quantity = 0, 0, 1
	p.TPRatios = []float64{50, 30} // 20% runner
	ctx, err := x.ExecuteOpen("open", "s-open", p)
	if err != nil {
		t.Fatal(err)
	}
	ctx, _ = st.CopyTrade().GetContext(ctx.ID)
	if ctx.TPPositionQuantity != 1 {
		t.Fatalf("ladder not synced: %v", ctx.TPPositionQuantity)
	}
	for _, tp := range readTPPlan(ctx) {
		if tp.SyncedFill == nil {
			t.Fatal("sync stamp missing")
		}
	}
	v.qty = .5 // manual / author reduction outside the TP ladder
	if err = x.restoreRemainingProtections("reduce", "", ctx, .5); err != nil {
		t.Fatal(err)
	}
	total := 0.0
	for _, tp := range readTPPlan(ctx) {
		if !tp.Filled && tp.ReplacedBy == "" {
			total += tp.Quantity - tp.FilledQuantity
		}
	}
	if math.Abs(total-.4) > 1e-9 {
		t.Fatalf("TP ladder after halving = %v, want .4 (runner .1 kept)", total)
	}
}

// --- 4. final limit stop side and second-leg distance floor -----------------

func routeTestEngine(t *testing.T, policy string, offset float64) (*Engine, *managedVenue) {
	x, v, st := managedExecutor(t)
	cfg := DefaultCopyTradingConfig()
	cfg.PrimaryChannelID = "chan-1"
	cfg.EntryPolicy = policy
	cfg.MajorPriceOffsetPct = offset
	cfg.RiskAmountUSD = 10
	return &Engine{traderID: "trader-1", cfg: &cfg, st: st, exec: x, events: x.events}, v
}

func TestFinalLimitEntryStopSideRejected(t *testing.T) {
	for _, tc := range []struct {
		name       string
		dir        Direction
		ref, sl    float64
		market     float64
		wantSkip   SkipReason
		wantOrders int
	}{
		// Adverse beyond threshold -> limit at reference 100; the stop is
		// below the live market but above the final limit entry.
		{"long stop above limit", DirectionLong, 100, 102, 105, SkipRiskRejected, 0},
		{"long stop below limit", DirectionLong, 100, 95, 105, SkipNone, 1},
		{"short stop below limit", DirectionShort, 100, 98, 95, SkipRiskRejected, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, v := routeTestEngine(t, EntryPolicyLegacy, 1)
			v.market = tc.market
			ins := &SourceInterpretation{Action: ActionOpen, Symbol: "BTC", Direction: tc.dir,
				EntryOrders:    []EntryOrder{{OrderType: EntryMarket, Price: PriceSpec{Type: PriceMarket, Price: tc.ref}}},
				StopLossLevels: []SLLevel{{Price: PriceSpec{Type: PriceFixed, Price: tc.sl}}}}
			skip, err := e.routeOpen("trace", "signal", &store.DiscordMessage{MessageID: "m", ChannelID: "chan-1", MessageTimestamp: time.Now()}, ins, "BTCUSDT", tc.market)
			if err != nil || skip != tc.wantSkip || len(entryCalls(v)) != tc.wantOrders {
				t.Fatalf("skip=%s err=%v entries=%d", skip, err, len(entryCalls(v)))
			}
			if tc.wantOrders == 1 && entryCalls(v)[0].Type != "LIMIT" {
				t.Fatal("expected a limit at the reference")
			}
		})
	}
}

func TestSecondLegStopDistanceFloorRejectsCombination(t *testing.T) {
	e, v := routeTestEngine(t, EntryPolicySplit, 2)
	ins := &SourceInterpretation{Action: ActionOpen, Symbol: "BTC", Direction: DirectionLong,
		EntryOrders:    []EntryOrder{{OrderType: EntryMarket, Price: PriceSpec{Type: PriceMarket, Price: 100}}, {OrderType: EntryLimit, Price: PriceSpec{Type: PriceFixed, Price: 90.03}}},
		StopLossLevels: []SLLevel{{Price: PriceSpec{Type: PriceFixed, Price: 90}}}}
	skip, err := e.routeOpen("trace", "signal", &store.DiscordMessage{MessageID: "m", ChannelID: "chan-1", MessageTimestamp: time.Now()}, ins, "BTCUSDT", 101)
	if err != nil || skip != SkipRiskRejected || len(v.calls) != 0 {
		t.Fatalf("skip=%s err=%v calls=%d", skip, err, len(v.calls))
	}
	rules := newManagedVenue().rules
	p := splitPlan()
	p.SplitReference = 90.03
	if _, _, err := splitQuantities(p, &rules); err == nil || !strings.Contains(err.Error(), "sizing floor") {
		t.Fatalf("executor must reject too-close second leg: %v", err)
	}
}

// --- 5. events, exit evidence, recovery outcomes ---------------------------

func TestManagedLegEventNamesByRole(t *testing.T) {
	for role, want := range map[string]string{
		"ENTRY_1": EvEntrySubmitted, "ENTRY_2": EvEntrySubmitted,
		"TP_1_0": EvTPUpdated, "CLOSE_sig-1": EvExitUpdated, "CLOSE_emergency-ctx": EvExitUpdated,
		"CLOSE_risk-abc": EvRiskUpdated,
	} {
		if got := managedLegEvent(role); got != want {
			t.Fatalf("%s -> %s, want %s", role, got, want)
		}
	}
	x, _, st := managedExecutor(t)
	if _, err := x.ExecuteOpen("open", "s-open", splitPlan()); err != nil {
		t.Fatal(err)
	}
	var tpEvents, entryEvents []store.CopyTradeEvent
	st.GormDB().Where("event = ?", EvTPUpdated).Find(&tpEvents)
	st.GormDB().Where("event = ?", EvEntrySubmitted).Find(&entryEvents)
	if len(tpEvents) == 0 || len(entryEvents) == 0 {
		t.Fatalf("tp=%d entry=%d", len(tpEvents), len(entryEvents))
	}
	for _, ev := range entryEvents {
		if strings.Contains(ev.Message, " TP_") {
			t.Fatal("TP leg logged as entry")
		}
	}
}

func TestMarketFillRiskVersusPlan(t *testing.T) {
	for _, tc := range []struct {
		name   string
		market float64
		level  string
	}{{"on plan", 101, "info"}, {"slipped", 105, "warn"}} {
		t.Run(tc.name, func(t *testing.T) {
			x, v, st := managedExecutor(t)
			v.market = tc.market
			p := splitPlan()
			p.SplitReference, p.RiskBudget = 0, 0
			if _, err := x.ExecuteOpen("open", "s-open", p); err != nil {
				t.Fatal(err)
			}
			var events []store.CopyTradeEvent
			st.GormDB().Where("event = ? AND message LIKE ?", EvQuantityPlan, "%market entry filled%").Find(&events)
			if len(events) != 1 || events[0].Level != tc.level {
				t.Fatalf("events %+v", events)
			}
			var data map[string]interface{}
			_ = json.Unmarshal([]byte(events[0].ContextJSON), &data)
			if data["planned_risk_usd"] == nil || data["actual_risk_usd"] == nil {
				t.Fatalf("risk evidence missing: %v", data)
			}
		})
	}
}

func TestEntryPlanEventDescribesActualPlan(t *testing.T) {
	x, _, st := managedExecutor(t)
	if _, err := x.ExecuteOpen("open", "s-open", splitPlan()); err != nil {
		t.Fatal(err)
	}
	var events []store.CopyTradeEvent
	st.GormDB().Where("event = ?", EvEntryDecision).Find(&events)
	if len(events) != 1 || !strings.Contains(events[0].Message, "2 legs") || strings.Contains(events[0].Message, "final entry plan; shared risk budget") {
		t.Fatalf("plan event %+v", events)
	}
	var data struct {
		RiskBudget float64 `json:"risk_budget"`
		Planned    float64 `json:"planned_risk_usd"`
	}
	_ = json.Unmarshal([]byte(events[0].ContextJSON), &data)
	if data.RiskBudget != 20 || data.Planned <= 0 || data.Planned > 20+1e-9 {
		t.Fatalf("budget fields %+v", data)
	}
}

type evidenceVenue struct {
	*managedVenue
	stop    *types.StopOrderResult
	stopErr error
	closed  []types.ClosedPnLRecord
}

func (v *evidenceVenue) GetStopOrder(symbol, orderID, clientID string) (*types.StopOrderResult, error) {
	return v.stop, v.stopErr
}
func (v *evidenceVenue) GetClosedPnL(time.Time, int) ([]types.ClosedPnLRecord, error) {
	return v.closed, nil
}

func evidenceFixture(t *testing.T, legs map[string]float64, order []string) (*Executor, *evidenceVenue, *store.CopyTradeContext) {
	t.Helper()
	st := newTestStore(t)
	v := &evidenceVenue{managedVenue: newManagedVenue()}
	x := NewExecutor("trader-1", v, st, NewEventLogger(st, "trader-1", "chan-1"))
	opened := time.Now().UTC().Add(-time.Hour)
	ctx := &store.CopyTradeContext{ID: "ctx-ev", TraderID: "trader-1", Symbol: "BTCUSDT", Direction: "LONG", State: string(StateOpen), ExecutionVersion: 1, EntryFilledQuantity: 1, Quantity: 1, AvgFillPrice: 100, StopLossPrice: 90, OpenedAt: &opened, StopOrderID: "777"}
	rows := []*store.CopyTradeOrder{x.newManagedOrder(ctx, "s", "ENTRY_1", "MARKET", 100, 1)}
	rows[0].Status, rows[0].ExecutedQty, rows[0].AvgPrice = "FILLED", 1, 100
	for _, role := range order {
		r := x.newManagedOrder(ctx, "s", role, "LIMIT", 110, legs[role])
		r.Status, r.ExecutedQty, r.AvgPrice = "FILLED", legs[role], 110
		rows = append(rows, r)
	}
	if err := st.CopyTrade().CreateManagedPlan(ctx, rows); err != nil {
		t.Fatal(err)
	}
	base := time.Now().UTC().Add(-30 * time.Minute)
	for i, r := range rows {
		if err := st.GormDB().Model(&store.CopyTradeOrder{}).Where("id = ?", r.ID).UpdateColumn("updated_at", base.Add(time.Duration(i)*time.Minute)).Error; err != nil {
			t.Fatal(err)
		}
	}
	return x, v, ctx
}

func TestExitEvidenceReasons(t *testing.T) {
	t.Run("stop filled", func(t *testing.T) {
		x, v, ctx := evidenceFixture(t, map[string]float64{"TP_1_0": .5}, []string{"TP_1_0"})
		v.stop = &types.StopOrderResult{OpenOrder: types.OpenOrder{OrderID: "777", Symbol: "BTCUSDT"}, Status: "FINISHED", ActualOrderID: "888", ActualOrderStatus: "FILLED"}
		if ev := x.collectExitEvidence("t", ctx, "777"); ev.Reason != ExitReasonStopLoss || ev.Source != "stop_order_history" {
			t.Fatalf("%+v", ev)
		}
	})
	t.Run("take profit ladder completed", func(t *testing.T) {
		x, v, ctx := evidenceFixture(t, map[string]float64{"TP_1_0": .5, "TP_2_0": .5}, []string{"TP_1_0", "TP_2_0"})
		v.stop = &types.StopOrderResult{OpenOrder: types.OpenOrder{OrderID: "777", Symbol: "BTCUSDT"}, Status: "CANCELED"}
		ev := x.collectExitEvidence("t", ctx, "777")
		if ev.Reason != ExitReasonTakeProfit || ev.ClosingRole != "TP_2_0" || ev.ClosedAt == nil || ev.ClosedAtSource != "ledger_fill_observed" {
			t.Fatalf("%+v", ev)
		}
	})
	t.Run("author close", func(t *testing.T) {
		x, _, ctx := evidenceFixture(t, map[string]float64{"TP_1_0": .5, "CLOSE_sig-2": .5}, []string{"TP_1_0", "CLOSE_sig-2"})
		if ev := x.collectExitEvidence("t", ctx, ""); ev.Reason != ExitReasonAuthor {
			t.Fatalf("%+v", ev)
		}
	})
	t.Run("unexplained exit is unknown", func(t *testing.T) {
		x, v, ctx := evidenceFixture(t, map[string]float64{"TP_1_0": .5}, []string{"TP_1_0"})
		v.stopErr = errors.New("history down")
		ev := x.collectExitEvidence("t", ctx, "777")
		if ev.Reason != ExitReasonUnknown || ev.StopStatus != "lookup_failed" || ev.ClosedAt == nil {
			t.Fatalf("%+v", ev)
		}
	})
	t.Run("realized pnl and venue close time", func(t *testing.T) {
		x, v, ctx := evidenceFixture(t, map[string]float64{"TP_1_0": .5}, []string{"TP_1_0"})
		closeAt := time.Now().UTC().Add(-5 * time.Minute).Truncate(time.Second)
		v.closed = []types.ClosedPnLRecord{
			{Symbol: "BTCUSDT", RealizedPnL: 5, ExitTime: closeAt.Add(-10 * time.Minute)},
			{Symbol: "BTCUSDT", RealizedPnL: -2, ExitTime: closeAt, CloseType: "stop_loss"},
			{Symbol: "ETHUSDT", RealizedPnL: 100, ExitTime: closeAt},
			{Symbol: "BTCUSDT", RealizedPnL: 50, ExitTime: closeAt.Add(-3 * time.Hour)}, // before open
		}
		ev := x.collectExitEvidence("t", ctx, "")
		if ev.RealizedPnL == nil || *ev.RealizedPnL != 3 || ev.ClosedAt == nil || !ev.ClosedAt.Equal(closeAt) || ev.Reason != ExitReasonStopLoss || ev.Source != "exchange_close_type" {
			t.Fatalf("%+v", ev)
		}
	})
}

func TestReconcileCloseRecordsExitReason(t *testing.T) {
	x, v, st := managedExecutor(t)
	p := splitPlan()
	p.SplitReference, p.RiskBudget = 0, 0
	ctx, err := x.ExecuteOpen("open", "s-open", p)
	if err != nil {
		t.Fatal(err)
	}
	// Both TP limits fill on the venue: the position is gone.
	for _, o := range v.orders {
		if o.Type == "LIMIT" && !orderTerminal(o.Status) {
			v.fill(o, o.Quantity-o.ExecutedQty, o.Price)
		}
	}
	if v.qty > 1e-9 {
		t.Fatalf("position remains %v", v.qty)
	}
	e := &Engine{traderID: "trader-1", cfg: &CopyTradingConfig{}, st: st, exec: x, events: x.events}
	e.reconcileOpenTrade(ctx)
	e.reconcileOpenTrade(ctx)
	saved, _ := st.CopyTrade().GetContext(ctx.ID)
	if saved.State != string(StateClosed) {
		t.Fatalf("state %s (%s)", saved.State, saved.LastError)
	}
	var events []store.CopyTradeEvent
	st.GormDB().Where("event = ?", EvTradeClosed).Find(&events)
	if len(events) != 1 || !strings.Contains(events[0].Message, ExitReasonTakeProfit) {
		t.Fatalf("closed events %+v", events)
	}
	var data struct {
		Exit exitEvidence `json:"exit"`
	}
	if err = json.Unmarshal([]byte(events[0].ContextJSON), &data); err != nil || data.Exit.Reason != ExitReasonTakeProfit || data.Exit.ClosedAt == nil {
		t.Fatalf("exit data %+v %v", data, err)
	}
}

func recoveryEngine(x *Executor, st *store.Store) *Engine {
	cfg := DefaultCopyTradingConfig()
	cfg.PrimaryChannelID = "chan-1"
	return &Engine{traderID: "trader-1", cfg: &cfg, st: st, exec: x, events: x.events}
}

func TestRecoveryDoneSyncsWaitingSignalOnly(t *testing.T) {
	for _, tc := range []struct{ initial, want string }{
		{"execution_wait", store.SignalStatusExecuted},
		{store.SignalStatusExecuting, store.SignalStatusExecuted},
		{store.SignalStatusFailed, store.SignalStatusFailed},
	} {
		t.Run(tc.initial, func(t *testing.T) {
			x, _, st := managedExecutor(t)
			ctx, err := x.ExecuteOpen("open", "s-open", splitPlan())
			if err != nil {
				t.Fatal(err)
			}
			if err = st.CopyTrade().CreateSignal(&store.CopyTradeSignal{ID: "s-open", TraderID: "trader-1", ChannelID: "chan-1", MessageID: "m-open", Status: tc.initial, SkipReason: string(SkipWaitingReconciliation)}); err != nil {
				t.Fatal(err)
			}
			if _, err = st.CopyTrade().ClaimAction(&store.CopyTradeAction{ID: "a-open", TraderID: "trader-1", SignalID: "s-open", MessageID: "m-open", ContextID: ctx.ID, Symbol: "BTCUSDT", Direction: "LONG", Action: "OPEN", Status: "executing", Phase: "waiting_admission", PayloadJSON: `{"action":"OPEN"}`}); err != nil {
				t.Fatal(err)
			}
			recoveryEngine(x, st).recoverPendingActions(ctx)
			a, _ := st.CopyTrade().GetAction("a-open")
			sig, _ := st.CopyTrade().GetSignal("s-open")
			if a.Status != "done" || sig.Status != tc.want {
				t.Fatalf("action %s signal %s", a.Status, sig.Status)
			}
			if tc.want == store.SignalStatusExecuted && sig.SkipReason != "" {
				t.Fatalf("stale skip reason %q", sig.SkipReason)
			}
		})
	}
}

func TestRevokedEntryActionSettledWhenTradeTerminal(t *testing.T) {
	x, _, st := managedExecutor(t)
	p := splitPlan()
	p.SplitReference = 0
	plan, _ := json.Marshal(managedEntryPlan{Version: 1, RiskBudget: p.RiskBudget, StopLoss: p.StopLoss, MaxNotional: p.MaxNotional, MarginBudget: p.AvailableMargin * .9, SignalExpiresAt: p.SignalExpiresAt, Rules: newManagedVenue().rules})
	ctx := &store.CopyTradeContext{ID: "revoked", TraderID: "trader-1", ChannelID: "chan-1", RootMessageID: "m-open", Symbol: p.Symbol, Direction: string(p.Direction), State: string(StateEntryPending), ExecutionVersion: 1, EntryPolicy: EntryPolicyLegacy, EntryPlanJSON: string(plan), StopLossPrice: p.StopLoss, Leverage: p.Leverage, TPRecipeJSON: planRecipeJSON(p), EntryWorking: true}
	first := x.newManagedOrder(ctx, "s-open", "ENTRY_1", "MARKET", p.EntryPrice, .5)
	if err := st.CopyTrade().CreateManagedPlan(ctx, []*store.CopyTradeOrder{first}); err != nil {
		t.Fatal(err)
	}
	if err := st.CopyTrade().CreateSignal(&store.CopyTradeSignal{ID: "s-open", TraderID: "trader-1", ChannelID: "chan-1", MessageID: "m-open", Status: "execution_wait"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CopyTrade().ClaimAction(&store.CopyTradeAction{ID: "a-open", TraderID: "trader-1", SignalID: "s-open", MessageID: "m-open", ContextID: ctx.ID, Symbol: "BTCUSDT", Direction: "LONG", Action: "OPEN", Status: "executing", Phase: "waiting_admission", PayloadJSON: `{"action":"OPEN"}`}); err != nil {
		t.Fatal(err)
	}
	x.beforeEntrySubmit = func(*store.CopyTradeOrder) error { return errEntryRevoked }
	_ = x.reconcileManagedEntry("revoke", ctx)
	_ = x.reconcileManagedEntry("revoke", ctx)
	saved, _ := st.CopyTrade().GetContext(ctx.ID)
	if !TradeState(saved.State).IsTerminal() {
		t.Fatalf("trade not terminal: %s", saved.State)
	}
	e := recoveryEngine(x, st)
	e.recoverOrphanActions()
	a, _ := st.CopyTrade().GetAction("a-open")
	if a.Status != "done" || a.Phase != "admission_revoked" {
		t.Fatalf("action %s/%s", a.Status, a.Phase)
	}
	sig, _ := st.CopyTrade().GetSignal("s-open")
	if sig.Status != store.SignalStatusSkipped || sig.SkipReason != "ENTRY_ADMISSION_REVOKED" {
		t.Fatalf("signal %s/%s", sig.Status, sig.SkipReason)
	}
	// Idempotent: a later scan finds nothing pending.
	if pending, _ := st.CopyTrade().UnresolvedActions("trader-1"); len(pending) != 0 {
		t.Fatalf("still pending %+v", pending)
	}
}
