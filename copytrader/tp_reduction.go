package copytrader

import (
	"encoding/json"
	"fmt"
	"math"
	"nofx/store"
	"nofx/trader/types"
	"time"
)

// ExecuteTPReduction replaces exactly one tracked TP allocation. Preparation
// is durable before any cancellation; the exit ledger freezes its final size.
func (x *Executor) ExecuteTPReduction(traceID, signalID string, ctx *store.CopyTradeContext, level int) (SkipReason, error) {
	managed, ok := x.ex.(types.ManagedOrderTrader)
	if !ok || x.gridEx == nil {
		return SkipUnsupportedPriceSpec, fmt.Errorf("numbered TP reduction requires managed orders")
	}
	if level < 1 || level > MaxTPLevels {
		return SkipAmbiguous, fmt.Errorf("invalid TP ordinal")
	}
	intent := managedExitIntent{SignalID: signalID, TPLevel: level}
	if ctx.ClosePendingJSON != "" {
		if err := json.Unmarshal([]byte(ctx.ClosePendingJSON), &intent); err != nil {
			return SkipNone, err
		}
		if intent.SignalID != signalID || intent.TPLevel != level {
			return SkipWaitingReconciliation, fmt.Errorf("another exit is pending")
		}
		if intent.OrderID != "" {
			return SkipNone, x.reconcileManagedClose(traceID, ctx)
		}
	} else {
		count := 0
		for _, tp := range readTPPlan(ctx) {
			if tp.Ordinal == level {
				count++
				if tp.Filled {
					return SkipDuplicate, nil
				}
				if tp.ReplacedBy != "" {
					return SkipWaitingReconciliation, fmt.Errorf("TP already replaced by another exit")
				}
			}
		}
		if count != 1 {
			return SkipNeedsContext, fmt.Errorf("unique original TP%d allocation required", level)
		}
		if ctx.TPUpdateIntentJSON != "" {
			return SkipWaitingReconciliation, fmt.Errorf("TP update pending")
		}
		// Pre-flight against the live position and venue rules BEFORE any
		// durable intent or TP cancellation: an exit that can never be sent
		// must leave the trade and its resting TP untouched.
		if skip, err := x.precheckTPReduction(traceID, ctx, managed, level); err != nil || skip != SkipNone {
			return skip, err
		}
		b, _ := json.Marshal(intent)
		if err := x.persistContext(ctx, map[string]interface{}{"close_pending_json": string(b), "state": string(StateClosePending)}); err != nil {
			return SkipNone, err
		}
	}
	// This is the existing exit rule: prevent later entry fills reopening a
	// reduced trade. A failed or uncertain cancel cannot fall through to selling.
	if err := x.closeEntryEligibility(traceID, ctx, "TP_REPLACEMENT"); err != nil {
		return SkipNone, err
	}
	if _, err := x.refreshTPProgressChecked(traceID, ctx); err != nil {
		return SkipNone, err
	}
	// Errors that no retry can fix release the not-yet-sent intent and restore
	// the trade (and any TP this attempt cancelled) instead of pinning it in
	// CLOSE_PENDING. Once an exit order exists, its ledger is authoritative.
	rollback := func(skip SkipReason, cause error) (SkipReason, error) {
		if intent.OrderID != "" {
			return skip, cause
		}
		return skip, x.rollbackTPIntent(traceID, signalID, ctx, cause)
	}
	plan := readTPPlan(ctx)
	index := -1
	for i := range plan {
		if plan[i].Ordinal == level {
			if index != -1 {
				return rollback(SkipNeedsContext, fmt.Errorf("duplicate TP ordinal"))
			}
			index = i
		}
	}
	if index < 0 {
		return rollback(SkipNeedsContext, fmt.Errorf("TP allocation disappeared"))
	}
	tp := &plan[index]
	if !tp.Filled && tp.OrderID != "" && !orderTerminal(tp.Status) {
		cancelErr := x.gridEx.CancelOrder(ctx.Symbol, tp.OrderID)
		status, err := x.ex.GetOrderStatus(ctx.Symbol, tp.OrderID)
		if err != nil {
			return SkipNone, err
		}
		state, _ := status["status"].(string)
		if !orderTerminal(state) {
			return SkipWaitingReconciliation, fmt.Errorf("TP cancellation unconfirmed: %s (%v)", state, cancelErr)
		}
	}
	// A terminally rejected TP never reached the book: nothing to cancel, the
	// level is reduced at market like a PLANNED one.
	if !tp.Filled && tp.OrderID == "" && tp.ClientID != "" && tp.Status != "REJECTED" {
		return SkipWaitingReconciliation, fmt.Errorf("TP submission identity pending")
	}
	if !tp.Filled && tp.OrderID == "" && tp.Status != "PLANNED" && tp.Status != "BELOW_MINIMUM" && tp.Status != "REJECTED" {
		return rollback(SkipNeedsContext, fmt.Errorf("anonymous TP cannot be replaced"))
	}
	if _, err := x.refreshTPProgressChecked(traceID, ctx); err != nil {
		return SkipNone, err
	}
	plan = readTPPlan(ctx)
	tp = &plan[index]
	pos, err := x.freshPosition(ctx.Symbol, ctx.Direction)
	if err != nil {
		return SkipNone, err
	}
	if pos == nil || pos.qty <= 0 {
		if err := x.finishTradeOrders(ctx); err != nil {
			return SkipNone, err
		}
		return SkipAlreadyFlat, x.finalizeManagedExit(ctx, signalID, map[string]interface{}{"state": string(StateClosed), "quantity": 0, "close_pending_json": "", "closed_at": time.Now().UTC(), "last_action": "TP_REPLACEMENT_ALREADY_FLAT"})
	}
	remaining := math.Max(0, tp.Quantity-tp.FilledQuantity)
	if tp.DesiredQuantity != nil {
		remaining = math.Min(remaining, *tp.DesiredQuantity)
	}
	if tp.Filled || remaining <= 0 {
		state := StateOpen
		if ctx.BreakevenApplied {
			state = StateBreakeven
		}
		return SkipDuplicate, x.finalizeManagedExit(ctx, signalID, map[string]interface{}{"state": string(state), "close_pending_json": "", "quantity": pos.qty, "last_action": "TP_REPLACEMENT_ALREADY_FILLED"})
	}
	rules, err := managed.MarketRules(ctx.Symbol)
	if err != nil {
		return SkipNone, err
	}
	qty, err := rules.FloorQuantity(types.SanitizeBaseQuantity(math.Min(remaining, pos.qty)), true)
	if err != nil {
		// e.g. a TP fill during cancellation left less than the market minimum.
		return rollback(SkipNeedsContext, fmt.Errorf("TP replacement %.8g below market minimum: %v", math.Min(remaining, pos.qty), err))
	}
	prior, err := x.exitRows(ctx, signalID)
	if err != nil {
		return SkipNone, err
	}
	tp.ReplacedBy = signalID
	if err = x.saveTPPlan(ctx, plan); err != nil {
		return SkipNone, err
	}
	row := x.newExitOrder(ctx, signalID, len(prior), pos.entryPrice, qty)
	intent.OrderID = row.ID
	intent.Full = qty >= pos.qty
	b, _ := json.Marshal(intent)
	if err = x.st.CopyTrade().CreateExitOrder(ctx, row, string(b)); err != nil {
		return SkipNone, err
	}
	ctx.ClosePendingJSON = string(b)
	ctx.State = string(StateClosePending)
	ctx.Version++
	if err = x.submitManaged(row, rules, true); err != nil {
		return SkipNone, err
	}
	return SkipNone, x.reconcileManagedClose(traceID, ctx)
}

