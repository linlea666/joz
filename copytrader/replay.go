package copytrader

import (
	"fmt"
	"strings"
	"time"

	"nofx/discord"
	"nofx/logger"
	"nofx/store"
)

// replay.go implements the recognition replay tool: it re-runs the AI
// interpretation over stored channel history WITHOUT executing anything and
// WITHOUT persisting AI runs / signals / events, producing an accuracy report
// the user can review before trusting the pipeline with real money.

// Replay report status values.
const (
	ReplayRunning = "running"
	ReplayDone    = "done"
	ReplayAborted = "aborted"
)

// Replay verdict values (what the live pipeline WOULD have done).
const (
	VerdictExecute = "EXECUTE" // would enter the execution path
	VerdictSkip    = "SKIP"    // classified but skipped by a gate
	VerdictInvalid = "INVALID" // interpretation rejected by validation
	VerdictError   = "ERROR"   // LLM call or parse failed
)

// ReplayItem is the dry-run interpretation result of one stored message.
type ReplayItem struct {
	MessageID      string    `json:"message_id"`
	Timestamp      time.Time `json:"timestamp"`
	Author         string    `json:"author"`
	Excerpt        string    `json:"excerpt"`
	ImageCount     int       `json:"image_count"` // image attachments on the message
	ImagesSent     int       `json:"images_sent"` // actually downloaded & sent to the LLM
	LLMMs          int64     `json:"llm_ms"`
	Classification string    `json:"classification,omitempty"`
	Action         string    `json:"action,omitempty"`
	Symbol         string    `json:"symbol,omitempty"`
	Canonical      string    `json:"canonical,omitempty"`
	Direction      string    `json:"direction,omitempty"`
	Entries        string    `json:"entries,omitempty"`
	StopLoss       string    `json:"stop_loss,omitempty"`
	TakeProfits    string    `json:"take_profits,omitempty"`
	Verdict        string    `json:"verdict"`
	VerdictDetail  string    `json:"verdict_detail,omitempty"`
	Reasoning      string    `json:"reasoning,omitempty"`
	Warnings       []string  `json:"warnings,omitempty"`
	Error          string    `json:"error,omitempty"`
	ImageError     string    `json:"image_error,omitempty"`
	SystemPrompt   string    `json:"system_prompt,omitempty"`
	UserPrompt     string    `json:"user_prompt,omitempty"`
	RawResponse    string    `json:"raw_response,omitempty"`
	ParsedJSON     string    `json:"parsed_json,omitempty"`
}

// ReplayReport is the full state of one replay run (kept in memory only).
type ReplayReport struct {
	Status     string       `json:"status"`
	Total      int          `json:"total"`
	Done       int          `json:"done"`
	StartedAt  time.Time    `json:"started_at"`
	FinishedAt *time.Time   `json:"finished_at,omitempty"`
	Items      []ReplayItem `json:"items"`
}

// StartReplay launches a dry-run interpretation of the most recent stored
// channel messages (oldest first). Only one replay per engine at a time.
func (e *Engine) StartReplay(limit int) error {
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}

	e.replayMu.Lock()
	defer e.replayMu.Unlock()
	if e.replay != nil && e.replay.Status == ReplayRunning {
		return fmt.Errorf("a replay is already running (%d/%d)", e.replay.Done, e.replay.Total)
	}

	msgs, err := e.st.DiscordMessage().GetRecentByChannel(e.cfg.PrimaryChannelID, time.Time{}, limit)
	if err != nil {
		return fmt.Errorf("failed to load channel history: %w", err)
	}
	// GetRecentByChannel returns newest first; replay in chronological order
	// and drop messages with nothing to interpret (same gate as HandleMessage).
	var queue []*store.DiscordMessage
	for i := len(msgs) - 1; i >= 0; i-- {
		m := msgs[i]
		if strings.TrimSpace(m.Content) == "" && m.EmbedsJSON == "" && m.AttachmentsJSON == "" {
			continue
		}
		queue = append(queue, m)
	}
	if len(queue) == 0 {
		return fmt.Errorf("no stored messages to replay for channel %s", e.cfg.PrimaryChannelID)
	}

	e.replay = &ReplayReport{
		Status:    ReplayRunning,
		Total:     len(queue),
		StartedAt: time.Now().UTC(),
		Items:     make([]ReplayItem, 0, len(queue)),
	}
	go e.runReplay(queue)
	logger.Infof("🔁 [CopyTrade %s] recognition replay started: %d messages", e.traderName, len(queue))
	return nil
}

// ReplayStatus returns a snapshot of the current/last replay report.
func (e *Engine) ReplayStatus() *ReplayReport {
	e.replayMu.Lock()
	defer e.replayMu.Unlock()
	if e.replay == nil {
		return nil
	}
	snap := *e.replay
	snap.Items = append([]ReplayItem(nil), e.replay.Items...)
	return &snap
}

func (e *Engine) runReplay(queue []*store.DiscordMessage) {
	finish := func(status string) {
		now := time.Now().UTC()
		e.replayMu.Lock()
		e.replay.Status = status
		e.replay.FinishedAt = &now
		e.replayMu.Unlock()
		logger.Infof("🔁 [CopyTrade %s] recognition replay %s (%d/%d)", e.traderName, status, len(queue), len(queue))
	}

	for i, msg := range queue {
		select {
		case <-e.stopCh:
			finish(ReplayAborted)
			return
		default:
		}

		item := e.replayOne(msg)
		e.replayMu.Lock()
		e.replay.Items = append(e.replay.Items, item)
		e.replay.Done = i + 1
		e.replayMu.Unlock()

		// Gentle pacing: LLM + Discord media fetches, no need to burst.
		time.Sleep(300 * time.Millisecond)
	}
	finish(ReplayDone)
}

