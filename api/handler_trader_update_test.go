package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/gin-gonic/gin"

	"nofx/config"
	"nofx/copytrader"
	"nofx/manager"
	"nofx/store"
)

func newTraderUpdateTestServer(t *testing.T) (*Server, *store.Store, *gin.Engine) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	t.Setenv("JWT_SECRET", strings.Repeat("x", 32))
	t.Setenv("TRANSPORT_ENCRYPTION", "false")
	config.Init()
	st, err := store.New(filepath.Join(t.TempDir(), "trader-update.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	for _, m := range []struct {
		id      string
		enabled bool
		key     string
	}{{"model", true, "sk-a"}, {"other", true, "sk-b"}, {"disabled", false, "sk-c"}, {"nokey", true, ""}} {
		if err := st.AIModel().Create("owner", m.id, strings.ToUpper(m.id), "claude", m.enabled, m.key, ""); err != nil {
			t.Fatal(err)
		}
	}
	s := &Server{store: st, traderManager: manager.NewTraderManager()}
	r := gin.New()
	r.PUT("/traders/:id", func(c *gin.Context) { c.Set("user_id", "owner"); s.handleUpdateTrader(c) })
	return s, st, r
}

func putTrader(t *testing.T, r *gin.Engine, id string, body map[string]interface{}) *httptest.ResponseRecorder {
	t.Helper()
	raw, _ := json.Marshal(body)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/traders/"+id, strings.NewReader(string(raw)))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	return w
}

func savedTrader(t *testing.T, st *store.Store, id string) store.Trader {
	t.Helper()
	var tr store.Trader
	if err := st.GormDB().First(&tr, "id = ?", id).Error; err != nil {
		t.Fatal(err)
	}
	return tr
}

func TestUpdateAIScanTraderKeepsOmittedLegacyFields(t *testing.T) {
	_, st, r := newTraderUpdateTestServer(t)
	if err := st.Trader().Create(&store.Trader{
		ID: "scan", UserID: "owner", Name: "scan", AIModelID: "model", ExchangeID: "account",
		StrategyID: "strategy", TraderType: string(copytrader.TraderTypeAIScan),
		TradingSymbols: "BTCUSDT,ETHUSDT", CustomPrompt: "be careful", OverrideBasePrompt: true,
		UseAI500: true, UseOITop: true, ScanIntervalMinutes: 15,
	}); err != nil {
		t.Fatal(err)
	}

	// The edit form used to omit these fields entirely.
	w := putTrader(t, r, "scan", map[string]interface{}{"name": "renamed", "ai_model_id": "model", "exchange_id": "account"})
	if w.Code != http.StatusOK {
		t.Fatalf("status %d %s", w.Code, w.Body.String())
	}
	got := savedTrader(t, st, "scan")
	if got.Name != "renamed" || got.TradingSymbols != "BTCUSDT,ETHUSDT" || got.CustomPrompt != "be careful" ||
		!got.OverrideBasePrompt || !got.UseAI500 || !got.UseOITop {
		t.Fatalf("omitted fields were not preserved: %+v", got)
	}

	// Explicit empty / false values still clear them.
	w = putTrader(t, r, "scan", map[string]interface{}{
		"name": "renamed", "ai_model_id": "model", "exchange_id": "account",
		"trading_symbols": "", "custom_prompt": "", "override_base_prompt": false, "use_ai500": false, "use_oi_top": false,
	})
	if w.Code != http.StatusOK {
		t.Fatalf("status %d %s", w.Code, w.Body.String())
	}
	got = savedTrader(t, st, "scan")
	if got.TradingSymbols != "" || got.CustomPrompt != "" || got.OverrideBasePrompt || got.UseAI500 || got.UseOITop {
		t.Fatalf("explicit clear was ignored: %+v", got)
	}

	w = putTrader(t, r, "scan", map[string]interface{}{"name": "renamed", "ai_model_id": "model", "exchange_id": "account", "trading_symbols": "BTC"})
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "BTC") {
		t.Fatalf("invalid symbol accepted: %d %s", w.Code, w.Body.String())
	}
}

