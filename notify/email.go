// Package notify provides bounded, certificate-verified SMTP delivery.
package notify

import (
	"crypto/tls"
	"errors"
	"fmt"
	"mime"
	"net"
	"net/smtp"
	"net/textproto"
	"strconv"
	"strings"
	"time"
)

func SendWithConfig(cfg SMTPConfig, to, subject, body string) error {
	return sendSMTP(cfg, to, subject, body, 20*time.Second, nil)
}

// SendBefore also respects a notification's receipt deadline. Existing alert
// callers keep SendWithConfig's unchanged 20-second limit.
func SendBefore(cfg SMTPConfig, to, subject, body string, deadline time.Time) error {
	timeout := time.Until(deadline)
	if timeout <= 0 {
		return &DeliveryError{Kind: "permanent", Stage: "deadline", message: "notification deadline expired before SMTP"}
	}
	if timeout > 20*time.Second {
		timeout = 20 * time.Second
	}
	return sendSMTP(cfg, to, subject, body, timeout, nil)
}

// tlsConfig is an internal test seam for a local CA, never an API option.
func sendSMTP(cfg SMTPConfig, to, subject, body string, timeout time.Duration, tlsConfig *tls.Config) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	if err := ValidateAddress(to); err != nil {
		return err
	}
	deadline := time.Now().Add(timeout)
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port)), timeout)
	if err != nil {
		return smtpError("connection", err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(deadline); err != nil {
		return smtpError("deadline", err)
	}
	if tlsConfig == nil {
		tlsConfig = &tls.Config{ServerName: cfg.Host, MinVersion: tls.VersionTLS12}
	}
	if cfg.Security == "tls" {
		conn = tls.Client(conn, tlsConfig)
	}
	client, err := smtp.NewClient(conn, cfg.Host)
	if err != nil {
		return smtpError("TLS/greeting", err)
	}
	defer client.Close()
	if cfg.Security == "starttls" {
		if ok, _ := client.Extension("STARTTLS"); !ok {
			return fmt.Errorf("SMTP STARTTLS unavailable; connection refused")
		}
		if err := client.StartTLS(tlsConfig); err != nil {
			return smtpError("STARTTLS", err)
		}
	}
	if err := client.Auth(smtp.PlainAuth("", cfg.User, cfg.Pass, cfg.Host)); err != nil {
		return smtpError("authentication", err)
	}
	if err := client.Mail(cfg.User); err != nil {
		return smtpError("sender", err)
	}
	if err := client.Rcpt(to); err != nil {
		return smtpError("recipient", err)
	}
	w, err := client.Data()
	if err != nil {
		return smtpError("DATA", err)
	}
	if _, err := w.Write(buildMessage(cfg.User, to, subject, body)); err != nil {
		return smtpError("write", err)
	}
	if err := w.Close(); err != nil {
		return smtpError("delivery", err)
	}
	// The server accepted DATA. A failed QUIT must not report delivery failure
	// and encourage sending a second copy.
	_ = client.Quit()
	return nil
}

// Do not relay remote SMTP text: a hostile/erroring server may echo credentials.
type DeliveryError struct {
	Kind    string
	Stage   string
	message string
}

func (e *DeliveryError) Error() string { return e.message }

func smtpError(stage string, err error) error {
	kind := "permanent"
	var reply *textproto.Error
	if errors.As(err, &reply) {
		if reply.Code >= 400 && reply.Code < 500 {
			kind = "temporary"
		}
	} else if stage == "delivery" || stage == "write" {
		kind = "uncertain"
	} else {
		var network net.Error
		if errors.As(err, &network) {
			kind = "temporary"
		}
	}
	// Certificate and authentication failures require operator intervention.
	var cert *tls.CertificateVerificationError
	if stage == "authentication" || errors.As(err, &cert) {
		kind = "permanent"
	}
	message := fmt.Sprintf("SMTP %s failed (check credentials, certificate and server settings)", stage)
	if e, ok := err.(net.Error); ok && e.Timeout() {
		message = fmt.Sprintf("SMTP %s timed out", stage)
	}
	return &DeliveryError{Kind: kind, Stage: stage, message: message}
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
