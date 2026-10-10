package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// DiscordInbound is an immutable receipt and normalized revision. ACK is sent
// only after this row and every recipient's delivery have committed together.
type DiscordInbound struct {
	ConfigVersion string    `json:"config_version"`
	Session       string    `json:"session,omitempty"`
	Sequence      int64     `json:"sequence,omitempty"`
	ID            int64     `gorm:"primaryKey" json:"id"`
	EventID       string    `gorm:"uniqueIndex;not null" json:"event_id"`
	Kind          string    `json:"kind"`
	ChannelID     string    `gorm:"index" json:"channel_id"`
	MessageID     string    `gorm:"index" json:"message_id"`
	Revision      int       `json:"revision"`
	Payload       string    `json:"-"`
	MessageJSON   string    `json:"-"`
	Baseline      bool      `json:"baseline"`
	ReceivedAt    time.Time `json:"received_at"`
	CreatedAt     time.Time `json:"created_at"`
}

type DiscordDelivery struct {
	ID           int64     `gorm:"primaryKey" json:"id"`
	EventID      string    `gorm:"uniqueIndex:idx_discord_delivery,priority:1" json:"event_id"`
	TraderID     string    `gorm:"uniqueIndex:idx_discord_delivery,priority:2;index" json:"trader_id"`
	MessageJSON  string    `json:"-"`
	RulesJSON    string    `json:"rules_json"`
	ExecutionKey string    `json:"-"`
	SourceTime   time.Time `gorm:"index" json:"source_time"`
	Status       string    `gorm:"index" json:"status"`
	LastError    string    `json:"last_error,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

type DiscordRoute struct {
	NeedsActivation bool
	Generation      uint64
	TraderID        string `gorm:"primaryKey"`
	ActivatedAt     time.Time
	ExecutionKey    string
}

type DiscordSourceState struct {
	ChannelID     string    `gorm:"primaryKey" json:"channel_id"`
	State         string    `json:"state"`
	LastError     string    `json:"last_error,omitempty"`
	LastMessageID string    `json:"last_message_id,omitempty"`
	LastEventAt   time.Time `json:"last_event_at,omitempty"`
	UpdatedAt     time.Time `json:"updated_at"`
}

func (s *DiscordMessageStore) CommitInbound(event *DiscordInbound, msg *DiscordMessage, recipients []DiscordDelivery) error {
	return s.db.Transaction(func(tx *gorm.DB) error {
		var count int64
		if err := tx.Model(&DiscordInbound{}).Where("event_id = ?", event.EventID).Count(&count).Error; err != nil {
			return err
		}
		if count > 0 {
			return nil
		}
		var queued int64
		if err := tx.Model(&DiscordDelivery{}).Where("status IN ?", []string{DiscordMsgPending, DiscordMsgProcessing}).Count(&queued).Error; err != nil {
			return err
		}
		if queued+int64(len(recipients)) > 10000 {
			return fmt.Errorf("delivery queue capacity exceeded; collector must retain event")
		}
		changed := false
		if msg != nil {
			result, err := NewDiscordMessageStore(tx).Upsert(msg)
			if err != nil {
				return err
			}
			changed = result != DiscordMsgUnchanged
			event.Revision = msg.Revision
			data, err := json.Marshal(msg)
			if err != nil {
				return err
			}
			event.MessageJSON = string(data)
			if event.Baseline {
				if err := tx.Model(&DiscordMessage{}).Where("id = ?", msg.ID).Update("processing_status", DiscordMsgSkipped).Error; err != nil {
					return err
				}
			}
		}
		if err := tx.Create(event).Error; err != nil {
			return err
		}
		if changed && !event.Baseline {
			for _, recipient := range recipients {
				recipient.EventID, recipient.MessageJSON, recipient.Status = event.EventID, event.MessageJSON, DiscordMsgPending
				recipient.SourceTime = msg.MessageTimestamp
				if msg.EditedAt != nil && msg.EditedAt.After(recipient.SourceTime) {
					recipient.SourceTime = *msg.EditedAt
				}
				if err := tx.Create(&recipient).Error; err != nil {
					return err
				}
			}
		}
		return nil
	})
}

func (s *DiscordMessageStore) RegisterRoute(traderID, executionKey string) (*DiscordRoute, error) {
	r := DiscordRoute{TraderID: traderID, ActivatedAt: time.Now().UTC(), ExecutionKey: executionKey}
	err := s.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "trader_id"}}, DoUpdates: clause.AssignmentColumns([]string{"execution_key"})}).Create(&r).Error; err != nil {
			return err
		}
		if err := tx.Model(&DiscordRoute{}).Where("trader_id = ? AND needs_activation = ?", traderID, true).Updates(map[string]interface{}{"activated_at": time.Now().UTC(), "needs_activation": false}).Error; err != nil {
			return err
		}
		return tx.First(&r, "trader_id = ?", traderID).Error
	})
	return &r, err
}

func (s *DiscordMessageStore) NextDelivery(traderID string) (*DiscordDelivery, error) {
	var d DiscordDelivery
	err := s.db.Where("trader_id = ? AND status = ?", traderID, DiscordMsgPending).Order("source_time ASC, id ASC").First(&d).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	r := s.db.Model(&DiscordDelivery{}).Where("id = ? AND status = ?", d.ID, DiscordMsgPending).Update("status", DiscordMsgProcessing)
	if r.Error != nil {
		return nil, r.Error
	}
	if r.RowsAffected != 1 {
		return nil, nil
	}
	return &d, nil
}

func (s *DiscordMessageStore) FinishDelivery(id int64, status, detail string) error {
	return s.db.Transaction(func(tx *gorm.DB) error {
		var delivery DiscordDelivery
		if err := tx.First(&delivery, "id = ?", id).Error; err != nil {
			return err
		}
		if err := tx.Model(&DiscordDelivery{}).Where("id = ?", id).Updates(map[string]interface{}{"status": status, "last_error": detail}).Error; err != nil {
			return err
		}
		if status != DiscordMsgDone {
			return nil
		}
		var pending int64
		if err := tx.Model(&DiscordDelivery{}).Where("event_id = ? AND status != ?", delivery.EventID, DiscordMsgDone).Count(&pending).Error; err != nil {
			return err
		}
		if pending > 0 {
			return nil
		}
		var event DiscordInbound
		if err := tx.First(&event, "event_id = ?", delivery.EventID).Error; err != nil {
			return err
		}
		return tx.Model(&DiscordMessage{}).Where("channel_id = ? AND message_id = ? AND revision = ?", event.ChannelID, event.MessageID, event.Revision).Update("processing_status", DiscordMsgDone).Error
	})
}
func (s *DiscordMessageStore) ResetDeliveries() error {
	return s.db.Model(&DiscordDelivery{}).Where("status = ?", DiscordMsgProcessing).Update("status", DiscordMsgPending).Error
}
func (s *DiscordMessageStore) PendingDeliveries() (int64, error) {
	var n int64
	err := s.db.Model(&DiscordDelivery{}).Where("status IN ?", []string{DiscordMsgPending, DiscordMsgProcessing}).Count(&n).Error
	return n, err
}
func (s *DiscordMessageStore) SaveSourceState(state *DiscordSourceState) error {
	return s.db.Save(state).Error
}
func (s *DiscordMessageStore) SourceStates() ([]DiscordSourceState, error) {
	var rows []DiscordSourceState
	err := s.db.Order("channel_id").Find(&rows).Error
	return rows, err
}
func (s *DiscordMessageStore) RecentInbound(channelIDs []string, since time.Time) ([]DiscordInbound, error) {
	var rows []DiscordInbound
	err := s.db.Where("channel_id IN ? AND received_at >= ? AND baseline = ?", channelIDs, since.UTC(), false).Order("id DESC").Limit(1000).Find(&rows).Error
	return rows, err
}
func (s *DiscordMessageStore) DeliveryMessage(traderID, messageID string, revision int) (*DiscordMessage, error) {
	var events []DiscordInbound
	if err := s.db.Where("message_id = ? AND revision = ?", messageID, revision).Order("id DESC").Find(&events).Error; err != nil {
		return nil, err
	}
	for _, ev := range events {
		var d DiscordDelivery
		err := s.db.Where("event_id = ? AND trader_id = ?", ev.EventID, traderID).First(&d).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		var msg DiscordMessage
		if err = json.Unmarshal([]byte(d.MessageJSON), &msg); err != nil {
			return nil, err
		}
		msg.DeliveryID = d.ID
		msg.SourceEventID = d.EventID
		msg.RulesSnapshot = d.RulesJSON
		msg.ExecutionKey = d.ExecutionKey
		return &msg, nil
	}
	return nil, nil
}

func (s *DiscordMessageStore) RevokeRoute(traderID string) error {
	return s.db.Model(&DiscordRoute{}).Where("trader_id = ?", traderID).Updates(map[string]interface{}{"generation": gorm.Expr("generation + 1"), "needs_activation": true}).Error
}
func (s *DiscordMessageStore) ResetTraderDeliveries(traderID string) error {
	return s.db.Model(&DiscordDelivery{}).Where("trader_id = ? AND status = ?", traderID, DiscordMsgProcessing).Update("status", DiscordMsgPending).Error
}

// SourceConfigSnapshot binds events captured during backend outages to the
// exact routing and interpretation rules acknowledged by that collector.
type DiscordSourceConfig struct {
	Version    string `gorm:"primaryKey"`
	RoutesJSON string
	CreatedAt  time.Time
}

func (s *DiscordMessageStore) SaveSourceConfig(version, routesJSON string) error {
	return s.db.Clauses(clause.OnConflict{DoNothing: true}).Create(&DiscordSourceConfig{Version: version, RoutesJSON: routesJSON}).Error
}
func (s *DiscordMessageStore) SourceConfig(version string) (string, error) {
	var row DiscordSourceConfig
	err := s.db.First(&row, "version = ?", version).Error
	return row.RoutesJSON, err
}

// Only remove acknowledged audit receipts after their signal retention expires.
// Active trades, waiting signals and pending deliveries always retain evidence.
func (s *DiscordMessageStore) CleanOldIngest(days int) (int64, error) {
	if days < 30 {
		days = 30
	}
	cutoff := time.Now().UTC().AddDate(0, 0, -days)
	var removed int64
	err := s.db.Transaction(func(tx *gorm.DB) error {
		protected := tx.Model(&CopyTradeContext{}).Select("root_message_id").Where("id IN (?)", protectedContexts(tx))
		signals := tx.Model(&CopyTradeSignal{}).Select("message_id")
		events := tx.Model(&DiscordInbound{}).Select("event_id").Where("created_at < ?", cutoff).Where("message_id NOT IN (?) AND message_id NOT IN (?)", protected, signals)
		if err := tx.Where("event_id IN (?) AND status = ?", events, DiscordMsgDone).Delete(&DiscordDelivery{}).Error; err != nil {
			return err
		}
		pending := tx.Model(&DiscordDelivery{}).Select("event_id")
		result := tx.Where("created_at < ? AND event_id NOT IN (?) AND message_id NOT IN (?) AND message_id NOT IN (?)", cutoff, pending, protected, signals).Delete(&DiscordInbound{})
		removed = result.RowsAffected
		return result.Error
	})
	return removed, err
}
