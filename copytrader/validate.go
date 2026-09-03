package copytrader

import (
	"fmt"
)

// DefaultSanityDeviationPct is the maximum plausible distance between a
// signal price and the live market before we suspect an OCR/LLM digit error.
const DefaultSanityDeviationPct = 60.0

// ValidateInterpretation applies deterministic business rules to an
// actionable interpretation BEFORE it may create or mutate a trade.
// marketPrice <= 0 skips the market sanity checks (used in pure parsing tests).
//
// Returns a SkipReason (non-error terminal outcome) or an error for outright
// invalid payloads. skip != SkipNone means "do not execute, log and move on".
func ValidateInterpretation(si *SourceInterpretation, marketPrice float64) (SkipReason, error) {
	if si == nil {
		return SkipNotSignal, fmt.Errorf("nil interpretation")
	}
	switch si.Classification {
	case ClassificationSignal:
	case ClassificationIgnore:
		return SkipNotSignal, nil
	case ClassificationNeedsContext:
		return SkipNeedsContext, nil
	case ClassificationAmbiguous:
		return SkipAmbiguous, nil
	case ClassificationUnsupported:
		return SkipUnsupportedInstrument, nil
	default:
		return SkipNotSignal, fmt.Errorf("unknown classification %q", si.Classification)
	}

	switch si.Action {
	case ActionOpen, ActionAdd:
		return validateOpen(si, marketPrice)
	case ActionReduce, ActionClose, ActionCancel:
		if si.Symbol == "" {
			return SkipNeedsContext, nil
		}
		return SkipNone, nil
	case ActionUpdateSL:
		if si.Symbol == "" {
			return SkipNeedsContext, nil
		}
		if len(si.StopLossLevels) == 0 {
			return SkipNone, fmt.Errorf("UPDATE_SL without a stop loss level")
		}
		return validateSLSpecs(si, marketPrice)
	case ActionUpdateTP:
		if si.Symbol == "" {
			return SkipNeedsContext, nil
		}
		if len(si.TakeProfitLevels) == 0 {
			return SkipNone, fmt.Errorf("UPDATE_TP without take profit levels")
		}
		return validateTPSpecs(si, marketPrice)
	case ActionIgnore:
		return SkipNotSignal, nil
	default:
		return SkipNotSignal, fmt.Errorf("unknown action %q", si.Action)
	}
}

func validateOpen(si *SourceInterpretation, marketPrice float64) (SkipReason, error) {
	if si.Symbol == "" {
		return SkipNeedsContext, nil
	}
	if si.Direction != DirectionLong && si.Direction != DirectionShort {
		return SkipAmbiguous, nil
	}
	if len(si.EntryOrders) == 0 {
		return SkipNone, fmt.Errorf("OPEN without entry orders")
	}
	// Require SL: live copy trading never opens unprotected positions.
	if len(si.StopLossLevels) == 0 {
		return SkipRiskRejected, nil
	}
	if len(si.TakeProfitLevels) > 3 {
		return SkipNone, fmt.Errorf("more than 3 TP levels not supported in V1")
	}

	entryRef := entryReferencePrice(si.EntryOrders[0].Price, marketPrice)
	if entryRef <= 0 {
		return SkipUnsupportedPriceSpec, nil
	}
	if err := SanityCheckPrice("entry", entryRef, marketPrice, sanityPct(marketPrice)); err != nil {
		return SkipSanityCheck, nil
	}

	// Stop loss must be a resolvable hard price on the correct side of entry.
	sl := si.StopLossLevels[0]
	slPrice := resolveHardPrice(sl.Price, entryRef)
	if slPrice <= 0 {
		return SkipUnsupportedPriceSpec, nil
	}
	if err := SanityCheckPrice("stop_loss", slPrice, marketPrice, sanityPct(marketPrice)); err != nil {
		return SkipSanityCheck, nil
	}
	if si.Direction == DirectionLong && slPrice >= entryRef {
		return SkipNone, fmt.Errorf("long stop loss %.8g must be below entry %.8g", slPrice, entryRef)
	}
	if si.Direction == DirectionShort && slPrice <= entryRef {
		return SkipNone, fmt.Errorf("short stop loss %.8g must be above entry %.8g", slPrice, entryRef)
	}

	// Take profits (optional) must sit on the profitable side.
	for i, tp := range si.TakeProfitLevels {
		tpPrice := resolveHardPrice(tp.Price, entryRef)
		if tpPrice <= 0 {
			return SkipUnsupportedPriceSpec, nil
		}
		if err := SanityCheckPrice("take_profit", tpPrice, marketPrice, sanityPct(marketPrice)); err != nil {
			return SkipSanityCheck, nil
		}
		if si.Direction == DirectionLong && tpPrice <= entryRef {
			return SkipNone, fmt.Errorf("long TP%d %.8g must be above entry %.8g", i+1, tpPrice, entryRef)
		}
		if si.Direction == DirectionShort && tpPrice >= entryRef {
			return SkipNone, fmt.Errorf("short TP%d %.8g must be below entry %.8g", i+1, tpPrice, entryRef)
		}
	}
	return SkipNone, nil
}

func validateSLSpecs(si *SourceInterpretation, marketPrice float64) (SkipReason, error) {
	for _, sl := range si.StopLossLevels {
		switch sl.Price.Type {
		case PriceFixed:
			if sl.Price.Price <= 0 {
				return SkipNone, fmt.Errorf("stop loss fixed price missing")
			}
			if err := SanityCheckPrice("stop_loss", sl.Price.Price, marketPrice, sanityPct(marketPrice)); err != nil {
				return SkipSanityCheck, nil
			}
		case PriceEntry, PriceBreakeven:
			// resolved against the trade context at execution time
		default:
			return SkipUnsupportedPriceSpec, nil
		}
	}
	return SkipNone, nil
}

func validateTPSpecs(si *SourceInterpretation, marketPrice float64) (SkipReason, error) {
	if len(si.TakeProfitLevels) > 3 {
		return SkipNone, fmt.Errorf("more than 3 TP levels not supported in V1")
	}
	for _, tp := range si.TakeProfitLevels {
		switch tp.Price.Type {
		case PriceFixed:
			if tp.Price.Price <= 0 {
				return SkipNone, fmt.Errorf("take profit fixed price missing")
			}
			if err := SanityCheckPrice("take_profit", tp.Price.Price, marketPrice, sanityPct(marketPrice)); err != nil {
				return SkipSanityCheck, nil
			}
		default:
			return SkipUnsupportedPriceSpec, nil
		}
	}
	return SkipNone, nil
}

// entryReferencePrice resolves the price used for validation and sizing.
func entryReferencePrice(spec PriceSpec, marketPrice float64) float64 {
	switch spec.Type {
	case PriceFixed:
		return spec.Price
	case PriceMarket:
		return marketPrice
	case PriceRange:
		low, high := spec.RangeLow, spec.RangeHigh
		if low > high {
			low, high = high, low
		}
		if low <= 0 {
			return 0
		}
		return (low + high) / 2
	default:
		return 0
	}
}

// resolveHardPrice resolves specs that map to a concrete price given the
// entry reference. Unsupported specs return 0.
func resolveHardPrice(spec PriceSpec, entryRef float64) float64 {
	switch spec.Type {
	case PriceFixed:
		return spec.Price
	case PriceEntry, PriceBreakeven:
		return entryRef
	default:
		return 0
	}
}

// sanityPct returns the sanity deviation threshold; disabled when the market
// price is unknown.
func sanityPct(marketPrice float64) float64 {
	if marketPrice <= 0 {
		return 0
	}
	return DefaultSanityDeviationPct
}
