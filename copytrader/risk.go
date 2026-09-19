package copytrader

import (
	"fmt"
	"math"
)

// SizingInput carries everything the deterministic risk engine needs.
// The AI never provides quantities; only prices and direction.
type SizingInput struct {
	RiskMode      RiskMode
	RiskAmountUSD float64 // by_loss: USD at risk; percent: % of equity; fixed: margin USD
	EquityUSD     float64 // account equity (needed for percent mode and exposure caps)

	EntryPrice    float64 // sizing reference price (signal entry, or market at submit time)
	StopLossPrice float64 // required for by_loss
	Leverage      int

	// Hard caps. Zero disables the individual cap.
	MaxPositionNotionalUSD float64
	AvailableMarginUSD     float64
	MarginBufferPct        float64 // portion of available margin usable, default 0.9
}

// SizingResult is the deterministic sizing outcome, with every applied
// constraint recorded for the trace log (mirrors the reference project's
// quantity_plan event).
type SizingResult struct {
	RawQuantity        float64  `json:"raw_quantity"`
	FinalQuantity      float64  `json:"final_quantity"`
	NotionalUSD        float64  `json:"notional_usd"`
	EstimatedMarginUSD float64  `json:"estimated_margin_usd"`
	RiskPerUnit        float64  `json:"risk_per_unit"`
	EstimatedRiskUSD   float64  `json:"estimated_risk_usd"`
	AppliedConstraints []string `json:"applied_constraints,omitempty"`
}

// ComputePositionSize derives the position quantity from the risk config.
// Returned quantity is unrounded; the executor applies exchange precision
// (FormatQuantity) right before submitting.
func ComputePositionSize(in SizingInput) (*SizingResult, error) {
	if in.EntryPrice <= 0 {
		return nil, fmt.Errorf("entry price must be > 0")
	}
	if in.Leverage <= 0 {
		return nil, fmt.Errorf("leverage must be > 0")
	}
	if in.RiskAmountUSD <= 0 {
		return nil, fmt.Errorf("risk amount must be > 0")
	}

	res := &SizingResult{}
	var qty float64

	switch in.RiskMode {
	case RiskModeByLoss:
		if in.StopLossPrice <= 0 {
			return nil, fmt.Errorf("by_loss mode requires a stop loss price")
		}
		riskPerUnit := math.Abs(in.EntryPrice - in.StopLossPrice)
		if riskPerUnit <= 0 {
			return nil, fmt.Errorf("stop loss equals entry price, cannot size by loss")
		}
		// Guard against absurd sizing from razor-thin stops (entry 100 / SL 99.95).
		if riskPerUnit/in.EntryPrice < 0.0005 {
			return nil, fmt.Errorf("stop distance %.4f%% of entry is below the 0.05%% sizing floor", riskPerUnit/in.EntryPrice*100)
		}
		res.RiskPerUnit = riskPerUnit
		qty = in.RiskAmountUSD / riskPerUnit
	case RiskModePercent:
		if in.EquityUSD <= 0 {
			return nil, fmt.Errorf("percent mode requires account equity")
		}
		margin := in.EquityUSD * in.RiskAmountUSD / 100
		qty = margin * float64(in.Leverage) / in.EntryPrice
	case RiskModeFixed:
		qty = in.RiskAmountUSD * float64(in.Leverage) / in.EntryPrice
	default:
		return nil, fmt.Errorf("unknown risk mode %q", in.RiskMode)
	}

	res.RawQuantity = qty

	// Hard cap: max position notional.
	if in.MaxPositionNotionalUSD > 0 {
		maxQty := in.MaxPositionNotionalUSD / in.EntryPrice
		if qty > maxQty {
			qty = maxQty
			res.AppliedConstraints = append(res.AppliedConstraints, "max_position_notional")
		}
	}

	// Hard cap: available margin (with buffer for fees/slippage).
	if in.AvailableMarginUSD > 0 {
		buffer := in.MarginBufferPct
		if buffer <= 0 || buffer > 1 {
			buffer = 0.9
		}
		maxQty := in.AvailableMarginUSD * buffer * float64(in.Leverage) / in.EntryPrice
		if qty > maxQty {
			qty = maxQty
			res.AppliedConstraints = append(res.AppliedConstraints, "available_margin")
		}
	}

	if qty <= 0 {
		return nil, fmt.Errorf("computed quantity is zero after applying constraints")
	}

	res.FinalQuantity = qty
	res.NotionalUSD = qty * in.EntryPrice
	res.EstimatedMarginUSD = res.NotionalUSD / float64(in.Leverage)
	if res.RiskPerUnit > 0 {
		res.EstimatedRiskUSD = qty * res.RiskPerUnit
	}
	return res, nil
}

