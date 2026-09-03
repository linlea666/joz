package copytrader

import (
	"encoding/base64"
	"encoding/json"
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
	Poller     *discord.PollerManager
}

// Engine runs copy trading for ONE trader: it consumes channel messages from
// the poller, interprets them with the LLM, applies deterministic risk rules
// and executes through the exchange. All message handling for the trader is
// strictly serial (mu) so lifecycle order can never invert.
type Engine struct {
	traderID   string
	traderName string
	userID     string
	cfg        *CopyTradingConfig
	st         *store.Store
	llm        mcp.AIClient
	modelID    string
	provider   string
	poller     *discord.PollerManager
	exec       *Executor
	events     *EventLogger

	mu      sync.Mutex // serializes message handling per trader
	stateMu sync.Mutex
	running bool
	stopCh  chan struct{}
	wg      sync.WaitGroup
}

// NewEngine creates the engine (Start must be called to begin processing).
func NewEngine(p EngineParams) *Engine {
	events := NewEventLogger(p.Store, p.TraderID, p.Config.PrimaryChannelID)
	return &Engine{
		traderID:   p.TraderID,
		traderName: p.TraderName,
		userID:     p.UserID,
		cfg:        p.Config,
		st:         p.Store,
		llm:        p.LLM,
		modelID:    p.ModelID,
		provider:   p.Provider,
		poller:     p.Poller,
		exec:       NewExecutor(p.TraderID, p.Exchange, p.Store, events),
		events:     events,
	}
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

	if err := e.poller.Subscribe(e.cfg.PrimaryChannelID, e.traderID, e.HandleMessage); err != nil {
		return fmt.Errorf("channel subscription failed: %w", err)
	}
	e.wg.Add(1)
	go e.reconcileLoop()
	logger.Infof("🎯 [CopyTrade %s] engine started (channel %s)", e.traderName, e.cfg.PrimaryChannelID)
	return nil
}

// Stop detaches from the poller and stops the reconcile loop.
func (e *Engine) Stop() {
	e.stateMu.Lock()
	if !e.running {
		e.stateMu.Unlock()
		return
	}
	e.running = false
	close(e.stopCh)
	e.stateMu.Unlock()

	e.poller.Unsubscribe(e.cfg.PrimaryChannelID, e.traderID)
	e.wg.Wait()
	logger.Infof("⏹ [CopyTrade %s] engine stopped", e.traderName)
}

// HandleMessage is the poller callback: one Discord message (or revision).
func (e *Engine) HandleMessage(msg *store.DiscordMessage, isEdit bool) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	// Dedup across restarts: this exact revision already interpreted?
	done, err := e.st.CopyTrade().SignalProcessed(e.traderID, msg.MessageID, msg.Revision)
	if err != nil {
		return fmt.Errorf("dedup check failed: %w", err)
	}
	if done {
		return nil
	}

	// Author filter (rule layer, zero cost).
	if len(e.cfg.SourceAuthorIDs) > 0 && !containsString(e.cfg.SourceAuthorIDs, msg.AuthorID) {
		return nil
	}
	// Nothing to interpret at all.
	if strings.TrimSpace(msg.Content) == "" && msg.EmbedsJSON == "" && msg.AttachmentsJSON == "" {
		return nil
	}

	e.processMessage(msg, isEdit)
	// Errors inside processMessage are recorded on the signal; the message
	// itself is considered consumed either way.
	return nil
}

