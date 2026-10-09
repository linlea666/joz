package notify

import (
	"errors"
	"net/mail"
	"os"
	"strconv"
	"strings"

	"nofx/crypto"
	"nofx/store"
)

type SMTPConfig struct {
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Security string `json:"security"`
	User     string `json:"user"`
	Pass     string `json:"-"`
}

func (SMTPConfig) String() string { return "SMTPConfig{credentials redacted}" }

type EmailSettings struct {
	SMTPConfig
	Source      string `json:"source"`
	PasswordSet bool   `json:"password_set"`
	Configured  bool   `json:"configured"`
	Recipient   string `json:"recipient"`
	Enabled     bool   `json:"enabled"`
}

type EmailDraft struct {
	Host      string `json:"host"`
	Port      int    `json:"port"`
	Security  string `json:"security"`
	User      string `json:"user"`
	Password  string `json:"password"`
	Recipient string `json:"recipient"`
	Enabled   bool   `json:"enabled"`
}

func (EmailDraft) String() string { return "EmailDraft{credentials redacted}" }

func ValidateAddress(address string) error {
	parsed, err := mail.ParseAddress(address)
	if err != nil || parsed.Address != address || strings.ContainsAny(address, "\r\n") {
		return errors.New("a single valid email address is required")
	}
	return nil
}
func (cfg SMTPConfig) Validate() error {
	if cfg.Host == "" || strings.ContainsAny(cfg.Host, " /:@\t\r\n") || (strings.Contains(cfg.Host, "..")) {
		return errors.New("invalid SMTP host")
	}
	if cfg.Port < 1 || cfg.Port > 65535 {
		return errors.New("SMTP port must be 1–65535")
	}
	if cfg.Security != "tls" && cfg.Security != "starttls" {
		return errors.New("SMTP security must be tls or starttls")
	}
	if err := ValidateAddress(cfg.User); err != nil {
		return errors.New("invalid SMTP sender/login email")
	}
	if cfg.Pass == "" {
		return errors.New("SMTP authorization code is required")
	}
	return nil
}

func environmentSMTP() SMTPConfig {
	port := 465
	if raw := os.Getenv("SMTP_PORT"); raw != "" {
		port, _ = strconv.Atoi(raw)
	}
	security := strings.ToLower(strings.TrimSpace(os.Getenv("SMTP_SECURITY")))
	if security == "" {
		security = "starttls"
		if port == 465 {
			security = "tls"
		}
	}
	return SMTPConfig{Host: strings.TrimSpace(os.Getenv("SMTP_HOST")), Port: port, Security: security,
		User: strings.TrimSpace(os.Getenv("SMTP_USER")), Pass: os.Getenv("SMTP_PASS")}
}

// ResolveEmail selects a whole configuration; any DB/cipher failure is fatal.
func ResolveEmail(st *store.Store) (EmailSettings, error) {
	cfg, recipient, enabled, err := st.DiscordConfig().GetEmail()
	if err != nil {
		return EmailSettings{}, err
	}
	result := EmailSettings{SMTPConfig: environmentSMTP(), Source: "environment", Recipient: recipient, Enabled: enabled}
	if cfg != nil {
		pass, err := crypto.DecryptRequired(cfg.Ciphertext)
		if err != nil {
			return EmailSettings{}, err
		}
		result.SMTPConfig = SMTPConfig{cfg.Host, cfg.Port, cfg.Security, cfg.User, pass}
		result.Source = "database"
	} else if result.Host == "" && result.User == "" && result.Pass == "" {
		result.Source = "none"
	}
	result.PasswordSet = result.Pass != ""
	result.Configured = result.SMTPConfig.Validate() == nil
	return result, nil
}

// PrepareEmail is shared by save and unsaved-form testing. Blank passwords can
// retain only the credential belonging to exactly the same host/login.
func PrepareEmail(st *store.Store, draft EmailDraft) (EmailSettings, error) {
	current, err := ResolveEmail(st)
	if err != nil {
		return EmailSettings{}, err
	}
	draft.Host = strings.ToLower(strings.TrimSpace(draft.Host))
	draft.User, draft.Recipient = strings.TrimSpace(draft.User), strings.TrimSpace(draft.Recipient)
	if draft.Password == "" {
		if !strings.EqualFold(draft.Host, current.Host) || draft.User != current.User {
			return EmailSettings{}, errors.New("changing SMTP host or login requires a new authorization code")
		}
		draft.Password = current.Pass
	}
	result := EmailSettings{SMTPConfig: SMTPConfig{draft.Host, draft.Port, draft.Security, draft.User, draft.Password}, Recipient: draft.Recipient, Enabled: draft.Enabled}
	if err := result.SMTPConfig.Validate(); err != nil {
		return result, err
	}
	if err := ValidateAddress(result.Recipient); err != nil {
		return result, err
	}
	return result, nil
}

func SendAlert(st *store.Store, subject, body string) error {
	settings, err := ResolveEmail(st)
	if err != nil {
		return err
	}
	if !settings.Enabled {
		return nil
	}
	return SendWithConfig(settings.SMTPConfig, settings.Recipient, subject, body)
}

// Retained for non-persistent callers; the monitor and API resolve DB settings.
func EmailConfigured() bool { return environmentSMTP().Validate() == nil }
func SendEmail(to, subject, body string) error {
	return SendWithConfig(environmentSMTP(), to, subject, body)
}
