package copytrader

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"nofx/mcp"
	"nofx/store"
)

// An adversarial model: whatever the message, it proposes opening and adding.
// jonzi_v1 may only let that through for a revision-0 native card.
const jonziAdversarialOpen = `{"classification":"SIGNAL","instructions":[` +
	`{"classification":"SIGNAL","action":"OPEN","symbol":"BTC","direction":"LONG","entry_orders":[{"order_type":"LIMIT","price":{"type":"FIXED","price":77005.1}}],"stop_loss_levels":[{"price":{"type":"FIXED","price":76851.3}}],"take_profit_levels":[{"price":{"type":"FIXED","price":77774}}],"action_evidence":{"source_id":"body","text":"long"}},` +
	`{"classification":"SIGNAL","action":"ADD","symbol":"BTC","direction":"LONG","entry_orders":[{"order_type":"LIMIT","price":{"type":"FIXED","price":77005.1}}],"stop_loss_levels":[{"price":{"type":"FIXED","price":76851.3}}],"action_evidence":{"source_id":"embed:0","text":"long"}}]}`

type jonziCountingLLM struct {
	mcp.AIClient
	response string
	calls    int
}

func (l *jonziCountingLLM) CallWithRequest(*mcp.Request) (string, error) {
	l.calls++
	return l.response, nil
}

func jonziInterpretEngine(t *testing.T, response string) (*Engine, *jonziCountingLLM) {
	t.Helper()
	cfg := DefaultCopyTradingConfig()
	cfg.PrimaryChannelID = jonziSampleChannel
	cfg.InterpretationProfile = "jonzi_v1"
	cfg.ParseImages, cfg.SendPositionSnapshot, cfg.SignalContextEnabled = false, false, false
	llm := &jonziCountingLLM{response: response}
	e := NewEngine(EngineParams{TraderID: "trader-1", Config: &cfg, Store: newTestStore(t), Exchange: &mockExchange{}, LLM: llm})
	return e, llm
}

type jonziExpectation struct {
	ID             string    `json:"id"`
	Symbol         string    `json:"symbol"`
	Direction      Direction `json:"direction"`
	ExpectedAction Action    `json:"expected_action"`
	Kind           string    `json:"kind"`
	Stop           float64   `json:"stop"`
	ModelCalled    bool      `json:"model_called"`
}

func jonziExpectations(t *testing.T) map[string]jonziExpectation {
	t.Helper()
	b, err := os.ReadFile("../testdata/copytrading/jonzi_regressions.json")
	if err != nil {
		t.Fatal(err)
	}
	var rows []jonziExpectation
	if err = json.Unmarshal(b, &rows); err != nil {
		t.Fatal(err)
	}
	out := map[string]jonziExpectation{}
	for _, r := range rows {
		out[r.ID] = r
	}
	return out
}

func assertNoJonziOpen(t *testing.T, label string, interp *SourceInterpretation) {
	t.Helper()
	items := append(interp.Flatten(), interp)
	for _, ins := range items {
		if ins.Action == ActionOpen || ins.Action == ActionAdd {
			t.Fatalf("%s: OPEN/ADD survived: %+v", label, ins)
		}
	}
}

