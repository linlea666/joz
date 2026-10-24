package discord

import (
	"errors"
	"fmt"
	"time"
)

// Execution gate codes returned by SourceManager.CheckExecution. They are
// persisted as copy-trade skip reasons and shown in the UI, so they are stable
// identifiers; the human-readable detail travels separately.
const (
	GateSourceDisabled = "SOURCE_DISABLED"
	GateObserveOnly    = "OBSERVE_ONLY"
	// GateBeforeLiveBoundary: the message predates the global live boundary
	// (DiscordConfig.ExecutionSince).
	GateBeforeLiveBoundary = "BEFORE_LIVE_BOUNDARY"
	// GateBeforeTraderActivation: new risk from a card posted before the
	// trader's current route activation (a restarted trader never opens cards
	// published before it was started).
	GateBeforeTraderActivation = "BEFORE_TRADER_ACTIVATION"
	// GateBeforeExecutionBoundary is the legacy code used for both boundaries
	// above. It is no longer produced, but stored history still carries it.
	GateBeforeExecutionBoundary = "BEFORE_EXECUTION_BOUNDARY"
	GateConfigRevoked           = "CONFIG_REVOKED"
	GateSourceNotReady          = "SOURCE_NOT_READY"
	GateSourceGapOrPermission   = "SOURCE_GAP_OR_PERMISSION"
)

// ExecutionGateError is a CheckExecution refusal: a stable Code plus an
// optional Detail (for example the two compared timestamps). Error() is the
// bare code when there is no detail.
type ExecutionGateError struct {
	Code   string
	Detail string
}

func (e *ExecutionGateError) Error() string {
	if e.Detail == "" {
		return e.Code
	}
	return e.Code + ": " + e.Detail
}

func gateError(code string) error { return &ExecutionGateError{Code: code} }

func boundaryError(code, subject string, ref time.Time, boundaryName string, boundary time.Time) error {
	return &ExecutionGateError{Code: code, Detail: fmt.Sprintf("%s %s is before %s %s",
		subject, ref.UTC().Format(time.RFC3339), boundaryName, boundary.UTC().Format(time.RFC3339))}
}

// ExecutionGateCode returns the stable code of a CheckExecution error. Errors
// that are not ExecutionGateError (for example test doubles) map to their
// message, which is how codes were represented before.
func ExecutionGateCode(err error) string {
	if err == nil {
		return ""
	}
	var gate *ExecutionGateError
	if errors.As(err, &gate) {
		return gate.Code
	}
	return err.Error()
}
