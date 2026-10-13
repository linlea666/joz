package copytrader

import (
	"encoding/json"
	"math"
	"testing"
	"time"

	"nofx/store"
)

func TestParticipationPolicyPriceBranches(t *testing.T) {
	for _, dir := range []Direction{DirectionLong, DirectionShort} {
		for _, threshold := range []float64{0, .5} {
			for _, price := range []float64{99, 100, 100.25, 100.5, 102} {
				market := price
				if dir == DirectionShort {
					market = 200 - price
				}
				orders := []EntryOrder{{OrderType: EntryMarket, Price: PriceSpec{Type: PriceMarket, Price: 100}}}
				p, skip, err := resolveEntryPlan(dir, orders, market, threshold, false, "market_reference_participate")
				if err != nil || skip != SkipNone || p.Decision.OrderType != EntryPlanMarket || p.Decision.EntryPrice != market {
					t.Fatalf("%s price=%g threshold=%g: %+v %s %v", dir, market, threshold, p, skip, err)
				}
				if (p.SplitReference > 0) != (price > 100) {
					t.Fatalf("only adverse prices must split: %+v", p)
				}
				if p.Decision.ThresholdApplied == nil || *p.Decision.ThresholdApplied || p.Decision.ThresholdPct != threshold || p.EntryPolicy != EntryPolicyParticipate {
					t.Fatalf("policy/threshold evidence missing: %+v", p)
				}
			}
		}
	}
}

func participationInstruction() *SourceInterpretation {
	return &SourceInterpretation{Classification: ClassificationSignal, Action: ActionOpen, Symbol: "BTC", Direction: DirectionLong,
		EntryOrders:      []EntryOrder{{OrderType: EntryMarket, Price: PriceSpec{Type: PriceMarket, Price: 100}}},
		StopLossLevels:   []SLLevel{{Price: PriceSpec{Type: PriceFixed, Price: 90}}},
		TakeProfitLevels: []TPLevel{{Price: PriceSpec{Type: PriceFixed, Price: 130}}}}
}

func TestParticipationExecutionRecoveryAndAudit(t *testing.T) {
	for _, scenario := range []string{"normal", "favorable", "equal", "lost_ack", "partial_fill", "protection_failed", "minimum_order", "slippage"} {
		t.Run(scenario, func(t *testing.T) {
			_, venue, st := managedExecutor(t)
			venue.market = 105
			e := liveTestEngine(t, venue, st, "default")
			e.cfg.EntryPolicy, e.cfg.RiskAmountUSD, e.cfg.MajorPriceOffsetPct = EntryPolicyParticipate, 20, .2
			switch scenario {
			case "favorable":
				venue.market = 99
			case "equal":
				venue.market = 100
			case "lost_ack":
				venue.lostAck, venue.queryDown = true, true
			case "partial_fill":
				venue.partialMarket = .5
			case "protection_failed":
				venue.failSL = true
			case "minimum_order":
				venue.rules.MinNotional = 1000
			}
			quote := venue.market
			if scenario == "slippage" {
				venue.market = 106
			}
			msg := &store.DiscordMessage{MessageID: scenario, ChannelID: "chan-1", MessageTimestamp: time.Now()}
			_, err := e.routeOpen("participation", "signal", msg, participationInstruction(), "BTCUSDT", quote)
			calls := entryCalls(venue)
			if scenario == "minimum_order" {
				if err == nil || len(calls) != 0 {
					t.Fatal("minimum size bypassed", err, calls)
				}
				return
			}
			if scenario == "normal" || scenario == "slippage" || scenario == "favorable" || scenario == "equal" {
				if err != nil {
					t.Fatal(err)
				}
				want := 2
				if scenario == "favorable" || scenario == "equal" {
					want = 1
				}
				if len(calls) != want || calls[0].Type != "MARKET" {
					t.Fatalf("bad plan: %+v", calls)
				}
				used := 0.0
				for _, call := range calls {
					price := call.Price
					if call.Type == "MARKET" {
						price = venue.market
					}
					used += call.Quantity * math.Abs(price-90)
				}
				if used > 20+1e-8 {
					t.Fatalf("risk expanded: %g", used)
				}
			} else if len(calls) != 1 {
				t.Fatalf("added risk before first leg was confirmed/protected: %+v", calls)
			}
			var events []store.CopyTradeEvent
			st.GormDB().Where("event = ?", EvEntryDecision).Find(&events)
			if len(events) != 1 {
				t.Fatalf("decision evidence: %d", len(events))
			}
			var audit struct {
				Plan resolvedEntryPlan
				Risk float64 `json:"risk_budget"`
			}
			if err := json.Unmarshal([]byte(events[0].ContextJSON), &audit); err != nil || audit.Plan.EntryPolicy != EntryPolicyParticipate || audit.Risk != 20 || audit.Plan.Decision.ThresholdApplied == nil || *audit.Plan.Decision.ThresholdApplied {
				t.Fatalf("missing immutable decision: %+v %v", audit, err)
			}
			if scenario == "lost_ack" || scenario == "normal" {
				venue.queryDown = false
				ctx, err := st.CopyTrade().GetActiveContextBySymbol(e.traderID, "BTCUSDT", "LONG")
				if err != nil || ctx == nil {
					t.Fatal(ctx, err)
				}
				// The durable execution plan remains valid after changing the future policy.
				e.cfg.EntryPolicy = EntryPolicyLegacy
				restarted := NewExecutor(e.traderID, venue, st, e.events)
				if err = restarted.reconcileManagedEntry("restart", ctx); err != nil {
					t.Fatal(err)
				}
				if got := entryCalls(venue); len(got) != 2 || got[0].Type != "MARKET" || got[1].Type != "LIMIT" {
					t.Fatalf("recovery duplicated or lost legs: %+v", got)
				}
			}
		})
	}
}

