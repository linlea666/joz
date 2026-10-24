package copytrader

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"strings"
	"testing"
	"time"

	"nofx/store"
)

// jonzi_revisions.json is the real gateway stream of the jonzi channel
// (MESSAGE_CREATE / MESSAGE_UPDATE in arrival order), every stored revision.
type jonziRevisionEvent struct {
	Kind       string               `json:"kind"`
	MessageID  string               `json:"message_id"`
	Revision   int                  `json:"revision"`
	ReceivedAt string               `json:"received_at"`
	Message    store.DiscordMessage `json:"message"`
}

func jonziRevisionEvents(t *testing.T) []jonziRevisionEvent {
	t.Helper()
	b, err := os.ReadFile("../testdata/copytrading/jonzi_revisions.json")
	if err != nil {
		t.Fatal(err)
	}
	var events []jonziRevisionEvent
	if err = json.Unmarshal(b, &events); err != nil {
		t.Fatal(err)
	}
	if len(events) != 32 {
		t.Fatalf("revision stream has %d events, want 32", len(events))
	}
	return events
}

func (ev jonziRevisionEvent) storeMessage(t *testing.T) *store.DiscordMessage {
	t.Helper()
	m := ev.Message
	if m.MessageID != ev.MessageID || m.Revision != ev.Revision || m.AuthorID != "1453389441585647798" || m.ChannelID != jonziSampleChannel {
		t.Fatalf("event/message mismatch: %s rev%d vs %s rev%d author %s", ev.MessageID, ev.Revision, m.MessageID, m.Revision, m.AuthorID)
	}
	if ev.Revision > 0 {
		at := m.ReceivedAt
		m.EditedAt = &at
	}
	return &m
}

type jonziRevExpect struct {
	action     Action
	entry, sl  float64
	tp         float64
	stop       float64 // UPDATE_SL target
	modelRoute bool    // model is consulted (non-card messages only)
}

var jonziRevisionOpens = map[string][4]float64{ // id -> direction(+1 long/-1 short), entry, stop/loss, TP
	"1558972491056160939": {-1, 83629.9, 83796.8, 83212},
	"1559041390095245333": {-1, 83664.5, 83951.4, 49236},
	"1559215957312798730": {-1, 2523.72, 2532.17, 2473},
	"1559568340135968908": {-1, 83479.6, 83653.2, 82611},
	"1559570443957043271": {-1, 83773.3, 83943.5, 82582},
	"1559919189517668493": {-1, 82505.5, 82782.8, 49230},
	"1560102871192838296": {-1, 81676, 81907.8, 49224},
	"1560247406753349725": {1, 80822.5, 80659.3, 84087},
	"1561742956229034055": {-1, 81969.1, 82296.5, 49228},
	"1561768758442459217": {-1, 81969.1, 82296.5, 49228},
	"1562420632480645182": {1, 83362.9, 83196.3, 84196},
	"1562460012180144178": {1, 83362.9, 83196.3, 84196},
	"1562499876934127718": {-1, 84574.9, 84928.4, 49224},
	"1563191327212965910": {-1, 81974.6, 82144.5, 81001},
	"1563212570750619700": {-1, 81487.4, 81667.2, 80894.7},
	"1563227154177597630": {-1, 81487.4, 81650.1, 80875.7},
}

var jonziRevisionCommentary = []string{"1558875268695859410", "1559640556328456276", "1562483589302718494", "1563196835034566726", "1562782311315939389"}
var jonziRevisionModelMessages = []string{"1558874462814867567", "1560439312838041612", "1560439571840503909", "1560439659987869908", "1560543339194089574", "1561819490524856368"}

func jonziRevisionExpectation(t *testing.T, ev jonziRevisionEvent) jonziRevExpect {
	t.Helper()
	key := fmt.Sprintf("%s/%d", ev.MessageID, ev.Revision)
	switch key {
	case "1563212570750619700/1":
		return jonziRevExpect{action: ActionIgnore}
	case "1563212570750619700/2", "1563227154177597630/1":
		return jonziRevExpect{action: ActionClose}
	case "1562499876934127718/1", "1562499876934127718/2":
		return jonziRevExpect{action: ActionUpdateSL, stop: 84574.9}
	}
	if o, ok := jonziRevisionOpens[ev.MessageID]; ok && ev.Revision == 0 {
		return jonziRevExpect{action: ActionOpen, entry: o[1], sl: o[2], tp: o[3]}
	}
	if containsString(jonziRevisionCommentary, ev.MessageID) && ev.Revision == 0 {
		return jonziRevExpect{action: ActionIgnore}
	}
	if containsString(jonziRevisionModelMessages, ev.MessageID) && ev.Revision == 0 {
		return jonziRevExpect{action: ActionIgnore, modelRoute: true}
	}
	t.Fatalf("no expectation for %s", key)
	return jonziRevExpect{}
}

