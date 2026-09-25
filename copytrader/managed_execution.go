package copytrader

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"nofx/store"
	"nofx/trader/types"
)

// Serialize copy-trade admission across engines sharing the same account.
// Ownership is additionally verified in durable contexts and fresh positions.
var managedAdmission sync.Mutex

type managedEntryPlan struct {
	Version         int                      `json:"version"`
	RiskBudget      float64                  `json:"risk_budget"`
	StopLoss        float64                  `json:"stop_loss"`
	MaxNotional     float64                  `json:"max_notional"`
	MarginBudget    float64                  `json:"margin_budget"`
	SignalExpiresAt time.Time                `json:"signal_expires_at"`
	Rules           types.ManagedMarketRules `json:"rules"`
}

func orderTerminal(status string) bool {
	switch status {
	case "FILLED", "CANCELED", "CANCELLED", "EXPIRED", "EXPIRED_IN_MATCH", "REJECTED", "ABANDONED":
		return true
	}
	return false
}
func openingSide(direction string) string {
	if direction == "SHORT" {
		return "SELL"
	}
	return "BUY"
}
func closingSide(direction string) string {
	if direction == "SHORT" {
		return "BUY"
	}
	return "SELL"
}

func (x *Executor) newManagedOrder(ctx *store.CopyTradeContext, signalID, role, kind string, price, qty float64) *store.CopyTradeOrder {
	id := stableID(ctx.ID, role)
	return &store.CopyTradeOrder{ID: id, TraderID: x.traderID, ContextID: ctx.ID, SignalID: signalID, Role: role, Symbol: ctx.Symbol, Direction: ctx.Direction, ClientID: "ct-" + id[:28], OrderType: kind, Price: price, Quantity: qty, Status: "PLANNED"}
}

func (x *Executor) recordManagedResult(row *store.CopyTradeOrder, result *types.ManagedOrderResult) error {
	if result == nil || result.OrderID == "" {
		return fmt.Errorf("empty managed order response")
	}
	// Reject mismatched identities instead of binding a query to another order.
	if result.Symbol != row.Symbol || (result.ClientID != "" && result.ClientID != row.ClientID) || (result.PositionSide != "" && result.PositionSide != row.Direction) {
		return fmt.Errorf("managed order identity mismatch")
	}
	if result.ExecutedQty+1e-10 < row.ExecutedQty {
		return fmt.Errorf("cumulative order fills moved backwards")
	}
	updates := map[string]interface{}{"order_id": result.OrderID, "status": result.Status, "executed_qty": result.ExecutedQty, "avg_price": result.AvgPrice, "last_error": ""}
	if err := x.st.CopyTrade().UpdateOrder(row.ID, updates); err != nil {
		return err
	}
	changed := row.OrderID != result.OrderID || row.Status != result.Status || row.ExecutedQty != result.ExecutedQty
	row.OrderID, row.Status, row.ExecutedQty, row.AvgPrice = result.OrderID, result.Status, result.ExecutedQty, result.AvgPrice
	if changed {
		trace := row.SignalID
		if trace == "" {
			trace = "reconcile-" + row.ContextID
		}
		x.events.Info(trace, row.SignalID, "", EvEntrySubmitted, fmt.Sprintf("%s %s %s: %s, filled %.8g/%.8g @ %.8g", row.Symbol, row.Direction, row.Role, row.Status, row.ExecutedQty, row.Quantity, row.AvgPrice), map[string]interface{}{"context_id": row.ContextID, "order_leg": row})
	}
	return nil
}

func (x *Executor) queryManaged(row *store.CopyTradeOrder) error {
	if row.Status == "PLANNED" || row.Status == "ABANDONED" {
		return nil
	}
	m := x.ex.(types.ManagedOrderTrader)
	result, err := m.GetManagedOrder(row.Symbol, row.OrderID, row.ClientID)
	if err != nil {
		return err
	}
	return x.recordManagedResult(row, result)
}