// processMessage runs the full pipeline for one message revision.
func (e *Engine) processMessage(msg *store.DiscordMessage, isEdit bool) {
	pipelineStart := time.Now()
	signalID := uuid.NewString()
	traceID := signalID

	sig := &store.CopyTradeSignal{
		ID:               signalID,
		TraderID:         e.traderID,
		ChannelID:        msg.ChannelID,
		MessageID:        msg.MessageID,
		MessageRevision:  msg.Revision,
		Status:           store.SignalStatusReceived,
		MessageTimestamp: msg.MessageTimestamp,
		ReceivedAt:       time.Now().UTC(),
		ReceiveLatencyMs: time.Since(msg.MessageTimestamp).Milliseconds(),
	}
	if err := e.st.CopyTrade().CreateSignal(sig); err != nil {
		logger.Errorf("[CopyTrade %s] signal persist failed: %v", e.traderID, err)
		return
	}
	e.events.Info(traceID, signalID, msg.MessageID, EvMessageReceived,
		fmt.Sprintf("message from %s (revision %d, edit=%v), receive latency %.1fs",
			msg.AuthorName, msg.Revision, isEdit, float64(sig.ReceiveLatencyMs)/1000), nil)

	fail := func(stage string, err error) {
		e.events.Error(traceID, signalID, msg.MessageID, EvExecutionError, stage+": "+err.Error(), nil)
		e.updateSignal(signalID, map[string]interface{}{
			"status": store.SignalStatusFailed, "error_message": err.Error(),
			"total_ms": time.Since(pipelineStart).Milliseconds(),
		})
	}
	skip := func(reason SkipReason, detail string) {
		e.events.Info(traceID, signalID, msg.MessageID, EvSignalSkipped, fmt.Sprintf("skipped (%s): %s", reason, detail), nil)
		e.updateSignal(signalID, map[string]interface{}{
			"status": store.SignalStatusSkipped, "skip_reason": string(reason),
			"total_ms": time.Since(pipelineStart).Milliseconds(),
		})
	}

	// --- 1. Assemble context ---
	interp, aiRunID, timings, err := e.interpret(traceID, signalID, msg, isEdit)
	if err != nil {
		fail("AI interpretation", err)
		return
	}
	interpJSON, _ := json.Marshal(interp)
	e.updateSignal(signalID, map[string]interface{}{
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
	})
	e.events.Success(traceID, signalID, msg.MessageID, EvSignalClassified,
		fmt.Sprintf("%s / %s %s %s (LLM %.1fs)", interp.Classification, interp.Action,
			interp.Symbol, interp.Direction, float64(timings.llmMs)/1000),
		timings.llmMs, map[string]interface{}{"reasoning": interp.Reasoning, "warnings": interp.Warnings})

	// --- 2. Validation & classification gates ---
	marketPrice := 0.0
	canonical := ""
	if interp.Symbol != "" {
		if c, rerr := ResolveInstrument(interp.Symbol); rerr == nil {
			canonical = c
			if mp, perr := e.exec.ex.GetMarketPrice(canonical); perr == nil {
				marketPrice = mp
			}
		} else if interp.IsActionable() {
			skip(SkipUnsupportedInstrument, rerr.Error())
			return
		}
	}

	skipReason, verr := ValidateInterpretation(interp, marketPrice)
	if verr != nil {
		fail("validation", verr)
		return
	}
	if skipReason != SkipNone {
		skip(skipReason, "validation gate")
		return
	}
	if !interp.IsActionable() {
		skip(SkipNotSignal, string(interp.Classification))
		return
	}

	// --- 3. TTL gate (per action class) ---
	ttl := time.Duration(e.cfg.MgmtSignalTTLSeconds) * time.Second
	if interp.Action == ActionOpen || interp.Action == ActionAdd {
		ttl = time.Duration(e.cfg.OpenSignalTTLSeconds) * time.Second
	}
	// Edits carry lifecycle updates; measure freshness from the edit, not the post.
	refTime := msg.MessageTimestamp
	if msg.EditedAt != nil && msg.EditedAt.After(refTime) {
		refTime = *msg.EditedAt
	}
	if IsExpired(refTime, time.Now().UTC(), ttl) {
		skip(SkipExpired, fmt.Sprintf("signal age %v exceeds TTL %v", time.Since(refTime).Round(time.Second), ttl))
		return
	}

	// --- 4. Route by action ---
	e.updateSignal(signalID, map[string]interface{}{"status": store.SignalStatusExecuting})
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

	totalMs := time.Since(pipelineStart).Milliseconds()
	switch {
	case execErr != nil:
		fail("execution", execErr)
	case finalSkip != SkipNone:
		skip(finalSkip, "execution gate")
	default:
		e.updateSignal(signalID, map[string]interface{}{
			"status": store.SignalStatusExecuted, "total_ms": totalMs,
		})
		e.events.Success(traceID, signalID, msg.MessageID, EvTradeUpdated,
			fmt.Sprintf("signal fully executed in %.1fs", float64(totalMs)/1000), totalMs, nil)
	}
}

