package copytrader

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"nofx/store"
	"nofx/trader/types"
)

// TPPlanEntry is one planned take-profit order (persisted on the context).
type TPPlanEntry struct {
	Price    float64 `json:"price"`
	Quantity float64 `json:"quantity"`
	OrderID  string  `json:"order_id,omitempty"`
	Filled   bool    `json:"filled,omitempty"`
}

// Executor turns validated instructions into idempotent exchange operations.
// Failure policy per saga step:
//   - entry failed          => trade INVALID, nothing to roll back
//   - entry ok, SL failed   => retry, then EMERGENCY CLOSE (never hold naked)
//   - SL ok, TP failed      => keep position (protected), warn
type Executor struct {
	traderID string
	ex       types.Trader
	gridEx   types.GridTrader // nil when the exchange has no native limit orders
	st       *store.Store
	events   *EventLogger
}

// NewExecutor wraps the exchange trader.
func NewExecutor(traderID string, ex types.Trader, st *store.Store, events *EventLogger) *Executor {
	gridEx, _ := ex.(types.GridTrader)
	return &Executor{traderID: traderID, ex: ex, gridEx: gridEx, st: st, events: events}
}

// retrySleep is time.Sleep, injectable so unit tests do not wait for real
// backoff delays.
var retrySleep = time.Sleep

// positionSideOf maps direction to the exchange positionSide parameter.
func positionSideOf(direction string) string {
	if direction == string(DirectionShort) {
		return "SHORT"
	}
	return "LONG"
}

// OpenPlan is the fully resolved, deterministic plan for an OPEN.
type OpenPlan struct {
	Symbol     string // canonical
	RawSymbol  string
	Direction  Direction
	EntryType  EntryPlanType
	EntryPrice float64 // limit price (LIMIT) or market reference (MARKET)
	Quantity   float64
	Leverage   int
	StopLoss   float64
	TPPrices   []float64
	TPRatios   []float64
	Sizing     *SizingResult
	RootMsgID  string
	ChannelID  string
}

// ExecuteOpen runs the OPEN saga and returns the created trade context.
func (x *Executor) ExecuteOpen(traceID, signalID string, plan *OpenPlan) (*store.CopyTradeContext, error) {
	// Best-effort account setup; failures here are tolerable on most exchanges.
	if err := x.ex.SetLeverage(plan.Symbol, plan.Leverage); err != nil {
		x.events.Warn(traceID, signalID, "", EvExecutionError,
			fmt.Sprintf("set leverage %dx failed (continuing): %v", plan.Leverage, err), nil)
	}

	qtyStr, err := x.ex.FormatQuantity(plan.Symbol, plan.Quantity)
	if err == nil {
		if q, perr := strconv.ParseFloat(qtyStr, 64); perr == nil && q > 0 {
			// Unit-mismatch guard: precision alignment may only nudge the
			// quantity (flooring to one step cuts at most ~50%; rounding up
			// adds at most half a step). A larger deviation means the
			// exchange returned a different UNIT (e.g. contract count
			// instead of base asset) — submitting it would trade a wildly
			// wrong size, so refuse instead.
			if q > plan.Quantity*1.5 || q < plan.Quantity*0.5 {
				x.events.Error(traceID, signalID, "", EvExecutionError,
					fmt.Sprintf("quantity sanity check failed: planned %.8g, formatted %.8g — unit mismatch suspected, order refused", plan.Quantity, q), nil)
				return nil, fmt.Errorf("quantity sanity check failed: planned %.8g vs formatted %.8g (unit mismatch suspected)", plan.Quantity, q)
			}
			plan.Quantity = q
		}
	}
	if plan.Quantity <= 0 {
		return nil, fmt.Errorf("quantity is zero after precision formatting")
	}

	ctx := &store.CopyTradeContext{
		ID:                uuid.NewString(),
		TraderID:          x.traderID,
		ChannelID:         plan.ChannelID,
		RootMessageID:     plan.RootMsgID,
		Symbol:            plan.Symbol,
		RawSymbol:         plan.RawSymbol,
		Direction:         string(plan.Direction),
		State:             string(StateNew),
		PlannedEntryPrice: plan.EntryPrice,
		Quantity:          plan.Quantity,
		Leverage:          plan.Leverage,
		StopLossPrice:     plan.StopLoss,
	}
	if err := x.st.CopyTrade().CreateContext(ctx); err != nil {
		return nil, fmt.Errorf("failed to persist trade context: %w", err)
	}

	switch plan.EntryType {
	case EntryPlanMarket:
		return x.executeMarketOpen(traceID, signalID, plan, ctx)
	case EntryPlanLimit:
		return x.executeLimitOpen(traceID, signalID, plan, ctx)
	default:
		x.markContext(ctx, StateInvalid, map[string]interface{}{"last_error": "unsupported entry plan"})
		return ctx, fmt.Errorf("unsupported entry plan %q", plan.EntryType)
	}
}

