package copytrader

import (
	"encoding/json"
	"fmt"
	"time"

	"nofx/store"
)

// Legacy entries retain their original one-leg behavior. The common settlement
// path protects continuous partial fills and cancellation races immediately.
func (x *Executor) settleLegacyEntry(traceID string, ctx *store.CopyTradeContext, status map[string]interface{}) error {
	st, _ := status["status"].(string)
	filled := executedQtyOf(status)
	terminal := orderTerminal(st)
	if st == "FILLED" && filled <= 0 {
		return fmt.Errorf("filled entry missing confirmed executed quantity")
	}
	if filled <= 0 {
		if terminal && ctx.OpenedAt == nil {
			x.markContext(ctx, StateCancelled, map[string]interface{}{"entry_working": false, "quantity": 0, "last_action": "ENTRY_" + st})
		}
		return nil
	}
	if ctx.TPRecipeJSON == "" {
		var prices, ratios []float64
		for _, tp := range readTPPlan(ctx) {
			prices = append(prices, tp.Price)
			ratios = append(ratios, tp.Quantity)
		}
		if ctx.OpenedAt == nil {
			if err := x.persistContext(ctx, map[string]interface{}{"tp_recipe_json": recipeJSON(prices, ratios), "tp_plan_json": ""}); err != nil {
				return err
			}
		}
	}
	avg, _ := status["avgPrice"].(float64)
	if avg <= 0 {
		if pos, err := x.freshPosition(ctx.Symbol, ctx.Direction); err == nil && pos != nil {
			avg = pos.entryPrice
		}
	}
	qty := filled
	if ctx.OpenedAt != nil {
		pos, err := x.freshPosition(ctx.Symbol, ctx.Direction)
		if err != nil {
			return err
		}
		if pos == nil {
			qty = 0
		} else {
			qty = pos.qty
		}
	}
	updates := map[string]interface{}{"avg_fill_price": avg, "quantity": qty, "entry_filled_quantity": filled, "entry_working": !terminal, "state": string(StateOpen)}
	if ctx.BreakevenApplied {
		updates["state"] = string(StateBreakeven)
	}
	if ctx.OpenedAt == nil {
		now := time.Now().UTC()
		updates["opened_at"] = &now
	}
	if err := x.persistContext(ctx, updates); err != nil {
		return err
	}
	x.events.Success(traceID, "", "", EvEntryFilled, fmt.Sprintf("entry %s: %s filled %.8g @ %.8g; unfilled remainder active=%t", st, ctx.Symbol, filled, avg, !terminal), 0, nil)
	if qty > 0 {
		if err := x.ensureStopProtection(traceID, "", ctx, qty); err != nil {
			_ = x.persistContext(ctx, map[string]interface{}{"entry_disabled": true, "last_error": "UNPROTECTED: " + err.Error()})
			if !terminal && x.gridEx != nil {
				_ = x.gridEx.CancelOrder(ctx.Symbol, ctx.EntryOrderID)
			}
			closeErr := x.emergencyClose(traceID, "", ctx.Symbol, ctx.Direction)
			return fmt.Errorf("partial entry SL failed: %v; emergency close: %v", err, closeErr)
		}
		if err := x.restoreRemainingProtections(traceID, "", ctx, qty); err != nil {
			return err
		}
		if avg <= 0 {
			return fmt.Errorf("entry filled and protected; actual average fill price is not confirmed")
		}
	}
	return nil
}

func (x *Executor) cancelLegacyRemainder(traceID string, ctx *store.CopyTradeContext) error {
	if !ctx.EntryWorking && ctx.State != string(StateEntryPending) {
		return nil
	}
	if ctx.EntryOrderID == "" || x.gridEx == nil {
		return fmt.Errorf("pending entry cannot be precisely cancelled")
	}
	status, err := x.ex.GetOrderStatus(ctx.Symbol, ctx.EntryOrderID)
	if err != nil {
		return err
	}
	st, _ := status["status"].(string)
	if !orderTerminal(st) {
		cancelErr := x.gridEx.CancelOrder(ctx.Symbol, ctx.EntryOrderID)
		status, err = x.ex.GetOrderStatus(ctx.Symbol, ctx.EntryOrderID)
		if err != nil {
			return fmt.Errorf("post-cancel entry state unknown: %w", err)
		}
		st, _ = status["status"].(string)
		if !orderTerminal(st) {
			return fmt.Errorf("entry cancel unconfirmed (%s): %v", st, cancelErr)
		}
	}
	return x.settleLegacyEntry(traceID, ctx, status)
}

func (x *Executor) cancelTrackedTPs(ctx *store.CopyTradeContext) error {
	for _, tp := range readTPPlan(ctx) {
		if tp.Filled {
			continue
		}
		if tp.OrderID == "" {
			return fmt.Errorf("legacy anonymous TP cannot be cancelled precisely; manual review required")
		}
		if x.gridEx == nil {
			return fmt.Errorf("exchange lacks precise TP cancellation")
		}
		if err := x.gridEx.CancelOrder(ctx.Symbol, tp.OrderID); err != nil {
			return err
		}
		after, err := x.ex.GetOrderStatus(ctx.Symbol, tp.OrderID)
		if err != nil {
			return err
		}
		status, _ := after["status"].(string)
		if !orderTerminal(status) {
			return fmt.Errorf("TP cancellation not confirmed")
		}
		if executedQtyOf(after) > tp.FilledQuantity {
			x.refreshTPProgress("tp-update", ctx)
			return fmt.Errorf("TP filled while updating; reconcile before retry")
		}
	}
	return nil
}

func (e *Engine) reconcileLegacyEntry(traceID string, ctx *store.CopyTradeContext) {
	status, err := e.exec.ex.GetOrderStatus(ctx.Symbol, ctx.EntryOrderID)
	if err != nil {
		return
	}
	if err = e.exec.settleLegacyEntry(traceID, ctx, status); err != nil {
		e.events.Error(traceID, "", "", EvExecutionError, err.Error(), nil)
		return
	}
	if (ctx.EntryWorking || ctx.State == string(StateEntryPending)) && (ctx.EntryDisabled || (e.cfg.EntryTimeoutMinutes > 0 && time.Since(ctx.CreatedAt) > time.Duration(e.cfg.EntryTimeoutMinutes)*time.Minute)) {
		if err = e.exec.closeEntryEligibility(traceID, ctx, "ENTRY_TIMEOUT_OR_CANCEL"); err != nil {
			e.events.Warn(traceID, "", "", EvExecutionError, err.Error(), nil)
			return
		}
		if ctx.OpenedAt == nil {
			e.exec.markContext(ctx, StateExpired, nil)
			e.events.Info(traceID, "", "", EvTradeExpired, "entry timed out; no fills", nil)
		}
	}
}

func recipePlan(ctx *store.CopyTradeContext) *OpenPlan {
	var r TPRecipe
	if json.Unmarshal([]byte(ctx.TPRecipeJSON), &r) != nil || r.Version != 1 {
		for _, tp := range readTPPlan(ctx) {
			r.Prices = append(r.Prices, tp.Price)
			r.Ratios = append(r.Ratios, tp.Quantity)
		}
	}
	return &OpenPlan{Symbol: ctx.Symbol, Direction: Direction(ctx.Direction), StopLoss: ctx.StopLossPrice, TPPrices: r.Prices, TPRatios: r.Ratios, Leverage: ctx.Leverage}
}