func assertJonziOpen(t *testing.T, interp *SourceInterpretation, msg *store.DiscordMessage, exp jonziRevExpect, dir Direction) {
	t.Helper()
	if len(interp.Flatten()) != 1 || interp.Action != ActionOpen || interp.Classification != ClassificationSignal || !interp.IsActionable() {
		t.Fatalf("want deterministic OPEN: %+v", interp)
	}
	if interp.Symbol != parseJonziCard(msg.Content).Symbol || interp.Direction != dir {
		t.Fatalf("identity from English header: %s %s", interp.Symbol, interp.Direction)
	}
	eo := interp.EntryOrders
	if len(eo) != 1 || eo[0].OrderType != EntryMarket || eo[0].Price.Type != PriceMarket || eo[0].Price.Price != exp.entry {
		t.Fatalf("entry must be MARKET with the card entry as reference: %+v", eo)
	}
	if sl := interp.StopLossLevels; len(sl) != 1 || sl[0].Price.Type != PriceFixed || sl[0].Price.Price != exp.sl {
		t.Fatalf("stop/loss: %+v", sl)
	}
	if tp := interp.TakeProfitLevels; len(tp) != 1 || tp[0].Price.Type != PriceFixed || tp[0].Price.Price != exp.tp || tp[0].Ratio != nil {
		t.Fatalf("take profit: %+v", tp)
	}
	if interp.ActionEvidence == nil || interp.ActionEvidence.SourceID != "body" || !strings.HasPrefix(interp.ActionEvidence.Text, "- Entry:") || !strings.Contains(interp.ActionEvidence.Text, ":ChromaCheck2:") {
		t.Fatalf("evidence must be the ticked English entry line: %+v", interp.ActionEvidence)
	}
	if interp.RequiresOriginalCard || interp.TradeReference.RootMessageID != "" {
		t.Fatalf("an OPEN is rooted by the pipeline, not a trade reference: %+v", interp.TradeReference)
	}
}

