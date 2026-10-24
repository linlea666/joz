package copytrader

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sync"
	"time"

	"nofx/store"
	"nofx/trader/types"
)

var ErrProtectionUnconfirmed = errors.New("protection observation or submission uncertain")

const (
	// stopNotFoundGrace bounds venue propagation delay: an algo order accepted
	// by Binance is visible to open-order and client-ID queries within seconds.
	// After a minute of consecutive not-found answers the request never landed.
	stopNotFoundGrace = 60 * time.Second
	// stopNotFoundMinObservations requires at least two separate not-found
	// answers (never a single lookup) before absence is accepted.
	stopNotFoundMinObservations = 2
	// stopUnprotectedAlertAfter: a live position whose stop has stayed
	// unconfirmed this long, with no stop order visible on its side, raises an
	// Error and is closed through the emergency path.
	stopUnprotectedAlertAfter = 3 * time.Minute
)

// unprotectedFallbackActive guards against re-entry per context ID.
var unprotectedFallbackActive sync.Map

// stopClock is replaced in tests only.
var stopClock = func() time.Time { return time.Now().UTC() }

// stopIntent is saved before touching the exchange. SUBMITTING is deliberately
// not retried: a lost acknowledgement must be reconciled by client identity.
type stopIntent struct {
	ClientID   string           `json:"client_id"`
	Price      float64          `json:"price"`
	Requested  float64          `json:"requested"`
	Quantity   float64          `json:"quantity"`
	Old        *types.OpenOrder `json:"old,omitempty"`
	Stage      string           `json:"stage"`
	ErrorCode  int64            `json:"error_code,omitempty"`
	ErrorStage string           `json:"error_stage,omitempty"`
	ErrorAt    *time.Time       `json:"error_at,omitempty"`
	// SubmittedAt is set when the request is about to be sent.
	SubmittedAt *time.Time `json:"submitted_at,omitempty"`
	// Consecutive exact client-ID history not-found answers.
	NotFoundCount   int        `json:"not_found_count,omitempty"`
	FirstNotFoundAt *time.Time `json:"first_not_found_at,omitempty"`
	// UnconfirmedSince survives resubmissions of the same protection intent.
	UnconfirmedSince *time.Time `json:"unconfirmed_since,omitempty"`
	AlertedAt        *time.Time `json:"alerted_at,omitempty"`
	// PriorClientIDs are earlier identities of this intent that were proven
	// absent; a stop with one of them is still ours (-4130 adoption).
	PriorClientIDs []string `json:"prior_client_ids,omitempty"`
}

func (p *stopIntent) ownsClientID(id string) bool {
	if id == "" {
		return false
	}
	if id == p.ClientID {
		return true
	}
	for _, prior := range p.PriorClientIDs {
		if prior == id {
			return true
		}
	}
	return false
}

func (x *Executor) normalizeStop(symbol string, price float64) (float64, float64, float64, error) {
	if !finite(price) || price <= 0 {
		return 0, 0, 0, fmt.Errorf("invalid stop price")
	}
	if reader, ok := x.ex.(types.MarketRulesReader); ok {
		rules, err := reader.MarketRules(symbol)
		if err != nil {
			return 0, 0, 0, fmt.Errorf("%w: market rules lookup failed: %v", ErrProtectionUnconfirmed, err)
		}
		p, err := rules.NormalizePrice(price)
		return p, rules.PriceTick, rules.QuantityStep, err
	}
	return price, 0, 0, nil
}
func sameStep(a, b, step float64) bool {
	tolerance := math.Max(math.Max(math.Abs(a), math.Abs(b))*1e-9, 1e-12)
	if step > 0 {
		tolerance = math.Max(tolerance, step*1e-6)
	}
	return math.Abs(a-b) <= tolerance
}

func (x *Executor) saveStopIntent(ctx *store.CopyTradeContext, intent *stopIntent) error {
	b, err := json.Marshal(intent)
	if err != nil {
		return err
	}
	return x.persistContext(ctx, map[string]interface{}{"stop_intent_json": string(b)})
}