// A submit is permitted exactly once after its intent was committed. Any
// ambiguous response is recovered by client ID; never repeat a market order.
func (x *Executor) submitManaged(row *store.CopyTradeOrder, rules *types.ManagedMarketRules, reduce bool) error {
	if row.Status != "PLANNED" {
		return x.queryManaged(row)
	}
	if err := x.st.CopyTrade().UpdateOrder(row.ID, map[string]interface{}{"status": "SUBMITTING"}); err != nil {
		return err
	}
	row.Status = "SUBMITTING"
	side := openingSide(row.Direction)
	if reduce {
		side = closingSide(row.Direction)
	}
	result, err := x.ex.(types.ManagedOrderTrader).SubmitManagedOrder(&types.ManagedOrderRequest{Symbol: row.Symbol, Type: row.OrderType, Side: side, PositionSide: row.Direction, ClientID: row.ClientID, Quantity: row.Quantity, Price: row.Price, ReferencePrice: row.Price, ReduceOnly: reduce, Rules: rules})
	if err != nil {
		if errors.Is(err, types.ErrManagedOrderRejected) {
			row.Status = "REJECTED"
			_ = x.st.CopyTrade().UpdateOrder(row.ID, map[string]interface{}{"status": "REJECTED", "last_error": err.Error()})
			return err
		}
		_ = x.st.CopyTrade().UpdateOrder(row.ID, map[string]interface{}{"status": "UNKNOWN", "last_error": err.Error()})
		row.Status = "UNKNOWN"
		if queryErr := x.queryManaged(row); queryErr != nil {
			return fmt.Errorf("submission uncertain; client ID %s must be reconciled: %w (query: %v)", row.ClientID, err, queryErr)
		}
		return nil
	}
	if err = x.recordManagedResult(row, result); err != nil {
		return err
	}
	if !orderTerminal(row.Status) && row.OrderType == "MARKET" {
		return x.queryManaged(row)
	}
	return nil
}

func (x *Executor) cancelManaged(row *store.CopyTradeOrder) error {
	if row.Status == "PLANNED" {
		if err := x.st.CopyTrade().UpdateOrder(row.ID, map[string]interface{}{"status": "ABANDONED"}); err != nil {
			return err
		}
		row.Status = "ABANDONED"
		return nil
	}
	if orderTerminal(row.Status) {
		return nil
	}
	result, err := x.ex.(types.ManagedOrderTrader).CancelManagedOrder(row.Symbol, row.OrderID, row.ClientID)
	if err != nil {
		if queryErr := x.queryManaged(row); queryErr != nil {
			return fmt.Errorf("cancel outcome unknown: %v; query: %w", err, queryErr)
		}
	} else if err = x.recordManagedResult(row, result); err != nil {
		return err
	}
	if !orderTerminal(row.Status) {
		return fmt.Errorf("cancel is not terminal: %s", row.Status)
	}
	return nil
}

// splitQuantities allocates RISK, not equal coin amounts. Aggregate margin and
// notional caps scale both legs together before flooring to venue lot sizes.
func splitQuantities(plan *OpenPlan, rules *types.ManagedMarketRules) (float64, float64, error) {
	if plan.RiskBudget <= 0 || plan.SplitReference <= 0 || plan.AvailableMargin <= 0 {
		return 0, 0, fmt.Errorf("split requires risk budget, reference and available margin")
	}
	d1, d2 := math.Abs(plan.EntryPrice-plan.StopLoss), math.Abs(plan.SplitReference-plan.StopLoss)
	if d1 <= 0 || d2 <= 0 || (plan.Direction == DirectionLong && plan.SplitReference <= plan.StopLoss) || (plan.Direction == DirectionShort && plan.SplitReference >= plan.StopLoss) {
		return 0, 0, fmt.Errorf("invalid split stop distance")
	}
	q1, q2 := plan.RiskBudget/2/d1, plan.RiskBudget/2/d2
	notional := q1*plan.EntryPrice + q2*plan.SplitReference
	cap := plan.MaxNotional
	if cap <= 0 {
		cap = 30000
	}
	cap = math.Min(cap, plan.AvailableMargin*.9*float64(plan.Leverage))
	scale := math.Min(1, cap/notional)
	q1 *= scale
	q2 *= scale
	var err error
	q1, err = rules.FloorQuantity(q1, true)
	if err != nil {
		return 0, 0, err
	}
	q2, err = rules.FloorQuantity(q2, false)
	if err != nil {
		return 0, 0, err
	}
	if q1*plan.EntryPrice < rules.MinNotional || q2*plan.SplitReference < rules.MinNotional {
		return 0, 0, fmt.Errorf("one split leg is below the exchange minimum notional")
	}
	return q1, q2, nil
}