func (x *Executor) executeMarketOpen(traceID, signalID string, plan *OpenPlan, ctx *store.CopyTradeContext) (*store.CopyTradeContext, error) {
	start := time.Now()
	var order map[string]interface{}
	var err error
	if plan.Direction == DirectionLong {
		order, err = x.ex.OpenLong(plan.Symbol, plan.Quantity, plan.Leverage)
	} else {
		order, err = x.ex.OpenShort(plan.Symbol, plan.Quantity, plan.Leverage)
	}
	if err != nil {
		x.markContext(ctx, StateInvalid, map[string]interface{}{"last_error": err.Error()})
		x.events.Error(traceID, signalID, "", EvExecutionError, fmt.Sprintf("market entry failed: %v", err), nil)
		return ctx, fmt.Errorf("market entry failed: %w", err)
	}
	orderID := orderIDString(order)
	x.events.Success(traceID, signalID, "", EvEntrySubmitted,
		fmt.Sprintf("market %s %s qty=%.8g", plan.Direction, plan.Symbol, plan.Quantity),
		time.Since(start).Milliseconds(),
		map[string]interface{}{"order_id": orderID, "type": "MARKET"})

	// Confirm the actual fill (price can differ from the signal reference).
	avgPrice, filledQty := x.confirmFill(plan.Symbol, orderID, plan.Quantity, plan.EntryPrice)
	now := time.Now().UTC()
	x.updateContext(ctx, map[string]interface{}{
		"state":          string(StateOpen),
		"entry_order_id": orderID,
		"avg_fill_price": avgPrice,
		"quantity":       filledQty,
		"opened_at":      &now,
	})
	ctx.State = string(StateOpen)
	ctx.AvgFillPrice = avgPrice
	ctx.Quantity = filledQty
	x.events.Success(traceID, signalID, "", EvEntryFilled,
		fmt.Sprintf("filled %s %s qty=%.8g avg=%.8g", plan.Direction, plan.Symbol, filledQty, avgPrice), 0,
		map[string]interface{}{"avg_price": avgPrice, "filled_qty": filledQty})

	// Effective risk audit: market fills away from the reference silently
	// inflate the loss if the SL hits.
	if plan.StopLoss > 0 && plan.Sizing != nil && plan.Sizing.EstimatedRiskUSD > 0 {
		eff := EffectiveRisk(filledQty, avgPrice, plan.StopLoss)
		if eff > plan.Sizing.EstimatedRiskUSD*1.5 {
			x.events.Warn(traceID, signalID, "", EvRiskRejected,
				fmt.Sprintf("effective risk $%.2f exceeds planned $%.2f by >50%% (slippage)", eff, plan.Sizing.EstimatedRiskUSD), nil)
		}
	}

	if err := x.placeProtections(traceID, signalID, plan, ctx, filledQty, avgPrice); err != nil {
		return ctx, err
	}
	return ctx, nil
}

