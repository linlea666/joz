package copytrader

import (
	"errors"
	"testing"
	"time"

	"nofx/discord"
	"nofx/store"
)

// Boundary refusals carry timestamps in their detail; the persisted skip
// reason must stay the bare stable code.
func TestExecutionGateSkipReasonIsStableCode(t *testing.T) {
	for _, code := range []string{discord.GateBeforeLiveBoundary, discord.GateBeforeTraderActivation} {
		t.Run(code, func(t *testing.T) {
			f := marketReferenceFixtures(t)[0]
			e, x, _, msg := fixtureEngine(t, f, 1)
			e.source = &admissionSource{err: &discord.ExecutionGateError{Code: code, Detail: "message posted 2026-01-01T00:00:00Z is before boundary 2026-01-01T01:00:00Z"}}
			msg.ReceivedAt = time.Now().UTC()
			if err := e.HandleMessage(msg, false); err != nil {
				t.Fatal(err)
			}
			if x.marketEntries != 0 || len(x.limitEntries) != 0 {
				t.Fatal("refused card opened a trade")
			}
			sig, err := e.st.CopyTrade().LatestSignal(e.traderID, msg.MessageID, msg.Revision)
			if err != nil || sig == nil {
				t.Fatal(sig, err)
			}
			if sig.Status != store.SignalStatusSkipped || sig.SkipReason != code {
				t.Fatalf("skip reason must be the bare code %s, got %+v", code, sig)
			}
		})
	}
}

func TestEntryAdmissionClassifiesGateCodes(t *testing.T) {
	st := newTestStore(t)
	cfg := DefaultCopyTradingConfig()
	rules := notificationRules()
	source := &admissionSource{}
	e := NewEngine(EngineParams{TraderID: "t", Store: st, Config: &cfg, Source: source})
	root := notificationFixture("added")
	root.ChannelID = "201"
	root.MessageTimestamp = time.Now().Add(-time.Minute)
	root.ReceivedAt = root.MessageTimestamp
	if err := st.DiscordMessage().CommitInbound(&store.DiscordInbound{EventID: "opening", Kind: "MESSAGE_CREATE", ChannelID: root.ChannelID, MessageID: root.MessageID, ReceivedAt: root.ReceivedAt}, root, []store.DiscordDelivery{{TraderID: "t", RulesJSON: rules.Snapshot(), ExecutionKey: "frozen"}}); err != nil {
		t.Fatal(err)
	}
	if err := st.CopyTrade().CreateSignal(&store.CopyTradeSignal{ID: "open", TraderID: "t", MessageID: root.MessageID, ChannelID: root.ChannelID, RulesSnapshotJSON: rules.Snapshot(), ExecutionVersion: 1}); err != nil {
		t.Fatal(err)
	}
	row := &store.CopyTradeOrder{SignalID: "open", Symbol: "BTCUSDT", Direction: "SHORT"}
	for _, tc := range []struct {
		err  error
		want error
	}{
		{&discord.ExecutionGateError{Code: discord.GateSourceNotReady}, errEntryDeferred},
		{&discord.ExecutionGateError{Code: discord.GateSourceGapOrPermission}, errEntryDeferred},
		{&discord.ExecutionGateError{Code: discord.GateBeforeLiveBoundary, Detail: "t1 before t2"}, errEntryRevoked},
		{&discord.ExecutionGateError{Code: discord.GateBeforeTraderActivation, Detail: "t1 before t2"}, errEntryRevoked},
		{&discord.ExecutionGateError{Code: discord.GateConfigRevoked}, errEntryRevoked},
		{errors.New(discord.GateBeforeExecutionBoundary), errEntryRevoked},
	} {
		source.err = tc.err
		if err := e.admitEntrySubmit(row); !errors.Is(err, tc.want) {
			t.Fatalf("%v: got %v want %v", tc.err, err, tc.want)
		}
	}
}
