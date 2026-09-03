package store

// Regression tests for the audit fixes:
//   - channel baseline is tracked by an explicit flag, not by row presence
//   - message status updates are revision-conditional (edits survive)
//   - signal dedup only counts terminal outcomes

import (
	"testing"
	"time"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func newDiscordTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	return db
}

func TestChannelBaselineFlagNotFooledBySingleRows(t *testing.T) {
	db := newDiscordTestDB(t)
	s := NewDiscordMessageStore(db)
	if err := s.initTables(); err != nil {
		t.Fatalf("init: %v", err)
	}

	// A cross-channel message lookup persists one row into a channel that was
	// never baselined (the exact scenario behind the baseline-pollution bug).
	if err := s.MarkBaseline(&DiscordMessage{
		ChannelID: "chan-x", MessageID: "m1", MessageTimestamp: time.Now(),
	}); err != nil {
		t.Fatalf("mark baseline: %v", err)
	}

	done, err := s.IsBaselineDone("chan-x")
	if err != nil {
		t.Fatalf("baseline check: %v", err)
	}
	if done {
		t.Fatal("row presence must NOT imply a completed baseline")
	}

	if err := s.MarkBaselineDone("chan-x", 50); err != nil {
		t.Fatalf("mark done: %v", err)
	}
	// Idempotent.
	if err := s.MarkBaselineDone("chan-x", 50); err != nil {
		t.Fatalf("mark done twice: %v", err)
	}
	done, _ = s.IsBaselineDone("chan-x")
	if !done {
		t.Fatal("baseline flag must be set after MarkBaselineDone")
	}
}

func TestSetStatusIfRevisionProtectsEdits(t *testing.T) {
	db := newDiscordTestDB(t)
	s := NewDiscordMessageStore(db)
	if err := s.initTables(); err != nil {
		t.Fatalf("init: %v", err)
	}

	msg := &DiscordMessage{
		ChannelID: "c1", MessageID: "m1", Content: "open long BTC",
		MessageTimestamp: time.Now().Add(-time.Minute),
	}
	if res, err := s.Upsert(msg); err != nil || res != DiscordMsgNew {
		t.Fatalf("upsert new: res=%v err=%v", res, err)
	}

	// Dispatcher claims revision 0.
	claimed, err := s.NextPending("c1")
	if err != nil || claimed == nil {
		t.Fatalf("claim: %v", err)
	}

	// The author edits the message mid-processing: revision 1, pending again.
	edit := &DiscordMessage{
		ChannelID: "c1", MessageID: "m1", Content: "SL moved to entry",
		MessageTimestamp: msg.MessageTimestamp,
	}
	if res, err := s.Upsert(edit); err != nil || res != DiscordMsgEdited {
		t.Fatalf("upsert edit: res=%v err=%v", res, err)
	}

	// Old revision's completion must NOT clobber the fresh pending row.
	applied, err := s.SetStatusIfRevision(claimed.ID, claimed.Revision, DiscordMsgDone, "")
	if err != nil {
		t.Fatalf("conditional status: %v", err)
	}
	if applied {
		t.Fatal("stale-revision status update must be rejected")
	}
	stored, _ := s.GetByMessageID("c1", "m1")
	if stored.ProcessingStatus != DiscordMsgPending || stored.Revision != 1 {
		t.Fatalf("edit lost: status=%s revision=%d, want pending/1", stored.ProcessingStatus, stored.Revision)
	}

	// The new revision processes normally.
	claimed2, err := s.NextPending("c1")
	if err != nil || claimed2 == nil {
		t.Fatalf("claim rev1: %v", err)
	}
	applied, err = s.SetStatusIfRevision(claimed2.ID, claimed2.Revision, DiscordMsgDone, "")
	if err != nil || !applied {
		t.Fatalf("rev1 completion: applied=%v err=%v", applied, err)
	}
	stored, _ = s.GetByMessageID("c1", "m1")
	if stored.ProcessingStatus != DiscordMsgDone {
		t.Fatalf("status=%s, want done", stored.ProcessingStatus)
	}
}

func TestSignalProcessedCountsOnlyTerminalStatuses(t *testing.T) {
	db := newDiscordTestDB(t)
	s := NewCopyTradeStore(db)
	if err := s.initTables(); err != nil {
		t.Fatalf("init: %v", err)
	}

	sig := &CopyTradeSignal{
		ID: "sig-1", TraderID: "tr-1", ChannelID: "c1", MessageID: "m1",
		MessageRevision: 0, Status: SignalStatusExecuting,
		MessageTimestamp: time.Now(), ReceivedAt: time.Now(),
	}
	if err := s.CreateSignal(sig); err != nil {
		t.Fatalf("create signal: %v", err)
	}

	// Half-finished pipeline (crash before terminal status) must be retryable.
	done, err := s.SignalProcessed("tr-1", "m1", 0)
	if err != nil {
		t.Fatalf("processed check: %v", err)
	}
	if done {
		t.Fatal("non-terminal signal must not block reprocessing")
	}

	if err := s.UpdateSignal("sig-1", map[string]interface{}{"status": SignalStatusExecuted}); err != nil {
		t.Fatalf("update: %v", err)
	}
	done, _ = s.SignalProcessed("tr-1", "m1", 0)
	if !done {
		t.Fatal("terminal signal must deduplicate")
	}
}
