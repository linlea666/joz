package copytrader

import "fmt"

// TradeState is the lifecycle state of one followed trade (TradeContext).
// TP hits are tracked via a separate TPHitCount counter on the trade context,
// not as dedicated states, to keep the machine small and correct.
type TradeState string

const (
	StateNew          TradeState = "NEW"           // interpreted, not yet submitted
	StateEntryPending TradeState = "ENTRY_PENDING" // entry order submitted, not (fully) filled
	StateOpen         TradeState = "OPEN"          // position open (protections placed)
	StateBreakeven    TradeState = "BREAKEVEN"     // SL moved to entry after TP hit
	StateClosePending TradeState = "CLOSE_PENDING" // close submitted, awaiting confirmation
	StateClosed       TradeState = "CLOSED"        // position fully closed (any reason)
	StateCancelled    TradeState = "CANCELLED"     // entry cancelled before fill
	StateInvalid      TradeState = "INVALID"       // rejected by validation / risk engine
	StateExpired      TradeState = "EXPIRED"       // entry never filled within its lifetime
)

// stateTransitions is the single source of truth for allowed transitions.
// Executor.persistContext (and so markContext) audits every state write
// against it (an error event, the write still persists); rewriting the
// current state is always allowed.
var stateTransitions = map[TradeState][]TradeState{
	StateNew: {StateEntryPending, StateOpen, StateInvalid, StateCancelled, StateExpired},
	// CLOSE_PENDING / CLOSED: a close routed to an entry that already carries
	// fills (routeClose cancels the remainder, then exits) before settlement
	// promoted the context to OPEN.
	StateEntryPending: {StateOpen, StateCancelled, StateExpired, StateInvalid, StateClosePending, StateClosed},
	StateOpen:         {StateBreakeven, StateClosePending, StateClosed},
	StateBreakeven:    {StateClosePending, StateClosed},
	// Back-transitions: partial close / TP reduction settled, and exit intents
	// abandoned, superseded or rolled back return to OPEN or BREAKEVEN.
	StateClosePending: {StateClosed, StateOpen, StateBreakeven},
	// Terminal. A stale event must never reopen a trade. The one sanctioned
	// exception, recheckClosedTrades reverting a RECONCILE_CLOSED verdict made
	// on stale data, is admitted explicitly by isSpuriousCloseRecovery.
	StateClosed: {},
	// CANCELLED -> EXPIRED: an entry cancelled because it timed out is
	// relabelled EXPIRED by reconcileLegacyEntry once no fill is confirmed.
	StateCancelled: {StateExpired},
	StateInvalid:   {},
	StateExpired:   {},
}

// CanTransition reports whether from → to is a legal lifecycle transition.
func CanTransition(from, to TradeState) bool {
	for _, allowed := range stateTransitions[from] {
		if allowed == to {
			return true
		}
	}
	return false
}

// lastActionSpuriousCloseRecovered marks the recheckClosedTrades update that
// reverts a RECONCILE_CLOSED verdict once the position provably reappears.
const lastActionSpuriousCloseRecovered = "RECOVERED_SPURIOUS_CLOSE"

// isSpuriousCloseRecovery admits CLOSED -> OPEN only for that recovery path
// (guarded by resurrectionRefusal); every other reopen stays illegal.
func isSpuriousCloseRecovery(from, to TradeState, updates map[string]interface{}) bool {
	return from == StateClosed && to == StateOpen && updates["last_action"] == lastActionSpuriousCloseRecovered
}

// Transition validates and returns the new state, or an error describing the
// illegal transition (callers must treat this as a logic bug / stale event).
func Transition(from, to TradeState) (TradeState, error) {
	if !CanTransition(from, to) {
		return from, fmt.Errorf("illegal trade state transition: %s -> %s", from, to)
	}
	return to, nil
}

// IsActive reports whether the trade still needs management
// (i.e. management signals like UPDATE_SL/CLOSE may apply to it).
func (s TradeState) IsActive() bool {
	switch s {
	case StateEntryPending, StateOpen, StateBreakeven, StateClosePending:
		return true
	}
	return false
}

// IsTerminal reports whether the trade lifecycle has ended.
func (s TradeState) IsTerminal() bool {
	switch s {
	case StateClosed, StateCancelled, StateInvalid, StateExpired:
		return true
	}
	return false
}

// HasOpenPosition reports whether the state implies a live position on the exchange.
func (s TradeState) HasOpenPosition() bool {
	switch s {
	case StateOpen, StateBreakeven, StateClosePending:
		return true
	}
	return false
}

// actionAllowed maps which actions are meaningful for a trade in a given state.
// Actions not listed degrade to idempotent skips (never errors), e.g.
// CLOSE on a CLOSED trade => NOOP_ALREADY_FLAT.
func ActionApplicable(state TradeState, action Action) (bool, SkipReason) {
	switch action {
	case ActionOpen:
		// OPEN creates a new trade context; it never applies to an existing one.
		return false, SkipDuplicate
	case ActionAdd, ActionReduce, ActionUpdateSL, ActionUpdateTP:
		if state.HasOpenPosition() {
			return true, SkipNone
		}
		return false, SkipNoPosition
	case ActionClose:
		if state.HasOpenPosition() {
			return true, SkipNone
		}
		if state == StateEntryPending {
			// Author closed a trade whose entry we never filled: cancel entry instead.
			return true, SkipNone
		}
		return false, SkipAlreadyFlat
	case ActionCancel:
		if state == StateEntryPending || state == StateNew {
			return true, SkipNone
		}
		return false, SkipAlreadyFlat
	}
	return false, SkipNotSignal
}