func (x *Executor) executeLimitOpen(traceID, signalID string, plan *OpenPlan, ctx *store.CopyTradeContext) (*store.CopyTradeContext, error) {
	if x.gridEx == nil {
		// No native limit orders on this exchange: refuse rather than fake it.
		x.markContext(ctx, StateInvalid, map[string]interface{}{"last_error": "exchange does not support limit entry"})
		return ctx, fmt.Errorf("exchange does not support limit entries")
	}
	side := "BUY"
	if plan.Direction == DirectionShort {
		side = "SELL"
	}
	clientID := "ct-" + ctx.ID[:8] + "-e"
	res, err := x.gridEx.PlaceLimitOrder(&types.LimitOrderRequest{
		Symbol:       plan.Symbol,
		Side:         side,
		PositionSide: positionSideOf(string(plan.Direction)),
		Price:        plan.EntryPrice,
		Quantity:     plan.Quantity,
		Leverage:     plan.Leverage,
		ClientID:     clientID,
	})
	if err != nil {
		x.markContext(ctx, StateInvalid, map[string]interface{}{"last_error": err.Error()})
		x.events.Error(traceID, signalID, "", EvExecutionError, fmt.Sprintf("limit entry failed: %v", err), nil)
		return ctx, fmt.Errorf("limit entry failed: %w", err)
	}
	// Persist the TP plan for post-fill placement by the reconciler.
	tpPlan := make([]TPPlanEntry, 0, len(plan.TPPrices))
	for i, p := range plan.TPPrices {
		ratio := 0.0
		if i < len(plan.TPRatios) {
			ratio = plan.TPRatios[i]
		}
		tpPlan = append(tpPlan, TPPlanEntry{Price: p, Quantity: ratio}) // quantity resolved at fill time from ratio
	}
	tpJSON, _ := json.Marshal(tpPlan)
	x.updateContext(ctx, map[string]interface{}{
		"state":          string(StateEntryPending),
		"entry_order_id": res.OrderID,
		"tp_plan_json":   string(tpJSON),
	})
	ctx.State = string(StateEntryPending)
	ctx.EntryOrderID = res.OrderID
	x.events.Success(traceID, signalID, "", EvEntrySubmitted,
		fmt.Sprintf("limit %s %s qty=%.8g @ %.8g", plan.Direction, plan.Symbol, plan.Quantity, plan.EntryPrice), 0,
		map[string]interface{}{"order_id": res.OrderID, "type": "LIMIT"})
	return ctx, nil
}

// placeProtections sets SL first (mandatory), then the TP ladder.
// Called for market fills immediately and by the reconciler after limit fills.
func (x *Executor) placeProtections(traceID, signalID string, plan *OpenPlan, ctx *store.CopyTradeContext, filledQty, avgPrice float64) error {
	posSide := positionSideOf(string(plan.Direction))

	// --- Stop loss (mandatory, retried, emergency close on failure) ---
	var slErr error
	for attempt := 1; attempt <= 3; attempt++ {
		slErr = x.ex.SetStopLoss(plan.Symbol, posSide, filledQty, plan.StopLoss)
		if slErr == nil {
			break
		}
		retrySleep(time.Duration(attempt) * time.Second)
	}
	if slErr != nil {
		x.events.Error(traceID, signalID, "", EvEmergencyClose,
			fmt.Sprintf("stop loss placement failed after retries (%v) — closing position immediately", slErr), nil)
		if closeErr := x.emergencyClose(traceID, signalID, plan.Symbol, string(plan.Direction)); closeErr != nil {
			// Both SL and emergency close failed: the position is live and
			// unprotected. Keep the context in its non-terminal state so the
			// reconciler keeps retrying SL placement every cycle.
			x.updateContext(ctx, map[string]interface{}{
				"last_error": "UNPROTECTED: SL placement failed (" + slErr.Error() + ") and emergency close failed (" + closeErr.Error() + ")",
			})
			return fmt.Errorf("SL placement failed and emergency close failed — position UNPROTECTED, reconciler will retry: SL err: %v, close err: %w", slErr, closeErr)
		}
		x.markContext(ctx, StateClosed, map[string]interface{}{
			"last_error": "emergency close: SL placement failed: " + slErr.Error(),
		})
		return fmt.Errorf("SL placement failed, position emergency-closed: %w", slErr)
	}
	x.events.Success(traceID, signalID, "", EvSLSet,
		fmt.Sprintf("stop loss set @ %.8g (qty %.8g)", plan.StopLoss, filledQty), 0, nil)

	// --- Take profits (best effort; position already protected) ---
	tpPlan := x.placeTPLadder(traceID, signalID, plan, filledQty)
	tpJSON, _ := json.Marshal(tpPlan)
	x.updateContext(ctx, map[string]interface{}{"tp_plan_json": string(tpJSON)})
	return nil
}