type pipelineTimings struct {
	mediaMs  int64
	promptMs int64
	llmMs    int64
}

// interpret builds the prompt (with context and optional images) and runs the LLM.
func (e *Engine) interpret(traceID, signalID string, msg *store.DiscordMessage, isEdit bool) (*SourceInterpretation, int64, pipelineTimings, error) {
	var t pipelineTimings

	// Context: active trades, recent signals, reply/linked messages.
	activeCtxs, _ := e.st.CopyTrade().GetActiveContexts(e.traderID)
	var recent []*store.CopyTradeSignal
	if e.cfg.SignalContextEnabled {
		since := time.Now().AddDate(0, 0, -e.cfg.ContextLookbackDays)
		recent, _ = e.st.CopyTrade().GetContextSignals(e.traderID, msg.ChannelID, since, 20)
	}

	var replyMsg *store.DiscordMessage
	if msg.ReplyToMessageID != "" {
		replyMsg = e.lookupMessage(msg.ChannelID, msg.ReplyToMessageID)
	}
	var linked []*store.DiscordMessage
	for _, link := range discord.ExtractMessageLinks(msg.Content) {
		if link.MessageID == msg.MessageID {
			continue
		}
		if lm := e.lookupMessage(link.ChannelID, link.MessageID); lm != nil {
			linked = append(linked, lm)
		}
		if len(linked) >= 3 {
			break
		}
	}

	// Images.
	mediaStart := time.Now()
	var imageParts []mcp.ContentPart
	imageCount := 0
	if e.cfg.ParseImages {
		imageParts, imageCount = e.collectImages(traceID, signalID, msg)
	}
	t.mediaMs = time.Since(mediaStart).Milliseconds()

	// Positions snapshot (optional, non-fatal).
	var positions []store.PositionSnapshot
	if e.cfg.SendPositionSnapshot {
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
		Message:        msg,
		EmbedsText:     discord.FlattenEmbeds(discord.ParseStoredEmbeds(msg.EmbedsJSON)),
		IsEdit:         isEdit,
		ImageCount:     imageCount,
		ChannelNotes:   e.cfg.ChannelNotes,
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
		TraderID:      e.traderID,
		ChannelID:     msg.ChannelID,
		MessageID:     msg.MessageID,
		Model:         e.modelID,
		Provider:      e.provider,
		PromptVersion: PromptVersion,
		SystemPrompt:  SystemPrompt,
		InputPrompt:   userPrompt,
		ImageCount:    imageCount,
		StartedAt:     time.Now().UTC(),
	}

	e.events.Info(traceID, signalID, msg.MessageID, EvAIRequest,
		fmt.Sprintf("LLM request (%s, %d images, prompt %d chars)", e.modelID, imageCount, len(userPrompt)), nil)

	llmStart := time.Now()
	raw, callErr := e.llm.CallWithRequest(&mcp.Request{Messages: messages})
	t.llmMs = time.Since(llmStart).Milliseconds()

	run.FinishedAt = time.Now().UTC()
	run.DurationMs = t.llmMs
	run.RawResponse = raw

	if callErr != nil {
		run.Error = callErr.Error()
		_ = e.st.CopyTrade().CreateAIRun(run)
		e.events.Error(traceID, signalID, msg.MessageID, EvAIError, callErr.Error(), nil)
		return nil, run.ID, t, fmt.Errorf("LLM call failed: %w", callErr)
	}

	interp, perr := ParseInterpretation(raw)
	if perr != nil {
		run.Error = perr.Error()
		_ = e.st.CopyTrade().CreateAIRun(run)
		e.events.Error(traceID, signalID, msg.MessageID, EvAIError, "parse failed: "+perr.Error(), nil)
		return nil, run.ID, t, fmt.Errorf("interpretation parse failed: %w", perr)
	}
	parsedJSON, _ := json.Marshal(interp)
	run.ParsedJSON = string(parsedJSON)
	_ = e.st.CopyTrade().CreateAIRun(run)

	e.events.Success(traceID, signalID, msg.MessageID, EvAIParsed,
		fmt.Sprintf("LLM responded in %.1fs", float64(t.llmMs)/1000), t.llmMs, nil)
	return interp, run.ID, t, nil
}