// Every message of the real channel sample, as an edit (its recorded state)
// and as if first seen at revision 0 (backfill): exits are deterministic,
// commentary never reaches the model, and nothing can open or add.
func TestJonziChannelSampleRouting(t *testing.T) {
	want := jonziExpectations(t)
	messages := jonziSampleMessages(t)
	if len(messages) != 100 || len(want) != 100 {
		t.Fatalf("sample size %d / expectations %d", len(messages), len(want))
	}
	kinds := map[string]int{}
	for _, msg := range messages {
		kinds[want[msg.MessageID].Kind]++
	}
	if kinds["terminal_usdt"] != 33 || kinds["terminal_other"] != 11 || kinds["trailed"] != 1 || kinds["commentary_with_card"] != 11 || kinds["non_card"] != 44 {
		t.Fatalf("expectation table drifted: %v", kinds)
	}
	for _, msg := range messages {
		exp, ok := want[msg.MessageID]
		if !ok {
			t.Fatalf("no expectation for %s", msg.MessageID)
		}
		revisions := []int{0}
		if msg.EditedAt != nil {
			revisions = []int{1, 0}
		}
		for _, rev := range revisions {
			t.Run(fmt.Sprintf("%s/%s/rev%d", exp.Kind, msg.MessageID, rev), func(t *testing.T) {
				m := *msg
				m.Revision = rev
				e, llm := jonziInterpretEngine(t, jonziAdversarialOpen)
				interp, run, _, err := e.interpret("t", "", &m, rev > 0, true)
				if err != nil {
					t.Fatal(err)
				}
				if (llm.calls > 0) != exp.ModelCalled {
					t.Fatalf("model called=%d, want %v (%+v)", llm.calls, exp.ModelCalled, interp)
				}
				if !exp.ModelCalled && (run == nil || run.Model != "deterministic:jonzi_v1") {
					t.Fatalf("deterministic route not recorded: %+v", run)
				}
				assertNoJonziOpen(t, "interpret", interp)
				sources := e.messageSources(&m, "jonzi_v1")
				switch exp.ExpectedAction {
				case ActionClose, ActionUpdateSL:
					if len(interp.Flatten()) != 1 || interp.Action != exp.ExpectedAction || interp.Classification != ClassificationSignal {
						t.Fatalf("want %s: %+v", exp.ExpectedAction, interp)
					}
					if interp.Symbol != exp.Symbol || interp.Direction != exp.Direction || !interp.RequiresOriginalCard || interp.TradeReference.RootMessageID != m.MessageID {
						t.Fatalf("identity/association: %+v", interp)
					}
					if interp.ActionEvidence == nil || interp.ActionEvidence.SourceID != "body" || !strings.Contains(interp.ActionEvidence.Text, ":ChromaAlert:") || strings.Contains(interp.ActionEvidence.Text, "Trade details") {
						t.Fatalf("evidence must be the status line: %+v", interp.ActionEvidence)
					}
					if skip, detail, err := ValidateActionEvidenceDetailed(interp, sources); skip != SkipNone || err != nil {
						t.Fatalf("source gate blocked deterministic %s: %s %s %v", exp.ExpectedAction, skip, detail, err)
					}
					if exp.ExpectedAction == ActionClose && (interp.CloseMode != CloseModeFull || (interp.CloseRatio != nil && *interp.CloseRatio != 100)) {
						t.Fatalf("close is not full: %+v", interp)
					}
					if exp.ExpectedAction == ActionUpdateSL {
						sl := interp.StopLossLevels
						if len(sl) != 1 || sl[0].Price.Type != PriceFixed || sl[0].Price.Price != exp.Stop {
							t.Fatalf("trailed stop: %+v", sl)
						}
					}
				default:
					for _, ins := range interp.Flatten() {
						if ins.IsActionable() {
							t.Fatalf("%s produced an action: %+v", exp.Kind, ins)
						}
					}
					if exp.ModelCalled && !strings.Contains(strings.Join(interp.Flatten()[0].Warnings, ";"), "downgraded to IGNORE") {
						t.Fatalf("model OPEN downgrade not recorded: %+v", interp.Flatten()[0])
					}
				}
			})
		}
	}
}

// The five terminal cards that the reasoning/sizing text used to block.
func TestJonziTerminalCardsNotBlockedByAuthorProse(t *testing.T) {
	for _, id := range []string{"1544313018068770859", "1541450528075153559", "1536740358673801326", "1527340089498406965", "1526639480730288229"} {
		t.Run(id, func(t *testing.T) {
			msg := jonziSample(t, id)
			msg.Revision = 1
			e, llm := jonziInterpretEngine(t, jonziAdversarialOpen)
			interp, _, _, err := e.interpret("t", "", msg, true, true)
			if err != nil || llm.calls != 0 || interp.Action != ActionClose || interp.Classification != ClassificationSignal {
				t.Fatalf("%+v %v", interp, err)
			}
			if skip, detail, _ := ValidateActionEvidenceDetailed(interp, e.messageSources(msg, "jonzi_v1")); skip != SkipNone {
				t.Fatalf("blocked: %s %s", skip, detail)
			}
		})
	}
}

