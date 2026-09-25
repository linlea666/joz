package copytrader

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"nofx/store"
)

type InstructionResult struct {
	Index      int        `json:"index"`
	Action     Action     `json:"action"`
	Symbol     string     `json:"symbol"`
	Direction  Direction  `json:"direction,omitempty"`
	Status     string     `json:"status"`
	SkipReason SkipReason `json:"skip_reason,omitempty"`
	Detail     string     `json:"detail,omitempty"`
}

func (e *Engine) messageSources(msg *store.DiscordMessage) []SourceSegment {
	segments := BuildSourceSegments(msg, e.cfg.InterpretationProfile)
	history, _ := e.st.DiscordMessage().GetRecentByChannel(msg.ChannelID, msg.MessageTimestamp.AddDate(0, 0, -7), 200)
	if active, err := e.st.CopyTrade().GetActiveContexts(e.traderID); err == nil {
		for _, c := range active {
			if c.ChannelID == msg.ChannelID {
				if root, err := e.st.DiscordMessage().GetByMessageID(c.ChannelID, c.RootMessageID); err == nil && root != nil {
					history = append(history, root)
				}
			}
		}
	}
	MatchHistoricalSegments(segments, msg, history)
	// A quoted embed's image inherits the role of the card after historical matching.
	for i := range segments {
		if segments[i].Image {
			for _, s := range segments {
				if segments[i].ID == s.ID+":image" {
					segments[i].Role = s.Role
				}
			}
		}
	}
	return segments
}

func stableID(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:])
}

func (e *Engine) claimInstruction(signalID string, msg *store.DiscordMessage, ins *SourceInterpretation, canonical string) (*store.CopyTradeAction, bool, error) {
	contextID := ""
	direction := string(ins.Direction)
	if ins.Action != ActionOpen && ins.Action != ActionAdd {
		if c := e.correlateContext(ins, msg, canonical); c != nil {
			contextID = c.ID
			direction = c.Direction
		}
	}
	// Entry/exit quantities are frozen for a message's first actionable version.
	// Revising an executed reduction's wording or percentage cannot reduce again.
	semantic := ""
	if ins.Action == ActionUpdateSL || ins.Action == ActionUpdateTP {
		b, _ := json.Marshal(struct {
			SL    []SLLevel
			TP    []TPLevel
			Rules []ConditionalRule
		}{ins.StopLossLevels, ins.TakeProfitLevels, ins.ConditionalRules})
		semantic = string(b)
	}
	id := stableID(e.traderID, msg.MessageID, canonical, direction, contextID, string(ins.Action), semantic)
	payload, _ := json.Marshal(ins)
	a := &store.CopyTradeAction{ID: id, TraderID: e.traderID, SignalID: signalID, MessageID: msg.MessageID, ContextID: contextID, Symbol: canonical, Direction: direction, Action: string(ins.Action), Status: "executing", PayloadJSON: string(payload)}
	legacy, err := e.st.CopyTrade().LegacyExecutedAction(e.traderID, msg.MessageID, canonical, string(ins.Action))
	if err != nil {
		return a, false, err
	}
	if legacy {
		return a, false, nil
	}
	claimed, err := e.st.CopyTrade().ClaimAction(a)
	return a, claimed, err
}

func (e *Engine) deferInterpretationRetry(sig *store.CopyTradeSignal, msg *store.DiscordMessage, err error) bool {
	if sig.ExecutionVersion == 0 || sig.RetryCount >= 2 || !transientModelError(err) {
		return false
	}
	actions, aerr := e.st.CopyTrade().GetActionsForMessage(e.traderID, msg.MessageID)
	if aerr != nil || len(actions) > 0 {
		return false
	}
	delays := []time.Duration{30 * time.Second, 120 * time.Second}
	next := time.Now().UTC().Add(delays[sig.RetryCount])
	ref := msg.MessageTimestamp
	if msg.EditedAt != nil && msg.EditedAt.After(ref) {
		ref = *msg.EditedAt
	}
	ttl := e.interpretationRetryTTL(msg)
	if ttl <= 0 || IsExpired(ref, next, ttl) {
		return false
	}
	e.updateSignal(sig.ID, map[string]interface{}{"status": "retry_wait", "retry_count": sig.RetryCount + 1, "next_retry_at": next, "error_message": err.Error()})
	e.events.Warn(sig.ID, sig.ID, msg.MessageID, EvAIError, fmt.Sprintf("temporary model failure; deferred attempt %d/2 at %s (no execution actions)", sig.RetryCount+1, next.Format(time.RFC3339)), nil)
	return true
}