// setStopProtection is shared by initial protection, reconciliation, explicit
// moves and breakeven. It never replaces a tighter, adequately sized stop.
func (x *Executor) setStopProtection(traceID, signalID string, ctx *store.CopyTradeContext, qty, requested float64) error {
	price, tick, step, err := x.normalizeStop(ctx.Symbol, requested)
	if err != nil {
		return err
	}
	if !finite(qty) || qty <= 0 {
		return fmt.Errorf("invalid protection quantity")
	}
	orders, err := x.ex.GetOpenOrders(ctx.Symbol)
	if err != nil {
		return fmt.Errorf("%w: lookup failed: %v", ErrProtectionUnconfirmed, err)
	}
	originalPrice, originalQty := price, qty
	var pending *stopIntent
	if ctx.StopIntentJSON != "" {
		pending = &stopIntent{}
		if err = json.Unmarshal([]byte(ctx.StopIntentJSON), pending); err != nil {
			return err
		}
		// Finish the durable operation before accepting a new target.
		price, requested, qty = pending.Price, pending.Requested, pending.Quantity
	}

	managed, precise := x.ex.(types.StopOrderTrader)
	var candidates []types.OpenOrder
	for _, o := range orders {
		if (o.Symbol == "" || o.Symbol == ctx.Symbol) && isStopOrder(o) && stopMatchesSide(o, ctx.Direction) {
			candidates = append(candidates, o)
		}
	}
	oldPrice, _, _, normErr := x.normalizeStop(ctx.Symbol, ctx.StopLossPrice)
	if normErr != nil {
		return normErr
	}
	for _, o := range candidates {
		identityOK := pending == nil || o.ClientID == pending.ClientID
		valid := (sameStep(o.StopPrice, price, tick) || tighterStop(ctx.Direction, o.StopPrice, price)) && (o.ClosePosition || sameStep(o.Quantity, qty, step))
		if !identityOK || !valid {
			continue
		}
		owned := pending != nil && o.ClientID == pending.ClientID || ctx.StopOrderID != "" && ctx.StopOrderID == o.OrderID
		legacy := ctx.StopOrderID == "" && len(candidates) == 1 && sameStep(o.StopPrice, oldPrice, tick)
		if precise && !owned && !legacy {
			return fmt.Errorf("%w: adequate stop retained but ownership cannot be established", ErrProtectionUnconfirmed)
		}
		if signalID != "" && pending == nil {
			x.events.Info(traceID, signalID, "", EvSLSet, fmt.Sprintf("SL already protects %s @ %.8g; no exchange change required", ctx.Symbol, o.StopPrice), map[string]interface{}{"context_id": ctx.ID, "order_id": o.OrderID, "requested_price": requested, "effective_price": o.StopPrice})
		}

		if ctx.StopIntentJSON == "" && ctx.StopOrderID == o.OrderID && ctx.StopLossPrice == o.StopPrice && ctx.RequestedStopLoss == requested {
			return nil
		}
		if err = x.persistContext(ctx, map[string]interface{}{"stop_loss_price": o.StopPrice, "requested_stop_loss": requested, "stop_order_id": o.OrderID, "stop_intent_json": ""}); err != nil {
			return err
		}
		if pending != nil && (!sameStep(originalPrice, pending.Price, tick) || !sameStep(originalQty, pending.Quantity, step)) {
			return fmt.Errorf("%w: previous stop intent completed; re-evaluate the newer target", ErrProtectionUnconfirmed)
		}
		return nil
	}

	if !precise { // Compatibility for adapters without stable protection identities.
		if pending != nil {
			return fmt.Errorf("pending stop requires stable order identity support")
		}
		for _, o := range candidates {
			if tighterStop(ctx.Direction, o.StopPrice, price) {
				price = o.StopPrice
			}
		}
		if len(candidates) > 0 {
			if err = x.cancelStopLossOrders(ctx.Symbol, ctx.Direction); err != nil {
				return err
			}
		}
		if err = x.ex.SetStopLoss(ctx.Symbol, positionSideOf(ctx.Direction), qty, price); err != nil {
			return err
		}
		return x.persistContext(ctx, map[string]interface{}{"stop_loss_price": price, "requested_stop_loss": requested})
	}
	if pending == nil && ctx.StopOrderID != "" && len(candidates) == 0 {
		if reader, ok := x.ex.(types.StopOrderReader); ok {
			result, herr := reader.GetStopOrder(ctx.Symbol, ctx.StopOrderID, "")
			if herr != nil || result == nil || result.OrderID != ctx.StopOrderID || result.Symbol != ctx.Symbol || !stopMatchesSide(result.OpenOrder, ctx.Direction) {
				return fmt.Errorf("%w: previous stop history unavailable or mismatched", ErrProtectionUnconfirmed)
			}
			if !result.Terminal() {
				return fmt.Errorf("%w: previous stop still active/triggering", ErrProtectionUnconfirmed)
			}
		}
		pos, perr := x.freshPosition(ctx.Symbol, ctx.Direction)
		if perr != nil {
			return fmt.Errorf("%w: fresh protection position lookup: %v", ErrProtectionUnconfirmed, perr)
		}
		if pos == nil || pos.qty <= 0 {
			return fmt.Errorf("%w: EXIT_OBSERVED; position exited before protection replacement", ErrProtectionUnconfirmed)
		}
		qty = pos.qty
	}
	if pending == nil {
		var old *types.OpenOrder
		oldPrice, _, _, err := x.normalizeStop(ctx.Symbol, ctx.StopLossPrice)
		if err != nil {
			return err
		}
		for i := range candidates {
			o := &candidates[i]
			owned := ctx.StopOrderID != "" && ctx.StopOrderID == o.OrderID
			// Legacy adoption requires one unique matching price AND full size.
			legacy := ctx.StopOrderID == "" && len(candidates) == 1 && sameStep(o.StopPrice, oldPrice, tick) && (o.ClosePosition || sameStep(o.Quantity, qty, step))
			if !owned && !legacy {
				return fmt.Errorf("%w: unattributed stop %s retained; manual ownership check required", ErrProtectionUnconfirmed, o.OrderID)
			}
			if old != nil {
				return fmt.Errorf("%w: multiple stop orders retained; ownership ambiguous", ErrProtectionUnconfirmed)
			}
			old = o
			if tighterStop(ctx.Direction, o.StopPrice, price) {
				price = o.StopPrice
			}
		}
		now := stopClock()
		pending = &stopIntent{ClientID: "ct" + stableID(ctx.ID, "SL", fmt.Sprint(ctx.Version))[:28], Price: price, Requested: requested, Quantity: qty, Old: old, Stage: "PLANNED", UnconfirmedSince: &now}
		if err = x.saveStopIntent(ctx, pending); err != nil {
			return err
		}
	}
	if pending.UnconfirmedSince == nil {
		// Intents written before this field existed start their clock now.
		now := stopClock()
		pending.UnconfirmedSince = &now
		if err = x.saveStopIntent(ctx, pending); err != nil {
			return err
		}
	}
	if pending.Stage == "REJECTED" {
		if pending.ErrorCode == -4130 {
			return x.unprotectedFallback(traceID, signalID, ctx, pending, x.adoptExistingStop(traceID, signalID, ctx, pending, tick))
		}
		// -4509 proves this request did not create an order. A new, freshly observed
		// position may still need protection after the close/submission race.
		if pending.ErrorCode == -4509 {
			pos, err := x.freshPosition(ctx.Symbol, ctx.Direction)
			if err != nil {
				return fmt.Errorf("%w: rejected stop position check: %v", ErrProtectionUnconfirmed, err)
			}
			if pos != nil && pos.qty > 0 {
				if err = x.persistContext(ctx, map[string]interface{}{"stop_intent_json": ""}); err != nil {
					return err
				}
				return fmt.Errorf("%w: rejected request resolved; fresh position requires a new protection pass", ErrProtectionUnconfirmed)
			}
		}
		return x.unprotectedFallback(traceID, signalID, ctx, pending, fmt.Errorf("stop explicitly rejected; awaiting flat-position cleanup or reviewed target"))
	}
	if pending.Stage == "SUBMITTING" || pending.Stage == "UNKNOWN" {
		outcome, err := x.resolveStopOutcome(ctx, pending)
		if err != nil {
			return x.unprotectedFallback(traceID, signalID, ctx, pending, err)
		}
		if outcome == stopTerminal {
			// Do not immediately resubmit against the pre-query position snapshot.
			if err := x.persistContext(ctx, map[string]interface{}{"stop_intent_json": "", "stop_order_id": ""}); err != nil {
				return err
			}
			return fmt.Errorf("%w: previous stop terminated; recheck current position before protecting", ErrProtectionUnconfirmed)
		}
		// stopAbsent: the request never landed. Re-protect with a NEW identity,
		// but only when no stop is visible on this side: any visible stop is
		// either protection already or an ownership question for review.
		if len(candidates) > 0 {
			return fmt.Errorf("%w: stop %s never landed but %d other stop order(s) exist on this side; manual ownership check required", ErrProtectionUnconfirmed, pending.ClientID, len(candidates))
		}
		pos, perr := x.freshPosition(ctx.Symbol, ctx.Direction)
		if perr != nil {
			return x.unprotectedFallback(traceID, signalID, ctx, pending, fmt.Errorf("%w: absent stop position check: %v", ErrProtectionUnconfirmed, perr))
		}
		if pos == nil || pos.qty <= 0 {
			if err := x.persistContext(ctx, map[string]interface{}{"stop_intent_json": "", "stop_order_id": ""}); err != nil {
				return err
			}
			return fmt.Errorf("%w: EXIT_OBSERVED; unlanded stop %s discarded for a flat position", ErrProtectionUnconfirmed, pending.ClientID)
		}
		lost := pending.ClientID
		pending.PriorClientIDs = append(pending.PriorClientIDs, lost)
		pending.ClientID = "ct" + stableID(ctx.ID, "SL-retry", lost)[:28]
		pending.Quantity = pos.qty
		pending.Old = nil
		pending.Stage = "PLANNED"
		pending.ErrorCode, pending.ErrorStage, pending.ErrorAt, pending.SubmittedAt = 0, "", nil, nil
		pending.NotFoundCount, pending.FirstNotFoundAt = 0, nil
		if err = x.saveStopIntent(ctx, pending); err != nil {
			return err
		}
		x.events.Warn(traceID, signalID, "", EvExecutionError, fmt.Sprintf("stop %s never reached the exchange (no open order, history not found beyond %s); resubmitting %s @ %.8g with a new identity", lost, stopNotFoundGrace, ctx.Symbol, pending.Price), map[string]interface{}{"context_id": ctx.ID, "lost_client_id": lost, "client_id": pending.ClientID, "quantity": pending.Quantity})
	}

	if pending.Old != nil && pending.Stage == "PLANNED" {
		// Re-query above covers a cancellation whose acknowledgement was lost.
		present := false
		for _, o := range candidates {
			if o.OrderID == pending.Old.OrderID {
				present = true
			}
		}
		if present {
			if err = managed.CancelStopOrder(ctx.Symbol, *pending.Old); err != nil {
				return err
			}
		}
	}
	pending.Stage = "SUBMITTING"
	pending.ErrorCode, pending.ErrorStage, pending.ErrorAt = 0, "", nil
	submittedAt := stopClock()
	pending.SubmittedAt = &submittedAt
	pending.NotFoundCount, pending.FirstNotFoundAt = 0, nil
	if err = x.saveStopIntent(ctx, pending); err != nil {
		return err
	}
	err = managed.SetManagedStopLoss(ctx.Symbol, positionSideOf(ctx.Direction), pending.Quantity, pending.Price, pending.ClientID)
	if err != nil {
		pending.ErrorStage = "submit_stop"
		now := time.Now().UTC()
		pending.ErrorAt = &now
		var rejection *types.ManagedRejection
		if errors.As(err, &rejection) {
			pending.ErrorCode = rejection.Code
		}
		if errors.Is(err, types.ErrManagedOrderRetryable) {
			// Not sent, or refused before reaching the engine (rate limit,
			// clock skew, credentials): no order exists. Keep the same intent
			// PLANNED so the next pass retries; never mark it UNKNOWN.
			pending.Stage = "PLANNED"
			pending.ErrorStage = "submit_stop_not_executed"
			pending.SubmittedAt = nil
			if perr := x.saveStopIntent(ctx, pending); perr != nil {
				return perr
			}
			x.events.Warn(traceID, signalID, "", EvExecutionError, fmt.Sprintf("stop submission not executed for %s; will retry: %v", ctx.Symbol, err), map[string]interface{}{"context_id": ctx.ID, "client_id": pending.ClientID, "error_code": pending.ErrorCode, "stage": pending.Stage, "error_stage": pending.ErrorStage, "occurred_at": pending.ErrorAt})
			return x.unprotectedFallback(traceID, signalID, ctx, pending, fmt.Errorf("%w: stop not executed, retry pending: %v", ErrProtectionUnconfirmed, err))
		}
		rejected := errors.Is(err, types.ErrManagedOrderRejected)
		pending.Stage = "UNKNOWN"
		if rejected {
			pending.Stage = "REJECTED"
		}
		if perr := x.saveStopIntent(ctx, pending); perr != nil {
			return perr
		}
		x.events.Warn(traceID, signalID, "", EvExecutionError, "stop submission requires reconciliation", map[string]interface{}{"context_id": ctx.ID, "client_id": pending.ClientID, "error_code": pending.ErrorCode, "stage": pending.Stage, "error_stage": pending.ErrorStage, "occurred_at": pending.ErrorAt})
		if rejected && pending.ErrorCode == -4130 {
			// A closePosition stop already exists on this side (e.g. the
			// earlier identity landed late). Adopt it if it is ours.
			return x.unprotectedFallback(traceID, signalID, ctx, pending, x.adoptExistingStop(traceID, signalID, ctx, pending, tick))
		}
		restore := false
		if rejected {
			pos, checkErr := x.freshPosition(ctx.Symbol, ctx.Direction)
			if checkErr != nil {
				return fmt.Errorf("%w: rejected stop position check: %v", ErrProtectionUnconfirmed, checkErr)
			}
			restore = pos != nil && pos.qty > 0
			if !restore {
				return fmt.Errorf("%w: EXIT_OBSERVED after rejected protection: %w", ErrProtectionUnconfirmed, err)
			}
		}
		if rejected && restore && pending.Old != nil {
			// Restore with its own durable identity, never retry the rejected request.
			rejected := err
			pending.Price = pending.Old.StopPrice
			pending.Requested = pending.Old.StopPrice
			pending.ClientID = "ct" + stableID(pending.ClientID, "restore")[:28]
			pending.Old = nil
			pending.Stage = "PLANNED"
			if err = x.saveStopIntent(ctx, pending); err != nil {
				return err
			}
			if err = x.setStopProtection(traceID, signalID, ctx, qty, requested); err != nil {
				return fmt.Errorf("stop rejected (%v), restoration: %w", rejected, err)
			}
			return rejected
		}
		if !errors.Is(err, types.ErrManagedOrderRejected) {
			return x.unprotectedFallback(traceID, signalID, ctx, pending, fmt.Errorf("%w: %v", ErrProtectionUnconfirmed, err))
		}
		return err
	}
	// Keep the stable identity until a query confirms the actual exchange order.
	confirmed, lookupErr := x.ex.GetOpenOrders(ctx.Symbol)
	if lookupErr != nil {
		return fmt.Errorf("%w: stop accepted; confirmation pending: %v", ErrProtectionUnconfirmed, lookupErr)
	}
	orderID := ""
	for _, o := range confirmed {
		if o.ClientID == pending.ClientID && isStopOrder(o) && stopMatchesSide(o, ctx.Direction) && sameStep(o.StopPrice, pending.Price, tick) && (o.ClosePosition || sameStep(o.Quantity, pending.Quantity, step)) {
			orderID = o.OrderID
			break
		}
	}
	if orderID == "" {
		return fmt.Errorf("%w: stop accepted; identity not yet visible, reconciliation required", ErrProtectionUnconfirmed)
	}
	if err = x.persistContext(ctx, map[string]interface{}{"stop_loss_price": pending.Price, "requested_stop_loss": pending.Requested, "stop_order_id": orderID, "stop_intent_json": ""}); err != nil {
		return err
	}
	if !sameStep(originalQty, pending.Quantity, step) {
		return fmt.Errorf("%w: previous stop intent completed; re-evaluate the current position quantity", ErrProtectionUnconfirmed)
	}

	x.events.Success(traceID, signalID, "", EvSLSet, fmt.Sprintf("SL protected %s @ %.8g (qty %.8g)", ctx.Symbol, pending.Price, qty), 0, map[string]interface{}{"context_id": ctx.ID, "order_id": orderID, "client_id": pending.ClientID, "requested_price": pending.Requested, "effective_price": pending.Price})
	return nil
}

