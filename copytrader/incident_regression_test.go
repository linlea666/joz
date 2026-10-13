package copytrader

import (
	"errors"
	"strings"
	"testing"
	"time"

	"nofx/store"
	"nofx/trader/types"
)

func TestIncidentManagementRecognition(t *testing.T) {
	for _, body := range []string{"#HYPE 磨磨唧唧的，止盈全部倉位，做新策略", "#HYPE 止盈全部仓位"} {
		msg := &store.DiscordMessage{Content: body}
		sources := BuildSourceSegments(msg, "tyler_v1")
		got := InterpretKnownSource(msg, sources, "tyler_v1")
		if got == nil || got.Action != ActionClose {
			t.Errorf("missing explicit close: %q => %+v", body, got)
			continue
		}
		if skip, detail, _ := ValidateActionEvidenceDetailed(got, sources); skip != SkipNone {
			t.Errorf("gate disagrees: %s %s", skip, detail)
		}
	}
	msg := &store.DiscordMessage{Content: "#BTC/USDT 均價獲利，可以考慮止盈離場"}
	if got := InterpretKnownSource(msg, BuildSourceSegments(msg, "tyler_v1"), "tyler_v1"); got != nil {
		t.Fatalf("unresolved advice must reach interpreter, got %+v", got)
	}
}

func TestIncidentReferenceEntryPlans(t *testing.T) {
	for _, c := range []struct {
		name                  string
		dir                   Direction
		ref, quote, threshold float64
	}{
		{"STX", DirectionLong, .4147, .423, .5}, {"CHZ", DirectionLong, .01496, .01589, .5}, {"BTC", DirectionShort, 85100, 83829.2, .2},
	} {
		t.Run(c.name, func(t *testing.T) {
			orders := []EntryOrder{{OrderType: EntryMarket, Price: PriceSpec{Type: PriceMarket, Price: c.ref}}}
			plan, skip, err := resolveEntryPlan(c.dir, orders, c.quote, c.threshold, true, EntryPolicySplit)
			if err != nil || skip != SkipNone || plan.Decision.OrderType != EntryPlanLimit || plan.Decision.EntryPrice != c.ref || plan.SplitReference != 0 {
				t.Fatalf("unexpected plan: %+v %s %v", plan, skip, err)
			}
		})
	}
}
func TestIncidentFullExitBoundaries(t *testing.T) {
	for _, body := range []string{"#HYPE 不要止盈全部倉位", "#HYPE 如果跌破就止盈全部倉位", "#HYPE 止盈全部倉位的50%", "#HYPE 止盈全部仓位，留下30%"} {
		t.Run(body, func(t *testing.T) {
			msg := &store.DiscordMessage{Content: body}
			sources := BuildSourceSegments(msg, "tyler_v1")
			candidate := &SourceInterpretation{Classification: ClassificationSignal, Action: ActionClose, Symbol: "HYPE", CloseMode: CloseModeFull, ActionEvidence: &ActionEvidence{SourceID: "body", Text: body}}
			if known := InterpretKnownSource(msg, sources, "tyler_v1"); known != nil {
				candidate = known
			}
			ApplyMessageRules(candidate, msg, sources, MessageRules{Profile: "tyler_v1", ReduceRatio: 50})
			if candidate.Action == ActionClose && candidate.IsActionable() {
				if skip, _, _ := ValidateActionEvidenceDetailed(candidate, sources); skip == SkipNone {
					t.Fatalf("unsafe full close: %+v", candidate)
				}
			}
		})
	}
}

func TestContractRestrictionWinsWithoutQuote(t *testing.T) {
	x := &replayCheckExchange{mockExchange: &mockExchange{}, rules: &types.ManagedMarketRules{Status: "SETTLING"}, priceErr: errors.New("no quote")}
	e := &Engine{exec: &Executor{ex: x}}
	ev, verdict, detail := e.replayChecks(replayOpen())
	if verdict != VerdictSkip || !strings.HasPrefix(detail, "CONTRACT_NOT_TRADING") || ev.Market.Status != "unavailable" || ev.Contract.Status != "failed" {
		t.Fatalf("checks lost: %+v %s", ev, detail)
	}
	x.rules = nil
	x.rulesErr = errors.New("timeout")
	ev, _, _ = e.replayChecks(replayOpen())
	if ev.Contract.Status != "unavailable" {
		t.Fatal("network error treated as unsupported")
	}
	x.rulesErr = types.ErrInstrumentUnavailable
	ev, _, _ = e.replayChecks(replayOpen())
	if ev.Contract.Status != "failed" {
		t.Fatal("explicit unlisted contract not classified")
	}
}
func TestLiveContractPreflightNoSubmission(t *testing.T) {
	for _, price := range []float64{0, 2.3} {
		x, v, st := managedExecutor(t)
		v.market = price
		v.rules.Status = "SETTLING"
		cfg := DefaultCopyTradingConfig()
		e := NewEngine(EngineParams{TraderID: "trader-1", Config: &cfg, Store: st, Exchange: v})
		e.exec = x
		ins := replayOpen()
		ins.EntryOrders = []EntryOrder{{OrderType: EntryLimit, Price: PriceSpec{Type: PriceFixed, Price: 2.2766}}}
		ins.StopLossLevels = []SLLevel{{Price: PriceSpec{Type: PriceFixed, Price: 2.4}}}
		ins.TakeProfitLevels = []TPLevel{{Price: PriceSpec{Type: PriceFixed, Price: 2.1788}}}
		sig := &store.CopyTradeSignal{ID: "ray", TraderID: "trader-1", RulesSnapshotJSON: cfg.MessageRules().Snapshot()}
		if err := st.GormDB().Create(sig).Error; err != nil {
			t.Fatal(err)
		}
		msg := &store.DiscordMessage{MessageID: "ray", MessageTimestamp: time.Now()}
		_, _, err := e.processInstruction("ray", "ray", msg, ins)
		if err == nil || !strings.Contains(err.Error(), "CONTRACT_NOT_TRADING") {
			t.Fatalf("%v", err)
		}
		if len(v.calls) > 0 {
			t.Fatal("non-trading contract submitted")
		}
	}
}
func TestSignalViewsExposeRecoveryWithoutChangingStatus(t *testing.T) {
	x, _, c := historyFixture(t)
	if err := x.persistContext(c, map[string]interface{}{"last_error": "FLAT_RECOVERY_PENDING: history missing"}); err != nil {
		t.Fatal(err)
	}
	sig := &store.CopyTradeSignal{ID: "s", TraderID: x.traderID, TradeContextID: c.ID, Status: "executed", InterpretationJSON: `{"action":"OPEN"}`}
	views, err := x.st.CopyTrade().SignalViews(x.traderID, []*store.CopyTradeSignal{sig})
	if err != nil {
		t.Fatal(err)
	}
	if len(views) != 1 || views[0].Status != "executed" || !strings.Contains(views[0].RecoveryError, "FLAT_RECOVERY_PENDING") {
		t.Fatalf("missing recovery evidence: %+v", views)
	}
	other, err := x.st.CopyTrade().SignalViews("other", []*store.CopyTradeSignal{sig})
	if err != nil || other[0].RecoveryError != "" {
		t.Fatal("recovery state exposed across owners")
	}
}
