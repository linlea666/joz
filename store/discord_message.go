package store

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"gorm.io/gorm"
)

// Discord message processing statuses. The raw-message table doubles as a
// durable processing queue: the poller only appends, the copy-trading engine
// consumes. Process restarts and AI timeouts can always be replayed safely.
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
	return s.db.AutoMigrate(&DiscordMessage{})
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

	if existing.ContentHash == msg.ContentHash {
		return DiscordMsgUnchanged, nil
	}

	updates := map[string]interface{}{
		"content":           msg.Content,
		"embeds_json":       msg.EmbedsJSON,
		"attachments_json":  msg.AttachmentsJSON,
		"edited_at":         msg.EditedAt,
		"content_hash":      msg.ContentHash,
		"raw_payload":       msg.RawPayload,
		"revision":          existing.Revision + 1,
		"processing_status": DiscordMsgPending,
		"processing_error":  "",
		"received_at":       msg.ReceivedAt,
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

// NextPending claims the oldest pending message of a channel for processing
// (single-consumer per channel; the engine serializes per channel by design).
func (s *DiscordMessageStore) NextPending(channelID string) (*DiscordMessage, error) {
	var msg DiscordMessage
	err := s.db.Where("channel_id = ? AND processing_status = ?", channelID, DiscordMsgPending).
		Order("message_timestamp ASC").
		First(&msg).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}
	if err := s.SetStatus(msg.ID, DiscordMsgProcessing, ""); err != nil {
		return nil, err
	}
	return &msg, nil
}

// SetStatus updates the processing status.
func (s *DiscordMessageStore) SetStatus(id int64, status, processingError string) error {
	return s.db.Model(&DiscordMessage{}).Where("id = ?", id).Updates(map[string]interface{}{
		"processing_status": status,
		"processing_error":  processingError,
	}).Error
}

// ResetStuckProcessing returns messages stuck in "processing" (e.g. process
// crash mid-AI-call) back to pending. Called on startup.
func (s *DiscordMessageStore) ResetStuckProcessing() (int64, error) {
	result := s.db.Model(&DiscordMessage{}).
		Where("processing_status = ?", DiscordMsgProcessing).
		Updates(map[string]interface{}{"processing_status": DiscordMsgPending})
	return result.RowsAffected, result.Error
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

// HasMessages reports whether any message is stored for the channel
// (used to establish the "no historical replay" baseline on first subscribe).
func (s *DiscordMessageStore) HasMessages(channelID string) (bool, error) {
	var count int64
	err := s.db.Model(&DiscordMessage{}).Where("channel_id = ?", channelID).Limit(1).Count(&count).Error
	return count > 0, err
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

func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "UNIQUE constraint failed") || strings.Contains(msg, "duplicate key value")
}
