package api

import (
	"encoding/json"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"net/http/httptest"
	"nofx/store"
	"strings"
	"testing"
	"time"
)

func TestLogCleanupTicketCannotBeReplayedOrStolen(t *testing.T) {
	s, err := store.New(t.TempDir() + "/test.db")
	require.NoError(t, err)
	defer s.Close()
	server := &Server{store: s}
	require.NoError(t, s.Logs().Append(&store.SystemEvent{EventID: "old", OccurredAt: time.Now().Add(-time.Hour)}))
	call := func(user, path, body string, handler gin.HandlerFunc) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest("POST", path, strings.NewReader(body))
		c.Request.Header.Set("Content-Type", "application/json")
		c.Set("user_id", user)
		handler(c)
		return w
	}
	w := call("u", "/preview", `{"cutoff":"`+time.Now().UTC().Format(time.RFC3339Nano)+`"}`, server.handleLogCleanupPreview)
	require.Equal(t, 200, w.Code, w.Body.String())
	var preview struct {
		Ticket string `json:"ticket"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &preview))
	body := `{"ticket":"` + preview.Ticket + `"}`
	require.Equal(t, 409, call("v", "/cleanup", body, server.handleLogCleanup).Code)
	require.Equal(t, 200, call("u", "/cleanup", body, server.handleLogCleanup).Code)
	require.Equal(t, 409, call("u", "/cleanup", body, server.handleLogCleanup).Code)
}
func TestCSVFormulaAndLogFilter(t *testing.T) {
	for _, s := range []string{"=SUM(1,2)", " @call", "-1", "\tcmd"} {
		require.True(t, strings.HasPrefix(csvSafe(s), "'"))
	}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Set("user_id", "u")
	c.Request = httptest.NewRequest("GET", "/logs?scope=invalid", nil)
	_, err := logFilter(c)
	require.Error(t, err)
}

func TestLogExportOwnedRowsAndFormulaEscaping(t *testing.T) {
	s, err := store.New(t.TempDir() + "/export.db")
	require.NoError(t, err)
	defer s.Close()
	server := &Server{store: s}
	require.NoError(t, s.GormDB().Exec("INSERT INTO traders(id,user_id,name,ai_model_id,exchange_id,initial_balance) VALUES('mine','u','mine','a','e',0),('other','v','other','a','e',0)").Error)
	require.NoError(t, s.CopyTrade().AppendEvent(&store.CopyTradeEvent{TraderID: "mine", Message: "=cmd", ContextJSON: `{"token":"private-secret","ok":1}`}))
	require.NoError(t, s.CopyTrade().AppendEvent(&store.CopyTradeEvent{TraderID: "other", Message: "not-yours"}))
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Set("user_id", "u")
	c.Request = httptest.NewRequest("GET", "/logs/export?scope=trade", nil)
	server.handleLogExport(c)
	require.Equal(t, 200, w.Code, w.Body.String())
	require.Equal(t, "1", w.Header().Get("X-Log-Count"))
	require.Contains(t, w.Body.String(), "'=cmd")
	require.NotContains(t, w.Body.String(), "not-yours")
	require.NotContains(t, w.Body.String(), "private-secret")
}

func TestLogExportLimitRejectsBeforeStreaming(t *testing.T) {
	s, err := store.New(t.TempDir() + "/limit.db")
	require.NoError(t, err)
	defer s.Close()
	require.NoError(t, s.GormDB().Exec(`WITH RECURSIVE n(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM n WHERE x<100001) INSERT INTO system_events(event_id,user_id,trader_id,occurred_at) SELECT CAST(x AS TEXT),'','',? FROM n`, time.Now().UTC()).Error)
	server := &Server{store: s}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Set("user_id", "u")
	c.Request = httptest.NewRequest("GET", "/logs/export", nil)
	server.handleLogExport(c)
	require.Equal(t, 400, w.Code)
	require.Contains(t, w.Body.String(), "100,000")
	require.Empty(t, w.Header().Get("Content-Disposition"))
}
