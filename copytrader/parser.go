package copytrader

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ParseInterpretation extracts and validates the SourceInterpretation JSON
// from a raw LLM response. Tolerates markdown fences and leading/trailing
// prose; rejects structurally invalid payloads.
func ParseInterpretation(raw string) (*SourceInterpretation, error) {
	jsonStr, err := extractJSONObject(raw)
	if err != nil {
		return nil, err
	}
	var si SourceInterpretation
	if err := json.Unmarshal([]byte(jsonStr), &si); err != nil {
		return nil, fmt.Errorf("interpretation JSON invalid: %w", err)
	}
	if err := normalizeInterpretation(&si); err != nil {
		return nil, err
	}
	return &si, nil
}

// extractJSONObject finds the first balanced top-level JSON object in text.
func extractJSONObject(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	// Strip common markdown fences.
	if idx := strings.Index(s, "```"); idx >= 0 {
		// Prefer the fenced block content when present.
		rest := s[idx+3:]
		rest = strings.TrimPrefix(rest, "json")
		if end := strings.Index(rest, "```"); end >= 0 {
			s = rest[:end]
		}
	}
	start := strings.Index(s, "{")
	if start < 0 {
		return "", fmt.Errorf("no JSON object found in AI response")
	}
	depth := 0
	inString := false
	escaped := false
	for i := start; i < len(s); i++ {
		c := s[i]
		if inString {
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			inString = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return s[start : i+1], nil
			}
		}
	}
	return "", fmt.Errorf("unbalanced JSON object in AI response")
}

// normalizeInterpretation canonicalizes enums and rejects invalid values.
func normalizeInterpretation(si *SourceInterpretation) error {
	si.Classification = Classification(strings.ToUpper(strings.TrimSpace(string(si.Classification))))
	switch si.Classification {
	case ClassificationSignal, ClassificationIgnore, ClassificationNeedsContext,
		ClassificationAmbiguous, ClassificationUnsupported:
	case "":
		return fmt.Errorf("missing classification")
	default:
		return fmt.Errorf("unknown classification %q", si.Classification)
	}

	// Multi-instruction message: every element inherits the message-level
	// classification and provenance, is normalized like a standalone
	// instruction, and may not nest further. The top-level per-trade fields
	// then act only as a summary for storage/UI.
	if len(si.Instructions) > 0 {
		symbols := make([]string, 0, len(si.Instructions))
		seen := map[string]bool{}
		for i, ins := range si.Instructions {
			if ins == nil {
				return fmt.Errorf("instructions[%d] is null", i)
			}
			ins.Instructions = nil
			ins.Classification = si.Classification
			ins.SourceInfo = si.SourceInfo
			if err := normalizeInstructionFields(ins); err != nil {
				return fmt.Errorf("instructions[%d]: %w", i, err)
			}
			if s := ins.Symbol; s != "" && !seen[s] {
				seen[s] = true
				symbols = append(symbols, s)
			}
		}
		if si.Action == "" {
			si.Action = si.Instructions[0].Action
		}
		if si.Symbol == "" {
			si.Symbol = strings.Join(symbols, ", ")
		}
	}

	return normalizeInstructionFields(si)
}

// normalizeInstructionFields canonicalizes the per-trade fields shared by the
// top level and every element of a multi-instruction message.
func normalizeInstructionFields(si *SourceInterpretation) error {
	si.Action = Action(strings.ToUpper(strings.TrimSpace(string(si.Action))))
	switch si.Action {
	case ActionOpen, ActionAdd, ActionReduce, ActionClose, ActionCancel,
		ActionUpdateSL, ActionUpdateTP, ActionIgnore:
	case "":
		if si.Classification == ClassificationSignal {
			return fmt.Errorf("SIGNAL without action")
		}
		si.Action = ActionIgnore
	default:
		return fmt.Errorf("unknown action %q", si.Action)
	}

	si.Direction = Direction(strings.ToUpper(strings.TrimSpace(string(si.Direction))))
	switch si.Direction {
	case DirectionLong, DirectionShort, "":
	case "BUY":
		si.Direction = DirectionLong
	case "SELL":
		si.Direction = DirectionShort
	default:
		return fmt.Errorf("unknown direction %q", si.Direction)
	}

	si.Symbol = strings.TrimSpace(si.Symbol)

	si.CloseMode = CloseMode(strings.ToUpper(strings.TrimSpace(string(si.CloseMode))))
	if si.CloseMode != "" && si.CloseMode != CloseModeFull && si.CloseMode != CloseModePartial {
		return fmt.Errorf("unknown close_mode %q", si.CloseMode)
	}
	if si.CloseRatio != nil && (*si.CloseRatio <= 0 || *si.CloseRatio > 100) {
		return fmt.Errorf("close_ratio %v out of range (0, 100]", *si.CloseRatio)
	}

	for i := range si.EntryOrders {
		si.EntryOrders[i].OrderType = EntryOrderType(strings.ToUpper(string(si.EntryOrders[i].OrderType)))
		if err := normalizePriceSpec(&si.EntryOrders[i].Price, "entry"); err != nil {
			return err
		}
	}
	for i := range si.TakeProfitLevels {
		if err := normalizePriceSpec(&si.TakeProfitLevels[i].Price, "take_profit"); err != nil {
			return err
		}
		if r := si.TakeProfitLevels[i].Ratio; r != nil && (*r <= 0 || *r > 100) {
			return fmt.Errorf("TP ratio %v out of range (0, 100]", *r)
		}
	}
	for i := range si.StopLossLevels {
		if err := normalizePriceSpec(&si.StopLossLevels[i].Price, "stop_loss"); err != nil {
			return err
		}
	}
	for i := range si.ConditionalRules {
		si.ConditionalRules[i].Action = Action(strings.ToUpper(string(si.ConditionalRules[i].Action)))
		si.ConditionalRules[i].Condition = ConditionType(strings.ToUpper(string(si.ConditionalRules[i].Condition)))
		if err := normalizePriceSpec(&si.ConditionalRules[i].Price, "conditional"); err != nil {
			return err
		}
	}
	return nil
}

func normalizePriceSpec(p *PriceSpec, field string) error {
	p.Type = PriceSpecType(strings.ToUpper(strings.TrimSpace(string(p.Type))))
	switch p.Type {
	case PriceFixed:
		if p.Price <= 0 {
			return fmt.Errorf("%s: FIXED price must be > 0", field)
		}
	case PriceRange:
		if p.RangeLow <= 0 || p.RangeHigh <= 0 {
			return fmt.Errorf("%s: RANGE requires both bounds", field)
		}
	case PriceMarket, PriceEntry, PriceBreakeven, PriceRMultiple, PricePercentOffset, PriceUnknown:
	case "":
		p.Type = PriceUnknown
	default:
		// Unknown spec types degrade to UNKNOWN (skip-unsupported later)
		// instead of failing the whole parse.
		p.Type = PriceUnknown
	}
	return nil
}
