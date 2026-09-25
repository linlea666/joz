package copytrader

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"nofx/mcp"
	"nofx/store"
)

func liveTestEngine(t *testing.T, v *managedVenue, st *store.Store, profile string) *Engine {
	cfg := DefaultCopyTradingConfig()
	cfg.PrimaryChannelID = "chan-1"
	cfg.InterpretationProfile = profile
	cfg.ParseImages = false
	cfg.SendPositionSnapshot = false
	cfg.SignalContextEnabled = false
	return NewEngine(EngineParams{TraderID: "trader-1", Store: st, Exchange: v, Config: &cfg})
}
func TestReductionNotRepeatedByRevisionOrRestart(t *testing.T) {
	x, v, st := managedExecutor(t)
	ctx, err := x.ExecuteOpen("open", "s-open", splitPlan())
	if err != nil {
		t.Fatal(err)
	}
	e := liveTestEngine(t, v, st, "tyler_v1")
	msg := &store.DiscordMessage{MessageID: "manage", ChannelID: "chan-1", MessageTimestamp: time.Now(), Content: "#BTC 止盈或者減倉"}
	if err = e.HandleMessage(msg, false); err != nil {
		t.Fatal(err)
	}
	after := v.qty
	if after >= .909 || after <= 0 {
		t.Fatalf("reduce did not execute: %g", after)
	}
	e = liveTestEngine(t, v, st, "tyler_v1")
	msg.Revision++
	msg.Content += " ✅"
	if err = e.HandleMessage(msg, true); err != nil {
		t.Fatal(err)
	}
	if v.qty != after {
		t.Fatal("cosmetic edit reduced again")
	}
	actions, _ := st.CopyTrade().GetActionsForMessage(e.traderID, msg.MessageID)
	if len(actions) != 1 || actions[0].Status != "done" {
		t.Fatalf("action journal %+v", actions)
	}
	saved, _ := st.CopyTrade().GetContext(ctx.ID)
	if !saved.EntryDisabled {
		t.Fatal("reduce did not disable additions")
	}
}

func TestReduceThenMoveStopToOriginalTP(t *testing.T) {
	x, v, st := managedExecutor(t)
	ctx, err := x.ExecuteOpen("open", "s-open", splitPlan())
	if err != nil {
		t.Fatal(err)
	}
	e := liveTestEngine(t, v, st, "tyler_v1")
	msg := &store.DiscordMessage{MessageID: "both", ChannelID: "chan-1", MessageTimestamp: time.Now(), Content: "#BTC 減倉50%，止損移至TP1"}
	if err = e.HandleMessage(msg, false); err != nil {
		t.Fatal(err)
	}
	saved, _ := st.CopyTrade().GetContext(ctx.ID)
	if saved.StopLossPrice != 110 || v.qty >= .909 {
		t.Fatalf("combined action failed %+v", saved)
	}
	sig, _ := st.CopyTrade().LatestSignal(e.traderID, msg.MessageID, 0)
	var results []InstructionResult
	_ = json.Unmarshal([]byte(sig.InstructionResultsJSON), &results)
	if len(results) != 2 || results[0].Status != "executed" || results[1].Status != "executed" {
		t.Fatalf("child outcomes %+v", results)
	}
}

func TestRequiresAddFillIsNotSatisfiedByModelWarning(t *testing.T) {
	x, v, st := managedExecutor(t)
	_, err := x.ExecuteOpen("open", "s-open", splitPlan())
	if err != nil {
		t.Fatal(err)
	}
	e := liveTestEngine(t, v, st, "tyler_v1")
	msg := &store.DiscordMessage{MessageID: "conditional", ChannelID: "chan-1", MessageTimestamp: time.Now(), Content: "#BTC 有補倉的自行提前作Tp1"}
	before := v.qty
	_ = e.HandleMessage(msg, false)
	if v.qty != before {
		t.Fatal("unfilled second leg satisfied add-only condition")
	}
	sig, _ := st.CopyTrade().LatestSignal(e.traderID, msg.MessageID, 0)
	if sig.SkipReason != string(SkipNeedsContext) {
		t.Fatalf("missing condition outcome %+v", sig)
	}
}