// placeTPLadder places reduce-only limit TPs (maker fees) with quantities from
// the ACTUAL filled amount; falls back to trigger TPs without grid support.
func (x *Executor) placeTPLadder(traceID, signalID string, plan *OpenPlan, filledQty float64) []TPPlanEntry {
	if len(plan.TPPrices) == 0 {
		return nil
	}
	stepSize := x.detectStepSize(plan.Symbol, filledQty)
	quantities, err := SplitTPQuantities(filledQty, plan.TPRatios, stepSize, 0)
	if err != nil {
		x.events.Warn(traceID, signalID, "", EvExecutionError, fmt.Sprintf("TP quantity split failed: %v", err), nil)
		return nil
	}

	posSide := positionSideOf(string(plan.Direction))
	closeSide := "SELL"
	if plan.Direction == DirectionShort {
		closeSide = "BUY"
	}

	result := make([]TPPlanEntry, 0, len(plan.TPPrices))
	for i, price := range plan.TPPrices {
		if i >= len(quantities) || quantities[i] <= 0 {
			continue
		}
		entry := TPPlanEntry{Price: price, Quantity: quantities[i]}
		if x.gridEx != nil {
			res, err := x.gridEx.PlaceLimitOrder(&types.LimitOrderRequest{
				Symbol:       plan.Symbol,
				Side:         closeSide,
				PositionSide: posSide,
				Price:        price,
				Quantity:     quantities[i],
				ReduceOnly:   true,
				PostOnly:     false, // do not risk rejection when price is already through
			})
			if err != nil {
				x.events.Warn(traceID, signalID, "", EvExecutionError,
					fmt.Sprintf("TP%d limit order failed, falling back to trigger TP: %v", i+1, err), nil)
				if terr := x.ex.SetTakeProfit(plan.Symbol, posSide, quantities[i], price); terr != nil {
					x.events.Warn(traceID, signalID, "", EvExecutionError, fmt.Sprintf("TP%d trigger fallback failed: %v", i+1, terr), nil)
					continue
				}
			} else {
				entry.OrderID = res.OrderID
			}
		} else {
			if err := x.ex.SetTakeProfit(plan.Symbol, posSide, quantities[i], price); err != nil {
				x.events.Warn(traceID, signalID, "", EvExecutionError, fmt.Sprintf("TP%d trigger order failed: %v", i+1, err), nil)
				continue
			}
		}
		x.events.Success(traceID, signalID, "", EvTPSet,
			fmt.Sprintf("TP%d set @ %.8g qty=%.8g", i+1, price, quantities[i]), 0, nil)
		result = append(result, entry)
	}
	return result
}