func (x *Executor) recordTPReplacementFill(ctx *store.CopyTradeContext, intent managedExitIntent, row *store.CopyTradeOrder) error {
	plan := readTPPlan(ctx)
	for i := range plan {
		tp := &plan[i]
		if tp.Ordinal != intent.TPLevel {
			continue
		}
		if tp.ReplacedBy != intent.SignalID {
			return fmt.Errorf("TP replacement identity changed")
		}
		// Earlier exit generations (resubmissions) contribute their fills too.
		filled := intent.PriorFilled + row.ExecutedQty
		if filled <= tp.ReplacementFilled {
			return nil
		}
		tp.ReplacementFilled = filled
		tp.Filled = tp.FilledQuantity+tp.ReplacementFilled+1e-10 >= tp.Quantity
		return x.saveTPPlan(ctx, plan)
	}
	return fmt.Errorf("TP replacement allocation missing")
}

// precheckTPReduction verifies, before anything durable happens, that the
// replacement for TP<level> can be sent as a market reduce-only order at the
// live position size. A TP that already filled is a duplicate.
func (x *Executor) precheckTPReduction(traceID string, ctx *store.CopyTradeContext, managed types.ManagedOrderTrader, level int) (SkipReason, error) {
	if _, err := x.refreshTPProgressChecked(traceID, ctx); err != nil {
		return SkipNone, err
	}
	pos, err := x.freshPosition(ctx.Symbol, ctx.Direction)
	if err != nil {
		return SkipNone, err
	}
	if pos == nil || pos.qty <= 0 {
		return SkipNone, nil // the flat path settles the trade after the intent
	}
	rules, err := managed.MarketRules(ctx.Symbol)
	if err != nil {
		return SkipNone, err
	}
	for _, tp := range readTPPlan(ctx) {
		if tp.Ordinal != level {
			continue
		}
		if tp.Filled {
			return SkipDuplicate, nil
		}
		remaining := math.Max(0, tp.Quantity-tp.FilledQuantity)
		if tp.DesiredQuantity != nil {
			remaining = math.Min(remaining, *tp.DesiredQuantity)
		}
		if remaining <= 0 {
			return SkipNone, nil
		}
		want := math.Min(remaining, pos.qty)
		if _, err = rules.FloorQuantity(types.SanitizeBaseQuantity(want), true); err != nil {
			return SkipNeedsContext, fmt.Errorf("TP%d replacement %.8g cannot be sent as a market order (%v); TP kept, no exit recorded", level, want, err)
		}
	}
	return SkipNone, nil
}

