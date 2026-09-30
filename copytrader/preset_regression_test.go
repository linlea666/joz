package copytrader

import (
	"encoding/json"
	"fmt"
	"math"
	"nofx/store"
	"strings"
	"testing"
	"time"
)

func presetInterpret(body, profile string, ratio float64, fallback *SourceInterpretation) *SourceInterpretation {
	msg := &store.DiscordMessage{Content: body, MessageID: "card", MessageTimestamp: time.Now()}
	sources := BuildSourceSegments(msg, profile)
	result := InterpretKnownSource(msg, sources, profile)
	if result == nil {
		result = fallback
	}
	result = ApplySourcePolicy(result, msg, sources, profile)
	return ApplyMessageRules(result, msg, sources, MessageRules{Version: MessageRulesVersion, Profile: profile, ReduceRatio: ratio, EntryPolicy: EntryPolicyLegacy})
}

func TestPresetReductionRatioEndToEnd(t *testing.T) {
	for _, profile := range []string{"tyler_v1", "cmm_v1", "jonzi_v1"} {
		for _, tc := range []struct {
			body      string
			want      float64
			ambiguous bool
		}{
			{"#BTC 减仓剩余仓位 25%", 25, false}, {"#BTC 减仓四分之一", 25, false}, {"#BTC 平仓四分之一", 25, false}, {"#BTC close 1/4", 25, false}, {"#BTC 减仓百分之二十五", 25, false}, {"#BTC 减仓三成", 30, false}, {"#BTC 减仓两成半", 25, false}, {"#BTC 减仓25%～25%", 0, true}, {"#BTC 减仓一到三成", 0, true}, {"#BTC reduce 1/4—1/4", 0, true}, {"#BTC 减仓百分之二十到三十", 0, true},
			{"#BTC reduce 1/4", 25, false}, {"#BTC 减仓", 60, false},
			{"#BTC 减仓20%-30%", 0, true}, {"#BTC reduce 1/0", 0, true}, {"#BTC reduce -1/4", 0, true}, {"#BTC 减仓零分之一", 0, true}, {"#BTC 减仓十分之二十五", 0, true},
			{"#BTC 减仓25%，reduce 50%", 0, true},
		} {
			t.Run(profile+tc.body, func(t *testing.T) {
				old := 50.0
				ins := presetInterpret(tc.body, profile, 60, &SourceInterpretation{Classification: ClassificationSignal, Action: ActionReduce, Symbol: "BTC", CloseRatio: &old})
				if tc.ambiguous {
					if ins.IsActionable() {
						t.Fatal("ambiguous ratio executable", ins)
					}
					return
				}
				if ins.CloseRatio == nil || *ins.CloseRatio != tc.want {
					t.Fatalf("ratio %+v want %v", ins, tc.want)
				}
			})
		}
	}
}

func TestPresetDiscretionaryManagementIsolation(t *testing.T) {
	for _, profile := range []string{"cmm_v1", "jonzi_v1"} {
		for _, body := range []string{"#BTC 可以先跑", "#BTC 自行止盈"} {
			ins := presetInterpret(body, profile, 50, &SourceInterpretation{Classification: ClassificationSignal, Action: ActionReduce, Symbol: "BTC"})
			if ins.IsActionable() {
				t.Fatal("TYLER convention leaked", profile, body)
			}
		}
	}
	if ins := presetInterpret("#BTC 可以先跑", "tyler_v1", 25, nil); !ins.IsActionable() || *ins.CloseRatio != 25 {
		t.Fatal("TYLER convention lost", ins)
	}
}

func TestFrozenPresetAndEntryPolicy(t *testing.T) {
	st := newTestStore(t)
	cfg := DefaultCopyTradingConfig()
	cfg.InterpretationProfile = "cmm_v1"
	cfg.EntryPolicy = EntryPolicySplit
	snapshot := cfg.MessageRules().Snapshot()
	sig := &store.CopyTradeSignal{ID: "frozen", TraderID: "trader-1", RulesSnapshotJSON: snapshot}
	if err := st.CopyTrade().CreateSignal(sig); err != nil {
		t.Fatal(err)
	}
	cfg.InterpretationProfile = "tyler_v1"
	cfg.EntryPolicy = EntryPolicyLegacy
	e := &Engine{cfg: &cfg, st: st, traderID: "trader-1"}
	rules, err := e.rulesForSignal("frozen")
	if err != nil || rules.Profile != "cmm_v1" || rules.EntryPolicy != EntryPolicySplit {
		t.Fatal(rules, err)
	}
	var old map[string]interface{}
	_ = json.Unmarshal([]byte(snapshot), &old)
	old["version"] = 1
	delete(old, "entry_policy")
	b, _ := json.Marshal(old)
	_ = st.CopyTrade().UpdateSignal("frozen", map[string]interface{}{"rules_snapshot_json": string(b)})
	rules, err = e.rulesForSignal("frozen")
	if err != nil || rules.EntryPolicy != "" {
		t.Fatal("missing legacy policy was guessed", rules, err)
	}
}

