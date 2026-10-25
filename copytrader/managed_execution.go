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

// managedAdmission serializes copy-trade admission per exchange account
// (exchange_id -> *sync.Mutex): engines sharing one account still admit one
// OPEN at a time, while different accounts no longer wait on each other's
// exchange round-trips. The in-process lock is only the first line: durable
// ownership (CopyTradeOwnership primary key on exchange_id/symbol/direction,
// claimed in the context-creating transaction) plus the per-account fence row
// (CopyTradeAccountFence, locked in that transaction) keep admission exclusive across
// processes, and fresh positions/open orders are re-checked under the lock.
var managedAdmission sync.Map

// admissionLock returns the admission mutex of one exchange account.
func admissionLock(exchangeID string) *sync.Mutex {
	mu, _ := managedAdmission.LoadOrStore(exchangeID, &sync.Mutex{})
	return mu.(*sync.Mutex)
}

// managedNotFoundGrace is how long an order row without a venue order ID may
// stay SUBMITTING/UNKNOWN while the venue reports it as not found, before the
// submission is treated as never having landed. Measured from the moment the
// SUBMITTING/UNKNOWN status was recorded (the row's updated_at).
const managedNotFoundGrace = 60 * time.Second

// maxExitResubmits bounds how many times one exit intent is resubmitted with a
// fresh client ID after a definite rejection or a confirmed residual position.
const maxExitResubmits = 3

// exitResidualConfirmAfter: a full close that FILLED while a position is still
// visible is only resubmitted after the residual was seen on two passes at
// least this far apart (a single read can be stale).
var exitResidualConfirmAfter = 10 * time.Second

// errEntryCancelUnconfirmed marks a closeEntryEligibility failure that happened
// AFTER the durable entry_disabled latch was persisted: only the venue-side
// cancellation (or its fill settlement) is unconfirmed. Exits and stop moves on
// the known position may proceed; the latch already prevents replenishment.
var errEntryCancelUnconfirmed = errors.New("entry cancellation unconfirmed")

type managedEntryPlan struct {
	// Frozen entry decision, used to re-validate a deferred market first leg.
	// Absent in plans created before deferred re-validation existed.
	Decision        *entryDecision           `json:"decision,omitempty"`
	SplitReference  float64                  `json:"split_reference,omitempty"`
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
		x.events.Info(trace, row.SignalID, "", managedLegEvent(row.Role), fmt.Sprintf("%s %s %s: %s, filled %.8g/%.8g @ %.8g", row.Symbol, row.Direction, row.Role, row.Status, row.ExecutedQty, row.Quantity, row.AvgPrice), map[string]interface{}{"context_id": row.ContextID, "order_leg": row})
		if row.Status == "FILLED" && row.Role == "ENTRY_1" && row.OrderType == "MARKET" {
			x.logMarketFillRisk(trace, row)
		}
	}
	return nil
}

// managedLegEvent names a ledger leg's progress event by its role.
func managedLegEvent(role string) string {
	switch {
	case strings.HasPrefix(role, "ENTRY_"):
		return EvEntrySubmitted
	case strings.HasPrefix(role, "TP_"):
		return EvTPUpdated
	case strings.HasPrefix(role, "CLOSE_risk-"):
		return EvRiskUpdated
	case strings.HasPrefix(role, "CLOSE_"):
		return EvExitUpdated // author exits, TP replacements and emergency closes
	}
	return EvReconcile
}

// marketFillRiskWarnRatio: a single market entry whose filled risk exceeds the
// planned risk by more than this fraction is reported as a warning.
const marketFillRiskWarnRatio = 0.2

// logMarketFillRisk compares the realized risk of a filled single-leg market
// entry (fill price vs stop) with the risk planned at submission.
func (x *Executor) logMarketFillRisk(trace string, row *store.CopyTradeOrder) {
	ctx, err := x.st.CopyTrade().GetContext(row.ContextID)
	if err != nil || ctx == nil || ctx.EntryPolicy == EntryPolicySplit {
		return
	}
	stop := ctx.StopLossPrice
	var plan managedEntryPlan
	if json.Unmarshal([]byte(ctx.EntryPlanJSON), &plan) == nil && plan.StopLoss > 0 {
		stop = plan.StopLoss
	}
	if stop <= 0 || row.Price <= 0 || row.AvgPrice <= 0 {
		return
	}
	planned := row.Quantity * math.Abs(row.Price-stop)
	actual := EffectiveRisk(row.ExecutedQty, row.AvgPrice, stop)
	if planned <= 0 {
		return
	}
	data := map[string]interface{}{"context_id": row.ContextID, "planned_risk_usd": planned, "actual_risk_usd": actual, "risk_budget_usd": plan.RiskBudget, "planned_price": row.Price, "fill_price": row.AvgPrice, "stop_loss": stop, "planned_quantity": row.Quantity, "filled_quantity": row.ExecutedQty}
	msg := fmt.Sprintf("%s %s market entry filled: actual risk $%.2f vs planned $%.2f (fill %.8g vs plan %.8g, SL %.8g)", row.Symbol, row.Direction, actual, planned, row.AvgPrice, row.Price, stop)
	if actual > planned*(1+marketFillRiskWarnRatio) {
		x.events.Warn(trace, row.SignalID, "", EvQuantityPlan, msg+fmt.Sprintf(" — exceeds plan by %.1f%%", (actual/planned-1)*100), data)
		return
	}
	x.events.Info(trace, row.SignalID, "", EvQuantityPlan, msg, data)
}

