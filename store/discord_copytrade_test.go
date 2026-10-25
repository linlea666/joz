package store

// Regression tests for the audit fixes:
//   - edits bump the message revision and reset it to pending
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

func TestUpsertEditBumpsRevisionAndResetsPending(t *testing.T) {
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

	// The author edits the message: revision 1, pending again.
	edit := &DiscordMessage{
		ChannelID: "c1", MessageID: "m1", Content: "SL moved to entry",
		MessageTimestamp: msg.MessageTimestamp,
	}
	if res, err := s.Upsert(edit); err != nil || res != DiscordMsgEdited {
		t.Fatalf("upsert edit: res=%v err=%v", res, err)
	}
	stored, _ := s.GetByMessageID("c1", "m1")
	if stored.ProcessingStatus != DiscordMsgPending || stored.Revision != 1 {
		t.Fatalf("edit lost: status=%s revision=%d, want pending/1", stored.ProcessingStatus, stored.Revision)
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

func TestDiscordMetadataRefreshNeverReplaysHistory(t *testing.T) {
	db := newDiscordTestDB(t)
	s := NewDiscordMessageStore(db)
	if err := s.initTables(); err != nil {
		t.Fatal(err)
	}
	old := &DiscordMessage{ChannelID: "chan", MessageID: "old", Content: "#FLOCK 起飛", EmbedsJSON: `[{"description":"#FLOCK entry market","timestamp":"2026-09-24T06:59:00Z"}]`, AttachmentsJSON: `[{"id":"image","url":"https://cdn.discordapp.com/image.png?ex=old"}]`, Revision: 4, MessageTimestamp: time.Now().Add(-24 * time.Hour)}
	if err := s.MarkBaseline(old); err != nil {
		t.Fatal(err)
	}
	fresh := *old
	fresh.ContentHash = "new-hash-version"
	fresh.ReplyToMessageID = "source"
	fresh.EmbedsJSON = `[{"description":"#FLOCK entry market","timestamp":"2026-09-24T06:59:00Z","author":{"name":"GKS"}}]`
	fresh.AttachmentsJSON = `[{"id":"image","url":"https://cdn.discordapp.com/image.png?ex=new"}]`
	result, err := s.Upsert(&fresh)
	if err != nil || result != DiscordMsgUnchanged {
		t.Fatalf("metadata queued old trade: %v %v", result, err)
	}
	got, _ := s.GetByMessageID("chan", "old")
	if got.Revision != 4 || got.ProcessingStatus != DiscordMsgSkipped || got.ReplyToMessageID != "source" {
		t.Fatalf("baseline corrupted: %+v", got)
	}
	fresh.EmbedsJSON = `[{"description":"#FLOCK SL to entry","author":{"name":"GKS"}}]`
	result, err = s.Upsert(&fresh)
	if err != nil || result != DiscordMsgEdited {
		t.Fatal("semantic lifecycle edit was lost")
	}
}