// adoptExistingStop handles -4130 (a closePosition stop already exists in this
// direction). A fresh open-order read must show exactly one stop on this side
// whose identity is ours (current/prior client ID, recorded order ID, or the
// order this intent replaces); it then becomes the recorded protection. A
// foreign or ambiguous stop is left untouched for manual review.
func (x *Executor) adoptExistingStop(traceID, signalID string, ctx *store.CopyTradeContext, pending *stopIntent, tick float64) error {
	orders, err := x.ex.GetOpenOrders(ctx.Symbol)
	if err != nil {
		return fmt.Errorf("%w: -4130 adoption lookup failed: %v", ErrProtectionUnconfirmed, err)
	}
	var found []types.OpenOrder
	for _, o := range orders {
		if (o.Symbol == "" || o.Symbol == ctx.Symbol) && isStopOrder(o) && stopMatchesSide(o, ctx.Direction) {
			found = append(found, o)
		}
	}
	if len(found) == 0 && pending.Stage == "REJECTED" {
		// The blocking stop is gone (triggered or cancelled). The rejected
		// request created nothing; re-plan under a new identity so the next
		// pass re-protects after its own fresh position/order checks.
		lost := pending.ClientID
		pending.PriorClientIDs = append(pending.PriorClientIDs, lost)
		pending.ClientID = "ct" + stableID(ctx.ID, "SL-4130", lost)[:28]
		pending.Old = nil
		pending.Stage = "PLANNED"
		pending.SubmittedAt = nil
		if err = x.saveStopIntent(ctx, pending); err != nil {
			return err
		}
		return fmt.Errorf("%w: -4130 blocking stop no longer visible; protection re-planned", ErrProtectionUnconfirmed)
	}
	if len(found) != 1 {
		return fmt.Errorf("%w: -4130 reported an existing closePosition stop but %d stop(s) are visible on this side; manual review required", ErrProtectionUnconfirmed, len(found))
	}
	o := found[0]
	ours := pending.ownsClientID(o.ClientID) || (ctx.StopOrderID != "" && o.OrderID == ctx.StopOrderID) || (pending.Old != nil && o.OrderID == pending.Old.OrderID)
	if !ours {
		x.events.Warn(traceID, signalID, "", EvExecutionError, fmt.Sprintf("%s %s: an unattributed closePosition stop (%s @ %.8g) blocks our protection (-4130); manual ownership check required", ctx.Symbol, ctx.Direction, o.OrderID, o.StopPrice), map[string]interface{}{"context_id": ctx.ID, "order_id": o.OrderID, "client_id": o.ClientID})
		return fmt.Errorf("%w: unattributed closePosition stop %s blocks protection (-4130)", ErrProtectionUnconfirmed, o.OrderID)
	}
	if err = x.persistContext(ctx, map[string]interface{}{"stop_loss_price": o.StopPrice, "requested_stop_loss": pending.Requested, "stop_order_id": o.OrderID, "stop_intent_json": ""}); err != nil {
		return err
	}
	x.events.Info(traceID, signalID, "", EvSLSet, fmt.Sprintf("SL adopted existing exchange stop for %s @ %.8g after -4130", ctx.Symbol, o.StopPrice), map[string]interface{}{"context_id": ctx.ID, "order_id": o.OrderID, "client_id": o.ClientID, "requested_price": pending.Requested})
	if !sameStep(o.StopPrice, pending.Price, tick) && !tighterStop(ctx.Direction, o.StopPrice, pending.Price) {
		return fmt.Errorf("%w: adopted stop @ %.8g differs from target %.8g; re-evaluate", ErrProtectionUnconfirmed, o.StopPrice, pending.Price)
	}
	return nil
}

