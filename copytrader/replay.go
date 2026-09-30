package copytrader

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"nofx/discord"
	"nofx/logger"
	"nofx/store"

	"github.com/google/uuid"
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

// Recognition verdicts only. EXECUTE is retained as a wire-compatibility value.
const (
	VerdictExecute = "EXECUTE" // recognition gates passed; execution gates are not simulated
	VerdictSkip    = "SKIP"    // classified but skipped by a gate
	VerdictInvalid = "INVALID" // interpretation rejected by validation
	VerdictError   = "ERROR"   // LLM call or parse failed
)

// ReplayItem is the dry-run interpretation result of one stored message.
type ReplayItem struct {
	EvaluationScope string    `json:"evaluation_scope"`
	UncheckedGates  []string  `json:"unchecked_gates"`
	MessageID       string    `json:"message_id"`
	Timestamp       time.Time `json:"timestamp"`
	Author          string    `json:"author"`
	Excerpt         string    `json:"excerpt"`
	ImageCount      int       `json:"image_count"` // image attachments on the message
	ImagesSent      int       `json:"images_sent"` // actually downloaded & sent to the LLM
	LLMMs           int64     `json:"llm_ms"`
	Classification  string    `json:"classification,omitempty"`
	Action          string    `json:"action,omitempty"`
	Symbol          string    `json:"symbol,omitempty"`
	Canonical       string    `json:"canonical,omitempty"`
	Direction       string    `json:"direction,omitempty"`
	Entries         string    `json:"entries,omitempty"`
	StopLoss        string    `json:"stop_loss,omitempty"`
	TakeProfits     string    `json:"take_profits,omitempty"`
	Verdict         string    `json:"verdict"`
	VerdictDetail   string    `json:"verdict_detail,omitempty"`
	Reasoning       string    `json:"reasoning,omitempty"`
	Warnings        []string  `json:"warnings,omitempty"`
	Error           string    `json:"error,omitempty"`
	ImageError      string    `json:"image_error,omitempty"`
	SystemPrompt    string    `json:"system_prompt,omitempty"`
	UserPrompt      string    `json:"user_prompt,omitempty"`
	RawResponse     string    `json:"raw_response,omitempty"`
	ParsedJSON      string    `json:"parsed_json,omitempty"`
}