const jonziTestCard = "## BTC/USDT long\n### Trade details\n- Entry: 100\n- Stop/loss: 90\n- TP: 120\n## BTC/USDT 多(long)\n### 交易详情\n- 入场价: 100.0\n- 止损: 90\n- 止盈: 120"

func TestJonziTerminalCardExactRootExecution(t *testing.T) {
	for _, rootMatch := range []bool{false, true} {
		for _, pending := range []bool{false, true} {
			t.Run(fmt.Sprintf("match=%v/pending=%v", rootMatch, pending), func(t *testing.T) {
				x, v, st := managedExecutor(t)
				p := splitPlan()
				if pending {
					p.SplitReference = 0
					p.EntryType = EntryPlanLimit
					p.EntryPrice = 99
				}
				ctx, err := x.ExecuteOpen("open", "s-open", p)
				if err != nil {
					t.Fatal(err)
				}
				e := liveTestEngine(t, v, st, "jonzi_v1")
				id := "different-original"
				if rootMatch {
					id = ctx.RootMessageID
				}
				now := time.Now()
				msg := &store.DiscordMessage{MessageID: id, ChannelID: ctx.ChannelID, Content: jonziTestCard + "\nTrade was manually closed\n交易已手动平仓", MessageTimestamp: now, EditedAt: &now, Revision: 1}
				before := len(v.calls)
				if err = e.HandleMessage(msg, true); err != nil {
					t.Fatal(err)
				}
				sig, _ := st.CopyTrade().LatestSignal(e.traderID, id, 1)
				saved, _ := st.CopyTrade().GetContext(ctx.ID)
				if rootMatch {
					if v.qty != 0 || (!saved.EntryDisabled && !pending) || sig.Status != store.SignalStatusExecuted {
						t.Fatalf("original card did not finish: qty=%v state=%s signal=%+v", v.qty, saved.State, sig)
					}
					for _, o := range v.orders {
						if !orderTerminal(o.Status) && ((o.PositionSide == "LONG" && o.Side == "BUY") || (o.PositionSide == "SHORT" && o.Side == "SELL")) {
							t.Fatalf("pending entry survives: %+v", o)
						}
					}
				} else if len(v.calls) != before || sig.SkipReason != string(SkipNeedsContext) {
					t.Fatalf("unrelated same-symbol trade affected: %+v", sig)
				}
				runs, _ := st.CopyTrade().GetAIRun(e.traderID, sig.AIRunID)
				if runs == nil || runs.Model != "deterministic:jonzi_v1" {
					t.Fatalf("wrong interpreter evidence: %+v", runs)
				}
			})
		}
	}
}

func TestJonziQuotedTerminalDoesNotClose(t *testing.T) {
	msg := &store.DiscordMessage{MessageID: "quote", Content: "收益播报\n> " + strings.ReplaceAll(jonziTestCard+"\nTrade was manually closed", "\n", "\n> "), MessageTimestamp: time.Now()}
	sources := BuildSourceSegments(msg, "jonzi_v1")
	result := ApplyMessageRules(ApplySourcePolicy(&SourceInterpretation{Classification: ClassificationSignal, Action: ActionClose, Symbol: "BTC", Direction: DirectionLong}, msg, sources, "jonzi_v1"), msg, sources, MessageRules{Profile: "jonzi_v1", ReduceRatio: 50})
	if result.IsActionable() {
		if skip, _ := ValidateActionEvidence(result, sources); skip == SkipNone {
			t.Fatal("reference terminal acquired closing authority")
		}
	}
}

