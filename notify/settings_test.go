package notify

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"path/filepath"
	"strings"
	"testing"

	"nofx/crypto"
	"nofx/store"
)

func mailTestStore(t *testing.T) *store.Store {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("RSA_PRIVATE_KEY", string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})))
	t.Setenv("DATA_ENCRYPTION_KEY", "test-key-for-mail-storage")
	cs, err := crypto.NewCryptoService()
	if err != nil {
		t.Fatal(err)
	}
	crypto.SetGlobalCryptoService(cs)
	t.Cleanup(func() { crypto.SetGlobalCryptoService(nil) })
	st, err := store.New(filepath.Join(t.TempDir(), "mail.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestEmailWholeConfigurationAndCredentials(t *testing.T) {
	st := mailTestStore(t)
	t.Setenv("SMTP_HOST", "env.example.com")
	t.Setenv("SMTP_PORT", "465")
	t.Setenv("SMTP_USER", "env@example.com")
	t.Setenv("SMTP_PASS", "env-secret")
	cfg, err := ResolveEmail(st)
	if err != nil || cfg.Source != "environment" || cfg.Pass != "env-secret" {
		t.Fatal(cfg, err)
	}
	draft := EmailDraft{"db.example.com", 587, "starttls", "db@example.com", "new-secret", "to@example.com", false}
	prepared, err := PrepareEmail(st, draft)
	if err != nil {
		t.Fatal(err)
	}
	before, _ := st.DiscordConfig().Get()
	if before != nil {
		t.Fatal("prepare must not save")
	}
	if err := st.DiscordConfig().SaveEmail(store.EmailConfig{Host: prepared.Host, Port: prepared.Port, Security: prepared.Security, User: prepared.User}, prepared.Pass, prepared.Recipient, prepared.Enabled); err != nil {
		t.Fatal(err)
	}
	cfg, err = ResolveEmail(st)
	if err != nil || cfg.Source != "database" || cfg.Host != "db.example.com" || cfg.Pass != "new-secret" || cfg.Enabled {
		t.Fatal(cfg, err)
	}
	data, _ := json.Marshal(cfg)
	if strings.Contains(string(data), "secret") {
		t.Fatal("secret serialized")
	}
	dc, _ := st.DiscordConfig().Get()
	if dc.Token != "" || dc.Revision != 0 || dc.RunMode != "observe" || dc.ExecutionSince != nil {
		t.Fatal(dc)
	}
	raw, _, _, _ := st.DiscordConfig().GetEmail()
	if !strings.HasPrefix(raw.Ciphertext, "ENC:v1:") || strings.Contains(raw.Ciphertext, "new-secret") {
		t.Fatal("not encrypted")
	}
	draft.Password = ""
	retained, err := PrepareEmail(st, draft)
	if err != nil || retained.Pass != "new-secret" {
		t.Fatal(retained, err)
	}
	draft.User = "changed@example.com"
	if _, err := PrepareEmail(st, draft); err == nil {
		t.Fatal("new user accepted old secret")
	}
	draft.User = "db@example.com"
	draft.Host = "changed.example.com"
	if _, err := PrepareEmail(st, draft); err == nil {
		t.Fatal("new host accepted old secret")
	}
	// Disabled alerts must not attempt network delivery.
	if err := SendAlert(st, "test", "body"); err != nil {
		t.Fatal(err)
	}
	crypto.SetGlobalCryptoService(nil)
	if _, err := ResolveEmail(st); err == nil {
		t.Fatal("decrypt failure fell back to environment")
	}
	if err := st.DiscordConfig().SaveEmail(*raw, "plain", "other@example.com", true); err == nil {
		t.Fatal("saved without cipher")
	}
}
func TestEmailEmptyFirstPasswordAndDatabaseFailure(t *testing.T) {
	st := mailTestStore(t)
	for _, name := range []string{"SMTP_HOST", "SMTP_USER", "SMTP_PASS"} {
		t.Setenv(name, "")
	}
	draft := EmailDraft{"smtp.163.com", 465, "tls", "sender@example.com", "", "to@example.com", true}
	if _, err := PrepareEmail(st, draft); err == nil {
		t.Fatal("missing initial password accepted")
	}
	st.Close()
	t.Setenv("SMTP_HOST", "env.example.com")
	t.Setenv("SMTP_USER", "env@example.com")
	t.Setenv("SMTP_PASS", "env-secret")
	if _, err := ResolveEmail(st); err == nil {
		t.Fatal("DB failure fell back to environment")
	}
}
