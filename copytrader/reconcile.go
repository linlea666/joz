package copytrader

import (
	"encoding/json"
	"fmt"
	"time"

	"nofx/store"
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
		}
		e.exec.markContext(ctx, StateExpired, map[string]interface{}{"last_action": "ENTRY_TIMEOUT"})
		e.events.Info(traceID, "", "", EvTradeExpired,
			fmt.Sprintf("%s entry not filled within %dm, cancelled", ctx.Symbol, e.cfg.EntryTimeoutMinutes), nil)
	}
}

// handleEntryFill promotes an ENTRY_PENDING context to OPEN and places
// protections from the stored TP plan.
func (e *Engine) handleEntryFill(traceID string, ctx *store.CopyTradeContext, status map[string]interface{}) {
	avgPrice := ctx.PlannedEntryPrice
	filledQty := ctx.Quantity
	if p, ok := status["avgPrice"].(float64); ok && p > 0 {
		avgPrice = p
	}
	if q, ok := status["executedQty"].(float64); ok && q > 0 {
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
		// Position gone: SL hit, TP ladder completed, or closed manually.
		e.exec.cancelAllQuiet(ctx.Symbol)
		e.exec.markContext(ctx, StateClosed, map[string]interface{}{"last_action": "RECONCILE_CLOSED"})
		e.events.Info(traceID, "", "", EvTradeClosed,
			fmt.Sprintf("%s position no longer on exchange (SL/TP hit or manual close); trade closed", ctx.Symbol), nil)
		return
	}

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
			if err := e.exec.ex.CancelStopLossOrders(ctx.Symbol); err == nil {
				_ = e.exec.ex.SetStopLoss(ctx.Symbol, positionSideOf(ctx.Direction), pos.qty, ctx.StopLossPrice)
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
	if err := e.exec.ex.CancelStopLossOrders(ctx.Symbol); err != nil {
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
		e.exec.cancelAllQuiet(ctx.Symbol)
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
