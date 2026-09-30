package copytrader

import "encoding/json"

// Normalize execution meaning, not model formatting. Empty and missing lists
// are equivalent; ENTRY and BREAKEVEN currently share execution semantics.
func normalizedPrice(p PriceSpec) PriceSpec {
	switch p.Type {
	case PriceBreakeven, PriceEntry:
		return PriceSpec{Type: PriceEntry}
	case PriceFixed, PriceMarket:
		return PriceSpec{Type: p.Type, Price: p.Price}
	case PriceRange:
		return PriceSpec{Type: p.Type, RangeLow: p.RangeLow, RangeHigh: p.RangeHigh}
	case PriceTPLevel:
		return PriceSpec{Type: p.Type, Level: p.Level}
	default:
		return p
	}
}
func instructionSemantic(ins *SourceInterpretation) string {
	if ins.Action != ActionUpdateSL && ins.Action != ActionUpdateTP {
		return ""
	}
	normalized := struct {
		SL    []PriceSpec       `json:"sl,omitempty"`
		TP    []TPLevel         `json:"tp,omitempty"`
		Rules []ConditionalRule `json:"rules,omitempty"`
	}{}
	if ins.Action == ActionUpdateSL {
		for _, s := range ins.StopLossLevels {
			normalized.SL = append(normalized.SL, normalizedPrice(s.Price))
		}
	}
	if ins.Action == ActionUpdateTP {
		for _, t := range ins.TakeProfitLevels {
			t.Price = normalizedPrice(t.Price)
			t.RatioSource = ""
			normalized.TP = append(normalized.TP, t)
		}
	}
	for _, r := range ins.ConditionalRules {
		r.Price = normalizedPrice(r.Price)
		if r.Condition == ConditionTPFilled && r.ConditionLevel <= 0 {
			r.ConditionLevel = 1
		}
		normalized.Rules = append(normalized.Rules, r)
	}
	b, _ := json.Marshal(normalized)
	return string(b)
}