// Replays the real revision stream in order; every event's routing is fixed.
func TestJonziRevisionStreamReplay(t *testing.T) {
	opens, closes, trails, ignores, modelRoutes := 0, 0, 0, 0, 0
	for _, ev := range jonziRevisionEvents(t) {
		ev := ev
		t.Run(fmt.Sprintf("%s/rev%d", ev.MessageID, ev.Revision), func(t *testing.T) {
			msg := ev.storeMessage(t)
			exp := jonziRevisionExpectation(t, ev)
			// The model always proposes OPEN+ADD: only a model-routed message
			// may call it, and its proposal can never survive.
			e, llm := jonziInterpretEngine(t, jonziAdversarialOpen)
			interp, run, _, err := e.interpret("t", "", msg, ev.Revision > 0, true)
			if err != nil {
				t.Fatal(err)
			}
			if exp.modelRoute != (llm.calls > 0) {
				t.Fatalf("model calls=%d, want model route %v (%+v)", llm.calls, exp.modelRoute, interp)
			}
			if !exp.modelRoute && (run == nil || run.Model != "deterministic:jonzi_v1") {
				t.Fatalf("deterministic route not recorded: %+v", run)
			}
			sources := e.messageSources(msg, "jonzi_v1")
			switch exp.action {
			case ActionOpen:
				opens++
				o := jonziRevisionOpens[ev.MessageID]
				dir := DirectionShort
				if o[0] > 0 {
					dir = DirectionLong
				}
				assertJonziOpen(t, interp, msg, exp, dir)
				if skip, detail, err := ValidateActionEvidenceDetailed(interp, sources); skip != SkipNone || err != nil {
					t.Fatalf("source gate blocked deterministic OPEN: %s %s %v", skip, detail, err)
				}
				// Price-band admission is the risk layer's job; swing TPs far from
				// entry (e.g. 49224) are parsed as stated and judged later.
				if math.Abs(exp.tp-exp.entry)/exp.entry < 0.1 {
					if skip, err := ValidateInterpretation(interp, exp.entry); skip != SkipNone || err != nil {
						t.Fatalf("deterministic OPEN invalid: %s %v", skip, err)
					}
				}
			case ActionClose, ActionUpdateSL:
				if exp.action == ActionClose {
					closes++
				} else {
					trails++
				}
				assertNoJonziOpen(t, "management", interp)
				if len(interp.Flatten()) != 1 || interp.Action != exp.action || interp.Classification != ClassificationSignal {
					t.Fatalf("want %s: %+v", exp.action, interp)
				}
				if interp.Symbol != "BTCUSDT" || interp.Direction != DirectionShort || !interp.RequiresOriginalCard || interp.TradeReference.RootMessageID != msg.MessageID {
					t.Fatalf("identity/association: %+v", interp)
				}
				if interp.ActionEvidence == nil || interp.ActionEvidence.SourceID != "body" || !strings.Contains(interp.ActionEvidence.Text, ":ChromaAlert:") || strings.Contains(interp.ActionEvidence.Text, "Reason for setup") {
					t.Fatalf("evidence must be the status line: %+v", interp.ActionEvidence)
				}
				if skip, detail, err := ValidateActionEvidenceDetailed(interp, sources); skip != SkipNone || err != nil {
					t.Fatalf("source gate blocked %s: %s %s %v", exp.action, skip, detail, err)
				}
				if exp.action == ActionClose && interp.CloseMode != CloseModeFull {
					t.Fatalf("close is not full: %+v", interp)
				}
				if exp.action == ActionUpdateSL {
					if sl := interp.StopLossLevels; len(sl) != 1 || sl[0].Price.Type != PriceFixed || sl[0].Price.Price != exp.stop {
						t.Fatalf("trailed stop: %+v", sl)
					}
				}
			default:
				ignores++
				assertNoJonziOpen(t, "ignore", interp)
				for _, ins := range interp.Flatten() {
					if ins.IsActionable() {
						t.Fatalf("produced an action: %+v", ins)
					}
				}
				if exp.modelRoute {
					modelRoutes++
					if !strings.Contains(strings.Join(interp.Flatten()[0].Warnings, ";"), "downgraded to IGNORE") {
						t.Fatalf("model OPEN downgrade not recorded: %+v", interp.Flatten()[0])
					}
				}
			}
		})
	}
	if opens != 16 || closes != 2 || trails != 2 || ignores != 12 || modelRoutes != 6 {
		t.Fatalf("routing totals drifted: open=%d close=%d trail=%d ignore=%d (model %d)", opens, closes, trails, ignores, modelRoutes)
	}
}

