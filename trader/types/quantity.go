package types

import (
	"math"
	"strconv"
	"strings"
)

// FormatBaseQuantityByContract formats a BASE-ASSET quantity for an exchange
// whose orders are denominated in contracts (OKX, KuCoin, Gate, ...).
//
// The Trader interface contract for FormatQuantity is "align a base-asset
// quantity to the instrument's valid precision" — the result must stay in
// base-asset units. Contract-denominated exchanges convert to contract count
// inside their order methods (OpenLong/CloseLong/...), so converting here as
// well would double-convert and inflate the order size (see the OKX
// copy-trading incident: 0.05 BTC became 5.02 BTC ≈ $390k notional).
//
// The quantity is floored to a whole multiple of the contract step
// (contractSize * lotSize) and rendered with matching decimal precision.
// A quantity smaller than one step formats to zero; callers already treat
// zero as "order too small".
func FormatBaseQuantityByContract(quantity, contractSize, lotSize float64) string {
	if contractSize <= 0 {
		contractSize = 1
	}
	if lotSize <= 0 {
		lotSize = 1
	}
	step := contractSize * lotSize
	// Absorb binary float artifacts (e.g. 0.1*0.1 = 0.010000000000000002)
	// so precision detection below stays sane.
	step = math.Round(step*1e12) / 1e12
	if step <= 0 {
		step = 1
	}

	steps := math.Floor(quantity/step + 1e-9)
	if steps < 0 {
		steps = 0
	}
	aligned := steps * step
	// Re-round the product to kill accumulated float error before printing.
	aligned = math.Round(aligned*1e12) / 1e12

	return strconv.FormatFloat(aligned, 'f', decimalPlaces(step), 64)
}

// decimalPlaces returns the number of decimal digits needed to represent the
// step exactly (e.g. 0.001 -> 3, 1 -> 0, 0.25 -> 2).
func decimalPlaces(step float64) int {
	s := strconv.FormatFloat(step, 'f', -1, 64)
	if i := strings.Index(s, "."); i >= 0 {
		return len(s) - i - 1
	}
	return 0
}
