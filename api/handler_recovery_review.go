package api

import (
	"github.com/gin-gonic/gin"
	"net/http"
)

func (s *Server) handleRecoveryReviewPreview(c *gin.Context) { s.handleRecoveryReview(c, false) }
func (s *Server) handleRecoveryReviewConfirm(c *gin.Context) { s.handleRecoveryReview(c, true) }
func (s *Server) handleRecoveryReview(c *gin.Context, confirm bool) {
	ctx, err := s.store.CopyTrade().GetContext(c.Param("id"))
	if err != nil || ctx == nil {
		SafeNotFound(c, "Trade context")
		return
	}
	if !s.verifyTraderOwnership(c, ctx.TraderID) {
		return
	}
	var req struct {
		Ticket string `json:"ticket"`
		Reason string `json:"reason"`
	}
	if confirm {
		if err := c.ShouldBindJSON(&req); err != nil || req.Ticket == "" {
			SafeBadRequest(c, "ticket and reason required")
			return
		}
	}
	if s.traderManager == nil {
		SafeBadRequest(c, "Trader runtime unavailable; reload configuration without starting trading")
		return
	}
	at, err := s.traderManager.GetTrader(ctx.TraderID)
	if err != nil {
		SafeBadRequest(c, "Trader runtime unavailable; reload configuration without starting trading")
		return
	}
	result, err := at.ReviewCopyRecovery(ctx.ID, c.GetString("user_id"), req.Ticket, req.Reason)
	if err != nil {
		SafeBadRequest(c, err.Error())
		return
	}
	c.JSON(http.StatusOK, gin.H{"review": result})
}
func (s *Server) handleHistorySyncStatus(c *gin.Context) {
	id := c.Query("trader_id")
	if !s.verifyTraderOwnership(c, id) {
		return
	}
	binding, err := s.store.CopyTrade().TraderExchangeID(id)
	if err != nil {
		SafeInternalError(c, "Cannot read account binding", err)
		return
	}
	state, err := s.store.Order().HistorySyncState(binding)
	if err != nil {
		SafeInternalError(c, "Cannot read history sync state", err)
		return
	}
	c.JSON(http.StatusOK, state)
}