// ReplayReport is the full state of one replay run. The engine keeps a live
// snapshot in memory and mirrors it to the replay tables for restart-safe
// inspection.
type ReplayReport struct {
	ID         string       `json:"id"`
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
	e.loadPersistedReplayLocked()
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

	replayID := uuid.NewString()
	startedAt := time.Now().UTC()
	if err := e.st.CopyTrade().CreateReplay(&store.CopyTradeReplay{
		ID:        replayID,
		TraderID:  e.traderID,
		ChannelID: e.cfg.PrimaryChannelID,
		Status:    ReplayRunning,
		Total:     len(queue),
		StartedAt: startedAt,
	}); err != nil {
		return fmt.Errorf("failed to persist replay: %w", err)
	}
	e.replay = &ReplayReport{
		ID:        replayID,
		Status:    ReplayRunning,
		Total:     len(queue),
		StartedAt: startedAt,
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
	e.loadPersistedReplayLocked()
	if e.replay == nil {
		return nil
	}
	snap := *e.replay
	snap.Items = append([]ReplayItem(nil), e.replay.Items...)
	return &snap
}

// loadPersistedReplayLocked restores the most recent report after an engine
// restart. A process that died while replaying cannot resume safely because
// its in-memory queue and model context are gone, so mark that stale run
// aborted before exposing it or allowing a new run to start.
// Caller must hold replayMu.
func (e *Engine) loadPersistedReplayLocked() {
	if e.replay != nil {
		return
	}
	replay, items, err := e.st.CopyTrade().GetLatestReplay(e.traderID)
	if err != nil || replay == nil {
		return
	}
	if replay.Status == ReplayRunning {
		now := time.Now().UTC()
		if err := e.st.CopyTrade().UpdateReplay(replay.ID, map[string]interface{}{
			"status":      ReplayAborted,
			"done":        replay.Done,
			"finished_at": &now,
		}); err != nil {
			logger.Warnf("[CopyTrade %s] stale replay %s could not be marked aborted: %v", e.traderName, replay.ID, err)
		}
		replay.Status = ReplayAborted
		replay.FinishedAt = &now
	}
	e.replay = replayReportFromStore(replay, items)
}

func (e *Engine) runReplay(queue []*store.DiscordMessage) {
	finish := func(status string) {
		now := time.Now().UTC()
		e.replayMu.Lock()
		e.replay.Status = status
		e.replay.FinishedAt = &now
		done := e.replay.Done
		replayID := e.replay.ID
		e.replayMu.Unlock()
		if replayID != "" {
			if err := e.st.CopyTrade().UpdateReplay(replayID, map[string]interface{}{
				"status":      status,
				"done":        done,
				"finished_at": &now,
			}); err != nil {
				logger.Warnf("[CopyTrade %s] replay persistence failed at finish: %v", e.traderName, err)
			}
		}
		logger.Infof("🔁 [CopyTrade %s] recognition replay %s (%d/%d)", e.traderName, status, done, len(queue))
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
		replayID := e.replay.ID
		e.replayMu.Unlock()
		if replayID != "" {
			if err := e.st.CopyTrade().CreateReplayItem(replayItemToStore(replayID, i, item)); err != nil {
				logger.Warnf("[CopyTrade %s] replay item persistence failed (%d/%d): %v", e.traderName, i+1, len(queue), err)
			}
			if err := e.st.CopyTrade().UpdateReplay(replayID, map[string]interface{}{"done": i + 1}); err != nil {
				logger.Warnf("[CopyTrade %s] replay progress persistence failed (%d/%d): %v", e.traderName, i+1, len(queue), err)
			}
		}

		// Gentle pacing: LLM + Discord media fetches, no need to burst.
		time.Sleep(300 * time.Millisecond)
	}
	finish(ReplayDone)
}

func replayItemToStore(replayID string, sequence int, item ReplayItem) *store.CopyTradeReplayItem {
	warnings, _ := json.Marshal(item.Warnings)
	return &store.CopyTradeReplayItem{
		ReplayID: replayID, Sequence: sequence, MessageID: item.MessageID, Timestamp: item.Timestamp,
		Author: item.Author, Excerpt: item.Excerpt, ImageCount: item.ImageCount, ImagesSent: item.ImagesSent,
		LLMMs: item.LLMMs, Classification: item.Classification, Action: item.Action, Symbol: item.Symbol,
		Canonical: item.Canonical, Direction: item.Direction, Entries: item.Entries, StopLoss: item.StopLoss,
		TakeProfits: item.TakeProfits, Verdict: item.Verdict, VerdictDetail: item.VerdictDetail,
		Reasoning: item.Reasoning, WarningsJSON: string(warnings), Error: item.Error, ImageError: item.ImageError,
		SystemPrompt: item.SystemPrompt, UserPrompt: item.UserPrompt, RawResponse: item.RawResponse, ParsedJSON: item.ParsedJSON,
	}
}

func replayReportFromStore(replay *store.CopyTradeReplay, items []*store.CopyTradeReplayItem) *ReplayReport {
	if replay == nil {
		return nil
	}
	report := &ReplayReport{
		ID: replay.ID, Status: replay.Status, Total: replay.Total, Done: replay.Done,
		StartedAt: replay.StartedAt, FinishedAt: replay.FinishedAt, Items: make([]ReplayItem, 0, len(items)),
	}
	for _, item := range items {
		var warnings []string
		_ = json.Unmarshal([]byte(item.WarningsJSON), &warnings)
		report.Items = append(report.Items, ReplayItem{
			MessageID: item.MessageID, Timestamp: item.Timestamp, Author: item.Author, Excerpt: item.Excerpt,
			ImageCount: item.ImageCount, ImagesSent: item.ImagesSent, LLMMs: item.LLMMs,
			Classification: item.Classification, Action: item.Action, Symbol: item.Symbol, Canonical: item.Canonical,
			Direction: item.Direction, Entries: item.Entries, StopLoss: item.StopLoss, TakeProfits: item.TakeProfits,
			Verdict: item.Verdict, VerdictDetail: item.VerdictDetail, Reasoning: item.Reasoning, Warnings: warnings,
			Error: item.Error, ImageError: item.ImageError, SystemPrompt: item.SystemPrompt, UserPrompt: item.UserPrompt,
			RawResponse: item.RawResponse, ParsedJSON: item.ParsedJSON,
		})
	}
	return report
}

// replayOne interprets one message in dry-run mode and evaluates the same
// gates the live pipeline applies (instrument resolution + validation),
// but never executes and never persists.
func (e *Engine) replayOne(msg *store.DiscordMessage) ReplayItem {
	item := ReplayItem{
		EvaluationScope: "current_rules_context_and_prices; historical execution state unavailable",
		UncheckedGates:  []string{"account_ownership", "balance", "contract_capability", "order_limits", "ttl", "live_execution"},
		MessageID:       msg.MessageID,
		Timestamp:       msg.MessageTimestamp,
		Author:          msg.AuthorName,
		Excerpt:         excerpt(msg.Content, 160),
	}
	for _, media := range MediaSources(msg, e.messageSources(msg)) {
		if media.Role == "current" {
			item.ImageCount++
		}
	}

	// Discord CDN attachment URLs are signed and expire; refresh the message
	// via the API so image replay still works on older history.
	if item.ImageCount > 0 && e.poller != nil {
		if client := e.poller.Client(); client != nil {
			if apiMsg, err := client.GetMessage(msg.ChannelID, msg.MessageID); err == nil {
				if fresh, cerr := discord.ToStoreMessage(apiMsg, msg.ChannelID); cerr == nil {
					refreshed := *msg
					refreshed.AttachmentsJSON = fresh.AttachmentsJSON
					refreshed.EmbedsJSON = fresh.EmbedsJSON
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

	instructions := interp.Flatten()
	if len(instructions) == 1 {
		verdict, detail, canonical := e.replayEvaluateSource(instructions[0], msg)
		item.Canonical = canonical
		item.Verdict = verdict
		item.VerdictDetail = detail
		return item
	}

	// Multi-instruction message: evaluate every instruction through the same
	// gates and aggregate (any EXECUTE wins, else any INVALID, else SKIP).
	var details []string
	executes, invalids := 0, 0
	for _, ins := range instructions {
		verdict, detail, _ := e.replayEvaluateSource(ins, msg)
		switch verdict {
		case VerdictExecute:
			executes++
		case VerdictInvalid:
			invalids++
		}
		d := fmt.Sprintf("%s %s => %s", ins.Action, ins.Symbol, verdict)
		if detail != "" {
			d += " (" + detail + ")"
		}
		details = append(details, d)
	}
	switch {
	case executes > 0:
		item.Verdict = VerdictExecute
	case invalids > 0:
		item.Verdict = VerdictInvalid
	default:
		item.Verdict = VerdictSkip
	}
	item.VerdictDetail = strings.Join(details, "; ")
	return item
}

// replayEvaluate applies the live pipeline's instrument + validation gates to
// ONE instruction (TTL is skipped on purpose: replayed history is always
// expired, and TTL is deterministic, not an AI concern). Returns the dry-run
// verdict, its detail and the resolved canonical symbol.
func (e *Engine) replayEvaluate(interp *SourceInterpretation) (verdict, detail, canonical string) {
	marketPrice := 0.0
	if interp.Symbol != "" {
		if c, rerr := ResolveInstrument(interp.Symbol); rerr == nil {
			canonical = c
			if mp, perr := e.exec.ex.GetMarketPrice(c); perr == nil {
				marketPrice = mp
			}
		} else if interp.IsActionable() {
			return VerdictSkip, string(SkipUnsupportedInstrument), ""
		}
	}

	skipReason, verr := ValidateInterpretation(interp, marketPrice)
	if verr != nil {
		return VerdictInvalid, verr.Error(), canonical
	}
	if skipReason != SkipNone {
		return VerdictSkip, string(skipReason), canonical
	}
	if !interp.IsActionable() {
		return VerdictSkip, string(SkipNotSignal), canonical
	}
	return VerdictExecute, fmt.Sprintf("%s %s %s", interp.Action, interp.Direction, canonical), canonical
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
	case PriceTPLevel:
		return fmt.Sprintf("TP%d", p.Level)
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

func (e *Engine) replayEvaluateSource(ins *SourceInterpretation, msg *store.DiscordMessage) (string, string, string) {
	if skip, err := ValidateActionEvidence(ins, e.messageSources(msg)); err != nil {
		return VerdictInvalid, err.Error(), ""
	} else if skip != SkipNone {
		return VerdictSkip, string(skip), ""
	}
	return e.replayEvaluate(ins)
}
