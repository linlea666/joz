package trader

import (
	"github.com/stretchr/testify/require"
	"nofx/mcp"
	"nofx/store"
	"testing"
)

func TestCopyTradeStatusUsesRetainedRecognitionAndClientModel(t *testing.T) {
	s, err := store.New(t.TempDir() + "/status.db")
	require.NoError(t, err)
	t.Cleanup(func() { s.Close() })
	for _, model := range []string{"gpt-test", "deterministic:tyler_v1"} {
		require.NoError(t, s.CopyTrade().CreateAIRun(&store.CopyTradeAIRun{TraderID: "one", Model: model}))
	}
	at := &AutoTrader{id: "one", aiModel: "openai", store: s, config: AutoTraderConfig{TraderType: "copy_trading"}, mcpClient: &mcp.Client{Provider: "openai", Model: "gpt-test"}}
	status := at.GetStatus()
	require.Equal(t, "openai", status["ai_provider"])
	require.Equal(t, "gpt-test", status["interpretation_model"])
	require.EqualValues(t, 1, status["call_count"])
	require.Equal(t, "retained_live_ai_runs", status["recognition_stats_scope"])
	require.NoError(t, s.GormDB().Exec("DROP TABLE copytrade_ai_runs").Error)
	status = at.GetStatus()
	require.Nil(t, status["call_count"])
	require.NotEmpty(t, status["recognition_stats_error"])
}
