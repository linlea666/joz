package store

import (
	"encoding/json"
	"fmt"
	"github.com/stretchr/testify/require"
	"strings"
	"testing"
	"time"
)

func logTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := New(t.TempDir() + "/logs.db")
	require.NoError(t, err)
	t.Cleanup(func() { s.Close() })
	return s
}
func TestSystemLogsBoundsRedactionOwnershipAndCleanup(t *testing.T) {
	s := logTestStore(t)
	require.NoError(t, s.GormDB().Exec("INSERT INTO traders(id,user_id,name,ai_model_id,exchange_id,initial_balance) VALUES('mine','u','mine','a','e',0),('other','v','other','a','e',0)").Error)
	now := time.Now().UTC()
	old := now.Add(-48 * time.Hour)
	for _, e := range []SystemEvent{{EventID: "global", Event: "gateway.error", OccurredAt: old, ContextJSON: `{"code":"SAFE","token":"secret","password":"hidden","stage":"receive"}`}, {EventID: "owned", TraderID: "mine", OccurredAt: old}, {EventID: "private", TraderID: "other", OccurredAt: old}} {
		require.NoError(t, s.Logs().Append(&e))
	}
	require.NoError(t, s.Logs().Append(&SystemEvent{EventID: "global", Message: "duplicate"}))
	upper, count, err := s.Logs().CleanupPreview("u", now)
	require.NoError(t, err)
	require.EqualValues(t, 2, count)
	require.NoError(t, s.Logs().Append(&SystemEvent{EventID: "late", OccurredAt: old}))
	f := LogFilter{Scope: "system", UserID: "u", Upper: -1, Limit: 100}
	rows, err := s.Logs().ListLogs(f)
	require.NoError(t, err)
	require.Len(t, rows, 3)
	for _, row := range rows {
		require.NotContains(t, row.ContextJSON, "secret")
		require.NotContains(t, row.ContextJSON, "hidden")
	}
	n, err := s.Logs().Cleanup("u", now, upper)
	require.NoError(t, err)
	require.EqualValues(t, 2, n)
	rows, err = s.Logs().ListLogs(f)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, "late", rows[0].EventID)
	var other int64
	require.NoError(t, s.GormDB().Model(&SystemEvent{}).Where("event_id='private'").Count(&other).Error)
	require.EqualValues(t, 1, other)
	stats, err := s.Logs().Stats()
	require.NoError(t, err)
	require.EqualValues(t, 2, stats["count"])
}
func TestLogsQuotaAggregationAndFailures(t *testing.T) {
	s := logTestStore(t)
	l := s.Logs()
	l.Record("discord", "fault", "error", "fault", nil)
	l.Record("discord", "fault", "error", "fault", nil)
	for _, w := range l.windows {
		w.start = time.Now().Add(-61 * time.Second)
	}
	l.Flush()
	rows, err := l.ListLogs(LogFilter{Scope: "system", UserID: "u", Upper: -1})
	require.NoError(t, err)
	require.Len(t, rows, 2)
	require.Contains(t, rows[0].ContextJSON, `"repeat_count":1`)
	require.NoError(t, l.Retain(1))
	rows, err = l.ListLogs(LogFilter{Scope: "system", UserID: "u", Upper: -1})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, "logs.retention", rows[0].Event)
	require.NoError(t, s.GormDB().Migrator().DropTable(&SystemEvent{}))
	require.Error(t, l.Append(&SystemEvent{EventID: "failed"}))
	require.EqualValues(t, 1, l.FailureCount())
}
func TestLogRetentionProtectsUncertainAndActiveEvidence(t *testing.T) {
	s := logTestStore(t)
	db := s.GormDB()
	old := time.Now().AddDate(0, 0, -120)
	for _, id := range []string{"active", "uncertain", "terminal"} {
		state := "CLOSED"
		if id == "active" {
			state = "OPEN"
		}
		require.NoError(t, db.Create(&CopyTradeContext{ID: id, TraderID: "t", State: state, EntrySignalID: id, CreatedAt: old, UpdatedAt: old}).Error)
		run := CopyTradeAIRun{TraderID: "t", MessageID: id, CreatedAt: old}
		require.NoError(t, db.Create(&run).Error)
		sig := CopyTradeSignal{ID: id, TraderID: "t", MessageID: id, TradeContextID: id, Status: SignalStatusExecuted, AIRunID: run.ID, CreatedAt: old, UpdatedAt: old}
		require.NoError(t, db.Create(&sig).Error)
		require.NoError(t, db.Create(&CopyTradeEvent{SignalID: id, MessageID: id, OccurredAt: old, CreatedAt: old}).Error)
	}
	require.NoError(t, db.Create(&CopyTradeAction{ID: "pending", ContextID: "uncertain", SignalID: "uncertain", TraderID: "t", Status: "uncertain"}).Error)
	n, err := s.CopyTrade().CleanOldEvents(90)
	require.NoError(t, err)
	require.EqualValues(t, 1, n)
	n, err = s.CopyTrade().CleanOldAIRuns(30)
	require.NoError(t, err)
	require.EqualValues(t, 1, n)
	n, err = s.CopyTrade().CleanOldSignals(90)
	require.NoError(t, err)
	require.EqualValues(t, 1, n)
	var signals []CopyTradeSignal
	require.NoError(t, db.Find(&signals).Error)
	require.Len(t, signals, 2)
}
func TestTradeLogCursorAndTraceAccess(t *testing.T) {
	s := logTestStore(t)
	db := s.GormDB()
	require.NoError(t, db.Exec("INSERT INTO traders(id,user_id,name,ai_model_id,exchange_id,initial_balance) VALUES('mine','u','mine','a','e',0),('other','v','other','a','e',0)").Error)
	require.NoError(t, db.Create(&CopyTradeSignal{ID: "s1", TraderID: "mine", ChannelID: "actual", LogicalChannelID: "logical", MessageID: "message", SourceEventID: "source"}).Error)
	require.NoError(t, db.Create(&CopyTradeEvent{TraderID: "mine", SignalID: "s1", TraceID: "s1", Event: "copytrade.signal.received"}).Error)
	require.NoError(t, db.Create(&CopyTradeSignal{ID: "private", TraderID: "other"}).Error)
	require.NoError(t, db.Create(&CopyTradeEvent{TraderID: "other", SignalID: "private", Event: "copytrade.signal.received"}).Error)
	f := LogFilter{Scope: "trade", UserID: "u", Upper: -1}
	upper, n, err := s.Logs().LogBoundary(f)
	require.NoError(t, err)
	require.EqualValues(t, 1, n)
	f.Upper = upper
	rows, err := s.Logs().ListLogs(f)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, "actual", rows[0].ChannelID)
	require.Equal(t, "logical", rows[0].LogicalChannelID)
	_, err = s.Logs().Trace("u", "", "private", "")
	require.Error(t, err)
	trace, err := s.Logs().Trace("u", "", "s1", "")
	require.NoError(t, err)
	require.NotNil(t, trace["signals"])
}
func TestLogSanitization(t *testing.T) {
	clean := SanitizeLogJSON(`{"authorization":"private","nested":"{\"token\":\"hidden\"}","text":"token=secret Bearer opaque","ok":42}`)
	require.NotContains(t, clean, "private")
	require.NotContains(t, clean, "hidden")
	require.NotContains(t, clean, "secret")
	require.NotContains(t, clean, "opaque")
	require.True(t, json.Valid([]byte(clean)))
	s := logTestStore(t)
	ctx, _ := json.Marshal(map[string]any{"code": strings.Repeat("a", 100000), "token": "sensitive"})
	e := SystemEvent{ContextJSON: string(ctx)}
	require.NoError(t, s.Logs().Append(&e))
	require.Less(t, e.ContentBytes, int64(16384))
}