func TestParticipationScopeSnapshotAndReplay(t *testing.T) {
	_, venue, st := managedExecutor(t)
	venue.market = 105
	e := liveTestEngine(t, venue, st, "default")
	e.cfg.EntryPolicy = EntryPolicyParticipate
	oldKey := e.cfg.ExecutionKey()
	snapshot := e.cfg.MessageRules().Snapshot()
	if err := st.CopyTrade().CreateSignal(&store.CopyTradeSignal{ID: "frozen", RulesSnapshotJSON: snapshot}); err != nil {
		t.Fatal(err)
	}
	e.cfg.EntryPolicy = EntryPolicyLegacy
	rules, err := e.rulesForSignal("frozen")
	if err != nil || rules.EntryPolicy != EntryPolicyParticipate || oldKey == e.cfg.ExecutionKey() {
		t.Fatal("lost snapshot or execution boundary", rules, err)
	}
	e.cfg.EntryPolicy = EntryPolicyParticipate
	ins := participationInstruction()
	ev, verdict, detail := e.replayChecks(ins)
	if verdict != VerdictExecute || ev.EntryPlan == nil || ev.EntryPlan.SplitReference != 100 || ev.EntryPlan.Decision.ThresholdApplied == nil || *ev.EntryPlan.Decision.ThresholdApplied {
		t.Fatal("preview does not match live shape", ev, detail)
	}
	data, _ := json.Marshal(ev)
	var restored ReplayEvaluation
	if err = json.Unmarshal(data, &restored); err != nil || restored.EntryPlan == nil || restored.EntryPlan.EntryPolicy != EntryPolicyParticipate {
		t.Fatal("replay evidence not serializable", err)
	}
	if len(venue.calls) != 0 {
		t.Fatal("replay submitted orders")
	}
	for _, price := range []float64{90, 130} {
		venue.market = price
		ev, verdict, _ = e.replayChecks(ins)
		if verdict == VerdictExecute || ev.Market.Status == "passed" || ev.EntryPlan != nil {
			t.Fatalf("invalid stop/target permitted: %+v", ev)
		}
	}
	ins.EntryOrders = append(ins.EntryOrders, EntryOrder{OrderType: EntryLimit, Price: PriceSpec{Type: PriceFixed, Price: 95}})
	if _, skip, err := resolveEntryPlan(DirectionLong, ins.EntryOrders, 105, .2, false, EntryPolicyParticipate); skip != SkipRiskRejected || err == nil {
		t.Fatal("explicit two-price protection bypassed")
	}
	for _, order := range []EntryOrder{
		{OrderType: EntryLimit, Price: PriceSpec{Type: PriceFixed, Price: 100}},
		{OrderType: EntryLimit, Price: PriceSpec{Type: PriceRange, RangeLow: 95, RangeHigh: 100}},
		{OrderType: EntryMarket, Price: PriceSpec{Type: PriceMarket}},
	} {
		plan, skip, err := resolveEntryPlan(DirectionLong, []EntryOrder{order}, 105, .2, false, EntryPolicyParticipate)
		if skip != SkipNone || err != nil || plan.SplitReference != 0 || plan.Decision.ThresholdApplied != nil {
			t.Fatalf("unrelated entry changed: %+v %s %v", plan, skip, err)
		}
	}
}

func TestParticipationPolicyConfiguration(t *testing.T) {
	cfg := DefaultCopyTradingConfig()
	cfg.PrimaryChannelID = "123"
	cfg.EntryPolicy = "market_reference_participate"
	if err := cfg.ValidateExchange("binance"); err != nil {
		t.Fatal(err)
	}
	if err := cfg.ValidateExchange("bybit"); err == nil {
		t.Fatal("unsupported venue accepted")
	}
	cfg.RiskMode = RiskModeFixed
	if err := cfg.ValidateExchange("binance"); err == nil {
		t.Fatal("non-risk sizing accepted")
	}
}

