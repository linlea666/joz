package store

import (
	"testing"
	"time"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestCopyTradeReplayPersistsReportAndItems(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	s := NewCopyTradeStore(db)
	if err := s.initTables(); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	started := time.Now().UTC().Add(-time.Minute)
	replay := &CopyTradeReplay{
		ID: "replay-1", TraderID: "trader-1", ChannelID: "channel-1",
		Status: "running", Total: 2, Done: 1, StartedAt: started,
	}
	if err := s.CreateReplay(replay); err != nil {
		t.Fatalf("create replay: %v", err)
	}
	if err := s.CreateReplayItem(&CopyTradeReplayItem{
		ReplayID: replay.ID, Sequence: 0, MessageID: "message-1", Verdict: "SKIP",
		WarningsJSON: `["action evidence normalized to current source segment"]`,
		RawResponse:  `{"classification":"SIGNAL"}`,
	}); err != nil {
		t.Fatalf("create replay item: %v", err)
	}
	finished := time.Now().UTC()
	if err := s.UpdateReplay(replay.ID, map[string]interface{}{
		"status": "aborted", "done": 1, "finished_at": &finished,
	}); err != nil {
		t.Fatalf("update replay: %v", err)
	}

	got, items, err := s.GetLatestReplay("trader-1")
	if err != nil {
		t.Fatalf("get latest replay: %v", err)
	}
	if got == nil || got.ID != replay.ID || got.Status != "aborted" || got.Done != 1 {
		t.Fatalf("unexpected replay: %+v", got)
	}
	if len(items) != 1 || items[0].Sequence != 0 || items[0].RawResponse == "" {
		t.Fatalf("unexpected replay items: %+v", items)
	}
}
