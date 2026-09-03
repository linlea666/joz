package copytrader

import (
	"math"
	"testing"
)

func TestComputePositionSizeByLoss(t *testing.T) {
	// Real jonzi card: entry 77005.1, SL 76851.3 => "for every $100 risk,
	// position = 0.6502 BTC". With $50 risk expect ~0.3251 (matches the
	// reference project's quantity_plan rawTotalQuantity 0.32509752...).
	res, err := ComputePositionSize(SizingInput{
		RiskMode:      RiskModeByLoss,
		RiskAmountUSD: 50,
		EntryPrice:    77005.1,
		StopLossPrice: 76851.3,
		Leverage:      100,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if math.Abs(res.FinalQuantity-0.3250975292587715) > 1e-9 {
		t.Errorf("quantity = %v, want 0.3250975292587715", res.FinalQuantity)
	}
	if math.Abs(res.EstimatedMarginUSD-250.34) > 0.01 {
		t.Errorf("margin = %v, want ~250.34", res.EstimatedMarginUSD)
	}
	if math.Abs(res.EstimatedRiskUSD-50) > 1e-6 {
		t.Errorf("estimated risk = %v, want 50", res.EstimatedRiskUSD)
	}
}

func TestComputePositionSizeMaxNotionalCap(t *testing.T) {
	// Razor-thin stop must not create huge notional: entry 100, SL 99.90 (0.1%)
	// risk $50 => raw qty 500 => notional $50k, capped to $10k.
	res, err := ComputePositionSize(SizingInput{
		RiskMode:               RiskModeByLoss,
		RiskAmountUSD:          50,
		EntryPrice:             100,
		StopLossPrice:          99.90,
		Leverage:               10,
		MaxPositionNotionalUSD: 10000,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if math.Abs(res.NotionalUSD-10000) > 1e-6 {
		t.Errorf("notional = %v, want 10000", res.NotionalUSD)
	}
	if len(res.AppliedConstraints) == 0 || res.AppliedConstraints[0] != "max_position_notional" {
		t.Errorf("expected max_position_notional constraint, got %v", res.AppliedConstraints)
	}
}

func TestComputePositionSizeRejectsTinyStopDistance(t *testing.T) {
	// entry 100, SL 99.99 => 0.01% distance, below the 0.05% floor.
	_, err := ComputePositionSize(SizingInput{
		RiskMode:      RiskModeByLoss,
		RiskAmountUSD: 50,
		EntryPrice:    100,
		StopLossPrice: 99.99,
		Leverage:      10,
	})
	if err == nil {
		t.Fatal("expected sizing floor rejection, got nil")
	}
}

func TestComputePositionSizeMarginCap(t *testing.T) {
	// fixed margin mode with insufficient balance
	res, err := ComputePositionSize(SizingInput{
		RiskMode:           RiskModeFixed,
		RiskAmountUSD:      1000, // wants $1000 margin
		EntryPrice:         100,
		Leverage:           10,
		AvailableMarginUSD: 100, // only $100 available
		MarginBufferPct:    0.9,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// capped qty = 100*0.9*10/100 = 9
	if math.Abs(res.FinalQuantity-9) > 1e-9 {
		t.Errorf("quantity = %v, want 9", res.FinalQuantity)
	}
}

func TestComputePositionSizePercentMode(t *testing.T) {
	res, err := ComputePositionSize(SizingInput{
		RiskMode:      RiskModePercent,
		RiskAmountUSD: 10, // 10% of equity
		EquityUSD:     5000,
		EntryPrice:    50,
		Leverage:      5,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// margin 500, notional 2500, qty 50
	if math.Abs(res.FinalQuantity-50) > 1e-9 {
		t.Errorf("quantity = %v, want 50", res.FinalQuantity)
	}
}

func TestEffectiveRisk(t *testing.T) {
	// signal entry 100 / SL 95 sized for $50; actual fill 101 => risk $60
	qty := 10.0
	risk := EffectiveRisk(qty, 101, 95)
	if math.Abs(risk-60) > 1e-9 {
		t.Errorf("effective risk = %v, want 60", risk)
	}
}

func TestDecideEntryType(t *testing.T) {
	tests := []struct {
		name        string
		spec        PriceSpec
		market      float64
		threshold   float64
		limitWithin bool
		wantType    EntryPlanType
		wantPrice   float64
	}{
		{"market within threshold", PriceSpec{Type: PriceMarket}, 100, 0.5, true, EntryPlanMarket, 100},
		{"fixed within threshold converts to market", PriceSpec{Type: PriceFixed, Price: 100}, 100.2, 0.3, true, EntryPlanMarket, 100.2},
		{"fixed within threshold stays limit when disabled", PriceSpec{Type: PriceFixed, Price: 100}, 100.2, 0.3, false, EntryPlanLimit, 100},
		{"fixed beyond threshold stays limit", PriceSpec{Type: PriceFixed, Price: 100}, 102, 0.3, true, EntryPlanLimit, 100},
		{"range inside is market", PriceSpec{Type: PriceRange, RangeLow: 61500, RangeHigh: 62000}, 61800, 1, true, EntryPlanMarket, 61800},
		{"range below limits at low edge", PriceSpec{Type: PriceRange, RangeLow: 61500, RangeHigh: 62000}, 61000, 1, true, EntryPlanLimit, 61500},
		{"range above limits at high edge", PriceSpec{Type: PriceRange, RangeLow: 61500, RangeHigh: 62000}, 63000, 1, true, EntryPlanLimit, 62000},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			planType, price, err := DecideEntryType(tt.spec, tt.market, tt.threshold, tt.limitWithin)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if planType != tt.wantType || math.Abs(price-tt.wantPrice) > 1e-9 {
				t.Errorf("got (%s, %v), want (%s, %v)", planType, price, tt.wantType, tt.wantPrice)
			}
		})
	}
}

func TestSanityCheckPrice(t *testing.T) {
	// OCR digit error: 62000 read as 6200 must be rejected
	if err := SanityCheckPrice("entry", 6200, 62000, DefaultSanityDeviationPct); err == nil {
		t.Error("expected sanity rejection for 10x digit error")
	}
	if err := SanityCheckPrice("entry", 61800, 62000, DefaultSanityDeviationPct); err != nil {
		t.Errorf("unexpected rejection for normal price: %v", err)
	}
}
