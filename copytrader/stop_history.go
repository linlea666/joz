package copytrader

import (
	"errors"
	"fmt"
	"time"

	"nofx/store"
	"nofx/trader/types"
)

type stopOutcome int

const (
	// stopTerminal: authenticated history proves the order ended.
	stopTerminal stopOutcome = iota + 1
	// stopAbsent: the order is not open and repeated exact client-ID history
	// lookups returned not-found beyond stopNotFoundGrace, so it never landed.
	stopAbsent
)

// Only exact authenticated-account history can resolve an unacknowledged stop.
// Flat positions and absent open-order lists are insufficient evidence on their
// own; a not-found answer is only accepted after it was observed repeatedly
// over the grace period (see stopAbsenceConverged). Callers must already have
// checked that the client ID is absent from the open-order list.
func (x *Executor) resolveTerminalStop(ctx *store.CopyTradeContext, pending *stopIntent) error {
	_, err := x.resolveStopOutcome(ctx, pending)
	return err
}

func (x *Executor) resolveStopOutcome(ctx *store.CopyTradeContext, pending *stopIntent) (stopOutcome, error) {
	reader, ok := x.ex.(types.StopOrderReader)
	if !ok {
		return 0, fmt.Errorf("%w: stop history query unavailable; manual verification required", ErrProtectionUnconfirmed)
	}
	account, err := x.st.CopyTrade().TraderExchangeID(x.traderID)
	if err != nil {
		return 0, err
	}
	if ctx.TraderID != x.traderID || (ctx.ExchangeID != "" && ctx.ExchangeID != account) {
		return 0, fmt.Errorf("%w: stop account binding mismatch", ErrProtectionUnconfirmed)
	}
	if pending.ClientID == "" {
		return 0, fmt.Errorf("%w: missing durable stop identity", ErrProtectionUnconfirmed)
	}
	result, err := reader.GetStopOrder(ctx.Symbol, "", pending.ClientID)
	if err != nil {
		if errors.Is(err, types.ErrManagedOrderNotFound) {
			converged, rerr := x.recordStopNotFound(ctx, pending)
			if rerr != nil {
				return 0, rerr
			}
			if converged {
				return stopAbsent, nil
			}
			return 0, fmt.Errorf("%w: stop history lookup: %v (not-found %d time(s); within grace period)", ErrProtectionUnconfirmed, err, pending.NotFoundCount)
		}
		// Any other failure breaks the consecutive not-found evidence.
		if rerr := x.resetStopNotFound(ctx, pending); rerr != nil {
			return 0, rerr
		}
		return 0, fmt.Errorf("%w: stop history lookup: %v", ErrProtectionUnconfirmed, err)
	}
	if rerr := x.resetStopNotFound(ctx, pending); rerr != nil {
		return 0, rerr
	}
	side := "SELL"
	if ctx.Direction == "SHORT" {
		side = "BUY"
	}
	if result == nil || result.ClientID != pending.ClientID || result.Symbol != ctx.Symbol || result.PositionSide != ctx.Direction || result.Side != side || !isStopOrder(result.OpenOrder) {
		return 0, fmt.Errorf("%w: stop history identity/side mismatch", ErrProtectionUnconfirmed)
	}
	if !result.Terminal() {
		return 0, fmt.Errorf("%w: stop history not terminal (%s)", ErrProtectionUnconfirmed, result.Status)
	}
	return stopTerminal, nil
}

// recordStopNotFound durably counts consecutive not-found answers and reports
// whether absence has converged: at least stopNotFoundMinObservations answers,
// the first one at least stopNotFoundGrace ago, and the submission/error
// itself older than the grace period.
func (x *Executor) recordStopNotFound(ctx *store.CopyTradeContext, pending *stopIntent) (bool, error) {
	now := stopClock()
	pending.NotFoundCount++
	if pending.FirstNotFoundAt == nil {
		pending.FirstNotFoundAt = &now
	}
	if ctx.StopIntentJSON != "" {
		if err := x.saveStopIntent(ctx, pending); err != nil {
			return false, err
		}
	}
	return stopAbsenceConverged(pending, now), nil
}

func stopAbsenceConverged(p *stopIntent, now time.Time) bool {
	if p.NotFoundCount < stopNotFoundMinObservations || p.FirstNotFoundAt == nil || now.Sub(*p.FirstNotFoundAt) < stopNotFoundGrace {
		return false
	}
	for _, at := range []*time.Time{p.SubmittedAt, p.ErrorAt} {
		if at != nil && now.Sub(*at) < stopNotFoundGrace {
			return false
		}
	}
	return true
}

func (x *Executor) resetStopNotFound(ctx *store.CopyTradeContext, pending *stopIntent) error {
	if pending.NotFoundCount == 0 && pending.FirstNotFoundAt == nil {
		return nil
	}
	pending.NotFoundCount, pending.FirstNotFoundAt = 0, nil
	if ctx.StopIntentJSON == "" {
		return nil
	}
	return x.saveStopIntent(ctx, pending)
}
