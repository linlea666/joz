package copytrader

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"nofx/store"
	"nofx/trader/types"
	"strings"
	"testing"
	"time"
)

type preciseStopVenue struct {
	*managedVenue
	submits, cancels int
	loseStopAck      bool
	rejectNext       bool
	lookupErr        error
	rulesErr         error
}

func (v *preciseStopVenue) MarketRules(symbol string) (*types.ManagedMarketRules, error) {
	if v.rulesErr != nil {
		return nil, v.rulesErr
	}
	return v.managedVenue.MarketRules(symbol)
}

func (v *preciseStopVenue) GetOpenOrders(symbol string) ([]types.OpenOrder, error) {
	if v.lookupErr != nil {
		return nil, v.lookupErr
	}
	return v.openOrders, nil
}
func (v *preciseStopVenue) SetManagedStopLoss(symbol, side string, qty, price float64, id string) error {
	v.submits++
	if v.rejectNext {
		v.rejectNext = false
		return types.ErrManagedOrderRejected
	}
	v.openOrders = append(v.openOrders, types.OpenOrder{OrderID: fmt.Sprintf("stop-%d", v.submits), ClientID: id, OrderKind: "ALGO", Symbol: symbol, PositionSide: side, Type: "STOP_MARKET", StopPrice: price, Quantity: qty})
	if v.loseStopAck {
		return errors.New("timeout after acceptance")
	}
	return nil
}
func (v *preciseStopVenue) CancelStopOrder(_ string, o types.OpenOrder) error {
	v.cancels++
	var keep []types.OpenOrder
	for _, p := range v.openOrders {
		if p.OrderID != o.OrderID {
			keep = append(keep, p)
		}
	}
	v.openOrders = keep
	return nil
}