// ExecuteClose closes a trade (full or partial). Idempotent: closing an
// already-flat position is a NOOP.
func (x *Executor) ExecuteClose(traceID, signalID string, ctx *store.CopyTradeContext, closeRatio float64) (SkipReason, error) {
	pos, err := x.findPosition(ctx.Symbol, ctx.Direction)
	if err != nil {
		return SkipNone, fmt.Errorf("position lookup failed: %w", err)
	}
	if pos == nil || pos.qty <= 0 {
		// Nothing on the exchange: reconcile the context.
		x.cancelAllQuiet(ctx.Symbol)
		x.markContext(ctx, StateClosed, map[string]interface{}{"last_action": "CLOSE(noop)"})
		x.events.Info(traceID, signalID, "", EvTradeClosed, "position already flat (NOOP_ALREADY_FLAT)", nil)
		return SkipAlreadyFlat, nil
	}

	closeQty := pos.qty
	full := closeRatio <= 0 || closeRatio >= 100
	if !full {
		closeQty = pos.qty * closeRatio / 100
		// Exchange precision: an unrounded quantity gets rejected by most venues.
		if qtyStr, ferr := x.ex.FormatQuantity(ctx.Symbol, closeQty); ferr == nil {
			if q, perr := strconv.ParseFloat(qtyStr, 64); perr == nil && q > 0 {
				closeQty = q
			}
		}
		if closeQty <= 0 {
			x.events.Info(traceID, signalID, "", EvSignalSkipped,
				fmt.Sprintf("partial close skipped: %.0f%% of %.8g rounds to zero", closeRatio, pos.qty), nil)
			return SkipAlreadyFlat, nil
		}
		if closeQty >= pos.qty {
			full = true
		}
	}

	if full {
		// Cancel protection orders first so reduce-only orders don't fight the close.
		x.cancelAllQuiet(ctx.Symbol)
	}

	start := time.Now()
	if ctx.Direction == string(DirectionLong) {
		_, err = x.ex.CloseLong(ctx.Symbol, closeQty)
	} else {
		_, err = x.ex.CloseShort(ctx.Symbol, closeQty)
	}
	if err != nil {
		x.events.Error(traceID, signalID, "", EvExecutionError, fmt.Sprintf("close failed: %v", err), nil)
		return SkipNone, fmt.Errorf("close failed: %w", err)
	}
	x.events.Success(traceID, signalID, "", EvCloseSubmitted,
		fmt.Sprintf("close %s %.8g/%.8g (%.0f%%)", ctx.Symbol, closeQty, pos.qty, math.Min(closeRatio, 100)),
		time.Since(start).Milliseconds(), nil)

	if full {
		x.markContext(ctx, StateClosed, map[string]interface{}{"last_action": "CLOSE"})
		x.events.Success(traceID, signalID, "", EvTradeClosed, fmt.Sprintf("trade %s closed", ctx.Symbol), 0, nil)
	} else {
		remaining := pos.qty - closeQty
		x.updateContext(ctx, map[string]interface{}{"quantity": remaining, "last_action": "REDUCE"})
		// Re-issue SL for the remaining quantity so protection matches the position.
		if ctx.StopLossPrice > 0 {
			if cerr := x.ex.CancelStopLossOrders(ctx.Symbol); cerr != nil {
				x.events.Warn(traceID, signalID, "", EvExecutionError,
					fmt.Sprintf("partial close: cancel old SL failed: %v", cerr), nil)
			}
			if serr := x.ex.SetStopLoss(ctx.Symbol, positionSideOf(ctx.Direction), remaining, ctx.StopLossPrice); serr != nil {
				// Reconciler's SL guard re-places it next cycle; log loudly.
				x.events.Error(traceID, signalID, "", EvExecutionError,
					fmt.Sprintf("partial close: re-issue SL failed (reconciler will retry): %v", serr), nil)
			}
		}
		x.events.Success(traceID, signalID, "", EvTradeUpdated,
			fmt.Sprintf("partial close done, remaining %.8g", remaining), 0, nil)
	}
	return SkipNone, nil
}

