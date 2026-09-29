package copytrader

import (
	"testing"
	"time"

	"nofx/store"
)

func TestReplayStatusRecoversInterruptedRun(t *testing.T) {
	st := newTestStore(t)
	started := time.Now().UTC().Add(-time.Minute)
	if err := st.CopyTrade().CreateReplay(&store.CopyTradeReplay{
		ID: "replay-recover", TraderID: "trader-1", ChannelID: "channel-1",
		Status: ReplayRunning, Total: 3, Done: 2, StartedAt: started,
	}); err != nil {
		t.Fatalf("create replay: %v", err)
	}
	if err := st.CopyTrade().CreateReplayItem(&store.CopyTradeReplayItem{
		ReplayID: "replay-recover", Sequence: 0, MessageID: "message-1", Verdict: VerdictSkip,
	}); err != nil {
		t.Fatalf("create replay item: %v", err)
	}

	e := &Engine{traderID: "trader-1", st: st}
	report := e.ReplayStatus()
	if report == nil || report.Status != ReplayAborted || report.Done != 2 || len(report.Items) != 1 {
		t.Fatalf("recovered report = %+v", report)
	}
	row, items, err := st.CopyTrade().GetLatestReplay("trader-1")
	if err != nil {
		t.Fatalf("reload replay: %v", err)
	}
	if row == nil || row.Status != ReplayAborted || row.Done != 2 || len(items) != 1 {
		t.Fatalf("persisted recovery = row=%+v items=%d", row, len(items))
	}
}

func TestAbortedReplayPersistsCompletedCount(t *testing.T) {
	st := newTestStore(t)
	if err := st.CopyTrade().CreateReplay(&store.CopyTradeReplay{
		ID: "replay-abort", TraderID: "trader-1", ChannelID: "channel-1",
		Status: ReplayRunning, Total: 1, StartedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("create replay: %v", err)
	}
	stopCh := make(chan struct{})
	close(stopCh)
	e := &Engine{
		traderID:   "trader-1",
		traderName: "test",
		st:         st,
		stopCh:     stopCh,
		replay:     &ReplayReport{ID: "replay-abort", Status: ReplayRunning, Total: 1, Items: []ReplayItem{}},
	}
	e.runReplay([]*store.DiscordMessage{{MessageID: "message-1"}})
	if e.replay.Status != ReplayAborted || e.replay.Done != 0 {
		t.Fatalf("in-memory aborted report = %+v", e.replay)
	}
	row, _, err := st.CopyTrade().GetLatestReplay("trader-1")
	if err != nil {
		t.Fatalf("reload replay: %v", err)
	}
	if row == nil || row.Status != ReplayAborted || row.Done != 0 {
		t.Fatalf("persisted aborted report = %+v", row)
	}
}