// collectImages downloads message images and converts them to data-URL parts.
// Failures degrade to text-only with a warning (never fail the signal).
func (e *Engine) collectImages(traceID, signalID string, msg *store.DiscordMessage) ([]mcp.ContentPart, int) {
	client := e.poller.Client()
	if client == nil {
		return nil, 0
	}
	var parts []mcp.ContentPart
	count := 0
	for _, att := range discord.ParseStoredAttachments(msg.AttachmentsJSON) {
		if !att.IsImage() || att.URL == "" {
			continue
		}
		if count >= 3 { // bound prompt size
			break
		}
		img, err := discord.DownloadImage(client, att.URL)
		if err != nil {
			e.events.Warn(traceID, signalID, msg.MessageID, EvMessageSkipped,
				fmt.Sprintf("image download failed, degrading to text-only: %v", err), nil)
			continue
		}
		data, err := discord.ReadImageBytes(img)
		if err != nil {
			continue
		}
		dataURL := "data:" + img.MimeType + ";base64," + base64.StdEncoding.EncodeToString(data)
		parts = append(parts, mcp.NewImagePart(dataURL))
		count++
	}
	return parts, count
}

// lookupMessage reads a message from the store, falling back to the API.
func (e *Engine) lookupMessage(channelID, messageID string) *store.DiscordMessage {
	if m, err := e.st.DiscordMessage().GetByMessageID(channelID, messageID); err == nil && m != nil {
		return m
	}
	client := e.poller.Client()
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
	_ = e.st.DiscordMessage().MarkBaseline(rec)
	return rec
}

// --- action routing ---

// correlateContext finds the trade context a management signal refers to.
// Priority: explicit root message ref > reply target > symbol+direction.
func (e *Engine) correlateContext(interp *SourceInterpretation, msg *store.DiscordMessage, canonical string) *store.CopyTradeContext {
	if rootID := interp.TradeReference.RootMessageID; rootID != "" {
		if ctx, _ := e.st.CopyTrade().GetActiveContextByRootMessage(e.traderID, rootID); ctx != nil {
			return ctx
		}
	}
	// Edited trade cards: the message itself is the root.
	if ctx, _ := e.st.CopyTrade().GetActiveContextByRootMessage(e.traderID, msg.MessageID); ctx != nil {
		return ctx
	}
	if msg.ReplyToMessageID != "" {
		if ctx, _ := e.st.CopyTrade().GetActiveContextByRootMessage(e.traderID, msg.ReplyToMessageID); ctx != nil {
			return ctx
		}
	}
	if canonical != "" {
		ctx, _ := e.st.CopyTrade().GetActiveContextBySymbol(e.traderID, canonical, string(interp.Direction))
		return ctx
	}
	return nil
}

