package types

import "testing"

func TestFormatBaseQuantityByContract(t *testing.T) {
	tests := []struct {
		name         string
		quantity     float64
		contractSize float64
		lotSize      float64
		want         string
	}{
		// The OKX incident: 0.050175615 BTC, ctVal=0.01, lotSz=0.01
		// => step 0.0001 BTC, floor to 0.0501 — NOT "5.02" contracts.
		{"okx BTC swap incident", 0.050175615, 0.01, 0.01, "0.0501"},
		{"okx exact multiple", 0.05, 0.01, 0.01, "0.0500"},
		{"okx lot size 1", 0.0502, 0.01, 1, "0.05"},
		// KuCoin/Gate style: integer lots, multiplier only.
		{"kucoin multiplier 0.001", 2.5678, 0.001, 1, "2.567"},
		{"gate quanto 0.0001", 0.050175615, 0.0001, 1, "0.0501"},
		// Below one step formats to zero (caller rejects zero).
		{"below one step", 0.00004, 0.01, 0.01, "0.0000"},
		{"zero quantity", 0, 0.01, 1, "0.00"},
		{"negative clamps to zero", -1, 0.01, 1, "0.00"},
		// Whole-unit contracts.
		{"step of 1", 42.9, 1, 1, "42"},
		{"step of 10", 129, 10, 1, "120"},
		// Float artifact: 0.1 * 0.1 must behave as step 0.01.
		{"float artifact step", 0.57, 0.1, 0.1, "0.57"},
		// Missing metadata falls back to a step of 1.
		{"zero contract size", 3.7, 0, 0, "3"},
		// Value sitting a hair below a step boundary due to float error
		// must not lose a step (epsilon in the floor).
		{"epsilon at boundary", 0.3, 0.1, 1, "0.3"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := FormatBaseQuantityByContract(tt.quantity, tt.contractSize, tt.lotSize)
			if got != tt.want {
				t.Errorf("FormatBaseQuantityByContract(%v, %v, %v) = %q, want %q",
					tt.quantity, tt.contractSize, tt.lotSize, got, tt.want)
			}
		})
	}
}
