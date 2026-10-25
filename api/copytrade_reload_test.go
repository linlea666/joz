package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"nofx/config"
	"nofx/copytrader"
	"nofx/manager"
	"nofx/store"
)

type recordingReloader struct{ calls map[string]string }

func (r *recordingReloader) PrepareReload(traderID, executionKey string) {
	r.calls[traderID] = executionKey
}

func withRecordingReloader(t *testing.T) *recordingReloader {
	t.Helper()
	rec := &recordingReloader{calls: map[string]string{}}
	previous := copyTradeReloadSource
	copyTradeReloadSource = func() copyTradeRouteReloader { return rec }
	t.Cleanup(func() { copyTradeReloadSource = previous })
	return rec
}

func TestPrepareCopyTradeReloadOnlyForCopyTraders(t *testing.T) {
	rec := withRecordingReloader(t)
	cfg := copytrader.DefaultCopyTradingConfig()
	cfg.PrimaryChannelID = "123"
	raw, _ := cfg.Encode()
	prepareCopyTradeReload(&store.Trader{ID: "copy", TraderType: string(copytrader.TraderTypeCopy), CopyTradingConfig: raw})
	prepareCopyTradeReload(&store.Trader{ID: "scan", TraderType: string(copytrader.TraderTypeAIScan), CopyTradingConfig: raw})
	prepareCopyTradeReload(&store.Trader{ID: "broken", TraderType: string(copytrader.TraderTypeCopy), CopyTradingConfig: "{"})
	prepareCopyTradeReload(nil)
	if len(rec.calls) != 1 || rec.calls["copy"] != cfg.ExecutionKey() {
		t.Fatalf("unexpected reload preparation: %+v", rec.calls)
	}
}

func TestModelConfigSavePreparesCopyTradeRouteReload(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Setenv("JWT_SECRET", strings.Repeat("x", 32))
	t.Setenv("TRANSPORT_ENCRYPTION", "false")
	config.Init()
	if config.Get() == nil || config.Get().TransportEncryption {
		t.Fatal("config not initialized for plain transport")
	}
	rec := withRecordingReloader(t)
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := db.DB()
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() { sqlDB.Close() })
	if err = db.AutoMigrate(&store.Trader{}, &store.Exchange{}, &store.AIModel{}); err != nil {
		t.Fatal(err)
	}
	st, err := store.NewFromGorm(db)
	if err != nil {
		t.Fatal(err)
	}
	cfg := copytrader.DefaultCopyTradingConfig()
	cfg.PrimaryChannelID = "123"
	raw, _ := cfg.Encode()
	if err = db.Create(&store.AIModel{ID: "model", UserID: "owner", Provider: "claude", Name: "Claude"}).Error; err != nil {
		t.Fatal(err)
	}
	if err = db.Create(&store.Trader{ID: "copy", UserID: "owner", Name: "copy", AIModelID: "model", ExchangeID: "account", TraderType: string(copytrader.TraderTypeCopy), CopyTradingConfig: raw}).Error; err != nil {
		t.Fatal(err)
	}
	if err = db.Create(&store.Trader{ID: "scan", UserID: "owner", Name: "scan", AIModelID: "model", ExchangeID: "account", TraderType: string(copytrader.TraderTypeAIScan)}).Error; err != nil {
		t.Fatal(err)
	}
	s := &Server{store: st, traderManager: manager.NewTraderManager()}
	r := gin.New()
	r.PUT("/models", func(c *gin.Context) { c.Set("user_id", "owner"); s.handleUpdateModelConfigs(c) })
	body, _ := json.Marshal(UpdateModelConfigRequest{Models: map[string]ModelConfigUpdate{"model": {Enabled: true, CustomModelName: "claude-opus-5-5"}}})
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/models", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d %s", w.Code, w.Body.String())
	}
	if len(rec.calls) != 1 || rec.calls["copy"] != cfg.ExecutionKey() {
		t.Fatalf("copy-trading route not prepared for reload: %+v", rec.calls)
	}
}

func TestExchangeConfigSavePreparesCopyTradeRouteReload(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Setenv("JWT_SECRET", strings.Repeat("x", 32))
	t.Setenv("TRANSPORT_ENCRYPTION", "false")
	config.Init()
	if config.Get() == nil || config.Get().TransportEncryption {
		t.Fatal("config not initialized for plain transport")
	}
	rec := withRecordingReloader(t)
	st, err := store.New(filepath.Join(t.TempDir(), "exchange.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
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
	if err = st.Trader().Create(&store.Trader{ID: "scan", UserID: "owner", Name: "scan", AIModelID: "model", ExchangeID: exchangeID, TraderType: string(copytrader.TraderTypeAIScan)}); err != nil {
		t.Fatal(err)
	}
	s := &Server{store: st, traderManager: manager.NewTraderManager(), exchangeAccountStateCache: NewExchangeAccountStateCache()}
	r := gin.New()
	r.PUT("/exchanges", func(c *gin.Context) { c.Set("user_id", "owner"); s.handleUpdateExchangeConfigs(c) })
	body, _ := json.Marshal(UpdateExchangeConfigRequest{Exchanges: map[string]ExchangeConfigUpdate{exchangeID: {Enabled: true}}})
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/exchanges", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d %s", w.Code, w.Body.String())
	}
	if len(rec.calls) != 1 || rec.calls["copy"] != cfg.ExecutionKey() {
		t.Fatalf("copy-trading route not prepared for reload: %+v", rec.calls)
	}
}
