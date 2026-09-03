package copytrader

import (
	"strings"
	"testing"
)

// Representative LLM outputs replayed through the parser — one per real
// message style observed in the reference channels.
func TestParseInterpretation_OpenSignal(t *testing.T) {
	raw := "```json\n" + `{
		"classification": "signal",
		"action": "open",
		"symbol": "BTC",
		"direction": "long",
		"entry_orders": [
			{"order_type": "limit", "price": {"type": "fixed", "price": 77005.1}}
		],
		"take_profit_levels": [
			{"level": 1, "price": {"type": "fixed", "price": 77774}}
		],
		"stop_loss_levels": [
			{"price": {"type": "fixed", "price": 76851.3}}
		],
		"confidence": {"overall": 0.95},
		"source_info": {"message_id": "m1", "channel_id": "c1"}
	}` + "\n```"

	si, err := ParseInterpretation(raw)
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	if si.Classification != ClassificationSignal {
		t.Fatalf("classification = %s", si.Classification)
	}
	if si.Action != ActionOpen || si.Direction != DirectionLong {
		t.Fatalf("action/direction = %s/%s", si.Action, si.Direction)
	}
	if len(si.EntryOrders) != 1 || si.EntryOrders[0].Price.Price != 77005.1 {
		t.Fatalf("entry orders wrong: %+v", si.EntryOrders)
	}
	if si.EntryOrders[0].Price.Type != PriceFixed {
		t.Fatalf("price type not normalized: %s", si.EntryOrders[0].Price.Type)
	}
	if !si.IsActionable() {
		t.Fatal("open signal should be actionable")
	}
}

func TestParseInterpretation_WithLeadingProse(t *testing.T) {
	raw := `Based on the message, this is a close instruction.
{"classification": "SIGNAL", "action": "CLOSE", "symbol": "ETHUSDT",
 "close_mode": "full", "source_info": {"message_id": "m2"}}
Hope this helps!`

	si, err := ParseInterpretation(raw)
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	if si.Action != ActionClose || si.CloseMode != CloseModeFull {
		t.Fatalf("close parse wrong: %+v", si)
	}
}

func TestParseInterpretation_NonSignal(t *testing.T) {
	raw := `{"classification": "ignore", "reasoning": "market commentary only", "source_info": {}}`
	si, err := ParseInterpretation(raw)
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	if si.Classification != ClassificationIgnore {
		t.Fatalf("classification = %s", si.Classification)
	}
	if si.Action != ActionIgnore {
		t.Fatalf("empty action should default to IGNORE, got %s", si.Action)
	}
	if si.IsActionable() {
		t.Fatal("ignore must not be actionable")
	}
}

func TestParseInterpretation_BuySellNormalization(t *testing.T) {
	raw := `{"classification": "SIGNAL", "action": "OPEN", "symbol": "SOL",
		"direction": "buy",
		"entry_orders": [{"order_type": "market", "price": {"type": "market"}}],
		"stop_loss_levels": [{"price": {"type": "fixed", "price": 100}}],
		"source_info": {}}`
	si, err := ParseInterpretation(raw)
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	if si.Direction != DirectionLong {
		t.Fatalf("BUY should normalize to LONG, got %s", si.Direction)
	}
}

func TestParseInterpretation_UnknownPriceTypeDegrades(t *testing.T) {
	raw := `{"classification": "SIGNAL", "action": "UPDATE_SL", "symbol": "BTC",
		"stop_loss_levels": [{"price": {"type": "fib_0618"}}],
		"source_info": {}}`
	si, err := ParseInterpretation(raw)
	if err != nil {
		t.Fatalf("unknown price type must not fail the parse: %v", err)
	}
	if si.StopLossLevels[0].Price.Type != PriceUnknown {
		t.Fatalf("expected UNKNOWN degrade, got %s", si.StopLossLevels[0].Price.Type)
	}
}

