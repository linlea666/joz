package api

import (
	"encoding/csv"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"nofx/copytrader"
	"nofx/discord"
	"nofx/logger"
	"nofx/store"
)

// maskToken shows only the first/last 4 characters.
func maskToken(token string) string {
	if token == "" {
		return ""
	}
	if len(token) <= 8 {
		return "****"
	}
	return token[:4] + "****" + token[len(token)-4:]
}

// handleGetDiscordConfig returns the global Discord config (token masked).
func (s *Server) handleGetDiscordConfig(c *gin.Context) {
	cfg, err := s.store.DiscordConfig().Get()
	if err != nil {
		SafeInternalError(c, "Failed to read Discord configuration", err)
		return
	}
	resp := gin.H{
		"configured":            false,
		"token_masked":          "",
		"poll_interval_seconds": 6,
		"enabled":               true,
	}
	if cfg != nil {
		resp["configured"] = cfg.Token != ""
		resp["token_masked"] = maskToken(string(cfg.Token))
		resp["poll_interval_seconds"] = cfg.PollIntervalSeconds
		resp["enabled"] = cfg.Enabled
	}
	if poller := discord.Global(); poller != nil {
		resp["channels"] = poller.Status()
	}
	c.JSON(http.StatusOK, resp)
}

// handleUpdateDiscordConfig saves the global Discord token / poll settings.
// An empty token keeps the stored one (so settings can be updated without
// re-entering the secret).
func (s *Server) handleUpdateDiscordConfig(c *gin.Context) {
	var req struct {
		Token               string `json:"token"`
		PollIntervalSeconds int    `json:"poll_interval_seconds"`
		Enabled             *bool  `json:"enabled"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		SafeBadRequest(c, "Invalid request parameters")
		return
	}
	// enabled omitted => preserve the stored value (never silently re-enable a
	// deliberately disabled poller). First-time setup defaults to enabled.
	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	} else if existing, gerr := s.store.DiscordConfig().Get(); gerr == nil && existing != nil {
		enabled = existing.Enabled
	}
	if req.PollIntervalSeconds != 0 && (req.PollIntervalSeconds < 3 || req.PollIntervalSeconds > 300) {
		SafeBadRequest(c, "poll_interval_seconds must be between 3 and 300")
		return
	}

	// Validate a newly provided token before persisting it.
	if req.Token != "" {
		client := discord.NewClient(req.Token)
		if _, err := client.GetCurrentUser(); err != nil {
			SafeBadRequest(c, "Discord Token validation failed: "+SanitizeError(err, "invalid token"))
			return
		}
	}

	if err := s.store.DiscordConfig().Save(req.Token, req.PollIntervalSeconds, enabled); err != nil {
		SafeInternalError(c, "Failed to save Discord configuration", err)
		return
	}
	if poller := discord.Global(); poller != nil {
		if err := poller.ReloadConfig(); err != nil {
			logger.Warnf("Discord poller reload failed: %v", err)
		}
	}
	logger.Infof("✓ Discord configuration updated (enabled=%v)", enabled)
	c.JSON(http.StatusOK, gin.H{"message": "Discord configuration saved"})
}

// handleDeleteDiscordToken clears the stored token and stops polling.
func (s *Server) handleDeleteDiscordToken(c *gin.Context) {
	if err := s.store.DiscordConfig().ClearToken(); err != nil {
		SafeInternalError(c, "Failed to clear Discord token", err)
		return
	}
	if poller := discord.Global(); poller != nil {
		_ = poller.ReloadConfig()
	}
	c.JSON(http.StatusOK, gin.H{"message": "Discord token cleared"})
}

// handleTestDiscordConnection validates the stored (or provided) token.
func (s *Server) handleTestDiscordConnection(c *gin.Context) {
	var req struct {
		Token string `json:"token"`
	}
	_ = c.ShouldBindJSON(&req)

	token := req.Token
	if token == "" {
		cfg, err := s.store.DiscordConfig().Get()
		if err != nil || cfg == nil || cfg.Token == "" {
			SafeBadRequest(c, "No Discord token configured")
			return
		}
		token = string(cfg.Token)
	}

	client := discord.NewClient(token)
	user, err := client.GetCurrentUser()
	if err != nil {
		c.JSON(http.StatusOK, gin.H{"ok": false, "error": SanitizeError(err, "token validation failed")})
		return
	}
	name := user.GlobalName
	if name == "" {
		name = user.Username
	}
	c.JSON(http.StatusOK, gin.H{"ok": true, "username": name, "user_id": user.ID})
}

// handleTestDiscordChannel fetches a channel preview (latest messages) so the
// user can verify the channel ID before binding a trader to it.
func (s *Server) handleTestDiscordChannel(c *gin.Context) {
	var req struct {
		ChannelID string `json:"channel_id" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		SafeBadRequest(c, "channel_id is required")
		return
	}
	cfg, err := s.store.DiscordConfig().Get()
	if err != nil || cfg == nil || cfg.Token == "" {
		SafeBadRequest(c, "No Discord token configured")
		return
	}
	client := discord.NewClient(string(cfg.Token))
	msgs, err := client.GetMessages(req.ChannelID, 5)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{"ok": false, "error": SanitizeError(err, "channel fetch failed")})
		return
	}
	preview := make([]gin.H, 0, len(msgs))
	for _, m := range msgs {
		content := m.Content
		if len(content) > 200 {
			content = content[:200] + "…"
		}
		authorName := m.Author.GlobalName
		if authorName == "" {
			authorName = m.Author.Username
		}
		preview = append(preview, gin.H{
			"message_id":  m.ID,
			"author_id":   m.Author.ID,
			"author_name": authorName,
			"content":     content,
			"timestamp":   m.Timestamp,
			"has_embeds":  len(m.Embeds) > 0,
			"attachments": len(m.Attachments),
			"edited":      m.EditedTimestamp != nil,
		})
	}
	c.JSON(http.StatusOK, gin.H{"ok": true, "messages": preview})
}

