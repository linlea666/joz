package api

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"nofx/config"
	"nofx/crypto"
	"nofx/store"
)

func TestEmailAPIIndependentAndEncrypted(t *testing.T) {
	gin.SetMode(gin.TestMode)
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	t.Setenv("JWT_SECRET", strings.Repeat("x", 32))
	t.Setenv("TRANSPORT_ENCRYPTION", "false")
	config.Init()
	t.Setenv("RSA_PRIVATE_KEY", string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})))
	t.Setenv("DATA_ENCRYPTION_KEY", "test-data-key")
	cs, err := crypto.NewCryptoService()
	if err != nil {
		t.Fatal(err)
	}
	crypto.SetGlobalCryptoService(cs)
	t.Cleanup(func() { crypto.SetGlobalCryptoService(nil) })
	st, err := store.New(filepath.Join(t.TempDir(), "email.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	s := &Server{store: st, cryptoHandler: NewCryptoHandler(cs)}
	r := gin.New()
	r.GET("/email", s.handleGetDiscordEmail)
	r.POST("/email", s.handleSaveDiscordEmail)
	r.POST("/test", s.handleTestDiscordAlertEmail)
	request := func(method, path, body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		r.ServeHTTP(w, req)
		return w
	}
	body := `{"host":"smtp.example.com","port":465,"security":"tls","user":"a@example.com","password":"private-code","recipient":"b@example.com","enabled":false}`
	w := request("POST", "/email", body)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	dc, _ := st.DiscordConfig().Get()
	if dc.Token != "" || dc.Revision != 0 || dc.RunMode != "observe" || dc.MonitorEnabled {
		t.Fatal(dc)
	}
	w = request("GET", "/email", "")
	if w.Code != 200 || strings.Contains(w.Body.String(), "private-code") || !strings.Contains(w.Body.String(), `"password_set":true`) {
		t.Fatal(w.Body.String())
	}
	var got map[string]interface{}
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if _, ok := got["password"]; ok {
		t.Fatal("password field returned")
	}
	// Current-form validation does not save, call Discord, or require a token.
	draft := strings.Replace(body, `"port":465`, `"port":0`, 1)
	w = request("POST", "/test", `{"draft":`+draft+`}`)
	if w.Code == 200 && !strings.Contains(w.Body.String(), `"ok":false`) {
		t.Fatal("invalid draft accepted")
	}
	raw, _, _, _ := st.DiscordConfig().GetEmail()
	if raw.Port != 465 {
		t.Fatal("test saved draft")
	}
	// Existing execution boundary and credentials survive a mail-only save.
	mode := "live"
	if err := st.DiscordConfig().Save(store.DiscordConfigUpdate{Token: "discord-test", RunMode: mode}); err != nil {
		t.Fatal(err)
	}
	before, _ := st.DiscordConfig().Get()
	w = request("POST", "/email", strings.Replace(body, "private-code", "", 1))
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	after, _ := st.DiscordConfig().Get()
	if after.Revision != before.Revision || after.Token != before.Token || !after.ExecutionSince.Equal(*before.ExecutionSince) {
		t.Fatal("mail changed runtime")
	}
	// Neither encryption failures nor DB failures may return success.
	crypto.SetGlobalCryptoService(nil)
	w = request("POST", "/email", body)
	if w.Code == 200 {
		t.Fatal("saved without decryption service")
	}
	w = request("GET", "/email", "")
	if w.Code != 500 {
		t.Fatal("concealed decryption failure")
	}
	crypto.SetGlobalCryptoService(cs)
	config.Get().TransportEncryption = true
	defer func() { config.Get().TransportEncryption = false }()
	w = request("POST", "/email", body)
	if w.Code != 400 {
		t.Fatal("plaintext allowed with transport encryption")
	}
	w = request("POST", "/test", `{"draft":`+body+`}`)
	if w.Code != 400 {
		t.Fatal("plaintext draft allowed")
	}
	// The same envelope as the browser must decrypt and save successfully.
	aesKey := make([]byte, 32)
	_, _ = rand.Read(aesKey)
	block, _ := aes.NewCipher(aesKey)
	gcm, _ := cipher.NewGCM(block)
	iv := make([]byte, gcm.NonceSize())
	_, _ = rand.Read(iv)
	wrapped, err := rsa.EncryptOAEP(sha256.New(), rand.Reader, &key.PublicKey, aesKey, nil)
	if err != nil {
		t.Fatal(err)
	}
	encode := base64.RawURLEncoding.EncodeToString
	payload, _ := json.Marshal(crypto.EncryptedPayload{WrappedKey: encode(wrapped), IV: encode(iv), Ciphertext: encode(gcm.Seal(nil, iv, []byte(body), nil)), TS: time.Now().Unix()})
	w = request("POST", "/email", string(payload))
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	// Legacy non-secret test requests remain decodable (without actually sending).
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest("POST", "/test", strings.NewReader(`{"email":"a@example.com"}`))
	if err := s.decodeEmail(ctx, &got, true); err != nil {
		t.Fatal(err)
	}
}
func TestEmailEndpointsRequireAuthentication(t *testing.T) {
	gin.SetMode(gin.TestMode)
	s := &Server{router: gin.New()}
	s.setupRoutes()
	for _, route := range []struct{ method, path string }{{"GET", "/api/discord/email"}, {"POST", "/api/discord/email"}, {"POST", "/api/discord/test-email"}} {
		w := httptest.NewRecorder()
		s.router.ServeHTTP(w, httptest.NewRequest(route.method, route.path, strings.NewReader(`{}`)))
		if w.Code != 401 {
			t.Fatalf("unauthenticated endpoint: %s %d", route.path, w.Code)
		}
	}
}