func (x *Executor) executeManagedOpen(traceID, signalID string, plan *OpenPlan) (*store.CopyTradeContext, error) {
	managedAdmission.Lock()
	defer managedAdmission.Unlock()
	m := x.ex.(types.ManagedOrderTrader)
	rules, err := m.MarketRules(plan.Symbol)
	if err != nil {
		return nil, err
	}
	split := plan.SplitReference > 0
	if split {
		owners, err := x.st.CopyTrade().AccountContexts(x.traderID, plan.Symbol, string(plan.Direction))
		if err != nil {
			return nil, err
		}
		if len(owners) > 0 {
			return nil, fmt.Errorf("split requires exclusive ownership of this account/symbol/direction")
		}
		pos, err := x.freshPosition(plan.Symbol, string(plan.Direction))
		if err != nil {
			return nil, err
		}
		if pos != nil && pos.qty > 0 {
			return nil, fmt.Errorf("split refused: existing exchange position on this symbol/direction")
		}
	}
	if err = x.ex.SetLeverage(plan.Symbol, plan.Leverage); err != nil {
		return nil, fmt.Errorf("configured leverage rejected: %w", err)
	}
	if plan.EntryType == EntryPlanLimit {
		plan.EntryPrice, err = rules.NormalizePrice(plan.EntryPrice)
		if err != nil {
			return nil, err
		}
	}
	q1, q2 := plan.Quantity, 0.0
	if split {
		plan.SplitReference, err = rules.NormalizePrice(plan.SplitReference)
		if err != nil {
			return nil, err
		}
		q1, q2, err = splitQuantities(plan, rules)
	} else {
		q1, err = rules.FloorQuantity(q1, plan.EntryType == EntryPlanMarket)
	}
	if err != nil {
		return nil, err
	}
	if q1*plan.EntryPrice < rules.MinNotional {
		return nil, fmt.Errorf("entry below exchange minimum notional")
	}
	policy := EntryPolicyLegacy
	if split {
		policy = EntryPolicySplit
	}
	state := managedEntryPlan{Version: 1, RiskBudget: plan.RiskBudget, StopLoss: plan.StopLoss, MaxNotional: plan.MaxNotional, MarginBudget: plan.AvailableMargin * .9, SignalExpiresAt: plan.SignalExpiresAt, Rules: *rules}
	b, _ := json.Marshal(state)
	ctx := &store.CopyTradeContext{ID: uuid.NewString(), TraderID: x.traderID, ChannelID: plan.ChannelID, RootMessageID: plan.RootMsgID, Symbol: plan.Symbol, RawSymbol: plan.RawSymbol, Direction: string(plan.Direction), State: string(StateEntryPending), ExecutionVersion: 1, EntryPolicy: policy, EntryPlanJSON: string(b), PlannedEntryPrice: plan.EntryPrice, StopLossPrice: plan.StopLoss, Leverage: plan.Leverage, TPRecipeJSON: planRecipeJSON(plan), EntryWorking: true}
	ctx.BreakevenAfterTP, ctx.BreakevenTPLevel = plan.BreakevenTPLevel > 0, plan.BreakevenTPLevel
	if plan.EntryTimeout > 0 {
		deadline := time.Now().UTC().Add(plan.EntryTimeout)
		ctx.EntryDeadline = &deadline
	}
	kind := "MARKET"
	if plan.EntryType == EntryPlanLimit {
		kind = "LIMIT"
	}
	first := x.newManagedOrder(ctx, signalID, "ENTRY_1", kind, plan.EntryPrice, q1)
	orders := []*store.CopyTradeOrder{first}
	if split {
		orders = append(orders, x.newManagedOrder(ctx, signalID, "ENTRY_2", "LIMIT", plan.SplitReference, q2))
	}
	if err = x.st.CopyTrade().CreateManagedPlan(ctx, orders); err != nil {
		return nil, err
	}
	if err = x.submitManaged(first, rules, false); err != nil {
		x.updateContext(ctx, map[string]interface{}{"last_error": err.Error()})
		_ = x.reconcileManagedEntry(traceID, ctx)
		return ctx, err
	}
	if err = x.reconcileManagedEntry(traceID, ctx); err != nil {
		return ctx, err
	}
	return ctx, nil
}

