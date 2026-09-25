package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"nofx/store"

	"github.com/gin-gonic/gin"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestCopyTradeSignalStatesAndTraceIsolation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := db.DB()
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = sqlDB.Close() })
	if err := db.AutoMigrate(&store.Trader{}, &store.CopyTradeSignal{}, &store.CopyTradeContext{}, &store.CopyTradeEvent{}, &store.CopyTradeAction{}, &store.CopyTradeOrder{}); err != nil {
		t.Fatal(err)
	}
	st, err := store.NewFromGorm(db)
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{store: st}
	r := gin.New()
	r.Use(func(c *gin.Context) { c.Set("user_id", "user-a"); c.Next() })
	r.GET("/signals", s.handleGetCopyTradeSignals)
	r.GET("/events", s.handleGetCopyTradeEvents)
	for _, trader := range []store.Trader{{ID: "owned", UserID: "user-a"}, {ID: "foreign", UserID: "user-b"}} {
		if err := db.Create(&trader).Error; err != nil {
			t.Fatal(err)
		}
	}
	for _, ctx := range []store.CopyTradeContext{
		{ID: "pending", TraderID: "owned", State: "ENTRY_PENDING"},
		{ID: "open", TraderID: "owned", State: "OPEN"},
		{ID: "expired", TraderID: "owned", State: "EXPIRED"},
		{ID: "foreign-context", TraderID: "foreign", State: "OPEN"},
	} {
		if err := db.Create(&ctx).Error; err != nil {
			t.Fatal(err)
		}
	}
	cases := []struct{ id, context, interpretation, state string }{
		{"pending", "pending", `{"action":"OPEN"}`, "ENTRY_PENDING"},
		{"open", "open", `{"action":"OPEN"}`, "OPEN"},
		{"expired", "expired", `{"action":"OPEN"}`, "EXPIRED"},
		{"multi", "open", `{"action":"OPEN","instructions":[{"action":"OPEN"},{"action":"OPEN"}]}`, ""},
		{"single-instruction", "open", `{"instructions":[{"action":"OPEN"}]}`, "OPEN"},
		{"null-instruction", "open", `{"action":"OPEN","instructions":[null]}`, ""},
		{"empty-instruction", "open", `{"action":"OPEN","instructions":[{}]}`, ""},
		{"malformed", "open", `{`, ""},
		{"missing", "deleted-context", `{"action":"OPEN"}`, ""},
		{"foreign-reference", "foreign-context", `{"action":"OPEN"}`, ""},
	}
	for _, tc := range cases {
		sig := store.CopyTradeSignal{ID: tc.id, TraderID: "owned", MessageID: tc.id, TradeContextID: tc.context,
			Status: store.SignalStatusExecuted, InterpretationJSON: tc.interpretation, MessageTimestamp: time.Now().UTC()}
		if err := db.Create(&sig).Error; err != nil {
			t.Fatal(err)
		}
	}
	request := func(url string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, url, nil))
		return w
	}
	for _, action := range []store.CopyTradeAction{
		{ID: "multi-first", TraderID: "owned", SignalID: "multi", ContextID: "open", Action: "OPEN", Symbol: "BTCUSDT", Status: "done"},
		{ID: "multi-second", TraderID: "owned", SignalID: "multi", ContextID: "pending", Action: "OPEN", Symbol: "ETHUSDT", Status: "done"},
		{ID: "foreign-action", TraderID: "foreign", SignalID: "multi", ContextID: "foreign-context", Action: "OPEN", Status: "done"},
	} {
		if err := db.Create(&action).Error; err != nil {
			t.Fatal(err)
		}
	}
	for _, leg := range []store.CopyTradeOrder{
		{ID: "leg-one", ClientID: "ct-one", TraderID: "owned", SignalID: "multi", ContextID: "open", Role: "ENTRY_1", Status: "FILLED", Quantity: .9, ExecutedQty: .9},
		{ID: "leg-two", ClientID: "ct-two", TraderID: "owned", SignalID: "multi", ContextID: "pending", Role: "ENTRY_2", Status: "PARTIALLY_FILLED", Quantity: 1, ExecutedQty: .2},
		{ID: "leg-private", ClientID: "ct-private", TraderID: "foreign", SignalID: "multi", ContextID: "foreign-context", Status: "NEW"},
	} {
		if err := db.Create(&leg).Error; err != nil {
			t.Fatal(err)
		}
	}
	w := request("/signals?trader_id=owned")
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	var response struct {
		Signals []map[string]interface{} `json:"signals"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Signals) != len(cases) {
		t.Fatalf("signals=%d", len(response.Signals))
	}
	byID := map[string]map[string]interface{}{}
	for _, sig := range response.Signals {
		byID[sig["id"].(string)] = sig
	}
	if actions, ok := byID["multi"]["action_results"].([]interface{}); !ok || len(actions) != 2 {
		t.Fatal("multi-action results missing or leaked across traders")
	}
	if legs, ok := byID["multi"]["order_legs"].([]interface{}); !ok || len(legs) != 2 {
		t.Fatal("order legs missing or leaked across traders")
	}
	for _, tc := range cases {
		sig := byID[tc.id]
		if sig["status"] != "executed" {
			t.Fatalf("persisted status contract changed: %+v", sig)
		}
		state, present := sig["trade_state"]
		if tc.state == "" && present || tc.state != "" && state != tc.state {
			t.Errorf("%s: state=%v present=%v, want %q", tc.id, state, present, tc.state)
		}
	}
	if db.Migrator().HasColumn(&store.CopyTradeSignal{}, "trade_state") {
		t.Fatal("read-only state became a persisted column")
	}
	if w := request("/signals?trader_id=foreign"); w.Code != http.StatusNotFound {
		t.Fatal("foreign signals were accessible")
	}
	if w := request("/signals?trader_id=owned&format=csv"); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "executed") {
		t.Fatal("legacy CSV contract broken")
	}
	for _, traderID := range []string{"owned", "foreign"} {
		if err := db.Create(&store.CopyTradeEvent{TraderID: traderID, TraceID: "same-trace", Event: "copytrade.entry.decision", Message: traderID}).Error; err != nil {
			t.Fatal(err)
		}
	}
	w = request("/events?trader_id=owned&trace_id=same-trace")
	var events struct {
		Events []store.CopyTradeEvent `json:"events"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &events); err != nil {
		t.Fatal(err)
	}
	if w.Code != http.StatusOK || len(events.Events) != 1 || events.Events[0].TraderID != "owned" {
		t.Fatal("trace query crossed trader boundary")
	}
}