func TestJonziCardTranslationRules(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		conflict   bool
	}{
		{"same", jonziTestCard, false}, {"price", strings.Replace(jonziTestCard, "入场价: 100.0", "入场价: 101", 1), true},
		{"direction", strings.Replace(jonziTestCard, "多(long)", "空(short)", 1), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			makeIns := func() *SourceInterpretation {
				return &SourceInterpretation{Classification: ClassificationSignal, Action: ActionOpen, Symbol: "BTC", Direction: DirectionLong, EntryOrders: []EntryOrder{{OrderType: EntryMarket, Price: PriceSpec{Type: PriceMarket}}}, StopLossLevels: []SLLevel{{Price: PriceSpec{Type: PriceFixed, Price: 90}}}, TakeProfitLevels: []TPLevel{{Price: PriceSpec{Type: PriceFixed, Price: 120}}}}
			}
			a, b := makeIns(), makeIns()
			b.Symbol = "BTCUSDT"
			b.Reasoning = "中文解释"
			b.ConditionalRules = []ConditionalRule{}
			got := presetInterpret(tc.body, "jonzi_v1", 50, &SourceInterpretation{Classification: ClassificationSignal, Instructions: []*SourceInterpretation{a, b}})
			if len(got.Flatten()) != 1 {
				t.Fatal("translation not merged")
			}
			if tc.conflict && got.Flatten()[0].IsActionable() {
				t.Fatal("card conflict executable")
			}
			if !tc.conflict && !got.Flatten()[0].IsActionable() {
				t.Fatal("matching card rejected")
			}
		})
	}
}

func TestPresetConditionsAndRecaps(t *testing.T) {
	for _, profile := range []string{"cmm_v1", "tyler_v1", "jonzi_v1"} {
		for _, body := range []string{"#BTC TP1 已到", "#BTC 止盈已到 收益20%"} {
			ins := presetInterpret(body, profile, 60, &SourceInterpretation{Classification: ClassificationSignal, Symbol: "BTC", Action: ActionReduce})
			if ins.IsActionable() {
				t.Fatal("recap executable", profile, body)
			}
		}
		for _, tc := range []struct {
			body  string
			add   bool
			tp    int
			heavy bool
		}{
			{"#BTC 2%～5% 仓位，20～30倍\n减仓25%", false, 0, false},
			{"#BTC 有补仓的 减仓25%", true, 0, false},
			{"#BTC TP1 成交后 减仓25%", false, 1, false},
			{"#BTC 仓位重的 减仓25%", false, 0, true},
		} {
			ins := presetInterpret(tc.body, profile, 60, &SourceInterpretation{Classification: ClassificationSignal, Symbol: "BTC", Action: ActionReduce})
			if ins.CloseRatio == nil || *ins.CloseRatio != 25 || ins.RequiresAddFill != tc.add || ins.RequiresTPFill != tc.tp || (tc.heavy && ins.IsActionable()) {
				t.Fatalf("%s %s: %+v", profile, tc.body, ins)
			}
		}
	}
}

func TestDualPriceAndSingleReferencePolicyMatrix(t *testing.T) {
	for _, dir := range []Direction{DirectionLong, DirectionShort} {
		for _, dual := range []string{DualPriceLegacy, DualPriceRange, DualPriceSplit, DualPriceReject} {
			for _, policy := range []string{EntryPolicyLegacy, EntryPolicySplit} {
				for _, kind := range []string{"dual", "single", "plain", "limit", "explicit-range", "explicit-two"} {
					t.Run(string(dir)+dual+policy+kind, func(t *testing.T) {
						b, market, stop, tp := 95.0, 100.1, 90.0, 120.0
						if dir == DirectionShort {
							b, market, stop, tp = 105, 99.9, 110, 80
						}
						entry := EntryOrder{OrderType: EntryMarket, Price: PriceSpec{Type: PriceMarket, Price: 100}}
						body := "#BTC 进场：市价100"
						if kind == "dual" || strings.HasPrefix(kind, "explicit") {
							body += fmt.Sprintf("—%g", b)
						}
						orders := []EntryOrder{entry}
						switch kind {
						case "plain":
							body = "#BTC 进场：市价"
							orders[0].Price.Price = 0
						case "limit":
							body = "#BTC 进场：限价100"
							orders[0] = EntryOrder{OrderType: EntryLimit, Price: PriceSpec{Type: PriceFixed, Price: 100}}
						case "explicit-range":
							body += " 区间"
							// Deliberately leave the model's single market order; author text must win.
						case "explicit-two":
							body += " 两笔订单"
							// Deliberately leave the model's single market order; author text must win.
						}
						msg := &store.DiscordMessage{Content: body}
						ins := &SourceInterpretation{Classification: ClassificationSignal, Action: ActionOpen, Symbol: "BTC", Direction: dir, EntryOrders: orders, StopLossLevels: []SLLevel{{Price: PriceSpec{Type: PriceFixed, Price: stop}}}, TakeProfitLevels: []TPLevel{{Price: PriceSpec{Type: PriceFixed, Price: tp}}}}
						ins = ApplyMessageRules(ins, msg, BuildSourceSegments(msg, "cmm_v1"), MessageRules{Profile: "cmm_v1", ReduceRatio: 50, DualPriceMode: dual, EntryPolicy: policy})
						if dual == DualPriceReject && kind == "dual" {
							if ins.IsActionable() || len(ins.EntryOrders) > 0 {
								t.Fatal("rejected dual executable")
							}
							return
						}
						plan, skip, err := resolveEntryPlan(dir, ins.EntryOrders, market, 1, true, policy)
						if err != nil || skip != SkipNone {
							t.Fatal(plan, skip, err)
						}
						want := 0.0
						if kind == "explicit-two" || (kind == "dual" && dual == DualPriceSplit) {
							want = b
						} else if (kind == "single" || (kind == "dual" && dual == DualPriceLegacy)) && policy == EntryPolicySplit {
							want = 100
						}
						if plan.SplitReference != want {
							t.Fatalf("duplicated/lost/replaced legs: %+v want second %g", plan, want)
						}
					})
				}
			}
		}
	}
}