func (x *Executor) entryOrders(ctx *store.CopyTradeContext) ([]*store.CopyTradeOrder, error) {
	all, err := x.st.CopyTrade().GetOrdersForContext(x.traderID, ctx.ID)
	if err != nil {
		return nil, err
	}
	var entries []*store.CopyTradeOrder
	for _, o := range all {
		if strings.HasPrefix(o.Role, "ENTRY_") {
			entries = append(entries, o)
		}
	}
	// IDs are hashes; creation timestamps need not distinguish transaction rows.
	if len(entries) == 2 && entries[0].Role == "ENTRY_2" {
		entries[0], entries[1] = entries[1], entries[0]
	}
	return entries, nil
}

// closeEntryEligibility persists the one-way latch BEFORE contacting the venue.
// Even a crash or a cancel/fill race cannot permit later replenishment.
func (x *Executor) closeEntryEligibility(traceID string, ctx *store.CopyTradeContext, reason string) error {
	if err := x.persistContext(ctx, map[string]interface{}{"entry_disabled": true, "last_action": reason}); err != nil {
		return err
	}
	if ctx.ExecutionVersion == 0 || ctx.EntryPlanJSON == "" {
		return x.cancelLegacyRemainder(traceID, ctx)
	}
	orders, err := x.entryOrders(ctx)
	if err != nil {
		return err
	}
	var cancelErr error
	for _, o := range orders {
		if err = x.cancelManaged(o); err != nil {
			cancelErr = errors.Join(cancelErr, err)
		}
	}
	if err = x.settleManagedFills(ctx, orders); err != nil {
		return errors.Join(cancelErr, err)
	}
	if cancelErr != nil {
		// A failed cancellation must not prevent protection of known fills.
		if pos, perr := x.freshPosition(ctx.Symbol, ctx.Direction); perr == nil && pos != nil && pos.qty > 0 {
			cancelErr = errors.Join(cancelErr, x.ensureStopProtection(traceID, "", ctx, pos.qty))
		}
		return cancelErr
	}
	return x.persistContext(ctx, map[string]interface{}{"entry_working": false})
}

func (x *Executor) settleManagedFills(ctx *store.CopyTradeContext, orders []*store.CopyTradeOrder) error {
	total, cost := 0.0, 0.0
	added := ctx.HasAddFill
	for _, o := range orders {
		if o.ExecutedQty > 0 {
			if o.AvgPrice <= 0 {
				return fmt.Errorf("fill price is not confirmed")
			}
			total += o.ExecutedQty
			cost += o.ExecutedQty * o.AvgPrice
			if o.Role == "ENTRY_2" {
				added = true
			}
		}
	}
	if total <= 0 {
		return nil
	}
	working := false
	for _, o := range orders {
		working = working || !orderTerminal(o.Status)
	}
	updates := map[string]interface{}{"avg_fill_price": cost / total, "entry_filled_quantity": total, "has_add_fill": added, "entry_working": working}
	// Never reconstruct remaining position from gross entries after an exit.
	pos, err := x.freshPosition(ctx.Symbol, ctx.Direction)
	if err != nil {
		return err
	}
	if pos != nil {
		updates["quantity"] = pos.qty
	} else if ctx.OpenedAt == nil {
		updates["quantity"] = total
	} else {
		updates["quantity"] = 0
	}
	if ctx.OpenedAt == nil {
		now := time.Now().UTC()
		updates["opened_at"] = &now
		updates["state"] = string(StateOpen)
	}
	if len(orders) > 0 {
		updates["entry_order_id"] = orders[0].OrderID
	}
	return x.persistContext(ctx, updates)
}

