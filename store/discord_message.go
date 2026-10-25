package store

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"gorm.io/gorm"
)

// Discord message processing statuses stored on raw messages. Delivery to the
// copy-trading engine is driven by discord_deliveries (see discord_ingest.go);
// these statuses mark baseline/terminal rows for retention and display.
const (
	DiscordMsgPending    = "pending"
	DiscordMsgProcessing = "processing"
	DiscordMsgDone       = "done"
	DiscordMsgFailed     = "failed"
	DiscordMsgSkipped    = "skipped"
)

// DiscordMessage is one raw Discord message (durable, before any AI work).
// (channel_id, message_id) is unique; edits update the row, bump Revision and
// reset ProcessingStatus so lifecycle changes carried by edits are reprocessed
// (jonzi-style trade cards mutate in place: opened -> SL moved -> closed).
type DiscordMessage struct {
	DeliveryID       int64      `gorm:"-" json:"delivery_id,omitempty"`
	SourceEventID    string     `gorm:"-" json:"source_event_id,omitempty"`
	RulesSnapshot    string     `gorm:"-" json:"rules_snapshot,omitempty"`
	ExecutionKey     string     `gorm:"-" json:"-"`
	ID               int64      `gorm:"primaryKey;autoIncrement" json:"id"`
	ChannelID        string     `gorm:"column:channel_id;not null;uniqueIndex:idx_discord_msg_channel_message,priority:1;index:idx_discord_msg_channel_time,priority:1" json:"channel_id"`
	MessageID        string     `gorm:"column:message_id;not null;uniqueIndex:idx_discord_msg_channel_message,priority:2" json:"message_id"`
	GuildID          string     `gorm:"column:guild_id;default:''" json:"guild_id"`
	AuthorID         string     `gorm:"column:author_id;default:''" json:"author_id"`
	AuthorName       string     `gorm:"column:author_name;default:''" json:"author_name"`
	Content          string     `gorm:"column:content;default:''" json:"content"`
	EmbedsJSON       string     `gorm:"column:embeds_json;default:''" json:"embeds_json"`
	AttachmentsJSON  string     `gorm:"column:attachments_json;default:''" json:"attachments_json"`
	ReplyToMessageID string     `gorm:"column:reply_to_message_id;default:''" json:"reply_to_message_id"`
	MessageTimestamp time.Time  `gorm:"column:message_timestamp;index:idx_discord_msg_channel_time,priority:2,sort:desc" json:"message_timestamp"`
	EditedAt         *time.Time `gorm:"column:edited_at" json:"edited_at,omitempty"`
	ReceivedAt       time.Time  `gorm:"column:received_at" json:"received_at"`
	ContentHash      string     `gorm:"column:content_hash;default:''" json:"content_hash"`
	RawPayload       string     `gorm:"column:raw_payload;default:''" json:"-"`
	Revision         int        `gorm:"column:revision;default:0" json:"revision"`
	ProcessingStatus string     `gorm:"column:processing_status;default:'pending';index" json:"processing_status"`
	ProcessingError  string     `gorm:"column:processing_error;default:''" json:"processing_error"`
	CreatedAt        time.Time  `json:"created_at"`
	UpdatedAt        time.Time  `json:"updated_at"`
}

func (DiscordMessage) TableName() string { return "discord_messages" }

// DiscordChannelBaseline is the per-channel baseline flag of the retired REST
// poller. Gateway ingest no longer reads or writes it; the model stays in
// AutoMigrate only so existing databases keep a consistent schema.
type DiscordChannelBaseline struct {
	ChannelID    string    `gorm:"column:channel_id;primaryKey" json:"channel_id"`
	MessageCount int       `gorm:"column:message_count;default:0" json:"message_count"`
	CompletedAt  time.Time `gorm:"column:completed_at" json:"completed_at"`
}

func (DiscordChannelBaseline) TableName() string { return "discord_channel_baselines" }

// HashDiscordContent produces the content hash used for edit detection
// (content + embeds; attachments rarely change on edit).
func HashDiscordContent(content, embedsJSON string) string {
	h := sha256.Sum256([]byte(content + "\x00" + embedsJSON))
	return hex.EncodeToString(h[:])
}

// DiscordMessageStore persists raw Discord messages.
type DiscordMessageStore struct {
	db *gorm.DB
}

