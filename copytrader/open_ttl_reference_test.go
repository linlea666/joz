package copytrader

import (
	"encoding/json"
	"testing"
	"time"

	"nofx/store"
)

// An old opening card edited long after it was posted must not open a trade:
// the opening TTL is measured from the original post, the same reference the
// source execution boundary uses for new risk.
func TestEditedStaleOpeningCardExpiresByOriginalPostTime(t *testing.T) {
	f := marketReferenceFixtures(t)[0] // LINK OPEN
	e, x, llm, msg := fixtureEngine(t, f, 1)
	e.cfg.OpenSignalTTLSeconds, e.cfg.MgmtSignalTTLSeconds = 300, 1800
	now := time.Now().UTC()
	msg.MessageTimestamp = now.Add(-20 * time.Minute) // beyond opening TTL, within management TTL
	msg.ReceivedAt = now
	msg.EditedAt = &now
	msg.Revision = 1
	if err := e.HandleMessage(msg, true); err != nil {
		t.Fatal(err)
	}
	if llm.calls != 1 {
		t.Fatalf("fresh edit must still be interpreted (management may apply), calls=%d", llm.calls)
	}
	if x.marketEntries != 0 || len(x.limitEntries) != 0 {
		t.Fatalf("stale card opened a trade: market=%d limit=%d", x.marketEntries, len(x.limitEntries))
	}
	sig, err := e.st.CopyTrade().LatestSignal(e.traderID, msg.MessageID, msg.Revision)
	if err != nil || sig == nil {
		t.Fatal(sig, err)
	}
	if sig.Status != store.SignalStatusSkipped || sig.SkipReason != string(SkipExpired) || sig.TradeContextID != "" {
		t.Fatalf("want EXPIRED skip without context, got %+v", sig)
	}
	if ctx, _ := e.st.CopyTrade().GetActiveContextBySymbol(e.traderID, "LINKUSDT", "LONG"); ctx != nil {
		t.Fatalf("context created for stale card: %+v", ctx)
	}
}

// Control: the same card edited within its opening TTL still opens.
func TestEditedFreshOpeningCardStillOpens(t *testing.T) {
	f := marketReferenceFixtures(t)[0]
	e, x, _, msg := fixtureEngine(t, f, 1)
	e.cfg.OpenSignalTTLSeconds = 300
	now := time.Now().UTC()
	msg.MessageTimestamp = now.Add(-time.Minute)
	msg.ReceivedAt = now
	msg.EditedAt = &now
	msg.Revision = 1
	if err := e.HandleMessage(msg, true); err != nil {
		t.Fatal(err)
	}
	if x.marketEntries != 1 {
		t.Fatalf("fresh opening card did not open, market=%d", x.marketEntries)
	}
}

// A resting managed entry's expiry is anchored to the original post, not to a
// later edit of the card.
func TestManagedEntryExpiryAnchoredToOriginalPost(t *testing.T) {
	_, venue, st := managedExecutor(t)
	venue.market = 100
	e := liveTestEngine(t, venue, st, "default")
	e.cfg.OpenSignalTTLSeconds, e.cfg.RiskAmountUSD = 300, 20
	posted := time.Now().UTC().Add(-2 * time.Minute)
	edited := time.Now().UTC()
	msg := &store.DiscordMessage{MessageID: "expiry", ChannelID: "chan-1", MessageTimestamp: posted, EditedAt: &edited, Revision: 1}
	if _, err := e.routeOpen("expiry", "signal", msg, participationInstruction(), "BTCUSDT", 100); err != nil {
		t.Fatal(err)
	}
	ctx, err := st.CopyTrade().GetActiveContextBySymbol(e.traderID, "BTCUSDT", "LONG")
	if err != nil || ctx == nil {
		t.Fatal(ctx, err)
	}
	var plan managedEntryPlan
	if err := json.Unmarshal([]byte(ctx.EntryPlanJSON), &plan); err != nil {
		t.Fatalf("entry plan: %v (%q)", err, ctx.EntryPlanJSON)
	}
	if want := posted.Add(300 * time.Second); !plan.SignalExpiresAt.Equal(want) {
		t.Fatalf("entry expiry must follow the original post: got %v want %v", plan.SignalExpiresAt, want)
	}
}