func (x *Executor) queryManaged(row *store.CopyTradeOrder) error {
	if row.Status == "PLANNED" || row.Status == "ABANDONED" {
		return nil
	}
	m := x.ex.(types.ManagedOrderTrader)
	result, err := m.GetManagedOrder(row.Symbol, row.OrderID, row.ClientID)
	if err != nil {
		if errors.Is(err, types.ErrManagedOrderNotFound) {
			abandoned, aerr := x.abandonUnlandedOrder(row, err)
			if aerr != nil {
				return aerr
			}
			if abandoned {
				return nil
			}
		}
		return err
	}
	return x.recordManagedResult(row, result)
}

// abandonUnlandedOrder converges an entry or exit row whose submission never
// reached the venue. Only rows WITHOUT a venue order ID qualify (a known order
// ID reported as not found is an inconsistency, not an unlanded submit), and
// only after the venue kept reporting not-found for managedNotFoundGrace since
// SUBMITTING/UNKNOWN was recorded. TP/SL rows keep their own recovery paths.
func (x *Executor) abandonUnlandedOrder(row *store.CopyTradeOrder, cause error) (bool, error) {
	if row.OrderID != "" || (row.Status != "SUBMITTING" && row.Status != "UNKNOWN") {
		return false, nil
	}
	if !strings.HasPrefix(row.Role, "ENTRY_") && !strings.HasPrefix(row.Role, "CLOSE_") {
		return false, nil
	}
	if row.UpdatedAt.IsZero() || time.Since(row.UpdatedAt) < managedNotFoundGrace {
		return false, nil
	}
	reason := fmt.Sprintf("NOT_FOUND_AFTER_GRACE: venue reports client ID %s unknown %s after submission; treated as not landed (%v)", row.ClientID, time.Since(row.UpdatedAt).Truncate(time.Second), cause)
	if err := x.st.CopyTrade().UpdateOrder(row.ID, map[string]interface{}{"status": "ABANDONED", "last_error": reason}); err != nil {
		return false, err
	}
	row.Status, row.LastError = "ABANDONED", reason
	trace := row.SignalID
	if trace == "" {
		trace = "reconcile-" + row.ContextID
	}
	x.events.Warn(trace, row.SignalID, "", EvReconcile, fmt.Sprintf("%s %s %s: %s", row.Symbol, row.Direction, row.Role, reason), map[string]interface{}{"context_id": row.ContextID, "order_leg": row})
	return true, nil
}

// A submit is permitted exactly once after its intent was committed. Any
// ambiguous response is recovered by client ID; never repeat a market order.
func (x *Executor) submitManaged(row *store.CopyTradeOrder, rules *types.ManagedMarketRules, reduce bool) error {
	return x.submitManagedChecked(row, rules, reduce, nil)
}

