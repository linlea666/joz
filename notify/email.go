// Package notify provides outbound email notifications (SMTP).
//
// SMTP sender credentials come from environment variables so secrets never
// enter the database or the repository:
//
//	SMTP_HOST=smtp.163.com
//	SMTP_PORT=465
//	SMTP_USER=sender@163.com
//	SMTP_PASS=<authorization code>
//
// Port 465 uses implicit TLS (the connection is TLS from the first byte),
// which net/smtp.SendMail does not support — so we dial TLS ourselves and
// hand the connection to smtp.NewClient. Port 587/25 uses STARTTLS.
package notify

import (
	"crypto/tls"
	"fmt"
	"mime"
	"net"
	"net/smtp"
	"os"
	"strconv"
	"strings"
	"time"
)

// smtpConfig is resolved from the environment on every send so operators can
// fix .env and restart without any DB state involved.
type smtpConfig struct {
	Host string
	Port int
	User string
	Pass string
}

func loadSMTPConfig() smtpConfig {
	port := 465
	if v := os.Getenv("SMTP_PORT"); v != "" {
		if p, err := strconv.Atoi(v); err == nil && p > 0 {
			port = p
		}
	}
	return smtpConfig{
		Host: os.Getenv("SMTP_HOST"),
		Port: port,
		User: os.Getenv("SMTP_USER"),
		Pass: os.Getenv("SMTP_PASS"),
	}
}

// EmailConfigured reports whether SMTP sender credentials are present.
func EmailConfigured() bool {
	cfg := loadSMTPConfig()
	return cfg.Host != "" && cfg.User != "" && cfg.Pass != ""
}

// SendEmail sends a UTF-8 plain-text email to a single recipient.
// Returns a descriptive error when SMTP is unconfigured or delivery fails.
func SendEmail(to, subject, body string) error {
	cfg := loadSMTPConfig()
	if cfg.Host == "" || cfg.User == "" || cfg.Pass == "" {
		return fmt.Errorf("SMTP not configured (set SMTP_HOST/SMTP_PORT/SMTP_USER/SMTP_PASS in .env)")
	}
	if to == "" {
		return fmt.Errorf("recipient email is empty")
	}

	addr := net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port))
	auth := smtp.PlainAuth("", cfg.User, cfg.Pass, cfg.Host)

	var client *smtp.Client
	var err error
	if cfg.Port == 465 {
		// Implicit TLS.
		conn, dialErr := tls.DialWithDialer(
			&net.Dialer{Timeout: 15 * time.Second},
			"tcp", addr,
			&tls.Config{ServerName: cfg.Host},
		)
		if dialErr != nil {
			return fmt.Errorf("SMTP TLS dial failed: %w", dialErr)
		}
		client, err = smtp.NewClient(conn, cfg.Host)
		if err != nil {
			conn.Close()
			return fmt.Errorf("SMTP handshake failed: %w", err)
		}
	} else {
		// Plain connection, upgrade via STARTTLS when offered.
		client, err = smtp.Dial(addr)
		if err != nil {
			return fmt.Errorf("SMTP dial failed: %w", err)
		}
		if ok, _ := client.Extension("STARTTLS"); ok {
			if err := client.StartTLS(&tls.Config{ServerName: cfg.Host}); err != nil {
				client.Close()
				return fmt.Errorf("SMTP STARTTLS failed: %w", err)
			}
		}
	}
	defer client.Close()

	if err := client.Auth(auth); err != nil {
		return fmt.Errorf("SMTP auth failed: %w", err)
	}
	if err := client.Mail(cfg.User); err != nil {
		return fmt.Errorf("SMTP MAIL FROM failed: %w", err)
	}
	if err := client.Rcpt(to); err != nil {
		return fmt.Errorf("SMTP RCPT TO failed: %w", err)
	}
	w, err := client.Data()
	if err != nil {
		return fmt.Errorf("SMTP DATA failed: %w", err)
	}
	if _, err := w.Write(buildMessage(cfg.User, to, subject, body)); err != nil {
		w.Close()
		return fmt.Errorf("SMTP write failed: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("SMTP delivery failed: %w", err)
	}
	return client.Quit()
}

// buildMessage assembles RFC 5322 headers + body with UTF-8 subject encoding
// (163 rejects mail with malformed headers or a missing From).
func buildMessage(from, to, subject, body string) []byte {
	var b strings.Builder
	b.WriteString("From: NOFX <" + from + ">\r\n")
	b.WriteString("To: " + to + "\r\n")
	b.WriteString("Subject: " + mime.QEncoding.Encode("UTF-8", subject) + "\r\n")
	b.WriteString("Date: " + time.Now().Format(time.RFC1123Z) + "\r\n")
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/plain; charset=UTF-8\r\n")
	b.WriteString("Content-Transfer-Encoding: 8bit\r\n")
	b.WriteString("\r\n")
	// Normalize bare \n to \r\n for SMTP.
	b.WriteString(strings.ReplaceAll(strings.ReplaceAll(body, "\r\n", "\n"), "\n", "\r\n"))
	b.WriteString("\r\n")
	return []byte(b.String())
}