func TestSystemRetentionOptionsAndCapacityBoundary(t *testing.T) {
	for _, days := range []int{7, 30, 90} {
		t.Run(fmt.Sprint(days), func(t *testing.T) {
			s := logTestStore(t)
			require.NoError(t, s.SetSystemConfig("system_log_retention_days", fmt.Sprint(days)))
			for _, offset := range []int{days - 1, days + 1} {
				require.NoError(t, s.Logs().Append(&SystemEvent{EventID: fmt.Sprint(offset), OccurredAt: time.Now().AddDate(0, 0, -offset)}))
			}
			require.NoError(t, s.Logs().Retain(512<<20))
			var count int64
			require.NoError(t, s.GormDB().Model(&SystemEvent{}).Where("event_id=?", fmt.Sprint(days-1)).Count(&count).Error)
			require.EqualValues(t, 1, count)
			require.NoError(t, s.GormDB().Model(&SystemEvent{}).Where("event_id=?", fmt.Sprint(days+1)).Count(&count).Error)
			require.EqualValues(t, 0, count)
		})
	}
}

func TestIncrementalAuditSchemaPreservesExistingEvidence(t *testing.T) {
	path := t.TempDir() + "/upgrade.db"
	s, err := New(path)
	require.NoError(t, err)
	require.NoError(t, s.GormDB().Create(&CopyTradeSignal{ID: "historical", TraderID: "t", Status: SignalStatusExecuted}).Error)
	require.NoError(t, s.GormDB().Create(&CopyTradeEvent{TraderID: "t", SignalID: "historical", Event: "old"}).Error)
	require.NoError(t, s.GormDB().Migrator().DropTable(&SystemEvent{}, &LogCleanupTicket{}))
	require.NoError(t, s.GormDB().Migrator().DropColumn(&CopyTradeEvent{}, "SourceEventID"))
	require.NoError(t, s.GormDB().Migrator().DropColumn(&CopyTradeEvent{}, "DeliveryID"))
	require.NoError(t, s.GormDB().Migrator().DropColumn(&CopyTradeEvent{}, "LogicalChannelID"))
	require.NoError(t, s.Close())
	s, err = New(path)
	require.NoError(t, err)
	defer s.Close()
	var n int64
	require.NoError(t, s.GormDB().Model(&CopyTradeSignal{}).Where("id='historical'").Count(&n).Error)
	require.EqualValues(t, 1, n)
	require.NoError(t, s.GormDB().Model(&CopyTradeEvent{}).Count(&n).Error)
	require.EqualValues(t, 1, n)
	require.True(t, s.GormDB().Migrator().HasTable(&SystemEvent{}))
}