// submitManagedChecked runs preSubmit (entries only) after source admission
// passed and immediately before SUBMITTING is written. An errEntryRevoked
// result abandons the PLANNED row exactly like a revoked admission.
func (x *Executor) submitManagedChecked(row *store.CopyTradeOrder, rules *types.ManagedMarketRules, reduce bool, preSubmit func(*store.CopyTradeOrder) error) error {
	if row.Status != "PLANNED" {
		return x.queryManaged(row)
	}
	if !reduce {
		var checks []func(*store.CopyTradeOrder) error
		if x.beforeEntrySubmit != nil {
			checks = append(checks, x.beforeEntrySubmit)
		}
		if preSubmit != nil {
			checks = append(checks, preSubmit)
		}
		for _, check := range checks {
			if err := check(row); err != nil {
				if errors.Is(err, errEntryRevoked) {
					if cancelErr := x.cancelManaged(row); cancelErr != nil {
						return cancelErr
					}
				}
				return err
			}
		}
	}
	if err := x.st.CopyTrade().UpdateOrder(row.ID, map[string]interface{}{"status": "SUBMITTING"}); err != nil {
		return err
	}
	row.Status, row.UpdatedAt = "SUBMITTING", time.Now().UTC()
	side := openingSide(row.Direction)
	if reduce {
		side = closingSide(row.Direction)
	}
	result, err := x.ex.(types.ManagedOrderTrader).SubmitManagedOrder(&types.ManagedOrderRequest{Symbol: row.Symbol, Type: row.OrderType, Side: side, PositionSide: row.Direction, ClientID: row.ClientID, Quantity: row.Quantity, Price: row.Price, ReferencePrice: row.Price, ReduceOnly: reduce, Rules: rules})
	if err != nil {
		if errors.Is(err, types.ErrManagedOrderRejected) {
			if persistErr := x.st.CopyTrade().UpdateOrder(row.ID, map[string]interface{}{"status": "REJECTED", "last_error": err.Error()}); persistErr != nil {
				return fmt.Errorf("record rejection: %w (exchange: %v)", persistErr, err)
			}
			row.Status = "REJECTED"
			return err
		}
		if persistErr := x.st.CopyTrade().UpdateOrder(row.ID, map[string]interface{}{"status": "UNKNOWN", "last_error": err.Error()}); persistErr != nil {
			return fmt.Errorf("record uncertain submission: %w (exchange: %v)", persistErr, err)
		}
		row.Status, row.UpdatedAt = "UNKNOWN", time.Now().UTC()
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

// splitStopDistanceFloor applies the single-entry 0.05% stop-distance sizing
// floor to BOTH legs of a shared-risk combination. A leg whose stop is too
// close would carry an absurd quantity; the whole combination is rejected
// (it is never silently converted into a single full-size entry).
func splitStopDistanceFloor(first, second, stop float64) error {
	for _, leg := range []struct {
		name  string
		price float64
	}{{"first", first}, {"second", second}} {
		if leg.price <= 0 {
			return fmt.Errorf("%s leg price missing", leg.name)
		}
		if r := math.Abs(leg.price-stop) / leg.price; r < MinStopDistanceRatio {
			return fmt.Errorf("%s leg stop distance %.4f%% of %.8g is below the %.2f%% sizing floor; combination rejected", leg.name, r*100, leg.price, MinStopDistanceRatio*100)
		}
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
	if err := splitStopDistanceFloor(plan.EntryPrice, plan.SplitReference, plan.StopLoss); err != nil {
		return 0, 0, err
	}
	q1, q2 := plan.RiskBudget/2/d1, plan.RiskBudget/2/d2
	notional := q1*plan.EntryPrice + q2*plan.SplitReference
	cap := math.Min(EffectiveNotionalCap(plan.MaxNotional), plan.AvailableMargin*.9*float64(plan.Leverage))
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
	m := x.ex.(types.ManagedOrderTrader)
	rules, err := m.MarketRules(plan.Symbol)
	if err != nil {
		return nil, err
	}
	split := plan.SplitReference > 0
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
	legs := []map[string]interface{}{{"type": plan.EntryType, "price": plan.EntryPrice, "quantity": q1, "risk_fraction": 1.0}}
	if split {
		legs[0]["risk_fraction"] = 0.5
		legs = append(legs, map[string]interface{}{"type": EntryPlanLimit, "price": plan.SplitReference, "quantity": q2, "risk_fraction": 0.5})
	}
	x.logEntryPlan(traceID, signalID, plan, legs)
	policy := EntryPolicyLegacy
	if split {
		policy = EntryPolicySplit
	}
	state := managedEntryPlan{Version: 1, RiskBudget: plan.RiskBudget, StopLoss: plan.StopLoss, MaxNotional: EffectiveNotionalCap(plan.MaxNotional), MarginBudget: plan.AvailableMargin * .9, SignalExpiresAt: plan.SignalExpiresAt, Rules: *rules, SplitReference: plan.SplitReference}
	if len(plan.EntryDecisionJSON) > 0 {
		var shape resolvedEntryPlan
		if json.Unmarshal(plan.EntryDecisionJSON, &shape) == nil && shape.Decision.SourcePriceSpec.Type != "" {
			state.Decision = &shape.Decision
		}
	}
	b, _ := json.Marshal(state)
	ctx := &store.CopyTradeContext{ID: uuid.NewString(), ExchangeID: plan.ExchangeID, RulesSnapshotJSON: plan.RulesSnapshotJSON, EntrySignalID: signalID, RequestedStopLoss: plan.RequestedStopLoss, TraderID: x.traderID, ChannelID: plan.ChannelID, RootMessageID: plan.RootMsgID, Symbol: plan.Symbol, RawSymbol: plan.RawSymbol, Direction: string(plan.Direction), State: string(StateEntryPending), ExecutionVersion: 1, EntryPolicy: policy, EntryPlanJSON: string(b), PlannedEntryPrice: plan.EntryPrice, StopLossPrice: plan.StopLoss, Leverage: plan.Leverage, TPRecipeJSON: planRecipeJSON(plan), EntryWorking: true}
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

// errEntryRevalidation: a recovered market first leg failed its fresh price
// checks. Always wrapped together with errEntryRevoked (the row is abandoned).
var errEntryRevalidation = errors.New("deferred market entry failed re-validation")

func revokeRepriced(format string, args ...interface{}) error {
	return fmt.Errorf("%w: %w: %s", errEntryRevoked, errEntryRevalidation, fmt.Sprintf(format, args...))
}

// revalidateDeferredMarket re-applies routeOpen's admission rules to a market
// first leg that is submitted later than its signal (deferred admission or an
// interrupted open): stop not already crossed, the frozen entry decision
// still yields MARKET at the live price, and the quantity re-sized at the
// live price stays within the original risk/notional/margin budget. A failed
// price read keeps the leg PLANNED; any failed check abandons it.
func (x *Executor) revalidateDeferredMarket(ctx *store.CopyTradeContext, plan *managedEntryPlan, row *store.CopyTradeOrder, split bool) error {
	market, err := x.ex.GetMarketPrice(row.Symbol)
	if err != nil || market <= 0 || math.IsNaN(market) || math.IsInf(market, 0) {
		return fmt.Errorf("%w: live price unavailable for deferred market entry (%v)", errEntryDeferred, err)
	}
	long := ctx.Direction != string(DirectionShort)
	stop := plan.StopLoss
	if stop <= 0 {
		stop = ctx.StopLossPrice
	}
	if stop <= 0 {
		return revokeRepriced("stop loss unknown")
	}
	if (long && market <= stop) || (!long && market >= stop) {
		return revokeRepriced("market %.8g already through stop loss %.8g", market, stop)
	}
	if d := plan.Decision; d != nil && (d.ThresholdApplied == nil || *d.ThresholdApplied) {
		fresh, derr := decideEntry(Direction(ctx.Direction), d.SourcePriceSpec, market, d.ThresholdPct, d.LimitToMarketWithin)
		if derr != nil {
			return revokeRepriced("entry decision at %.8g failed: %v", market, derr)
		}
		if fresh.OrderType != EntryPlanMarket {
			return revokeRepriced("entry decision at %.8g is now %s (%s), not market", market, fresh.OrderType, fresh.Reason)
		}
	}
	if ref := plan.SplitReference; split && ref > 0 && ((long && ref >= market) || (!long && ref <= market)) {
		return revokeRepriced("second-leg reference %.8g no longer between stop %.8g and market %.8g", ref, stop, market)
	}
	qty := row.Quantity
	if plan.RiskBudget > 0 {
		budget := plan.RiskBudget
		if split {
			budget /= 2 // the first leg carries half of the shared risk
		}
		qty = math.Min(qty, budget/math.Abs(market-stop))
	}
	qty = math.Min(qty, EffectiveNotionalCap(plan.MaxNotional)/market)
	if plan.MarginBudget > 0 && ctx.Leverage > 0 {
		qty = math.Min(qty, plan.MarginBudget*float64(ctx.Leverage)/market)
	}
	qty, err = plan.Rules.FloorQuantity(types.SanitizeBaseQuantity(qty), true)
	if err != nil || qty*market < plan.Rules.MinNotional {
		return revokeRepriced("quantity re-sized at %.8g is below the exchange minimum (%v)", market, err)
	}
	updates := map[string]interface{}{"price": market}
	if qty+1e-12 < row.Quantity {
		updates["quantity"] = qty
	}
	if err = x.st.CopyTrade().UpdateOrder(row.ID, updates); err != nil {
		return err
	}
	if qty+1e-12 < row.Quantity {
		x.events.Info("reconcile-"+ctx.ID, row.SignalID, "", EvQuantityPlan, fmt.Sprintf("deferred market entry re-sized at %.8g: %.8g -> %.8g (risk budget preserved)", market, row.Quantity, qty), map[string]interface{}{"context_id": ctx.ID})
		row.Quantity = qty
	}
	row.Price = market
	return nil
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
		// The latch above is durable; only venue-side settlement is unknown.
		return fmt.Errorf("%w: %w", errEntryCancelUnconfirmed, errors.Join(cancelErr, err))
	}
	if cancelErr != nil {
		// A failed cancellation must not prevent protection of known fills.
		if pos, perr := x.freshPosition(ctx.Symbol, ctx.Direction); perr == nil && pos != nil && pos.qty > 0 {
			cancelErr = errors.Join(cancelErr, x.ensureStopProtection(traceID, "", ctx, pos.qty))
		}
		return fmt.Errorf("%w: %w", errEntryCancelUnconfirmed, cancelErr)
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
		// This trade owns at most its own confirmed entry fills; a larger
		// position on the side is not adopted (manual/foreign exposure).
		updates["quantity"] = math.Min(pos.qty, total)
		if pos.qty > total+1e-9 && ctx.LastAction != "POSITION_GROWTH_NOT_ADOPTED" {
			updates["last_action"] = "POSITION_GROWTH_NOT_ADOPTED"
			x.events.Warn("reconcile-"+ctx.ID, "", "", EvReconcile, fmt.Sprintf("%s %s exchange position %.8g exceeds this trade's confirmed entry fills %.8g; excess not adopted — manual review required", ctx.Symbol, ctx.Direction, pos.qty, total), map[string]interface{}{"context_id": ctx.ID})
		}
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
					if errors.Is(err, ErrProtectionUnconfirmed) {
						x.events.Warn(traceID, "", "", EvReconcile, "PROTECTION_PENDING: "+err.Error(), nil)
					} else {
						x.events.Error(traceID, "", "", EvExecutionError, "UNPROTECTED: "+err.Error(), nil)
					}
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
	for _, o := range orders {
		// A leg that just converged to ABANDONED (never landed) or REJECTED
		// ends replenishment in this same pass.
		if o.Status == "REJECTED" || o.Status == "ABANDONED" {
			ctx.EntryDisabled = true
		}
	}
	if err = x.settleManagedFills(ctx, orders); err != nil {
		return err
	}
	advanced, err := x.refreshTPProgressChecked(traceID, ctx)
	if err != nil {
		return err
	}
	if advanced || ctx.Quantity+1e-10 < oldQty {
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
			if errors.Is(err, ErrProtectionUnconfirmed) {
				return err
			}
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
				if _, err = x.refreshTPProgressChecked(traceID, ctx); err != nil {
					return err
				}
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
				want = math.Min(want, math.Max(0, EffectiveNotionalCap(plan.MaxNotional)-usedNotional)/o.Price)
				if plan.MarginBudget > 0 {
					want = math.Min(want, math.Max(0, plan.MarginBudget*float64(ctx.Leverage)-usedNotional)/o.Price)
				}
				_, available, balanceErr := readAccountBalances(x.ex)
				if balanceErr != nil {
					return balanceErr
				}
				want = math.Min(want, available*.9*float64(ctx.Leverage)/o.Price)
				want, err = plan.Rules.FloorQuantity(want, false)
				if err != nil || want*o.Price < plan.Rules.MinNotional {
					return x.closeEntryEligibility(traceID, ctx, "SECOND_LEG_BELOW_MINIMUM")
				}
				if err = x.st.CopyTrade().UpdateOrder(o.ID, map[string]interface{}{"quantity": want}); err != nil {
					return err
				}
				o.Quantity = want
			}
			var preSubmit func(*store.CopyTradeOrder) error
			if i == 0 && o.OrderType == "MARKET" {
				// A first market leg submitted from recovery (deferred admission
				// or an interrupted open) is priced NOW, not at signal time.
				split := len(orders) > 1
				preSubmit = func(row *store.CopyTradeOrder) error {
					return x.revalidateDeferredMarket(ctx, &plan, row, split)
				}
			}
			if err = x.submitManagedChecked(o, &plan.Rules, false, preSubmit); err != nil {
				if i == 0 && errors.Is(err, errEntryRevalidation) {
					x.events.Warn(traceID, o.SignalID, "", EvSignalSkipped, "deferred market entry abandoned before submission: "+err.Error(), map[string]interface{}{"context_id": ctx.ID, "order_leg": o})
					if err = x.persistContext(ctx, map[string]interface{}{"last_action": "ENTRY_REVOKED_REPRICED", "last_error": "ENTRY_REVOKED: " + err.Error()}); err != nil {
						return err
					}
					if err = x.closeEntryEligibility(traceID, ctx, "ENTRY_REVOKED_REPRICED"); err != nil {
						return err
					}
					if orders, err = x.entryOrders(ctx); err != nil {
						return err
					}
					break
				}
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
		if err = x.markContext(ctx, state, nil); err != nil {
			return err
		}
	}
	if ctx.LastError != "" && !strings.HasPrefix(ctx.LastError, "FLAT_RECOVERY_PENDING:") && !strings.HasPrefix(ctx.LastError, "ENTRY_REVOKED:") {
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
	capQty = math.Min(capQty, EffectiveNotionalCap(plan.MaxNotional)/ctx.AvgFillPrice)
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
	capQty = math.Min(capQty, EffectiveNotionalCap(plan.MaxNotional)/ctx.AvgFillPrice)
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
	SignalID string `json:"signal_id,omitempty"`
	TPLevel  int    `json:"tp_level,omitempty"`
	OrderID  string `json:"order_id"`
	Full     bool   `json:"full"`
	// Resubmission bookkeeping (absent in intents written before it existed).
	Attempts       int        `json:"attempts,omitempty"`         // resubmits of this intent so far
	PriorFilled    float64    `json:"prior_filled,omitempty"`     // fills of earlier generations
	ResidualSeenAt *time.Time `json:"residual_seen_at,omitempty"` // first sighting of a residual after a FILLED full close
}

func (x *Executor) executeManagedClose(traceID, signalID string, ctx *store.CopyTradeContext, ratio float64) (SkipReason, error) {
	return x.executeManagedCloseAmount(traceID, signalID, ctx, ratio, 0)
}

// exitRole is the durable role of every exit generation of one signal. The
// first generation keeps the historical row ID; later generations add their
// ordinal so a resubmission never collides with stableID(ctx.ID, role).
func exitRole(signalID string) string { return "CLOSE_" + signalID }

func (x *Executor) newExitOrder(ctx *store.CopyTradeContext, signalID string, generation int, price, qty float64) *store.CopyTradeOrder {
	role := exitRole(signalID)
	key := role
	if generation > 0 {
		key = fmt.Sprintf("%s#%d", role, generation)
	}
	id := stableID(ctx.ID, key)
	return &store.CopyTradeOrder{ID: id, TraderID: x.traderID, ContextID: ctx.ID, SignalID: signalID, Role: role, Symbol: ctx.Symbol, Direction: ctx.Direction, ClientID: "ct-" + id[:28], OrderType: "MARKET", Price: price, Quantity: qty, Status: "PLANNED"}
}

// exitRows returns every recorded exit generation of signalID on this trade.
func (x *Executor) exitRows(ctx *store.CopyTradeContext, signalID string) ([]*store.CopyTradeOrder, error) {
	all, err := x.st.CopyTrade().GetOrdersForContext(x.traderID, ctx.ID)
	if err != nil {
		return nil, err
	}
	var rows []*store.CopyTradeOrder
	for _, o := range all {
		if o.Role == exitRole(signalID) {
			rows = append(rows, o)
		}
	}
	return rows, nil
}

// pendingExitSignal identifies the signal that owns the current exit intent.
func (x *Executor) pendingExitSignal(ctx *store.CopyTradeContext, intent managedExitIntent) string {
	if intent.SignalID != "" {
		return intent.SignalID
	}
	if rows, err := x.st.CopyTrade().GetOrdersForContext(x.traderID, ctx.ID); err == nil {
		for _, o := range rows {
			if o.ID == intent.OrderID {
				return o.SignalID
			}
		}
	}
	return ""
}

// minimumReduction is used only to remove a known risk overshoot. Round that
// reduction UP (never an entry), so a market lot does not leave excess risk.
func (x *Executor) executeManagedCloseAmount(traceID, signalID string, ctx *store.CopyTradeContext, ratio, minimumReduction float64) (SkipReason, error) {
	if ctx.ClosePendingJSON != "" {
		var pending managedExitIntent
		if err := json.Unmarshal([]byte(ctx.ClosePendingJSON), &pending); err != nil {
			return SkipNone, err
		}
		owner := x.pendingExitSignal(ctx, pending)
		requestFull := minimumReduction <= 0 && (ratio <= 0 || ratio >= 100)
		// Only a full close from ANOTHER signal may go past a pending partial
		// exit; everything else keeps the historical duplicate semantics.
		if owner == signalID || !requestFull || pending.Full {
			return SkipDuplicate, x.reconcileManagedClose(traceID, ctx)
		}
		// Settle the partial exit without resubmitting it: the full close
		// below replaces it and is sized from the fresh position.
		reconcileErr := x.reconcileManagedCloseStep(traceID, ctx, false)
		if ctx.ClosePendingJSON != "" {
			superseded, err := x.supersedePartialExit(traceID, signalID, ctx)
			if err != nil {
				return SkipWaitingReconciliation, err
			}
			if !superseded {
				return SkipWaitingReconciliation, fmt.Errorf("partial exit still in flight; full close waits for it to settle (%v)", reconcileErr)
			}
		}
		if TradeState(ctx.State).IsTerminal() {
			return SkipAlreadyFlat, nil
		}
	}
	prior, err := x.exitRows(ctx, signalID)
	if err != nil {
		return SkipNone, err
	}
	full := ratio <= 0 || ratio >= 100
	for _, o := range prior {
		if !orderTerminal(o.Status) {
			return SkipWaitingReconciliation, fmt.Errorf("exit %s for this signal is unresolved (%s); no new order", o.ClientID, o.Status)
		}
		// A partial reduction that already executed must never be repeated.
		// A full close always targets the fresh remaining position instead.
		if (o.ExecutedQty > 0 || o.Status == "FILLED") && !full {
			return SkipDuplicate, nil
		}
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
	row := x.newExitOrder(ctx, signalID, len(prior), pos.entryPrice, qty)
	intent, _ := json.Marshal(managedExitIntent{SignalID: signalID, OrderID: row.ID, Full: full})
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
	return x.reconcileManagedCloseStep(traceID, ctx, true)
}

// reconcileManagedCloseStep settles the durable exit intent. allowResubmit is
// false right after a resubmission so one pass sends at most one new order.
func (x *Executor) reconcileManagedCloseStep(traceID string, ctx *store.CopyTradeContext, allowResubmit bool) error {
	var intent managedExitIntent
	if err := json.Unmarshal([]byte(ctx.ClosePendingJSON), &intent); err != nil {
		return err
	}
	if intent.TPLevel > 0 && intent.OrderID == "" {
		_, err := x.ExecuteTPReduction(traceID, intent.SignalID, ctx, intent.TPLevel)
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
	if intent.TPLevel > 0 {
		if err := x.recordTPReplacementFill(ctx, intent, row); err != nil {
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
		if err = x.finishTradeOrders(ctx); err != nil {
			return err
		}
		now := time.Now().UTC()
		return x.finalizeManagedExit(ctx, row.SignalID, map[string]interface{}{"state": string(StateClosed), "closed_at": &now, "quantity": 0, "close_pending_json": "", "last_action": "CLOSE"})
	}
	if row.Status == "FILLED" && !intent.Full {
		state := StateOpen
		if ctx.BreakevenApplied {
			state = StateBreakeven
		}
		return x.finalizeManagedExit(ctx, row.SignalID, map[string]interface{}{"state": string(state), "quantity": pos.qty, "close_pending_json": "", "last_action": "REDUCE"})
	}
	if row.Status == "FILLED" {
		// A filled full close with a visible residual: one read may be stale,
		// so require a second sighting on a later pass before acting on it.
		if intent.ResidualSeenAt == nil {
			now := time.Now().UTC()
			intent.ResidualSeenAt = &now
			b, _ := json.Marshal(intent)
			if err = x.persistContext(ctx, map[string]interface{}{"close_pending_json": string(b)}); err != nil {
				return err
			}
			return fmt.Errorf("full close filled but remaining position %.8g is still visible; awaiting fresh confirmation", pos.qty)
		}
		if time.Since(*intent.ResidualSeenAt) < exitResidualConfirmAfter {
			return fmt.Errorf("full close filled but remaining position %.8g is still visible; awaiting fresh confirmation", pos.qty)
		}
	}
	if !allowResubmit {
		return fmt.Errorf("exit resubmitted (%s); awaiting next reconciliation, remaining position %.8g", row.Status, pos.qty)
	}
	return x.resubmitManagedExit(traceID, ctx, intent, row, pos)
}

// resubmitManagedExit replaces a definitively ended exit order (rejected,
// cancelled, expired, abandoned, or a full close with a confirmed residual)
// with a new generation sized from the FRESH position. It never reuses the old
// client ID. Bounded by maxExitResubmits; a quantity the venue cannot execute
// releases the intent for manual review instead of blocking later exits.
func (x *Executor) resubmitManagedExit(traceID string, ctx *store.CopyTradeContext, intent managedExitIntent, prev *store.CopyTradeOrder, pos *positionInfo) error {
	signalID := intent.SignalID
	if signalID == "" {
		signalID = prev.SignalID
	}
	if intent.Attempts >= maxExitResubmits {
		return x.abandonManagedExit(traceID, ctx, signalID, pos, fmt.Sprintf("exit ended %s with remaining position %.8g after %d resubmissions", prev.Status, pos.qty, intent.Attempts))
	}
	rules, err := x.ex.(types.ManagedOrderTrader).MarketRules(ctx.Symbol)
	if err != nil {
		return err
	}
	want := pos.qty
	if !intent.Full {
		want = math.Min(pos.qty, math.Max(0, prev.Quantity-prev.ExecutedQty))
	}
	qty, ferr := rules.FloorQuantity(types.SanitizeBaseQuantity(want), true)
	if ferr != nil || qty <= 0 {
		return x.abandonManagedExit(traceID, ctx, signalID, pos, fmt.Sprintf("exit ended %s; remaining %.8g cannot be sent as a market reduce-only order (%v)", prev.Status, want, ferr))
	}
	rows, err := x.exitRows(ctx, signalID)
	if err != nil {
		return err
	}
	row := x.newExitOrder(ctx, signalID, len(rows), pos.entryPrice, qty)
	next := intent
	next.SignalID, next.OrderID = signalID, row.ID
	next.Attempts++
	next.PriorFilled += prev.ExecutedQty
	next.ResidualSeenAt = nil
	b, _ := json.Marshal(next)
	if err = x.st.CopyTrade().CreateExitOrder(ctx, row, string(b)); err != nil {
		return err
	}
	ctx.State = string(StateClosePending)
	ctx.ClosePendingJSON = string(b)
	ctx.Version++
	x.events.Warn(traceID, signalID, "", EvCloseSubmitted, fmt.Sprintf("%s %s exit %s ended %s with position %.8g remaining; resubmitting %.8g as generation %d with a new client ID", ctx.Symbol, ctx.Direction, prev.ClientID, prev.Status, pos.qty, qty, len(rows)), map[string]interface{}{"context_id": ctx.ID, "previous": prev, "next": row})
	if err = x.submitManaged(row, rules, true); err != nil {
		return err
	}
	return x.reconcileManagedCloseStep(traceID, ctx, false)
}

// abandonManagedExit releases an exit intent that cannot converge on its own.
// The trade returns to OPEN/BREAKEVEN with its fresh quantity so protections
// keep running and a later author exit (a full close) can still execute.
func (x *Executor) abandonManagedExit(traceID string, ctx *store.CopyTradeContext, signalID string, pos *positionInfo, reason string) error {
	state := StateOpen
	if ctx.BreakevenApplied {
		state = StateBreakeven
	}
	message := "EXIT_ABANDONED: " + reason + "; manual review required"
	if err := x.persistContext(ctx, map[string]interface{}{"state": string(state), "close_pending_json": "", "quantity": pos.qty, "last_action": "EXIT_ABANDONED", "last_error": message}); err != nil {
		return err
	}
	// The abandoned author action must not be blindly retried by recovery.
	if actions, err := x.st.CopyTrade().PendingActions(x.traderID, ctx.ID); err == nil {
		for _, a := range actions {
			if a.SignalID == signalID && (a.Action == string(ActionClose) || a.Action == string(ActionReduce)) {
				_ = x.st.CopyTrade().UpdateAction(a.ID, map[string]interface{}{"status": "rejected", "phase": "exit_abandoned", "error": message})
			}
		}
	}
	x.events.Error(traceID, signalID, "", EvExecutionError, fmt.Sprintf("%s %s: %s", ctx.Symbol, ctx.Direction, message), map[string]interface{}{"context_id": ctx.ID, "position_quantity": pos.qty})
	if err := x.restoreRemainingProtections(traceID, signalID, ctx, pos.qty); err != nil {
		x.events.Warn(traceID, signalID, "", EvExecutionError, "protection repair after abandoned exit: "+err.Error(), nil)
	}
	return fmt.Errorf("%s", message)
}

// supersedePartialExit lets a full close from a new author signal replace a
// partial exit that is no longer in flight. Returns false while the partial
// order may still execute (it settles first; the full close is retried).
func (x *Executor) supersedePartialExit(traceID, signalID string, ctx *store.CopyTradeContext) (bool, error) {
	var intent managedExitIntent
	if err := json.Unmarshal([]byte(ctx.ClosePendingJSON), &intent); err != nil {
		return false, err
	}
	if intent.Full {
		return false, nil
	}
	if intent.OrderID != "" {
		rows, err := x.st.CopyTrade().GetOrdersForContext(x.traderID, ctx.ID)
		if err != nil {
			return false, err
		}
		for _, o := range rows {
			if o.ID == intent.OrderID && !orderTerminal(o.Status) {
				return false, nil
			}
		}
	}
	state := StateOpen
	if ctx.BreakevenApplied {
		state = StateBreakeven
	}
	if err := x.persistContext(ctx, map[string]interface{}{"state": string(state), "close_pending_json": "", "last_action": "EXIT_SUPERSEDED"}); err != nil {
		return false, err
	}
	x.events.Warn(traceID, signalID, "", EvTradeUpdated, fmt.Sprintf("%s %s: pending partial exit of %s superseded by full close %s", ctx.Symbol, ctx.Direction, x.pendingExitSignal(ctx, intent), signalID), map[string]interface{}{"context_id": ctx.ID, "superseded_intent": intent})
	return true, nil
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
		if TradeState(ctx.State).IsTerminal() && (a.Action == string(ActionOpen) || a.Action == string(ActionAdd)) {
			e.settleTerminalEntryAction(ctx, a)
			continue
		}
		if TradeState(ctx.State).IsTerminal() && a.Action != string(ActionCancel) && a.Action != string(ActionClose) && a.Action != string(ActionReduce) {
			continue
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
				// Legacy venues have no client-ID recovery: retry ONLY an exit
				// whose durable intent proves no order was sent for it.
				if ins.ReduceTPLevel > 0 || !e.exec.legacyExitRetryable(ctx, a.SignalID) {
					continue
				}
				ratio := 100.0
				if ins.Action == ActionReduce || ins.CloseMode == CloseModePartial {
					rules, rerr := e.rulesForSignal(a.SignalID)
					if rerr != nil {
						continue
					}
					ratio = rules.ReduceRatio
					if ins.CloseRatio != nil {
						ratio = *ins.CloseRatio
					}
				}
				_, err = e.exec.ExecuteClose(trace, a.SignalID, ctx, ratio)
				if err != nil && !errors.Is(err, errExitNotSubmitted) {
					_ = e.st.CopyTrade().UpdateAction(a.ID, map[string]interface{}{"status": "uncertain", "phase": "submission_unknown", "error": err.Error()})
				}
				break
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
					rules, rerr := e.rulesForSignal(a.SignalID)
					if rerr != nil {
						continue
					}
					ratio = rules.ReduceRatio
					if ins.CloseRatio != nil {
						ratio = *ins.CloseRatio
					}
				}
				if ins.ReduceTPLevel > 0 {
					_, err = e.exec.ExecuteTPReduction(trace, a.SignalID, ctx, ins.ReduceTPLevel)
				} else {
					_, err = e.exec.ExecuteClose(trace, a.SignalID, ctx, ratio)
				}
			}
		case ActionCancel:
			if TradeState(ctx.State).IsTerminal() && !ctx.EntryWorking {
				break
			}
			_, err = e.exec.ExecuteCancel(trace, a.SignalID, ctx)
		case ActionUpdateTP:
			if ctx.LastTPSignalID == a.SignalID && ctx.TPUpdateIntentJSON == "" {
				break
			}
			if ctx.TPUpdateIntentJSON == "" {
				continue
			} // no durable prices: never infer an old target
			_, err = e.exec.updateTakeProfits(trace, a.SignalID, ctx, nil, nil)

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
			if e.st.CopyTrade().UpdateAction(a.ID, map[string]interface{}{"status": "done", "error": ""}) == nil {
				e.syncRecoveredSignal(a, store.SignalStatusExecuted, "")
			}
		}
	}
}

// settleTerminalEntryAction closes an OPEN/ADD action that is still pending
// (typically executing/waiting_admission after a deferred entry was revoked)
// once its trade reached a terminal state with every entry leg settled.
func (e *Engine) settleTerminalEntryAction(ctx *store.CopyTradeContext, a *store.CopyTradeAction) {
	orders, err := e.exec.entryOrders(ctx)
	if err != nil {
		return
	}
	filled := ctx.EntryFilledQuantity > 0 || ctx.OpenedAt != nil
	revoked := strings.HasPrefix(ctx.LastError, "ENTRY_REVOKED") || strings.HasPrefix(ctx.LastAction, "ENTRY_REVOKED")
	for _, o := range orders {
		if !orderTerminal(o.Status) {
			return // a leg is still unresolved; keep the action pending
		}
		filled = filled || o.ExecutedQty > 0
		revoked = revoked || (o.Status == "ABANDONED" && o.OrderID == "")
	}
	phase, signalStatus, skip := "completed", store.SignalStatusExecuted, ""
	switch {
	case filled:
	case revoked:
		phase, signalStatus, skip = "admission_revoked", store.SignalStatusSkipped, "ENTRY_ADMISSION_REVOKED"
	case ctx.State == string(StateExpired):
		phase, signalStatus, skip = "expired", store.SignalStatusSkipped, string(SkipExpired)
	default:
		phase, signalStatus, skip = "cancelled", store.SignalStatusSkipped, "ENTRY_CANCELLED"
	}
	updates := map[string]interface{}{"status": "done", "phase": phase}
	if phase != "completed" && a.Error == "" && ctx.LastError != "" {
		updates["error"] = ctx.LastError
	}
	if err = e.st.CopyTrade().UpdateAction(a.ID, updates); err != nil {
		return
	}
	e.events.Info("reconcile-"+ctx.ID, a.SignalID, a.MessageID, EvReconcile,
		fmt.Sprintf("%s %s %s action settled after trade became %s: phase %s", ctx.Symbol, ctx.Direction, a.Action, ctx.State, phase),
		map[string]interface{}{"context_id": ctx.ID, "action_id": a.ID, "phase": phase})
	e.syncRecoveredSignal(a, signalStatus, skip)
}

// syncRecoveredSignal moves a signal that is still waiting on recovery
// (executing / execution_wait) to its recovered outcome, once none of its
// recorded actions remain unresolved. Committed outcomes are never rewritten.
func (e *Engine) syncRecoveredSignal(a *store.CopyTradeAction, status, skipReason string) {
	if a.SignalID == "" {
		return
	}
	sig, err := e.st.CopyTrade().GetSignal(a.SignalID)
	if err != nil || sig == nil || (sig.Status != store.SignalStatusExecuting && sig.Status != "execution_wait") {
		return
	}
	if actions, aerr := e.st.CopyTrade().GetActionsForMessage(e.traderID, a.MessageID); aerr != nil {
		return
	} else {
		for _, other := range actions {
			if other.SignalID == a.SignalID && other.ID != a.ID && (other.Status == "executing" || other.Status == "uncertain") {
				return
			}
		}
	}
	updates := map[string]interface{}{"status": status, "next_retry_at": nil, "skip_reason": skipReason}
	if err = e.st.CopyTrade().UpdateSignal(a.SignalID, updates); err != nil {
		return
	}
	e.events.Info("reconcile-action-"+a.ID, a.SignalID, a.MessageID, EvReconcile,
		fmt.Sprintf("signal outcome recovered: %s %s", status, skipReason), map[string]interface{}{"action_id": a.ID, "context_id": a.ContextID})
}

// Orphan intents have no exchange side effects if they never passed preflight.
// Older ambiguous rows remain unresolved, with a diagnostic instead of a new order.
func (e *Engine) recoverOrphanActions() {
	actions, err := e.st.CopyTrade().UnresolvedActions(e.traderID)
	if err != nil {
		return
	}
	for _, a := range actions {
		if a.ContextID != "" {
			if c, cerr := e.st.CopyTrade().GetContext(a.ContextID); cerr == nil && c != nil && TradeState(c.State).IsTerminal() {
				e.recoverPendingActions(c)
			}
			continue
		}
		orders, qerr := e.st.CopyTrade().GetOrdersForSignal(e.traderID, a.SignalID)
		if qerr != nil {
			continue
		}
		ids := map[string]bool{}
		for _, o := range orders {
			if o.Symbol == a.Symbol && o.Direction == a.Direction {
				ids[o.ContextID] = true
			}
		}
		if len(ids) == 1 {
			for id := range ids {
				if err = e.st.CopyTrade().UpdateAction(a.ID, map[string]interface{}{"context_id": id}); err == nil {
					if c, cerr := e.st.CopyTrade().GetContext(id); cerr == nil && c != nil {
						e.recoverPendingActions(c)
					}
				}
			}
			continue
		}
		if len(ids) == 0 && a.Phase == "preflight" {
			_ = e.st.CopyTrade().UpdateAction(a.ID, map[string]interface{}{"status": "preflight_rejected", "error": "interrupted before durable execution intent"})
		} else if a.Error != "orphan action outcome unresolved; manual audit required" {
			_ = e.st.CopyTrade().UpdateAction(a.ID, map[string]interface{}{"error": "orphan action outcome unresolved; manual audit required"})
			e.events.Warn("reconcile-action-"+a.ID, a.SignalID, a.MessageID, EvExecutionError, "orphan action outcome unresolved; manual audit required", nil)
		}
	}
}
