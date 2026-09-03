package copytrader

import (
	"math"
	"testing"
)

func ratio(v float64) *float64 { return &v }

func TestAllocateTPRatios(t *testing.T) {
	defaults := []float64{50, 30, 20}
	tests := []struct {
		name   string
		levels []TPLevel
		want   []float64
	}{
		{"single unspecified => 100", []TPLevel{{}}, []float64{100}},
		{"two unspecified => 50/50", []TPLevel{{}, {}}, []float64{50, 50}},
		{"three unspecified => defaults", []TPLevel{{}, {}, {}}, []float64{50, 30, 20}},
		{"explicit wins over defaults", []TPLevel{{Ratio: ratio(25)}, {Ratio: ratio(75)}}, []float64{25, 75}},
		{"partial: TP1 25%, TP2 rest", []TPLevel{{Ratio: ratio(25)}, {}}, []float64{25, 75}},
		{"fully explicit below 100 kept (runner)", []TPLevel{{Ratio: ratio(30)}, {Ratio: ratio(30)}}, []float64{30, 30}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := AllocateTPRatios(tt.levels, defaults)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
			for i := range got {
				if math.Abs(got[i]-tt.want[i]) > 1e-9 {
					t.Errorf("level %d: got %v, want %v", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestAllocateTPRatiosErrors(t *testing.T) {
	if _, err := AllocateTPRatios([]TPLevel{{}, {}, {}, {}}, nil); err == nil {
		t.Error("expected error for >3 levels")
	}
	if _, err := AllocateTPRatios([]TPLevel{{Ratio: ratio(60)}, {Ratio: ratio(60)}}, nil); err == nil {
		t.Error("expected error for explicit sum > 100")
	}
}

func TestSplitTPQuantitiesNoDust(t *testing.T) {
	// The classic dust case: 0.007 filled, step 0.001, 50/30/20.
	// Naive rounding gives 0.003+0.002+0.001=0.006 leaving 0.001 dust.
	// Last level must absorb the remainder: 0.003+0.002+0.002=0.007.
	qtys, err := SplitTPQuantities(0.007, []float64{50, 30, 20}, 0.001, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []float64{0.003, 0.002, 0.002}
	sum := 0.0
	for i := range qtys {
		if math.Abs(qtys[i]-want[i]) > 1e-12 {
			t.Errorf("level %d: got %v, want %v", i, qtys[i], want[i])
		}
		sum += qtys[i]
	}
	if math.Abs(sum-0.007) > 1e-12 {
		t.Errorf("total = %v, want exactly 0.007", sum)
	}
}

func TestSplitTPQuantitiesSingleTP(t *testing.T) {
	qtys, err := SplitTPQuantities(0.325, []float64{100}, 0.001, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(qtys) != 1 || math.Abs(qtys[0]-0.325) > 1e-12 {
		t.Errorf("got %v, want [0.325]", qtys)
	}
}

func TestSplitTPQuantitiesRunnerKeepsRemainder(t *testing.T) {
	// Explicit 30/30 (runner 40% stays in position): total reduce = 60% of 1.0.
	qtys, err := SplitTPQuantities(1.0, []float64{30, 30}, 0.001, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	sum := qtys[0] + qtys[1]
	if math.Abs(sum-0.6) > 1e-9 {
		t.Errorf("reduce total = %v, want 0.6", sum)
	}
}

func TestSplitTPQuantitiesMergesSubMinimum(t *testing.T) {
	// step 0.1, min 0.5: 1.0 split 50/30/20 => 0.5/0.3/0.2, levels 2-3 below min.
	qtys, err := SplitTPQuantities(1.0, []float64{50, 30, 20}, 0.1, 0.5)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	sum := 0.0
	for _, q := range qtys {
		if q > 0 && q < 0.5 {
			t.Errorf("level quantity %v below minQty 0.5", q)
		}
		sum += q
	}
	if math.Abs(sum-1.0) > 1e-9 {
		t.Errorf("total = %v, want 1.0", sum)
	}
}
