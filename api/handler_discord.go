package api

import (
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"nofx/discord"
	"nofx/logger"
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
	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
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

// handleGetCopyTradeEvents returns the trace event stream for a trader.
func (s *Server) handleGetCopyTradeEvents(c *gin.Context) {
	traderID := c.Query("trader_id")
	limit := parseIntDefault(c.Query("limit"), 100, 1, 500)
	offset := parseIntDefault(c.Query("offset"), 0, 0, 1<<30)
	traceID := c.Query("trace_id")

	if traceID != "" {
		events, err := s.store.CopyTrade().GetEventsByTrace(traceID)
		if err != nil {
			SafeInternalError(c, "Failed to read events", err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"events": events})
		return
	}
	if traderID == "" {
		SafeBadRequest(c, "trader_id is required")
		return
	}
	events, err := s.store.CopyTrade().GetEventsByTrader(traderID, limit, offset)
	if err != nil {
		SafeInternalError(c, "Failed to read events", err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"events": events})
}

// handleGetCopyTradeSignals returns interpreted signals for a trader.
func (s *Server) handleGetCopyTradeSignals(c *gin.Context) {
	traderID := c.Query("trader_id")
	if traderID == "" {
		SafeBadRequest(c, "trader_id is required")
		return
	}
	limit := parseIntDefault(c.Query("limit"), 50, 1, 200)
	signals, err := s.store.CopyTrade().GetRecentSignals(traderID, limit)
	if err != nil {
		SafeInternalError(c, "Failed to read signals", err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"signals": signals})
}

// handleGetCopyTradeContexts returns a trader's active followed trades.
func (s *Server) handleGetCopyTradeContexts(c *gin.Context) {
	traderID := c.Query("trader_id")
	if traderID == "" {
		SafeBadRequest(c, "trader_id is required")
		return
	}
	ctxs, err := s.store.CopyTrade().GetActiveContexts(traderID)
	if err != nil {
		SafeInternalError(c, "Failed to read trade contexts", err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"contexts": ctxs})
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