func TestEntryPlanSingleReferenceBoundaries(t *testing.T) {
	for _, dir := range []Direction{DirectionLong, DirectionShort} {
		for _, p := range []struct {
			market float64
			split  bool
			kind   EntryPlanType
		}{{99, false, EntryPlanMarket}, {100, false, EntryPlanMarket}, {100.5, true, EntryPlanMarket}, {101, true, EntryPlanMarket}, {101.00001, false, EntryPlanLimit}} {
			market := p.market
			if dir == DirectionShort {
				market = 200 - market
			}
			orders := []EntryOrder{{OrderType: EntryMarket, Price: PriceSpec{Type: PriceMarket, Price: 100}}}
			plan, _, err := resolveEntryPlan(dir, orders, market, 1, true, EntryPolicySplit)
			if err != nil || (plan.SplitReference > 0) != p.split || plan.Decision.OrderType != p.kind {
				t.Fatal(dir, p, plan, err)
			}
		}
	}
}

func TestReplayPreservesRuleSnapshotAcrossReload(t *testing.T) {
	st := newTestStore(t)
	cfg := DefaultCopyTradingConfig()
	cfg.InterpretationProfile = "cmm_v1"
	cfg.EntryPolicy = EntryPolicySplit
	item := ReplayItem{MessageID: "snapshot", RulesSnapshotJSON: cfg.MessageRules().Snapshot(), EvaluationScope: "recognition only", UncheckedGates: []string{"balance", "ttl"}}
	if err := st.CopyTrade().CreateReplay(&store.CopyTradeReplay{ID: "rules", TraderID: "trader-1", Status: ReplayDone, Total: 1, Done: 1}); err != nil {
		t.Fatal(err)
	}
	if err := st.CopyTrade().CreateReplayItem(replayItemToStore("rules", 0, item)); err != nil {
		t.Fatal(err)
	}
	e := &Engine{traderID: "trader-1", st: st}
	report := e.ReplayStatus()
	got := report.Items[0]
	if got.RulesSnapshotJSON != item.RulesSnapshotJSON || got.EvaluationScope != item.EvaluationScope || strings.Join(got.UncheckedGates, ",") != "balance,ttl" {
		t.Fatalf("replay lost audit: %+v", got)
	}
}

func TestAuthorPresetValidationAndUnknownSnapshots(t *testing.T) {
	cfg := DefaultCopyTradingConfig()
	cfg.PrimaryChannelID = "123"
	for _, p := range InterpretationPresets() {
		cfg.InterpretationProfile = p.ID
		raw, _ := cfg.Encode()
		parsed, err := ParseCopyTradingConfig(raw)
		if err != nil || parsed.InterpretationProfile != p.ID {
			t.Fatal(p.ID, err)
		}
	}
	cfg.InterpretationProfile = "unknown"
	if cfg.Validate() == nil {
		t.Fatal("unknown profile accepted")
	}
	st := newTestStore(t)
	e := &Engine{cfg: &cfg, st: st}
	for _, tc := range []struct{ id, snapshot string }{{"missing", ""}, {"unknown-profile", `{"version":2,"interpretation_profile":"unknown","default_reduce_ratio":50,"entry_policy":"legacy"}`}, {"unknown-policy", `{"version":2,"interpretation_profile":"cmm_v1","default_reduce_ratio":50,"entry_policy":"new"}`}} {
		if err := st.CopyTrade().CreateSignal(&store.CopyTradeSignal{ID: tc.id, RulesSnapshotJSON: tc.snapshot}); err != nil {
			t.Fatal(err)
		}
		if _, err := e.rulesForSignal(tc.id); err == nil {
			t.Fatal("unknown historical rules guessed", tc.id)
		}
	}
}

