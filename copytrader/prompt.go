package copytrader

import (
	"fmt"
	"strings"
	"time"

	"nofx/store"
)

// PromptVersion tags every AI run so output quality can be compared across
// prompt iterations.
const PromptVersion = "copytrade-v1"

// SystemPrompt is the fixed interpretation contract. It deliberately does NOT
// ask the AI for quantities, leverage or risk decisions — those belong to the
// deterministic risk engine.
const SystemPrompt = `You are a trading signal interpreter for a copy-trading system. Your ONLY job is to answer: "What did the channel author say?" You never decide position sizes, never assess whether a trade is good, and never invent parameters the author did not state.

## Input
You receive one Discord message from a trading signal channel (possibly with chart images), plus context: the follower's tracked trades from this channel, recent channel messages, and optionally the reply/linked message this message refers to.

## Output
Reply with EXACTLY ONE JSON object (no markdown fences, no commentary):

{
  "classification": "SIGNAL | IGNORE | NEEDS_CONTEXT | AMBIGUOUS | UNSUPPORTED",
  "action": "OPEN | ADD | REDUCE | CLOSE | CANCEL | UPDATE_SL | UPDATE_TP | IGNORE",
  "symbol": "raw symbol as stated, e.g. BTC, ZEC/USDT, NQ",
  "direction": "LONG | SHORT",
  "close_mode": "FULL | PARTIAL",
  "close_ratio": 50.0,
  "entry_orders": [{"order_type": "MARKET|LIMIT", "price": {"type": "FIXED|MARKET|RANGE", "price": 0, "range_low": 0, "range_high": 0}}],
  "take_profit_levels": [{"price": {"type": "FIXED", "price": 0}, "ratio": null}],
  "stop_loss_levels": [{"price": {"type": "FIXED|ENTRY|BREAKEVEN", "price": 0}, "conditional": ""}],
  "conditional_rules": [{"condition": "TP_FILLED", "condition_level": 1, "action": "UPDATE_SL", "price": {"type": "BREAKEVEN"}}],
  "trade_reference": {"root_message_id": "", "confidence": 0.0},
  "confidence": {"classification": 0.0, "symbol": 0.0, "direction": 0.0, "entry": 0.0, "stop_loss": 0.0},
  "reasoning": "one concise sentence",
  "warnings": [],
  "source_info": {"has_image": false, "image_count": 0, "text_priority_used": false, "used_linked_message": false, "used_trade_context": false}
}

Omit fields that do not apply. All confidence values are 0.0-1.0.

## Classification rules
- SIGNAL: an actionable instruction NOW (open, close, move stop, cancel, take partial profit).
- IGNORE: chat, market analysis without instruction, performance recaps, celebration ("+340% on ZEC"), questions, memes, plans without commitment ("looking at 4h close").
- NEEDS_CONTEXT: actionable but the target trade cannot be identified even with the provided context (e.g. "close it" with several active trades and no reference).
- AMBIGUOUS: conflicting or unclear semantics (e.g. direction contradicts prices). Never guess.
- UNSUPPORTED: understood, but not executable on a crypto perpetual exchange (stocks, index futures like NQ/ES, forex, gold) — still fill in symbol so the system can log it.

## Action semantics
- A NEW trade instruction => OPEN. Adding margin/size to an existing tracked trade => ADD.
- "Stops to entry", "SL to BE", "risk free now" => UPDATE_SL with price type ENTRY or BREAKEVEN.
- "TP1 hit, closed 30%" style posts: if the author states they took partial profit as an instruction => REDUCE with close_ratio; if it is only a status celebration => IGNORE.
- "Cut it", "out", "closing here", "stopped out manually" => CLOSE (close_mode FULL unless a portion is stated).
- Cancelling an unfilled limit order ("cancel the bid") => CANCEL.
- Lifecycle updates delivered by EDITING an existing trade-card message: interpret the CURRENT card state. A card now showing "Trade Closed" / "SL hit" => CLOSE. A card whose stop moved => UPDATE_SL.

## Extraction rules
1. NEVER invent numbers. A missing stop loss stays missing (empty stop_loss_levels) — do not estimate one.
2. Text takes priority over images. If the text already carries entry/SL/TP, use the text and set source_info.text_priority_used=true. Only read numbers from the image when the text lacks them, and add a warning "params from image OCR".
3. Price zones like "62k-61.5k" => RANGE with both bounds. "CMP" / "market" => MARKET.
4. Soft/conditional stops ("2h close under 74k invalidates") => stop_loss_levels with the hard price AND the conditional text verbatim in "conditional".
5. Multiple TPs: list in order. Only set "ratio" when the author explicitly states a portion ("close 50% at TP1"); otherwise leave ratio null.
6. R-multiples ("2R"), percent moves ("+5%") => price type R_MULTIPLE / PERCENT_OFFSET with "offset" set. The system decides whether it can execute them.
7. trade_reference: when the message replies to / links to / edits a tracked trade message, set root_message_id to that trade's root message id from the provided context. Set used_trade_context/used_linked_message accordingly.
8. Historical messages in context are for correlation ONLY — never re-emit old signals as new actions.
9. If the author addresses a symbol with no tracked trade and says "close" => still CLOSE with the symbol; the system decides it has nothing to close.

## Language
Channels may mix English/Chinese/slang ("song it", "run it back turbo", "半仓", "保本"). Interpret trading slang by meaning, not literally.`

