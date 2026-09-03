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

// DecideEntryType applies the price-deviation policy:
//   - Signal wants market (CMP): execute market while |market-ref| <= threshold,
//     otherwise fall back to a limit at the reference price (protects against
//     late fills far from the author's intent). With no reference price the
//     market order goes through as-is.
//   - Signal gives a limit price: if limitToMarketWithin is on and the market
//     is already within the threshold of that price, take market immediately
//     (avoids missing fast moves); otherwise place the limit.
//
// thresholdPct <= 0 disables deviation checks.
func DecideEntryType(spec PriceSpec, marketPrice, thresholdPct float64, limitToMarketWithin bool) (EntryPlanType, float64, error) {
	if marketPrice <= 0 {
		return EntryPlanSkip, 0, fmt.Errorf("market price unavailable")
	}
	deviationOK := func(ref float64) bool {
		if thresholdPct <= 0 || ref <= 0 {
			return true
		}
		return math.Abs(marketPrice-ref)/ref*100 <= thresholdPct
	}

	switch spec.Type {
	case PriceMarket:
		// Deviation circuit breaker: when the author stated a reference price
		// alongside a market entry ("CMP ~62000") and the live market has
		// already run away past the threshold, fall back to a limit at the
		// reference instead of chasing the move. No reference => plain market.
		if spec.Price > 0 && !deviationOK(spec.Price) {
			return EntryPlanLimit, spec.Price, nil
		}
		return EntryPlanMarket, marketPrice, nil
	case PriceFixed:
		if spec.Price <= 0 {
			return EntryPlanSkip, 0, fmt.Errorf("fixed entry price missing")
		}
		if deviationOK(spec.Price) {
			if limitToMarketWithin {
				return EntryPlanMarket, marketPrice, nil
			}
			return EntryPlanLimit, spec.Price, nil
		}
		return EntryPlanLimit, spec.Price, nil
	case PriceRange:
		low, high := spec.RangeLow, spec.RangeHigh
		if low > high {
			low, high = high, low
		}
		if low <= 0 {
			return EntryPlanSkip, 0, fmt.Errorf("entry range invalid")
		}
		// V1: market when already inside the range, otherwise limit at the
		// range edge closest to the market.
		if marketPrice >= low && marketPrice <= high {
			return EntryPlanMarket, marketPrice, nil
		}
		if marketPrice < low {
			return EntryPlanLimit, low, nil
		}
		return EntryPlanLimit, high, nil
	default:
		return EntryPlanSkip, 0, fmt.Errorf("unsupported entry price spec %q", spec.Type)
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
