package copytrader

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"nofx/discord"
	"nofx/logger"
	"nofx/mcp"
	"nofx/store"
	"nofx/trader/types"
)

// EngineParams wires an Engine to its dependencies.
type EngineParams struct {
	TraderID   string
	TraderName string
	UserID     string
	Config     *CopyTradingConfig
	Store      *store.Store
	LLM        mcp.AIClient
	ModelID    string // for AI run records
	Provider   string
	Exchange   types.Trader
	Source     discord.Source
}

// Engine runs copy trading for ONE trader: it consumes channel messages from
// the durable event source, interprets them with the LLM, applies deterministic risk rules
// and executes through the exchange. All message handling for the trader is
// strictly serial (messageMu); mu independently protects execution/reconciliation.
type Engine struct {
	traderID   string
	traderName string
	userID     string
	cfg        *CopyTradingConfig
	st         *store.Store
	llm        mcp.AIClient
	modelID    string
	provider   string
	source     discord.Source
	exec       *Executor
	events     *EventLogger

	messageMu sync.Mutex // preserves message order while the model is waiting
	mu        sync.Mutex // serializes exchange operations and reconciliation only
	stateMu   sync.Mutex
	running   bool
	stopCh    chan struct{}
	wg        sync.WaitGroup

	// posMissSeen records trade context IDs whose position was not found on
	// the previous reconcile cycle. A trade is only declared closed after two
	// consecutive misses: exchange GetPositions results are cached (~15s), so
	// a single miss can be a stale snapshot taken before the entry filled
	// (the Binance BTCUSDT short incident: reconcile closed a 1-second-old
	// trade and cancelled its fresh SL/TP). Guarded by mu (reconcileOnce and
	// message handling both hold it). Lost on restart, which only means one
	// extra 45s confirmation cycle — the conservative direction.
	posMissSeen map[string]bool

	// closedRecheck maps context IDs that reconcile just closed
	// (RECONCILE_CLOSED) to the number of confirmation cycles left. If the
	// position reappears within that window the close was spurious (stale
	// data): the context is resurrected to OPEN so the SL guard re-arms its
	// protections — without this, a wrongly-closed trade leaves a live
	// position permanently untracked and unprotected (the BTCUSDT 0.007 and
	// SNDKUSDT 0.15 incidents). Guarded by mu; lost on restart (acceptable:
	// the debounce makes new spurious closes very unlikely to begin with).
	closedRecheck map[string]int

	// Recognition replay (dry-run accuracy testing); own mutex domain so a
	// running replay never blocks live message handling.
	replayMu sync.Mutex
	replay   *ReplayReport
}

// NewEngine creates the engine (Start must be called to begin processing).
func NewEngine(p EngineParams) *Engine {
	// The client may resolve a provider default when no custom model is set.
	// Record what CallWithRequest actually uses, not the empty form field.
	if client, ok := p.LLM.(mcp.ClientEmbedder); ok && client.BaseClient() != nil {
		p.ModelID, p.Provider = client.BaseClient().Model, client.BaseClient().Provider
	}
	events := NewEventLogger(p.Store, p.TraderID, p.Config.PrimaryChannelID)
	e := &Engine{
		traderID:   p.TraderID,
		traderName: p.TraderName,
		userID:     p.UserID,
		cfg:        p.Config,
		st:         p.Store,
		llm:        p.LLM,
		modelID:    p.ModelID,
		provider:   p.Provider,
		source:     p.Source,
		exec:       NewExecutor(p.TraderID, p.Exchange, p.Store, events),
		events:     events,

		posMissSeen:   make(map[string]bool),
		closedRecheck: make(map[string]int),
	}
	if p.Source != nil {
		e.exec.beforeEntrySubmit = e.admitEntrySubmit
	}
	return e
}

// Start subscribes to the channel and launches the reconcile loop.
func (e *Engine) Start() error {
	e.stateMu.Lock()
	if e.running {
		e.stateMu.Unlock()
		return nil
	}
	e.running = true
	e.stopCh = make(chan struct{})
	e.stateMu.Unlock()

	if err := e.source.SubscribeRoute(discord.Route{TraderID: e.traderID, Channels: e.cfg.ListenChannels(), RulesJSON: e.cfg.MessageRules().Snapshot(), ExecutionKey: e.cfg.ExecutionKey()}, e.HandleMessage); err != nil {
		// Roll back so a later Start() attempt is not silently ignored.
		e.stateMu.Lock()
		e.running = false
		close(e.stopCh)
		e.stateMu.Unlock()
		return fmt.Errorf("channel subscription failed: %w", err)
	}
	e.wg.Add(2)
	go e.reconcileLoop()
	go e.retryLoop()
	logger.Infof("🎯 [CopyTrade %s] engine started (channel %s)", e.traderName, e.cfg.PrimaryChannelID)
	return nil
}

// Stop detaches from the source and stops the reconcile loop.
func (e *Engine) Stop() {
	e.stateMu.Lock()
	if !e.running {
		e.stateMu.Unlock()
		return
	}
	e.running = false
	close(e.stopCh)
	e.stateMu.Unlock()

	e.source.UnsubscribeRoute(e.traderID)
	e.wg.Wait()
	// Drain an exchange operation already in progress. An interpretation still
	// waiting for its model will observe stopCh before taking any new action.
	e.mu.Lock()
	e.mu.Unlock()
	logger.Infof("⏹ [CopyTrade %s] engine stopped", e.traderName)
}

// HandleMessage is the durable source callback: one Discord message (or revision).
func (e *Engine) HandleMessage(msg *store.DiscordMessage, isEdit bool) error {
	e.messageMu.Lock()
	defer e.messageMu.Unlock()

	// Dedup across restarts: this exact revision already interpreted?
	done, err := e.st.CopyTrade().SignalProcessed(e.traderID, msg.MessageID, msg.Revision)
	if err != nil {
		return fmt.Errorf("dedup check failed: %w", err)
	}
	if done {
		return nil
	}

	// Author filter (rule layer, zero cost).
	if !e.messageRules(msg).AllowsAuthor(msg) {
		return nil
	}
	// Nothing to interpret at all.
	if strings.TrimSpace(msg.Content) == "" && msg.EmbedsJSON == "" && msg.AttachmentsJSON == "" {
		return nil
	}

	if err := e.processMessage(msg, isEdit); err != nil {
		return err
	}
	if msg.DeliveryID != 0 {
		sig, err := e.st.CopyTrade().LatestSignal(e.traderID, msg.MessageID, msg.Revision)
		if err != nil {
			return err
		}
		if sig == nil {
			return fmt.Errorf("signal persistence incomplete")
		}
		if sig.Status == store.SignalStatusReceived || sig.Status == store.SignalStatusParsed || sig.Status == store.SignalStatusExecuting {
			return fmt.Errorf("signal outcome not committed")
		}
	}
	// Errors inside processMessage are recorded on the signal; the message
	// itself is considered consumed either way.
	return nil
}

