package copytrader

import (
	"fmt"
	"strings"
	"time"

	"nofx/discord"
	"nofx/mcp"
)

// ProfileGeneratePrompt is a dedicated drafting prompt. It is NOT the
// trade-interpretation contract and must never be mixed with SystemPrompt.
const ProfileGeneratePrompt = `You write a "channel profile" for a crypto copy-trading interpreter.

The profile is injected as background knowledge. It must NOT contain trading JSON, position sizes, or execution rules. Write operator notes the interpreter can use to recognise THIS author's style.

Cover only what the samples support:
- How they post entries (cards, screenshots, text, edits)
- Typical symbols / markets
- How they mark close / SL move / partial TP / cancel
- Slang or emoji that means a trade action
- Recurring noise to IGNORE (mentorship, live voice, recaps, memes)
- Whether numbers live in text, embeds, or images

Output PLAIN TEXT in the same language as the samples (Chinese if mixed). 8-20 short lines. No markdown fences. No JSON.`

// GenerateProfile drafts a channel profile from stored history. Dry of
// persistence: the caller decides whether to save it as channel_notes.
func (e *Engine) GenerateProfile(limit int) (string, error) {
	if limit <= 0 {
		limit = 40
	}
	if limit > 80 {
		limit = 80
	}
	msgs, err := e.st.DiscordMessage().GetRecentByChannel(e.cfg.PrimaryChannelID, time.Time{}, limit)
	if err != nil {
		return "", fmt.Errorf("failed to load channel history: %w", err)
	}
	if len(msgs) == 0 {
		return "", fmt.Errorf("no stored messages for channel %s — wait for the poller to baseline, then retry", e.cfg.PrimaryChannelID)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Draft a channel profile from these %d recent messages (newest first). Existing operator notes (may be empty):\n%s\n\n--- samples ---\n",
		len(msgs), strings.TrimSpace(e.cfg.ChannelNotes))
	for _, m := range msgs {
		content := strings.TrimSpace(m.Content)
		embeds := discord.FlattenEmbeds(discord.ParseStoredEmbeds(m.EmbedsJSON))
		if content == "" && embeds == "" && m.AttachmentsJSON == "" {
			continue
		}
		imgN := 0
		for _, att := range discord.ParseStoredAttachments(m.AttachmentsJSON) {
			if att.IsImage() {
				imgN++
			}
		}
		fmt.Fprintf(&b, "[%s %s rev=%d images=%d]\n",
			m.MessageTimestamp.UTC().Format("01-02 15:04"), m.AuthorName, m.Revision, imgN)
		if content != "" {
			b.WriteString(truncateText(content, 400))
			b.WriteString("\n")
		}
		if embeds != "" {
			b.WriteString(truncateText(embeds, 300))
			b.WriteString("\n")
		}
	}
	b.WriteString("--- end samples ---\nWrite the profile now.")

	raw, err := e.llm.CallWithRequest(&mcp.Request{
		Messages: []mcp.Message{
			mcp.NewSystemMessage(ProfileGeneratePrompt),
			mcp.NewUserMessage(b.String()),
		},
	})
	if err != nil {
		return "", fmt.Errorf("profile generation LLM failed: %w", err)
	}
	draft := strings.TrimSpace(raw)
	draft = strings.TrimPrefix(draft, "```")
	draft = strings.TrimSuffix(draft, "```")
	draft = strings.TrimSpace(draft)
	if draft == "" {
		return "", fmt.Errorf("LLM returned an empty profile")
	}
	return draft, nil
}