// --- Copy trading observability ---

// verifyTraderOwnership rejects queries for traders that do not belong to the
// authenticated user. Returns false after writing the error response.
func (s *Server) verifyTraderOwnership(c *gin.Context, traderID string) bool {
	userID := c.GetString("user_id")
	trader, err := s.store.Trader().GetByID(traderID)
	if err != nil || trader == nil || trader.UserID != userID {
		SafeNotFound(c, "Trader")
		return false
	}
	return true
}

// parseTimeRange reads optional start_time / end_time query params
// (RFC3339 or unix milliseconds). Zero values disable the bound.
func parseTimeRange(c *gin.Context) (start, end time.Time, err error) {
	parse := func(s string) (time.Time, error) {
		if s == "" {
			return time.Time{}, nil
		}
		if ms, perr := strconv.ParseInt(s, 10, 64); perr == nil {
			return time.UnixMilli(ms).UTC(), nil
		}
		return time.Parse(time.RFC3339, s)
	}
	if start, err = parse(c.Query("start_time")); err != nil {
		return
	}
	end, err = parse(c.Query("end_time"))
	return
}

// handleGetCopyTradeEvents returns the trace event stream for a trader.
// Supports start_time/end_time filtering and format=csv export.
func (s *Server) handleGetCopyTradeEvents(c *gin.Context) {
	traderID := c.Query("trader_id")
	limit := parseIntDefault(c.Query("limit"), 100, 1, 500)
	offset := parseIntDefault(c.Query("offset"), 0, 0, 1<<30)
	traceID := c.Query("trace_id")
	asCSV := c.Query("format") == "csv"

	if traderID == "" {
		SafeBadRequest(c, "trader_id is required")
		return
	}
	if !s.verifyTraderOwnership(c, traderID) {
		return
	}
	if traceID != "" {
		events, err := s.store.CopyTrade().GetEventsByTrace(traderID, traceID)
		if err != nil {
			SafeInternalError(c, "Failed to read events", err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"events": events})
		return
	}
	start, end, err := parseTimeRange(c)
	if err != nil {
		SafeBadRequest(c, "invalid start_time/end_time (use RFC3339 or unix milliseconds)")
		return
	}
	if asCSV {
		// Export ignores pagination: bounded by the time range instead.
		limit = 100000
		offset = 0
	}
	events, err := s.store.CopyTrade().GetEventsByTrader(traderID, start, end, limit, offset)
	if err != nil {
		SafeInternalError(c, "Failed to read events", err)
		return
	}
	if asCSV {
		writeEventsCSV(c, events)
		return
	}
	c.JSON(http.StatusOK, gin.H{"events": events})
}

