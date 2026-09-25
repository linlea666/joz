package copytrader

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"

	"nofx/store"
	"nofx/trader/types"
)

func readTPPlan(ctx *store.CopyTradeContext) []TPPlanEntry {
	var plan []TPPlanEntry
	_ = json.Unmarshal([]byte(ctx.TPPlanJSON), &plan)
	for i := range plan {
		if plan[i].Ordinal == 0 {
			plan[i].Ordinal = i + 1
		}
	}
	return plan
}

func (x *Executor) saveTPPlan(ctx *store.CopyTradeContext, plan []TPPlanEntry) error {
	b, _ := json.Marshal(plan)
	return x.persistContext(ctx, map[string]interface{}{"tp_plan_json": string(b)})
}

// refreshTPProgress uses order fills, never a position-size change, as TP
// evidence. An anonymous legacy trigger order remains unknown. FILLED flags
// from older records are preserved, but an old tp_hit_count is not evidence.
func (x *Executor) refreshTPProgress(traceID string, ctx *store.CopyTradeContext) bool {
	plan := readTPPlan(ctx)
	ledger := map[string]*store.CopyTradeOrder{}
	if _, ok := x.ex.(types.ManagedOrderTrader); ok {
		if rows, err := x.st.CopyTrade().GetOrdersForContext(x.traderID, ctx.ID); err == nil {
			for _, row := range rows {
				ledger[row.ClientID] = row
			}
		}
	}
	changed, advanced, hits := false, false, 0
	for i := range plan {
		tp := &plan[i]
		if tp.OrderID != "" && tp.OrderKind != "ALGO" && !tp.Filled {
			var status map[string]interface{}
			var err error
			if row := ledger[tp.ClientID]; row != nil {
				err = x.queryManaged(row)
				status = map[string]interface{}{"status": row.Status, "executedQty": row.ExecutedQty}
			} else {
				status, err = x.ex.GetOrderStatus(ctx.Symbol, tp.OrderID)
			}
			if err != nil {
				continue
			}
			st, _ := status["status"].(string)
			q := executedQtyOf(status)
			if q > tp.FilledQuantity {
				if tp.DesiredQuantity != nil {
					v := math.Max(0, *tp.DesiredQuantity-(math.Min(q, tp.Quantity)-tp.FilledQuantity))
					tp.DesiredQuantity = &v
				}
				tp.FilledQuantity = math.Min(q, tp.Quantity)
				advanced, changed = true, true
			}
			if st == "FILLED" && !tp.Filled {
				tp.Filled = true
				if tp.DesiredQuantity != nil {
					v := 0.0
					tp.DesiredQuantity = &v
				}
				tp.FilledQuantity = tp.Quantity
				advanced, changed = true, true
			}
			if st != "" && tp.Status != st {
				tp.Status, changed = st, true
			}
		}
		if tp.Filled || tp.FilledQuantity > 0 || tp.PriorFilledQuantity > 0 {
			hits++
		}
	}
	if changed {
		b, _ := json.Marshal(plan)
		x.updateContext(ctx, map[string]interface{}{"tp_plan_json": string(b), "tp_hit_count": hits})
	}
	if advanced {
		x.events.Success(traceID, "", "", EvReconcile,
			fmt.Sprintf("%s confirmed take-profit order fill (%d levels with fills)", ctx.Symbol, hits), 0, nil)
	}
	return advanced
}

func confirmedTPLevel(ctx *store.CopyTradeContext, level int) bool {
	for _, tp := range readTPPlan(ctx) {
		if tp.Ordinal == level && (tp.Filled || tp.FilledQuantity > 0 || tp.PriorFilledQuantity > 0) {
			return true
		}
	}
	return false
}

func stopMatchesSide(o types.OpenOrder, direction string) bool {
	if o.PositionSide != "" && !strings.EqualFold(o.PositionSide, "BOTH") && !strings.EqualFold(o.PositionSide, "NET") {
		return strings.EqualFold(o.PositionSide, direction)
	}
	if o.Side != "" {
		want := "SELL"
		if strings.EqualFold(direction, "SHORT") {
			want = "BUY"
		}
		return strings.EqualFold(o.Side, want)
	}
	// Missing side metadata cannot establish protection of this position.
	return false
}

func isStopOrder(o types.OpenOrder) bool {
	t := strings.ToUpper(o.Type)
	return !strings.Contains(t, "TAKE_PROFIT") && (strings.Contains(t, "STOP") || o.StopPrice > 0)
}

func tighterStop(direction string, a, b float64) bool {
	if strings.EqualFold(direction, "SHORT") {
		return a < b
	}
	return a > b
}

