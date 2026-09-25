package copytrader

import (
	"fmt"
	"time"

	"nofx/store"
	"nofx/trader/types"
)

const reconcileInterval = 45 * time.Second

// reconcileLoop periodically aligns local trade contexts with exchange truth:
//   - ENTRY_PENDING: detect limit fills (place protections) and entry timeouts
//   - OPEN/BREAKEVEN: detect closes (SL/TP hit or manual), detect TP partial
//     fills and apply auto-breakeven
//   - CLOSE_PENDING: confirm the close landed
//
// The exchange is the source of truth; the DB only mirrors it.
func (e *Engine) reconcileLoop() {
	defer e.wg.Done()
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	lastFull := time.Time{}
	for {
		select {
		case <-e.stopCh:
			return
		case <-ticker.C:
			e.mu.Lock()
			fast := false
			if contexts, err := e.st.CopyTrade().GetActiveContexts(e.traderID); err == nil {
				for _, c := range contexts {
					if c.EntryWorking || c.State == string(StateEntryPending) || c.State == string(StateClosePending) || c.LastError != "" {
						fast = true
						break
					}
				}
			}
			if fast || time.Since(lastFull) >= reconcileInterval {
				e.reconcileOnce()
				lastFull = time.Now()
			}
			e.mu.Unlock()
		}
	}
}

func (e *Engine) reconcileOnce() {
	ctxs, err := e.st.CopyTrade().GetActiveContexts(e.traderID)
	if err != nil {
		return
	}
	if len(ctxs) == 0 {
		e.recheckClosedTrades()
		return
	}
	for _, ctx := range ctxs {
		e.recoverPendingActions(ctx)
		if TradeState(ctx.State).IsTerminal() {
			continue
		}
		// A legacy entry may have a newly journaled Binance exit. Its exit
		// recovery is selected by durable intent, not by the entry's version.
		if ctx.ClosePendingJSON != "" {
			if _, ok := e.exec.ex.(types.ManagedOrderTrader); ok {
				if err := e.exec.reconcileManagedClose("reconcile-"+ctx.ID, ctx); err != nil {
					e.events.Warn("reconcile-"+ctx.ID, "", "", EvExecutionError, err.Error(), nil)
				}
			}
			continue
		}
		if ctx.ExecutionVersion > 0 && ctx.EntryPlanJSON != "" {
			if err := e.exec.reconcileManagedEntry("reconcile-"+ctx.ID, ctx); err != nil {
				e.events.Warn("reconcile-"+ctx.ID, "", "", EvExecutionError, err.Error(), nil)
			}
			if ctx.State == string(StateOpen) || ctx.State == string(StateBreakeven) {
				e.reconcileOpenTrade(ctx)
			}
			continue
		}
		if ctx.EntryWorking && ctx.State != string(StateEntryPending) {
			e.reconcileLegacyEntry("reconcile-"+ctx.ID, ctx)
		}
		switch TradeState(ctx.State) {
		case StateEntryPending:
			e.reconcileEntryPending(ctx)
		case StateOpen, StateBreakeven:
			e.reconcileOpenTrade(ctx)
		case StateClosePending:
			e.reconcileClosePending(ctx)
		case StateNew:
			// NEW older than 10 minutes means the open saga crashed mid-way.
			if time.Since(ctx.CreatedAt) > 10*time.Minute {
				e.exec.markContext(ctx, StateInvalid, map[string]interface{}{
					"last_error": "stale NEW context (open saga did not complete)",
				})
			}
		}
	}
	e.recheckClosedTrades()
}

// closedRecheckCycles is how many reconcile cycles a RECONCILE_CLOSED trade
// stays on the watch list waiting for its position to (not) reappear.
const closedRecheckCycles = 3

// recheckClosedTrades resurrects trades that reconcile closed on stale data.
// A genuinely closed position stays gone; if it is visible again within the
// watch window, the close verdict was wrong — the exchange still holds a live
// position whose protections were cancelled by the close cleanup and which no
// guard covers while the context is CLOSED. Restoring the context to OPEN
// puts it back under reconcile management; the SL guard re-places the stop
// on the same pass.
func (e *Engine) recheckClosedTrades() {
	for id, left := range e.closedRecheck {
		ctx, err := e.st.CopyTrade().GetContext(id)
		if err != nil {
			continue // transient read failure: retry next cycle
		}
		if ctx == nil || ctx.State != string(StateClosed) {
			delete(e.closedRecheck, id)
			continue
		}
		pos, err := e.exec.freshPosition(ctx.Symbol, ctx.Direction)
		if err != nil {
			continue // transient exchange failure: retry, do not consume a cycle
		}
		if pos == nil || pos.qty <= 0 {
			if left <= 1 {
				delete(e.closedRecheck, id) // confirmed gone, normal close
			} else {
				e.closedRecheck[id] = left - 1
			}
			continue
		}

		// Position is back: spurious close. Resurrect and re-protect.
		delete(e.closedRecheck, id)
		e.events.Error("reconcile-"+ctx.ID, "", "", EvExecutionError,
			fmt.Sprintf("%s %s position reappeared after RECONCILE_CLOSED (qty %.8g) — close was spurious, resurrecting trade and restoring protections",
				ctx.Symbol, ctx.Direction, pos.qty), nil)
		e.exec.updateContext(ctx, map[string]interface{}{
			"state":       string(StateOpen),
			"quantity":    pos.qty,
			"last_action": "RECOVERED_SPURIOUS_CLOSE",
			"closed_at":   nil,
		})
		ctx.State = string(StateOpen)
		ctx.Quantity = pos.qty
		// Run the open-trade pass immediately so the SL guard restores the
		// stop loss now instead of one cycle later.
		e.reconcileOpenTrade(ctx)
	}
}