// Every frozen rule snapshot takes the same source-policy path for the stream:
// revision>0, embeds and commentary never yield OPEN/ADD at any version.
func TestJonziRevisionStreamAllRuleVersions(t *testing.T) {
	fallback := func() *SourceInterpretation {
		p, err := ParseInterpretation(jonziAdversarialOpen)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	for v := 1; v <= MessageRulesVersion; v++ {
		for _, ev := range jonziRevisionEvents(t) {
			msg := ev.storeMessage(t)
			exp := jonziRevisionExpectation(t, ev)
			sources := BuildSourceSegments(msg, "jonzi_v1")
			result := interpretKnownVersion(msg, sources, "jonzi_v1", v)
			if (result == nil) != exp.modelRoute {
				t.Fatalf("v%d %s rev%d: model route mismatch %+v", v, ev.MessageID, ev.Revision, result)
			}
			if result == nil {
				result = fallback()
			}
			result = applySourcePolicyVersion(result, msg, sources, "jonzi_v1", v)
			result = ApplyMessageRules(result, msg, sources, MessageRules{Version: v, Profile: "jonzi_v1", ReduceRatio: 50, EntryPolicy: EntryPolicyLegacy})
			label := fmt.Sprintf("v%d %s rev%d", v, ev.MessageID, ev.Revision)
			if exp.action != ActionOpen {
				assertNoJonziOpen(t, label, result)
			}
			if exp.action == ActionIgnore {
				if result.IsActionable() {
					t.Fatalf("%s: actionable %+v", label, result)
				}
			} else if result.Action != exp.action || !result.IsActionable() {
				t.Fatalf("%s: want %s got %+v", label, exp.action, result)
			}
		}
	}
}

// A rev0 card whose entry is not yet ticked (pending limit) goes to the model;
// a model OPEN with the English values passes, one with other values does not.
// The same card with a translated-price conflict also goes to the model, and
// the existing native-card fact check keeps any OPEN from it non-actionable
// (AMBIGUOUS). Revision>0 copies never open.
func TestJonziUnTickedOrConflictingCardUsesModel(t *testing.T) {
	var base string
	for _, ev := range jonziRevisionEvents(t) {
		if ev.MessageID == "1563191327212965910" {
			base = ev.Message.Content
		}
	}
	unticked := strings.Replace(base, "- Entry: $`81974.6` :ChromaCheck2: (<t:1792764239:R>)", "- Entry: $`81974.6`", 1)
	conflict := strings.Replace(base, "- 入场：$`81974.6`", "- 入场：$`81947.6`", 1)
	if unticked == base || conflict == base {
		t.Fatal("sample changed")
	}
	if c := parseJonziCard(conflict); c.PriceConflict == "" || c.Entry.Value != 81974.6 {
		t.Fatalf("conflict not detected: %+v", c)
	}
	open := func(entry, sl float64) string {
		return fmt.Sprintf(`{"classification":"SIGNAL","action":"OPEN","symbol":"BTC","direction":"SHORT","entry_orders":[{"order_type":"LIMIT","price":{"type":"FIXED","price":%g}}],"stop_loss_levels":[{"price":{"type":"FIXED","price":%g}}],"take_profit_levels":[{"price":{"type":"FIXED","price":81001}}],"action_evidence":{"source_id":"body","text":"- Entry: $`+"`81974.6`"+`"}}`, entry, sl)
	}
	for _, tc := range []struct {
		name, content, response string
		revision                int
		wantOpen                bool
	}{
		{"unticked/english", unticked, open(81974.6, 82144.5), 0, true},
		{"unticked/wrong-entry", unticked, open(81947.6, 82144.5), 0, false},
		{"unticked/wrong-stop", unticked, open(81974.6, 82244.5), 0, false},
		{"conflict/english", conflict, open(81974.6, 82144.5), 0, false},
		{"conflict/translated", conflict, open(81947.6, 82144.5), 0, false},
		{"unticked/edit", unticked, open(81974.6, 82144.5), 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Now().UTC()
			msg := &store.DiscordMessage{MessageID: "card-" + tc.name, ChannelID: jonziSampleChannel, AuthorID: "1453389441585647798", AuthorName: "JONZi", Content: tc.content, MessageTimestamp: now, Revision: tc.revision}
			if tc.revision > 0 {
				msg.EditedAt = &now
			}
			e, llm := jonziInterpretEngine(t, tc.response)
			interp, _, _, err := e.interpret("t", "", msg, tc.revision > 0, true)
			if err != nil {
				t.Fatal(err)
			}
			if tc.revision == 0 && llm.calls != 1 {
				t.Fatalf("rev0 non-deterministic card must reach the model, calls=%d", llm.calls)
			}
			if tc.revision > 0 && llm.calls != 0 {
				t.Fatalf("edit reached the model")
			}
			if got := interp.Action == ActionOpen && interp.IsActionable(); got != tc.wantOpen {
				t.Fatalf("open=%v want %v: %+v", got, tc.wantOpen, interp)
			}
			if !tc.wantOpen && strings.HasPrefix(tc.name, "unticked") {
				assertNoJonziOpen(t, tc.name, interp)
			}
		})
	}
}