// processMessage runs the full pipeline for one message revision.
func (e *Engine) processMessage(msg *store.DiscordMessage, isEdit bool) (pipelineErr error) {
	pipelineStart := time.Now()
	e.stateMu.Lock()
	stopForThisRun := e.stopCh
	e.stateMu.Unlock()
	existing, lookupErr := e.st.CopyTrade().LatestSignal(e.traderID, msg.MessageID, msg.Revision)
	if lookupErr != nil {
		return lookupErr
	}
	if existing != nil && (existing.Status == "retry_wait" || existing.Status == "execution_wait") && existing.NextRetryAt != nil && existing.NextRetryAt.After(time.Now()) {
		return
	}
	if existing != nil && existing.ExecutionVersion == 0 {
		return
	} // never resume an unknown legacy side effect
	signalID := uuid.NewString()
	if existing != nil {
		signalID = existing.ID
	}
	traceID := signalID

	// Receive latency is measured from the revision the pipeline is reacting
	// to: for edits that is EditedAt, not the original post time (an old
	// message edited days later would otherwise record a multi-day latency
	// and poison the latency stats). Mirrors the TTL gate's refTime rule.
	latencyRef := msg.MessageTimestamp
	if msg.EditedAt != nil && msg.EditedAt.After(latencyRef) {
		latencyRef = *msg.EditedAt
	}

	sig := &store.CopyTradeSignal{
		ID:                signalID,
		ExecutionVersion:  1,
		RulesSnapshotJSON: e.messageRules(msg).Snapshot(),
		SourceEventID:     msg.SourceEventID, DeliveryID: msg.DeliveryID, LogicalChannelID: e.cfg.PrimaryChannelID, QueueMs: time.Since(msg.ReceivedAt).Milliseconds(),
		TraderID:         e.traderID,
		ChannelID:        msg.ChannelID,
		MessageID:        msg.MessageID,
		MessageRevision:  msg.Revision,
		Status:           store.SignalStatusReceived,
		MessageTimestamp: msg.MessageTimestamp,
		ReceivedAt:       time.Now().UTC(),
		ReceiveLatencyMs: msg.ReceivedAt.Sub(latencyRef).Milliseconds(),
	}
	if existing != nil {
		sig = existing
	}
	if existing == nil {
		if err := e.st.CopyTrade().CreateSignal(sig); err != nil {
			return err
		}
	}
	e.events.Info(traceID, signalID, msg.MessageID, EvMessageReceived,
		fmt.Sprintf("message from %s (revision %d, edit=%v), receive latency %.1fs",
			msg.AuthorName, msg.Revision, isEdit, float64(sig.ReceiveLatencyMs)/1000), nil)

	persist := func(updates map[string]interface{}) {
		if pipelineErr != nil {
			return
		}
		pipelineErr = e.st.CopyTrade().UpdateSignal(signalID, updates)
	}
	fail := func(stage string, err error) {
		e.events.Error(traceID, signalID, msg.MessageID, EvExecutionError, stage+": "+err.Error(), nil)
		persist(map[string]interface{}{
			"status": store.SignalStatusFailed, "error_message": err.Error(),
			"total_ms": time.Since(pipelineStart).Milliseconds(),
		})
	}
	skip := func(reason SkipReason, detail string) {
		e.events.Info(traceID, signalID, msg.MessageID, EvSignalSkipped, fmt.Sprintf("skipped (%s): %s", reason, detail), nil)
		persist(map[string]interface{}{
			"status": store.SignalStatusSkipped, "skip_reason": string(reason), "next_retry_at": nil,
			"total_ms": time.Since(pipelineStart).Milliseconds(),
		})
	}

	// --- 1. Assemble context ---
	if _, err := e.rulesForSignal(signalID); err != nil {
		skip(SkipNeedsContext, "rule snapshot unavailable: "+err.Error())
		return
	}
	// Only brand-new tasks can be discarded before interpretation. Either zero
	// TTL disables this optimization; management may outlive an opening signal.
	// Stored interpretations and uncertain execution continue through recovery.
	if existing == nil && e.cfg.OpenSignalTTLSeconds > 0 && e.cfg.MgmtSignalTTLSeconds > 0 &&
		IsExpired(latencyRef, time.Now().UTC(), time.Duration(max(e.cfg.OpenSignalTTLSeconds, e.cfg.MgmtSignalTTLSeconds))*time.Second) {
		skip(SkipExpired, "expired before interpretation (both opening and management TTL); source retained for context")
		return
	}
	if existing != nil && existing.Status == "retry_wait" && IsExpired(latencyRef, time.Now().UTC(), e.interpretationRetryTTL(msg, signalID)) {
		skip(SkipExpired, "deferred interpretation expired before retry")
		return
	}
	var interp *SourceInterpretation
	var run *store.CopyTradeAIRun
	var timings pipelineTimings
	var err error
	if existing != nil && existing.InterpretationJSON != "" && existing.Status != "retry_wait" {
		interp, err = ParseInterpretation(existing.InterpretationJSON)
	} else {
		interp, run, timings, err = e.interpret(traceID, signalID, msg, isEdit, false)
	}
	if run != nil {
		persist(map[string]interface{}{"ai_run_id": run.ID, "media_download_ms": timings.mediaMs, "prompt_build_ms": timings.promptMs, "llm_request_ms": timings.llmMs})
	}
	if err != nil {
		if e.deferInterpretationRetry(sig, msg, err) {
			return
		}
		fail("AI interpretation", err)
		return
	}
	aiRunID := int64(0)
	if run != nil {
		aiRunID = run.ID
	} else if existing != nil {
		aiRunID = existing.AIRunID
		timings.mediaMs, timings.promptMs, timings.llmMs = existing.MediaDownloadMs, existing.PromptBuildMs, existing.LLMRequestMs
	}
	interpJSON, _ := json.Marshal(interp)
	persist(map[string]interface{}{
		"status":               store.SignalStatusParsed,
		"ai_run_id":            aiRunID,
		"classification":       string(interp.Classification),
		"action":               string(interp.Action),
		"symbol":               interp.Symbol,
		"direction":            string(interp.Direction),
		"interpretation_json":  string(interpJSON),
		"has_execution_intent": interp.IsActionable(),
		"media_download_ms":    timings.mediaMs,
		"prompt_build_ms":      timings.promptMs,
		"llm_request_ms":       timings.llmMs,
		"next_retry_at":        nil,
		"error_message":        "",
		"skip_reason":          "",
	})
	processingPath, processingModel := "stored_interpretation", ""
	if run != nil {
		processingPath, processingModel = "ai", run.Model
		if strings.HasPrefix(run.Model, "deterministic:") {
			processingPath = "deterministic"
		}
	}
	e.events.Success(traceID, signalID, msg.MessageID, EvSignalClassified,
		fmt.Sprintf("%s / %s %s %s ("+processingPath+" %.1fs)", interp.Classification, interp.Action,
			interp.Symbol, interp.Direction, float64(timings.llmMs)/1000),
		timings.llmMs, map[string]interface{}{"reasoning": interp.Reasoning, "warnings": interp.Warnings, "processing_path": processingPath, "model": processingModel})

	if pipelineErr != nil {
		return
	}
	// Model calls never hold the execution lock. Read target state again under
	// this lock; a TP fill or timeout may have changed it during interpretation.
	e.mu.Lock()
	defer e.mu.Unlock()
	select {
	case <-stopForThisRun:
		if msg.DeliveryID != 0 {
			return fmt.Errorf("engine replaced during interpretation; receipt retained")
		}
		skip(SkipPaused, "engine stopped while interpreting; no actions submitted")
		return
	default:
	}
	rules, rulesErr := e.rulesForSignal(signalID)
	if rulesErr != nil {
		fail("message rules", rulesErr)
		return
	}
	interp = ApplyNotificationPolicy(interp, msg, rules)
	sources := e.messageSources(msg, rules.Profile)
	// --- 2..4. Gates & routing, per instruction ---
	// Multi-instruction messages (one post managing several tracked trades,
	// e.g. "SEI SL to BE, SUI SL to BE") flatten to their instructions;
	// classic single-instruction messages flatten to themselves. Every
	// instruction runs the same gates the single path always had.
	instructions := interp.Flatten()
	multi := len(instructions) > 1

	if interp.IsActionable() {
		persist(map[string]interface{}{"status": store.SignalStatusExecuting})
	}

	var (
		firstErr   error
		errCount   int
		firstSkip  SkipReason
		waiting    bool
		skipDetail string
		executed   int
	)
	results := make([]InstructionResult, 0, len(instructions))
	for i, ins := range instructions {
		if pipelineErr != nil {
			return
		}
		insSkip, insDetail, insErr := ValidateActionEvidenceDetailed(ins, sources)
		if insSkip == SkipNone && insErr == nil {
			insSkip, insDetail, insErr = e.processInstruction(traceID, signalID, msg, ins)
		}
		if insSkip == SkipWaitingReconciliation || insSkip == "SOURCE_NOT_READY" || insSkip == "SOURCE_GAP_OR_PERMISSION" {
			waiting = true
		}
		result := InstructionResult{Index: i, Action: ins.Action, Symbol: ins.Symbol, Direction: ins.Direction, Status: "executed", SkipReason: insSkip, Detail: insDetail}
		if insErr != nil {
			result.Status = "failed"
			result.Detail = insErr.Error()
		} else if insSkip != SkipNone {
			result.Status = "skipped"
			if insSkip == SkipWaitingReconciliation || insSkip == "SOURCE_NOT_READY" || insSkip == "SOURCE_GAP_OR_PERMISSION" {
				result.Status = "waiting"
			}
		}
		if actions, aerr := e.st.CopyTrade().GetActionsForMessage(e.traderID, msg.MessageID); aerr == nil {
			for _, a := range actions {
				if a.Action != string(ins.Action) || (a.Symbol != ins.Symbol && a.Symbol != ins.Symbol+"USDT") || (ins.Direction != "" && a.Direction != string(ins.Direction)) {
					continue
				}
				var payload SourceInterpretation
				if json.Unmarshal([]byte(a.PayloadJSON), &payload) != nil || instructionSemantic(&payload) != instructionSemantic(ins) {
					continue
				}
				result.ActionIDs = append(result.ActionIDs, a.ID)
				if a.ContextID != "" {
					result.ContextIDs = append(result.ContextIDs, a.ContextID)
				}
				if rows, rerr := e.st.CopyTrade().GetOrdersForSignal(e.traderID, a.SignalID); rerr == nil {
					for _, o := range rows {
						if o.Symbol == a.Symbol && o.Direction == a.Direction {
							result.OrderIDs = append(result.OrderIDs, o.ID)
						}
					}
				}
			}
		}
		results = append(results, result)
		label := ""
		if multi {
			label = fmt.Sprintf("instruction %d/%d (%s %s): ", i+1, len(instructions), ins.Action, ins.Symbol)
		}
		switch {
		case insErr != nil:
			errCount++
			if firstErr == nil {
				firstErr = insErr
			}
			if multi {
				e.events.Error(traceID, signalID, msg.MessageID, EvExecutionError, label+insErr.Error(), nil)
			}
		case insSkip != SkipNone:
			if firstSkip == SkipNone {
				firstSkip, skipDetail = insSkip, insDetail
			}
			if multi {
				e.events.Info(traceID, signalID, msg.MessageID, EvSignalSkipped,
					fmt.Sprintf("%sskipped (%s): %s", label, insSkip, insDetail), nil)
			}
		default:
			executed++
		}
	}

	resultJSON, _ := json.Marshal(results)
	persist(map[string]interface{}{"instruction_results_json": string(resultJSON), "next_retry_at": nil})
	totalMs := time.Since(pipelineStart).Milliseconds()
	switch {
	case waiting:
		next := time.Now().UTC().Add(15 * time.Second)
		persist(map[string]interface{}{"status": "execution_wait", "next_retry_at": next, "skip_reason": string(SkipWaitingReconciliation), "total_ms": totalMs})
	case firstErr != nil:
		if multi && errCount > 1 {
			fail("execution", fmt.Errorf("%d/%d instructions failed, first: %w", errCount, len(instructions), firstErr))
		} else {
			fail("execution", firstErr)
		}
	case executed == 0:
		if firstSkip == SkipNone {
			// No instruction was actionable (pure status / chatter).
			firstSkip, skipDetail = SkipNotSignal, string(interp.Classification)
		}
		skip(firstSkip, skipDetail)
	default:
		persist(map[string]interface{}{
			"status": store.SignalStatusExecuted, "total_ms": totalMs,
		})
		summary := fmt.Sprintf("signal fully executed in %.1fs", float64(totalMs)/1000)
		if multi {
			summary = fmt.Sprintf("signal executed in %.1fs (%d/%d instructions applied)",
				float64(totalMs)/1000, executed, len(instructions))
		}
		e.events.Success(traceID, signalID, msg.MessageID, EvTradeUpdated, summary, totalMs, nil)
	}
	return
}