func (x *Executor) reconcileManagedEntry(traceID string, ctx *store.CopyTradeContext) (outErr error) {
	defer func() {
		if outErr != nil {
			x.updateContext(ctx, map[string]interface{}{"last_error": outErr.Error()})
			if pos, err := x.freshPosition(ctx.Symbol, ctx.Direction); err == nil && pos != nil && pos.qty > 0 {
				x.updateContext(ctx, map[string]interface{}{"quantity": pos.qty})
				if err = x.ensureStopProtection(traceID, "", ctx, pos.qty); err != nil {
					x.events.Error(traceID, "", "", EvExecutionError, "UNPROTECTED: "+err.Error(), nil)
				}
			}
		}
	}()
	var plan managedEntryPlan
	if err := json.Unmarshal([]byte(ctx.EntryPlanJSON), &plan); err != nil {
		return err
	}
	orders, err := x.entryOrders(ctx)
	if err != nil {
		return err
	}
	if len(orders) == 0 {
		return fmt.Errorf("entry ledger missing")
	}
	for _, o := range orders {
		if o.Status == "REJECTED" || o.Status == "ABANDONED" {
			ctx.EntryDisabled = true
		}
	}
	oldQty := ctx.Quantity
	if ctx.BreakevenApplied || ctx.State == string(StateBreakeven) || (ctx.EntryDeadline != nil && !time.Now().Before(*ctx.EntryDeadline)) {
		if err = x.persistContext(ctx, map[string]interface{}{"entry_disabled": true}); err != nil {
			return err
		}
	}
	var queryErr error
	for _, o := range orders {
		if o.Status != "PLANNED" && !orderTerminal(o.Status) {
			if err = x.queryManaged(o); err != nil {
				queryErr = err
			}
		}
	}
	if err = x.settleManagedFills(ctx, orders); err != nil {
		return err
	}
	if x.refreshTPProgress(traceID, ctx) || ctx.Quantity+1e-10 < oldQty {
		ctx.EntryDisabled = true
	}
	if ctx.BreakevenApplied || ctx.State == string(StateBreakeven) || (ctx.EntryDeadline != nil && !time.Now().Before(*ctx.EntryDeadline)) {
		ctx.EntryDisabled = true
	}
	if ctx.EntryDisabled {
		if err = x.closeEntryEligibility(traceID, ctx, "ENTRY_DISABLED"); err != nil {
			return err
		}
		orders, err = x.entryOrders(ctx)
		if err != nil {
			return err
		}
	}
	if ctx.Quantity > 0 {
		if err = x.ensureStopProtection(traceID, "", ctx, ctx.Quantity); err != nil {
			_ = x.closeEntryEligibility(traceID, ctx, "PROTECTION_FAILED")
			// A protected first leg is mandatory before any additional exposure.
			_, closeErr := x.executeManagedClose(traceID, "emergency-"+ctx.ID, ctx, 100)
			return fmt.Errorf("entry protection failed: %v; emergency close: %v", err, closeErr)
		}
		if ctx.EntryPolicy == EntryPolicySplit {
			if err = x.enforceManagedRisk(traceID, ctx, &plan, orders); err != nil {
				return err
			}
		}
		if err = x.restoreRemainingProtections(traceID, "", ctx, ctx.Quantity); err != nil {
			return err
		}
	}
	for _, tp := range readTPPlan(ctx) {
		if tp.Filled || tp.FilledQuantity > 0 || tp.PriorFilledQuantity > 0 {
			if !ctx.EntryDisabled {
				if err = x.closeEntryEligibility(traceID, ctx, "TP_FILL"); err != nil {
					return err
				}
			}
			break
		}
	}
	if queryErr != nil {
		return queryErr
	}
	if !ctx.EntryDisabled && ctx.ClosePendingJSON == "" {
		for i, o := range orders {
			if o.Status != "PLANNED" {
				continue
			}
			if i == 0 {
				if !plan.SignalExpiresAt.IsZero() && time.Now().After(plan.SignalExpiresAt) {
					_ = x.cancelManaged(o)
					continue
				}
			} else {
				if orders[0].Status != "FILLED" || ctx.Quantity <= 0 {
					continue
				}
				// Recovery may have submitted the first leg and its protections
				// earlier in this same pass. Recheck exits before adding exposure.
				x.refreshTPProgress(traceID, ctx)
				for _, tp := range readTPPlan(ctx) {
					if tp.Filled || tp.FilledQuantity > 0 || tp.PriorFilledQuantity > 0 {
						if err = x.closeEntryEligibility(traceID, ctx, "TP_FILL"); err != nil {
							return err
						}
						return x.restoreRemainingProtections(traceID, "", ctx, ctx.Quantity)
					}
				}
				live, err := x.freshPosition(ctx.Symbol, ctx.Direction)
				if err != nil {
					return err
				}
				if live == nil || live.qty <= 0 {
					return x.closeEntryEligibility(traceID, ctx, "FIRST_LEG_NO_LONGER_OPEN")
				}
				if live.qty+1e-10 < ctx.Quantity {
					if err = x.closeEntryEligibility(traceID, ctx, "POSITION_REDUCED"); err != nil {
						return err
					}
					return x.restoreRemainingProtections(traceID, "", ctx, ctx.Quantity)
				}
				used := orders[0].ExecutedQty * math.Abs(orders[0].AvgPrice-plan.StopLoss)
				budget := math.Max(0, plan.RiskBudget-used)
				want := math.Min(o.Quantity, budget/math.Abs(o.Price-plan.StopLoss))
				usedNotional := orders[0].ExecutedQty * orders[0].AvgPrice
				if plan.MaxNotional > 0 {
					want = math.Min(want, math.Max(0, plan.MaxNotional-usedNotional)/o.Price)
				}
				if plan.MarginBudget > 0 {
					want = math.Min(want, math.Max(0, plan.MarginBudget*float64(ctx.Leverage)-usedNotional)/o.Price)
				}
				want, err = plan.Rules.FloorQuantity(want, false)
				if err != nil || want*o.Price < plan.Rules.MinNotional {
					_ = x.closeEntryEligibility(traceID, ctx, "SECOND_LEG_BELOW_MINIMUM")
					return nil
				}
				if err = x.st.CopyTrade().UpdateOrder(o.ID, map[string]interface{}{"quantity": want}); err != nil {
					return err
				}
				o.Quantity = want
			}
			if err = x.submitManaged(o, &plan.Rules, false); err != nil {
				x.updateContext(ctx, map[string]interface{}{"last_error": err.Error()})
				return err
			}
			if o.ExecutedQty > 0 {
				if err = x.settleManagedFills(ctx, orders); err != nil {
					return err
				}
				if err = x.ensureStopProtection(traceID, "", ctx, ctx.Quantity); err != nil {
					return err
				}
				if ctx.EntryPolicy == EntryPolicySplit {
					if err = x.enforceManagedRisk(traceID, ctx, &plan, orders); err != nil {
						return err
					}
				}
				if err = x.restoreRemainingProtections(traceID, "", ctx, ctx.Quantity); err != nil {
					return err
				}
			}
		}
	}
	allTerminal, filled := true, false
	for _, o := range orders {
		allTerminal = allTerminal && orderTerminal(o.Status)
		filled = filled || o.ExecutedQty > 0
	}
	if allTerminal && !filled {
		state := StateCancelled
		if ctx.EntryDeadline != nil && time.Now().After(*ctx.EntryDeadline) {
			state = StateExpired
		}
		x.markContext(ctx, state, nil)
	}
	if ctx.LastError != "" {
		hasRejected := false
		for _, o := range orders {
			hasRejected = hasRejected || o.Status == "REJECTED"
		}
		if !hasRejected {
			x.updateContext(ctx, map[string]interface{}{"last_error": ""})
		}
	}
	return nil
}

