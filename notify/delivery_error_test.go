package notify

import (
	"errors"
	"io"
	"net/textproto"
	"testing"
	"time"
)

func TestDeliveryFailureClassification(t *testing.T) {
	for _, tc := range []struct {
		stage string
		err   error
		kind  string
	}{
		{"recipient", &textproto.Error{Code: 450, Msg: "private"}, "temporary"},
		{"recipient", &textproto.Error{Code: 550, Msg: "private"}, "permanent"},
		{"authentication", &textproto.Error{Code: 454, Msg: "private"}, "permanent"},
		{"delivery", io.EOF, "uncertain"},
		{"write", io.EOF, "uncertain"},
		{"delivery", &textproto.Error{Code: 451, Msg: "private"}, "temporary"},
	} {
		var failure *DeliveryError
		err := smtpError(tc.stage, tc.err)
		if !errors.As(err, &failure) || failure.Kind != tc.kind {
			t.Fatalf("%s: %v", tc.stage, err)
		}
	}
}

func TestExpiredNotificationNeverConnects(t *testing.T) {
	err := SendBefore(SMTPConfig{Host: "should-not-be-contacted.invalid", Port: 465, Security: "tls", User: "a@example.com", Pass: "test"}, "b@example.com", "test", "body", time.Now().Add(-time.Second))
	var failure *DeliveryError
	if !errors.As(err, &failure) || failure.Stage != "deadline" {
		t.Fatal(err)
	}
}