func TestProtectionTickAndUnknownAcknowledgement(t *testing.T) {
	st := newTestStore(t)
	v := &preciseStopVenue{managedVenue: newManagedVenue()}
	x := NewExecutor("trader-1", v, st, NewEventLogger(st, "trader-1", "chan-1"))
	ctx := newTestContext(t, st, StateOpen)
	if err := x.persistContext(ctx, map[string]interface{}{"stop_loss_price": 294.9876923, "requested_stop_loss": 294.9876923}); err != nil {
		t.Fatal(err)
	}
	v.openOrders = []types.OpenOrder{{OrderID: "old", Symbol: "BTCUSDT", PositionSide: "LONG", Type: "STOP_MARKET", StopPrice: 294.98, Quantity: .7}}
	for i := 0; i < 3; i++ {
		if err := x.ensureStopProtection("trace", "signal", ctx, .7); err != nil {
			t.Fatal(err)
		}
	}
	if v.submits != 0 || v.cancels != 0 || ctx.StopLossPrice != 294.98 {
		t.Fatalf("precision churn: %+v", ctx)
	}
	v.lookupErr = errors.New("lookup unavailable")
	if err := x.setStopProtection("trace", "signal", ctx, .7, 295.12); err == nil {
		t.Fatal("lookup failure accepted")
	}
	if v.cancels != 0 {
		t.Fatal("lookup error cancelled protection")
	}
	v.lookupErr = nil
	v.loseStopAck = true
	if err := x.setStopProtection("trace", "signal", ctx, .7, 295.12); err == nil {
		t.Fatal("lost acknowledgement must remain uncertain")
	}
	if ctx.StopIntentJSON == "" {
		t.Fatal("lost durable intent")
	}
	restored, err := st.CopyTrade().GetContext(ctx.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err = x.ensureStopProtection("restart", "", restored, .7); err != nil {
		t.Fatal(err)
	}
	if v.submits != 1 || restored.StopLossPrice != 295.12 || restored.StopIntentJSON != "" {
		t.Fatal("recovery repeated stop or lost actual target")
	}
}

func TestProtectionDefiniteFailureRestoresOldStop(t *testing.T) {
	st := newTestStore(t)
	v := &preciseStopVenue{managedVenue: newManagedVenue(), rejectNext: true}
	x := NewExecutor("trader-1", v, st, NewEventLogger(st, "trader-1", "chan-1"))
	ctx := newTestContext(t, st, StateOpen)
	v.openOrders = []types.OpenOrder{{OrderID: "old", OrderKind: "ALGO", Symbol: ctx.Symbol, PositionSide: "LONG", Type: "STOP_MARKET", StopPrice: 95, Quantity: .5}}
	if err := x.setStopProtection("trace", "signal", ctx, .5, 99); !errors.Is(err, types.ErrManagedOrderRejected) {
		t.Fatalf("expected rejection, got %v", err)
	}
	if len(v.openOrders) != 1 || v.openOrders[0].StopPrice != 95 || ctx.StopLossPrice != 95 {
		t.Fatalf("old protection missing: %+v", v.openOrders)
	}
}

func TestIndependentAccountAdmissionAndUnsplitExclusivity(t *testing.T) {
	x, v, st := managedExecutor(t)
	p := splitPlan()
	p.SplitReference = 0
	c, err := x.ExecuteOpen("first", "s1", p)
	if err != nil {
		t.Fatal(err)
	}
	if c.ExchangeID != "account-1" {
		t.Fatal("account not frozen")
	}
	if err = st.Trader().Create(&store.Trader{ID: "same", UserID: "test", Name: "same", ExchangeID: "account-1"}); err != nil {
		t.Fatal(err)
	}
	same := NewExecutor("same", v, st, NewEventLogger(st, "same", "chan-1"))
	if _, err = same.ExecuteOpen("second", "s2", p); err == nil {
		t.Fatal("unsplit path bypassed ownership")
	}
	if err = st.Trader().Create(&store.Trader{ID: "independent", UserID: "test", Name: "independent", ExchangeID: "account-2"}); err != nil {
		t.Fatal(err)
	}
	other := NewExecutor("independent", newManagedVenue(), st, NewEventLogger(st, "independent", "chan-1"))
	if _, err = other.ExecuteOpen("other", "s3", p); err != nil {
		t.Fatalf("independent account blocked: %v", err)
	}
	if busy, err := st.CopyTrade().AccountBusy("account-1"); err != nil || !busy {
		t.Fatal("active binding not protected")
	}
}

func TestTraderDualPriceFixturesAndConditions(t *testing.T) {
	for _, tc := range []struct {
		symbol, text string
		a, b         float64
	}{
		{"QNT", "#QNT ｜做多 50X\n進場：市價288.00—279.33\n止盈：300—317—340\n止損：270.57", 288, 279.33},
		{"NEAR", "#NEAR ｜做空 50X\n進場：市價4.929附近—5.0377\n止盈：4.6236—4.4265—3.9335\n止損：5.225", 4.929, 5.0377},
		{"AXTI", "#AXTI ｜做多\n進場：市價75附近—71.13\n止盈：80\n止損：69", 75, 71.13},
	} {
		t.Run(tc.symbol, func(t *testing.T) {
			msg := &store.DiscordMessage{Content: tc.text}
			sources := BuildSourceSegments(msg, "default")
			ins := &SourceInterpretation{Classification: ClassificationAmbiguous, Action: ActionOpen, Symbol: tc.symbol, Direction: DirectionLong, StopLossLevels: []SLLevel{{Price: PriceSpec{Type: PriceFixed, Price: 1}}}, TakeProfitLevels: []TPLevel{{Price: PriceSpec{Type: PriceFixed, Price: 400}}}}
			ApplyMessageRules(ins, msg, sources, MessageRules{DualPriceMode: DualPriceSplit, ReduceRatio: 50})
			if len(ins.EntryOrders) != 2 || ins.EntryOrders[0].Price.Price != tc.a || ins.EntryOrders[1].Price.Price != tc.b || ins.Classification != ClassificationSignal {
				t.Fatalf("lost entry legs: %+v", ins)
			}
		})
	}
	msg := &store.DiscordMessage{Content: "#DASH TP1觸及，到止盈後可減倉上成本"}
	src := BuildSourceSegments(msg, "tyler_v1")
	ins := InterpretKnownSource(msg, src, "tyler_v1")
	if ins == nil {
		t.Fatal("missing compound policy")
	}
	ApplyMessageRules(ins, msg, src, MessageRules{Profile: "tyler_v1", ReduceRatio: 25})
	for _, child := range ins.Flatten() {
		if child.Action == ActionReduce && (child.RequiresTPFill != 1 || child.CloseRatio == nil || *child.CloseRatio != 25) {
			t.Fatalf("condition/default not propagated: %+v", child)
		}
	}
}

func TestHistoricalActionNilArraysRemainDeduplicated(t *testing.T) {
	st := newTestStore(t)
	ctx := newTestContext(t, st, StateOpen)
	e := &Engine{traderID: "trader-1", st: st, cfg: &CopyTradingConfig{PrimaryChannelID: "chan-1"}, events: NewEventLogger(st, "trader-1", "chan-1")}
	msg := &store.DiscordMessage{MessageID: "move", ChannelID: "chan-1", ReplyToMessageID: ctx.RootMessageID}
	ins := &SourceInterpretation{Classification: ClassificationSignal, Action: ActionUpdateSL, Symbol: "BTC", Direction: DirectionLong, TradeReference: TradeReference{RootMessageID: ctx.RootMessageID}, StopLossLevels: []SLLevel{{Price: PriceSpec{Type: PriceFixed, Price: 84340}}}}
	payload, _ := json.Marshal(ins)
	old := &store.CopyTradeAction{ID: "legacy-id", TraderID: "trader-1", MessageID: "move", Symbol: "BTCUSDT", Direction: "LONG", ContextID: ctx.ID, Action: "UPDATE_SL", Status: "done", PayloadJSON: string(payload)}
	if _, err := st.CopyTrade().ClaimAction(old); err != nil {
		t.Fatal(err)
	}
	ins.ConditionalRules = []ConditionalRule{}
	ins.TakeProfitLevels = []TPLevel{}
	_, claimed, err := e.claimInstruction("new", msg, ins, "BTCUSDT")
	if err != nil || claimed {
		t.Fatalf("legacy semantic identity lost: claimed=%v err=%v", claimed, err)
	}
}

func TestRemainingTPAllocationKeepsFillEvidenceAndRunner(t *testing.T) {
	old := []TPPlanEntry{{Ordinal: 1, Filled: true, Quantity: 5, FilledQuantity: 5}, {Ordinal: 2, Quantity: 3}, {Ordinal: 3, Quantity: 2}}
	w, err := remainingTPAllocation(5, old, []int{1, 2, 3}, []float64{50, 30, 20})
	if err != nil {
		t.Fatal(err)
	}
	q, err := SplitTPQuantities(5, w, .1, 0)
	if err != nil {
		t.Fatal(err)
	}
	if q[0] != 0 || q[1] != 3 || q[2] != 2 {
		t.Fatalf("remaining TP lost quantity: %v", q)
	}
	partial := []TPPlanEntry{{Ordinal: 1, Quantity: 5, FilledQuantity: 2}, {Ordinal: 2, Quantity: 3}, {Ordinal: 3, Quantity: 2}}
	w, err = remainingTPAllocation(8, partial, []int{1, 2, 3}, []float64{50, 30, 20})
	if err != nil {
		t.Fatal(err)
	}
	q, _ = SplitTPQuantities(8, w, .1, 0)
	if q[0] != 3 || q[1] != 3 || q[2] != 2 {
		t.Fatalf("partial fill deducted incorrectly: %v", q)
	}
	w, err = remainingTPAllocation(5, old, []int{1, 2, 3}, []float64{25, 15, 10})
	if err != nil {
		t.Fatal(err)
	}
	sum := 0.0
	for _, r := range w {
		sum += r
	}
	if math.Abs(sum-50) > 1e-9 {
		t.Fatal("runner lost")
	}
	if _, err = mapTPUpdate(old, []float64{125, 140}); err == nil {
		t.Fatal("ambiguous ordinal mapping accepted")
	}
}

func TestInvalidConfigAndUnknownMarginFailClosed(t *testing.T) {
	cfg := DefaultCopyTradingConfig()
	cfg.PrimaryChannelID = "123"
	for _, v := range []int{-1, 153722868} {
		cfg.EntryTimeoutMinutes = v
		if err := cfg.Validate(); err == nil {
			t.Fatal("invalid duration accepted")
		}
	}
	cfg.EntryTimeoutMinutes = 0
	if err := cfg.Validate(); err != nil {
		t.Fatal("legacy disabled timeout rejected", err)
	}
	cfg.MajorPriceOffsetPct = math.NaN()
	if err := cfg.Validate(); err == nil {
		t.Fatal("NaN accepted")
	}
	for _, v := range []float64{0, -1, math.NaN(), math.Inf(1)} {
		if _, err := ComputePositionSize(SizingInput{RiskMode: RiskModeByLoss, RiskAmountUSD: 10, EntryPrice: 100, StopLossPrice: 90, Leverage: 10, AvailableMarginUSD: v}); err == nil {
			t.Fatal("invalid margin permitted", v)
		}
	}
	if !transientModelError(errors.New("LLM empty response")) || transientModelError(errors.New("LLM call failed: 402 insufficient balance")) {
		t.Fatal("retry classification wrong")
	}
}

func TestExplicitSplitRefusesMarketConversionAndUnsupportedLegs(t *testing.T) {
	x, _, st := managedExecutor(t)
	cfg := DefaultCopyTradingConfig()
	cfg.PrimaryChannelID = "chan-1"
	e := &Engine{traderID: "trader-1", cfg: &cfg, st: st, exec: x, events: x.events}
	msg := &store.DiscordMessage{MessageID: "dual", ChannelID: "chan-1", MessageTimestamp: time.Now()}
	ins := &SourceInterpretation{Action: ActionOpen, Direction: DirectionLong, Symbol: "BTC", EntryOrders: []EntryOrder{{OrderType: EntryMarket, Price: PriceSpec{Type: PriceMarket, Price: 100}}, {OrderType: EntryLimit, Price: PriceSpec{Type: PriceFixed, Price: 95}}}, StopLossLevels: []SLLevel{{Price: PriceSpec{Type: PriceFixed, Price: 90}}}, TakeProfitLevels: []TPLevel{{Price: PriceSpec{Type: PriceFixed, Price: 110}}}}
	skip, err := e.routeOpen("trace", "signal", msg, ins, "BTCUSDT", 105)
	if skip != SkipRiskRejected || err != nil {
		t.Fatalf("first market leg changed semantics: %s %v", skip, err)
	}
	ins.EntryOrders = append(ins.EntryOrders, ins.EntryOrders[1])
	_, err = e.routeOpen("trace", "signal", msg, ins, "BTCUSDT", 100)
	if err == nil || !strings.Contains(err.Error(), "unsupported entry combination") {
		t.Fatal("unsupported legs silently truncated")
	}
}

func TestTPUpdateIntegrationAfterFirstFill(t *testing.T) {
	x, v, _ := managedExecutor(t)
	p := splitPlan()
	p.SplitReference = 0
	p.Quantity = 10
	p.TPPrices = []float64{110, 120, 130}
	p.TPRatios = []float64{50, 30, 20}
	ctx, err := x.ExecuteOpen("open", "open-signal", p)
	if err != nil {
		t.Fatal(err)
	}
	first := readTPPlan(ctx)[0]
	v.fill(v.orders[first.ClientID], 5, 110)
	x.refreshTPProgress("fill", ctx)
	if _, err = x.ExecuteUpdateTP("update", "update-signal", ctx, []float64{111, 121, 131}, []float64{50, 30, 20}); err != nil {
		t.Fatal(err)
	}
	remaining := 0.0
	for _, tp := range readTPPlan(ctx) {
		if tp.Ordinal == 1 {
			if !tp.Filled || tp.OrderID != first.OrderID || tp.FilledQuantity != 5 {
				t.Fatal("completed TP evidence mutated")
			}
		} else {
			remaining += tp.Quantity - tp.FilledQuantity
		}
	}
	if remaining != 5 || ctx.LastTPSignalID != "update-signal" || ctx.TPUpdateIntentJSON != "" {
		t.Fatalf("remaining=%g context=%+v", remaining, ctx)
	}
}

func TestTPFillWriteFailurePreventsProtectionRebuild(t *testing.T) {
	x, v, st := managedExecutor(t)
	p := splitPlan()
	p.SplitReference = 0
	p.Quantity = 10
	p.TPPrices = []float64{110, 120, 130}
	p.TPRatios = []float64{50, 30, 20}
	ctx, err := x.ExecuteOpen("open", "signal", p)
	if err != nil {
		t.Fatal(err)
	}
	first := readTPPlan(ctx)[0]
	v.fill(v.orders[first.ClientID], 5, 110)
	if err = st.GormDB().Exec("CREATE TRIGGER reject_tp_progress BEFORE UPDATE OF tp_plan_json ON copytrade_trade_contexts BEGIN SELECT RAISE(FAIL, 'simulated TP persistence failure'); END").Error; err != nil {
		t.Fatal(err)
	}
	before := len(v.orders)
	if err = x.restoreRemainingProtections("repair", "", ctx, v.qty); err == nil {
		t.Fatal("TP fill persistence failure was swallowed")
	}
	if len(v.orders) != before || confirmedTPLevel(ctx, 1) {
		t.Fatal("uncommitted progress triggered a TP replacement or completion")
	}
	if err = st.GormDB().Exec("DROP TRIGGER reject_tp_progress").Error; err != nil {
		t.Fatal(err)
	}
	if _, err = x.refreshTPProgressChecked("retry", ctx); err != nil || !confirmedTPLevel(ctx, 1) {
		t.Fatalf("durable fill recovery failed: %v", err)
	}
}

func TestMessageRulesPreserveExplicitReductionRatios(t *testing.T) {
	for _, text := range []string{"BTC 减仓剩余仓位 25%", "BTC take 25% off the position", "BTC 减仓四分之一", "BTC reduce 1/4"} {
		ratio := 25.0
		ins := &SourceInterpretation{Action: ActionReduce, Symbol: "BTC", CloseRatio: &ratio}
		ApplyMessageRules(ins, &store.DiscordMessage{}, []SourceSegment{{Role: "current", Text: text}}, MessageRules{ReduceRatio: 60})
		if ins.CloseRatio == nil || *ins.CloseRatio != 25 {
			t.Fatalf("explicit author ratio overwritten: %s", text)
		}
	}
}

func TestPendingStopRecoveryRechecksChangedQuantity(t *testing.T) {
	st := newTestStore(t)
	v := &preciseStopVenue{managedVenue: newManagedVenue(), loseStopAck: true}
	x := NewExecutor("trader-1", v, st, NewEventLogger(st, "trader-1", "chan-1"))
	ctx := newTestContext(t, st, StateOpen)
	if err := x.setStopProtection("submit", "", ctx, .7, 90); !errors.Is(err, ErrProtectionUnconfirmed) {
		t.Fatal(err)
	}
	if err := x.setStopProtection("recover", "", ctx, .4, 90); !errors.Is(err, ErrProtectionUnconfirmed) {
		t.Fatal("stale quantity was accepted as current protection", err)
	}
	if v.submits != 1 || ctx.StopIntentJSON != "" {
		t.Fatal("old intent was not reconciled exactly once")
	}
	v.rulesErr = errors.New("instrument lookup timeout")
	if err := x.setStopProtection("rules", "", ctx, .4, 90); !errors.Is(err, ErrProtectionUnconfirmed) {
		t.Fatal("market metadata failure must remain an observation error", err)
	}
	if v.cancels != 0 {
		t.Fatal("lookup failure cancelled existing protection")
	}
}

func TestLookupFailureKeepsExistingPositionAndStop(t *testing.T) {
	st := newTestStore(t)
	v := &preciseStopVenue{managedVenue: newManagedVenue()}
	x := NewExecutor("trader-1", v, st, NewEventLogger(st, "trader-1", "chan-1"))
	p := splitPlan()
	p.SplitReference = 0
	ctx, err := x.ExecuteOpen("open", "signal", p)
	if err != nil {
		t.Fatal(err)
	}
	before := v.qty
	count := v.cancels
	v.lookupErr = errors.New("temporary read outage")
	err = x.reconcileManagedEntry("reconcile", ctx)
	if !errors.Is(err, ErrProtectionUnconfirmed) || v.qty != before || v.cancels != count {
		t.Fatalf("read failure modified protection/position: %v", err)
	}
}

func TestDASHAuthorSizingIsNotEligibilityAndHeavyConditionSurvives(t *testing.T) {
	msg := &store.DiscordMessage{Content: "#DASH 做多\n進場：市價50\n止損：45\n止盈：55\n2%～5% 倉位｜20-30x"}
	src := BuildSourceSegments(msg, "tyler_v1")
	ins := &SourceInterpretation{Classification: ClassificationNeedsContext, Action: ActionOpen, Symbol: "DASH", Direction: DirectionLong, EntryOrders: []EntryOrder{{OrderType: EntryMarket, Price: PriceSpec{Type: PriceMarket, Price: 50}}}, StopLossLevels: []SLLevel{{Price: PriceSpec{Type: PriceFixed, Price: 45}}}, TakeProfitLevels: []TPLevel{{Price: PriceSpec{Type: PriceFixed, Price: 55}}}, EligibilityConditions: []string{"2%～5% 倉位｜20-30x"}}
	ApplyMessageRules(ins, msg, src, MessageRules{Profile: "tyler_v1", ReduceRatio: 50})
	if ins.Classification != ClassificationSignal || len(ins.EligibilityConditions) > 0 || ins.ActionEvidence == nil {
		t.Fatalf("author sizing became eligibility: %+v", ins)
	}
	if skip, _, err := ValidateActionEvidenceDetailed(ins, src); err != nil || skip != SkipNone {
		t.Fatalf("restored open lacks valid source evidence: %s %v", skip, err)
	}
	msg.Content = "#DASH 倉位重的可以減倉上成本"
	src = BuildSourceSegments(msg, "tyler_v1")
	ins = InterpretKnownSource(msg, src, "tyler_v1")
	if ins == nil {
		t.Fatal("expected compound instruction")
	}
	ApplyMessageRules(ins, msg, src, MessageRules{Profile: "tyler_v1", ReduceRatio: 25})
	for _, child := range ins.Flatten() {
		if child.Classification != ClassificationNeedsContext {
			t.Fatalf("heavy position condition lost: %+v", child)
		}
	}
}

func TestPendingStopQuantityIsReconciledBeforeNewMove(t *testing.T) {
	st := newTestStore(t)
	v := &preciseStopVenue{managedVenue: newManagedVenue()}
	x := NewExecutor("trader-1", v, st, NewEventLogger(st, "trader-1", "chan-1"))
	ctx := newTestContext(t, st, StateOpen)
	if err := x.setStopProtection("trace", "signal", ctx, .5, 95); err != nil {
		t.Fatal(err)
	}
	if ctx.StopOrderID == "" {
		t.Fatal("acknowledged stop identity not saved")
	}
	if err := x.setStopProtection("trace", "signal", ctx, .2, 99); err != nil {
		t.Fatal("owned stop cannot be resized", err)
	}
	if len(v.openOrders) != 1 || v.openOrders[0].Quantity != .2 {
		t.Fatal("remaining size not protected")
	}
}

type tpCancelRaceVenue struct {
	*managedVenue
	race bool
}

func (v *tpCancelRaceVenue) CancelOrder(symbol, id string) error {
	if v.race {
		for _, o := range v.orders {
			if o.OrderID == id && o.Side == "SELL" && o.Status != "FILLED" {
				v.fill(o, .5, o.Price)
				v.race = false
				break
			}
		}
	}
	return v.managedVenue.CancelOrder(symbol, id)
}
func TestTPUpdateRestartAfterCancelFillRace(t *testing.T) {
	st := newTestStore(t)
	v := &tpCancelRaceVenue{managedVenue: newManagedVenue()}
	x := NewExecutor("trader-1", v, st, NewEventLogger(st, "trader-1", "chan-1"))
	p := splitPlan()
	p.SplitReference = 0
	p.Quantity = 10
	ctx, err := x.ExecuteOpen("open", "s", p)
	if err != nil {
		t.Fatal(err)
	}
	v.race = true
	if _, err = x.ExecuteUpdateTP("update", "tp-signal", ctx, []float64{111, 121}, []float64{50, 50}); err == nil {
		t.Fatal("cancel race not detected")
	}
	if ctx.TPUpdateIntentJSON == "" {
		t.Fatal("durable TP intent missing")
	}
	ctx, err = st.CopyTrade().GetContext(ctx.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = x.updateTakeProfits("restart", "", ctx, nil, nil); err != nil {
		t.Fatal(err)
	}
	total := 0.0
	fills := 0.0
	for _, tp := range readTPPlan(ctx) {
		if !tp.Filled {
			total += tp.Quantity - tp.FilledQuantity
		}
		fills += tp.FilledQuantity + tp.PriorFilledQuantity
	}
	if math.Abs(total-v.qty) > 1e-9 || fills != .5 {
		t.Fatalf("cancel fill lost/overallocated: pending=%g position=%g fills=%g", total, v.qty, fills)
	}
}

func TestRejectedRevisionAndUnknownActionAreDifferent(t *testing.T) {
	st := newTestStore(t)
	e := &Engine{traderID: "trader-1", st: st, cfg: &CopyTradingConfig{}}
	msg := &store.DiscordMessage{MessageID: "edited", Revision: 1}
	ins := &SourceInterpretation{Action: ActionOpen, Symbol: "BTC", Direction: DirectionLong}
	a, claimed, err := e.claimInstruction("first", msg, ins, "BTCUSDT")
	if err != nil || !claimed {
		t.Fatal(err)
	}
	if err = st.CopyTrade().UpdateAction(a.ID, map[string]interface{}{"context_id": "rejected-context", "status": "rejected"}); err != nil {
		t.Fatal(err)
	}
	msg.Revision = 2
	b, claimed, err := e.claimInstruction("revision", msg, ins, "BTCUSDT")
	if err != nil || !claimed || b.ID == a.ID {
		t.Fatalf("new revision remained blocked: %v %v", claimed, err)
	}
	if err = st.CopyTrade().UpdateAction(b.ID, map[string]interface{}{"context_id": "unknown-context", "status": "uncertain"}); err != nil {
		t.Fatal(err)
	}
	msg.Revision = 3
	_, claimed, err = e.claimInstruction("later", msg, ins, "BTCUSDT")
	if err != nil || claimed {
		t.Fatal("uncertain market order resubmitted")
	}
}

func TestContextWriteFailurePreventsStopMutation(t *testing.T) {
	st := newTestStore(t)
	v := &preciseStopVenue{managedVenue: newManagedVenue()}
	x := NewExecutor("trader-1", v, st, NewEventLogger(st, "trader-1", "chan-1"))
	ctx := newTestContext(t, st, StateOpen)
	if err := st.GormDB().Exec("CREATE TRIGGER reject_context_update BEFORE UPDATE ON copytrade_trade_contexts BEGIN SELECT RAISE(FAIL, 'simulated database failure'); END").Error; err != nil {
		t.Fatal(err)
	}
	if err := x.setStopProtection("trace", "signal", ctx, .5, 99); err == nil {
		t.Fatal("failed intent persistence ignored")
	}
	if v.submits != 0 || v.cancels != 0 || ctx.StopLossPrice != 95 {
		t.Fatal("exchange or memory mutated after failed persistence")
	}
}

func TestAccountBindingGuardAndFrozenRules(t *testing.T) {
	st := newTestStore(t)
	newTestContext(t, st, StateOpen)
	err := st.Trader().Update(&store.Trader{ID: "trader-1", UserID: "test", Name: "test", ExchangeID: "other"})
	if err == nil {
		t.Fatal("active binding changed")
	}
	cfg := DefaultCopyTradingConfig()
	cfg.MarketDualPriceMode = DualPriceSplit
	r := cfg.MessageRules()
	if err = st.CopyTrade().CreateSignal(&store.CopyTradeSignal{ID: "snapshot", TraderID: "trader-1", RulesSnapshotJSON: r.Snapshot()}); err != nil {
		t.Fatal(err)
	}
	cfg.MarketDualPriceMode = DualPriceRange
	e := &Engine{traderID: "trader-1", st: st, cfg: &cfg}
	frozen, err := e.rulesForSignal("snapshot")
	if err != nil || frozen.DualPriceMode != DualPriceSplit {
		t.Fatal("retry rules changed with live config")
	}
	if err = st.CopyTrade().UpdateSignal("snapshot", map[string]interface{}{"rules_snapshot_json": "corrupt"}); err != nil {
		t.Fatal(err)
	}
	if _, err = e.rulesForSignal("snapshot"); err == nil {
		t.Fatal("corrupt snapshot silently fell back")
	}
}

func TestCompletedTradeReleasesAccountForNextSignal(t *testing.T) {
	x, v, _ := managedExecutor(t)
	p := splitPlan()
	p.SplitReference = 0
	ctx, err := x.ExecuteOpen("open", "first", p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = x.ExecuteClose("close", "close-signal", ctx, 100); err != nil {
		t.Fatal(err)
	}
	if ctx.State != string(StateClosed) || v.qty != 0 {
		t.Fatal("first trade not closed")
	}
	p.RootMsgID = "second-root"
	if _, err = x.ExecuteOpen("new", "second", p); err != nil {
		t.Fatal("stale TP ledger held completed ownership", err)
	}
}