// reconcileEntryPending handles limit entries: fill detection and timeout.
// Exchange truth is checked BEFORE the timeout path so a filled order is never
// marked EXPIRED, and a failed cancel never orphans a fill.
func (e *Engine) reconcileEntryPending(ctx *store.CopyTradeContext) {
	e.reconcileLegacyEntry("reconcile-"+ctx.ID, ctx)
}

// executedQtyOf extracts the filled quantity from a GetOrderStatus result,
// sanitized like confirmFill's path: contract-denominated exchanges convert
// fills with contracts*ctVal, whose float artifacts must not be persisted.
func executedQtyOf(status map[string]interface{}) float64 {
	if q, ok := status["executedQty"].(float64); ok && q > 0 {
		return types.SanitizeBaseQuantity(q)
	}
	return 0
}

// handleEntryFill promotes an ENTRY_PENDING context to OPEN and places
// protections from the stored TP plan.
func (e *Engine) handleEntryFill(traceID string, ctx *store.CopyTradeContext, status map[string]interface{}) {
	if err := e.exec.settleLegacyEntry(traceID, ctx, status); err != nil {
		e.events.Error(traceID, "", "", EvExecutionError, err.Error(), nil)
	}
}

// reconcileOpenTrade detects closes and TP partial fills.
func (e *Engine) reconcileOpenTrade(ctx *store.CopyTradeContext) {
	traceID := "reconcile-" + ctx.ID
	pos, err := e.exec.freshPosition(ctx.Symbol, ctx.Direction)
	if err != nil {
		return // transient
	}

	if pos == nil || pos.qty <= 0 {
		// Position not found. Do NOT close on the first miss: GetPositions is
		// served from a short-lived cache, so a snapshot taken just before
		// the entry filled makes a brand-new position invisible for one
		// cycle. Closing on that stale read cancels a freshly placed SL/TP
		// and permanently abandons a live position. Require two consecutive
		// cycles (using fresh position reads) before declaring the
		// trade closed.
		if !e.posMissSeen[ctx.ID] {
			if e.posMissSeen == nil {
				e.posMissSeen = make(map[string]bool)
			}
			e.posMissSeen[ctx.ID] = true
			e.events.Info(traceID, "", "", EvReconcile,
				fmt.Sprintf("%s position not found on exchange; re-checking next cycle before closing", ctx.Symbol), nil)
			return
		}
		delete(e.posMissSeen, ctx.ID)
		if err := e.exec.closeEntryEligibility(traceID, ctx, "POSITION_GONE"); err != nil {
			return
		}
		if fresh, err := e.exec.freshPosition(ctx.Symbol, ctx.Direction); err != nil || (fresh != nil && fresh.qty > 0) {
			return
		}
		// Position gone: SL hit, TP ladder completed, or closed manually.
		e.exec.cancelTradeOrdersQuiet(ctx.Symbol, ctx.Direction)
		e.exec.markContext(ctx, StateClosed, map[string]interface{}{"last_action": "RECONCILE_CLOSED"})
		e.events.Info(traceID, "", "", EvTradeClosed,
			fmt.Sprintf("%s position no longer on exchange (SL/TP hit or manual close); trade closed", ctx.Symbol), nil)
		// Keep watching for a few cycles: if the position reappears the close
		// was based on stale data and the trade must be resurrected.
		if e.closedRecheck == nil {
			e.closedRecheck = make(map[string]int)
		}
		e.closedRecheck[ctx.ID] = closedRecheckCycles
		return
	}
	delete(e.posMissSeen, ctx.ID)

	// Actual associated order fills are the only TP evidence. A manual
	// reduction still terminates replenishment, but never fabricates a TP hit.
	advanced := e.exec.refreshTPProgress(traceID, ctx)
	decreased := ctx.Quantity > 0 && pos.qty < ctx.Quantity-1e-10
	if advanced || decreased {
		if err := e.exec.closeEntryEligibility(traceID, ctx, "EXIT_FILL"); err != nil {
			e.events.Error(traceID, "", "", EvExecutionError, err.Error(), nil)
			return
		}
	}
	if ctx.Quantity != pos.qty {
		e.exec.updateContext(ctx, map[string]interface{}{"quantity": pos.qty})
	}
	if ctx.ExecutionVersion == 0 && pos.entryPrice > 0 && ctx.AvgFillPrice != pos.entryPrice {
		e.exec.updateContext(ctx, map[string]interface{}{"avg_fill_price": pos.entryPrice})
	}
	level := 1
	if ctx.BreakevenAfterTP && ctx.BreakevenTPLevel > 0 {
		level = ctx.BreakevenTPLevel
	}
	if !ctx.BreakevenApplied && confirmedTPLevel(ctx, level) && e.breakevenWanted(ctx) {
		e.applyBreakeven(traceID, ctx, pos.qty)
	}
	if err := e.exec.restoreRemainingProtections(traceID, "", ctx, pos.qty); err != nil {
		e.exec.updateContext(ctx, map[string]interface{}{"last_error": err.Error()})
		e.events.Warn(traceID, "", "", EvExecutionError, "protection reconciliation: "+err.Error(), nil)
	}

}