func TestExplicitTwoLegEntryAuditUsesOneRiskBudget(t *testing.T) {
	x, v, st := managedExecutor(t)
	cfg := DefaultCopyTradingConfig()
	cfg.PrimaryChannelID = "chan-1"
	cfg.EntryPolicy = EntryPolicySplit
	cfg.MajorPriceOffsetPct = 2
	cfg.RiskAmountUSD = 10
	e := &Engine{traderID: "trader-1", cfg: &cfg, st: st, exec: x, events: x.events}
	ins := &SourceInterpretation{Action: ActionOpen, Symbol: "BTC", Direction: DirectionLong, EntryOrders: []EntryOrder{{OrderType: EntryMarket, Price: PriceSpec{Type: PriceMarket, Price: 100}}, {OrderType: EntryLimit, Price: PriceSpec{Type: PriceFixed, Price: 95}}}, StopLossLevels: []SLLevel{{Price: PriceSpec{Type: PriceFixed, Price: 90}}}, TakeProfitLevels: []TPLevel{{Price: PriceSpec{Type: PriceFixed, Price: 120}}}}
	skip, err := e.routeOpen("plan", "plan", &store.DiscordMessage{MessageID: "plan", ChannelID: "chan-1", MessageTimestamp: time.Now()}, ins, "BTCUSDT", 101)
	if err != nil || skip != SkipNone {
		t.Fatal(skip, err)
	}
	count, risk := 0, 0.0
	for _, o := range v.calls {
		if !o.ReduceOnly {
			count++
			p := o.Price
			if o.Type == "MARKET" {
				p = v.market
			}
			risk += o.Quantity * math.Abs(p-90)
			if o.Type == "LIMIT" && p != 95 {
				t.Fatal("second leg replaced by A")
			}
		}
	}
	if count != 2 || risk > 10+1e-8 {
		t.Fatalf("entries=%d risk=%v", count, risk)
	}
	var events []store.CopyTradeEvent
	st.GormDB().Where("signal_id = ? AND event = ?", "plan", EvEntryDecision).Find(&events)
	if len(events) != 1 {
		t.Fatalf("expected one final plan event: %d", len(events))
	}
	var event struct {
		Plan       resolvedEntryPlan
		Legs       []map[string]interface{}
		RiskBudget float64 `json:"risk_budget"`
	}
	if err = json.Unmarshal([]byte(events[0].ContextJSON), &event); err != nil || len(event.Legs) != 2 || event.Plan.Origin != "explicit_market_limit" || event.RiskBudget != 10 {
		t.Fatalf("incomplete final plan %+v %v", event, err)
	}
}

func TestProfileSwitchCannotChangeCompletedActionIdentity(t *testing.T) {
	x, v, st := managedExecutor(t)
	ctx, err := x.ExecuteOpen("open", "s-open", splitPlan())
	if err != nil {
		t.Fatal(err)
	}
	e := liveTestEngine(t, v, st, "tyler_v1")
	msg := &store.DiscordMessage{MessageID: "close", ChannelID: ctx.ChannelID, MessageTimestamp: time.Now(), Content: "#BTC 减仓25%"}
	if err = e.HandleMessage(msg, false); err != nil {
		t.Fatal(err)
	}
	before := v.qty
	e = liveTestEngine(t, v, st, "cmm_v1")
	msg.Revision++
	// Claim directly with the same normalized action after the preset changed.
	ratio := 25.0
	_, claimed, err := e.claimInstruction("new-revision", msg, &SourceInterpretation{Action: ActionReduce, Direction: DirectionLong, Symbol: "BTC", CloseRatio: &ratio}, "BTCUSDT")
	if err != nil || claimed || v.qty != before {
		t.Fatal("preset change bypassed action identity", claimed, err)
	}
}