// ensureStopProtection validates the side AND remaining size. A zero-sized
// Binance closePosition stop covers the complete side. Never loosen an
// exchange stop that has already been raised beyond our recorded price.
func (x *Executor) ensureStopProtection(traceID, signalID string, ctx *store.CopyTradeContext, qty float64) error {
	if ctx.StopLossPrice <= 0 || qty <= 0 {
		return nil
	}
	orders, err := x.ex.GetOpenOrders(ctx.Symbol)
	if err != nil {
		return err
	}
	price := ctx.StopLossPrice
	found, valid := false, false
	for _, o := range orders {
		if (o.Symbol != "" && o.Symbol != ctx.Symbol) || !isStopOrder(o) || !stopMatchesSide(o, ctx.Direction) {
			continue
		}
		found = true
		if o.StopPrice > 0 && tighterStop(ctx.Direction, o.StopPrice, price) {
			price = o.StopPrice
		}
		priceOK := o.StopPrice > 0 && (math.Abs(o.StopPrice-ctx.StopLossPrice) <= math.Abs(ctx.StopLossPrice)*1e-8 || tighterStop(ctx.Direction, o.StopPrice, ctx.StopLossPrice))
		sizeOK := o.ClosePosition || math.Abs(o.Quantity-qty) <= math.Max(qty*1e-6, 1e-12)
		valid = valid || (priceOK && sizeOK)
	}
	if price != ctx.StopLossPrice {
		x.updateContext(ctx, map[string]interface{}{"stop_loss_price": price})
	}
	if valid {
		return nil
	}
	if found {
		if err := x.cancelStopLossOrders(ctx.Symbol, ctx.Direction); err != nil {
			return err
		}
	}
	if err := x.ex.SetStopLoss(ctx.Symbol, positionSideOf(ctx.Direction), qty, price); err != nil {
		return err
	}
	x.events.Success(traceID, signalID, "", EvSLSet,
		fmt.Sprintf("SL guard restored %s %s stop @ %.8g (qty %.8g)", ctx.Symbol, ctx.Direction, price, qty), 0, nil)
	return nil
}

