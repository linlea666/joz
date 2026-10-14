package api

import (
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"net/http/httptest"
	"nofx/store"
	"strings"
	"testing"
)

func TestRecoveryReviewAndHistorySyncRequireOwnership(t *testing.T) {
	st, err := store.New(t.TempDir() + "/auth.db")
	require.NoError(t, err)
	defer st.Close()
	require.NoError(t, st.GormDB().Exec("INSERT INTO traders(id,user_id,name,ai_model_id,exchange_id,initial_balance) VALUES('t','owner','trade','a','e',0)").Error)
	require.NoError(t, st.GormDB().Create(&store.CopyTradeContext{ID: "ctx", TraderID: "t", ChannelID: "c", Symbol: "BTCUSDT", Direction: "LONG", State: "BREAKEVEN"}).Error)
	server := &Server{store: st}
	for _, h := range []gin.HandlerFunc{server.handleRecoveryReviewPreview, server.handleRecoveryReviewConfirm, server.handleHistorySyncStatus} {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Set("user_id", "other")
		c.Params = gin.Params{{Key: "id", Value: "ctx"}}
		c.Request = httptest.NewRequest("POST", "/test?trader_id=t", strings.NewReader(`{"ticket":"stolen","reason":"test"}`))
		c.Request.Header.Set("Content-Type", "application/json")
		h(c)
		require.Equal(t, 404, w.Code, w.Body.String())
	}
	var audits int64
	require.NoError(t, st.GormDB().Model(&store.CopyTradeRecoveryReview{}).Count(&audits).Error)
	require.Zero(t, audits)
}