func TestImageRatioKeepsTextConditionsAndPresetBoundary(t *testing.T) {
	ratio := 25.0
	msg := &store.DiscordMessage{Content: "#BTC TP1 成交后 减仓"}
	sources := []SourceSegment{{ID: "body", Role: "current", Text: msg.Content}, {ID: "image", Role: "current", Image: true}}
	ins := &SourceInterpretation{Classification: ClassificationSignal, Action: ActionReduce, Symbol: "BTC", CloseRatio: &ratio, ActionEvidence: &ActionEvidence{SourceID: "image", Text: "reduce 25%"}}
	got := ApplyMessageRules(ins, msg, sources, MessageRules{Profile: "cmm_v1", ReduceRatio: 60})
	if *got.CloseRatio != 25 || got.RequiresTPFill != 1 {
		t.Fatal("image ratio or text condition lost", got)
	}
	msg.Content = "#BTC 可以先跑"
	sources[0].Text = msg.Content
	got = ApplyMessageRules(ins, msg, sources, MessageRules{Profile: "cmm_v1", ReduceRatio: 60})
	if got.IsActionable() {
		t.Fatal("image evidence bypassed vague management rule")
	}
}

func TestJonziCardCannotOverrideDirectionOrSymbol(t *testing.T) {
	for _, ins := range []*SourceInterpretation{
		{Classification: ClassificationSignal, Action: ActionUpdateSL, Symbol: "BTC", Direction: DirectionShort},
		{Classification: ClassificationSignal, Action: ActionUpdateSL, Symbol: "ETH", Direction: DirectionLong},
	} {
		got := presetInterpret(jonziTestCard, "jonzi_v1", 50, ins)
		if got.IsActionable() {
			t.Fatal("model contradicted native identity", got)
		}
	}
}

func TestLegacySnapshotOnlyBlocksEntryWhenPolicyIsNecessary(t *testing.T) {
	orders := []EntryOrder{{OrderType: EntryMarket, Price: PriceSpec{Type: PriceMarket, Price: 100}}}
	for _, tc := range []struct {
		market float64
		skip   SkipReason
	}{{99, SkipNone}, {100, SkipNone}, {100.5, SkipNeedsContext}, {102, SkipNone}} {
		p, skip, err := resolveEntryPlan(DirectionLong, orders, tc.market, 1, true, "")
		if skip != tc.skip || (tc.skip == SkipNone && err != nil) || p.SplitReference > 0 {
			t.Fatal("legacy strategy guessed or unnecessarily blocked", tc, p, skip, err)
		}
	}
}

func TestJonziEquivalentPartialCloseAndReduceMerged(t *testing.T) {
	ratio := 25.0
	a := &SourceInterpretation{Classification: ClassificationSignal, Action: ActionClose, CloseMode: CloseModePartial, CloseRatio: &ratio, Symbol: "BTC", Direction: DirectionLong}
	b := *a
	b.Action = ActionReduce
	b.CloseMode = ""
	got := presetInterpret(jonziTestCard+"\n减仓四分之一\nclose 1/4", "jonzi_v1", 50, &SourceInterpretation{Classification: ClassificationSignal, Instructions: []*SourceInterpretation{a, &b}})
	if len(got.Flatten()) != 1 || !got.IsActionable() || got.Flatten()[0].Action != ActionReduce {
		t.Fatalf("partial translation duplicated/rejected: %+v", got)
	}
}

func TestPartialCloseAliasRetainsHistoricalDeduplication(t *testing.T) {
	for _, status := range []string{"done", "uncertain"} {
		x, v, st := managedExecutor(t)
		ctx, err := x.ExecuteOpen("open", "s-open", splitPlan())
		if err != nil {
			t.Fatal(err)
		}
		e := liveTestEngine(t, v, st, "jonzi_v1")
		ratio := 25.0
		old := &SourceInterpretation{Classification: ClassificationSignal, Action: ActionClose, CloseMode: CloseModePartial, CloseRatio: &ratio, Symbol: "BTC", Direction: DirectionLong}
		payload, _ := json.Marshal(old)
		a := &store.CopyTradeAction{ID: "old", TraderID: e.traderID, MessageID: "partial", Symbol: "BTCUSDT", Direction: "LONG", ContextID: ctx.ID, Action: string(ActionClose), Status: status, PayloadJSON: string(payload)}
		if _, err = st.CopyTrade().ClaimAction(a); err != nil {
			t.Fatal(err)
		}
		ins := *old
		ins.Action = ActionReduce
		_, claimed, err := e.claimInstruction("new", &store.DiscordMessage{MessageID: "partial", ChannelID: ctx.ChannelID}, &ins, "BTCUSDT")
		if err != nil || claimed {
			t.Fatal("historical partial close repeated", status, claimed, err)
		}
	}
}