// processInstruction runs the instrument / validation / TTL gates and routes
// ONE instruction — exactly the pipeline stages 2-4 that the single-
// instruction path always had. Returns a terminal skip reason with detail,
// or an error.
func (e *Engine) processInstruction(traceID, signalID string, msg *store.DiscordMessage, interp *SourceInterpretation) (SkipReason, string, error) {
	if interp.IsActionable() {
		// TTL gate (per action class). Edits carry lifecycle updates; measure
		// freshness from the edit, not the original post.
		ttl := time.Duration(e.cfg.MgmtSignalTTLSeconds) * time.Second
		if interp.Action == ActionOpen || interp.Action == ActionAdd {
			ttl = time.Duration(e.cfg.OpenSignalTTLSeconds) * time.Second
		}
		refTime := msg.MessageTimestamp
		if msg.EditedAt != nil && msg.EditedAt.After(refTime) {
			refTime = *msg.EditedAt
		}
		if IsExpired(refTime, time.Now().UTC(), ttl) {
			return SkipExpired, fmt.Sprintf("signal age %v exceeds TTL %v", time.Since(refTime).Round(time.Second), ttl), nil
		}

	}

	if interp.IsActionable() && e.source != nil {
		if err := e.source.CheckExecution(e.traderID, msg, interp.Action == ActionOpen || interp.Action == ActionAdd); err != nil {
			return SkipReason(err.Error()), err.Error(), nil
		}
	}
	rules, ruleErr := e.rulesForSignal(signalID)
	if ruleErr != nil {
		return SkipNeedsContext, ruleErr.Error(), nil
	}
	if rules.SourceMode == "chroma" && interp.IsActionable() {
		if interp.Action == ActionOpen || interp.Action == ActionAdd {
			if err := e.notificationOpenGate(msg, interp, rules); err != nil {
				if errors.Is(err, errNotificationEnded) {
					return SkipReason("OPEN_SUPERSEDED"), err.Error(), nil
				}
				return SkipWaitingReconciliation, err.Error(), nil
			}
		} else {
			target, reason, err := e.notificationTarget(msg, interp, rules)
			if err != nil {
				if errors.Is(err, errNotificationPending) {
					return SkipWaitingReconciliation, err.Error(), nil
				}
				return SkipNeedsContext, err.Error(), nil
			}
			if target == nil {
				return SkipAlreadyFlat, reason, nil
			}
			interp.TradeReference.RootMessageID = target.RootMessageID
		}
	}
	// Resolve missing management symbols only from a unique tracked target.
	if interp.IsActionable() && interp.Action != ActionOpen && interp.Action != ActionAdd && interp.Symbol == "" {
		if ctx := e.correlateContext(interp, msg, "", signalID); ctx != nil {
			interp.Symbol = ctx.Symbol
			interp.Direction = Direction(ctx.Direction)
		}
	}
	// Instrument resolution & market reference.
	marketPrice := 0.0
	canonical := ""
	if interp.Symbol != "" {
		if c, rerr := ResolveInstrument(interp.Symbol); rerr == nil {
			canonical = c
			if mp, perr := e.exec.ex.GetMarketPrice(canonical); perr == nil {
				marketPrice = mp
			} else if interp.IsActionable() {
				// Surface the real cause: validation below will skip market
				// entries as UNSUPPORTED_PRICE_SPEC when there is no market
				// price, which reads like an AI problem when it is actually
				// an unlisted symbol or a price API failure (the PUMPFUN
				// incident: "PUMPFUN" resolved but the perp is PUMPUSDT).
				e.events.Warn(traceID, signalID, msg.MessageID, EvSignalSkipped,
					fmt.Sprintf("market price unavailable for %s (unlisted symbol or price API failure): %v", canonical, perr), nil)
			}
		} else if interp.IsActionable() {
			return SkipUnsupportedInstrument, rerr.Error(), nil
		}
	}

	// Validation & classification gates.
	skipReason, verr := ValidateInterpretation(interp, marketPrice)
	if verr != nil {
		return SkipNone, "", fmt.Errorf("validation failed: %w", verr)
	}
	if skipReason != SkipNone {
		return skipReason, "validation gate", nil
	}
	if !interp.IsActionable() {
		return SkipNotSignal, string(interp.Classification), nil
	}

	if interp.Action != ActionOpen && interp.Action != ActionAdd {
		target := e.correlateContext(interp, msg, canonical, signalID)
		if target == nil {
			return SkipNeedsContext, "target is missing or not unique", nil
		}
		if interp.RequiresAddFill && !target.HasAddFill && target.EntryPlanJSON != "" {
			if orders, err := e.exec.entryOrders(target); err == nil {
				confirmed := true
				for _, o := range orders {
					if err = e.exec.queryManaged(o); err != nil {
						confirmed = false
						break
					}
				}
				if confirmed {
					_ = e.exec.settleManagedFills(target, orders)
				}
			}
		}
		if interp.RequiresTPFill > 0 {
			e.exec.refreshTPProgress(traceID, target)
			if !confirmedTPLevel(target, interp.RequiresTPFill) {
				return SkipNeedsContext, "requires confirmed TP fill before immediate management", nil
			}
		}
		if interp.RequiresAddFill && !target.HasAddFill {
			return SkipNeedsContext, "requires a confirmed add fill for this trade", nil
		}
	}
	action, claimed, claimErr := e.claimInstruction(signalID, msg, interp, canonical)
	if claimErr != nil {
		return SkipNone, "", claimErr
	}
	if !claimed {
		if action.Status == "done" && action.SignalID == signalID {
			if action.Phase == "admission_revoked" {
				return SkipReason("ENTRY_ADMISSION_REVOKED"), action.Error, nil
			}
			return SkipNone, "existing action completed by recovery; no resubmission", nil
		}
		if action.Status == "uncertain" || action.Status == "executing" {
			return SkipWaitingReconciliation, "recorded action awaits order reconciliation; no resubmission", nil
		}
		return SkipDuplicate, "action already recorded; unresolved side effects are reconciled, never blindly repeated", nil
	}
	if action.ContextID != "" {
		e.updateSignal(signalID, map[string]interface{}{"trade_context_id": action.ContextID})
		if err := e.st.CopyTrade().UpdateAction(action.ID, map[string]interface{}{"phase": "execution"}); err != nil {
			return SkipNone, "", err
		}
	}
	// Route by action.
	var execErr error
	var finalSkip SkipReason
	switch interp.Action {
	case ActionOpen:
		finalSkip, execErr = e.routeOpen(traceID, signalID, msg, interp, canonical, marketPrice)
	case ActionAdd:
		// V1: ADD is treated as OPEN-if-flat, skip-if-position (documented limit).
		finalSkip, execErr = e.routeAdd(traceID, signalID, msg, interp, canonical, marketPrice)
	case ActionClose, ActionReduce:
		finalSkip, execErr = e.routeClose(traceID, signalID, msg, interp, canonical)
	case ActionCancel:
		finalSkip, execErr = e.routeCancel(traceID, signalID, msg, interp, canonical)
	case ActionUpdateSL:
		finalSkip, execErr = e.routeUpdateSL(traceID, signalID, msg, interp, canonical)
	case ActionUpdateTP:
		finalSkip, execErr = e.routeUpdateTP(traceID, signalID, msg, interp, canonical)
	default:
		finalSkip = SkipNotSignal
	}
	updates := map[string]interface{}{"status": "done", "phase": "completed"}
	if execErr != nil {
		updates["status"] = "uncertain"
		updates["phase"] = "submission_unknown"
		if interp.Action == ActionOpen || interp.Action == ActionAdd {
			persisted, pErr := e.st.CopyTrade().GetAction(action.ID)
			if pErr == nil && persisted != nil && persisted.ContextID == "" {
				updates["status"] = "preflight_rejected"
				updates["phase"] = "preflight"
			} else if pErr == nil && persisted != nil {
				rows, rErr := e.st.CopyTrade().GetOrdersForContext(e.traderID, persisted.ContextID)
				if rErr == nil && len(rows) > 0 {
					rejected := true
					for _, o := range rows {
						if o.ExecutedQty > 0 || (o.Status != "REJECTED" && o.Status != "ABANDONED" && o.Status != "PLANNED") {
							rejected = false
						}
					}
					if rejected {
						updates["status"] = "rejected"
						updates["phase"] = "exchange_rejected"
					}
				}
			}
		}
		updates["error"] = execErr.Error()
	} else if finalSkip != SkipNone {
		updates["status"] = "preflight_rejected"
		updates["phase"] = "preflight"
		updates["error"] = string(finalSkip)
	}
	if errors.Is(execErr, errEntryDeferred) {
		updates["status"] = "executing"
		updates["phase"] = "waiting_admission"
	}
	if errors.Is(execErr, errEntryRevoked) {
		updates["status"] = "done"
		updates["phase"] = "admission_revoked"
	}
	if err := e.st.CopyTrade().UpdateAction(action.ID, updates); err != nil {
		return SkipNone, "", err
	}
	if errors.Is(execErr, errEntryDeferred) {
		return SkipWaitingReconciliation, execErr.Error(), nil
	}
	if errors.Is(execErr, errEntryRevoked) {
		return SkipReason("ENTRY_ADMISSION_REVOKED"), execErr.Error(), nil
	}
	if execErr != nil && rules.SourceMode == "chroma" && interp.Action != ActionOpen && interp.Action != ActionAdd {
		return SkipWaitingReconciliation, "recorded management action awaits reconciliation: " + execErr.Error(), nil
	}
	return finalSkip, "execution gate", execErr
}