// restoreRemainingProtections checks the remaining SL/TP set after every
// fill or manual reduction. Completed levels remain immutable fill evidence.
func (x *Executor) restoreRemainingProtections(traceID, signalID string, ctx *store.CopyTradeContext, qty float64) error {
	if err := x.ensureStopProtection(traceID, signalID, ctx, qty); err != nil {
		return err
	}
	if qty <= 0 || x.gridEx == nil {
		return nil
	}
	x.refreshTPProgress(traceID, ctx)
	plan := readTPPlan(ctx)
	var rules *types.ManagedMarketRules
	managed, hasManaged := x.ex.(types.ManagedOrderTrader)
	if hasManaged {
		var err error
		rules, err = managed.MarketRules(ctx.Symbol)
		if err != nil {
			return err
		}
	}
	step := x.detectStepSize(ctx.Symbol, qty)
	minQty := 0.0
	if rules != nil {
		step, minQty = rules.QuantityStep, rules.MinQuantity
	}
	if len(plan) == 0 && ctx.TPRecipeJSON != "" {
		var recipe TPRecipe
		if err := json.Unmarshal([]byte(ctx.TPRecipeJSON), &recipe); err != nil {
			return err
		}
		amounts, err := SplitTPQuantities(qty, recipe.Ratios, step, minQty)
		if err != nil {
			return err
		}
		for i, p := range recipe.Prices {
			if i < len(amounts) && amounts[i] > 0 {
				plan = append(plan, TPPlanEntry{Ordinal: recipeOrdinal(recipe, i), Price: p, Quantity: amounts[i], Status: "PLANNED"})
			}
		}
		if err := x.saveTPPlan(ctx, plan); err != nil {
			return err
		}
	}
	total := 0.0
	for _, tp := range plan {
		if !tp.Filled {
			total += math.Max(0, tp.Quantity-tp.FilledQuantity)
		}
	}
	if total <= 0 {
		return nil
	}
	if ctx.TPPositionQuantity != qty {
		scale := math.Min(1, qty/total)
		if ctx.TPPositionQuantity > 0 && qty > ctx.TPPositionQuantity+1e-10 {
			scale = qty / ctx.TPPositionQuantity
		}
		for i := range plan {
			if !plan[i].Filled {
				want := math.Max(0, plan[i].Quantity-plan[i].FilledQuantity) * scale
				if step > 0 && want > 0 {
					want, _ = types.DecimalFloor(types.SanitizeBaseQuantity(want), step)
				}
				plan[i].DesiredQuantity = &want
			}
		}
		b, _ := json.Marshal(plan)
		if err := x.persistContext(ctx, map[string]interface{}{"tp_plan_json": string(b), "tp_position_quantity": qty}); err != nil {
			return err
		}
	}
	for i := range plan {
		tp := &plan[i]
		if tp.Filled {
			continue
		}
		remaining := math.Max(0, tp.Quantity-tp.FilledQuantity)
		want := remaining
		if tp.DesiredQuantity != nil {
			want = *tp.DesiredQuantity
		}
		if step > 0 && want > 0 {
			want, _ = types.DecimalFloor(types.SanitizeBaseQuantity(want), step)
		}
		if tp.OrderID == "" && tp.ClientID == "" && tp.Status != "PLANNED" && tp.Status != "PLACEMENT_FAILED" {
			continue
		} // anonymous legacy trigger: not safe to guess
		if tp.OrderID != "" {
			status, err := x.ex.GetOrderStatus(ctx.Symbol, tp.OrderID)
			if err != nil {
				return fmt.Errorf("TP%d status unknown: %w", tp.Ordinal, err)
			}
			st, _ := status["status"].(string)
			if st == "FILLED" || executedQtyOf(status) > tp.FilledQuantity+1e-12 {
				x.refreshTPProgress(traceID, ctx)
				return fmt.Errorf("TP fill changed during protection repair; refresh position")
			}
			switch st {
			case "NEW", "PARTIALLY_FILLED":
				if math.Abs(want-remaining) <= math.Max(1e-12, remaining*1e-8) {
					continue
				}
				if err := x.gridEx.CancelOrder(ctx.Symbol, tp.OrderID); err != nil {
					return err
				}
				after, err := x.ex.GetOrderStatus(ctx.Symbol, tp.OrderID)
				if err != nil {
					return err
				}
				ast, _ := after["status"].(string)
				if executedQtyOf(after) > tp.FilledQuantity+1e-12 || ast == "FILLED" {
					x.refreshTPProgress(traceID, ctx)
					return fmt.Errorf("TP filled during cancel; refresh position")
				}
				if !orderTerminal(ast) {
					return fmt.Errorf("TP cancel not confirmed: %s", ast)
				}
			case "CANCELED", "CANCELLED", "EXPIRED", "EXPIRED_IN_MATCH", "REJECTED":
			default:
				return fmt.Errorf("TP%d status unknown: %s", tp.Ordinal, st)
			}
			tp.PriorFilledQuantity += tp.FilledQuantity
			tp.FilledQuantity = 0
			tp.OrderID = ""
			tp.ClientID = ""
			tp.Generation++
			tp.Status = "PLANNED"
			tp.Quantity = want
			if err := x.saveTPPlan(ctx, plan); err != nil {
				return err
			}
		}
		if want <= 0 || want < minQty {
			tp.Status = "BELOW_MINIMUM"
			if err := x.saveTPPlan(ctx, plan); err != nil {
				return err
			}
			continue
		}
		if hasManaged {
			role := fmt.Sprintf("TP_%d_%d", tp.Ordinal, tp.Generation)
			row := x.newManagedOrder(ctx, signalID, role, "LIMIT", tp.Price, want)
			if tp.ClientID != "" {
				rows, err := x.st.CopyTrade().GetOrdersForContext(x.traderID, ctx.ID)
				if err != nil {
					return err
				}
				var found bool
				for _, r := range rows {
					if r.ClientID == tp.ClientID {
						row = r
						found = true
						break
					}
				}
				if !found {
					return fmt.Errorf("TP order ledger missing")
				}
			} else {
				// Store the intent and the TP link atomically to cover a crash between them.
				tp.ClientID = row.ClientID
				tp.Quantity = want
				b, _ := json.Marshal(plan)
				if err := x.st.CopyTrade().CreateProtectionOrder(ctx, row, string(b)); err != nil {
					return err
				}
				ctx.TPPlanJSON = string(b)
				ctx.Version++
			}
			if err := x.submitManaged(row, rules, true); err != nil {
				return err
			}
			tp.OrderID, tp.Status = row.OrderID, row.Status
			tp.FilledQuantity = row.ExecutedQty
			tp.Filled = row.Status == "FILLED"
		} else {
			if tp.Status == "SUBMITTING" {
				return fmt.Errorf("TP outcome unknown; venue lacks client-ID recovery")
			}
			tp.Status = "SUBMITTING"
			tp.Quantity = want
			if err := x.saveTPPlan(ctx, plan); err != nil {
				return err
			}
			res, err := x.gridEx.PlaceLimitOrder(&types.LimitOrderRequest{Symbol: ctx.Symbol, Side: closingSide(ctx.Direction), PositionSide: positionSideOf(ctx.Direction), Price: tp.Price, Quantity: want, ReduceOnly: true})
			if err != nil {
				return err
			}
			tp.OrderID, tp.Status = res.OrderID, "NEW"
		}
		if err := x.saveTPPlan(ctx, plan); err != nil {
			return err
		}
	}
	return x.persistContext(ctx, map[string]interface{}{"tp_position_quantity": qty})
}

func (x *Executor) freshPosition(symbol, direction string) (*positionInfo, error) {
	if reader, ok := x.ex.(types.FreshPositionReader); ok {
		positions, err := reader.GetFreshPositions()
		if err != nil {
			return nil, err
		}
		return positionFromRows(positions, symbol, direction), nil
	}
	if c, ok := x.ex.(interface{ InvalidatePositionCache() }); ok {
		c.InvalidatePositionCache()
	}
	return x.findPosition(symbol, direction)
}
