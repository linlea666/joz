package copytrader

import (
	"encoding/json"
	"fmt"
	"nofx/store"
	"nofx/trader/types"
)

// finishTradeOrders is used only AFTER a fresh flat-position confirmation.
// It settles exact ledger identities so terminal trades can release ownership;
// stale NEW rows must not permanently lock the account or hide live orders.
func (x *Executor) finishTradeOrders(ctx *store.CopyTradeContext) error {
	pos, err := x.freshPosition(ctx.Symbol, ctx.Direction)
	if err != nil {
		return err
	}
	if pos != nil && pos.qty > 0 {
		return fmt.Errorf("position remains open during terminal cleanup")
	}
	if _, ok := x.ex.(types.ManagedOrderTrader); ok {
		orders, err := x.st.CopyTrade().GetOrdersForContext(x.traderID, ctx.ID)
		if err != nil {
			return err
		}
		for _, o := range orders {
			if orderTerminal(o.Status) {
				continue
			}
			if o.Status != "PLANNED" {
				if err = x.queryManaged(o); err != nil {
					return err
				}
			}
			if !orderTerminal(o.Status) {
				if err = x.cancelManaged(o); err != nil {
					return err
				}
			}
		}
	}
	precise, ok := x.ex.(types.StopOrderTrader)
	if !ok {
		x.cancelTradeOrdersQuiet(ctx.Symbol, ctx.Direction)
		return nil
	}
	orders, err := x.ex.GetOpenOrders(ctx.Symbol)
	if err != nil {
		return err
	}
	owned := map[string]bool{}
	for _, tp := range readTPPlan(ctx) {
		if tp.OrderID != "" {
			owned[tp.OrderID] = true
		}
	}
	pending := stopIntent{}
	if ctx.StopIntentJSON != "" {
		if err = json.Unmarshal([]byte(ctx.StopIntentJSON), &pending); err != nil {
			return err
		}
	}
	if pending.Stage == "SUBMITTING" || pending.Stage == "UNKNOWN" {
		found := false
		for _, o := range orders {
			if o.ClientID == pending.ClientID {
				found = true
			}
		}
		if !found {
			return fmt.Errorf("unresolved stop submission retained after position closed; verify exchange history")
		}
	}

	for _, o := range orders {
		if !stopMatchesSide(o, ctx.Direction) {
			continue
		}
		if isStopOrder(o) {
			if o.OrderID != ctx.StopOrderID && (pending.ClientID == "" || o.ClientID != pending.ClientID) {
				return fmt.Errorf("unattributed terminal stop retained for manual review")
			}
			if err = precise.CancelStopOrder(ctx.Symbol, o); err != nil {
				return err
			}
		} else if owned[o.OrderID] && x.gridEx != nil {
			if err = x.gridEx.CancelOrder(ctx.Symbol, o.OrderID); err != nil {
				return err
			}
		} else {
			return fmt.Errorf("unattributed terminal order retained for manual review")
		}
	}
	remaining, err := x.ex.GetOpenOrders(ctx.Symbol)
	if err != nil {
		return err
	}
	for _, o := range remaining {
		if stopMatchesSide(o, ctx.Direction) {
			return fmt.Errorf("terminal order cancellation not yet confirmed")
		}
	}
	return x.persistContext(ctx, map[string]interface{}{"stop_intent_json": "", "tp_update_intent_json": "", "stop_order_id": "", "entry_working": false})
}
