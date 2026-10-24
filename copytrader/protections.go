package copytrader

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

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
	advanced, err := x.refreshTPProgressChecked(traceID, ctx)
	if err != nil {
		x.events.Warn(traceID, "", "", EvExecutionError, fmt.Sprintf("TP progress unavailable: %v", err), nil)
	}
	return advanced
}

// Callers that will replace TP orders or add exposure must stop when fill
// evidence cannot be read or durably recorded. Exits may still proceed.
func (x *Executor) refreshTPProgressChecked(traceID string, ctx *store.CopyTradeContext) (bool, error) {
	plan := readTPPlan(ctx)
	ledger := map[string]*store.CopyTradeOrder{}
	if _, ok := x.ex.(types.ManagedOrderTrader); ok {
		rows, err := x.st.CopyTrade().GetOrdersForContext(x.traderID, ctx.ID)
		if err != nil {
			return false, err
		}
		for _, row := range rows {
			ledger[row.ClientID] = row
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
				return false, fmt.Errorf("TP%d fill lookup: %w", tp.Ordinal, err)
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
		if tp.Filled || tp.FilledQuantity > 0 || tp.PriorFilledQuantity > 0 || tp.ReplacementFilled > 0 {
			hits++
		}
	}
	if changed {
		b, _ := json.Marshal(plan)
		if err := x.persistContext(ctx, map[string]interface{}{"tp_plan_json": string(b), "tp_hit_count": hits}); err != nil {
			return false, err
		}
	}
	if advanced {
		x.events.Success(traceID, "", "", EvReconcile,
			fmt.Sprintf("%s confirmed take-profit order fill (%d levels with fills)", ctx.Symbol, hits), 0, nil)
	}
	return advanced, nil
}

func confirmedTPLevel(ctx *store.CopyTradeContext, level int) bool {
	for _, tp := range readTPPlan(ctx) {
		if tp.Ordinal == level && (tp.Filled || tp.FilledQuantity > 0 || tp.PriorFilledQuantity > 0 || tp.ReplacementFilled > 0) {
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
	requested := ctx.RequestedStopLoss
	if ctx.StopIntentJSON != "" {
		var intent stopIntent
		if err := json.Unmarshal([]byte(ctx.StopIntentJSON), &intent); err != nil {
			return err
		}
		requested = intent.Requested
	}
	if requested <= 0 {
		requested = ctx.StopLossPrice
	}
	return x.setStopProtection(traceID, signalID, ctx, qty, requested)
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
	if _, err := x.refreshTPProgressChecked(traceID, ctx); err != nil {
		return err
	}
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
		if !tp.Filled && tp.ReplacedBy == "" {
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
			if !plan[i].Filled && plan[i].ReplacedBy == "" {
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
	// The mark/last price is read at most once per pass and only when the
	// venue publishes PERCENT_PRICE multipliers. Failures never block a TP.
	reference, referenceRead := 0.0, false
	tpPriceAllowed := func(price float64) bool {
		if rules == nil {
			return true
		}
		if !referenceRead && (rules.PercentPriceUp > 0 || rules.PercentPriceDown > 0) {
			referenceRead = true
			if p, err := x.ex.GetMarketPrice(ctx.Symbol); err == nil && finite(p) && p > 0 {
				reference = p
			}
		}
		return rules.LimitPriceAllowed(closingSide(ctx.Direction), price, reference)
	}
	for i := range plan {
		tp := &plan[i]
		if tp.Filled || tp.ReplacedBy != "" {
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
			orderSignalID := signalID
			if orderSignalID == "" {
				orderSignalID = ctx.EntrySignalID
			}
			var row *store.CopyTradeOrder
			if tp.ClientID != "" {
				rows, err := x.st.CopyTrade().GetOrdersForContext(x.traderID, ctx.ID)
				if err != nil {
					return err
				}
				for _, r := range rows {
					if r.ClientID == tp.ClientID {
						row = r
						break
					}
				}
				if row == nil {
					return fmt.Errorf("TP order ledger missing")
				}
				// A TP that cannot be confirmed or was refused must never block
				// the stop, other levels, exits or finalization.
				if row.Status != "PLANNED" {
					resubmit, err := x.settleUnplacedTP(traceID, signalID, ctx, plan, tp, row)
					if err != nil {
						return err
					}
					if !resubmit {
						continue
					}
					if tp.ClientID != "" {
						// Confirmed by query: record what was observed.
						tp.OrderID, tp.Status = row.OrderID, row.Status
						tp.FilledQuantity = row.ExecutedQty
						tp.Filled = row.Status == "FILLED"
						if err := x.saveTPPlan(ctx, plan); err != nil {
							return err
						}
						continue
					}
				}
			}
			if tp.ClientID == "" {
				if tpDeferredByVenue(ctx, tp) {
					continue
				}
				if !tpPriceAllowed(tp.Price) {
					x.warnTPOnce(traceID, signalID, ctx, tp, "price_band", fmt.Sprintf("TP%d @ %.8g 超出交易所限价范围（参考价 %.8g），待价格接近后补挂", tp.Ordinal, tp.Price, reference), map[string]interface{}{"reference_price": reference, "percent_price_up": rules.PercentPriceUp, "percent_price_down": rules.PercentPriceDown, "min_price": rules.MinPrice, "max_price": rules.MaxPrice})
					continue
				}
				row = x.newManagedOrder(ctx, orderSignalID, fmt.Sprintf("TP_%d_%d", tp.Ordinal, tp.Generation), "LIMIT", tp.Price, want)
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
				switch row.Status {
				case "REJECTED":
					if row.LastError == "" {
						row.LastError = err.Error()
					}
					if _, serr := x.settleUnplacedTP(traceID, signalID, ctx, plan, tp, row); serr != nil {
						return serr
					}
					continue
				case "UNKNOWN":
					// Recovered by client ID on a later pass (see settleUnplacedTP).
					x.warnTPOnce(traceID, signalID, ctx, tp, fmt.Sprintf("unknown/%d", tp.Generation), fmt.Sprintf("TP%d submission outcome unknown; reconciling by client ID: %v", tp.Ordinal, err), nil)
					continue
				}
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

const (
	// tpUnknownGrace: an UNKNOWN TP whose client ID is still not found this
	// long after the uncertain submission was recorded never landed.
	tpUnknownGrace = stopNotFoundGrace
	// tpRejectedRetry throttles resubmission of a level whose request proved
	// not executed but may succeed later: a venue price-band rejection
	// (-4016/-4024/-4131, bound not published in exchangeInfo), a rate limit /
	// clock / credential refusal, or a local pre-send failure.
	tpRejectedRetry = 5 * time.Minute
)

var (
	// Per-process de-duplication of TP warnings and price-band retry timers.
	// Losing them on restart only repeats one warning or one attempt.
	tpWarned        sync.Map
	tpRetryAt       sync.Map
	rejectionCodeRE = regexp.MustCompile(`code=(-?\d+)`)
)

func tpKey(ctx *store.CopyTradeContext, tp *TPPlanEntry) string {
	return fmt.Sprintf("%s/%d", ctx.ID, tp.Ordinal)
}

func (x *Executor) warnTPOnce(traceID, signalID string, ctx *store.CopyTradeContext, tp *TPPlanEntry, kind, msg string, details map[string]interface{}) {
	if _, seen := tpWarned.LoadOrStore(tpKey(ctx, tp)+"/"+kind, true); seen {
		return
	}
	if details == nil {
		details = map[string]interface{}{}
	}
	details["context_id"], details["tp_level"], details["client_id"] = ctx.ID, tp.Ordinal, tp.ClientID
	x.events.Warn(traceID, signalID, "", EvExecutionError, fmt.Sprintf("%s %s", ctx.Symbol, msg), details)
}

func tpDeferredByVenue(ctx *store.CopyTradeContext, tp *TPPlanEntry) bool {
	v, ok := tpRetryAt.Load(tpKey(ctx, tp))
	return ok && stopClock().Before(v.(time.Time))
}

func venueRejectionCode(lastError string) int64 {
	m := rejectionCodeRE.FindStringSubmatch(lastError)
	if m == nil {
		return 0
	}
	code, _ := strconv.ParseInt(m[1], 10, 64)
	return code
}

// replanTP retires the current identity (its ledger row stays as evidence)
// and makes the level submittable again under a new generation/client ID.
func (x *Executor) replanTP(ctx *store.CopyTradeContext, plan []TPPlanEntry, tp *TPPlanEntry) error {
	tp.PriorFilledQuantity += tp.FilledQuantity
	tp.FilledQuantity = 0
	tp.OrderID, tp.ClientID = "", ""
	tp.Generation++
	tp.Status = "PLANNED"
	return x.saveTPPlan(ctx, plan)
}

// settleUnplacedTP resolves a TP whose ledger row is not known to be live.
// It returns resubmit=true when the caller should continue with tp: either
// tp.ClientID is still set (row confirmed by query; record it) or the level
// was re-planned (tp.ClientID == "") and may be submitted now. Only durable
// store failures are returned as errors; venue uncertainty skips this level.
func (x *Executor) settleUnplacedTP(traceID, signalID string, ctx *store.CopyTradeContext, plan []TPPlanEntry, tp *TPPlanEntry, row *store.CopyTradeOrder) (bool, error) {
	switch row.Status {
	case "PLANNED":
		return true, nil // intent stored, never sent: submit as before
	case "ABANDONED":
		return true, x.replanTP(ctx, plan, tp)
	case "REJECTED":
		code := venueRejectionCode(row.LastError)
		switch {
		case code == -4016 || code == -4024 || code == -4131:
			tpRetryAt.Store(tpKey(ctx, tp), stopClock().Add(tpRejectedRetry))
			x.warnTPOnce(traceID, signalID, ctx, tp, fmt.Sprintf("rejected/%d", code), fmt.Sprintf("TP%d @ %.8g 超出交易所限价范围（code %d），待价格接近后补挂", tp.Ordinal, tp.Price, code), map[string]interface{}{"error_code": code})
			return false, x.replanTP(ctx, plan, tp)
		case code == -1003 || code == -1021 || code == -1022 || code == -2015 || strings.Contains(row.LastError, types.ErrManagedOrderNotSent.Error()):
			tpRetryAt.Store(tpKey(ctx, tp), stopClock().Add(tpRejectedRetry))
			x.warnTPOnce(traceID, signalID, ctx, tp, fmt.Sprintf("rejected/%d", code), fmt.Sprintf("TP%d not executed (code %d); will retry: %s", tp.Ordinal, code, row.LastError), map[string]interface{}{"error_code": code})
			return false, x.replanTP(ctx, plan, tp)
		}
		x.warnTPOnce(traceID, signalID, ctx, tp, fmt.Sprintf("rejected/%d/%d", code, tp.Generation), fmt.Sprintf("TP%d @ %.8g rejected by exchange (code %d); level skipped: %s", tp.Ordinal, tp.Price, code, row.LastError), map[string]interface{}{"error_code": code})
		if tp.Status != "REJECTED" {
			tp.Status = "REJECTED"
			return false, x.saveTPPlan(ctx, plan)
		}
		return false, nil
	case "UNKNOWN", "SUBMITTING":
		err := x.queryManaged(row)
		if err == nil {
			if row.Status == "UNKNOWN" || row.Status == "SUBMITTING" {
				return false, nil
			}
			return true, nil
		}
		if errors.Is(err, types.ErrManagedOrderNotFound) && !row.UpdatedAt.IsZero() && stopClock().Sub(row.UpdatedAt) >= tpUnknownGrace {
			if uerr := x.st.CopyTrade().UpdateOrder(row.ID, map[string]interface{}{"status": "ABANDONED", "last_error": fmt.Sprintf("not found by client ID %s after %s; never landed", tpUnknownGrace, row.ClientID)}); uerr != nil {
				return false, uerr
			}
			row.Status = "ABANDONED"
			x.events.Warn(traceID, signalID, "", EvExecutionError, fmt.Sprintf("%s TP%d submission never reached the exchange (client ID %s not found); resubmitting", ctx.Symbol, tp.Ordinal, row.ClientID), map[string]interface{}{"context_id": ctx.ID, "tp_level": tp.Ordinal, "client_id": row.ClientID})
			return true, x.replanTP(ctx, plan, tp)
		}
		x.warnTPOnce(traceID, signalID, ctx, tp, fmt.Sprintf("unknown/%d", tp.Generation), fmt.Sprintf("TP%d outcome unconfirmed; level skipped this pass: %v", tp.Ordinal, err), nil)
		return false, nil
	}
	return true, nil
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
