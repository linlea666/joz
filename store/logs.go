package store

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// SystemEvent contains diagnostic evidence only. Trading ledgers remain authoritative.
type SystemEvent struct {
	ID           int64     `gorm:"primaryKey" json:"id"`
	EventID      string    `gorm:"uniqueIndex;not null" json:"event_id"`
	TraderID     string    `gorm:"index" json:"trader_id,omitempty"`
	UserID       string    `gorm:"index" json:"-"`
	ChannelID    string    `gorm:"index" json:"channel_id,omitempty"`
	Component    string    `gorm:"index" json:"component"`
	Level        string    `gorm:"index" json:"level"`
	Event        string    `gorm:"index" json:"event"`
	Message      string    `json:"message"`
	ContextJSON  string    `json:"context_json"`
	OccurredAt   time.Time `gorm:"index" json:"occurred_at"`
	ContentBytes int64     `json:"-"`
}

func (SystemEvent) TableName() string { return "system_events" }

type logWindow struct {
	event SystemEvent
	start time.Time
	count int
}
type LogStore struct {
	db       *gorm.DB
	failures atomic.Uint64
	mu       sync.Mutex
	windows  map[string]*logWindow
}

func (s *Store) Logs() *LogStore {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.logs == nil {
		s.logs = &LogStore{db: s.gdb, windows: map[string]*logWindow{}}
	}
	return s.logs
}

var contextKeys = map[string]bool{}

func init() {
	for _, k := range strings.Fields("code stage last_heartbeat exception_type close_code file function line channel_id state previous_state config_version repeat_count generation count cutoff upper_id retention_days truncated original_bytes source_event_id signal_id delivery_id message_id context_id order_id duration_ms result") {
		contextKeys[k] = true
	}
}

var credentialText = regexp.MustCompile(`(?i)(Bearer\s+[^\s"']+|(?:mfa\.[\w-]{20,}|[\w-]{23,}\.[\w-]{6}\.[\w-]{20,})|(?:api[_-]?key|authorization|password|token|secret|smtp_pass)\s*[=:]\s*[^\s,;"}]+)`)

func RedactLogText(s string) string { return credentialText.ReplaceAllString(s, "[REDACTED]") }
func safeText(s string, n int) string {
	s = RedactLogText(s)
	if len(s) > n {
		return strings.ToValidUTF8(s[:n], "") + "…"
	}
	return s
}

// SanitizeLogJSON also handles nested JSON strings used by existing audit records.
func SanitizeLogJSON(raw string) string {
	var v any
	if json.Unmarshal([]byte(raw), &v) != nil {
		return safeText(raw, 16384)
	}
	var clean func(any) any
	clean = func(v any) any {
		switch x := v.(type) {
		case map[string]any:
			for k, val := range x {
				low := strings.ToLower(k)
				if strings.Contains(low, "password") || strings.Contains(low, "secret") || strings.Contains(low, "authorization") || strings.Contains(low, "api_key") || strings.Contains(low, "credential") || low == "token" || low == "session" || low == "execution_key" {
					x[k] = "[REDACTED]"
				} else {
					x[k] = clean(val)
				}
			}
		case []any:
			for i := range x {
				x[i] = clean(x[i])
			}
		case string:
			if (strings.HasPrefix(x, "{") || strings.HasPrefix(x, "[")) && json.Valid([]byte(x)) {
				var inner any
				_ = json.Unmarshal([]byte(x), &inner)
				b, _ := json.Marshal(clean(inner))
				return string(b)
			}
			return RedactLogText(x)
		}
		return v
	}
	b, _ := json.Marshal(clean(v))
	return string(b)
}
func (s *LogStore) FailureCount() uint64 { return s.failures.Load() }
func (s *LogStore) NoteFailure() {
	s.failures.Add(1)
	fmt.Fprintln(os.Stderr, "diagnostic evidence incomplete; persistence failed")
}
func (s *LogStore) Append(ev *SystemEvent) error {
	ev.Level = strings.ToLower(ev.Level)
	switch ev.Level {
	case "info", "success", "warn", "error":
	default:
		ev.Level = "info"
	}
	ev.Component = safeText(ev.Component, 64)
	ev.Event = safeText(ev.Event, 128)
	ev.Message = safeText(ev.Message, 1024)
	if ev.EventID == "" {
		ev.EventID = uuid.NewString()
	}
	if len(ev.EventID) > 128 {
		return fmt.Errorf("invalid diagnostic identity")
	}
	var ctx map[string]any
	_ = json.Unmarshal([]byte(ev.ContextJSON), &ctx)
	safe := map[string]any{}
	for k, v := range ctx {
		if contextKeys[k] {
			switch t := v.(type) {
			case string:
				safe[k] = safeText(t, 512)
			case float64, bool:
				safe[k] = t
			}
		}
	}
	b, _ := json.Marshal(safe)
	if len(b) > 14000 {
		b = []byte(`{"truncated":true}`)
	}
	if len(ev.ContextJSON) > len(b)+64 {
		safe["truncated"] = true
		safe["original_bytes"] = len(ev.ContextJSON)
		b, _ = json.Marshal(safe)
	}
	ev.ContextJSON = string(b)
	if ev.OccurredAt.IsZero() {
		ev.OccurredAt = time.Now().UTC()
	}
	encoded, _ := json.Marshal(ev)
	if len(encoded) > 16384 {
		ev.Message = safeText(ev.Message, 256)
		ev.ContextJSON = fmt.Sprintf(`{"truncated":true,"original_bytes":%d}`, len(encoded))
		encoded, _ = json.Marshal(ev)
	}
	ev.ContentBytes = int64(len(encoded))
	err := s.db.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "event_id"}}, DoNothing: true}).Create(ev).Error
	if err != nil {
		s.failures.Add(1)
		fmt.Fprintln(os.Stderr, "system log persistence failed; diagnostic evidence incomplete")
	}
	return err
}