func (e *Engine) interpretationRetryTTL(msg *store.DiscordMessage) time.Duration {
	openTTL := time.Duration(e.cfg.OpenSignalTTLSeconds) * time.Second
	mgmtTTL := time.Duration(e.cfg.MgmtSignalTTLSeconds) * time.Second
	current := ""
	for _, s := range e.messageSources(msg) {
		if s.Role == "current" && !s.Image {
			current += "\n" + s.Text
		}
	}
	if (sourceManagement.MatchString(current) || sourceCancel.MatchString(current)) && !sourceOpen.MatchString(current) && !sourceAdd.MatchString(current) {
		return mgmtTTL
	}
	// With no successful interpretation, unknown/image-only tasks use the
	// shorter TTL. They cannot obtain a longer entry lifetime through retries.
	if openTTL > 0 && (mgmtTTL <= 0 || openTTL < mgmtTTL) {
		return openTTL
	}
	return mgmtTTL
}

func transientModelError(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	if !strings.Contains(s, "llm call failed") {
		return false
	}
	for _, word := range []string{"504", "502", "503", "429", "timeout", "timed out", "connection reset", "temporarily unavailable", "unexpected eof"} {
		if strings.Contains(s, word) {
			return true
		}
	}
	return false
}

func (e *Engine) retryLoop() {
	defer e.wg.Done()
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-e.stopCh:
			return
		case <-ticker.C:
			rows, err := e.st.CopyTrade().DueRetries(e.traderID, time.Now().UTC())
			if err != nil {
				continue
			}
			for _, s := range rows {
				select {
				case <-e.stopCh:
					return
				default:
				}
				msg, err := e.st.DiscordMessage().GetByMessageID(s.ChannelID, s.MessageID)
				if err != nil {
					continue
				}
				if msg == nil || msg.Revision != s.MessageRevision {
					e.updateSignal(s.ID, map[string]interface{}{"status": store.SignalStatusSkipped, "skip_reason": "REVISION_SUPERSEDED", "next_retry_at": nil})
					continue
				}
				_ = e.HandleMessage(msg, msg.Revision > 0)
			}
		}
	}
}

// Original levels never change when remaining orders are resized/replaced.
type TPRecipe struct {
	Version        int       `json:"version"`
	Ordinals       []int     `json:"ordinals,omitempty"`
	OriginalPrices []float64 `json:"original_prices,omitempty"`
	Prices         []float64 `json:"prices"`
	Ratios         []float64 `json:"ratios"`
}

func recipeJSON(prices, ratios []float64) string {
	b, _ := json.Marshal(TPRecipe{Version: 1, Prices: prices, Ratios: ratios})
	return string(b)
}
func resolveContextPrice(spec PriceSpec, ctx *store.CopyTradeContext, entry float64) float64 {
	if spec.Type != PriceTPLevel {
		return resolveHardPrice(spec, entry)
	}
	var recipe TPRecipe
	if json.Unmarshal([]byte(ctx.TPRecipeJSON), &recipe) == nil && recipe.Version == 1 {
		prices := recipe.OriginalPrices
		if len(prices) == 0 {
			prices = recipe.Prices
		}
		if spec.Level > 0 && spec.Level <= len(prices) {
			return prices[spec.Level-1]
		}
	}

	for _, tp := range readTPPlan(ctx) {
		if tp.Ordinal == spec.Level {
			return tp.Price
		}
	}
	return 0
}

func planOrdinal(plan *OpenPlan, index int) int {
	if index < len(plan.TPOrdinals) && plan.TPOrdinals[index] > 0 {
		return plan.TPOrdinals[index]
	}
	return index + 1
}
func planRecipeJSON(plan *OpenPlan) string {
	b, _ := json.Marshal(TPRecipe{Version: 1, Prices: plan.TPPrices, Ratios: plan.TPRatios, Ordinals: plan.TPOrdinals, OriginalPrices: plan.TPOriginalPrices})
	return string(b)
}
func recipeOrdinal(recipe TPRecipe, index int) int {
	if index < len(recipe.Ordinals) && recipe.Ordinals[index] > 0 {
		return recipe.Ordinals[index]
	}
	return index + 1
}