// NewDiscordMessageStore creates a new DiscordMessageStore.
func NewDiscordMessageStore(db *gorm.DB) *DiscordMessageStore {
	return &DiscordMessageStore{db: db}
}

func (s *DiscordMessageStore) initTables() error {
	return s.db.AutoMigrate(&DiscordMessage{}, &DiscordChannelBaseline{}, &DiscordInbound{}, &DiscordDelivery{}, &DiscordRoute{}, &DiscordSourceState{}, &DiscordSourceConfig{}, &ChannelNotification{})
}

// UpsertResult describes what the upsert observed.
type UpsertResult int

const (
	DiscordMsgUnchanged UpsertResult = iota
	DiscordMsgNew
	DiscordMsgEdited
)

// Upsert inserts a new message or detects an edit of an existing one.
// Edits bump Revision and reset ProcessingStatus to pending so the engine
// reprocesses the new content.
func (s *DiscordMessageStore) Upsert(msg *DiscordMessage) (UpsertResult, error) {
	if msg.ContentHash == "" {
		msg.ContentHash = HashDiscordContent(msg.Content, msg.EmbedsJSON)
	}
	if msg.ReceivedAt.IsZero() {
		msg.ReceivedAt = time.Now().UTC()
	}

	var existing DiscordMessage
	err := s.db.Where("channel_id = ? AND message_id = ?", msg.ChannelID, msg.MessageID).
		First(&existing).Error
	if err != nil {
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return DiscordMsgUnchanged, err
		}
		msg.ProcessingStatus = DiscordMsgPending
		if createErr := s.db.Create(msg).Error; createErr != nil {
			return DiscordMsgUnchanged, fmt.Errorf("failed to insert discord message: %w", createErr)
		}
		return DiscordMsgNew, nil
	}

	// Compare semantic payloads as well as hashes: enriching old rows with
	// author/reference metadata or rotating signed media URLs is not a new signal.
	if existing.Content == msg.Content && semanticDiscordJSON(existing.EmbedsJSON) == semanticDiscordJSON(msg.EmbedsJSON) && semanticDiscordJSON(existing.AttachmentsJSON) == semanticDiscordJSON(msg.AttachmentsJSON) {
		updates := map[string]interface{}{"embeds_json": msg.EmbedsJSON, "attachments_json": msg.AttachmentsJSON, "reply_to_message_id": msg.ReplyToMessageID, "raw_payload": msg.RawPayload, "content_hash": msg.ContentHash}
		err := s.db.Model(&DiscordMessage{}).Where("id = ?", existing.ID).Updates(updates).Error
		msg.ID, msg.Revision, msg.ProcessingStatus = existing.ID, existing.Revision, existing.ProcessingStatus
		return DiscordMsgUnchanged, err
	}

	updates := map[string]interface{}{
		"reply_to_message_id": msg.ReplyToMessageID,
		"content":             msg.Content,
		"embeds_json":         msg.EmbedsJSON,
		"attachments_json":    msg.AttachmentsJSON,
		"edited_at":           msg.EditedAt,
		"content_hash":        msg.ContentHash,
		"raw_payload":         msg.RawPayload,
		"revision":            existing.Revision + 1,
		"processing_status":   DiscordMsgPending,
		"processing_error":    "",
		"received_at":         msg.ReceivedAt,
	}
	if err := s.db.Model(&DiscordMessage{}).Where("id = ?", existing.ID).Updates(updates).Error; err != nil {
		return DiscordMsgUnchanged, fmt.Errorf("failed to update edited discord message: %w", err)
	}
	msg.ID = existing.ID
	msg.Revision = existing.Revision + 1
	return DiscordMsgEdited, nil
}

// GetByMessageID fetches one message.
func (s *DiscordMessageStore) GetByMessageID(channelID, messageID string) (*DiscordMessage, error) {
	var msg DiscordMessage
	err := s.db.Where("channel_id = ? AND message_id = ?", channelID, messageID).First(&msg).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return &msg, nil
}