type pipelineTimings struct {
	mediaMs  int64
	promptMs int64
	llmMs    int64
	imageErr string
}

// interpret builds the prompt (with context and optional images) and runs the LLM.
// dryRun skips all persistence (AI run records, events): used by the replay tool
// so accuracy testing never pollutes real stats or the event stream.
func (e *Engine) interpret(traceID, signalID string, msg *store.DiscordMessage, isEdit, dryRun bool) (*SourceInterpretation, *store.CopyTradeAIRun, pipelineTimings, error) {
	var t pipelineTimings
	rules, rulesErr := e.rulesForSignal(signalID)
	if rulesErr != nil {
		return nil, nil, t, rulesErr
	}

	// Context: active trades, recent signals, reply/linked messages.
	activeCtxs, _ := e.st.CopyTrade().GetActiveContexts(e.traderID)
	filtered := activeCtxs[:0]
	for _, c := range activeCtxs {
		if containsString(rules.SourceChannels, c.ChannelID) || c.ChannelID == msg.ChannelID {
			filtered = append(filtered, c)
		}
	}
	activeCtxs = filtered
	var recent []*store.CopyTradeSignal
	if rules.SignalContextEnabled || (rules.Version < 3 && e.cfg.SignalContextEnabled) {
		since := time.Now().UTC().AddDate(0, 0, -rules.ContextDays)
		recent, _ = e.st.CopyTrade().GetContextSignalsForSources(e.traderID, rules.SourceChannels, since, 20)
	}

	var replyMsg *store.DiscordMessage
	if msg.ReplyToMessageID != "" {
		replyMsg = e.lookupMessageForInterpret(msg.ChannelID, msg.ReplyToMessageID, dryRun)
	}
	var linked []*store.DiscordMessage
	for _, link := range discord.ExtractMessageLinks(msg.Content) {
		if link.MessageID == msg.MessageID {
			continue
		}
		if lm := e.lookupMessageForInterpret(link.ChannelID, link.MessageID, dryRun); lm != nil {
			linked = append(linked, lm)
		}
		if len(linked) >= 3 {
			break
		}
	}

	sources := e.messageSources(msg, rules.Profile)
	// Images.
	mediaStart := time.Now()
	var imageParts []mcp.ContentPart
	imageCount := 0
	var imageErr string
	if rules.ParseImages || (rules.Version < 3 && e.cfg.ParseImages) {
		imageParts, imageCount, imageErr = e.collectImages(traceID, signalID, msg, dryRun, rules.Profile)
	}
	t.mediaMs = time.Since(mediaStart).Milliseconds()
	t.imageErr = imageErr

	// Positions snapshot (optional, non-fatal).
	var positions []store.PositionSnapshot
	if rules.SendPositionSnapshot || (rules.Version < 3 && e.cfg.SendPositionSnapshot) {
		if raw, err := e.exec.ex.GetPositions(); err == nil {
			for _, p := range raw {
				qty, _ := p["positionAmt"].(float64)
				if qty < 0 {
					qty = -qty
				}
				if qty == 0 {
					continue
				}
				sym, _ := p["symbol"].(string)
				side, _ := p["side"].(string)
				entry, _ := p["entryPrice"].(float64)
				upnl, _ := p["unRealizedProfit"].(float64)
				positions = append(positions, store.PositionSnapshot{
					Symbol: sym, Side: side, PositionAmt: qty, EntryPrice: entry, UnrealizedProfit: upnl,
				})
			}
		}
	}

	promptStart := time.Now()
	userPrompt := BuildUserPrompt(PromptInput{
		Message:      msg,
		EmbedsText:   discord.FlattenEmbeds(discord.ParseStoredEmbeds(msg.EmbedsJSON)),
		IsEdit:       isEdit,
		ImageCount:   imageCount,
		ChannelNotes: rules.Notes,
		MessageRules: rules,
		Sources:      sources, InterpretationProfile: rules.Profile,
		ReplyToMessage: replyMsg,
		LinkedMessages: linked,
		ActiveContexts: activeCtxs,
		RecentSignals:  recent,
		Positions:      positions,
	})
	t.promptMs = time.Since(promptStart).Milliseconds()

	// Build the LLM request (multimodal when images are present).
	messages := []mcp.Message{mcp.NewSystemMessage(SystemPrompt)}
	if len(imageParts) > 0 {
		parts := append([]mcp.ContentPart{mcp.NewTextPart(userPrompt)}, imageParts...)
		messages = append(messages, mcp.NewMultimodalUserMessage(parts...))
	} else {
		messages = append(messages, mcp.NewUserMessage(userPrompt))
	}

	run := &store.CopyTradeAIRun{
		TraderID:          e.traderID,
		ChannelID:         msg.ChannelID,
		MessageID:         msg.MessageID,
		Model:             e.modelID,
		Provider:          e.provider,
		PromptVersion:     PromptVersion,
		RulesSnapshotJSON: rules.Snapshot(),
		SystemPrompt:      SystemPrompt,
		InputPrompt:       userPrompt,
		ImageCount:        imageCount,
		StartedAt:         time.Now().UTC(),
	}

	llmStart := time.Now()
	var raw string
	var callErr error
	known := InterpretChromaNotification(msg, rules)
	if known == nil {
		known = InterpretKnownSource(msg, sources, rules.Profile)
	}
	if known != nil {
		b, _ := json.Marshal(known)
		raw = string(b)
		run.Model = "deterministic:" + rules.Profile
		run.Provider = "deterministic"
		if rules.SourceMode == "chroma" {
			run.Model = "deterministic:chroma"
		}
	} else {
		if !dryRun {
			e.events.Info(traceID, signalID, msg.MessageID, EvAIRequest,
				fmt.Sprintf("LLM request (%s, %d images, prompt %d chars)", e.modelID, imageCount, len(userPrompt)), nil)
		}
		raw, callErr = e.llm.CallWithRequest(&mcp.Request{Messages: messages})
	}
	t.llmMs = time.Since(llmStart).Milliseconds()

	run.FinishedAt = time.Now().UTC()
	run.DurationMs = t.llmMs
	run.RawResponse = raw

	if callErr != nil {
		run.Error = callErr.Error()
		if !dryRun {
			if dbErr := e.st.CopyTrade().CreateAIRun(run); dbErr != nil {
				return nil, run, t, fmt.Errorf("persist AI run: %w", dbErr)
			}
			e.events.Error(traceID, signalID, msg.MessageID, EvAIError, callErr.Error(), nil)
		}
		return nil, run, t, fmt.Errorf("LLM call failed: %w", callErr)
	}

	interp, perr := ParseInterpretation(raw)
	if perr != nil {
		run.Error = perr.Error()
		if !dryRun {
			if dbErr := e.st.CopyTrade().CreateAIRun(run); dbErr != nil {
				return nil, run, t, fmt.Errorf("persist AI run: %w", dbErr)
			}
			e.events.Error(traceID, signalID, msg.MessageID, EvAIError, "parse failed: "+perr.Error(), nil)
		}
		if strings.TrimSpace(raw) == "" {
			return nil, run, t, fmt.Errorf("LLM empty response")
		}
		return nil, run, t, fmt.Errorf("interpretation parse failed: %w", perr)
	}
	interp = ApplyNotificationPolicy(interp, msg, rules)
	interp = ApplySourcePolicy(interp, msg, sources, rules.Profile)
	interp = ApplyMessageRules(interp, msg, sources, rules)
	for _, ins := range interp.Flatten() {
		if ins.ActionEvidence != nil && strings.HasSuffix(ins.ActionEvidence.SourceID, ":image") {
			for _, part := range imageParts {
				if part.Text == "Current image source_id="+ins.ActionEvidence.SourceID {
					ins.EvidenceVerified = true
				}
			}
		}
	}
	parsedJSON, _ := json.Marshal(interp)
	run.ParsedJSON = string(parsedJSON)
	if !dryRun {
		if dbErr := e.st.CopyTrade().CreateAIRun(run); dbErr != nil {
			return nil, run, t, fmt.Errorf("persist AI run: %w", dbErr)
		}
		if known != nil {
			e.events.Success(traceID, signalID, msg.MessageID, EvRuleMatched,
				"deterministic interpretation ("+run.Model+")", t.llmMs, map[string]interface{}{"model": run.Model})
		} else {
			e.events.Success(traceID, signalID, msg.MessageID, EvAIParsed,
				fmt.Sprintf("LLM responded in %.1fs", float64(t.llmMs)/1000), t.llmMs, map[string]interface{}{"model": run.Model})
		}
	}
	return interp, run, t, nil
}

