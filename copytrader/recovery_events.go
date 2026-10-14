package copytrader

import (
	"strings"
	"time"

	"nofx/store"
)

type recoveryEvent struct {
	message string
	since   time.Time
	repeats int
}

// Aggregates diagnostics, never execution checks. Engine continues every scan.
func (l *EventLogger) recoveryResult(contextID, phase, message string, now time.Time) {
	l.recoveryMu.Lock()
	defer l.recoveryMu.Unlock()
	if l.recovery == nil {
		l.recovery = map[string]recoveryEvent{}
	}
	key := contextID + "/" + phase
	previous, exists := l.recovery[key]
	fields := func(n int) map[string]interface{} {
		return map[string]interface{}{"context_id": contextID, "stage": phase, "repeat_count": n}
	}
	window := time.Minute
	if strings.HasPrefix(message, "FLAT_RECOVERY_PENDING:") {
		window = 15 * time.Minute
	}
	if exists && message == previous.message && now.Sub(previous.since) < window {
		previous.repeats++
		l.recovery[key] = previous
		return
	}
	if exists && message == previous.message {
		previous.repeats++
	}
	if exists && previous.repeats > 0 {
		l.Warn("reconcile-"+contextID, "", "", EvExecutionError, previous.message, fields(previous.repeats))
	}
	if message == "" {
		if exists {
			l.Info("reconcile-"+contextID, "", "", EvReconcile, "recovery check resolved", fields(0))
		}
		delete(l.recovery, key)
		return
	}
	if !exists || previous.message != message {
		l.Warn("reconcile-"+contextID, "", "", EvExecutionError, message, fields(0))
	}
	l.recovery[key] = recoveryEvent{message: message, since: now}
}

func (e *Engine) recordRecovery(ctx *store.CopyTradeContext, phase string, err error, flat bool) {
	prefix := "RECOVERY_PENDING: "
	if flat {
		prefix = "FLAT_RECOVERY_PENDING: "
	}
	message := ""
	if err != nil {
		message = prefix + err.Error()
	}
	if message != "" && ctx.LastError != message {
		e.exec.updateContext(ctx, map[string]interface{}{"last_error": message})
	} else if message == "" && (strings.HasPrefix(ctx.LastError, "FLAT_RECOVERY_PENDING:") || strings.HasPrefix(ctx.LastError, "RECOVERY_PENDING:")) {
		e.exec.updateContext(ctx, map[string]interface{}{"last_error": ""})
	}
	e.events.recoveryResult(ctx.ID, phase, message, time.Now().UTC())
}