// rollbackTPIntent releases a TP-replacement intent that never produced an
// exit order, returns the trade to OPEN/BREAKEVEN and re-places any TP this
// attempt cancelled. The entry latch stays closed (one-way by design).
func (x *Executor) rollbackTPIntent(traceID, signalID string, ctx *store.CopyTradeContext, cause error) error {
	state := StateOpen
	if ctx.BreakevenApplied {
		state = StateBreakeven
	}
	if err := x.persistContext(ctx, map[string]interface{}{"close_pending_json": "", "state": string(state), "last_action": "TP_REPLACEMENT_ROLLED_BACK", "last_error": "TP_REPLACEMENT_ROLLED_BACK: " + cause.Error()}); err != nil {
		return fmt.Errorf("%v; rollback failed: %w", cause, err)
	}
	x.events.Warn(traceID, signalID, "", EvExecutionError, fmt.Sprintf("%s %s: TP replacement not executable, intent rolled back: %v", ctx.Symbol, ctx.Direction, cause), map[string]interface{}{"context_id": ctx.ID})
	if pos, err := x.freshPosition(ctx.Symbol, ctx.Direction); err == nil && pos != nil && pos.qty > 0 {
		if err = x.restoreRemainingProtections(traceID, signalID, ctx, pos.qty); err != nil {
			x.events.Warn(traceID, signalID, "", EvExecutionError, "protection repair after TP rollback: "+err.Error(), nil)
		}
	}
	return cause
}