// EffectiveRisk recomputes the true USD risk after the entry actually fills.
// Market entries can fill away from the signal price, silently inflating risk;
// callers compare against the configured risk and log a critical warning when
// the threshold is exceeded.
func EffectiveRisk(actualQty, avgFillPrice, stopLossPrice float64) float64 {
	if actualQty <= 0 || avgFillPrice <= 0 || stopLossPrice <= 0 {
		return 0
	}
	return actualQty * math.Abs(avgFillPrice-stopLossPrice)
}

// EntryPlanType is the concrete order type decision for an entry.
type EntryPlanType string

const (
	EntryPlanMarket EntryPlanType = "MARKET"
	EntryPlanLimit  EntryPlanType = "LIMIT"
	EntryPlanSkip   EntryPlanType = "SKIP"
)

// entryDecision is the policy result and its input snapshot. The executor and
// audit event consume the SAME result, so explaining a decision cannot drift
// from the order actually planned. Deviations are percentage points, not ratios.
type entryDecision struct {
	Direction           Direction     `json:"direction"`
	SourcePriceSpec     PriceSpec     `json:"source_price_spec"`
	MarketPrice         float64       `json:"market_price"`
	ReferencePrice      float64       `json:"reference_price,omitempty"`
	AdverseDeviationPct *float64      `json:"adverse_deviation_pct,omitempty"`
	ThresholdPct        float64       `json:"threshold_pct"`
	LimitToMarketWithin bool          `json:"limit_to_market_within_threshold"`
	OrderType           EntryPlanType `json:"order_type"`
	EntryPrice          float64       `json:"entry_price"`
	Reason              string        `json:"reason"`
}

// Only absorbs floating-point rounding at the inclusive threshold boundary.
// A zero threshold is handled separately and never gains any tolerance.
const entryThresholdEpsilon = 1e-9

// DecideEntryType applies the DIRECTION-AWARE price-deviation policy.
//
// "Favorable" means the live market is at or better than the author's
// reference (long: market <= ref, short: market >= ref) — the follower's
// R:R is then at least as good as the author's.
//
//   - Favorable => MARKET immediately. Never gated by the threshold or the
//     limitToMarketWithin toggle: a limit at the reference would cross the
//     book and fill as taker anyway, only through the slower limit path with
//     delayed SL/TP placement. (The caller must still apply the stop-loss
//     invalidation guard: a market already through the author's stop is a
//     broken setup, not a bargain.)
//   - Unfavorable within threshold => MARKET when the author asked for a
//     market entry, or when limitToMarketWithin is enabled for fixed prices;
//     otherwise LIMIT at the reference.
//   - Unfavorable beyond threshold => LIMIT at the reference (never chase).
//   - thresholdPct <= 0 disables the adverse tolerance entirely: only
//     favorable prices enter at market, everything unfavorable rests as a
//     limit at the reference.
func DecideEntryType(direction Direction, spec PriceSpec, marketPrice, thresholdPct float64, limitToMarketWithin bool) (EntryPlanType, float64, error) {
	d, err := decideEntry(direction, spec, marketPrice, thresholdPct, limitToMarketWithin)
	return d.OrderType, d.EntryPrice, err
}

