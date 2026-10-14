package copytrader

import (
	"testing"
)

func TestManagementV4CTAndMagic(t *testing.T) {
	for _, body := range []string{"#CT 這裏震蕩太久了，我們提前止盈出", "#CT 我们提前止盈出", "#CT 止盈出场"} {
		msg := quotedMessage(body)
		sources := BuildSourceSegments(msg, "tyler_v1")
		got := InterpretKnownSource(msg, sources, "tyler_v1")
		if got == nil || got.Action != ActionClose || got.CloseMode != CloseModeFull {
			t.Fatalf("%s: %+v", body, got)
		}
		if skip, err := ValidateActionEvidence(got, sources); skip != SkipNone || err != nil {
			t.Fatalf("%s %v", skip, err)
		}
	}
	msg := quotedMessage("#MAGIC 補倉可以撤了，TP1修改為這裡減倉後再拿")
	sources := BuildSourceSegments(msg, "tyler_v1")
	got := InterpretKnownSource(msg, sources, "tyler_v1")
	got = ApplyMessageRules(got, msg, sources, MessageRules{Version: 4, Profile: "tyler_v1", ReduceRatio: 50})
	if got == nil || got.Action != ActionReduce || got.ReduceTPLevel != 1 || !got.CancelPendingEntry || got.CloseRatio != nil || got.RequiresTPFill != 0 {
		t.Fatalf("compound: %+v", got)
	}
	if skip, err := ValidateActionEvidence(got, sources); skip != SkipNone || err != nil {
		t.Fatalf("compound evidence %s %v", skip, err)
	}
}

func TestManagementV4Boundaries(t *testing.T) {
	for _, body := range []string{"#CT 不要提前止盈出", "#CT 如果跌破支撑，我们提前止盈出", "#CT 止盈出一半，剩余继续拿", "#CT 止盈出50%", "#CT 止盈出，留30%", "#CT TP1 已到"} {
		msg := quotedMessage(body)
		got := InterpretKnownSource(msg, BuildSourceSegments(msg, "tyler_v1"), "tyler_v1")
		if got != nil && got.IsActionable() && got.Action == ActionClose {
			t.Fatalf("unsafe full close %s: %+v", body, got)
		}
	}
	for _, body := range []string{"#MAGIC TP1成交後減倉", "#MAGIC TP1后減倉"} {
		if n, _ := requiredTPFillV4(body); n != 1 {
			t.Fatalf("lost real prerequisite %s", body)
		}
	}
	if n, _ := requiredTPFillV4("#MAGIC TP1修改為這裡減倉後再拿"); n != 0 {
		t.Fatal("replacement mistaken for prerequisite")
	}
}

func TestManagementClausesKeepTheirOwnPrerequisites(t *testing.T) {
	body := "#BTC 这里减仓50%，TP1成交后止损移至保本"
	msg := quotedMessage(body)
	sources := BuildSourceSegments(msg, "tyler_v1")
	reduce := &SourceInterpretation{Action: ActionReduce, Symbol: "BTC", Classification: ClassificationSignal}
	got := ApplyMessageRules(reduce, msg, sources, MessageRules{Version: 4, Profile: "tyler_v1", ReduceRatio: 50})
	if got.RequiresTPFill != 0 || got.CloseRatio == nil || *got.CloseRatio != 50 {
		t.Fatalf("unrelated condition leaked %+v", got)
	}
	for _, text := range []string{"#CT 提前止盈出，留30%", "#CT 止盈出50%"} {
		m := quotedMessage(text)
		s := BuildSourceSegments(m, "tyler_v1")
		g := InterpretKnownSource(m, s, "tyler_v1")
		g = ApplyMessageRules(g, m, s, MessageRules{Version: 4, Profile: "tyler_v1", ReduceRatio: 50})
		want := 50.0
		if text == "#CT 提前止盈出，留30%" {
			want = 70
		}
		if g == nil || g.Classification != ClassificationSignal || g.CloseRatio == nil || *g.CloseRatio != want {
			t.Fatalf("explicit portion lost: %s %+v", text, g)
		}
	}
}

func TestCTFastPathAvoidsModelAndNetworkContext(t *testing.T) {
	_, v, st := managedExecutor(t)
	e := liveTestEngine(t, v, st, "tyler_v1")
	llm := &fixtureLLM{}
	e.llm = llm
	e.cfg.ParseImages = true
	e.cfg.SendPositionSnapshot = true
	m := quotedMessage("#CT 這裏震蕩太久了，我們提前止盈出")
	got, run, _, err := e.interpret("trace", "", m, false, true)
	if err != nil || got == nil || got.Action != ActionClose || run.Provider != "deterministic" || llm.calls != 0 {
		t.Fatalf("fast path failed %+v %v calls=%d", got, err, llm.calls)
	}
}

func TestOldSnapshotRejectsTPReplacementAndExecutedReductionCannotUpgrade(t *testing.T) {
	m := quotedMessage("#BTC 補倉可以撤了，TP1修改為這裡減倉後再拿")
	s := BuildSourceSegments(m, "tyler_v1")
	got := InterpretKnownSource(m, s, "tyler_v1")
	got = ApplyMessageRules(got, m, s, MessageRules{Version: 3, Profile: "tyler_v1", ReduceRatio: 50})
	if got.IsActionable() {
		t.Fatal("old snapshot gained new action")
	}
	x, v, st := managedExecutor(t)
	if _, err := x.ExecuteOpen("open", "s-open", splitPlan()); err != nil {
		t.Fatal(err)
	}
	e := liveTestEngine(t, v, st, "tyler_v1")
	m = quotedMessage("#BTC 止盈或者減倉")
	m.ChannelID = "chan-1"
	if err := e.HandleMessage(m, false); err != nil {
		t.Fatal(err)
	}
	before := v.qty
	m.Revision++
	m.Content = "#BTC 補倉可以撤了，TP1修改為這裡減倉後再拿"
	if err := e.HandleMessage(m, true); err != nil {
		t.Fatal(err)
	}
	if v.qty != before {
		t.Fatal("edited reduction executed twice")
	}
}

func TestManagementAIValidationKeepsExitAndNegationBoundaries(t *testing.T) {
	for _, tc := range []struct {
		body   string
		action Action
	}{
		{"#CT 我們提前止盈出", ActionReduce},
		{"#CT 提前止盈出50%", ActionClose},
		{"#CT 提前止盈出，留30%", ActionClose},
		{"#CT 不要，提前止盈出", ActionClose},
	} {
		m := quotedMessage(tc.body)
		sources := BuildSourceSegments(m, "tyler_v1")
		ins := &SourceInterpretation{Classification: ClassificationSignal, Action: tc.action, Symbol: "CT", ActionEvidence: &ActionEvidence{SourceID: sources[0].ID, Text: tc.body}}
		normalizeActionEvidence(ins, sources)
		if skip, err := ValidateActionEvidence(ins, sources); skip == SkipNone && err == nil {
			t.Fatalf("unsafe AI interpretation accepted %s", tc.body)
		}
	}
}
