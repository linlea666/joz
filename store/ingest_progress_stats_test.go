package store

import (
	"github.com/stretchr/testify/require"
	"testing"
	"time"
)

func TestReceiptProgressMonotonicAndTransactional(t *testing.T) {
	s := logTestStore(t)
	d := s.DiscordMessage()
	now := time.Now().UTC().Truncate(time.Millisecond)
	commit := func(id, kind, message string, at time.Time, baseline bool) {
		require.NoError(t, d.CommitInbound(&DiscordInbound{EventID: id, Kind: kind, ChannelID: "channel", MessageID: message, ReceivedAt: at, Baseline: baseline}, nil, nil))
	}
	commit("baseline", "MESSAGE_CREATE", "999", now.Add(-time.Hour), true)
	rows, err := d.SourceStates()
	require.NoError(t, err)
	require.Equal(t, "unknown", rows[0].State)
	require.True(t, rows[0].LastEventAt.IsZero())
	commit("new", "MESSAGE_CREATE", "1000", now, false)
	commit("new", "MESSAGE_CREATE", "2000", now.Add(time.Hour), false) // ACK lost
	commit("edit", "MESSAGE_UPDATE", "998", now.Add(time.Minute), false)
	commit("late", "MESSAGE_CREATE", "997", now.Add(-time.Minute), false)
	require.NoError(t, d.SaveSourceState(&DiscordSourceState{ChannelID: "channel", State: "ready", LastMessageID: "999", UpdatedAt: now}))
	rows, err = d.SourceStates()
	require.NoError(t, err)
	require.Equal(t, "1000", rows[0].LastMessageID)
	require.Equal(t, "ready", rows[0].State)
	require.True(t, rows[0].LastEventAt.Equal(now.Add(time.Minute)))
	// A failed progress write rolls back the receipt, so the collector retains it.
	require.NoError(t, s.GormDB().Exec("CREATE TRIGGER reject_progress BEFORE UPDATE ON discord_source_states BEGIN SELECT RAISE(ABORT, 'injected'); END").Error)
	err = d.CommitInbound(&DiscordInbound{EventID: "failed", Kind: "MESSAGE_CREATE", ChannelID: "channel", MessageID: "1001", ReceivedAt: now}, nil, nil)
	require.Error(t, err)
	var n int64
	require.NoError(t, s.GormDB().Model(&DiscordInbound{}).Where("event_id = ?", "failed").Count(&n).Error)
	require.Zero(t, n)
}

func TestRecognitionStatsCountsLiveCallsAndRulesSeparately(t *testing.T) {
	s := logTestStore(t)
	for _, r := range []CopyTradeAIRun{
		{TraderID: "one", Model: "deterministic:tyler_v1"},
		{TraderID: "one", Model: "gpt-test"},
		{TraderID: "one", Model: "gpt-test", Error: "timeout"},
		{TraderID: "other", Model: "gpt-test"},
	} {
		require.NoError(t, s.CopyTrade().CreateAIRun(&r))
	}
	stats, err := s.CopyTrade().RecognitionStats("one")
	require.NoError(t, err)
	require.EqualValues(t, 3, stats.RecognitionRuns)
	require.EqualValues(t, 2, stats.ModelCalls)
	require.EqualValues(t, 1, stats.DeterministicRuns)
	empty, err := s.CopyTrade().RecognitionStats("none")
	require.NoError(t, err)
	require.Zero(t, empty.ModelCalls)
	require.Nil(t, empty.EarliestRun)
}