func TestParticipationHistoricalEntryPrices(t *testing.T) {
	for _, tc := range []struct {
		direction                    Direction
		ref, market, stop, threshold float64
	}{
		{DirectionShort, 85100, 83829.2, 86100, .2},
		{DirectionLong, .4147, .423, .4007, .5},
	} {
		orders := []EntryOrder{{OrderType: EntryMarket, Price: PriceSpec{Type: PriceMarket, Price: tc.ref}}}
		p, skip, err := resolveEntryPlan(tc.direction, orders, tc.market, tc.threshold, true, "market_reference_participate")
		if err != nil || skip != SkipNone || p.SplitReference != tc.ref || p.Decision.OrderType != EntryPlanMarket {
			t.Fatalf("historical plan: %+v %s %v", p, skip, err)
		}
		v := newManagedVenue()
		v.rules.PriceTick, v.rules.QuantityStep, v.rules.MarketQuantityStep = .0001, .0001, .0001
		v.rules.MinQuantity, v.rules.MarketMinQuantity = .0001, .0001
		q1, q2, err := splitQuantities(&OpenPlan{Direction: tc.direction, EntryPrice: p.Decision.EntryPrice, SplitReference: p.SplitReference, StopLoss: tc.stop, RiskBudget: 20, AvailableMargin: 1000, Leverage: 10}, &v.rules)
		if err != nil {
			t.Fatal(err)
		}
		r1, r2 := q1*math.Abs(tc.market-tc.stop), q2*math.Abs(tc.ref-tc.stop)
		if r1 > 10+1e-8 || r2 > 10+1e-8 || q1 >= q2 {
			t.Fatalf("risk was not split: market=%g limit=%g risk=%g/%g", q1, q2, r1, r2)
		}
	}
}

func TestParticipationMessagePipelineKeepsOldPolicyAndDeduplication(t *testing.T) {
	for _, policy := range []string{EntryPolicySplit, EntryPolicyParticipate} {
		t.Run(policy, func(t *testing.T) {
			_, venue, st := managedExecutor(t)
			venue.market = 83829.2
			e := liveTestEngine(t, venue, st, "tyler_v1")
			e.cfg.EntryPolicy, e.cfg.RiskAmountUSD = policy, 20
			ins := participationInstruction()
			ins.Direction = DirectionShort
			ins.EntryOrders[0].Price.Price = 85100
			ins.StopLossLevels[0].Price.Price = 86100
			ins.TakeProfitLevels = []TPLevel{{Price: PriceSpec{Type: PriceFixed, Price: 82700}}, {Price: PriceSpec{Type: PriceFixed, Price: 81200}}}
			response, err := json.Marshal(ins)
			if err != nil {
				t.Fatal(err)
			}
			llm := &fixtureLLM{response: string(response)}
			e.llm = llm
			msg := &store.DiscordMessage{MessageID: "btc-reference", ChannelID: "chan-1", MessageTimestamp: time.Now(),
				Content: "#BTC/USDT 空📉\n進場：市價-85100\n止盈：82700-81200\n止損：86100"}
			if err := e.HandleMessage(msg, false); err != nil {
				t.Fatal(err)
			}
			signals, err := st.CopyTrade().GetRecentSignals(e.traderID, time.Time{}, time.Time{}, 10)
			if err != nil || len(signals) != 1 || signals[0].Status != store.SignalStatusExecuted {
				t.Fatalf("pipeline did not execute: %+v %v", signals, err)
			}
			var frozen MessageRules
			if err := json.Unmarshal([]byte(signals[0].RulesSnapshotJSON), &frozen); err != nil || frozen.EntryPolicy != policy {
				t.Fatalf("pipeline lost selected policy: %+v %v", frozen, err)
			}
			calls := entryCalls(venue)
			if policy == EntryPolicySplit {
				if len(calls) != 1 || calls[0].Type != "LIMIT" || calls[0].Price != 85100 {
					t.Fatalf("old admission policy changed: %+v", calls)
				}
			} else if len(calls) != 2 || calls[0].Type != "MARKET" || calls[1].Type != "LIMIT" || calls[1].Price != 85100 {
				t.Fatalf("participation policy not applied: %+v", calls)
			}
			if err := e.HandleMessage(msg, false); err != nil {
				t.Fatal(err)
			}
			if llm.calls != 1 || len(entryCalls(venue)) != len(calls) {
				t.Fatal("duplicate message reinterpreted or resubmitted")
			}
		})
	}
}