// replayOne interprets one message in dry-run mode and evaluates the same
// gates the live pipeline applies (instrument resolution + validation),
// but never executes and never persists.
func (e *Engine) replayOne(msg *store.DiscordMessage) ReplayItem {
	item := ReplayItem{
		MessageID: msg.MessageID,
		Timestamp: msg.MessageTimestamp,
		Author:    msg.AuthorName,
		Excerpt:   excerpt(msg.Content, 160),
	}
	for _, att := range discord.ParseStoredAttachments(msg.AttachmentsJSON) {
		if att.IsImage() {
			item.ImageCount++
		}
	}

	// Discord CDN attachment URLs are signed and expire; refresh the message
	// via the API so image replay still works on older history.
	if item.ImageCount > 0 {
		if client := e.poller.Client(); client != nil {
			if apiMsg, err := client.GetMessage(msg.ChannelID, msg.MessageID); err == nil {
				if fresh, cerr := discord.ToStoreMessage(apiMsg, msg.ChannelID); cerr == nil {
					refreshed := *msg
					refreshed.AttachmentsJSON = fresh.AttachmentsJSON
					msg = &refreshed
				}
			}
		}
	}

	interp, run, timings, err := e.interpret("replay", "", msg, false, true)
	item.LLMMs = timings.llmMs
	item.ImageError = timings.imageErr
	if run != nil {
		item.ImagesSent = run.ImageCount
		item.SystemPrompt = run.SystemPrompt
		item.UserPrompt = run.InputPrompt
		item.RawResponse = run.RawResponse
		item.ParsedJSON = run.ParsedJSON
	}
	if err != nil {
		item.Verdict = VerdictError
		item.Error = err.Error()
		return item
	}

	item.Classification = string(interp.Classification)
	item.Action = string(interp.Action)
	item.Symbol = interp.Symbol
	item.Direction = string(interp.Direction)
	item.Reasoning = interp.Reasoning
	item.Warnings = interp.Warnings
	item.Entries = fmtEntries(interp.EntryOrders)
	item.StopLoss = fmtSLLevels(interp.StopLossLevels)
	item.TakeProfits = fmtTPLevels(interp.TakeProfitLevels)

	// Same gate order as processMessage (TTL is skipped on purpose: replayed
	// history is always expired, and TTL is deterministic, not an AI concern).
	marketPrice := 0.0
	if interp.Symbol != "" {
		if c, rerr := ResolveInstrument(interp.Symbol); rerr == nil {
			item.Canonical = c
			if mp, perr := e.exec.ex.GetMarketPrice(c); perr == nil {
				marketPrice = mp
			}
		} else if interp.IsActionable() {
			item.Verdict = VerdictSkip
			item.VerdictDetail = string(SkipUnsupportedInstrument)
			return item
		}
	}

	skipReason, verr := ValidateInterpretation(interp, marketPrice)
	if verr != nil {
		item.Verdict = VerdictInvalid
		item.VerdictDetail = verr.Error()
		return item
	}
	if skipReason != SkipNone {
		item.Verdict = VerdictSkip
		item.VerdictDetail = string(skipReason)
		return item
	}
	if !interp.IsActionable() {
		item.Verdict = VerdictSkip
		item.VerdictDetail = string(SkipNotSignal)
		return item
	}

	item.Verdict = VerdictExecute
	item.VerdictDetail = fmt.Sprintf("%s %s %s", interp.Action, interp.Direction, item.Canonical)
	return item
}

func excerpt(s string, max int) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}

func fmtPriceSpec(p PriceSpec) string {
	switch p.Type {
	case PriceFixed:
		return trimFloat(p.Price)
	case PriceMarket:
		if p.Price > 0 {
			return "market(~" + trimFloat(p.Price) + ")"
		}
		return "market"
	case PriceRange:
		return trimFloat(p.RangeLow) + "-" + trimFloat(p.RangeHigh)
	case PriceRMultiple:
		return fmt.Sprintf("%.2gR", p.Offset)
	default:
		return string(p.Type)
	}
}

func fmtEntries(orders []EntryOrder) string {
	var parts []string
	for _, o := range orders {
		parts = append(parts, string(o.OrderType)+"@"+fmtPriceSpec(o.Price))
	}
	return strings.Join(parts, ", ")
}

func fmtSLLevels(levels []SLLevel) string {
	var parts []string
	for _, l := range levels {
		s := fmtPriceSpec(l.Price)
		if l.Conditional != "" {
			s += " (" + l.Conditional + ")"
		}
		parts = append(parts, s)
	}
	return strings.Join(parts, ", ")
}

func fmtTPLevels(levels []TPLevel) string {
	var parts []string
	for _, l := range levels {
		s := fmtPriceSpec(l.Price)
		if l.Ratio != nil {
			s += fmt.Sprintf(" %.0f%%", *l.Ratio)
		}
		parts = append(parts, s)
	}
	return strings.Join(parts, ", ")
}

func trimFloat(f float64) string {
	return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.6f", f), "0"), ".")
}
