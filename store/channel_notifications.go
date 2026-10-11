package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/mail"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const ChannelNotificationConfigKey = "discord_channel_notifications"

var ErrNotificationConflict = errors.New("notification configuration changed; refresh and retry")

type ChannelMonitor struct {
	ID          string    `json:"id"`
	ChannelID   string    `json:"channel_id"`
	Label       string    `json:"label"`
	Enabled     bool      `json:"enabled"`
	Generation  uint64    `json:"generation"`
	ActivatedAt time.Time `json:"activated_at"`
}
type ChannelNotificationConfig struct {
	RecipientSince time.Time        `json:"recipient_since"`
	Revision       uint64           `json:"revision"`
	Enabled        bool             `json:"enabled"`
	Recipient      string           `json:"recipient"`
	Monitors       []ChannelMonitor `json:"monitors"`
}

// A separate ledger: no trading signal, execution identity or SMTP credential.
type ChannelNotification struct {
	ID            int64     `gorm:"primaryKey" json:"id"`
	MonitorID     string    `gorm:"uniqueIndex:notification_identity,priority:1" json:"monitor_id"`
	Generation    uint64    `gorm:"uniqueIndex:notification_identity,priority:2" json:"generation"`
	ChannelID     string    `gorm:"uniqueIndex:notification_identity,priority:3;index" json:"channel_id"`
	MessageID     string    `gorm:"uniqueIndex:notification_identity,priority:4" json:"message_id"`
	SourceEventID string    `json:"source_event_id"`
	Label         string    `json:"-"`
	Recipient     string    `json:"-"`
	MessageJSON   string    `json:"-"`
	Status        string    `gorm:"index" json:"status"`
	Attempts      int       `json:"attempts"`
	Error         string    `json:"error,omitempty"`
	ReceivedAt    time.Time `json:"received_at"`
	ExpiresAt     time.Time `json:"expires_at"`
	NextAttemptAt time.Time `json:"-"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

func notificationConfig(db *gorm.DB) (ChannelNotificationConfig, string, error) {
	var row struct{ Value string }
	err := db.Raw("SELECT value FROM system_config WHERE key=?", ChannelNotificationConfigKey).Scan(&row).Error
	cfg := ChannelNotificationConfig{Monitors: []ChannelMonitor{}}
	if err == nil && row.Value != "" {
		err = json.Unmarshal([]byte(row.Value), &cfg)
	}
	return cfg, row.Value, err
}
func (s *Store) ChannelNotifications() (ChannelNotificationConfig, error) {
	cfg, _, err := notificationConfig(s.gdb)
	return cfg, err
}
func notificationActive(cfg ChannelNotificationConfig, id string, generation uint64) bool {
	if !cfg.Enabled {
		return false
	}
	for _, m := range cfg.Monitors {
		if m.ID == id && m.Generation == generation && m.Enabled {
			return true
		}
	}
	return false
}
func (s *Store) SaveChannelNotifications(next ChannelNotificationConfig, now time.Time) (ChannelNotificationConfig, error) {
	if len(next.Monitors) > 20 {
		return next, fmt.Errorf("at most 20 channels are supported")
	}
	next.Recipient = strings.TrimSpace(next.Recipient)
	if next.Enabled || next.Recipient != "" {
		parsed, e := mail.ParseAddress(next.Recipient)
		if e != nil || parsed.Address != next.Recipient || strings.ContainsAny(next.Recipient, "\r\n") {
			return next, fmt.Errorf("a single valid notification email is required")
		}
	}
	cancelled := []ChannelNotification{}
	err := s.gdb.Transaction(func(tx *gorm.DB) error {
		old, raw, err := notificationConfig(tx)
		if err != nil {
			return err
		}
		if next.Revision != old.Revision {
			return ErrNotificationConflict
		}
		next.RecipientSince = old.RecipientSince
		if next.Recipient != old.Recipient {
			next.RecipientSince = now
		}
		seen := map[string]bool{}
		ids := map[string]bool{}
		monitors := []ChannelMonitor{}
		for _, m := range next.Monitors {
			m.ChannelID = strings.TrimSpace(m.ChannelID)
			m.Label = strings.TrimSpace(m.Label)
			if len(m.ChannelID) < 17 || len(m.ChannelID) > 20 || len(m.Label) > 120 {
				return fmt.Errorf("invalid channel ID or label (maximum 120 bytes)")
			}
			if id, e := strconv.ParseUint(m.ChannelID, 10, 64); e != nil || id == 0 || strconv.FormatUint(id, 10) != m.ChannelID {
				return fmt.Errorf("channel ID must be an unsigned decimal Discord ID")
			}
			if seen[m.ChannelID] {
				continue
			}
			seen[m.ChannelID] = true
			var previous *ChannelMonitor
			for i := range old.Monitors {
				if old.Monitors[i].ID == m.ID && m.ID != "" {
					previous = &old.Monitors[i]
					break
				}
			}
			if m.ID != "" && (previous == nil || previous.ChannelID != m.ChannelID) {
				return fmt.Errorf("unknown monitor or channel identity changed")
			}
			if m.ID == "" {
				m.ID = uuid.NewString()
				m.Generation = 1
				m.ActivatedAt = now
			} else {
				m.Generation = previous.Generation
				m.ActivatedAt = previous.ActivatedAt
				if m.Enabled && next.Enabled && (!previous.Enabled || !old.Enabled) {
					m.Generation++
					m.ActivatedAt = now
				}
			}
			if ids[m.ID] {
				return fmt.Errorf("duplicate monitor identity")
			}
			ids[m.ID] = true
			monitors = append(monitors, m)
		}
		next.Monitors = monitors
		next.Revision = old.Revision + 1
		data, err := json.Marshal(next)
		if err != nil {
			return err
		}
		if raw == "" {
			r := tx.Exec("INSERT INTO system_config(key,value) VALUES (?,?) ON CONFLICT(key) DO NOTHING", ChannelNotificationConfigKey, string(data))
			if r.Error != nil {
				return r.Error
			}
			if r.RowsAffected != 1 {
				return ErrNotificationConflict
			}
		} else {
			r := tx.Exec("UPDATE system_config SET value=? WHERE key=? AND value=?", string(data), ChannelNotificationConfigKey, raw)
			if r.Error != nil {
				return r.Error
			}
			if r.RowsAffected != 1 {
				return ErrNotificationConflict
			}
		}
		var pending []ChannelNotification
		if err = tx.Where("status IN ?", []string{"pending", "retry"}).Find(&pending).Error; err != nil {
			return err
		}
		for _, job := range pending {
			if job.Recipient != next.Recipient || !notificationActive(next, job.MonitorID, job.Generation) {
				if err = tx.Model(&ChannelNotification{}).Where("id=? AND status IN ?", job.ID, []string{"pending", "retry"}).Updates(map[string]any{"status": "cancelled", "error": "notification configuration changed", "updated_at": now}).Error; err != nil {
					return err
				}
				cancelled = append(cancelled, job)
			}
		}
		return nil
	})
	if err == nil {
		for _, job := range cancelled {
			context, _ := json.Marshal(map[string]any{"source_event_id": job.SourceEventID, "message_id": job.MessageID, "delivery_id": job.ID, "result": "cancelled", "code": "NOTIFICATION_CONFIG_CHANGED"})
			if e := s.Logs().Append(&SystemEvent{Component: "channel_notify", Event: "channel_notify.cancelled", Level: "info", Message: "通知配置变更，已取消待发送邮件", ChannelID: job.ChannelID, ContextJSON: string(context)}); e != nil {
				s.Logs().NoteFailure()
			}
		}
	}
	return next, err
}

// Startup never resumes old mail. A DATA acceptance lost to a crash is unknown.
func (s *Store) ResetChannelNotifications(now time.Time) error {
	return s.gdb.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&ChannelNotification{}).Where("status=?", "sending").Updates(map[string]any{"status": "uncertain", "error": "process stopped during SMTP attempt; not resent", "updated_at": now}).Error; err != nil {
			return err
		}
		return tx.Model(&ChannelNotification{}).Where("status IN ?", []string{"pending", "retry"}).Updates(map[string]any{"status": "cancelled", "error": "restart boundary; no catch-up", "updated_at": now}).Error
	})
}

// This transaction is intentionally outside CommitInbound: email failures may
// not withhold the collector's receipt ACK or affect a trading delivery.
func (s *Store) QueueChannelNotifications(epoch string, cursor int64, now time.Time) (int64, []ChannelNotification, error) {
	next := cursor
	jobs := []ChannelNotification{}
	err := s.gdb.Transaction(func(tx *gorm.DB) error {
		var events []DiscordInbound
		if err := tx.Where("id>? AND notification_epoch=?", cursor, epoch).Order("id").Limit(100).Find(&events).Error; err != nil {
			return err
		}
		if len(events) == 0 {
			return nil
		}
		cfg, _, err := notificationConfig(tx)
		if err != nil {
			return err
		}
		var count int64
		if err = tx.Model(&ChannelNotification{}).Where("status IN ?", []string{"pending", "retry", "sending"}).Count(&count).Error; err != nil {
			return err
		}
		for _, ev := range events {
			next = ev.ID
			if ev.ReceivedAt.Before(cfg.RecipientSince) || !cfg.Enabled || !ev.NotificationRealtime || ev.NotificationEpoch != epoch || ev.Baseline || ev.Kind != "MESSAGE_CREATE" {
				continue
			}
			var snapshot DiscordSourceConfig
			if err = tx.First(&snapshot, "version=?", ev.ConfigVersion).Error; err != nil {
				return err
			}
			var frozen ChannelNotificationConfig
			if snapshot.NotificationsJSON == "" {
				continue
			}
			if err = json.Unmarshal([]byte(snapshot.NotificationsJSON), &frozen); err != nil {
				return err
			}
			var msg DiscordMessage
			if err = json.Unmarshal([]byte(ev.NotificationMessageJSON), &msg); err != nil {
				return err
			}
			var payload struct {
				Type int `json:"type"`
			}
			if err = json.Unmarshal([]byte(ev.Payload), &payload); err != nil {
				return err
			}
			if payload.Type != 0 && payload.Type != 19 {
				continue
			}
			for _, m := range frozen.Monitors {
				if !frozen.Enabled || !m.Enabled || m.ChannelID != ev.ChannelID || msg.MessageTimestamp.Before(m.ActivatedAt) || !notificationActive(cfg, m.ID, m.Generation) {
					continue
				}
				job := ChannelNotification{MonitorID: m.ID, Generation: m.Generation, ChannelID: ev.ChannelID, MessageID: ev.MessageID, SourceEventID: ev.EventID, Label: m.Label, Recipient: cfg.Recipient, MessageJSON: ev.NotificationMessageJSON, Status: "pending", ReceivedAt: ev.ReceivedAt, ExpiresAt: ev.ReceivedAt.Add(2 * time.Minute), NextAttemptAt: now}
				if !now.Before(job.ExpiresAt) {
					job.Status = "expired"
					job.Error = "notification exceeded two-minute deadline"
				} else if count >= 1000 {
					job.Status = "dropped"
					job.Error = "notification queue capacity exceeded"
				}
				r := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&job)
				if r.Error != nil {
					return r.Error
				}
				if r.RowsAffected > 0 {
					jobs = append(jobs, job)
					if job.Status == "pending" {
						count++
					}
				}
			}
		}
		// Cursor and inserts commit together. Queries are scoped to this process
		// epoch, so startup never scans or resends prior receipt history.
		return tx.Exec("INSERT INTO system_config(key,value) VALUES (?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", "discord_channel_notification_cursor", strconv.FormatInt(next, 10)).Error
	})
	if err != nil {
		return cursor, nil, err
	}
	return next, jobs, nil
}

func (s *Store) ClaimChannelNotification(now time.Time) (*ChannelNotification, error) {
	var result *ChannelNotification
	err := s.gdb.Transaction(func(tx *gorm.DB) error {
		var job ChannelNotification
		// The single sender has a 20-second total SMTP timeout. A stale claim
		// indicates an uncommitted outcome; never feed it back to SMTP.
		err := tx.Where("status=? AND updated_at<?", "sending", now.Add(-30*time.Second)).Order("id").First(&job).Error
		if err == nil {
			job.Status, job.Error = "uncertain", "SMTP attempt outcome not committed; not resent"
			if e := tx.Model(&ChannelNotification{}).Where("id=? AND status=?", job.ID, "sending").Updates(map[string]any{"status": job.Status, "error": job.Error, "updated_at": now}).Error; e != nil {
				return e
			}
			result = &job
			return nil
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		err = tx.Where("status IN ? AND next_attempt_at<=?", []string{"pending", "retry"}, now).Order("id").First(&job).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		cfg, _, err := notificationConfig(tx)
		if err != nil {
			return err
		}
		var dc DiscordConfig
		if err = tx.First(&dc, 1).Error; err != nil {
			return err
		}
		status := "sending"
		if !now.Before(job.ExpiresAt) {
			status = "expired"
		} else if !dc.Enabled || dc.Token == "" || job.Recipient != cfg.Recipient || !notificationActive(cfg, job.MonitorID, job.Generation) {
			status = "cancelled"
		}
		updates := map[string]any{"status": status, "updated_at": now}
		if status == "sending" {
			updates["attempts"] = gorm.Expr("attempts+1")
			job.Attempts++
		}
		r := tx.Model(&ChannelNotification{}).Where("id=? AND status IN ?", job.ID, []string{"pending", "retry"}).Updates(updates)
		if r.Error != nil {
			return r.Error
		}
		if r.RowsAffected == 1 {
			job.Status = status
			result = &job
		}
		return nil
	})
	return result, err
}
func (s *Store) FinishChannelNotification(job *ChannelNotification, status, detail string, next time.Time) error {
	r := s.gdb.Model(&ChannelNotification{}).Where("id=? AND status=?", job.ID, "sending").Updates(map[string]any{"status": status, "error": detail, "next_attempt_at": next, "updated_at": time.Now().UTC()})
	if r.Error != nil {
		return r.Error
	}
	if r.RowsAffected != 1 {
		return errors.New("notification result could not be committed")
	}
	return nil
}
func (s *Store) LatestChannelNotification(id string) (*ChannelNotification, error) {
	var job ChannelNotification
	err := s.gdb.Where("monitor_id=?", id).Order("id DESC").First(&job).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	return &job, err
}
func (s *Store) CleanChannelNotifications() error {
	return s.gdb.Where("updated_at<? AND status NOT IN ?", time.Now().UTC().AddDate(0, 0, -30), []string{"pending", "retry", "sending"}).Delete(&ChannelNotification{}).Error
}