func (x *Executor) enforceManagedRisk(traceID string, ctx *store.CopyTradeContext, plan *managedEntryPlan, orders []*store.CopyTradeOrder) error {
	if plan.RiskBudget <= 0 || ctx.Quantity <= 0 {
		return nil
	}
	if ctx.AvgFillPrice <= 0 {
		return fmt.Errorf("risk settlement awaits a confirmed cumulative fill price")
	}
	distance := math.Abs(ctx.AvgFillPrice - plan.StopLoss)
	capQty := plan.RiskBudget / distance
	if plan.MaxNotional > 0 {
		capQty = math.Min(capQty, plan.MaxNotional/ctx.AvgFillPrice)
	}
	if plan.MarginBudget > 0 {
		capQty = math.Min(capQty, plan.MarginBudget*float64(ctx.Leverage)/ctx.AvgFillPrice)
	}
	if ctx.Quantity <= capQty+1e-10 {
		return nil
	}
	if err := x.closeEntryEligibility(traceID, ctx, "RISK_BUDGET_EXCEEDED"); err != nil {
		return err
	}
	// Recompute after cancelling: that request may itself have raced a fill.
	distance = math.Abs(ctx.AvgFillPrice - plan.StopLoss)
	capQty = plan.RiskBudget / distance
	if plan.MaxNotional > 0 {
		capQty = math.Min(capQty, plan.MaxNotional/ctx.AvgFillPrice)
	}
	if plan.MarginBudget > 0 {
		capQty = math.Min(capQty, plan.MarginBudget*float64(ctx.Leverage)/ctx.AvgFillPrice)
	}
	safe, _ := types.DecimalFloor(capQty, plan.Rules.QuantityStep)
	excess := ctx.Quantity - safe
	if excess <= 0 {
		return nil
	}
	_, err := x.executeManagedCloseAmount(traceID, "risk-"+stableID(ctx.ID, fmt.Sprint(ctx.Quantity), fmt.Sprint(ctx.AvgFillPrice)), ctx, excess/ctx.Quantity*100, excess)
	if err != nil {
		return err
	}
	return nil
}