func TestUpdateTraderValidatesNewAIModel(t *testing.T) {
	_, st, r := newTraderUpdateTestServer(t)
	if err := st.Trader().Create(&store.Trader{
		ID: "scan", UserID: "owner", Name: "scan", AIModelID: "model", ExchangeID: "account",
		StrategyID: "strategy", TraderType: string(copytrader.TraderTypeAIScan), CustomPrompt: "keep",
	}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ model, key string }{
		{"missing", "trader.update.model_not_found"},
		{"disabled", "trader.update.model_disabled"},
		{"nokey", "trader.update.model_missing_credentials"},
	} {
		w := putTrader(t, r, "scan", map[string]interface{}{"name": "scan", "ai_model_id": tc.model, "exchange_id": "account"})
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), tc.key) {
			t.Fatalf("%s: expected %s, got %d %s", tc.model, tc.key, w.Code, w.Body.String())
		}
		if got := savedTrader(t, st, "scan"); got.AIModelID != "model" {
			t.Fatalf("%s: rejected model was saved", tc.model)
		}
	}
	w := putTrader(t, r, "scan", map[string]interface{}{"name": "scan", "ai_model_id": "other", "exchange_id": "account"})
	if w.Code != http.StatusOK {
		t.Fatalf("usable model rejected: %d %s", w.Code, w.Body.String())
	}
	if got := savedTrader(t, st, "scan"); got.AIModelID != "other" || got.CustomPrompt != "keep" {
		t.Fatalf("model switch not saved correctly: %+v", got)
	}
}

func TestCopyTradeSaveDoesNotReportSourceAppliedSynchronously(t *testing.T) {
	_, st, r := newTraderUpdateTestServer(t)
	exchangeID, err := st.Exchange().Create("owner", "binance", "main", true, "key", "secret", "", false, "", false, false, "", "", "", "", "", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	cfg := copytrader.DefaultCopyTradingConfig()
	cfg.PrimaryChannelID = "123"
	raw, _ := cfg.Encode()
	if err = st.Trader().Create(&store.Trader{ID: "copy", UserID: "owner", Name: "copy", AIModelID: "model", ExchangeID: exchangeID, TraderType: string(copytrader.TraderTypeCopy), CopyTradingConfig: raw}); err != nil {
		t.Fatal(err)
	}
	w := putTrader(t, r, "copy", map[string]interface{}{"name": "copy", "ai_model_id": "model", "exchange_id": exchangeID, "copy_trading_config": raw})
	if w.Code != http.StatusOK {
		t.Fatalf("status %d %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if _, ok := resp["source_applied"]; ok {
		t.Fatalf("source_applied must not be computed while the restart is still pending: %s", w.Body.String())
	}
	if _, ok := resp["restart_requested"]; !ok {
		t.Fatalf("restart_requested missing: %s", w.Body.String())
	}
}

func TestTruncateRunesKeepsMultibyteCharactersIntact(t *testing.T) {
	long := strings.Repeat("中", 250)
	got := truncateRunes(long, channelPreviewMaxRunes)
	if !utf8.ValidString(got) || utf8.RuneCountInString(got) != channelPreviewMaxRunes+1 || !strings.HasSuffix(got, "…") {
		t.Fatalf("bad truncation: %d runes valid=%v", utf8.RuneCountInString(got), utf8.ValidString(got))
	}
	// 200 runes is exactly at the limit: untouched.
	exact := strings.Repeat("é", 200)
	if truncateRunes(exact, 200) != exact {
		t.Fatal("text at the limit must not be truncated")
	}
	if truncateRunes("short", 200) != "short" || truncateRunes("", 200) != "" {
		t.Fatal("short text changed")
	}
	mixed := strings.Repeat("a", 199) + "中文"
	if got := truncateRunes(mixed, 200); got != strings.Repeat("a", 199)+"中…" {
		t.Fatalf("mixed truncation wrong: %q", got)
	}
}
