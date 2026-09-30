package copytrader

import (
	"encoding/json"
	"fmt"
	"math"
	"nofx/store"
)

type tpUpdateIntent struct {
	SignalID string    `json:"signal_id"`
	Prices   []float64 `json:"prices"`
	Ratios   []float64 `json:"ratios"`
	Ordinals []int     `json:"ordinals"`
	Stage    string    `json:"stage"`
}

// mapTPUpdate refuses positional guesses when a shortened ladder could rename
// an already-filled level. Exact surviving prices can retain their old ordinal.
func mapTPUpdate(old []TPPlanEntry, prices []float64) ([]int, error) {
	max := 0
	for _, p := range old {
		if p.Ordinal > max {
			max = p.Ordinal
		}
	}
	ids := make([]int, len(prices))
	if max == 0 || max == len(prices) {
		for i := range ids {
			ids[i] = i + 1
		}
		return ids, nil
	}
	used := map[int]bool{}
	for i, p := range prices {
		for _, o := range old {
			if !o.Filled && sameStep(p, o.Price, 0) && !used[o.Ordinal] {
				ids[i] = o.Ordinal
				used[o.Ordinal] = true
				break
			}
		}
		if ids[i] == 0 {
			return nil, fmt.Errorf("TP ordinal mapping ambiguous; original protection retained")
		}
	}
	return ids, nil
}

// remainingTPAllocation subtracts partial fills before normalizing the live
// weights; a total below 100 explicitly leaves the same runner fraction.
func remainingTPAllocation(qty float64, old []TPPlanEntry, ids []int, ratios []float64) ([]float64, error) {
	if len(ids) != len(ratios) {
		return nil, fmt.Errorf("TP ratio/target length mismatch")
	}
	total, weights := 0.0, make([]float64, len(ids))
	priorTotal := 0.0
	for _, o := range old {
		priorTotal += o.PriorFilledQuantity + o.FilledQuantity
	}
	for i, id := range ids {
		if !finite(ratios[i]) || ratios[i] < 0 {
			return nil, fmt.Errorf("invalid TP ratio")
		}
		total += ratios[i]
		prior := 0.0
		done := false
		for _, o := range old {
			if o.Ordinal == id {
				done = o.Filled
				prior = o.FilledQuantity + o.PriorFilledQuantity
				break
			}
		}
		if !done {
			weights[i] = math.Max(0, (qty+priorTotal)*ratios[i]/100-prior)
		}
	}
	if total > 100+1e-9 {
		return nil, fmt.Errorf("TP ratios exceed 100")
	}
	sum := 0.0
	for _, w := range weights {
		sum += w
	}
	if sum <= 0 {
		return nil, fmt.Errorf("no unfinished TP allocation remains")
	}
	for i := range weights {
		weights[i] = weights[i] / sum * total
	}
	return weights, nil
}

func (x *Executor) updateTakeProfits(traceID, signalID string, ctx *store.CopyTradeContext, prices, ratios []float64) (SkipReason, error) {
	var intent tpUpdateIntent
	if ctx.TPUpdateIntentJSON != "" {
		if err := json.Unmarshal([]byte(ctx.TPUpdateIntentJSON), &intent); err != nil {
			return SkipNone, err
		}
		if signalID != "" && intent.SignalID != signalID {
			return SkipNone, fmt.Errorf("previous TP update still pending")
		}
	} else {
		if len(prices) == 0 || len(prices) > MaxTPLevels || len(prices) != len(ratios) {
			return SkipUnsupportedPriceSpec, fmt.Errorf("invalid TP ladder")
		}
		for _, p := range prices {
			if p <= 0 || !finite(p) {
				return SkipUnsupportedPriceSpec, fmt.Errorf("invalid TP price")
			}
		}
		ids, err := mapTPUpdate(readTPPlan(ctx), prices)
		if err != nil {
			return SkipNeedsContext, err
		}
		intent = tpUpdateIntent{signalID, prices, ratios, ids, "CANCEL"}
		b, _ := json.Marshal(intent)
		if err = x.persistContext(ctx, map[string]interface{}{"tp_update_intent_json": string(b)}); err != nil {
			return SkipNone, err
		}
	}
	signalID = intent.SignalID
	if intent.Stage == "CANCEL" {
		if err := x.cancelTrackedTPs(ctx); err != nil {
			return SkipNone, err
		}
		if _, err := x.refreshTPProgressChecked(traceID, ctx); err != nil {
			return SkipNone, err
		}
		// Cancellation can fill: position truth is fetched AFTER all cancels finish.
		pos, err := x.freshPosition(ctx.Symbol, ctx.Direction)
		if err != nil {
			return SkipNone, err
		}
		if pos == nil || pos.qty <= 0 {
			return SkipNoPosition, x.persistContext(ctx, map[string]interface{}{"tp_update_intent_json": "", "last_tp_signal_id": intent.SignalID})
		}
		old := readTPPlan(ctx)
		weights, err := remainingTPAllocation(pos.qty, old, intent.Ordinals, intent.Ratios)
		if err != nil {
			return SkipNone, err
		}
		quantities, err := SplitTPQuantities(pos.qty, weights, x.detectStepSize(ctx.Symbol, pos.qty), 0)
		if err != nil {
			return SkipNone, err
		}
		plan := make([]TPPlanEntry, 0, len(old)+len(prices))
		for _, o := range old {
			if o.Filled {
				plan = append(plan, o)
			}
		}
		for i, id := range intent.Ordinals {
			prior := TPPlanEntry{Ordinal: id}
			for _, o := range old {
				if o.Ordinal == id {
					prior = o
					break
				}
			}
			if prior.Filled {
				continue
			}
			prior.Generation++
			prior.PriorFilledQuantity += prior.FilledQuantity
			prior.FilledQuantity = 0
			prior.Price = intent.Prices[i]
			prior.Quantity = quantities[i]
			q := quantities[i]
			prior.DesiredQuantity = &q
			prior.Status = "PLANNED"
			prior.OrderID = ""
			prior.ClientID = ""
			prior.OrderKind = ""
			plan = append(plan, prior)
		}
		intent.Stage = "INSTALL"
		b, _ := json.Marshal(intent)
		p, _ := json.Marshal(plan)
		if err = x.persistContext(ctx, map[string]interface{}{"tp_update_intent_json": string(b), "tp_plan_json": string(p), "tp_position_quantity": pos.qty, "last_action": "UPDATE_TP"}); err != nil {
			return SkipNone, err
		}
	}
	pos, err := x.freshPosition(ctx.Symbol, ctx.Direction)
	if err != nil {
		return SkipNone, err
	}
	if pos != nil && pos.qty > 0 {
		if err = x.restoreRemainingProtections(traceID, signalID, ctx, pos.qty); err != nil {
			return SkipNone, err
		}
	}
	return SkipNone, x.persistContext(ctx, map[string]interface{}{"tp_update_intent_json": "", "last_tp_signal_id": intent.SignalID})
}
