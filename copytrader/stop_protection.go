package copytrader

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"

	"nofx/store"
	"nofx/trader/types"
)

var ErrProtectionUnconfirmed = errors.New("protection observation or submission uncertain")

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
		pending = &stopIntent{ClientID: "ct" + stableID(ctx.ID, "SL", fmt.Sprint(ctx.Version))[:28], Price: price, Requested: requested, Quantity: qty, Old: old, Stage: "PLANNED"}
		if err = x.saveStopIntent(ctx, pending); err != nil {
			return err
		}
	}
	if pending.Stage == "REJECTED" {
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
		return fmt.Errorf("stop explicitly rejected; awaiting flat-position cleanup or reviewed target")
	}
	if pending.Stage == "SUBMITTING" || pending.Stage == "UNKNOWN" {
		if err := x.resolveTerminalStop(ctx, pending); err != nil {
			return err
		}
		// Do not immediately resubmit against the pre-query position snapshot.
		if err := x.persistContext(ctx, map[string]interface{}{"stop_intent_json": "", "stop_order_id": ""}); err != nil {
			return err
		}
		return fmt.Errorf("%w: previous stop terminated; recheck current position before protecting", ErrProtectionUnconfirmed)
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
		rejected := errors.Is(err, types.ErrManagedOrderRejected)
		pending.Stage = "UNKNOWN"
		if rejected {
			pending.Stage = "REJECTED"
		}
		if perr := x.saveStopIntent(ctx, pending); perr != nil {
			return perr
		}
		x.events.Warn(traceID, signalID, "", EvExecutionError, "stop submission requires reconciliation", map[string]interface{}{"context_id": ctx.ID, "client_id": pending.ClientID, "error_code": pending.ErrorCode, "stage": pending.Stage, "error_stage": pending.ErrorStage, "occurred_at": pending.ErrorAt})
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
			return fmt.Errorf("%w: %v", ErrProtectionUnconfirmed, err)
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