// collectImages downloads message images and converts them to data-URL parts.
// Failures degrade to text-only with a warning (never fail the signal).
func (e *Engine) collectImages(traceID, signalID string, msg *store.DiscordMessage, dryRun bool, profiles ...string) ([]mcp.ContentPart, int, string) {
	if e.source == nil {
		return nil, 0, "discord client unavailable"
	}
	client := e.source.Client()
	if client == nil {
		return nil, 0, "discord client unavailable"
	}
	var parts []mcp.ContentPart
	count := 0
	var lastErr string
	failed := 0
	available := 0
	for _, att := range MediaSources(msg, e.messageSources(msg, profiles...)) {
		if att.Role != "current" {
			continue
		} // historical media cannot authorize actions
		available++
		if count >= 3 { // bound prompt size
			continue
		}
		img, err := discord.DownloadImage(client, att.URL)
		if err != nil {
			failed++
			lastErr = err.Error()
			if !dryRun {
				e.events.Warn(traceID, signalID, msg.MessageID, EvMessageSkipped,
					fmt.Sprintf("image download failed, degrading to text-only: %v", err), nil)
			}
			continue
		}
		data, err := discord.ReadImageBytes(img)
		if err != nil {
			failed++
			lastErr = err.Error()
			continue
		}
		dataURL := "data:" + img.MimeType + ";base64," + base64.StdEncoding.EncodeToString(data)
		parts = append(parts, mcp.NewTextPart("Current image source_id="+att.SourceID+":image"), mcp.NewImagePart(dataURL))
		count++
	}
	var imageErr string
	if failed > 0 && count == 0 && available > 0 {
		imageErr = fmt.Sprintf("%d/%d images failed to download: %s", failed, available, lastErr)
	} else if failed > 0 {
		imageErr = fmt.Sprintf("%d/%d images failed to download: %s", failed, available, lastErr)
	}
	return parts, count, imageErr
}

// lookupMessage reads a message from the store, falling back to the API.
func (e *Engine) lookupMessage(channelID, messageID string) *store.DiscordMessage {
	return e.lookupMessageForInterpret(channelID, messageID, false)
}
func (e *Engine) lookupMessageForInterpret(channelID, messageID string, dryRun bool) *store.DiscordMessage {
	if m, err := e.st.DiscordMessage().GetByMessageID(channelID, messageID); err == nil && m != nil {
		return m
	}
	if e.source == nil {
		return nil
	}
	client := e.source.Client()
	if client == nil {
		return nil
	}
	apiMsg, err := client.GetMessage(channelID, messageID)
	if err != nil {
		return nil
	}
	rec, err := discord.ToStoreMessage(apiMsg, channelID)
	if err != nil {
		return nil
	}
	// Persist as baseline so it is never dispatched as a fresh signal.
	if !dryRun {
		_ = e.st.DiscordMessage().MarkBaseline(rec)
	}
	return rec
}

