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

// DiscordChannelBaseline marks that a channel's pre-subscription history has
// been fully persisted as baseline. The poller must ONLY trust this flag when
// deciding between "establish baseline" and "incremental fetch" — presence of
// stored rows is NOT sufficient (cross-channel message lookups can write
// single rows into a channel that was never actually baselined, which would
// otherwise cause the whole history window to be traded as fresh signals).
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
	return s.db.AutoMigrate(&DiscordMessage{}, &DiscordChannelBaseline{})
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
// The claim is revision-conditional: if the author edits the message between
// read and claim, the claim fails and the caller re-reads the fresh revision.
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
	claimed, err := s.SetStatusIfRevision(msg.ID, msg.Revision, DiscordMsgProcessing, "")
	if err != nil {
		return nil, err
	}
	if !claimed {
		// Edited between read and claim: not an error, just nothing claimed
		// this round; the dispatcher loops and picks up the new revision.
		return nil, nil
	}
	return &msg, nil
}

// SetStatus updates the processing status unconditionally.
func (s *DiscordMessageStore) SetStatus(id int64, status, processingError string) error {
	return s.db.Model(&DiscordMessage{}).Where("id = ?", id).Updates(map[string]interface{}{
		"processing_status": status,
		"processing_error":  processingError,
	}).Error
}

// SetStatusIfRevision updates the processing status only when the stored
// revision still matches. Returns false (without error) when the row changed:
// an edit during processing resets the status to pending, and that pending
// must survive so the new revision is reprocessed (lifecycle updates like
// "SL moved" or "closed" arrive as in-place edits).
func (s *DiscordMessageStore) SetStatusIfRevision(id int64, revision int, status, processingError string) (bool, error) {
	result := s.db.Model(&DiscordMessage{}).
		Where("id = ? AND revision = ?", id, revision).
		Updates(map[string]interface{}{
			"processing_status": status,
			"processing_error":  processingError,
		})
	if result.Error != nil {
		return false, result.Error
	}
	return result.RowsAffected > 0, nil
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

// IsBaselineDone reports whether the channel's baseline has been fully
// established (the only trusted signal for the poller's baseline decision).
func (s *DiscordMessageStore) IsBaselineDone(channelID string) (bool, error) {
	var n int64
	err := s.db.Model(&DiscordChannelBaseline{}).Where("channel_id = ?", channelID).Count(&n).Error
	return n > 0, err
}

// MarkBaselineDone records that the channel's history window was fully
// persisted as baseline. Idempotent.
func (s *DiscordMessageStore) MarkBaselineDone(channelID string, messageCount int) error {
	rec := &DiscordChannelBaseline{
		ChannelID:    channelID,
		MessageCount: messageCount,
		CompletedAt:  time.Now().UTC(),
	}
	err := s.db.Create(rec).Error
	if err != nil && (errors.Is(err, gorm.ErrDuplicatedKey) || isUniqueViolation(err)) {
		return nil
	}
	return err
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
// the retention window. Pending/processing rows are never removed; channel
// baselines are tracked in discord_channel_baselines, so deleting old skipped
// baseline rows cannot cause a historical replay.
func (s *DiscordMessageStore) CleanOldMessages(days int) (int64, error) {
	cutoff := time.Now().AddDate(0, 0, -days)
	result := s.db.Where("message_timestamp < ? AND processing_status IN ?", cutoff,
		[]string{DiscordMsgDone, DiscordMsgSkipped, DiscordMsgFailed}).
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
