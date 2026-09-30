package api

import (
	"encoding/json"
	"github.com/gin-gonic/gin"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"net/http"
	"net/http/httptest"
	"nofx/copytrader"
	"nofx/manager"
	"nofx/store"
	"strings"
	"testing"
)

func TestCopyTradeProfileCatalogAuthenticatedAndReadOnly(t *testing.T) {
	gin.SetMode(gin.TestMode)
	s := &Server{router: gin.New()}
	s.setupRoutes()
	w := httptest.NewRecorder()
	s.router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/copytrade/profiles", nil))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("unprotected catalog: %d", w.Code)
	}
	for _, r := range s.router.Routes() {
		if r.Path == "/api/copytrade/profiles" && r.Method != http.MethodGet {
			t.Fatal("mutable catalog route")
		}
	}
	r := gin.New()
	r.GET("/profiles", s.handleGetCopyTradeProfiles)
	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/profiles", nil))
	var result struct {
		Profiles []copytrader.InterpretationPreset `json:"profiles"`
	}
	if json.Unmarshal(w.Body.Bytes(), &result) != nil || len(result.Profiles) != 4 {
		t.Fatal(w.Body.String())
	}
	for _, p := range result.Profiles {
		if p.ID != "default" && (p.Recommended.Notes == "" || p.Description == "" || p.Recommended.ReduceRatio != 50) {
			t.Fatal("incomplete catalog", p)
		}
	}
	if strings.Contains(w.Body.String(), "StrictManagement") || strings.Contains(w.Body.String(), "Prompt") {
		t.Fatal("internal rule implementation leaked")
	}
}

func TestCopyTradeSaveReportsRuntimeReloadFailure(t *testing.T) {
	for _, modelTable := range []bool{false, true} {
		t.Run(map[bool]string{false: "reload-query-failure", true: "runtime-skipped"}[modelTable], func(t *testing.T) {
			db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
			if err != nil {
				t.Fatal(err)
			}
			sqlDB, _ := db.DB()
			sqlDB.SetMaxOpenConns(1)
			t.Cleanup(func() { sqlDB.Close() })
			if err = db.AutoMigrate(&store.Trader{}, &store.Exchange{}); err != nil {
				t.Fatal(err)
			}
			if modelTable {
				if err = db.AutoMigrate(&store.AIModel{}); err != nil {
					t.Fatal(err)
				}
			}
			st, err := store.NewFromGorm(db)
			if err != nil {
				t.Fatal(err)
			}
			cfg := copytrader.DefaultCopyTradingConfig()
			cfg.PrimaryChannelID = "123"
			cfg.InterpretationProfile = "cmm_v1"
			raw, _ := cfg.Encode()
			if err = db.Create(&store.Exchange{ID: "account", UserID: "owner", ExchangeType: "binance"}).Error; err != nil {
				t.Fatal(err)
			}
			if err = db.Create(&store.Trader{ID: "copy", UserID: "owner", Name: "original", AIModelID: "missing-model", ExchangeID: "account", TraderType: string(copytrader.TraderTypeCopy), CopyTradingConfig: raw}).Error; err != nil {
				t.Fatal(err)
			}
			s := &Server{store: st, traderManager: manager.NewTraderManager()}
			r := gin.New()
			r.PUT("/traders/:id", func(c *gin.Context) { c.Set("user_id", "owner"); s.handleUpdateTrader(c) })
			body, _ := json.Marshal(UpdateTraderRequest{Name: "saved", AIModelID: "missing-model", ExchangeID: "account", CopyTradingConfig: raw})
			w := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPut, "/traders/copy", strings.NewReader(string(body)))
			req.Header.Set("Content-Type", "application/json")
			r.ServeHTTP(w, req)
			var response map[string]interface{}
			_ = json.Unmarshal(w.Body.Bytes(), &response)
			if w.Code != http.StatusOK || response["config_reload_status"] != "failed" || response["restart_requested"] != false || response["is_running"] != false {
				t.Fatalf("wrong reload result %d %s", w.Code, w.Body.String())
			}
			var saved store.Trader
			if err = db.First(&saved, "id = ?", "copy").Error; err != nil || saved.Name != "saved" {
				t.Fatal("database save incorrectly reported", err)
			}
		})
	}
}

func TestCopyTradeSaveNeverRequestsASecondLoaderRestart(t *testing.T) {
	for _, tc := range []struct{ persisted, runtime, loaded, requested, manual bool }{
		{true, true, true, true, false},                                       // normal reload: the loader has already requested Run
		{true, false, true, true, false},                                      // persisted restart intent, runtime not yet running
		{false, true, true, true, true},                                       // runtime-only start is restored exactly once
		{false, false, true, false, false},                                    // editing a stopped follower does not start it
		{true, true, false, false, false}, {false, true, false, false, false}, // failed reload never starts
	} {
		requested, manual := copyTradeRestartState(tc.persisted, tc.runtime, tc.loaded)
		if requested != tc.requested || manual != tc.manual {
			t.Fatalf("restart state %+v got requested=%v manual=%v", tc, requested, manual)
		}
	}
}
