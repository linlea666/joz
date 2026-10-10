package copytrader

import (
	"nofx/store"
	"strings"
	"testing"
	"time"
)

func TestUnresolvedManagementCannotChangeLivePositionOrClaimAICall(t *testing.T) {
	x, v, st := managedExecutor(t)
	_, err := x.ExecuteOpen("open", "s-open", splitPlan())
	if err != nil {
		t.Fatal(err)
	}
	e := liveTestEngine(t, v, st, "tyler_v1")
	before := v.qty
	msg := &store.DiscordMessage{MessageID: "unresolved", ChannelID: "chan-1", MessageTimestamp: time.Now(), Content: "#BTC TP1附近手動減倉上套保"}
	if err := e.HandleMessage(msg, false); err != nil {
		t.Fatal(err)
	}
	if v.qty != before {
		t.Fatal("partially executed unresolved compound instruction")
	}
	actions, err := st.CopyTrade().GetActionsForMessage(e.traderID, msg.MessageID)
	if err != nil || len(actions) != 0 {
		t.Fatal(actions, err)
	}
	signals, err := st.CopyTrade().GetRecentSignals(e.traderID, time.Time{}, time.Time{}, 10)
	if err != nil || len(signals) != 1 || signals[0].SkipReason != string(SkipAmbiguous) {
		t.Fatal(signals, err)
	}
	events, err := st.CopyTrade().GetEventsByTrace(e.traderID, signals[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	matched := false
	for _, ev := range events {
		if ev.Event == EvRuleMatched {
			matched = true
		}
		if ev.Event == EvAIRequest || ev.Event == EvAIParsed || strings.Contains(ev.Message, "LLM responded") {
			t.Fatal("rule path claimed a model call", ev)
		}
	}
	if !matched {
		t.Fatal("missing deterministic evidence")
	}
	stats, err := st.CopyTrade().RecognitionStats(e.traderID)
	if err != nil || stats.ModelCalls != 0 || stats.DeterministicRuns != 1 {
		t.Fatal(stats, err)
	}
}

func TestAuditedManagementCompleteness(t *testing.T) {
	for _, body := range []string{"#RAY TP1附近手動減倉上套保", "#W 同樣2倍拿下，可止盈或者上套繼續持有", "#BTC 减仓或者上成本继续持有"} {
		t.Run(body, func(t *testing.T) {
			v := presetInterpret(body, "tyler_v1", 50, &SourceInterpretation{Classification: ClassificationSignal, Action: ActionReduce, Symbol: "BTC"})
			if v.Classification != ClassificationAmbiguous || v.IsActionable() {
				t.Fatalf("unresolved compound management must not execute partially: %+v", v)
			}
		})
	}
	for _, body := range []string{"#BTC 减仓25%，上成本", "#BTC 止盈或者減倉"} {
		v := presetInterpret(body, "tyler_v1", 50, nil)
		if !v.IsActionable() {
			t.Fatalf("verified explicit management regressed: %s %+v", body, v)
		}
	}
	v := presetInterpret("#KAIA 2倍拿下", "tyler_v1", 50, nil)
	if v.Classification != ClassificationIgnore {
		t.Fatalf("recap changed: %+v", v)
	}
}

func TestUnresolvedManagementGuardsAIAndExcludesReferences(t *testing.T) {
	msg := &store.DiscordMessage{Content: "#RAY 手動減倉上套保", MessageID: "manage"}
	sources := BuildSourceSegments(msg, "tyler_v1")
	v := ApplyMessageRules(&SourceInterpretation{Classification: ClassificationSignal, Action: ActionReduce, Symbol: "RAY"}, msg, sources, MessageRules{Version: MessageRulesVersion, Profile: "tyler_v1", ReduceRatio: 50})
	if v.IsActionable() || v.Classification != ClassificationAmbiguous {
		t.Fatal("AI bypassed unresolved wording", v)
	}
	msg.Content = "#BTC 减仓25%"
	sources = BuildSourceSegments(msg, "tyler_v1")
	sources = append(sources, SourceSegment{ID: "old", Role: "reference", Text: "#BTC TP1附近手動減倉上套保"})
	v = InterpretKnownSource(msg, sources, "tyler_v1")
	v = ApplyMessageRules(v, msg, sources, MessageRules{Version: MessageRulesVersion, Profile: "tyler_v1", ReduceRatio: 50})
	if !v.IsActionable() || v.CloseRatio == nil || *v.CloseRatio != 25 {
		t.Fatal("old reference overrode current explicit action", v)
	}
}

func TestExpiredBacklogBeforeInterpretation(t *testing.T) {
	for _, tc := range []struct {
		name             string
		age              time.Duration
		openTTL, mgmtTTL int
		edited           bool
		wantCalls        int
	}{
		{"both_expired", 2 * time.Hour, 300, 1800, false, 0},
		{"management_still_fresh", 10 * time.Minute, 300, 1800, false, 1},
		{"disabled_open_ttl", 2 * time.Hour, 0, 1800, false, 1},
		{"disabled_management_ttl", 2 * time.Hour, 300, 0, false, 1},
		{"fresh_edit", 2 * time.Hour, 300, 1800, true, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, x, llm, msg := fixtureEngine(t, marketReferenceFixture{Name: tc.name, Content: "ordinary commentary", Response: []byte(`{"classification":"IGNORE","action":"IGNORE"}`)}, 1)
			e.cfg.OpenSignalTTLSeconds, e.cfg.MgmtSignalTTLSeconds = tc.openTTL, tc.mgmtTTL
			msg.MessageTimestamp = time.Now().Add(-tc.age)
			msg.ReceivedAt = time.Now()
			if tc.edited {
				now := time.Now()
				msg.EditedAt = &now
			}
			if err := e.HandleMessage(msg, tc.edited); err != nil {
				t.Fatal(err)
			}
			if llm.calls != tc.wantCalls || x.marketEntries != 0 || len(x.limitEntries) != 0 {
				t.Fatalf("calls=%d want=%d", llm.calls, tc.wantCalls)
			}
			sigs, err := e.st.CopyTrade().GetRecentSignals(e.traderID, time.Time{}, time.Time{}, 10)
			if err != nil || len(sigs) != 1 {
				t.Fatal(sigs, err)
			}
			if tc.wantCalls == 0 && (sigs[0].SkipReason != string(SkipExpired) || sigs[0].AIRunID != 0 || sigs[0].Status != store.SignalStatusSkipped) {
				t.Fatalf("expired audit %+v", sigs[0])
			}
		})
	}
}