func TestParseInterpretation_Rejections(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"no json", "sorry I cannot help", "no JSON object"},
		{"unbalanced", `{"classification": "SIGNAL"`, "unbalanced"},
		{"missing classification", `{"action": "OPEN"}`, "missing classification"},
		{"unknown classification", `{"classification": "MAYBE"}`, "unknown classification"},
		{"signal without action", `{"classification": "SIGNAL"}`, "SIGNAL without action"},
		{"bad close ratio", `{"classification": "SIGNAL", "action": "REDUCE", "close_ratio": 150}`, "close_ratio"},
		{"fixed without price", `{"classification": "SIGNAL", "action": "OPEN",
			"entry_orders": [{"order_type": "limit", "price": {"type": "fixed"}}]}`, "FIXED price"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseInterpretation(tc.raw)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not contain %q", err.Error(), tc.want)
			}
		})
	}
}

// The trader-bamp style: one post managing several tracked trades
// ("SEI - SL to breakeven / SUI - SL to breakeven / BTC - letting it run").
func TestParseInterpretation_MultiInstruction(t *testing.T) {
	raw := `{"classification": "SIGNAL",
		"reasoning": "status update moving two stops to breakeven",
		"source_info": {"has_image": true, "image_count": 1},
		"instructions": [
			{"action": "update_sl", "symbol": "SEI", "direction": "long",
			 "stop_loss_levels": [{"price": {"type": "breakeven"}}]},
			{"action": "UPDATE_SL", "symbol": "SUI", "direction": "long",
			 "stop_loss_levels": [{"price": {"type": "breakeven"}}]}
		]}`
	si, err := ParseInterpretation(raw)
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	if len(si.Instructions) != 2 {
		t.Fatalf("expected 2 instructions, got %d", len(si.Instructions))
	}
	if !si.IsActionable() {
		t.Fatal("multi-instruction signal should be actionable")
	}
	flat := si.Flatten()
	if len(flat) != 2 {
		t.Fatalf("Flatten should return the 2 instructions, got %d", len(flat))
	}
	for i, ins := range flat {
		if ins.Classification != ClassificationSignal {
			t.Fatalf("instruction %d must inherit classification, got %s", i, ins.Classification)
		}
		if !ins.SourceInfo.HasImage {
			t.Fatalf("instruction %d must inherit source_info", i)
		}
		if ins.Action != ActionUpdateSL {
			t.Fatalf("instruction %d action = %s", i, ins.Action)
		}
		if ins.StopLossLevels[0].Price.Type != PriceBreakeven {
			t.Fatalf("instruction %d SL type = %s", i, ins.StopLossLevels[0].Price.Type)
		}
	}
	// Top level becomes a storage/UI summary.
	if si.Action != ActionUpdateSL {
		t.Fatalf("summary action = %s", si.Action)
	}
	if si.Symbol != "SEI, SUI" {
		t.Fatalf("summary symbol = %q", si.Symbol)
	}
}

func TestParseInterpretation_MultiInstructionRejections(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"instruction without action",
			`{"classification": "SIGNAL", "instructions": [{"symbol": "SEI"}]}`,
			"SIGNAL without action"},
		{"null instruction",
			`{"classification": "SIGNAL", "instructions": [null]}`,
			"is null"},
		{"bad instruction field",
			`{"classification": "SIGNAL", "instructions": [
				{"action": "REDUCE", "symbol": "SEI", "close_ratio": 150}]}`,
			"close_ratio"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseInterpretation(tc.raw)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not contain %q", err.Error(), tc.want)
			}
		})
	}
}

func TestParseInterpretation_SingleInstructionFlatten(t *testing.T) {
	raw := `{"classification": "SIGNAL", "action": "CLOSE", "symbol": "BTC", "source_info": {}}`
	si, err := ParseInterpretation(raw)
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	flat := si.Flatten()
	if len(flat) != 1 || flat[0] != si {
		t.Fatalf("single-instruction Flatten must return the interpretation itself")
	}
}

func TestParseInterpretation_NestedBracesInReasoning(t *testing.T) {
	raw := `{"classification": "SIGNAL", "action": "CLOSE", "symbol": "BTC",
		"reasoning": "author wrote {closed at breakeven} with a \" quote",
		"source_info": {}}`
	si, err := ParseInterpretation(raw)
	if err != nil {
		t.Fatalf("nested braces inside strings broke extraction: %v", err)
	}
	if si.Action != ActionClose {
		t.Fatalf("action = %s", si.Action)
	}
}
