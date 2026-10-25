package copytrader

import (
	"encoding/json"
	"testing"
	"time"
)

// A message rejected by the author filter is consumed (nil error, no signal,
// no model call) and leaves an observable info event with the author identity.
func TestAuthorFilteredMessageLogsEventWithoutSignal(t *testing.T) {
	f := marketReferenceFixtures(t)[0]
	e, x, llm, msg := fixtureEngine(t, f, 1)
	e.cfg.SourceAuthorIDs = []string{"allowed-author"}
	msg.AuthorID, msg.AuthorName = "other-author", "Someone Else"
	msg.DeliveryID = 7 // delivered messages must still count as consumed
	if err := e.HandleMessage(msg, false); err != nil {
		t.Fatalf("filtered message must be consumed: %v", err)
	}
	if llm.calls != 0 || x.marketEntries != 0 || len(x.limitEntries) != 0 {
		t.Fatalf("filtered message reached interpretation/execution: calls=%d market=%d", llm.calls, x.marketEntries)
	}
	sigs, err := e.st.CopyTrade().GetRecentSignals(e.traderID, time.Time{}, time.Time{}, 10)
	if err != nil || len(sigs) != 0 {
		t.Fatalf("filtered message created signals: %v %v", sigs, err)
	}
	events, err := e.st.CopyTrade().GetEventsByTrace(e.traderID, msg.MessageID)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, ev := range events {
		if ev.Event != EvSourceAuthorFiltered {
			continue
		}
		found = true
		if ev.Level != "info" || ev.MessageID != msg.MessageID || ev.SignalID != "" {
			t.Fatalf("unexpected event: %+v", ev)
		}
		var ctx map[string]interface{}
		if err := json.Unmarshal([]byte(ev.ContextJSON), &ctx); err != nil {
			t.Fatal(err)
		}
		if ctx["author_id"] != "other-author" || ctx["author_name"] != "Someone Else" || ctx["message_id"] != msg.MessageID {
			t.Fatalf("event context missing author identity: %v", ctx)
		}
	}
	if !found {
		t.Fatalf("no %s event: %+v", EvSourceAuthorFiltered, events)
	}

	// Allowed author is unaffected and emits no filter event.
	msg2 := *msg
	msg2.MessageID, msg2.AuthorID, msg2.DeliveryID = msg.MessageID+"-allowed", "allowed-author", 0
	if err := e.HandleMessage(&msg2, false); err != nil {
		t.Fatal(err)
	}
	if llm.calls != 1 {
		t.Fatalf("allowed author not interpreted, calls=%d", llm.calls)
	}
	events, _ = e.st.CopyTrade().GetEventsByTrace(e.traderID, msg2.MessageID)
	for _, ev := range events {
		if ev.Event == EvSourceAuthorFiltered {
			t.Fatal("allowed author logged as filtered")
		}
	}
}

// A delivery whose frozen rule snapshot cannot be decoded is rejected: it must
// not run with empty rules (which would bypass the author filter), it creates
// no signal, and it leaves an error event naming the reason.
func TestCorruptRulesSnapshotRejectsMessage(t *testing.T) {
	f := marketReferenceFixtures(t)[0]
	e, x, llm, msg := fixtureEngine(t, f, 1)
	e.cfg.SourceAuthorIDs = []string{"allowed-author"}
	msg.AuthorID, msg.AuthorName = "other-author", "Someone Else"
	msg.DeliveryID = 9
	msg.RulesSnapshot = `{"source_author_ids":`
	if _, err := e.messageRules(msg); err == nil {
		t.Fatal("corrupt snapshot decoded without error")
	}
	if err := e.HandleMessage(msg, false); err != nil {
		t.Fatalf("rejected message must be consumed, not retried: %v", err)
	}
	if llm.calls != 0 || x.marketEntries != 0 || len(x.limitEntries) != 0 {
		t.Fatalf("corrupt snapshot reached interpretation/execution: calls=%d market=%d", llm.calls, x.marketEntries)
	}
	sigs, err := e.st.CopyTrade().GetRecentSignals(e.traderID, time.Time{}, time.Time{}, 10)
	if err != nil || len(sigs) != 0 {
		t.Fatalf("corrupt snapshot created signals: %v %v", sigs, err)
	}
	events, err := e.st.CopyTrade().GetEventsByTrace(e.traderID, msg.MessageID)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, ev := range events {
		if ev.Event == EvSourceAuthorFiltered {
			t.Fatal("corrupt snapshot must be rejected before the author filter")
		}
		if ev.Event == EvSourceRulesInvalid {
			found = ev.Level == "error" && ev.SignalID == ""
		}
	}
	if !found {
		t.Fatalf("no %s error event: %+v", EvSourceRulesInvalid, events)
	}

	// A valid snapshot is still honored (author filter frozen on the delivery).
	msg2 := *msg
	msg2.MessageID = msg.MessageID + "-valid"
	msg2.RulesSnapshot = e.cfg.MessageRules().Snapshot()
	if err := e.HandleMessage(&msg2, false); err != nil {
		t.Fatal(err)
	}
	if llm.calls != 0 {
		t.Fatalf("frozen author filter bypassed, calls=%d", llm.calls)
	}
}