// breakevenWanted reports whether this trade should move its SL to entry
// after the first TP fill (global config; per-trade author rules are OR-ed in).
func (e *Engine) breakevenWanted(ctx *store.CopyTradeContext) bool {
	return e.cfg.AutoBreakevenAfterTP || ctx.BreakevenAfterTP
}

// applyBreakeven moves the SL to the average entry price.
func (e *Engine) applyBreakeven(traceID string, ctx *store.CopyTradeContext, qty float64) {
	if ctx.AvgFillPrice <= 0 {
		return
	}
	if err := e.exec.ensureStopProtection(traceID, "", ctx, qty); err != nil {
		return
	}
	if err := e.exec.closeEntryEligibility(traceID, ctx, "BREAKEVEN"); err != nil {
		e.events.Warn(traceID, "", "", EvExecutionError, err.Error(), nil)
		return
	}
	// A limit fill may race cancellation and change both the average and size.
	entry := ctx.AvgFillPrice
	pos, err := e.exec.freshPosition(ctx.Symbol, ctx.Direction)
	if err != nil || pos == nil || pos.qty <= 0 {
		return
	}
	qty = pos.qty
	if ctx.ExecutionVersion == 0 && pos.entryPrice > 0 {
		entry = pos.entryPrice
	}
	if tighterStop(ctx.Direction, ctx.StopLossPrice, entry) {
		entry = ctx.StopLossPrice
	}
	if err := e.exec.cancelStopLossOrders(ctx.Symbol, ctx.Direction); err != nil {
		e.events.Warn(traceID, "", "", EvExecutionError, fmt.Sprintf("breakeven: cancel old SL failed: %v", err), nil)
	}
	if err := e.exec.ex.SetStopLoss(ctx.Symbol, positionSideOf(ctx.Direction), qty, entry); err != nil {
		e.events.Error(traceID, "", "", EvExecutionError,
			fmt.Sprintf("breakeven SL failed (will retry next cycle): %v", err), nil)
		// The old SL may already be cancelled: restore protection at the
		// previous price so the position is not naked until the retry.
		if ctx.StopLossPrice > 0 && ctx.StopLossPrice != entry {
			if rerr := e.exec.ex.SetStopLoss(ctx.Symbol, positionSideOf(ctx.Direction), qty, ctx.StopLossPrice); rerr != nil {
				e.events.Error(traceID, "", "", EvExecutionError,
					fmt.Sprintf("breakeven: restore previous SL @ %.8g also failed (SL guard will retry): %v", ctx.StopLossPrice, rerr), nil)
			}
		}
		return
	}
	updates := map[string]interface{}{
		"stop_loss_price":   entry,
		"breakeven_applied": true,
	}
	if CanTransition(TradeState(ctx.State), StateBreakeven) {
		updates["state"] = string(StateBreakeven)
		ctx.State = string(StateBreakeven)
	}
	e.exec.updateContext(ctx, updates)
	e.events.Success(traceID, "", "", EvSLSet,
		fmt.Sprintf("auto-breakeven: %s SL moved to entry %.8g after TP fill", ctx.Symbol, entry), 0, nil)
}

// reconcileClosePending confirms a submitted close actually landed.
func (e *Engine) reconcileClosePending(ctx *store.CopyTradeContext) {
	pos, err := e.exec.freshPosition(ctx.Symbol, ctx.Direction)
	if err != nil {
		return
	}
	if pos == nil || pos.qty <= 0 {
		e.exec.cancelTradeOrdersQuiet(ctx.Symbol, ctx.Direction)
		e.exec.markContext(ctx, StateClosed, nil)
		return
	}
	// Close was submitted but position persists: surface it loudly.
	if time.Since(ctx.UpdatedAt) > 5*time.Minute {
		e.events.Error("reconcile-"+ctx.ID, "", "", EvExecutionError,
			fmt.Sprintf("%s close submitted >5m ago but position still open — manual check required", ctx.Symbol), nil)
	}
}

// planFromContext rebuilds an OpenPlan for protection placement after a limit
// fill, using the TP plan stored at submit time (Quantity field holds ratios).
func (e *Engine) planFromContext(ctx *store.CopyTradeContext) *OpenPlan {
	return recipePlan(ctx)
}