// managedExitIntent freezes the original close amount, including across restart.
type managedExitIntent struct {
	OrderID string `json:"order_id"`
	Full    bool   `json:"full"`
}

func (x *Executor) executeManagedClose(traceID, signalID string, ctx *store.CopyTradeContext, ratio float64) (SkipReason, error) {
	return x.executeManagedCloseAmount(traceID, signalID, ctx, ratio, 0)
}

// minimumReduction is used only to remove a known risk overshoot. Round that
// reduction UP (never an entry), so a market lot does not leave excess risk.
func (x *Executor) executeManagedCloseAmount(traceID, signalID string, ctx *store.CopyTradeContext, ratio, minimumReduction float64) (SkipReason, error) {
	if ctx.ClosePendingJSON != "" {
		return SkipDuplicate, x.reconcileManagedClose(traceID, ctx)
	}
	pos, err := x.freshPosition(ctx.Symbol, ctx.Direction)
	if err != nil {
		return SkipNone, err
	}
	if pos == nil || pos.qty <= 0 {
		return SkipAlreadyFlat, nil
	}
	m := x.ex.(types.ManagedOrderTrader)
	rules, err := m.MarketRules(ctx.Symbol)
	if err != nil {
		return SkipNone, err
	}
	full := ratio <= 0 || ratio >= 100
	qty := pos.qty
	if !full {
		qty *= ratio / 100
	}
	if minimumReduction > 0 {
		step, minimum := rules.QuantityStep, rules.MinQuantity
		if rules.MarketQuantityStep > 0 {
			step, minimum = rules.MarketQuantityStep, rules.MarketMinQuantity
		}
		requested := types.SanitizeBaseQuantity(math.Max(minimumReduction, minimum))
		floor, ferr := types.DecimalFloor(requested, step)
		if ferr != nil {
			return SkipNone, ferr
		}
		qty = floor
		if floor < requested {
			qty = types.SanitizeBaseQuantity(floor + step)
		}
		if qty >= pos.qty {
			qty, full = pos.qty, true
		}
	}
	qty, err = rules.FloorQuantity(types.SanitizeBaseQuantity(qty), true)
	if err != nil {
		return SkipNone, err
	}
	if minimumReduction > 0 && qty+1e-10 < minimumReduction {
		return SkipNone, fmt.Errorf("market lot prevents required risk reduction; manual review required")
	}
	row := x.newManagedOrder(ctx, signalID, "CLOSE_"+signalID, "MARKET", pos.entryPrice, qty)
	intent, _ := json.Marshal(managedExitIntent{OrderID: row.ID, Full: full})
	if err = x.st.CopyTrade().CreateExitOrder(ctx, row, string(intent)); err != nil {
		return SkipNone, err
	}
	ctx.State = string(StateClosePending)
	ctx.ClosePendingJSON = string(intent)
	ctx.Version++
	if err = x.submitManaged(row, rules, true); err != nil {
		return SkipNone, err
	}
	return SkipNone, x.reconcileManagedClose(traceID, ctx)
}

func (x *Executor) reconcileManagedClose(traceID string, ctx *store.CopyTradeContext) error {
	var intent managedExitIntent
	if err := json.Unmarshal([]byte(ctx.ClosePendingJSON), &intent); err != nil {
		return err
	}
	orders, err := x.st.CopyTrade().GetOrdersForContext(x.traderID, ctx.ID)
	if err != nil {
		return err
	}
	var row *store.CopyTradeOrder
	for _, o := range orders {
		if o.ID == intent.OrderID {
			row = o
		}
	}
	if row == nil {
		return fmt.Errorf("close ledger missing")
	}
	if row.Status == "PLANNED" {
		rules, err := x.ex.(types.ManagedOrderTrader).MarketRules(ctx.Symbol)
		if err != nil {
			return err
		}
		if err = x.submitManaged(row, rules, true); err != nil {
			return err
		}
	} else if !orderTerminal(row.Status) {
		if err = x.queryManaged(row); err != nil {
			return err
		}
	}
	pos, err := x.freshPosition(ctx.Symbol, ctx.Direction)
	if err != nil {
		return err
	}
	if pos != nil && pos.qty > 0 {
		if err = x.restoreRemainingProtections(traceID, row.SignalID, ctx, pos.qty); err != nil {
			return err
		}
	}
	if !orderTerminal(row.Status) {
		return fmt.Errorf("close fill pending: %s", row.Status)
	}
	if pos == nil || pos.qty <= 0 {
		x.cancelTradeOrdersQuiet(ctx.Symbol, ctx.Direction)
		now := time.Now().UTC()
		return x.finalizeManagedExit(ctx, row.SignalID, map[string]interface{}{"state": string(StateClosed), "closed_at": &now, "quantity": 0, "close_pending_json": "", "last_action": "CLOSE"})
	}
	if row.Status != "FILLED" {
		return fmt.Errorf("close ended %s with remaining position %.8g; manual review required", row.Status, pos.qty)
	}
	if intent.Full {
		return fmt.Errorf("full close filled but remaining position %.8g is still visible; awaiting fresh confirmation", pos.qty)
	}
	state := StateOpen
	if ctx.BreakevenApplied {
		state = StateBreakeven
	}
	return x.finalizeManagedExit(ctx, row.SignalID, map[string]interface{}{"state": string(state), "quantity": pos.qty, "close_pending_json": "", "last_action": "REDUCE"})
}

