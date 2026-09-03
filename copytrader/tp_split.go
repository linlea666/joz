package copytrader

import (
	"fmt"
	"math"

	"nofx/trader/types"
)

// AllocateTPRatios decides the position percentage per TP level.
//
// Semantics (never invent TP levels the author didn't give):
//   - explicit ratios from the signal always win over configured defaults;
//   - all levels unspecified: 1 TP => 100%, 2 TPs => 50/50,
//     3 TPs => configured defaults (e.g. 50/30/20);
//   - partially specified: explicit values kept, the remaining percentage is
//     split evenly across unspecified levels ("TP1 25%, TP2 rest" => 25/75);
//   - at most 3 levels in V1 (callers truncate before allocation);
//   - the final level absorbs rounding so the total is exactly 100.
func AllocateTPRatios(levels []TPLevel, defaults []float64) ([]float64, error) {
	n := len(levels)
	if n == 0 {
		return nil, nil
	}
	if n > 3 {
		return nil, fmt.Errorf("at most 3 TP levels supported, got %d", n)
	}

	out := make([]float64, n)
	explicitTotal := 0.0
	unspecified := make([]int, 0, n)
	for i, lv := range levels {
		if lv.Ratio != nil {
			if *lv.Ratio <= 0 || *lv.Ratio > 100 {
				return nil, fmt.Errorf("TP level %d ratio %v out of range (0, 100]", i+1, *lv.Ratio)
			}
			out[i] = *lv.Ratio
			explicitTotal += *lv.Ratio
		} else {
			unspecified = append(unspecified, i)
		}
	}
	if explicitTotal > 100+1e-9 {
		return nil, fmt.Errorf("explicit TP ratios sum to %.2f, exceeding 100", explicitTotal)
	}

	switch {
	case len(unspecified) == 0:
		// Fully explicit. A total below 100 means the author intentionally
		// keeps a runner; respect it as-is.
	case len(unspecified) == n:
		// Fully unspecified: apply the default policy.
		switch n {
		case 1:
			out[0] = 100
		case 2:
			out[0], out[1] = 50, 50
		case 3:
			d := defaults
			if len(d) != 3 {
				d = []float64{50, 30, 20}
			}
			copy(out, d)
			// Normalize configured defaults that don't reach 100.
			sum := d[0] + d[1] + d[2]
			if math.Abs(sum-100) > 1e-9 && sum > 0 {
				for i := range out {
					out[i] = out[i] / sum * 100
				}
			}
		}
	default:
		// Mixed: split the remainder evenly across unspecified levels.
		remaining := 100 - explicitTotal
		share := remaining / float64(len(unspecified))
		for _, idx := range unspecified {
			out[idx] = share
		}
	}
	return out, nil
}

// SplitTPQuantities converts ratios into per-level quantities based on the
// ACTUAL filled quantity (never the submitted quantity).
//
// Precision handling: the first N-1 levels are floored to stepSize and the
// last level receives the exact remainder, so the reduce-only total always
// equals the remaining position (no dust, no over-reduce). Levels below
// minQty are merged into the following level.
func SplitTPQuantities(filledQty float64, ratios []float64, stepSize, minQty float64) ([]float64, error) {
	if filledQty <= 0 {
		return nil, fmt.Errorf("filled quantity must be > 0")
	}
	if len(ratios) == 0 {
		return nil, nil
	}
	totalRatio := 0.0
	for _, r := range ratios {
		totalRatio += r
	}
	if totalRatio > 100+1e-9 {
		return nil, fmt.Errorf("TP ratios sum to %.2f, exceeding 100", totalRatio)
	}

	floor := func(q float64) float64 {
		if stepSize <= 0 {
			return q
		}
		// Round to the step grid, guarding float artifacts (0.007/0.001 => 6.999...).
		steps := math.Floor(q/stepSize + 1e-9)
		return steps * stepSize
	}

	quantities := make([]float64, len(ratios))
	allocated := 0.0
	// The portion of the position covered by TPs (total may be < 100 for runners).
	tpTotal := filledQty * totalRatio / 100

	for i, r := range ratios {
		if i == len(ratios)-1 {
			quantities[i] = floor(tpTotal - allocated)
		} else {
			q := floor(filledQty * r / 100)
			quantities[i] = q
			allocated += q
		}
	}

	// Merge sub-minimum levels forward so no order is rejected for size.
	if minQty > 0 {
		for i := 0; i < len(quantities)-1; i++ {
			if quantities[i] > 0 && quantities[i] < minQty {
				quantities[i+1] += quantities[i]
				quantities[i] = 0
			}
		}
	}

	// Drop zero-quantity levels but keep order alignment by compacting later:
	// callers pair quantities[i] with the TP price at the same index and skip zeros.
	// Sanitize each level: ratio math (qty*r/100, steps*step) leaves binary
	// float artifacts that would be persisted in the TP plan and displayed.
	sum := 0.0
	for i, q := range quantities {
		if q < 0 {
			return nil, fmt.Errorf("internal error: negative TP quantity")
		}
		quantities[i] = types.SanitizeBaseQuantity(q)
		sum += quantities[i]
	}
	if sum > filledQty+1e-9 {
		return nil, fmt.Errorf("TP quantities %.10f exceed filled quantity %.10f", sum, filledQty)
	}
	return quantities, nil
}