// unprotectedFallback passes cause through unchanged unless the position has
// had no confirmed stop for stopUnprotectedAlertAfter AND a fresh open-order
// read shows no stop at all on this side. Then it raises an Error event and
// closes the position through the emergency path. A pending close is never
// duplicated (this also prevents re-entry from the close reconciliation).
func (x *Executor) unprotectedFallback(traceID, signalID string, ctx *store.CopyTradeContext, pending *stopIntent, cause error) error {
	if cause == nil || pending == nil || pending.UnconfirmedSince == nil || ctx.ClosePendingJSON != "" {
		return cause
	}
	// The emergency close paths may re-enter stop protection; never nest.
	if _, active := unprotectedFallbackActive.LoadOrStore(ctx.ID, true); active {
		return cause
	}
	defer unprotectedFallbackActive.Delete(ctx.ID)
	now := stopClock()
	age := now.Sub(*pending.UnconfirmedSince)
	if age < stopUnprotectedAlertAfter {
		return cause
	}
	orders, err := x.ex.GetOpenOrders(ctx.Symbol)
	if err != nil {
		return cause
	}
	for _, o := range orders {
		if (o.Symbol == "" || o.Symbol == ctx.Symbol) && isStopOrder(o) && stopMatchesSide(o, ctx.Direction) {
			return cause // some stop protects this side; ownership is a review matter
		}
	}
	pos, err := x.freshPosition(ctx.Symbol, ctx.Direction)
	if err != nil || pos == nil || pos.qty <= 0 {
		return cause
	}
	if ctx.StopIntentJSON != "" && pending.AlertedAt == nil {
		pending.AlertedAt = &now
		if err := x.saveStopIntent(ctx, pending); err != nil {
			return err
		}
	}
	x.events.Error(traceID, signalID, "", EvEmergencyClose, fmt.Sprintf("持仓无确认止损: %s %s qty %.8g has had no confirmed stop-loss for %s (last: %v); emergency closing", ctx.Symbol, ctx.Direction, pos.qty, age.Truncate(time.Second), cause), map[string]interface{}{"context_id": ctx.ID, "client_id": pending.ClientID, "stage": pending.Stage, "error_code": pending.ErrorCode, "unconfirmed_since": pending.UnconfirmedSince})
	var closeErr error
	if _, managed := x.ex.(types.ManagedOrderTrader); managed {
		// Same order as the entry-protection failure path: stop adding
		// exposure first (best effort), then close the whole position.
		if ctx.EntryWorking || !ctx.EntryDisabled {
			_ = x.closeEntryEligibility(traceID, ctx, "PROTECTION_UNCONFIRMED")
		}
		_, closeErr = x.executeManagedClose(traceID, "emergency-sl-"+ctx.ID, ctx, 100)
	} else {
		closeErr = x.emergencyClose(traceID, signalID, ctx.Symbol, ctx.Direction)
	}
	if closeErr != nil {
		return fmt.Errorf("%w: UNPROTECTED position, emergency close failed: %v (stop: %v)", ErrProtectionUnconfirmed, closeErr, cause)
	}
	return fmt.Errorf("%w: UNPROTECTED position emergency close submitted (stop: %v)", ErrProtectionUnconfirmed, cause)
}