func (x *Executor) finalizeManagedExit(ctx *store.CopyTradeContext, signalID string, updates map[string]interface{}) error {
	if err := x.st.CopyTrade().FinalizeExit(ctx, signalID, updates); err != nil {
		return err
	}
	b, _ := json.Marshal(updates)
	return json.Unmarshal(b, ctx)
}

// Recover only operations whose exact venue identity is recorded (Binance
// exits), or idempotent stop moves. Legacy ambiguous market exits are left for
// review instead of sending another reduction after a possible success.
func (e *Engine) recoverPendingActions(ctx *store.CopyTradeContext) {
	actions, err := e.st.CopyTrade().PendingActions(e.traderID, ctx.ID)
	if err != nil {
		return
	}
	for _, a := range actions {
		if TradeState(ctx.State).IsTerminal() {
			return
		}
		var ins SourceInterpretation
		if json.Unmarshal([]byte(a.PayloadJSON), &ins) != nil {
			continue
		}
		trace := "reconcile-" + ctx.ID
		var err error
		switch ins.Action {
		case ActionOpen, ActionAdd:
			orders, qerr := e.exec.entryOrders(ctx)
			if qerr != nil || len(orders) == 0 || orders[0].OrderID == "" || orders[0].Status == "SUBMITTING" || orders[0].Status == "UNKNOWN" {
				continue
			}
		case ActionClose, ActionReduce:
			if _, ok := e.exec.ex.(types.ManagedOrderTrader); !ok {
				continue
			}
			if ctx.ClosePendingJSON != "" {
				continue
			}
			// A completed close ledger is definitive even if the action update crashed.
			orders, qerr := e.st.CopyTrade().GetOrdersForContext(e.traderID, ctx.ID)
			if qerr != nil {
				continue
			}
			completed := false
			for _, o := range orders {
				if o.Role == "CLOSE_"+a.SignalID && o.Status == "FILLED" {
					completed = true
				}
			}
			if !completed {
				ratio := 100.0
				if ins.Action == ActionReduce || ins.CloseMode == CloseModePartial {
					ratio = 50
					if ins.CloseRatio != nil {
						ratio = *ins.CloseRatio
					}
				}
				_, err = e.exec.ExecuteClose(trace, a.SignalID, ctx, ratio)
			}
		case ActionUpdateSL:
			if len(ins.ConditionalRules) > 0 {
				err = e.applyConditionalRules(trace, a.SignalID, a.MessageID, &ins, ctx)
				break
			}
			if len(ins.StopLossLevels) == 0 {
				continue
			}
			spec := ins.StopLossLevels[0].Price
			price := resolveContextPrice(spec, ctx, ctx.AvgFillPrice)
			if price <= 0 {
				continue
			}
			if tighterStop(ctx.Direction, ctx.StopLossPrice, price) {
				spec = PriceSpec{Type: PriceFixed, Price: ctx.StopLossPrice}
			}
			_, err = e.exec.ExecuteUpdateSLSpec(trace, a.SignalID, ctx, spec)
		default:
			continue
		}
		if err == nil {
			_ = e.st.CopyTrade().UpdateAction(a.ID, map[string]interface{}{"status": "done", "error": ""})
		}
	}
}
