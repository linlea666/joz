package store

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"testing"

	"nofx/crypto"
)

func TestEmailTransactionRollsBackRecipientAndSMTP(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	t.Setenv("RSA_PRIVATE_KEY", string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})))
	t.Setenv("DATA_ENCRYPTION_KEY", "test-key")
	cs, err := crypto.NewCryptoService()
	if err != nil {
		t.Fatal(err)
	}
	crypto.SetGlobalCryptoService(cs)
	defer crypto.SetGlobalCryptoService(nil)
	db := newDiscordTestDB(t)
	sqlDB, _ := db.DB()
	defer sqlDB.Close()
	s := NewDiscordConfigStore(db)
	if err := s.initTables(); err != nil {
		t.Fatal(err)
	}
	original := EmailConfig{Host: "smtp.example.com", Port: 465, Security: "tls", User: "a@example.com"}
	if err := s.SaveEmail(original, "secret", "a@example.com", false); err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("CREATE TRIGGER fail_mail BEFORE UPDATE ON discord_configs BEGIN SELECT RAISE(ABORT, 'injected database failure'); END").Error; err != nil {
		t.Fatal(err)
	}
	original.Host = "changed.example.com"
	if err := s.SaveEmail(original, "new-secret", "b@example.com", true); err == nil {
		t.Fatal("failure ignored")
	}
	cfg, recipient, enabled, err := s.GetEmail()
	if err != nil || cfg.Host != "smtp.example.com" || recipient != "a@example.com" || enabled {
		t.Fatal(cfg, recipient, enabled, err)
	}
	if err := db.Model(&EmailConfig{}).Where("id = 1").Update("ciphertext", "plaintext-not-supported").Error; err != nil {
		t.Fatal(err)
	}
	cfg, _, _, _ = s.GetEmail()
	if _, err := crypto.DecryptRequired(cfg.Ciphertext); err == nil {
		t.Fatal("plaintext decrypted")
	}
}