// handleGetCopyTradeSignals returns interpreted signals for a trader.
// Supports start_time/end_time filtering and format=csv export.
func (s *Server) handleGetCopyTradeSignals(c *gin.Context) {
	traderID := c.Query("trader_id")
	if traderID == "" {
		SafeBadRequest(c, "trader_id is required")
		return
	}
	if !s.verifyTraderOwnership(c, traderID) {
		return
	}
	limit := parseIntDefault(c.Query("limit"), 50, 1, 200)
	asCSV := c.Query("format") == "csv"
	start, end, err := parseTimeRange(c)
	if err != nil {
		SafeBadRequest(c, "invalid start_time/end_time (use RFC3339 or unix milliseconds)")
		return
	}
	if asCSV {
		limit = 100000
	}
	signals, err := s.store.CopyTrade().GetRecentSignals(traderID, start, end, limit)
	if err != nil {
		SafeInternalError(c, "Failed to read signals", err)
		return
	}
	if asCSV {
		writeSignalsCSV(c, signals)
		return
	}
	c.JSON(http.StatusOK, gin.H{"signals": signals})
}

// writeEventsCSV streams events as a CSV attachment.
func writeEventsCSV(c *gin.Context, events []*store.CopyTradeEvent) {
	c.Header("Content-Type", "text/csv; charset=utf-8")
	c.Header("Content-Disposition", `attachment; filename="copytrade_events.csv"`)
	w := csv.NewWriter(c.Writer)
	_ = w.Write([]string{"occurred_at", "level", "event", "message", "trace_id", "signal_id", "message_id", "channel_id", "duration_ms"})
	for _, ev := range events {
		_ = w.Write([]string{
			ev.OccurredAt.UTC().Format(time.RFC3339),
			ev.Level, ev.Event, ev.Message,
			ev.TraceID, ev.SignalID, ev.MessageID, ev.ChannelID,
			strconv.FormatInt(ev.DurationMs, 10),
		})
	}
	w.Flush()
}

// writeSignalsCSV streams signals as a CSV attachment.
func writeSignalsCSV(c *gin.Context, signals []*store.CopyTradeSignal) {
	c.Header("Content-Type", "text/csv; charset=utf-8")
	c.Header("Content-Disposition", `attachment; filename="copytrade_signals.csv"`)
	w := csv.NewWriter(c.Writer)
	_ = w.Write([]string{
		"message_timestamp", "status", "classification", "action", "symbol", "direction",
		"skip_reason", "error_message", "receive_latency_ms", "llm_request_ms", "total_ms",
		"message_id", "message_revision", "trade_context_id",
	})
	for _, sig := range signals {
		_ = w.Write([]string{
			sig.MessageTimestamp.UTC().Format(time.RFC3339),
			sig.Status, sig.Classification, sig.Action, sig.Symbol, sig.Direction,
			sig.SkipReason, sig.ErrorMessage,
			strconv.FormatInt(sig.ReceiveLatencyMs, 10),
			strconv.FormatInt(sig.LLMRequestMs, 10),
			strconv.FormatInt(sig.TotalMs, 10),
			sig.MessageID, strconv.Itoa(sig.MessageRevision), sig.TradeContextID,
		})
	}
	w.Flush()
}

// handleGetCopyTradeContexts returns a trader's active followed trades.
func (s *Server) handleGetCopyTradeContexts(c *gin.Context) {
	traderID := c.Query("trader_id")
	if traderID == "" {
		SafeBadRequest(c, "trader_id is required")
		return
	}
	if !s.verifyTraderOwnership(c, traderID) {
		return
	}
	ctxs, err := s.store.CopyTrade().GetActiveContexts(traderID)
	if err != nil {
		SafeInternalError(c, "Failed to read trade contexts", err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"contexts": ctxs})
}