func decideEntry(direction Direction, spec PriceSpec, marketPrice, thresholdPct float64, limitToMarketWithin bool) (entryDecision, error) {
	d := entryDecision{
		Direction: direction, SourcePriceSpec: spec, MarketPrice: marketPrice,
		ThresholdPct: thresholdPct, LimitToMarketWithin: limitToMarketWithin,
		OrderType: EntryPlanSkip,
	}
	finish := func(kind EntryPlanType, price float64, reason string) (entryDecision, error) {
		d.OrderType, d.EntryPrice, d.Reason = kind, price, reason
		return d, nil
	}
	if marketPrice <= 0 || math.IsNaN(marketPrice) || math.IsInf(marketPrice, 0) {
		return d, fmt.Errorf("market price unavailable")
	}
	if math.IsNaN(thresholdPct) || math.IsInf(thresholdPct, 0) {
		return d, fmt.Errorf("price offset threshold must be finite")
	}
	favorable := func(ref float64) bool {
		if direction == DirectionShort {
			return marketPrice >= ref
		}
		return marketPrice <= ref
	}
	setReference := func(ref float64) {
		d.ReferencePrice = ref
		deviation := (marketPrice - ref) / ref * 100
		if direction == DirectionShort {
			deviation = -deviation
		}
		deviation = math.Max(0, deviation)
		d.AdverseDeviationPct = &deviation
	}
	withinThreshold := func() bool {
		return thresholdPct > 0 && d.AdverseDeviationPct != nil &&
			*d.AdverseDeviationPct <= thresholdPct+entryThresholdEpsilon
	}

	switch spec.Type {
	case PriceMarket:
		if spec.Price < 0 || math.IsNaN(spec.Price) || math.IsInf(spec.Price, 0) {
			return d, fmt.Errorf("market entry reference must be non-negative and finite")
		}
		if spec.Price == 0 {
			return finish(EntryPlanMarket, marketPrice, "market_without_reference")
		}
		setReference(spec.Price)
		if favorable(spec.Price) {
			return finish(EntryPlanMarket, marketPrice, "favorable_price")
		}
		if withinThreshold() {
			return finish(EntryPlanMarket, marketPrice, "adverse_within_threshold")
		}
		if thresholdPct <= 0 {
			return finish(EntryPlanLimit, spec.Price, "adverse_tolerance_disabled")
		}
		return finish(EntryPlanLimit, spec.Price, "adverse_beyond_threshold")
	case PriceFixed:
		if spec.Price <= 0 || math.IsNaN(spec.Price) || math.IsInf(spec.Price, 0) {
			return d, fmt.Errorf("fixed entry price missing or invalid")
		}
		setReference(spec.Price)
		if favorable(spec.Price) {
			return finish(EntryPlanMarket, marketPrice, "favorable_price")
		}
		if withinThreshold() {
			if limitToMarketWithin {
				return finish(EntryPlanMarket, marketPrice, "adverse_within_threshold")
			}
			return finish(EntryPlanLimit, spec.Price, "limit_conversion_disabled")
		}
		if thresholdPct <= 0 {
			return finish(EntryPlanLimit, spec.Price, "adverse_tolerance_disabled")
		}
		return finish(EntryPlanLimit, spec.Price, "adverse_beyond_threshold")
	case PriceRange:
		low, high := spec.RangeLow, spec.RangeHigh
		if low > high {
			low, high = high, low
		}
		if low <= 0 || math.IsNaN(low) || math.IsNaN(high) || math.IsInf(low, 0) || math.IsInf(high, 0) {
			return d, fmt.Errorf("entry range invalid")
		}
		setReference((low + high) / 2)
		if marketPrice >= low && marketPrice <= high {
			return finish(EntryPlanMarket, marketPrice, "inside_entry_range")
		}
		if direction == DirectionShort && marketPrice > high { // better than the whole zone
			return finish(EntryPlanMarket, marketPrice, "favorable_price")
		}
		if direction != DirectionShort && marketPrice < low { // long better than the whole zone
			return finish(EntryPlanMarket, marketPrice, "favorable_price")
		}
		// Adverse: rest a limit at the zone midpoint — a balance between the
		// near edge (fills first, worst price of the zone) and the far edge
		// (best price, may never fill). Matches how authors mean a zone:
		// an average entry around its middle.
		return finish(EntryPlanLimit, d.ReferencePrice, "adverse_range_midpoint")
	default:
		return d, fmt.Errorf("unsupported entry price spec %q", spec.Type)
	}
}

// SanityCheckPrice rejects prices that are implausibly far from the market
// (OCR/LLM digit errors: 62000 read as 6200 or 620000). maxDeviationPct <= 0
// disables the check.
func SanityCheckPrice(name string, price, marketPrice, maxDeviationPct float64) error {
	if maxDeviationPct <= 0 || price <= 0 || marketPrice <= 0 {
		return nil
	}
	deviation := math.Abs(price-marketPrice) / marketPrice * 100
	if deviation > maxDeviationPct {
		return fmt.Errorf("%s price %.8g deviates %.1f%% from market %.8g (max %.1f%%)",
			name, price, deviation, marketPrice, maxDeviationPct)
	}
	return nil
}
