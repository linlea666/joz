package discord

// Tests for the token monitor state machine: alert on ok→invalid, throttled
// reminders while invalid, recovery notice on invalid→ok, and silence on
// transient errors / deliberate operator states.

import (
	"errors"
	"testing"
	"time"

	"nofx/store"
)

type sentMail struct {
	to      string
	subject string
}

func newTestMonitor(t *testing.T) (*TokenMonitor, *[]sentMail, func(err error)) {
	t.Helper()
	st, err := store.New(":memory:")
	if err != nil {
		t.Fatalf("store init: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	if err := st.DiscordConfig().Save(store.DiscordConfigUpdate{
		Token:                  "test-token",
		AlertEmail:             strPtr("ops@example.com"),
		MonitorIntervalSeconds: 60,
	}); err != nil {
		t.Fatalf("save config: %v", err)
	}

	tm := NewTokenMonitor(st, nil)
	var mails []sentMail
	tm.send = func(to, subject, body string) error {
		mails = append(mails, sentMail{to: to, subject: subject})
		return nil
	}
	var probeErr error
	tm.probe = func(token string) error { return probeErr }
	setProbe := func(err error) { probeErr = err }
	return tm, &mails, setProbe
}

func strPtr(s string) *string { return &s }

func authErr() error { return &StatusError{StatusCode: 401, Body: "401: Unauthorized"} }

func TestMonitorAlertsOnInvalidAndRecovery(t *testing.T) {
	tm, mails, setProbe := newTestMonitor(t)

	// Healthy check: no mail.
	setProbe(nil)
	tm.checkOnce()
	if tm.Status().Status != TokenStatusOK {
		t.Fatalf("want ok, got %s", tm.Status().Status)
	}
	if len(*mails) != 0 {
		t.Fatalf("no mail expected on ok, got %d", len(*mails))
	}

	// ok → invalid: exactly one alert.
	setProbe(authErr())
	tm.checkOnce()
	if tm.Status().Status != TokenStatusInvalid {
		t.Fatalf("want invalid, got %s", tm.Status().Status)
	}
	if len(*mails) != 1 {
		t.Fatalf("want 1 alert mail, got %d", len(*mails))
	}
	if (*mails)[0].to != "ops@example.com" {
		t.Fatalf("wrong recipient: %s", (*mails)[0].to)
	}

	// Still invalid inside the reminder window: no extra mail.
	tm.checkOnce()
	tm.checkOnce()
	if len(*mails) != 1 {
		t.Fatalf("reminder throttling failed, got %d mails", len(*mails))
	}

	// Reminder window elapsed: one reminder.
	tm.mu.Lock()
	tm.lastAlertAt = time.Now().Add(-reminderInterval - time.Minute)
	tm.mu.Unlock()
	tm.checkOnce()
	if len(*mails) != 2 {
		t.Fatalf("want reminder mail, got %d mails", len(*mails))
	}

	// invalid → ok: recovery notice.
	setProbe(nil)
	tm.checkOnce()
	if tm.Status().Status != TokenStatusOK {
		t.Fatalf("want ok after recovery, got %s", tm.Status().Status)
	}
	if len(*mails) != 3 {
		t.Fatalf("want recovery mail, got %d mails", len(*mails))
	}

	// Healthy again: no more mail.
	tm.checkOnce()
	if len(*mails) != 3 {
		t.Fatalf("unexpected mail on healthy state, got %d", len(*mails))
	}
}

func TestMonitorIgnoresTransientErrors(t *testing.T) {
	tm, mails, setProbe := newTestMonitor(t)

	setProbe(nil)
	tm.checkOnce()

	// Network / 5xx / rate-limit style failures never alert and keep status.
	setProbe(errors.New("connection reset by peer"))
	tm.checkOnce()
	if got := tm.Status().Status; got != TokenStatusOK {
		t.Fatalf("transient error must keep status ok, got %s", got)
	}
	setProbe(&StatusError{StatusCode: 500, Body: "server error"})
	tm.checkOnce()
	if got := tm.Status().Status; got != TokenStatusOK {
		t.Fatalf("5xx must keep status ok, got %s", got)
	}
	if len(*mails) != 0 {
		t.Fatalf("transient errors must not mail, got %d", len(*mails))
	}
	if tm.Status().LastError == "" {
		t.Fatalf("transient error should still be recorded")
	}
}

func TestMonitorIdleStatesNeverAlert(t *testing.T) {
	tm, mails, setProbe := newTestMonitor(t)

	// Go invalid first so idle transitions have alert state to reset.
	setProbe(authErr())
	tm.checkOnce()
	if len(*mails) != 1 {
		t.Fatalf("setup: want 1 alert, got %d", len(*mails))
	}

	// Operator disables monitoring: state resets to unknown, no mail.
	off := false
	if err := tm.store.DiscordConfig().Save(store.DiscordConfigUpdate{MonitorEnabled: &off}); err != nil {
		t.Fatalf("disable monitor: %v", err)
	}
	tm.checkOnce()
	if got := tm.Status().Status; got != TokenStatusUnknown {
		t.Fatalf("disabled monitor must report unknown, got %s", got)
	}
	if len(*mails) != 1 {
		t.Fatalf("idle state must not mail, got %d", len(*mails))
	}

	// Re-enable while still broken: fresh transition alerts again.
	on := true
	if err := tm.store.DiscordConfig().Save(store.DiscordConfigUpdate{MonitorEnabled: &on}); err != nil {
		t.Fatalf("enable monitor: %v", err)
	}
	tm.checkOnce()
	if len(*mails) != 2 {
		t.Fatalf("re-enabled monitor must alert on invalid, got %d mails", len(*mails))
	}

	// Token cleared: idle again, no mail.
	if err := tm.store.DiscordConfig().ClearToken(); err != nil {
		t.Fatalf("clear token: %v", err)
	}
	tm.checkOnce()
	if got := tm.Status().Status; got != TokenStatusUnknown {
		t.Fatalf("cleared token must report unknown, got %s", got)
	}
	if len(*mails) != 2 {
		t.Fatalf("cleared token must not mail, got %d", len(*mails))
	}
}

func TestMonitorSkipsMailWithoutRecipient(t *testing.T) {
	tm, mails, setProbe := newTestMonitor(t)
	if err := tm.store.DiscordConfig().Save(store.DiscordConfigUpdate{AlertEmail: strPtr("")}); err != nil {
		t.Fatalf("clear alert email: %v", err)
	}
	setProbe(authErr())
	tm.checkOnce()
	if tm.Status().Status != TokenStatusInvalid {
		t.Fatalf("want invalid, got %s", tm.Status().Status)
	}
	if len(*mails) != 0 {
		t.Fatalf("no recipient configured, must not mail, got %d", len(*mails))
	}
}