// LatestMessageTime returns the newest known message timestamp of a channel
// (zero time when the channel has no stored messages yet).
func (s *DiscordMessageStore) LatestMessageTime(channelID string) (time.Time, error) {
	var msg DiscordMessage
	err := s.db.Where("channel_id = ?", channelID).
		Order("message_timestamp DESC").
		First(&msg).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return time.Time{}, nil
		}
		return time.Time{}, err
	}
	return msg.MessageTimestamp, nil
}

// MarkBaseline stores messages as already-processed (skipped) — used on first
// subscription so pre-existing channel history is never traded on.
func (s *DiscordMessageStore) MarkBaseline(msg *DiscordMessage) error {
	if msg.ContentHash == "" {
		msg.ContentHash = HashDiscordContent(msg.Content, msg.EmbedsJSON)
	}
	if msg.ReceivedAt.IsZero() {
		msg.ReceivedAt = time.Now().UTC()
	}
	msg.ProcessingStatus = DiscordMsgSkipped
	msg.ProcessingError = "baseline (before subscription)"
	err := s.db.Create(msg).Error
	if err != nil && (errors.Is(err, gorm.ErrDuplicatedKey) || isUniqueViolation(err)) {
		return nil
	}
	return err
}

// GetRecentByChannel returns recent messages for context building.
func (s *DiscordMessageStore) GetRecentByChannel(channelID string, since time.Time, limit int) ([]*DiscordMessage, error) {
	var msgs []*DiscordMessage
	q := s.db.Where("channel_id = ?", channelID)
	if !since.IsZero() {
		q = q.Where("message_timestamp >= ?", since)
	}
	err := q.Order("message_timestamp DESC").Limit(limit).Find(&msgs).Error
	return msgs, err
}

// CleanOldMessages removes terminal messages (done/skipped/failed) older than
// the retention window. Pending/processing rows are never removed; replay is
// driven by the ingest/delivery tables, so deleting old skipped baseline rows
// cannot cause a historical replay.
func (s *DiscordMessageStore) CleanOldMessages(days int) (int64, error) {
	cutoff := time.Now().AddDate(0, 0, -days)
	result := s.db.Where("message_timestamp < ? AND processing_status IN ?", cutoff,
		[]string{DiscordMsgDone, DiscordMsgSkipped, DiscordMsgFailed}).
		Where("message_id NOT IN (?)", s.db.Model(&CopyTradeContext{}).Select("root_message_id").Where("id IN (?)", protectedContexts(s.db))).
		Where("message_id NOT IN (?)", s.db.Model(&CopyTradeSignal{}).Select("message_id")).
		Delete(&DiscordMessage{})
	return result.RowsAffected, result.Error
}

func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "UNIQUE constraint failed") || strings.Contains(msg, "duplicate key value")
}

// Only trading text and media identity cause revisions. Provenance is refreshed
// in place, including on baseline messages which must never be replayed.
func semanticDiscordJSON(raw string) string {
	var value interface{}
	if raw == "" || raw == "null" {
		return "[]"
	}
	if json.Unmarshal([]byte(raw), &value) != nil {
		return raw
	}
	var clean func(interface{}) interface{}
	clean = func(v interface{}) interface{} {
		switch item := v.(type) {
		case map[string]interface{}:
			result := map[string]interface{}{}
			for k, child := range item {
				switch k {
				case "author", "timestamp", "proxy_url", "proxy_icon_url", "height", "width", "content_type", "size":
					continue
				}
				if k == "url" {
					if str, ok := child.(string); ok {
						if u, err := url.Parse(str); err == nil && (strings.HasSuffix(u.Host, "discordapp.com") || strings.HasSuffix(u.Host, "discordapp.net")) {
							u.RawQuery = ""
							child = u.String()
						}
					}
				}
				result[k] = clean(child)
			}
			return result
		case []interface{}:
			for i := range item {
				item[i] = clean(item[i])
			}
			return item
		default:
			return v
		}
	}
	data, _ := json.Marshal(clean(value))
	return string(data)
}

func (s *DiscordMessageStore) GetRecentByChannels(channels []string, since time.Time, limit int) ([]*DiscordMessage, error) {
	var rows []*DiscordMessage
	q := s.db.Where("channel_id IN ?", channels)
	if !since.IsZero() {
		q = q.Where("message_timestamp >= ?", since)
	}
	err := q.Order("message_timestamp DESC").Limit(limit).Find(&rows).Error
	return rows, err
}
