package copytrader

import (
	"encoding/json"
	"time"

	"nofx/logger"
	"nofx/store"
)

// Event names (stable identifiers used by the UI and analytics).
const (
	EvMessageReceived  = "copytrade.message.received"
	EvMessageSkipped   = "copytrade.message.skipped"
	EvAIRequest        = "copytrade.ai.request"
	EvAIParsed         = "copytrade.ai.parsed"
	EvAIError          = "copytrade.ai.error"
	EvSignalClassified = "copytrade.signal.classified"
	EvSignalSkipped    = "copytrade.signal.skipped"
	EvQuantityPlan     = "copytrade.risk.quantity_plan"
	EvRiskRejected     = "copytrade.risk.rejected"
	EvEntrySubmitted   = "copytrade.order.entry_submitted"
	EvEntryFilled      = "copytrade.order.entry_filled"
	EvSLSet            = "copytrade.order.sl_set"
	EvTPSet            = "copytrade.order.tp_set"
	EvOrderCancelled   = "copytrade.order.cancelled"
	EvCloseSubmitted   = "copytrade.order.close_submitted"
	EvTradeOpened      = "copytrade.trade.opened"
	EvTradeUpdated     = "copytrade.trade.updated"
	EvTradeClosed      = "copytrade.trade.closed"
	EvTradeCancelled   = "copytrade.trade.cancelled"
	EvTradeExpired     = "copytrade.trade.expired"
	EvEmergencyClose   = "copytrade.execution.emergency_close"
	EvExecutionError   = "copytrade.execution.error"
	EvReconcile        = "copytrade.reconcile.update"
)

// EventLogger writes trace events to the store and mirrors them to the
// application log. Persistence failures never break the pipeline.
type EventLogger struct {
	st        *store.Store
	traderID  string
	channelID string
}

// NewEventLogger creates an event logger bound to one trader.
func NewEventLogger(st *store.Store, traderID, channelID string) *EventLogger {
	return &EventLogger{st: st, traderID: traderID, channelID: channelID}
}

// Log writes one event. ctx is marshalled to JSON (nil allowed).
func (l *EventLogger) Log(traceID, signalID, messageID, level, event, message string, durationMs int64, ctx map[string]interface{}) {
	ctxJSON := ""
	if len(ctx) > 0 {
		if b, err := json.Marshal(ctx); err == nil {
			ctxJSON = string(b)
		}
	}
	ev := &store.CopyTradeEvent{
		TraceID:     traceID,
		SignalID:    signalID,
		TraderID:    l.traderID,
		ChannelID:   l.channelID,
		MessageID:   messageID,
		Level:       level,
		Event:       event,
		Message:     message,
		ContextJSON: ctxJSON,
		DurationMs:  durationMs,
		OccurredAt:  time.Now().UTC(),
	}
	if err := l.st.CopyTrade().AppendEvent(ev); err != nil {
		logger.Errorf("[CopyTrade %s] failed to persist event %s: %v", l.traderID, event, err)
	}
	switch level {
	case "error":
		logger.Errorf("[CopyTrade %s] %s: %s", l.traderID, event, message)
	case "warn":
		logger.Warnf("[CopyTrade %s] %s: %s", l.traderID, event, message)
	default:
		logger.Infof("[CopyTrade %s] %s: %s", l.traderID, event, message)
	}
}

// Info / Warn / Error are convenience wrappers.
func (l *EventLogger) Info(traceID, signalID, messageID, event, message string, ctx map[string]interface{}) {
	l.Log(traceID, signalID, messageID, "info", event, message, 0, ctx)
}

func (l *EventLogger) Success(traceID, signalID, messageID, event, message string, durationMs int64, ctx map[string]interface{}) {
	l.Log(traceID, signalID, messageID, "success", event, message, durationMs, ctx)
}

func (l *EventLogger) Warn(traceID, signalID, messageID, event, message string, ctx map[string]interface{}) {
	l.Log(traceID, signalID, messageID, "warn", event, message, 0, ctx)
}

func (l *EventLogger) Error(traceID, signalID, messageID, event, message string, ctx map[string]interface{}) {
	l.Log(traceID, signalID, messageID, "error", event, message, 0, ctx)
}
