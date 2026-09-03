// Package discord implements the copy-trading signal source: a rate-limited
// REST client for the Discord API and a polling manager that persists raw
// messages (including edits) into the durable store queue.
//
// Access uses a personal account token over REST polling by explicit product
// decision (the followed channels cannot invite bots). The client is designed
// to be maximally conservative: one global serial request queue, strict
// rate-limit header compliance, exponential backoff on 429 and jittered
// polling intervals.
package discord

import "time"

// User is the subset of the Discord user object we need.
type User struct {
	ID         string `json:"id"`
	Username   string `json:"username"`
	GlobalName string `json:"global_name"`
	Bot        bool   `json:"bot"`
}

// Attachment is a message attachment (usually a chart screenshot).
type Attachment struct {
	ID          string `json:"id"`
	Filename    string `json:"filename"`
	Size        int64  `json:"size"`
	URL         string `json:"url"`
	ProxyURL    string `json:"proxy_url"`
	ContentType string `json:"content_type"`
	Width       int    `json:"width"`
	Height      int    `json:"height"`
}

// EmbedField is a key/value field inside a rich embed
// (nurse-neil style signals put Entry / Stop Loss here).
type EmbedField struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	Inline bool   `json:"inline"`
}

// EmbedMedia is an embed image/thumbnail.
type EmbedMedia struct {
	URL         string `json:"url"`
	ProxyURL    string `json:"proxy_url"`
	ContentType string `json:"content_type"`
	Width       int    `json:"width"`
	Height      int    `json:"height"`
}

// Embed is a Discord rich embed.
type Embed struct {
	Type        string       `json:"type"`
	Title       string       `json:"title"`
	Description string       `json:"description"`
	URL         string       `json:"url"`
	Timestamp   string       `json:"timestamp"`
	Fields      []EmbedField `json:"fields"`
	Image       *EmbedMedia  `json:"image"`
	Thumbnail   *EmbedMedia  `json:"thumbnail"`
	Footer      *struct {
		Text string `json:"text"`
	} `json:"footer"`
}

// MessageReference marks a reply relationship.
type MessageReference struct {
	MessageID string `json:"message_id"`
	ChannelID string `json:"channel_id"`
	GuildID   string `json:"guild_id"`
}

// Message is the subset of the Discord message object we consume.
type Message struct {
	ID               string            `json:"id"`
	ChannelID        string            `json:"channel_id"`
	GuildID          string            `json:"guild_id"`
	Author           User              `json:"author"`
	Content          string            `json:"content"`
	Timestamp        time.Time         `json:"timestamp"`
	EditedTimestamp  *time.Time        `json:"edited_timestamp"`
	Attachments      []Attachment      `json:"attachments"`
	Embeds           []Embed           `json:"embeds"`
	MessageReference *MessageReference `json:"message_reference"`
}

// IsImage reports whether the attachment is an image we can send to a vision model.
func (a *Attachment) IsImage() bool {
	switch a.ContentType {
	case "image/png", "image/jpeg", "image/jpg", "image/webp", "image/gif":
		return true
	}
	return false
}