// Record is best effort: never change an execution result because diagnostics failed.
func (s *LogStore) Record(component, event, level, message string, ctx map[string]any) {
	s.Flush()
	b, _ := json.Marshal(ctx)
	ev := SystemEvent{Component: component, Event: event, Level: level, Message: message, ContextJSON: string(b)}
	if level == "error" || level == "warn" {
		key := component + "/" + event + "/" + string(b)
		s.mu.Lock()
		if w := s.windows[key]; w != nil && time.Since(w.start) < time.Minute {
			w.count++
			s.mu.Unlock()
			return
		}
		// Bound aggregation memory even under a burst of distinct failures.
		if len(s.windows) < 1000 {
			s.windows[key] = &logWindow{event: ev, start: time.Now()}
		}
		s.mu.Unlock()
	}
	_ = s.Append(&ev)
}
func (s *LogStore) Flush() {
	s.mu.Lock()
	var batch []SystemEvent
	for key, w := range s.windows {
		if time.Since(w.start) >= time.Minute {
			if w.count > 0 {
				e := w.event
				var ctx map[string]any
				_ = json.Unmarshal([]byte(e.ContextJSON), &ctx)
				if ctx == nil {
					ctx = map[string]any{}
				}
				ctx["repeat_count"] = w.count
				b, _ := json.Marshal(ctx)
				e.ContextJSON = string(b)
				e.Event += ".repeated"
				batch = append(batch, e)
			}
			delete(s.windows, key)
		}
	}
	s.mu.Unlock()
	for i := range batch {
		_ = s.Append(&batch[i])
	}
}
func (s *LogStore) SystemDays() (int, error) {
	var val string
	err := s.db.Raw("SELECT value FROM system_config WHERE key=?", "system_log_retention_days").Scan(&val).Error
	if err != nil {
		return 0, err
	}
	if val == "" {
		return 30, nil
	}
	n, e := strconv.Atoi(val)
	if e != nil || (n != 7 && n != 30 && n != 90) {
		return 0, fmt.Errorf("invalid saved retention")
	}
	return n, nil
}
func EffectiveRetention() map[string]int {
	m := map[string]int{"events": 90, "signals": 90, "ai": 30, "messages": 30, "media": 30}
	for k, env := range map[string]string{"events": "COPYTRADE_EVENT_RETENTION_DAYS", "signals": "COPYTRADE_SIGNAL_RETENTION_DAYS", "ai": "COPYTRADE_AIRUN_RETENTION_DAYS", "messages": "DISCORD_MESSAGE_RETENTION_DAYS", "media": "DISCORD_MEDIA_RETENTION_DAYS"} {
		if n, e := strconv.Atoi(os.Getenv(env)); e == nil && n > 0 {
			m[k] = n
		}
	}
	m["ingest"] = max(30, m["signals"])
	return m
}
func (s *LogStore) Stats() (map[string]any, error) {
	var row struct {
		Count int64
		Bytes int64
	}
	if err := s.db.Model(&SystemEvent{}).Select("count(*) AS count, coalesce(sum(content_bytes),0) AS bytes").Scan(&row).Error; err != nil {
		return nil, err
	}
	var first []SystemEvent
	if err := s.db.Select("occurred_at").Order("occurred_at").Limit(1).Find(&first).Error; err != nil {
		return nil, err
	}
	var earliest *time.Time
	if len(first) > 0 {
		earliest = &first[0].OccurredAt
	}
	return map[string]any{"count": row.Count, "logical_bytes": row.Bytes, "earliest": earliest, "write_failures": s.FailureCount()}, nil
}
func (s *LogStore) visible(q *gorm.DB, user string) *gorm.DB {
	return q.Where("(user_id = ? OR (user_id = '' AND (trader_id = '' OR trader_id IN (SELECT id FROM traders WHERE user_id = ?))))", user, user)
}
func (s *LogStore) CleanupPreview(user string, cutoff time.Time) (int64, int64, error) {
	q := s.visible(s.db.Model(&SystemEvent{}), user).Where("occurred_at < ?", cutoff)
	var upper int64
	var n int64
	if err := q.Select("coalesce(max(id),0)").Scan(&upper).Error; err != nil {
		return 0, 0, err
	}
	err := s.visible(s.db.Model(&SystemEvent{}), user).Where("occurred_at < ? AND id <= ?", cutoff, upper).Count(&n).Error
	return upper, n, err
}
func (s *LogStore) Cleanup(user string, cutoff time.Time, upper int64) (int64, error) {
	var total int64
	for {
		var ids []int64
		err := s.visible(s.db.Model(&SystemEvent{}), user).Where("occurred_at < ? AND id <= ?", cutoff, upper).Order("id").Limit(500).Pluck("id", &ids).Error
		if err != nil {
			return total, err
		}
		if len(ids) == 0 {
			return total, nil
		}
		r := s.db.Where("id IN ?", ids).Delete(&SystemEvent{})
		total += r.RowsAffected
		if r.Error != nil {
			return total, r.Error
		}
	}
}

