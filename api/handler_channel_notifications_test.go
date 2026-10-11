package api

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"nofx/store"
)

func TestChannelNotificationAPIIndependentSave(t *testing.T) {
	gin.SetMode(gin.TestMode)
	st, err := store.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	server := &Server{store: st}
	router := gin.New()
	router.GET("/notifications", server.handleGetChannelNotifications)
	router.PUT("/notifications", server.handleSaveChannelNotifications)
	router.POST("/test", server.handleTestChannelNotificationEmail)
	request := func(method, path, body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(method, path, bytes.NewBufferString(body))
		r.Header.Set("Content-Type", "application/json")
		router.ServeHTTP(w, r)
		return w
	}
	result := request("GET", "/notifications", "")
	if result.Code != 200 {
		t.Fatal(result.Body.String())
	}
	draft := `{"revision":0,"enabled":true,"recipient":"notice@example.com","monitors":[{"channel_id":"1539560125189062696","enabled":true,"label":"notice"}]}`
	result = request("PUT", "/notifications", draft)
	if result.Code != 200 {
		t.Fatal(result.Body.String())
	}
	var response struct {
		Saved   bool
		Applied bool
		Config  store.ChannelNotificationConfig
	}
	if err = json.Unmarshal(result.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if !response.Saved || response.Applied || response.Config.Revision != 1 {
		t.Fatal(result.Body.String())
	}
	dc, err := st.DiscordConfig().Get()
	if err != nil || dc != nil {
		t.Fatal("notification created/changed Discord trading configuration", err)
	}
	if r := request("PUT", "/notifications", draft); r.Code != 409 {
		t.Fatal(r.Code, r.Body.String())
	}
	if r := request("POST", "/test", `{"recipient":"invalid"}`); r.Code != 400 {
		t.Fatal(r.Code)
	}
	if r := request("POST", "/test", `{"recipient":"notice@example.com"}`); r.Code != 400 {
		t.Fatal(r.Code, r.Body.String())
	}
	saved, _ := st.ChannelNotifications()
	if saved.Revision != 1 {
		t.Fatal("test modified settings")
	}
}

func TestChannelNotificationEndpointsRequireAuthentication(t *testing.T) {
	gin.SetMode(gin.TestMode)
	s := &Server{router: gin.New()}
	s.setupRoutes()
	for _, route := range []struct{ method, path string }{{"GET", "/api/discord/channel-notifications"}, {"PUT", "/api/discord/channel-notifications"}, {"POST", "/api/discord/channel-notifications/test-email"}} {
		w := httptest.NewRecorder()
		s.router.ServeHTTP(w, httptest.NewRequest(route.method, route.path, bytes.NewBufferString(`{}`)))
		if w.Code != 401 {
			t.Fatalf("unprotected endpoint %s: %d", route.path, w.Code)
		}
	}
}