// ExecuteUpdateSL replaces the stop loss. newPrice must already be resolved
// (ENTRY/BREAKEVEN mapped to the fill price by the caller).
func (x *Executor) ExecuteUpdateSL(traceID, signalID string, ctx *store.CopyTradeContext, newPrice float64) (SkipReason, error) {
	pos, err := x.findPosition(ctx.Symbol, ctx.Direction)
	if err != nil {
		return SkipNone, fmt.Errorf("position lookup failed: %w", err)
	}
	if pos == nil || pos.qty <= 0 {
		x.markContext(ctx, StateClosed, map[string]interface{}{"last_action": "UPDATE_SL(skipped)"})
		x.events.Info(traceID, signalID, "", EvSignalSkipped, "UPDATE_SL skipped: no live position (SKIPPED_NO_POSITION)", nil)
		return SkipNoPosition, nil
	}

	if err := x.ex.CancelStopLossOrders(ctx.Symbol); err != nil {
		x.events.Warn(traceID, signalID, "", EvExecutionError,
			fmt.Sprintf("cancel old SL failed (may not exist): %v", err), nil)
	}
	var slErr error
	for attempt := 1; attempt <= 3; attempt++ {
		slErr = x.ex.SetStopLoss(ctx.Symbol, positionSideOf(ctx.Direction), pos.qty, newPrice)
		if slErr == nil {
			break
		}
		retrySleep(time.Duration(attempt) * time.Second)
	}
	if slErr != nil {
		x.events.Error(traceID, signalID, "", EvExecutionError, fmt.Sprintf("set new SL failed: %v", slErr), nil)
		// The old SL was already cancelled: restore protection at the previous
		// price immediately instead of leaving the position naked.
		if ctx.StopLossPrice > 0 {
			if rerr := x.ex.SetStopLoss(ctx.Symbol, positionSideOf(ctx.Direction), pos.qty, ctx.StopLossPrice); rerr != nil {
				x.events.Error(traceID, signalID, "", EvExecutionError,
					fmt.Sprintf("restore previous SL @ %.8g also failed (reconciler will retry): %v", ctx.StopLossPrice, rerr), nil)
			} else {
				x.events.Warn(traceID, signalID, "", EvSLSet,
					fmt.Sprintf("new SL failed; previous SL restored @ %.8g", ctx.StopLossPrice), nil)
			}
		}
		return SkipNone, fmt.Errorf("set new SL failed: %w", slErr)
	}

	updates := map[string]interface{}{"stop_loss_price": newPrice, "last_action": "UPDATE_SL"}
	// Moving the stop to (or past) entry makes the trade risk-free.
	entry := ctx.AvgFillPrice
	if entry > 0 {
		if (ctx.Direction == string(DirectionLong) && newPrice >= entry) ||
			(ctx.Direction == string(DirectionShort) && newPrice <= entry) {
			if CanTransition(TradeState(ctx.State), StateBreakeven) {
				updates["state"] = string(StateBreakeven)
				updates["breakeven_applied"] = true
			}
		}
	}
	x.updateContext(ctx, updates)
	x.events.Success(traceID, signalID, "", EvSLSet,
		fmt.Sprintf("stop loss moved to %.8g (qty %.8g)", newPrice, pos.qty), 0, nil)
	return SkipNone, nil
}

// ExecuteUpdateTP replaces the take-profit ladder for the remaining position.
func (x *Executor) ExecuteUpdateTP(traceID, signalID string, ctx *store.CopyTradeContext, prices, ratios []float64) (SkipReason, error) {
	pos, err := x.findPosition(ctx.Symbol, ctx.Direction)
	if err != nil {
		return SkipNone, fmt.Errorf("position lookup failed: %w", err)
	}
	if pos == nil || pos.qty <= 0 {
		x.events.Info(traceID, signalID, "", EvSignalSkipped, "UPDATE_TP skipped: no live position", nil)
		return SkipNoPosition, nil
	}
	if err := x.ex.CancelTakeProfitOrders(ctx.Symbol); err != nil {
		x.events.Warn(traceID, signalID, "", EvExecutionError, fmt.Sprintf("cancel old TPs failed: %v", err), nil)
	}
	plan := &OpenPlan{
		Symbol: ctx.Symbol, Direction: Direction(ctx.Direction),
		TPPrices: prices, TPRatios: ratios,
	}
	tpPlan := x.placeTPLadder(traceID, signalID, plan, pos.qty)
	tpJSON, _ := json.Marshal(tpPlan)
	x.updateContext(ctx, map[string]interface{}{"tp_plan_json": string(tpJSON), "last_action": "UPDATE_TP"})
	return SkipNone, nil
}

