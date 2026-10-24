package discord

import (
	"errors"
	"strings"
	"testing"
	"time"

	"nofx/store"
)

func TestCheckExecutionSeparatesLiveBoundaryFromTraderActivation(t *testing.T) {
	st := sourceStore(t)
	m := NewSourceManager(st)
	now := time.Now().UTC().Truncate(time.Second)
	liveSince := now.Add(-time.Hour)
	activated := now.Add(-10 * time.Minute)
	m.cfg = &store.DiscordConfig{Enabled: true, Token: "test", RunMode: "live", ExecutionSince: &liveSince}
	m.subscriptions["t"] = &sourceSubscription{route: Route{TraderID: "t", Channels: []string{"200"}, ExecutionKey: "k", ActivatedAt: activated}}

	// Posted before the global live boundary.
	posted := liveSince.Add(-time.Minute)
	msg := &store.DiscordMessage{ChannelID: "200", ReceivedAt: now, MessageTimestamp: posted, ExecutionKey: "k"}
	err := m.CheckExecution("t", msg, true)
	if ExecutionGateCode(err) != GateBeforeLiveBoundary {
		t.Fatalf("want live boundary, got %v", err)
	}
	if s := err.Error(); !strings.Contains(s, posted.Format(time.RFC3339)) || !strings.Contains(s, liveSince.Format(time.RFC3339)) {
		t.Fatalf("detail must carry both compared times: %s", s)
	}
	// Received (backfilled) before the live boundary.
	msg = &store.DiscordMessage{ChannelID: "200", ReceivedAt: liveSince, MessageTimestamp: now, ExecutionKey: "k"}
	if err = m.CheckExecution("t", msg, false); ExecutionGateCode(err) != GateBeforeLiveBoundary || !strings.Contains(err.Error(), "message received") {
		t.Fatalf("want live boundary on receipt, got %v", err)
	}

	// After the live boundary but before this trader's activation: new risk is
	// refused with the activation code, management is not.
	posted = activated.Add(-time.Minute)
	msg = &store.DiscordMessage{ChannelID: "200", ReceivedAt: now, MessageTimestamp: posted, ExecutionKey: "k"}
	err = m.CheckExecution("t", msg, true)
	if ExecutionGateCode(err) != GateBeforeTraderActivation {
		t.Fatalf("want trader activation, got %v", err)
	}
	if s := err.Error(); !strings.Contains(s, posted.Format(time.RFC3339)) || !strings.Contains(s, activated.Format(time.RFC3339)) {
		t.Fatalf("detail must carry both compared times: %s", s)
	}
	var gate *ExecutionGateError
	if !errors.As(err, &gate) || gate.Code != GateBeforeTraderActivation {
		t.Fatalf("typed gate error missing: %#v", err)
	}
	if err = m.CheckExecution("t", msg, false); err != nil {
		t.Fatalf("management must pass the activation boundary: %v", err)
	}
}

func TestExecutionGateCodeFallsBackToMessage(t *testing.T) {
	if got := ExecutionGateCode(errors.New("SOURCE_NOT_READY")); got != GateSourceNotReady {
		t.Fatal(got)
	}
	if got := ExecutionGateCode(gateError(GateConfigRevoked)); got != GateConfigRevoked || gateError(GateConfigRevoked).Error() != GateConfigRevoked {
		t.Fatal(got)
	}
	if ExecutionGateCode(nil) != "" {
		t.Fatal("nil error must have no code")
	}
}