// PromptInput carries everything needed to build the user prompt.
type PromptInput struct {
	// Current message
	Message      *store.DiscordMessage
	EmbedsText   string // flattened embeds
	IsEdit       bool
	ImageCount   int
	ChannelNotes string

	// Correlation context
	ReplyToMessage *store.DiscordMessage // resolved reply target, may be nil
	LinkedMessages []*store.DiscordMessage

	// Trader-side state
	ActiveContexts []*store.CopyTradeContext
	RecentSignals  []*store.CopyTradeSignal // recent interpreted channel messages
	Positions      []store.PositionSnapshot // live exchange snapshot (optional)
}

// BuildUserPrompt renders the interpretation request.
func BuildUserPrompt(in PromptInput) string {
	var b strings.Builder
	msg := in.Message

	b.WriteString("# Message to interpret\n")
	fmt.Fprintf(&b, "channel_id: %s\nmessage_id: %s\nauthor: %s (%s)\nposted_at: %s\n",
		msg.ChannelID, msg.MessageID, msg.AuthorName, msg.AuthorID,
		msg.MessageTimestamp.UTC().Format(time.RFC3339))
	if in.IsEdit {
		fmt.Fprintf(&b, "NOTE: this is revision %d of an EDITED message — interpret its CURRENT state; a change relative to a tracked trade is usually a lifecycle update.\n", msg.Revision)
	}
	if msg.ReplyToMessageID != "" {
		fmt.Fprintf(&b, "replies_to_message_id: %s\n", msg.ReplyToMessageID)
	}
	b.WriteString("--- content ---\n")
	if strings.TrimSpace(msg.Content) != "" {
		b.WriteString(msg.Content)
		b.WriteString("\n")
	}
	if in.EmbedsText != "" {
		b.WriteString(in.EmbedsText)
		b.WriteString("\n")
	}
	if strings.TrimSpace(msg.Content) == "" && in.EmbedsText == "" {
		b.WriteString("(no text content)\n")
	}
	b.WriteString("--- end content ---\n")
	if in.ImageCount > 0 {
		fmt.Fprintf(&b, "attached_images: %d (provided below if vision is enabled)\n", in.ImageCount)
	}

	if in.ChannelNotes != "" {
		b.WriteString("\n# Channel profile (operator notes)\n")
		b.WriteString(in.ChannelNotes)
		b.WriteString("\n")
	}

	if in.ReplyToMessage != nil {
		b.WriteString("\n# Message being replied to\n")
		writeContextMessage(&b, in.ReplyToMessage)
	}
	if len(in.LinkedMessages) > 0 {
		b.WriteString("\n# Linked messages referenced in content\n")
		for _, lm := range in.LinkedMessages {
			writeContextMessage(&b, lm)
		}
	}

	b.WriteString("\n# Follower's tracked trades from this channel\n")
	if len(in.ActiveContexts) == 0 {
		b.WriteString("(none)\n")
	}
	for _, tc := range in.ActiveContexts {
		fmt.Fprintf(&b, "- trade root_message_id=%s: %s %s, state=%s, entry=%.8g, stop_loss=%.8g, tp_hits=%d, opened_at=%s\n",
			tc.RootMessageID, tc.Symbol, tc.Direction, tc.State,
			tc.AvgFillPrice, tc.StopLossPrice, tc.TPHitCount, formatMaybeTime(tc.OpenedAt))
	}

	if len(in.RecentSignals) > 0 {
		b.WriteString("\n# Recent channel messages (correlation only — NEVER act on these)\n")
		for i := len(in.RecentSignals) - 1; i >= 0; i-- { // oldest first
			sg := in.RecentSignals[i]
			fmt.Fprintf(&b, "- [%s] msg %s: %s/%s %s %s\n",
				sg.MessageTimestamp.UTC().Format("01-02 15:04"),
				sg.MessageID, sg.Classification, sg.Action, sg.Symbol, sg.Direction)
		}
	}

	if len(in.Positions) > 0 {
		b.WriteString("\n# Follower's current exchange positions\n")
		for _, p := range in.Positions {
			fmt.Fprintf(&b, "- %s %s qty=%.8g entry=%.8g uPnL=%.2f\n",
				p.Symbol, p.Side, p.PositionAmt, p.EntryPrice, p.UnrealizedProfit)
		}
	}

	b.WriteString("\nInterpret the message. Reply with the single JSON object only.")
	return b.String()
}

func writeContextMessage(b *strings.Builder, m *store.DiscordMessage) {
	fmt.Fprintf(b, "[message_id=%s author=%s at=%s]\n",
		m.MessageID, m.AuthorName, m.MessageTimestamp.UTC().Format(time.RFC3339))
	content := strings.TrimSpace(m.Content)
	if content != "" {
		b.WriteString(truncateText(content, 1500))
		b.WriteString("\n")
	}
	if m.EmbedsJSON != "" {
		b.WriteString("(message carries rich embeds; key values may be in the JSON below)\n")
		b.WriteString(truncateText(m.EmbedsJSON, 1500))
		b.WriteString("\n")
	}
}

func formatMaybeTime(t *time.Time) string {
	if t == nil {
		return "-"
	}
	return t.UTC().Format(time.RFC3339)
}

func truncateText(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…(truncated)"
}