func TestMultiOpenMessageCreatesIndependentContexts(t *testing.T) {
	f := marketReferenceFixture{Name: "multi", MarketPrice: 100, Content: "#BTC 做多 進場：市價 止損90 止盈110\n#ETH 做多 進場：市價 止損90 止盈110", Response: json.RawMessage(`{"classification":"SIGNAL","instructions":[{"classification":"SIGNAL","action":"OPEN","symbol":"BTC","direction":"LONG","entry_orders":[{"order_type":"MARKET","price":{"type":"MARKET"}}],"stop_loss_levels":[{"price":{"type":"FIXED","price":90}}],"take_profit_levels":[{"price":{"type":"FIXED","price":110}}]},{"classification":"SIGNAL","action":"OPEN","symbol":"ETH","direction":"LONG","entry_orders":[{"order_type":"MARKET","price":{"type":"MARKET"}}],"stop_loss_levels":[{"price":{"type":"FIXED","price":90}}],"take_profit_levels":[{"price":{"type":"FIXED","price":110}}]}]}`)}
	e, ex, _, msg := fixtureEngine(t, f, 1)
	_ = e.HandleMessage(msg, false)
	contexts, _ := e.st.CopyTrade().GetActiveContexts(e.traderID)
	if len(contexts) != 2 || ex.marketEntries != 2 {
		t.Fatalf("multi open lost a child: %d %d", len(contexts), ex.marketEntries)
	}
	sig, _ := e.st.CopyTrade().LatestSignal(e.traderID, msg.MessageID, 0)
	views, err := e.st.CopyTrade().SignalViews(e.traderID, []*store.CopyTradeSignal{sig})
	if err != nil || len(views[0].ActionResults) != 2 || views[0].TradeState != "" {
		t.Fatalf("ambiguous parent lifecycle leaked: %+v %v", views, err)
	}
}

type blockingModel struct {
	mcp.AIClient
	entered, release chan struct{}
	fail             bool
	response         string
}

func (m *blockingModel) CallWithRequest(*mcp.Request) (string, error) {
	close(m.entered)
	<-m.release
	if m.fail {
		return "", errors.New("504 gateway timeout")
	}
	if m.response != "" {
		return m.response, nil
	}
	return `{"classification":"IGNORE","action":"IGNORE"}`, nil
}
func TestModelWaitDoesNotHoldReconciliationLock(t *testing.T) {
	_, v, st := managedExecutor(t)
	e := liveTestEngine(t, v, st, "default")
	model := &blockingModel{entered: make(chan struct{}), release: make(chan struct{})}
	e.llm = model
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = e.HandleMessage(&store.DiscordMessage{MessageID: "slow", ChannelID: "chan-1", MessageTimestamp: time.Now(), Content: "analysis"}, false)
	}()
	<-model.entered
	locked := make(chan struct{})
	go func() { e.mu.Lock(); e.mu.Unlock(); close(locked) }()
	select {
	case <-locked:
	case <-time.After(time.Second):
		t.Fatal("slow model blocked execution lock")
	}
	close(model.release)
	<-done
}

func TestStoppedEngineDoesNotExecuteDelayedModelResponse(t *testing.T) {
	_, v, st := managedExecutor(t)
	e := liveTestEngine(t, v, st, "default")
	e.stopCh = make(chan struct{})
	model := &blockingModel{entered: make(chan struct{}), release: make(chan struct{}), response: `{"classification":"SIGNAL","action":"OPEN","symbol":"BTC","direction":"LONG","entry_orders":[{"order_type":"MARKET","price":{"type":"MARKET"}}],"stop_loss_levels":[{"price":{"type":"FIXED","price":90}}]}`}
	e.llm = model
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = e.HandleMessage(&store.DiscordMessage{MessageID: "stopped", ChannelID: "chan-1", MessageTimestamp: time.Now(), Content: "#BTC 進場市價 止損90"}, false)
	}()
	<-model.entered
	close(e.stopCh)
	close(model.release)
	<-done
	if len(v.calls) != 0 {
		t.Fatal("stopped engine placed an order after slow model returned")
	}
	sig, _ := st.CopyTrade().LatestSignal(e.traderID, "stopped", 0)
	if sig == nil || sig.SkipReason != string(SkipPaused) {
		t.Fatal("stopped outcome not recorded")
	}
}
func TestDeferredModelRetryBoundedAndNoHistoricalReplay(t *testing.T) {
	_, v, st := managedExecutor(t)
	e := liveTestEngine(t, v, st, "default")
	now := time.Now()
	msg := &store.DiscordMessage{MessageID: "retry", MessageTimestamp: now}
	sig := &store.CopyTradeSignal{ID: "new", TraderID: e.traderID, MessageID: msg.MessageID, ExecutionVersion: 1}
	if err := st.CopyTrade().CreateSignal(sig); err != nil {
		t.Fatal(err)
	}
	err := errors.New("LLM call failed: 504 gateway timeout")
	if !e.deferInterpretationRetry(sig, msg, err) {
		t.Fatal("fresh transient failure not deferred")
	}
	got, _ := st.CopyTrade().GetSignal(sig.ID)
	if got.RetryCount != 1 || got.NextRetryAt.Sub(now) < 29*time.Second || got.NextRetryAt.Sub(now) > 31*time.Second {
		t.Fatal("wrong first retry")
	}
	if !e.deferInterpretationRetry(got, msg, err) {
		t.Fatal("second retry missing")
	}
	got, _ = st.CopyTrade().GetSignal(sig.ID)
	if got.RetryCount != 2 || got.NextRetryAt.Sub(now) < 119*time.Second || e.deferInterpretationRetry(got, msg, err) {
		t.Fatal("retry limit/delay incorrect")
	}
	got.ExecutionVersion = 0
	got.RetryCount = 0
	if e.deferInterpretationRetry(got, msg, err) {
		t.Fatal("historical failure requeued")
	}
	got.ExecutionVersion = 1
	msg.MessageTimestamp = now.Add(-2 * time.Hour)
	if e.deferInterpretationRetry(got, msg, err) {
		t.Fatal("expired signal requeued")
	}
	if transientModelError(errors.New("interpretation parse failed: 504")) {
		t.Fatal("permanent parse error retried")
	}
}