// Management edits of an old card stay governed by the edit time.
func TestEditedStaleCardManagementNotExpired(t *testing.T) {
	for _, tc := range []struct {
		name, content, response string
	}{
		{"close", "#LINK 全平", `{"classification":"SIGNAL","action":"CLOSE","symbol":"LINK","direction":"LONG"}`},
		{"update_sl", "#LINK 止損移到 10.8", `{"classification":"SIGNAL","action":"UPDATE_SL","symbol":"LINK","direction":"LONG","stop_loss_levels":[{"price":{"type":"FIXED","price":10.8}}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := marketReferenceFixtures(t)[0]
			e, x, llm, open := fixtureEngine(t, f, 1)
			e.cfg.OpenSignalTTLSeconds, e.cfg.MgmtSignalTTLSeconds = 300, 1800
			if err := e.HandleMessage(open, false); err != nil {
				t.Fatal(err)
			}
			if x.marketEntries != 1 {
				t.Fatalf("setup open failed, market=%d", x.marketEntries)
			}
			ctx, err := e.st.CopyTrade().GetActiveContextBySymbol(e.traderID, "LINKUSDT", "LONG")
			if err != nil || ctx == nil {
				t.Fatal(ctx, err)
			}
			x.positions = []map[string]interface{}{{"symbol": "LINKUSDT", "side": "long", "positionAmt": ctx.Quantity, "entryPrice": ctx.AvgFillPrice}}
			now := time.Now().UTC()
			mgmt := &store.DiscordMessage{MessageID: "old-card-" + tc.name, ChannelID: "123", Content: tc.content,
				MessageTimestamp: now.Add(-20 * time.Minute), ReceivedAt: now, EditedAt: &now, Revision: 1}
			llm.response = tc.response
			stops := len(x.setStopLossLog)
			if err := e.HandleMessage(mgmt, true); err != nil {
				t.Fatal(err)
			}
			sig, err := e.st.CopyTrade().LatestSignal(e.traderID, mgmt.MessageID, mgmt.Revision)
			if err != nil || sig == nil {
				t.Fatal(sig, err)
			}
			if sig.SkipReason == string(SkipExpired) || sig.Status != store.SignalStatusExecuted {
				t.Fatalf("management edit must execute (edit-time TTL): %+v", sig)
			}
			switch tc.name {
			case "close":
				if x.closeCalled != 1 {
					t.Fatalf("close not submitted, calls=%d", x.closeCalled)
				}
			case "update_sl":
				if got := x.setStopLossLog[stops:]; len(got) == 0 || got[len(got)-1] != 10.8 {
					t.Fatalf("stop not moved: %v", got)
				}
			}
		})
	}
}

// A deferred interpretation of an entry-looking edit is measured from the
// original post; management-looking or ambiguous edits keep the edit time.
func TestInterpretationRetryWindowReference(t *testing.T) {
	f := marketReferenceFixtures(t)[0]
	e, _, _, _ := fixtureEngine(t, f, 1)
	e.cfg.OpenSignalTTLSeconds, e.cfg.MgmtSignalTTLSeconds = 300, 1800
	posted := time.Now().UTC().Add(-20 * time.Minute)
	edited := time.Now().UTC()
	for _, tc := range []struct {
		name, content string
		want          time.Time
	}{
		{"entry", "#LINK 做多 進場 11.2", posted},
		{"management", "#LINK 止損移到 10.8", edited},
		{"ambiguous", "#LINK", edited},
	} {
		msg := &store.DiscordMessage{MessageID: "retry-" + tc.name, ChannelID: "123", Content: tc.content, MessageTimestamp: posted, EditedAt: &edited, Revision: 1}
		ref, _ := e.interpretationRetryWindow(msg)
		if !ref.Equal(tc.want) {
			t.Fatalf("%s: ref %v want %v", tc.name, ref, tc.want)
		}
	}
}