// Retain is bounded per batch; no VACUUM, no interaction with trading ledgers.
func (s *LogStore) Retain(capacity int64) error {
	_ = s.db.Where("expires_at < ?", time.Now().Add(-24*time.Hour)).Delete(&LogCleanupTicket{}).Error
	days, err := s.SystemDays()
	if err != nil {
		return err
	}
	cutoff := time.Now().UTC().AddDate(0, 0, -days)
	var removed int64
	for {
		var ids []int64
		if err := s.db.Model(&SystemEvent{}).Where("occurred_at < ?", cutoff).Order("id").Limit(500).Pluck("id", &ids).Error; err != nil {
			return err
		}
		if len(ids) == 0 {
			break
		}
		r := s.db.Where("id IN ?", ids).Delete(&SystemEvent{})
		if r.Error != nil {
			return r.Error
		}
		removed += r.RowsAffected
	}
	var quota int64
	for {
		var size int64
		if err := s.db.Model(&SystemEvent{}).Select("coalesce(sum(content_bytes),0)").Scan(&size).Error; err != nil {
			return err
		}
		if size <= capacity {
			break
		}
		var ids []int64
		if err := s.db.Model(&SystemEvent{}).Order("occurred_at,id").Limit(500).Pluck("id", &ids).Error; err != nil {
			return err
		}
		if len(ids) == 0 {
			break
		}
		r := s.db.Where("id IN ?", ids).Delete(&SystemEvent{})
		if r.Error != nil {
			return r.Error
		}
		quota += r.RowsAffected
	}
	if removed+quota > 0 {
		s.Record("logs", "logs.retention", "info", "历史系统日志已按保留规则清理", map[string]any{"count": removed + quota, "retention_days": days, "result": fmt.Sprintf("age=%d quota=%d", removed, quota)})
	}
	return nil
}

type LogCleanupTicket struct {
	ID        string `gorm:"primaryKey"`
	UserID    string
	Cutoff    time.Time
	UpperID   int64
	ExpiresAt time.Time `gorm:"index"`
	Consumed  bool
}

func (s *LogStore) UserAudit(user, event, message string, ctx map[string]any) {
	b, _ := json.Marshal(ctx)
	_ = s.Append(&SystemEvent{UserID: user, Component: "logs", Event: event, Level: "info", Message: message, ContextJSON: string(b)})
}