func TestPendingCloseRecoveryDoesNotRepeatMarket(t *testing.T) {
	x, v, st := managedExecutor(t)
	ctx, err := x.ExecuteOpen("open", "s-open", splitPlan())
	if err != nil {
		t.Fatal(err)
	}
	if err = x.closeEntryEligibility("cancel", ctx, "EXIT_REQUESTED"); err != nil {
		t.Fatal(err)
	}
	v.lostAck = true
	v.queryDown = true
	_, err = x.ExecuteClose("close", "s-close", ctx, 50)
	if err == nil {
		t.Fatal("uncertain close hidden")
	}
	after := v.qty
	closeCalls := len(v.calls)
	v.queryDown = false
	stored, _ := st.CopyTrade().GetContext(ctx.ID)
	if stored.ClosePendingJSON == "" {
		t.Fatal("close intent was not persisted")
	}

	restarted := NewExecutor("trader-1", v, st, NewEventLogger(st, "trader-1", "chan-1"))
	if err = restarted.reconcileManagedClose("restart", stored); err != nil {
		t.Fatal(err)
	}
	for _, r := range v.calls[closeCalls:] {
		if r.Type == "MARKET" {
			t.Fatal("recovery resent market reduction")
		}
	}
	if v.qty != after {
		t.Fatal("recovery reduced twice")
	}
}

func TestSplitPolicyThresholdAndCompatibility(t *testing.T) {
	for _, tt := range []struct {
		name              string
		market, threshold float64
		policy            string
		entry             EntryPlanType
		legs              int
	}{{"favorable", 99, 5, EntryPolicySplit, EntryPlanMarket, 1}, {"equal", 100, 5, EntryPolicySplit, EntryPlanMarket, 1}, {"within", 101, 5, EntryPolicySplit, EntryPlanMarket, 2}, {"threshold equality", 105, 5, EntryPolicySplit, EntryPlanMarket, 2}, {"over", 105.093, 5, EntryPolicySplit, EntryPlanLimit, 1}, {"legacy", 101, 5, EntryPolicyLegacy, EntryPlanMarket, 1}} {
		t.Run(tt.name, func(t *testing.T) {
			_, v, st := managedExecutor(t)
			v.market = tt.market
			e := liveTestEngine(t, v, st, "default")
			e.cfg.EntryPolicy = tt.policy
			e.cfg.MajorPriceOffsetPct = tt.threshold
			e.cfg.RiskAmountUSD = 20
			ins := &SourceInterpretation{Classification: ClassificationSignal, Action: ActionOpen, Symbol: "BTC", Direction: DirectionLong, EntryOrders: []EntryOrder{{OrderType: EntryMarket, Price: PriceSpec{Type: PriceMarket, Price: 100}}}, StopLossLevels: []SLLevel{{Price: PriceSpec{Type: PriceFixed, Price: 90}}}, TakeProfitLevels: []TPLevel{{Price: PriceSpec{Type: PriceFixed, Price: 130}}}}
			msg := &store.DiscordMessage{MessageID: strings.ReplaceAll(tt.name, " ", "-"), ChannelID: "chan-1", MessageTimestamp: time.Now()}
			if skip, err := e.routeOpen("trace", "signal", msg, ins, "BTCUSDT", v.market); err != nil || skip != SkipNone {
				t.Fatalf("open %s %v", skip, err)
			}
			calls := entryCalls(v)
			if len(calls) != tt.legs || calls[0].Type != string(tt.entry) {
				t.Fatalf("entry %+v", calls)
			}
		})
	}
}
