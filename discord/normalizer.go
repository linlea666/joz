package discord

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"nofx/store"
)

// messageLinkPattern matches Discord message URLs embedded in content
// (jonzi alert posts link back to the trade card message).
var messageLinkPattern = regexp.MustCompile(`discord(?:app)?\.com/channels/(\d+)/(\d+)/(\d+)`)

// MessageLink is a parsed Discord message URL.
type MessageLink struct {
	GuildID   string
	ChannelID string
	MessageID string
}

// ExtractMessageLinks finds Discord message URLs inside text.
func ExtractMessageLinks(content string) []MessageLink {
	matches := messageLinkPattern.FindAllStringSubmatch(content, -1)
	links := make([]MessageLink, 0, len(matches))
	seen := map[string]bool{}
	for _, m := range matches {
		key := m[2] + "/" + m[3]
		if seen[key] {
			continue
		}
		seen[key] = true
		links = append(links, MessageLink{GuildID: m[1], ChannelID: m[2], MessageID: m[3]})
	}
	return links
}

// FlattenEmbeds renders rich embeds as readable text for the LLM prompt
// (nurse-neil signals carry Entry / Stop Loss in embed fields).
func FlattenEmbeds(embeds []Embed) string {
	if len(embeds) == 0 {
		return ""
	}
	var b strings.Builder
	for i, e := range embeds {
		if i > 0 {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "[Embed %d]", i+1)
		if e.Title != "" {
			fmt.Fprintf(&b, " Title: %s", e.Title)
		}
		b.WriteString("\n")
		if e.Description != "" {
			fmt.Fprintf(&b, "%s\n", e.Description)
		}
		for _, f := range e.Fields {
			fmt.Fprintf(&b, "- %s: %s\n", f.Name, f.Value)
		}
		if e.URL != "" {
			fmt.Fprintf(&b, "URL: %s\n", e.URL)
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// ImageURLs collects downloadable image URLs from attachments and embeds.
func ImageURLs(msg *Message) []string {
	var urls []string
	for _, a := range msg.Attachments {
		if a.IsImage() && a.URL != "" {
			urls = append(urls, a.URL)
		}
	}
	for _, e := range msg.Embeds {
		if e.Image != nil && e.Image.URL != "" {
			urls = append(urls, e.Image.URL)
		}
	}
	return urls
}

// ToStoreMessage converts an API message into the durable store row.
func ToStoreMessage(msg *Message, channelID string) (*store.DiscordMessage, error) {
	embedsJSON := ""
	if len(msg.Embeds) > 0 {
		b, err := json.Marshal(msg.Embeds)
		if err != nil {
			return nil, fmt.Errorf("marshal embeds: %w", err)
		}
		embedsJSON = string(b)
	}
	attachmentsJSON := ""
	if len(msg.Attachments) > 0 {
		b, err := json.Marshal(msg.Attachments)
		if err != nil {
			return nil, fmt.Errorf("marshal attachments: %w", err)
		}
		attachmentsJSON = string(b)
	}
	raw, _ := json.Marshal(msg)

	replyTo := ""
	if msg.MessageReference != nil {
		replyTo = msg.MessageReference.MessageID
	}

	authorName := msg.Author.GlobalName
	if authorName == "" {
		authorName = msg.Author.Username
	}

	rec := &store.DiscordMessage{
		ChannelID:        channelID,
		MessageID:        msg.ID,
		GuildID:          msg.GuildID,
		AuthorID:         msg.Author.ID,
		AuthorName:       authorName,
		Content:          msg.Content,
		EmbedsJSON:       embedsJSON,
		AttachmentsJSON:  attachmentsJSON,
		ReplyToMessageID: replyTo,
		MessageTimestamp: msg.Timestamp.UTC(),
		EditedAt:         msg.EditedTimestamp,
		ReceivedAt:       time.Now().UTC(),
		RawPayload:       string(raw),
	}
	rec.ContentHash = store.HashDiscordContent(rec.Content, rec.EmbedsJSON)
	return rec, nil
}

// ParseStoredEmbeds decodes the embeds JSON of a stored message.
func ParseStoredEmbeds(embedsJSON string) []Embed {
	if strings.TrimSpace(embedsJSON) == "" {
		return nil
	}
	var embeds []Embed
	if err := json.Unmarshal([]byte(embedsJSON), &embeds); err != nil {
		return nil
	}
	return embeds
}

// ParseStoredAttachments decodes the attachments JSON of a stored message.
func ParseStoredAttachments(attachmentsJSON string) []Attachment {
	if strings.TrimSpace(attachmentsJSON) == "" {
		return nil
	}
	var atts []Attachment
	if err := json.Unmarshal([]byte(attachmentsJSON), &atts); err != nil {
		return nil
	}
	return atts
}
