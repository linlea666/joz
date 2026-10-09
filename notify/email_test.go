package notify

import (
	"bufio"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestSMTPTransport(t *testing.T) {
	certServer := httptest.NewTLSServer(nil)
	certificate := certServer.TLS.Certificates[0]
	certServer.Close()
	cert, _ := x509.ParseCertificate(certificate.Certificate[0])
	roots := x509.NewCertPool()
	roots.AddCert(cert)
	for _, tc := range []struct {
		name, security, fault string
		success               bool
	}{
		{"TLS", "tls", "", true}, {"STARTTLS", "starttls", "", true},
		{"missing_STARTTLS", "starttls", "no_tls", false}, {"bad_certificate", "tls", "cert", false},
		{"auth_rejected", "tls", "auth", false}, {"recipient_rejected", "tls", "recipient", false},
		{"greeting_timeout", "tls", "greeting", false}, {"delivery_timeout", "tls", "delivery", false},
		{"quit_after_accept", "tls", "quit", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			done := make(chan struct{})
			go func() {
				defer close(done)
				conn, err := listener.Accept()
				if err != nil {
					return
				}
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(time.Second))
				serverTLS := &tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS12}
				if tc.security == "tls" {
					conn = tls.Server(conn, serverTLS)
				}
				if tc.fault == "greeting" {
					time.Sleep(200 * time.Millisecond)
					return
				}
				fmt.Fprint(conn, "220 local test SMTP\r\n")
				reader := bufio.NewReader(conn)
				for {
					line, err := reader.ReadString('\n')
					if err != nil {
						return
					}
					switch {
					case strings.HasPrefix(line, "EHLO"):
						fmt.Fprint(conn, "250-local\r\n250-AUTH PLAIN\r\n")
						if tc.fault != "no_tls" {
							fmt.Fprint(conn, "250-STARTTLS\r\n")
						}
						fmt.Fprint(conn, "250 OK\r\n")
					case strings.HasPrefix(line, "STARTTLS"):
						fmt.Fprint(conn, "220 ready\r\n")
						conn = tls.Server(conn, serverTLS)
						reader = bufio.NewReader(conn)
					case strings.HasPrefix(line, "AUTH"):
						if tc.fault == "auth" {
							fmt.Fprint(conn, "535 private-secret\r\n")
						} else {
							fmt.Fprint(conn, "235 OK\r\n")
						}
					case strings.HasPrefix(line, "RCPT") && tc.fault == "recipient":
						fmt.Fprint(conn, "550 private-secret\r\n")
					case strings.HasPrefix(line, "DATA"):
						fmt.Fprint(conn, "354 continue\r\n")
						for {
							line, err = reader.ReadString('\n')
							if err != nil {
								return
							}
							if line == ".\r\n" {
								break
							}
						}
						if tc.fault == "delivery" {
							time.Sleep(200 * time.Millisecond)
							return
						}
						fmt.Fprint(conn, "250 accepted\r\n")
					case strings.HasPrefix(line, "QUIT"):
						if tc.fault != "quit" {
							fmt.Fprint(conn, "221 bye\r\n")
						}
						return
					default:
						fmt.Fprint(conn, "250 OK\r\n")
					}
				}
			}()
			_, port, _ := net.SplitHostPort(listener.Addr().String())
			n, _ := strconv.Atoi(port)
			trust := &tls.Config{RootCAs: roots, ServerName: "example.com", MinVersion: tls.VersionTLS12}
			if tc.fault == "cert" {
				trust = nil
			}
			timeout := time.Second
			if tc.fault == "greeting" || tc.fault == "delivery" {
				timeout = 80 * time.Millisecond
			}
			start := time.Now()
			err = sendSMTP(SMTPConfig{"127.0.0.1", n, tc.security, "sender@example.com", "private-secret"}, "recipient@example.com", "test", "body", timeout, trust)
			if (err == nil) != tc.success {
				t.Fatalf("success=%v error=%v", tc.success, err)
			}
			if err != nil && strings.Contains(err.Error(), "private-secret") {
				t.Fatal("credential leaked")
			}
			if time.Since(start) > 2*time.Second {
				t.Fatal("timeout unbounded")
			}
			<-done
		})
	}
}

func TestSMTPValidation(t *testing.T) {
	for _, email := range []string{"a@b.com\r\nBcc: c@d.com", "name <a@b.com>", "a@b.com,c@d.com", ""} {
		if ValidateAddress(email) == nil {
			t.Fatalf("accepted %q", email)
		}
	}
	cfg := SMTPConfig{"smtp.example.com", 465, "tls", "sender@example.com", "secret"}
	for _, port := range []int{0, -1, 65536} {
		cfg.Port = port
		if cfg.Validate() == nil {
			t.Fatal("invalid port accepted")
		}
	}
	if err := sendSMTP(SMTPConfig{"invalid.invalid", 465, "tls", "sender@example.com", "secret"}, "a@b.com", "test", "", time.Millisecond, nil); err == nil {
		t.Fatal("connection should fail")
	}
}