func (e *Engine) routeOpen(traceID, signalID string, msg *store.DiscordMessage, interp *SourceInterpretation, canonical string, marketPrice float64) (SkipReason, error) {
	if e.cfg.Paused {
		return SkipPaused, nil
	}
	// Duplicate protection: an active trade on this symbol+direction already exists.
	if e.cfg.DuplicateOpenProtection {
		if existing, _ := e.st.CopyTrade().GetActiveContextBySymbol(e.traderID, canonical, string(interp.Direction)); existing != nil {
			e.events.Warn(traceID, signalID, msg.MessageID, EvSignalSkipped,
				fmt.Sprintf("duplicate open blocked: active trade %s exists (state %s)", existing.ID, existing.State), nil)
			return SkipDuplicate, nil
		}
	}
	// Max concurrent trades.
	if n, _ := e.st.CopyTrade().CountActiveByTrader(e.traderID); int(n) >= e.cfg.MaxOpenPositions {
		return SkipMaxPositions, nil
	}
	if marketPrice <= 0 {
		return SkipNone, fmt.Errorf("market price unavailable for %s", canonical)
	}

	// Entry decision.
	entrySpec := interp.EntryOrders[0].Price
	entryType, entryPrice, err := DecideEntryType(entrySpec, marketPrice,
		e.cfg.PriceOffsetPctFor(canonical), e.cfg.LimitToMarketWithin)
	if err != nil || entryType == EntryPlanSkip {
		if err == nil {
			err = fmt.Errorf("no executable entry")
		}
		return SkipUnsupportedPriceSpec, nil
	}

	// Resolve SL / TP hard prices against the entry reference.
	slPrice := resolveHardPrice(interp.StopLossLevels[0].Price, entryPrice)
	if slPrice <= 0 {
		return SkipUnsupportedPriceSpec, nil
	}
	if interp.StopLossLevels[0].Conditional != "" {
		e.events.Warn(traceID, signalID, msg.MessageID, EvSignalClassified,
			"author uses a conditional stop ("+interp.StopLossLevels[0].Conditional+"); executing the hard price", nil)
	}
	var tpPrices []float64
	for _, tp := range interp.TakeProfitLevels {
		p := resolveHardPrice(tp.Price, entryPrice)
		if p <= 0 {
			e.events.Warn(traceID, signalID, msg.MessageID, EvSignalClassified,
				fmt.Sprintf("skipping unsupported TP spec %s", tp.Price.Type), nil)
			continue
		}
		tpPrices = append(tpPrices, p)
	}
	defaults, _ := ParseTPRatios(e.cfg.DefaultTPRatios)
	tpRatios, err := AllocateTPRatios(interp.TakeProfitLevels[:len(tpPrices)], defaults)
	if err != nil {
		return SkipNone, fmt.Errorf("TP allocation failed: %w", err)
	}

	// Deterministic sizing.
	riskStart := time.Now()
	equity, available := e.accountBalances()
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
		Symbol:     canonical,
		RawSymbol:  interp.Symbol,
		Direction:  interp.Direction,
		EntryType:  entryType,
		EntryPrice: entryPrice,
		Quantity:   sizing.FinalQuantity,
		Leverage:   leverage,
		StopLoss:   slPrice,
		TPPrices:   tpPrices,
		TPRatios:   tpRatios,
		Sizing:     sizing,
		RootMsgID:  msg.MessageID,
		ChannelID:  msg.ChannelID,
	}

	submitStart := time.Now()
	ctx, err := e.exec.ExecuteOpen(traceID, signalID, plan)
	e.updateSignal(signalID, map[string]interface{}{
		"exchange_submit_ms": time.Since(submitStart).Milliseconds(),
	})
	if err != nil {
		return SkipNone, err
	}
	e.updateSignal(signalID, map[string]interface{}{"trade_context_id": ctx.ID})
	if ctx.State == string(StateOpen) {
		e.events.Success(traceID, signalID, msg.MessageID, EvTradeOpened,
			fmt.Sprintf("%s %s opened: qty=%.8g @ %.8g, SL %.8g, %d TPs",
				canonical, interp.Direction, ctx.Quantity, ctx.AvgFillPrice, slPrice, len(tpPrices)), 0, nil)
	}

	// Author-stated conditional rules are stored on the context via TP plan;
	// AutoBreakevenAfterTP / rules are enforced by the reconciler.
	return SkipNone, nil
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
	ctx := e.correlateContext(interp, msg, canonical)
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
		return e.exec.ExecuteCancel(traceID, signalID, ctx)
	}
	ratio := 100.0
	if interp.Action == ActionReduce || interp.CloseMode == CloseModePartial {
		if interp.CloseRatio != nil {
			ratio = *interp.CloseRatio
		} else {
			ratio = 50 // partial close with unspecified portion: conservative half
			e.events.Warn(traceID, signalID, msg.MessageID, EvSignalClassified,
				"partial close without stated portion; defaulting to 50%", nil)
		}
	}
	return e.exec.ExecuteClose(traceID, signalID, ctx, ratio)
}