// --- action routing ---

// correlateContext finds the trade context a management signal refers to.
// Priority: explicit root message ref > reply target > symbol+direction.
func (e *Engine) correlateContext(interp *SourceInterpretation, msg *store.DiscordMessage, canonical string, signalIDs ...string) *store.CopyTradeContext {
	contexts, err := e.st.CopyTrade().GetActiveContexts(e.traderID)
	if err != nil {
		return nil
	}
	roots := map[string]bool{}
	verifiedRoots := map[string]bool{}
	if msg.ReplyToMessageID != "" {
		verifiedRoots[msg.ReplyToMessageID] = true
	}
	unresolvedQuote := false
	verifiedReference := msg.ReplyToMessageID != ""
	for _, root := range []string{interp.TradeReference.RootMessageID, interp.TradeReference.ReplyMessageID, msg.ReplyToMessageID} {
		if root != "" {
			roots[root] = true
		}
	}
	rules := e.cfg.MessageRules()
	if len(signalIDs) > 0 {
		var err error
		rules, err = e.rulesForSignal(signalIDs[0])
		if err != nil {
			return nil
		}
	}
	if rules.SourceMode == "chroma" {
		ctx, _, err := e.notificationTarget(msg, interp, rules)
		if err != nil {
			return nil
		}
		return ctx
	}
	if interp.RequiresOriginalCard {
		var match *store.CopyTradeContext
		for _, c := range contexts {
			if c.ChannelID == msg.ChannelID && c.RootMessageID == msg.MessageID && c.Symbol == canonical && (interp.Direction == "" || c.Direction == string(interp.Direction)) {
				if match != nil {
					return nil
				}
				match = c
			}
		}
		return match
	}
	for _, source := range e.messageSources(msg, rules.Profile) {
		if source.Role == "reference" && !source.Image {
			if source.MessageID != "" {
				roots[source.MessageID] = true
				verifiedRoots[source.MessageID] = true
				verifiedReference = true
			} else if sourceOpen.MatchString(source.Text) {
				unresolvedQuote = true
			}
		}
	}
	for _, link := range discord.ExtractMessageLinks(msg.Content) {
		if link.ChannelID == msg.ChannelID {
			roots[link.MessageID] = true
			verifiedRoots[link.MessageID] = true
			verifiedReference = true
		}
	}
	if unresolvedQuote && !verifiedReference {
		return nil
	}
	if root := interp.TradeReference.RootMessageID; root != "" && len(verifiedRoots) > 0 && !verifiedRoots[root] {
		return nil
	}
	// An explicit target cannot contradict a native reply. Do not let the
	// union of two conflicting references select whichever trade is active.
	if root := interp.TradeReference.RootMessageID; root != "" && msg.ReplyToMessageID != "" && msg.ReplyToMessageID != root {
		for _, c := range contexts {
			if c.ChannelID == msg.ChannelID && c.RootMessageID == msg.ReplyToMessageID {
				return nil
			}
		}
	}
	if len(roots) == 0 {
		for _, c := range contexts {
			if c.RootMessageID == msg.MessageID {
				roots[msg.MessageID] = true
			}
		}
	}
	var match *store.CopyTradeContext
	for _, c := range contexts {
		if c.ChannelID != msg.ChannelID || (canonical != "" && c.Symbol != canonical) || (interp.Direction != "" && c.Direction != string(interp.Direction)) {
			continue
		}
		if len(roots) > 0 && !roots[c.RootMessageID] {
			continue
		}
		if interp.TradeReference.RootMessageID != "" && c.RootMessageID != interp.TradeReference.RootMessageID {
			continue
		}
		if len(roots) == 0 && canonical == "" {
			continue
		}
		if match != nil {
			return nil
		}
		match = c
	}
	return match
}