// Older frozen rule snapshots (v1-v3) take the same source-policy path.
func TestJonziRoutingAllRuleVersions(t *testing.T) {
	want := jonziExpectations(t)
	fallback := func() *SourceInterpretation {
		p, err := ParseInterpretation(jonziAdversarialOpen)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	for v := 1; v <= MessageRulesVersion; v++ {
		for _, msg := range jonziSampleMessages(t) {
			msg.Revision = 1
			exp := want[msg.MessageID]
			sources := BuildSourceSegments(msg, "jonzi_v1")
			result := interpretKnownVersion(msg, sources, "jonzi_v1", v)
			if (result == nil) != exp.ModelCalled {
				t.Fatalf("v%d %s: model route mismatch %+v", v, msg.MessageID, result)
			}
			if result == nil {
				result = fallback()
			}
			result = applySourcePolicyVersion(result, msg, sources, "jonzi_v1", v)
			result = ApplyMessageRules(result, msg, sources, MessageRules{Version: v, Profile: "jonzi_v1", ReduceRatio: 50, EntryPolicy: EntryPolicyLegacy})
			assertNoJonziOpen(t, fmt.Sprintf("v%d %s", v, msg.MessageID), result)
			if exp.ExpectedAction == ActionClose || exp.ExpectedAction == ActionUpdateSL {
				if result.Action != exp.ExpectedAction || !result.IsActionable() {
					t.Fatalf("v%d %s: want %s got %+v", v, msg.MessageID, exp.ExpectedAction, result)
				}
			} else if result.IsActionable() {
				t.Fatalf("v%d %s: actionable %+v", v, msg.MessageID, result)
			}
		}
	}
}

// A revision-0 native card still goes to the model and its OPEN passes; the
// same card as an edit is a deterministic IGNORE and a model OPEN is downgraded.
func TestJonziFreshCardOpenOnlyAtRevisionZero(t *testing.T) {
	for _, tc := range []struct {
		id, symbol string
		dir        Direction
		entry, sl  float64
		tp         float64
	}{
		{"1544845105448353833", "BTC", DirectionLong, 77005.1, 76851.3, 77774},
		// Reasoning contains "if internal low" / "如果内部低点": not an entry condition.
		{"1544313018068770859", "ZEC", DirectionLong, 0, 0, 0},
	} {
		t.Run(tc.id, func(t *testing.T) {
			sample := jonziSample(t, tc.id)
			fresh := jonziFreshCard(sample.Content)
			card := parseJonziCard(fresh)
			if card.Status != jonziCardNew || card.Entry == nil || card.StopLoss == nil || len(card.TakeProfits) == 0 {
				t.Fatalf("synthetic card %+v", card)
			}
			response := fmt.Sprintf(`{"classification":"SIGNAL","action":"OPEN","symbol":%q,"direction":%q,"entry_orders":[{"order_type":"LIMIT","price":{"type":"FIXED","price":%g}}],"stop_loss_levels":[{"price":{"type":"FIXED","price":%g}}],"take_profit_levels":[{"price":{"type":"FIXED","price":%g}}],"action_evidence":{"source_id":"body","text":%q}}`,
				tc.symbol, tc.dir, card.Entry.Value, card.StopLoss.Value, card.TakeProfits[0].Value, strings.TrimSpace(card.Entry.Line))
			now := time.Now().UTC()
			msg := &store.DiscordMessage{MessageID: "fresh-" + tc.id, ChannelID: jonziSampleChannel, AuthorID: sample.AuthorID, AuthorName: sample.AuthorName, Content: fresh, AttachmentsJSON: sample.AttachmentsJSON, MessageTimestamp: now}

			e, llm := jonziInterpretEngine(t, response)
			interp, run, _, err := e.interpret("t", "", msg, false, true)
			if err != nil || llm.calls != 1 || run.Model == "deterministic:jonzi_v1" {
				t.Fatalf("rev0 card must reach the model: calls=%d run=%+v err=%v", llm.calls, run, err)
			}
			if interp.Action != ActionOpen || !interp.IsActionable() {
				t.Fatalf("rev0 OPEN rejected: %+v", interp)
			}
			if skip, detail, err := ValidateActionEvidenceDetailed(interp, e.messageSources(msg, "jonzi_v1")); skip != SkipNone || err != nil {
				t.Fatalf("rev0 OPEN blocked by source gate: %s %s %v", skip, detail, err)
			}
			if skip, err := ValidateInterpretation(interp, card.Entry.Value); skip != SkipNone || err != nil {
				t.Fatalf("rev0 OPEN invalid: %s %v", skip, err)
			}

			// Same card as an edit: deterministic IGNORE, no model call.
			edit := *msg
			edit.Revision, edit.EditedAt = 1, &now
			e, llm = jonziInterpretEngine(t, response)
			interp, run, _, err = e.interpret("t", "", &edit, true, true)
			if err != nil || llm.calls != 0 || run.Model != "deterministic:jonzi_v1" || interp.IsActionable() || interp.Action == ActionOpen {
				t.Fatalf("rev1 card: calls=%d %+v %v", llm.calls, interp, err)
			}
			// And if a model OPEN reaches the preset policy for that edit, it is downgraded.
			sources := BuildSourceSegments(&edit, "jonzi_v1")
			aiOpen, _ := ParseInterpretation(response)
			got := ApplyMessageRules(aiOpen, &edit, sources, MessageRules{Version: MessageRulesVersion, Profile: "jonzi_v1", ReduceRatio: 50, EntryPolicy: EntryPolicyLegacy})
			assertNoJonziOpen(t, "rev1 model open", got)
		})
	}
}

func TestJonziOpenGuard(t *testing.T) {
	fresh := jonziFreshCard(jonziSample(t, "1544845105448353833").Content)
	open := func(sym string, dir Direction, sourceID string) *SourceInterpretation {
		return &SourceInterpretation{Classification: ClassificationSignal, Action: ActionOpen, Symbol: sym, Direction: dir,
			EntryOrders:    []EntryOrder{{OrderType: EntryLimit, Price: PriceSpec{Type: PriceFixed, Price: 77005.1}}},
			StopLossLevels: []SLLevel{{Price: PriceSpec{Type: PriceFixed, Price: 76851.3}}}, ActionEvidence: &ActionEvidence{SourceID: sourceID, Text: "Entry"}}
	}
	rules := MessageRules{Version: MessageRulesVersion, Profile: "jonzi_v1", ReduceRatio: 50, EntryPolicy: EntryPolicyLegacy}
	run := func(content string, rev int, ins *SourceInterpretation, embeds string) *SourceInterpretation {
		msg := &store.DiscordMessage{MessageID: "m", Content: content, EmbedsJSON: embeds, Revision: rev, MessageTimestamp: time.Now()}
		sources := BuildSourceSegments(msg, "jonzi_v1")
		return ApplyMessageRules(ins, msg, sources, rules)
	}
	if got := run(fresh, 0, open("BTC", DirectionLong, "body"), ""); !got.IsActionable() || got.Action != ActionOpen {
		t.Fatalf("legitimate rev0 open lost: %+v", got)
	}
	for name, got := range map[string]*SourceInterpretation{
		"wrong direction": run(fresh, 0, open("BTC", DirectionShort, "body"), ""),
		"wrong symbol":    run(fresh, 0, open("ETH", DirectionLong, "body"), ""),
		"plain comment":   run("not the place for longs! / 这里不是做多(long)的地方, BTC long entry 77005", 0, open("BTC", DirectionLong, "body"), ""),
		"embedded card":   run("+5R booked, still got my HTF swing short going", 0, open("BTC", DirectionLong, "embed:0"), mustEmbed(t, fresh, time.Now().Add(-time.Hour))),
		"quoted card":     run("> "+strings.ReplaceAll(fresh, "\n", "\n> "), 0, open("BTC", DirectionLong, "body"), ""),
		"edited card":     run(fresh, 2, open("BTC", DirectionLong, "body"), ""),
	} {
		assertNoJonziOpen(t, name, got)
		if got.IsActionable() {
			t.Fatalf("%s: actionable %+v", name, got)
		}
	}
	// Chroma notification mode keeps its own guard; jonzi_v1 card rules do not apply.
	chroma := rules
	chroma.SourceMode = "chroma"
	msg := &store.DiscordMessage{MessageID: "n", Content: "**🔴 BTC/USDT short was added**", MessageTimestamp: time.Now()}
	if got := ApplyMessageRules(open("BTC", DirectionShort, "body"), msg, BuildSourceSegments(msg, "jonzi_v1"), chroma); !got.IsActionable() {
		t.Fatalf("chroma notification open changed: %+v", got)
	}
}

func mustEmbed(t *testing.T, description string, ts time.Time) string {
	t.Helper()
	b, err := json.Marshal([]map[string]interface{}{{"type": "rich", "description": description, "timestamp": ts.UTC().Format(time.RFC3339), "author": map[string]string{"name": "JONZi"}}})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// "+4R booked ... time to look for a long / 做多机会" commentary attaching an
// older card: the card is a reference and the message is a deterministic IGNORE.
func TestJonziCommentaryEmbedIsReference(t *testing.T) {
	msg := jonziSample(t, "1527670566444601434")
	if !strings.Contains(msg.Content, "做多") {
		t.Fatal("sample changed: expected opening wording in the commentary")
	}
	for _, s := range BuildSourceSegments(msg, "jonzi_v1") {
		if strings.HasPrefix(s.ID, "embed:") && !s.Image && s.Role != "reference" {
			t.Fatalf("embedded jonzi card stayed current: %+v", s)
		}
	}
	// Other presets' embed handling is unchanged by the jonzi card rule.
	for _, s := range BuildSourceSegments(msg, "default") {
		if s.ID == "embed:0" && s.Role != "current" {
			t.Fatalf("default profile embed role changed: %+v", s)
		}
	}
}

// End to end: a trailed-stop edit of the original card moves the stop of the
// trade rooted at that card, and leaves an unrelated same-symbol trade alone.
func TestJonziTrailedEditMovesOriginalStop(t *testing.T) {
	card := "## 🟢 BTC/USDT long\n-# **Last updated:** <t:1788395288:R>\n\n**:ChromaAlert: Stops are trailed to $`95`, resulting in `0R`:**\n\n### Reason for setup\nnice raid if internal low, 日线级别 setup\n\n### Trade details\n- Entry: $`101` :ChromaCheck2: (<t:1788390164:R>)\n- Trailed stop: $`95`\n- TP: $`120`\n- Stop/loss: $`90`\n\n### Position sizing\nFor every $`100` you want to risk on this trade...\n-----------------------\n## 🟢 BTC/USDT 多(long)\n\n### 交易详情\n- 入场：$`101` :ChromaCheck2: ()\n- 跟踪止损：$`95`\n- 止盈(TP)：$`120`\n- 止损：$`90`\n\n### 仓位规模\n如果你希望每笔交易承担 $`100` 的风险..."
	for _, rootMatch := range []bool{true, false} {
		t.Run(fmt.Sprintf("root=%v", rootMatch), func(t *testing.T) {
			x, v, st := managedExecutor(t)
			ctx, err := x.ExecuteOpen("open", "s-open", splitPlan())
			if err != nil {
				t.Fatal(err)
			}
			e := liveTestEngine(t, v, st, "jonzi_v1")
			id := "other-card"
			if rootMatch {
				id = ctx.RootMessageID
			}
			now := time.Now()
			msg := &store.DiscordMessage{MessageID: id, ChannelID: ctx.ChannelID, Content: card, MessageTimestamp: now.Add(-time.Minute), EditedAt: &now, Revision: 1}
			if err = e.HandleMessage(msg, true); err != nil {
				t.Fatal(err)
			}
			saved, _ := st.CopyTrade().GetContext(ctx.ID)
			sig, _ := st.CopyTrade().LatestSignal(e.traderID, id, 1)
			if rootMatch && saved.StopLossPrice != 95 {
				t.Fatalf("stop not moved: %v signal=%+v", saved.StopLossPrice, sig)
			}
			if !rootMatch && saved.StopLossPrice != 90 {
				t.Fatalf("unrelated trade changed: %v", saved.StopLossPrice)
			}
			run, _ := st.CopyTrade().GetAIRun(e.traderID, sig.AIRunID)
			if run == nil || run.Model != "deterministic:jonzi_v1" {
				t.Fatalf("not deterministic: %+v", run)
			}
		})
	}
}