func (e *Engine) routeCancel(traceID, signalID string, msg *store.DiscordMessage, interp *SourceInterpretation, canonical string) (SkipReason, error) {
	ctx := e.correlateContext(interp, msg, canonical)
	if ctx == nil {
		return SkipNoPosition, nil
	}
	if ok, reason := ActionApplicable(TradeState(ctx.State), ActionCancel); !ok {
		return reason, nil
	}
	return e.exec.ExecuteCancel(traceID, signalID, ctx)
}

func (e *Engine) routeUpdateSL(traceID, signalID string, msg *store.DiscordMessage, interp *SourceInterpretation, canonical string) (SkipReason, error) {
	ctx := e.correlateContext(interp, msg, canonical)
	if ctx == nil {
		return SkipNoPosition, nil
	}
	if ok, reason := ActionApplicable(TradeState(ctx.State), ActionUpdateSL); !ok {
		return reason, nil
	}
	spec := interp.StopLossLevels[0].Price
	entryRef := ctx.AvgFillPrice
	if entryRef <= 0 {
		entryRef = ctx.PlannedEntryPrice
	}
	newPrice := resolveHardPrice(spec, entryRef)
	if newPrice <= 0 {
		return SkipUnsupportedPriceSpec, nil
	}
	return e.exec.ExecuteUpdateSL(traceID, signalID, ctx, newPrice)
}

func (e *Engine) routeUpdateTP(traceID, signalID string, msg *store.DiscordMessage, interp *SourceInterpretation, canonical string) (SkipReason, error) {
	ctx := e.correlateContext(interp, msg, canonical)
	if ctx == nil {
		return SkipNoPosition, nil
	}
	if ok, reason := ActionApplicable(TradeState(ctx.State), ActionUpdateTP); !ok {
		return reason, nil
	}
	entryRef := ctx.AvgFillPrice
	var prices []float64
	for _, tp := range interp.TakeProfitLevels {
		if p := resolveHardPrice(tp.Price, entryRef); p > 0 {
			prices = append(prices, p)
		}
	}
	if len(prices) == 0 {
		return SkipUnsupportedPriceSpec, nil
	}
	defaults, _ := ParseTPRatios(e.cfg.DefaultTPRatios)
	ratios, err := AllocateTPRatios(interp.TakeProfitLevels[:len(prices)], defaults)
	if err != nil {
		return SkipNone, err
	}
	return e.exec.ExecuteUpdateTP(traceID, signalID, ctx, prices, ratios)
}

// --- helpers ---

// accountBalances reads equity and available margin (best effort).
func (e *Engine) accountBalances() (equity, available float64) {
	account, err := e.exec.ex.GetBalance()
	if err != nil {
		return 0, 0
	}
	for _, key := range []string{"totalEquity", "totalWalletBalance", "total_equity", "totalMarginBalance"} {
		if v, ok := account[key].(float64); ok && v > 0 {
			equity = v
			break
		}
	}
	for _, key := range []string{"availableBalance", "available_balance", "availableMargin"} {
		if v, ok := account[key].(float64); ok && v > 0 {
			available = v
			break
		}
	}
	return equity, available
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
