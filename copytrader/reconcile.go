package copytrader

import (
	"encoding/json"
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
	ticker := time.NewTicker(reconcileInterval)
	defer ticker.Stop()
	for {
		select {
		case <-e.stopCh:
			return
		case <-ticker.C:
			e.mu.Lock()
			e.reconcileOnce()
			e.mu.Unlock()
		}
	}
}

func (e *Engine) reconcileOnce() {
	ctxs, err := e.st.CopyTrade().GetActiveContexts(e.traderID)
	if err != nil || len(ctxs) == 0 {
		return
	}
	for _, ctx := range ctxs {
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
		pos, err := e.exec.findPosition(ctx.Symbol, ctx.Direction)
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
	traceID := "reconcile-" + ctx.ID

	if ctx.EntryOrderID != "" {
		status, err := e.exec.ex.GetOrderStatus(ctx.Symbol, ctx.EntryOrderID)
		if err == nil {
			st, _ := status["status"].(string)
			switch st {
			case "FILLED":
				e.handleEntryFill(traceID, ctx, status)
				return
			case "CANCELED", "CANCELLED", "EXPIRED", "REJECTED":
				// A cancelled order can still carry partial fills (manual
				// cancel or timeout cancel racing a fill). That quantity is
				// a LIVE position — it must get its protections, not be
				// orphaned by marking the context cancelled.
				if q := executedQtyOf(status); q > 0 {
					e.events.Warn(traceID, "", "", EvReconcile,
						fmt.Sprintf("%s entry order %s after partial fill qty=%.8g — opening with protections", ctx.Symbol, st, q), nil)
					e.handleEntryFill(traceID, ctx, status)
					return
				}
				e.exec.markContext(ctx, StateCancelled, map[string]interface{}{"last_action": "ENTRY_" + st})
				e.events.Info(traceID, "", "", EvTradeCancelled,
					fmt.Sprintf("%s entry order %s on exchange", ctx.Symbol, st), nil)
				return
			}
		}
	}

	// Timeout: cancel entries that never filled. Only mark EXPIRED once the
	// cancel is confirmed; a failed cancel retries next cycle (the order may
	// have just filled — the status check above will then pick it up).
	if e.cfg.EntryTimeoutMinutes > 0 &&
		time.Since(ctx.CreatedAt) > time.Duration(e.cfg.EntryTimeoutMinutes)*time.Minute {
		if ctx.EntryOrderID != "" && e.exec.gridEx != nil {
			if err := e.exec.gridEx.CancelOrder(ctx.Symbol, ctx.EntryOrderID); err != nil {
				e.events.Warn(traceID, "", "", EvExecutionError,
					fmt.Sprintf("entry timeout cancel failed, retrying next cycle: %v", err), nil)
				return
			}
			// The cancel may have raced a partial fill; re-check before
			// declaring the entry dead. If the status read fails, retry the
			// whole path next cycle rather than risk orphaning a fill (the
			// CANCELED branch above will then settle it).
			status, err := e.exec.ex.GetOrderStatus(ctx.Symbol, ctx.EntryOrderID)
			if err != nil {
				e.events.Warn(traceID, "", "", EvExecutionError,
					fmt.Sprintf("post-cancel status check failed, retrying next cycle: %v", err), nil)
				return
			}
			if q := executedQtyOf(status); q > 0 {
				e.events.Warn(traceID, "", "", EvReconcile,
					fmt.Sprintf("%s entry partially filled qty=%.8g before timeout cancel — opening with protections", ctx.Symbol, q), nil)
				e.handleEntryFill(traceID, ctx, status)
				return
			}
		}
		e.exec.markContext(ctx, StateExpired, map[string]interface{}{"last_action": "ENTRY_TIMEOUT"})
		e.events.Info(traceID, "", "", EvTradeExpired,
			fmt.Sprintf("%s entry not filled within %dm, cancelled", ctx.Symbol, e.cfg.EntryTimeoutMinutes), nil)
	}
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
	avgPrice := ctx.PlannedEntryPrice
	filledQty := ctx.Quantity
	if p, ok := status["avgPrice"].(float64); ok && p > 0 {
		avgPrice = p
	}
	if q := executedQtyOf(status); q > 0 {
		filledQty = q
	}
	now := time.Now().UTC()
	e.exec.updateContext(ctx, map[string]interface{}{
		"state":          string(StateOpen),
		"avg_fill_price": avgPrice,
		"quantity":       filledQty,
		"opened_at":      &now,
	})
	ctx.State = string(StateOpen)
	ctx.AvgFillPrice = avgPrice
	ctx.Quantity = filledQty
	e.events.Success(traceID, "", "", EvEntryFilled,
		fmt.Sprintf("limit entry filled: %s %s qty=%.8g @ %.8g", ctx.Symbol, ctx.Direction, filledQty, avgPrice), 0, nil)

	// Place protections from the stored TP plan (ratios stashed at submit).
	plan := e.planFromContext(ctx)
	if err := e.exec.placeProtections(traceID, "", plan, ctx, filledQty, avgPrice); err != nil {
		e.events.Error(traceID, "", "", EvExecutionError, err.Error(), nil)
	} else {
		e.events.Success(traceID, "", "", EvTradeOpened,
			fmt.Sprintf("%s %s opened via limit fill", ctx.Symbol, ctx.Direction), 0, nil)
	}
}

// reconcileOpenTrade detects closes and TP partial fills.
func (e *Engine) reconcileOpenTrade(ctx *store.CopyTradeContext) {
	traceID := "reconcile-" + ctx.ID
	pos, err := e.exec.findPosition(ctx.Symbol, ctx.Direction)
	if err != nil {
		return // transient
	}

	if pos == nil || pos.qty <= 0 {
		// Position not found. Do NOT close on the first miss: GetPositions is
		// served from a short-lived cache, so a snapshot taken just before
		// the entry filled makes a brand-new position invisible for one
		// cycle. Closing on that stale read cancels a freshly placed SL/TP
		// and permanently abandons a live position. Require two consecutive
		// cycles (45s apart, far beyond any cache TTL) before declaring the
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

	// TP partial-fill detection: position shrank relative to our record.
	if ctx.Quantity > 0 && pos.qty < ctx.Quantity*0.999 {
		newHits := ctx.TPHitCount + 1
		e.exec.updateContext(ctx, map[string]interface{}{
			"quantity":     pos.qty,
			"tp_hit_count": newHits,
		})
		ctx.Quantity = pos.qty
		ctx.TPHitCount = newHits
		e.events.Success(traceID, "", "", EvReconcile,
			fmt.Sprintf("%s TP level filled (hit #%d), remaining qty %.8g", ctx.Symbol, newHits, pos.qty), 0, nil)

		// Resize the SL to the remaining quantity so protection stays exact
		// (the breakeven path below re-places the SL itself).
		if !e.breakevenWanted(ctx) && ctx.StopLossPrice > 0 {
			if err := e.exec.cancelStopLossOrders(ctx.Symbol, ctx.Direction); err == nil {
				if serr := e.exec.ex.SetStopLoss(ctx.Symbol, positionSideOf(ctx.Direction), pos.qty, ctx.StopLossPrice); serr != nil {
					// Old SL is already cancelled: the position is naked
					// until the SL guard below (or next cycle) restores it.
					e.events.Warn(traceID, "", "", EvExecutionError,
						fmt.Sprintf("%s SL resize after TP fill failed (SL guard will restore): %v", ctx.Symbol, serr), nil)
				}
			}
		}
	}

	// Position grew relative to our record: a second open on the same
	// symbol+direction merged into this position, or an orphaned remnant from
	// an earlier incident got absorbed (SNDKUSDT: 0.27 on the exchange, SL
	// sized for 0.12). Adopt the exchange quantity and resize the SL so the
	// whole position is protected — over-protecting is the safe direction.
	if ctx.Quantity > 0 && pos.qty > ctx.Quantity*1.001 {
		e.events.Warn(traceID, "", "", EvExecutionError,
			fmt.Sprintf("%s position (%.8g) larger than tracked quantity (%.8g) — adopting exchange size and resizing SL to cover it",
				ctx.Symbol, pos.qty, ctx.Quantity), nil)
		e.exec.updateContext(ctx, map[string]interface{}{"quantity": pos.qty})
		ctx.Quantity = pos.qty
		if ctx.StopLossPrice > 0 {
			if err := e.exec.cancelStopLossOrders(ctx.Symbol, ctx.Direction); err == nil {
				if serr := e.exec.ex.SetStopLoss(ctx.Symbol, positionSideOf(ctx.Direction), pos.qty, ctx.StopLossPrice); serr != nil {
					e.events.Warn(traceID, "", "", EvExecutionError,
						fmt.Sprintf("%s SL resize to grown position failed (SL guard will restore): %v", ctx.Symbol, serr), nil)
				}
			}
		}
	}

	// Auto-breakeven after the first TP (config or author rule). Evaluated on
	// every cycle so a previously failed attempt (or a crash between TP fill
	// and SL move) is retried until it lands.
	if !ctx.BreakevenApplied && ctx.TPHitCount >= 1 && e.breakevenWanted(ctx) {
		e.applyBreakeven(traceID, ctx, pos.qty)
	}

	// SL guard: an OPEN/BREAKEVEN position must always have a live stop order.
	// Covers every naked-position path (open saga crash after entry, SL update
	// failure, partial-close re-issue failure, failed emergency close).
	if ctx.StopLossPrice > 0 {
		if exists, ok := e.exec.hasStopLossOrder(ctx.Symbol); ok && !exists {
			e.events.Warn(traceID, "", "", EvExecutionError,
				fmt.Sprintf("%s has a live position but NO stop-loss order — re-placing @ %.8g (qty %.8g)",
					ctx.Symbol, ctx.StopLossPrice, pos.qty), nil)
			if err := e.exec.ex.SetStopLoss(ctx.Symbol, positionSideOf(ctx.Direction), pos.qty, ctx.StopLossPrice); err != nil {
				e.events.Error(traceID, "", "", EvExecutionError,
					fmt.Sprintf("SL guard re-place failed (will retry next cycle): %v", err), nil)
			} else {
				e.events.Success(traceID, "", "", EvSLSet,
					fmt.Sprintf("SL guard restored stop loss @ %.8g (qty %.8g)", ctx.StopLossPrice, pos.qty), 0, nil)
			}
		}
	}
}

// breakevenWanted reports whether this trade should move its SL to entry
// after the first TP fill (global config; per-trade author rules are OR-ed in).
func (e *Engine) breakevenWanted(ctx *store.CopyTradeContext) bool {
	return e.cfg.AutoBreakevenAfterTP || ctx.BreakevenAfterTP
}

// applyBreakeven moves the SL to the average entry price.
func (e *Engine) applyBreakeven(traceID string, ctx *store.CopyTradeContext, qty float64) {
	entry := ctx.AvgFillPrice
	if entry <= 0 {
		return
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
	pos, err := e.exec.findPosition(ctx.Symbol, ctx.Direction)
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
	var tpPlan []TPPlanEntry
	if ctx.TPPlanJSON != "" {
		_ = json.Unmarshal([]byte(ctx.TPPlanJSON), &tpPlan)
	}
	var prices, ratios []float64
	for _, tp := range tpPlan {
		prices = append(prices, tp.Price)
		ratios = append(ratios, tp.Quantity) // ratio stashed in Quantity pre-fill
	}
	return &OpenPlan{
		Symbol:    ctx.Symbol,
		Direction: Direction(ctx.Direction),
		StopLoss:  ctx.StopLossPrice,
		TPPrices:  prices,
		TPRatios:  ratios,
		Leverage:  ctx.Leverage,
	}
}
