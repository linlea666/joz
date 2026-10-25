package copytrader

import (
	"fmt"
	"math"
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
	e.recoverOrphanActions()
	if terminal, err := e.st.CopyTrade().TerminalPendingContexts(e.traderID); err == nil {
		bound, bindErr := e.st.CopyTrade().TraderExchangeID(e.traderID)
		if bindErr == nil {
			for _, c := range terminal {
				if c.ExchangeID != "" && c.ExchangeID != bound {
					continue
				}
				err = e.exec.finishTradeOrders(c)
				e.recordRecovery(c, "terminal_cleanup", err, false)
			}
		}
	}
	ctxs, err := e.st.CopyTrade().GetActiveContexts(e.traderID)
	if err != nil {
		return
	}
	if len(ctxs) == 0 {
		e.recheckClosedTrades()
		return
	}
	for _, ctx := range ctxs {
		bound, bindErr := e.st.CopyTrade().TraderExchangeID(e.traderID)
		if bindErr != nil || (ctx.ExchangeID != "" && ctx.ExchangeID != bound) {
			e.events.Error("reconcile-"+ctx.ID, "", "", EvExecutionError, "frozen exchange binding mismatch; recovery blocked", nil)
			continue
		}
		if ctx.ExchangeID == "" {
			if err := e.exec.persistContext(ctx, map[string]interface{}{"exchange_id": bound}); err != nil {
				continue
			}
		}
		if ctx.TPUpdateIntentJSON != "" {
			if _, err := e.exec.updateTakeProfits("reconcile-"+ctx.ID, "", ctx, nil, nil); err != nil {
				e.events.Warn("reconcile-"+ctx.ID, "", "", EvExecutionError, err.Error(), nil)
				continue
			}
		}
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
			entryErr := e.exec.reconcileManagedEntry("reconcile-"+ctx.ID, ctx)
			message := ""
			if entryErr != nil {
				message = entryErr.Error()
			}
			e.events.recoveryResult(ctx.ID, "entry_recovery", message, time.Now().UTC())
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
			// Limit entries: exchange truth is checked BEFORE the timeout path
			// so a filled order is never marked EXPIRED, and a failed cancel
			// never orphans a fill.
			e.reconcileLegacyEntry("reconcile-"+ctx.ID, ctx)
		case StateOpen, StateBreakeven:
			e.reconcileOpenTrade(ctx)
		case StateClosePending:
			e.reconcileClosePending(ctx)
		case StateNew:
			// NEW older than 10 minutes means the open saga crashed mid-way.
			if time.Since(ctx.CreatedAt) > 10*time.Minute && ctx.LastAction != "ENTRY_SUBMITTING" {
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

		delete(e.closedRecheck, id)
		if refuse := e.resurrectionRefusal(ctx, pos); refuse != "" {
			// Never claim a position this trade cannot prove is its own.
			e.events.Error("reconcile-"+ctx.ID, "", "", EvExecutionError,
				fmt.Sprintf("%s %s position (qty %.8g @ %.8g) visible after RECONCILE_CLOSED is NOT adopted: %s — manual review required",
					ctx.Symbol, ctx.Direction, pos.qty, pos.entryPrice, refuse), map[string]interface{}{"context_id": ctx.ID})
			continue
		}
		// Position is back: spurious close. Resurrect and re-protect.
		e.events.Error("reconcile-"+ctx.ID, "", "", EvExecutionError,
			fmt.Sprintf("%s %s position reappeared after RECONCILE_CLOSED (qty %.8g) — close was spurious, resurrecting trade and restoring protections",
				ctx.Symbol, ctx.Direction, pos.qty), nil)
		e.exec.updateContext(ctx, map[string]interface{}{
			"state":       string(StateOpen),
			"quantity":    pos.qty,
			"last_action": lastActionSpuriousCloseRecovered,
			"closed_at":   nil,
		})
		ctx.State = string(StateOpen)
		ctx.Quantity = pos.qty
		// Run the open-trade pass immediately so the SL guard restores the
		// stop loss now instead of one cycle later.
		e.reconcileOpenTrade(ctx)
	}
}

// resurrectEntryTolerancePct bounds how far a reappearing position's average
// entry may differ from the closed trade's before it is treated as foreign.
const resurrectEntryTolerancePct = 0.5

// resurrectionRefusal returns why a position seen after RECONCILE_CLOSED must
// not be re-attached to ctx ("" = it may be). Venues with fresh position reads
// (Binance managed orders) confirmed the close with fresh data twice, so a
// position appearing later is a new (manual or foreign) position. Elsewhere it
// must match the old trade exactly and be owned by nobody else.
func (e *Engine) resurrectionRefusal(ctx *store.CopyTradeContext, pos *positionInfo) string {
	if _, ok := e.exec.ex.(types.ManagedOrderTrader); ok {
		return "close was confirmed by fresh position reads on a managed venue"
	}
	if _, ok := e.exec.ex.(types.FreshPositionReader); ok {
		return "close was confirmed by fresh position reads"
	}
	owners, err := e.st.CopyTrade().AccountContexts(e.traderID, ctx.Symbol, ctx.Direction)
	if err != nil {
		return "ownership lookup failed: " + err.Error()
	}
	for _, o := range owners {
		if o.ID != ctx.ID {
			return fmt.Sprintf("another active trade %s owns this account/symbol/direction", o.ID)
		}
	}
	if ctx.Quantity <= 0 {
		return "closed trade has no recorded quantity to match"
	}
	if !e.exec.sameVenueQuantity(ctx.Symbol, pos.qty, ctx.Quantity) {
		return fmt.Sprintf("quantity %.8g differs from the closed trade's %.8g", pos.qty, ctx.Quantity)
	}
	ref := ctx.AvgFillPrice
	if ref <= 0 {
		ref = ctx.PlannedEntryPrice
	}
	// Without any recorded entry reference only the exact-size match applies.
	if ref > 0 {
		if pos.entryPrice <= 0 {
			return "position entry price unavailable"
		}
		if math.Abs(pos.entryPrice-ref)/ref*100 > resurrectEntryTolerancePct {
			return fmt.Sprintf("entry %.8g differs from the closed trade's %.8g", pos.entryPrice, ref)
		}
	}
	return ""
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
		if err := e.exec.closeEntryEligibility(traceID, ctx, "POSITION_GONE"); err != nil {
			return
		}
		if fresh, err := e.exec.freshPosition(ctx.Symbol, ctx.Direction); err != nil || (fresh != nil && fresh.qty > 0) {
			return
		}
		// Position gone: SL hit, TP ladder completed, or closed manually.
		// Terminal cleanup clears stop_order_id; keep it as exit evidence.
		stopID := ctx.StopOrderID
		if err := e.exec.finishTradeOrders(ctx); err != nil {
			e.recordRecovery(ctx, "terminal_cleanup", err, true)
			return
		}
		e.recordRecovery(ctx, "terminal_cleanup", nil, true)
		e.events.recoveryResult(ctx.ID, "protection", "", time.Now().UTC())
		delete(e.posMissSeen, ctx.ID)
		ev := e.exec.collectExitEvidence(traceID, ctx, stopID)
		closeUpdates := map[string]interface{}{"last_action": "RECONCILE_CLOSED"}
		if ev.ClosedAt != nil && (ctx.OpenedAt == nil || !ev.ClosedAt.Before(*ctx.OpenedAt)) {
			closeUpdates["closed_at"] = ev.ClosedAt
		}
		if err := e.exec.markContext(ctx, StateClosed, closeUpdates); err != nil {
			return
		}
		pnl := "realized PnL unavailable"
		if ev.RealizedPnL != nil {
			pnl = fmt.Sprintf("realized PnL %.4f", *ev.RealizedPnL)
		}
		e.events.Info(traceID, "", "", EvTradeClosed,
			fmt.Sprintf("%s position no longer on exchange; trade closed: exit %s (%s), %s", ctx.Symbol, ev.Reason, ev.Source, pnl),
			map[string]interface{}{"context_id": ctx.ID, "exit": ev})
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
	// Growth beyond the tracked size has no fill evidence from this trade (its
	// own entry fills are settled by the entry paths): it may be a manual or
	// foreign position on the same side. Warn only; never adopt it or enlarge
	// this trade's SL/TP to cover it.
	protectQty := pos.qty
	if e.grownBeyondTracked(ctx, pos.qty) {
		protectQty = ctx.Quantity
		if ctx.LastAction != "POSITION_GROWTH_NOT_ADOPTED" {
			e.events.Warn(traceID, "", "", EvReconcile,
				fmt.Sprintf("%s %s exchange position %.8g exceeds tracked %.8g without fill evidence; not adopted, protections stay sized to %.8g — manual review required",
					ctx.Symbol, ctx.Direction, pos.qty, ctx.Quantity, ctx.Quantity), map[string]interface{}{"context_id": ctx.ID, "position_quantity": pos.qty})
			e.exec.updateContext(ctx, map[string]interface{}{"last_action": "POSITION_GROWTH_NOT_ADOPTED"})
		}
	} else if ctx.Quantity != pos.qty {
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
		e.applyBreakeven(traceID, ctx, protectQty)
	}
	e.recordRecovery(ctx, "protection", e.exec.restoreRemainingProtections(traceID, "", ctx, protectQty), false)

}

// sameVenueQuantity reports whether two base quantities are the same size at
// the venue's quantity precision (within half a lot step).
func (x *Executor) sameVenueQuantity(symbol string, a, b float64) bool {
	if math.Abs(a-b) <= 1e-9*math.Max(1, math.Max(a, b)) {
		return true
	}
	if m, ok := x.ex.(types.ManagedOrderTrader); ok {
		if rules, err := m.MarketRules(symbol); err == nil && rules.QuantityStep > 0 {
			return math.Abs(a-b) < rules.QuantityStep/2
		}
		return false
	}
	fa, errA := x.ex.FormatQuantity(symbol, a)
	fb, errB := x.ex.FormatQuantity(symbol, b)
	return errA == nil && errB == nil && fa == fb
}

// grownBeyondTracked: the exchange shows more than this trade has adopted.
func (e *Engine) grownBeyondTracked(ctx *store.CopyTradeContext, qty float64) bool {
	return ctx.Quantity > 0 && qty > ctx.Quantity && !e.exec.sameVenueQuantity(ctx.Symbol, qty, ctx.Quantity)
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
	if e.grownBeyondTracked(ctx, qty) {
		qty = ctx.Quantity // never size protection to un-adopted growth
	}
	if ctx.ExecutionVersion == 0 && pos.entryPrice > 0 {
		entry = pos.entryPrice
	}
	if tighterStop(ctx.Direction, ctx.StopLossPrice, entry) {
		entry = ctx.StopLossPrice
	}
	if err := e.exec.setStopProtection(traceID, "", ctx, qty, entry); err != nil {
		e.events.Error(traceID, "", "", EvExecutionError, err.Error(), nil)
		return
	}
	entry = ctx.StopLossPrice

	updates := map[string]interface{}{
		"stop_loss_price":   entry,
		"breakeven_applied": true,
	}
	if CanTransition(TradeState(ctx.State), StateBreakeven) {
		updates["state"] = string(StateBreakeven)
	}
	e.exec.updateContext(ctx, updates)
	e.events.Success(traceID, "", "", EvSLSet,
		fmt.Sprintf("auto-breakeven: %s SL confirmed at %.8g after TP fill", ctx.Symbol, entry), 0, nil)
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