func (e *Engine) routeOpen(traceID, signalID string, msg *store.DiscordMessage, interp *SourceInterpretation, canonical string, marketPrice float64) (SkipReason, error) {
	rules, rulesErr := e.rulesForSignal(signalID)
	if rulesErr != nil {
		return SkipNone, rulesErr
	}
	if e.cfg.Paused {
		return SkipPaused, nil
	}
	if managed, ok := e.exec.ex.(types.ManagedOrderTrader); ok {
		if _, err := managed.MarketRules(canonical); err != nil {
			return SkipUnsupportedInstrument, fmt.Errorf("contract capability: %w", err)
		}
	}
	// Duplicate protection: an active trade on this symbol+direction already exists.
	if e.cfg.DuplicateOpenProtection {
		existing, lookupErr := e.st.CopyTrade().GetActiveContextBySymbol(e.traderID, canonical, string(interp.Direction))
		if lookupErr != nil {
			return SkipNone, lookupErr
		}
		if existing != nil {
			e.events.Warn(traceID, signalID, msg.MessageID, EvSignalSkipped,
				fmt.Sprintf("duplicate open blocked: active trade %s exists (state %s)", existing.ID, existing.State), nil)
			return SkipDuplicate, nil
		}
	}
	// Max concurrent trades.
	n, countErr := e.st.CopyTrade().CountActiveByTrader(e.traderID)
	if countErr != nil {
		return SkipNone, countErr
	}
	if int(n) >= e.cfg.MaxOpenPositions {
		return SkipMaxPositions, nil
	}
	if marketPrice <= 0 {
		return SkipNone, fmt.Errorf("market price unavailable for %s", canonical)
	}

	// Entry decision (direction-aware: favorable prices enter at market,
	// adverse ones tolerate the configured threshold, then rest as limits).
	if len(interp.EntryOrders) == 0 {
		return SkipUnsupportedPriceSpec, nil
	}
	shape, skip, err := resolveEntryPlan(interp.Direction, interp.EntryOrders, marketPrice, e.cfg.PriceOffsetPctFor(canonical), e.cfg.LimitToMarketWithin, rules.EntryPolicy)
	if err != nil {
		if skip == SkipRiskRejected || skip == SkipNeedsContext {
			e.events.Warn(traceID, signalID, msg.MessageID, EvSignalSkipped, err.Error(), nil)
			return skip, nil
		}
		return skip, err
	}
	if shape.SplitReference > 0 {
		if e.cfg.RiskMode != RiskModeByLoss {
			return SkipRiskRejected, fmt.Errorf("multi-leg entry requires by_loss")
		}
		if _, ok := e.exec.ex.(types.ManagedOrderTrader); !ok {
			return SkipUnsupportedInstrument, fmt.Errorf("multi-leg entry requires managed orders")
		}
	}
	entryType, entryPrice := shape.Decision.OrderType, shape.Decision.EntryPrice

	// Resolve SL / TP hard prices against the entry reference.
	slPrice := resolveHardPrice(interp.StopLossLevels[0].Price, entryPrice)
	if slPrice <= 0 {
		return SkipUnsupportedPriceSpec, nil
	}

	requestedSL := slPrice
	slPrice, _, _, err = e.exec.normalizeStop(canonical, slPrice)
	if err != nil {
		return SkipRiskRejected, err
	}
	// Setup-invalidation guard: a market that has already traded through the
	// author's stop is a broken setup, not a favorable entry — entering now
	// would open a position whose stop triggers immediately.
	if entryType == EntryPlanMarket {
		if (interp.Direction == DirectionLong && marketPrice <= slPrice) ||
			(interp.Direction == DirectionShort && marketPrice >= slPrice) {
			e.events.Warn(traceID, signalID, msg.MessageID, EvSignalSkipped,
				fmt.Sprintf("market %.8g already through stop loss %.8g — setup invalidated, not entering", marketPrice, slPrice), nil)
			return SkipSanityCheck, nil
		}
	}
	if interp.StopLossLevels[0].Conditional != "" {
		e.events.Warn(traceID, signalID, msg.MessageID, EvSignalClassified,
			"author uses a conditional stop ("+interp.StopLossLevels[0].Conditional+"); executing the hard price", nil)
	}
	// Resolve TP levels keeping prices paired with their level structs (a
	// skipped middle spec must not misalign explicit ratios), then cap the
	// ladder at the executor's capacity: the nearest targets are kept, the
	// far ones dropped — never reject the whole trade over extra TP levels.
	var tpPrices, originalTPPrices []float64
	var tpOrdinals []int
	var tpLevels []TPLevel
	for ordinal, tp := range interp.TakeProfitLevels {
		tp.Ordinal = ordinal + 1
		p := resolveHardPrice(tp.Price, entryPrice)
		if p <= 0 {
			e.events.Warn(traceID, signalID, msg.MessageID, EvSignalClassified,
				fmt.Sprintf("skipping unsupported TP spec %s", tp.Price.Type), nil)
			continue
		}
		originalTPPrices = append(originalTPPrices, p)
		tpPrices = append(tpPrices, p)
		tpLevels = append(tpLevels, tp)
	}
	var droppedTPs []float64
	tpPrices, tpLevels, droppedTPs = CapTPLadder(tpPrices, tpLevels, interp.Direction)
	if len(droppedTPs) > 0 {
		e.events.Warn(traceID, signalID, msg.MessageID, EvSignalClassified,
			fmt.Sprintf("signal has %d TP levels, keeping the %d nearest and dropping %v (executor cap)",
				len(tpPrices)+len(droppedTPs), MaxTPLevels, droppedTPs), nil)
	}
	for _, tp := range tpLevels {
		tpOrdinals = append(tpOrdinals, tp.Ordinal)
	}
	defaults, _ := ParseTPRatios(e.cfg.DefaultTPRatios)
	tpRatios, err := AllocateTPRatios(tpLevels, defaults)
	if err != nil {
		return SkipNone, fmt.Errorf("TP allocation failed: %w", err)
	}

	// Deterministic sizing.
	riskStart := time.Now()
	equity, available, balanceErr := e.accountBalances()
	if balanceErr != nil {
		return SkipNone, balanceErr
	}
	if available <= 0 {
		return SkipNone, fmt.Errorf("no available margin for new risk")
	}
	leverage := e.cfg.LeverageFor(canonical)
	sizing, err := ComputePositionSize(SizingInput{
		RiskMode:               e.cfg.RiskMode,
		RiskAmountUSD:          e.cfg.RiskAmountUSD,
		EquityUSD:              equity,
		EntryPrice:             entryPrice,
		StopLossPrice:          slPrice,
		Leverage:               leverage,
		MaxPositionNotionalUSD: e.cfg.MaxPositionNotionalUSD,
		AvailableMarginUSD:     available,
	})
	riskMs := time.Since(riskStart).Milliseconds()
	if err != nil {
		e.events.Warn(traceID, signalID, msg.MessageID, EvRiskRejected, err.Error(), nil)
		return SkipRiskRejected, nil
	}
	e.updateSignal(signalID, map[string]interface{}{"risk_calc_ms": riskMs})
	sizingJSON, _ := json.Marshal(sizing)
	e.events.Info(traceID, signalID, msg.MessageID, EvQuantityPlan,
		fmt.Sprintf("qty=%.8g notional=$%.2f margin=$%.2f risk=$%.2f constraints=%v",
			sizing.FinalQuantity, sizing.NotionalUSD, sizing.EstimatedMarginUSD,
			sizing.EstimatedRiskUSD, sizing.AppliedConstraints),
		map[string]interface{}{"sizing": json.RawMessage(sizingJSON)})

	plan := &OpenPlan{
		Symbol:            canonical,
		RulesSnapshotJSON: rules.Snapshot(),
		RawSymbol:         interp.Symbol,
		Direction:         interp.Direction,
		EntryType:         entryType,
		EntryPrice:        entryPrice,
		Quantity:          sizing.FinalQuantity,
		Leverage:          leverage,
		StopLoss:          slPrice, RequestedStopLoss: requestedSL,
		TPPrices:   tpPrices,
		TPRatios:   tpRatios,
		TPOrdinals: tpOrdinals, TPOriginalPrices: originalTPPrices,
		Sizing:     sizing,
		RootMsgID:  msg.MessageID,
		ChannelID:  msg.ChannelID,
		RiskBudget: 0, MaxNotional: e.cfg.MaxPositionNotionalUSD, AvailableMargin: available,
		EntryTimeout: time.Duration(e.cfg.EntryTimeoutMinutes) * time.Minute,
	}
	if e.cfg.RiskMode == RiskModeByLoss {
		plan.RiskBudget = e.cfg.RiskAmountUSD
	}
	refTime := msg.MessageTimestamp
	if msg.EditedAt != nil && msg.EditedAt.After(refTime) {
		refTime = *msg.EditedAt
	}
	if e.cfg.OpenSignalTTLSeconds > 0 {
		plan.SignalExpiresAt = refTime.Add(time.Duration(e.cfg.OpenSignalTTLSeconds) * time.Second)
	}
	plan.SplitReference = shape.SplitReference
	plan.EntryDecisionJSON, _ = json.Marshal(shape)
	if shape.Origin == "explicit_market_limit" && ((interp.Direction == DirectionLong && (shape.SplitReference <= slPrice || shape.SplitReference >= marketPrice)) || (interp.Direction == DirectionShort && (shape.SplitReference >= slPrice || shape.SplitReference <= marketPrice))) {
		return SkipRiskRejected, fmt.Errorf("second limit must remain between stop and market")
	}

	// Author conditions are committed with the entry intent, before a fill or
	// protection error can interrupt the open saga and expose global TP1 rules.
	for _, rule := range interp.ConditionalRules {
		if rule.Condition == ConditionTPFilled && rule.Action == ActionUpdateSL && (rule.Price.Type == PriceEntry || rule.Price.Type == PriceBreakeven) {
			plan.BreakevenTPLevel = rule.ConditionLevel
			if plan.BreakevenTPLevel <= 0 {
				plan.BreakevenTPLevel = 1
			}
		}
	}

	if e.source != nil {
		if err := e.source.CheckExecution(e.traderID, msg, true); err != nil {
			return SkipReason(err.Error()), nil
		}
	}
	if rules.SourceMode == "chroma" {
		if err := e.notificationOpenGate(msg, interp, rules); err != nil {
			return SkipNeedsContext, nil
		}
	}
	submitStart := time.Now()
	ctx, err := e.exec.ExecuteOpen(traceID, signalID, plan)
	e.updateSignal(signalID, map[string]interface{}{
		"exchange_submit_ms": time.Since(submitStart).Milliseconds(),
	})
	if ctx != nil {
		e.updateSignal(signalID, map[string]interface{}{"trade_context_id": ctx.ID})
		_ = e.st.CopyTrade().BindOpenAction(signalID, canonical, string(interp.Direction), ctx.ID)
	}
	if err != nil {
		return SkipNone, err
	}
	if ctx.State == string(StateOpen) {
		e.events.Success(traceID, signalID, msg.MessageID, EvTradeOpened,
			fmt.Sprintf("%s %s opened: qty=%.8g @ %.8g, SL %.8g, %d TPs",
				canonical, interp.Direction, ctx.Quantity, ctx.AvgFillPrice, slPrice, len(tpPrices)), 0, nil)
	}

	// Author-stated conditional rules: the supported subset ("after TP fill,
	// move SL to entry/breakeven") is armed on the context and enforced by the
	// reconciler; everything else is logged explicitly as unsupported instead
	// of being dropped silently.
	return SkipNone, e.applyConditionalRules(traceID, signalID, msg.MessageID, interp, ctx)
}

