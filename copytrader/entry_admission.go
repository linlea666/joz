package copytrader

import (
	"errors"
	"fmt"
	"nofx/store"
)

var errEntryDeferred = errors.New("entry submission awaiting source readiness")
var errEntryRevoked = errors.New("unsubmitted entry no longer authorized")

// Called only for a PLANNED entry, before SUBMITTING is written. Queries,
// reductions, protection and unknown submissions retain their recovery paths.
func (e *Engine) admitEntrySubmit(order *store.CopyTradeOrder) error {
	sig, err := e.st.CopyTrade().GetSignal(order.SignalID)
	if err != nil {
		return fmt.Errorf("%w: %v", errEntryDeferred, err)
	}
	if sig == nil {
		return fmt.Errorf("%w: signal snapshot missing", errEntryRevoked)
	}
	msg, err := e.st.DiscordMessage().DeliveryMessage(e.traderID, sig.MessageID, sig.MessageRevision)
	if err != nil {
		return fmt.Errorf("%w: %v", errEntryDeferred, err)
	}
	if msg == nil {
		return fmt.Errorf("%w: immutable receipt missing", errEntryRevoked)
	}
	if err = e.source.CheckExecution(e.traderID, msg, true); err != nil {
		switch err.Error() {
		case "SOURCE_NOT_READY", "SOURCE_GAP_OR_PERMISSION":
			return fmt.Errorf("%w: %v", errEntryDeferred, err)
		default:
			return fmt.Errorf("%w: %v", errEntryRevoked, err)
		}
	}
	rules, err := e.rulesForSignal(sig.ID)
	if err != nil {
		return fmt.Errorf("%w: %v", errEntryDeferred, err)
	}
	if rules.SourceMode == "chroma" {
		ins := &SourceInterpretation{Action: ActionOpen, Symbol: order.Symbol, Direction: Direction(order.Direction)}
		if err = e.notificationOpenGate(msg, ins, rules); err != nil {
			if errors.Is(err, errNotificationEnded) {
				return fmt.Errorf("%w: %v", errEntryRevoked, err)
			}
			return fmt.Errorf("%w: %v", errEntryDeferred, err)
		}
	}
	return nil
}