// copyEngineFor resolves the RUNNING copy-trading engine of an owned trader.
// Writes the error response and returns nil when unavailable.
func (s *Server) copyEngineFor(c *gin.Context, traderID string) *copytrader.Engine {
	if traderID == "" {
		SafeBadRequest(c, "trader_id is required")
		return nil
	}
	if !s.verifyTraderOwnership(c, traderID) {
		return nil
	}
	at, err := s.traderManager.GetTrader(traderID)
	if err != nil {
		SafeNotFound(c, "Trader")
		return nil
	}
	engine := at.CopyEngine()
	if engine == nil {
		SafeBadRequest(c, "交易员未在运行中（该操作需要跟单交易员处于运行状态）")
		return nil
	}
	return engine
}

// handleStartCopyTradeReplay launches a dry-run recognition replay over the
// stored channel history. No orders are placed, nothing is persisted.
func (s *Server) handleStartCopyTradeReplay(c *gin.Context) {
	var req struct {
		TraderID string `json:"trader_id"`
		Limit    int    `json:"limit"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		SafeBadRequest(c, "Invalid request format")
		return
	}
	engine := s.copyEngineFor(c, req.TraderID)
	if engine == nil {
		return
	}
	if err := engine.StartReplay(req.Limit); err != nil {
		SafeBadRequest(c, err.Error())
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "started"})
}

// handleGetCopyTradeReplay returns the progress/result of the current replay.
func (s *Server) handleGetCopyTradeReplay(c *gin.Context) {
	engine := s.copyEngineFor(c, c.Query("trader_id"))
	if engine == nil {
		return
	}
	report := engine.ReplayStatus()
	if report == nil {
		c.JSON(http.StatusOK, gin.H{"report": nil})
		return
	}
	c.JSON(http.StatusOK, gin.H{"report": report})
}

// handleGetCopyTradeAIRun returns one persisted AI interaction (live path).
func (s *Server) handleGetCopyTradeAIRun(c *gin.Context) {
	traderID := c.Query("trader_id")
	if traderID == "" {
		SafeBadRequest(c, "trader_id is required")
		return
	}
	if !s.verifyTraderOwnership(c, traderID) {
		return
	}
	id, err := strconv.ParseInt(c.Query("id"), 10, 64)
	if err != nil || id <= 0 {
		SafeBadRequest(c, "id is required")
		return
	}
	run, err := s.store.CopyTrade().GetAIRun(traderID, id)
	if err != nil || run == nil {
		SafeNotFound(c, "AI run")
		return
	}
	c.JSON(http.StatusOK, gin.H{"run": run})
}

// handleGenerateCopyTradeProfile drafts channel_notes from stored history.
func (s *Server) handleGenerateCopyTradeProfile(c *gin.Context) {
	var req struct {
		TraderID string `json:"trader_id"`
		Limit    int    `json:"limit"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		SafeBadRequest(c, "Invalid request format")
		return
	}
	engine := s.copyEngineFor(c, req.TraderID)
	if engine == nil {
		return
	}
	draft, err := engine.GenerateProfile(req.Limit)
	if err != nil {
		SafeBadRequest(c, err.Error())
		return
	}
	c.JSON(http.StatusOK, gin.H{"draft": draft})
}

// handleGetCopyTradeAIStats aggregates per-model parse latency for comparison.
func (s *Server) handleGetCopyTradeAIStats(c *gin.Context) {
	days := parseIntDefault(c.Query("days"), 7, 1, 90)
	stats, err := s.store.CopyTrade().GetAIStats(time.Now().AddDate(0, 0, -days))
	if err != nil {
		SafeInternalError(c, "Failed to read AI stats", err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"stats": stats})
}

func parseIntDefault(s string, def, min, max int) int {
	if s == "" {
		return def
	}
	v, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	if v < min {
		return min
	}
	if v > max {
		return max
	}
	return v
}