// ExecuteCancel cancels an unfilled entry order. It only marks the context
// CANCELLED once the exchange confirms the order is gone; a filled-in-race
// order stays ENTRY_PENDING so the reconciler converts it to OPEN and places
// protections instead of orphaning the position.
func (x *Executor) ExecuteCancel(traceID, signalID string, ctx *store.CopyTradeContext) (SkipReason, error) {
	if ctx.State != string(StateEntryPending) && ctx.State != string(StateNew) {
		return SkipAlreadyFlat, nil
	}
	if ctx.EntryOrderID != "" && x.gridEx != nil {
		// Race check: the entry may have filled before the cancel arrives.
		if status, serr := x.ex.GetOrderStatus(ctx.Symbol, ctx.EntryOrderID); serr == nil {
			if st, _ := status["status"].(string); st == "FILLED" {
				x.events.Warn(traceID, signalID, "", EvExecutionError,
					fmt.Sprintf("cancel requested but entry %s already FILLED; keeping trade active for reconciliation", ctx.EntryOrderID), nil)
				return SkipNone, fmt.Errorf("entry order filled before cancel; position will be protected by the reconciler")
			}
		}
		if err := x.gridEx.CancelOrder(ctx.Symbol, ctx.EntryOrderID); err != nil {
			// Cancel failed: verify against exchange truth before deciding.
			if status, serr := x.ex.GetOrderStatus(ctx.Symbol, ctx.EntryOrderID); serr == nil {
				st, _ := status["status"].(string)
				switch st {
				case "FILLED":
					x.events.Warn(traceID, signalID, "", EvExecutionError,
						"cancel failed because entry already filled; keeping trade active for reconciliation", nil)
					return SkipNone, fmt.Errorf("entry order filled before cancel; position will be protected by the reconciler")
				case "CANCELED", "CANCELLED", "EXPIRED", "REJECTED":
					// Already gone on the exchange: safe to finalize below.
				default:
					x.events.Warn(traceID, signalID, "", EvExecutionError,
						fmt.Sprintf("cancel entry order failed (status %s), will retry: %v", st, err), nil)
					return SkipNone, fmt.Errorf("cancel entry order failed: %w", err)
				}
			} else {
				x.events.Warn(traceID, signalID, "", EvExecutionError,
					fmt.Sprintf("cancel entry order failed and status unknown, will retry: %v", err), nil)
				return SkipNone, fmt.Errorf("cancel entry order failed: %w", err)
			}
		}
	} else {
		x.cancelAllQuiet(ctx.Symbol)
	}
	x.markContext(ctx, StateCancelled, map[string]interface{}{"last_action": "CANCEL"})
	x.events.Success(traceID, signalID, "", EvTradeCancelled, fmt.Sprintf("pending entry for %s cancelled", ctx.Symbol), 0, nil)
	return SkipNone, nil
}

// emergencyClose force-closes a position after protection placement failed.
// Returns the close error so callers can avoid marking the trade terminal
// while an unprotected position may still be live on the exchange.
func (x *Executor) emergencyClose(traceID, signalID, symbol, direction string) error {
	var err error
	if direction == string(DirectionLong) {
		_, err = x.ex.CloseLong(symbol, 0) // 0 = close all
	} else {
		_, err = x.ex.CloseShort(symbol, 0)
	}
	if err != nil {
		// Worst case: position open without SL and close failed. Loudest alarm we have.
		x.events.Error(traceID, signalID, "", EvEmergencyClose,
			fmt.Sprintf("EMERGENCY CLOSE FAILED for %s %s — POSITION IS UNPROTECTED, manual action required: %v", symbol, direction, err), nil)
	}
	return err
}

// --- helpers ---

type positionInfo struct {
	qty        float64
	entryPrice float64
}

// findPosition locates the live position for symbol+direction.
func (x *Executor) findPosition(symbol, direction string) (*positionInfo, error) {
	positions, err := x.ex.GetPositions()
	if err != nil {
		return nil, err
	}
	wantSide := strings.ToLower(direction) // "long"/"short"
	for _, pos := range positions {
		s, _ := pos["symbol"].(string)
		if s != symbol {
			continue
		}
		side, _ := pos["side"].(string)
		if !strings.EqualFold(side, wantSide) {
			continue
		}
		qty, _ := pos["positionAmt"].(float64)
		if qty < 0 {
			qty = -qty
		}
		entry, _ := pos["entryPrice"].(float64)
		if qty > 0 {
			return &positionInfo{qty: qty, entryPrice: entry}, nil
		}
	}
	return nil, nil
}