// Deterministic OPEN requires every fact; a missing or already-triggered field
// sends the card to the model instead of guessing.
func TestJonziDeterministicOpenRequiresCompleteCard(t *testing.T) {
	var base string
	for _, ev := range jonziRevisionEvents(t) {
		if ev.MessageID == "1563212570750619700" && ev.Revision == 0 {
			base = ev.Message.Content
		}
	}
	msg := func(content string) (*store.DiscordMessage, []SourceSegment) {
		m := &store.DiscordMessage{MessageID: "x", Content: content, MessageTimestamp: time.Now()}
		return m, BuildSourceSegments(m, "jonzi_v1")
	}
	m, s := msg(base)
	if got := interpretJonziSource(m, s); got == nil || got.Action != ActionOpen {
		t.Fatalf("base card: %+v", got)
	}
	for name, content := range map[string]string{
		"no stop":         strings.Replace(base, "- Stop/loss: $`81667.2`\n", "", 1),
		"no tp":           strings.Replace(base, "- TP: $`80894.7` :ChromaHourglass:\n", "", 1),
		"stop wrong side": strings.Replace(base, "- Stop/loss: $`81667.2`", "- Stop/loss: $`81000`", 1),
		"tp wrong side":   strings.Replace(base, "- TP: $`80894.7` :ChromaHourglass:", "- TP: $`82000` :ChromaHourglass:", 1),
		"tp ticked":       strings.Replace(base, "- TP: $`80894.7` :ChromaHourglass:", "- TP: $`80894.7` :ChromaCheck2:", 1),
	} {
		m, s := msg(content)
		if got := interpretJonziSource(m, s); got != nil {
			t.Fatalf("%s: incomplete card handled deterministically: %+v", name, got)
		}
	}
	// An already-triggered TP also blocks a model OPEN.
	m, s = msg(strings.Replace(base, "- TP: $`80894.7` :ChromaHourglass:", "- TP: $`80894.7` :ChromaCheck2:", 1))
	ai, _ := ParseInterpretation(jonziAdversarialOpen)
	assertNoJonziOpen(t, "tp ticked", ApplyMessageRules(ai, m, s, MessageRules{Version: MessageRulesVersion, Profile: "jonzi_v1", ReduceRatio: 50, EntryPolicy: EntryPolicyLegacy}))
}

// End to end through HandleMessage: the rev0 card opens without the model at
// the card's levels; its stop/loss edit closes exactly that trade.
func TestJonziRevisionOpenThenCloseEndToEnd(t *testing.T) {
	var events []jonziRevisionEvent
	for _, ev := range jonziRevisionEvents(t) {
		if ev.MessageID == "1563227154177597630" {
			events = append(events, ev)
		}
	}
	if len(events) != 2 {
		t.Fatalf("events %d", len(events))
	}
	_, venue, st := managedExecutor(t)
	venue.market = 81487.4
	e := liveTestEngine(t, venue, st, "jonzi_v1")
	e.cfg.RiskAmountUSD = 20
	llm := &jonziCountingLLM{response: jonziAdversarialOpen}
	e.llm = llm

	// Shift the recorded times to now so the signal TTL admits the replay.
	now := time.Now().UTC()
	open := events[0].storeMessage(t)
	open.ChannelID, open.MessageTimestamp, open.ReceivedAt = "chan-1", now, now
	if err := e.HandleMessage(open, false); err != nil {
		t.Fatal(err)
	}
	ctxs, err := st.CopyTrade().GetActiveContexts(e.traderID)
	if err != nil || len(ctxs) != 1 {
		sig, _ := st.CopyTrade().LatestSignal(e.traderID, open.MessageID, 0)
		t.Fatalf("open did not execute: %+v %v signal=%+v", ctxs, err, sig)
	}
	ctx := ctxs[0]
	if ctx.RootMessageID != open.MessageID || ctx.Symbol != "BTCUSDT" || ctx.Direction != string(DirectionShort) || ctx.StopLossPrice != 81650.1 {
		t.Fatalf("context: %+v", ctx)
	}
	if calls := entryCalls(venue); len(calls) == 0 || calls[0].Type != "MARKET" {
		t.Fatalf("entry: %+v", calls)
	}

	closeMsg := events[1].storeMessage(t)
	edited := now.Add(7 * time.Minute)
	closeMsg.ChannelID, closeMsg.MessageTimestamp, closeMsg.ReceivedAt, closeMsg.EditedAt = "chan-1", now, edited, &edited
	if err := e.HandleMessage(closeMsg, true); err != nil {
		t.Fatal(err)
	}
	if llm.calls != 0 {
		t.Fatalf("model consulted %d times", llm.calls)
	}
	saved, _ := st.CopyTrade().GetContext(ctx.ID)
	if saved == nil || saved.State == string(StateEntryPending) || venue.qty != 0 {
		t.Fatalf("trade not closed: state=%v qty=%g", saved, venue.qty)
	}
	active, _ := st.CopyTrade().GetActiveContexts(e.traderID)
	if len(active) != 0 {
		t.Fatalf("still active: %+v", active)
	}
}