// applyConditionalRules arms supported author-stated follow-up rules on the
// trade context. Supported in V1: TP_FILLED => UPDATE_SL to ENTRY/BREAKEVEN.
func (e *Engine) applyConditionalRules(traceID, signalID, messageID string, interp *SourceInterpretation, ctx *store.CopyTradeContext) error {
	for _, rule := range interp.ConditionalRules {
		supported := rule.Condition == ConditionTPFilled &&
			rule.Action == ActionUpdateSL &&
			(rule.Price.Type == PriceEntry || rule.Price.Type == PriceBreakeven)
		if !supported {
			e.events.Warn(traceID, signalID, messageID, EvSignalClassified,
				fmt.Sprintf("author conditional rule not supported in V1, ignored: %s(level %d) -> %s %s",
					rule.Condition, rule.ConditionLevel, rule.Action, rule.Price.Type), nil)
			continue
		}
		if rule.ConditionLevel <= 0 {
			rule.ConditionLevel = 1
		}
		if err := e.exec.persistContext(ctx, map[string]interface{}{"breakeven_after_tp": true, "breakeven_tp_level": rule.ConditionLevel}); err != nil {
			return err
		}
		e.events.Info(traceID, signalID, messageID, EvSignalClassified,
			fmt.Sprintf("author rule armed: SL moves to entry after TP%d fill", rule.ConditionLevel), nil)
	}
	return nil
}

// routeAdd: V1 treats ADD conservatively — only executes when there is no
// active trade yet (then it behaves like OPEN); otherwise it is skipped with
// an explicit event, never silently scaled.
func (e *Engine) routeAdd(traceID, signalID string, msg *store.DiscordMessage, interp *SourceInterpretation, canonical string, marketPrice float64) (SkipReason, error) {
	if existing, _ := e.st.CopyTrade().GetActiveContextBySymbol(e.traderID, canonical, string(interp.Direction)); existing != nil {
		e.events.Info(traceID, signalID, msg.MessageID, EvSignalSkipped,
			"ADD to existing position is not executed in V1 (risk policy); logged only", nil)
		return SkipDuplicate, nil
	}
	return e.routeOpen(traceID, signalID, msg, interp, canonical, marketPrice)
}

func (e *Engine) routeClose(traceID, signalID string, msg *store.DiscordMessage, interp *SourceInterpretation, canonical string) (SkipReason, error) {
	ctx := e.correlateContext(interp, msg, canonical, signalID)
	if ctx == nil {
		e.events.Info(traceID, signalID, msg.MessageID, EvSignalSkipped,
			fmt.Sprintf("no tracked trade for %s (we never followed this open)", canonical), nil)
		return SkipNoPosition, nil
	}
	if ok, reason := ActionApplicable(TradeState(ctx.State), ActionClose); !ok {
		return reason, nil
	}
	// Close of an unfilled entry = cancel.
	if ctx.State == string(StateEntryPending) {
		if _, err := e.exec.ExecuteCancel(traceID, signalID, ctx); err != nil {
			return SkipNone, err
		}
		if ctx.Quantity <= 0 || ctx.OpenedAt == nil {
			return SkipNone, nil
		}
	}
	ratio := 100.0
	if interp.Action == ActionReduce || interp.CloseMode == CloseModePartial {
		if interp.CloseRatio != nil {
			ratio = *interp.CloseRatio
		} else {
			rules, err := e.rulesForSignal(signalID)
			if err != nil {
				return SkipNone, err
			}
			ratio = rules.ReduceRatio // percentage of remaining position
			e.events.Warn(traceID, signalID, msg.MessageID, EvSignalClassified,
				fmt.Sprintf("partial close without stated portion; defaulting to %.2f%%", ratio), nil)
		}
	}
	return e.exec.ExecuteClose(traceID, signalID, ctx, ratio)
}

func (e *Engine) routeCancel(traceID, signalID string, msg *store.DiscordMessage, interp *SourceInterpretation, canonical string) (SkipReason, error) {
	ctx := e.correlateContext(interp, msg, canonical, signalID)
	if ctx == nil {
		return SkipNoPosition, nil
	}
	if ok, reason := ActionApplicable(TradeState(ctx.State), ActionCancel); !ok && !ctx.EntryWorking && !(ctx.ExecutionVersion > 0 && !ctx.EntryDisabled) {
		return reason, nil
	}
	return e.exec.ExecuteCancel(traceID, signalID, ctx)
}

func (e *Engine) routeUpdateSL(traceID, signalID string, msg *store.DiscordMessage, interp *SourceInterpretation, canonical string) (SkipReason, error) {
	ctx := e.correlateContext(interp, msg, canonical, signalID)
	if ctx == nil {
		return SkipNoPosition, nil
	}
	if ok, reason := ActionApplicable(TradeState(ctx.State), ActionUpdateSL); !ok {
		return reason, nil
	}
	if len(interp.ConditionalRules) > 0 {
		for _, rule := range interp.ConditionalRules {
			if rule.Condition != ConditionTPFilled || rule.Action != ActionUpdateSL || (rule.Price.Type != PriceEntry && rule.Price.Type != PriceBreakeven) {
				return SkipUnsupportedPriceSpec, nil
			}
		}
		return SkipNone, e.applyConditionalRules(traceID, signalID, msg.MessageID, interp, ctx)
	}
	spec := interp.StopLossLevels[0].Price
	entryRef := ctx.AvgFillPrice
	if entryRef <= 0 {
		entryRef = ctx.PlannedEntryPrice
	}
	newPrice := resolveContextPrice(spec, ctx, entryRef)
	if newPrice <= 0 {
		return SkipUnsupportedPriceSpec, nil
	}
	return e.exec.ExecuteUpdateSLSpec(traceID, signalID, ctx, spec)
}

func (e *Engine) routeUpdateTP(traceID, signalID string, msg *store.DiscordMessage, interp *SourceInterpretation, canonical string) (SkipReason, error) {
	ctx := e.correlateContext(interp, msg, canonical, signalID)
	if ctx == nil {
		return SkipNoPosition, nil
	}
	if ok, reason := ActionApplicable(TradeState(ctx.State), ActionUpdateTP); !ok {
		return reason, nil
	}
	entryRef := ctx.AvgFillPrice
	var prices []float64
	var levels []TPLevel
	for _, tp := range interp.TakeProfitLevels {
		if p := resolveHardPrice(tp.Price, entryRef); p > 0 {
			prices = append(prices, p)
			levels = append(levels, tp)
		}
	}
	if len(prices) == 0 {
		return SkipUnsupportedPriceSpec, nil
	}
	var droppedTPs []float64
	prices, levels, droppedTPs = CapTPLadder(prices, levels, Direction(ctx.Direction))
	if len(droppedTPs) > 0 {
		e.events.Warn(traceID, signalID, msg.MessageID, EvSignalClassified,
			fmt.Sprintf("TP update has %d levels, keeping the %d nearest and dropping %v (executor cap)",
				len(prices)+len(droppedTPs), MaxTPLevels, droppedTPs), nil)
	}
	defaults, _ := ParseTPRatios(e.cfg.DefaultTPRatios)
	ratios, err := AllocateTPRatios(levels, defaults)
	if err != nil {
		return SkipNone, err
	}
	return e.exec.ExecuteUpdateTP(traceID, signalID, ctx, prices, ratios)
}

// --- helpers ---

// accountBalances reads equity and available margin (best effort).
func (e *Engine) accountBalances() (equity, available float64, err error) {
	return readAccountBalances(e.exec.ex)
}

func readAccountBalances(ex types.Trader) (equity, available float64, err error) {
	account, err := ex.GetBalance()
	if err != nil {
		return 0, 0, fmt.Errorf("balance lookup failed: %w", err)
	}
	for _, key := range []string{"totalEquity", "totalWalletBalance", "total_equity", "totalMarginBalance"} {
		if v, ok := account[key].(float64); ok && finite(v) && v > 0 {
			equity = v
			break
		}
	}
	known := false
	for _, key := range []string{"availableBalance", "available_balance", "availableMargin"} {
		if v, ok := account[key].(float64); ok && finite(v) && v >= 0 {
			available = v
			known = true
			break
		}
	}
	if !known {
		return equity, 0, fmt.Errorf("available margin unknown")
	}
	return equity, available, nil
}

func (e *Engine) updateSignal(signalID string, updates map[string]interface{}) {
	if err := e.st.CopyTrade().UpdateSignal(signalID, updates); err != nil {
		logger.Errorf("[CopyTrade %s] signal update failed: %v", e.traderID, err)
	}
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