// hasStopLossOrder reports whether a live stop-loss style order exists for the
// symbol. Returns (exists, ok); ok=false means the exchange query failed and
// the answer is unknown (callers must NOT treat that as "missing").
func (x *Executor) hasStopLossOrder(symbol string) (bool, bool) {
	orders, err := x.ex.GetOpenOrders(symbol)
	if err != nil {
		return false, false
	}
	for _, o := range orders {
		t := strings.ToUpper(o.Type)
		if strings.Contains(t, "TAKE_PROFIT") {
			continue
		}
		// STOP / STOP_MARKET / STOP_LIMIT, or any conditional order with a
		// trigger price that is not a take-profit.
		if strings.Contains(t, "STOP") || o.StopPrice > 0 {
			return true, true
		}
	}
	return false, true
}

// confirmFill polls the order status briefly for actual avg price / quantity,
// degrading to the reference values when the exchange is slow.
func (x *Executor) confirmFill(symbol, orderID string, fallbackQty, fallbackPrice float64) (avgPrice, filledQty float64) {
	avgPrice, filledQty = fallbackPrice, fallbackQty
	if orderID == "" {
		return
	}
	for attempt := 0; attempt < 5; attempt++ {
		status, err := x.ex.GetOrderStatus(symbol, orderID)
		if err == nil {
			st, _ := status["status"].(string)
			if st == "FILLED" {
				if p, ok := status["avgPrice"].(float64); ok && p > 0 {
					avgPrice = p
				}
				if q, ok := status["executedQty"].(float64); ok && q > 0 {
					filledQty = q
				}
				return
			}
		}
		time.Sleep(time.Duration(attempt+1) * 500 * time.Millisecond)
	}
	return
}

// detectStepSize infers the quantity step from FormatQuantity's precision.
func (x *Executor) detectStepSize(symbol string, sampleQty float64) float64 {
	formatted, err := x.ex.FormatQuantity(symbol, sampleQty)
	if err != nil {
		return 0
	}
	if idx := strings.Index(formatted, "."); idx >= 0 {
		decimals := len(strings.TrimRight(formatted[idx+1:], "0"))
		if decimals == 0 {
			decimals = len(formatted[idx+1:])
		}
		return math.Pow(10, -float64(decimals))
	}
	return 1
}

func (x *Executor) cancelAllQuiet(symbol string) {
	if err := x.ex.CancelAllOrders(symbol); err != nil {
		x.events.Warn("", "", "", EvOrderCancelled, fmt.Sprintf("cancel orders for %s failed: %v", symbol, err), nil)
	}
}

// updateContext persists updates ignoring version conflicts (engine is the
// only writer per trader; versioning guards the reconciler races).
func (x *Executor) updateContext(ctx *store.CopyTradeContext, updates map[string]interface{}) {
	if err := x.st.CopyTrade().UpdateContextVersioned(ctx.ID, ctx.Version, updates); err != nil {
		x.events.Warn("", "", "", EvExecutionError, fmt.Sprintf("context update failed: %v", err), nil)
		return
	}
	ctx.Version++
}

func (x *Executor) markContext(ctx *store.CopyTradeContext, state TradeState, extra map[string]interface{}) {
	updates := map[string]interface{}{"state": string(state)}
	if state == StateClosed {
		now := time.Now().UTC()
		updates["closed_at"] = &now
	}
	for k, v := range extra {
		updates[k] = v
	}
	x.updateContext(ctx, updates)
	ctx.State = string(state)
}

func orderIDString(order map[string]interface{}) string {
	if order == nil {
		return ""
	}
	switch v := order["orderId"].(type) {
	case string:
		return v
	case int64:
		return strconv.FormatInt(v, 10)
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	}
	return ""
}
