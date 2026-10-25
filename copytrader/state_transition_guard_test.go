package copytrader

import "testing"

// persistContext / markContext audit the lifecycle table: a transition
// outside it still persists (refusing could freeze a live trade) but leaves an
// error event for review.
func TestMarkContextAuditsIllegalTransition(t *testing.T) {
	st := newTestStore(t)
	events := NewEventLogger(st, "trader-1", "chan-1")
	x := NewExecutor("trader-1", &mockExchange{}, st, events)

	ctx := newTestContext(t, st, StateCancelled)
	if err := x.markContext(ctx, StateOpen, map[string]interface{}{"last_action": "LATE"}); err != nil {
		t.Fatalf("audited transition must still persist: %v", err)
	}
	stored, err := st.CopyTrade().GetContext(ctx.ID)
	if err != nil || stored.State != string(StateOpen) || stored.LastAction != "LATE" {
		t.Fatalf("audited update did not reach the store: %+v %v", stored, err)
	}
	evs, err := st.CopyTrade().GetEventsByTrace("trader-1", "reconcile-"+ctx.ID)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, ev := range evs {
		found = found || (ev.Level == "error" && ev.Event == EvExecutionError)
	}
	if !found {
		t.Fatalf("no error event for the illegal transition: %+v", evs)
	}

	// Same-state rewrites and table transitions leave no audit event.
	ctx2 := newTestContext(t, st, StateCancelled)
	if err := x.markContext(ctx2, StateCancelled, nil); err != nil {
		t.Fatalf("same-state rewrite refused: %v", err)
	}
	if err := x.markContext(ctx2, StateExpired, nil); err != nil {
		t.Fatalf("CANCELLED -> EXPIRED (timed-out entry relabel) refused: %v", err)
	}
	if evs, _ = st.CopyTrade().GetEventsByTrace("trader-1", "reconcile-"+ctx2.ID); len(evs) != 0 {
		t.Fatalf("legal transitions must not be audited: %+v", evs)
	}
}

// CLOSED -> OPEN is audited, except the explicit spurious-close recovery.
func TestClosedReopenAuditedExceptSpuriousCloseRecovery(t *testing.T) {
	st := newTestStore(t)
	x := NewExecutor("trader-1", &mockExchange{}, st, NewEventLogger(st, "trader-1", "chan-1"))

	ctx := newTestContext(t, st, StateClosed)
	if err := x.persistContext(ctx, map[string]interface{}{"state": string(StateOpen), "last_action": lastActionSpuriousCloseRecovered}); err != nil {
		t.Fatalf("spurious-close recovery refused: %v", err)
	}
	stored, err := st.CopyTrade().GetContext(ctx.ID)
	if err != nil || stored.State != string(StateOpen) {
		t.Fatalf("recovery not persisted: %+v %v", stored, err)
	}
	if evs, _ := st.CopyTrade().GetEventsByTrace("trader-1", "reconcile-"+ctx.ID); len(evs) != 0 {
		t.Fatalf("sanctioned recovery must not be audited: %+v", evs)
	}

	stale := newTestContext(t, st, StateClosed)
	if err := x.persistContext(stale, map[string]interface{}{"state": string(StateOpen)}); err != nil {
		t.Fatalf("audited transition must still persist: %v", err)
	}
	if evs, _ := st.CopyTrade().GetEventsByTrace("trader-1", "reconcile-"+stale.ID); len(evs) == 0 {
		t.Fatal("stale CLOSED -> OPEN must leave an audit event")
	}
}
