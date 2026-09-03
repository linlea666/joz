// Package copytrader implements Discord channel copy-trading:
// signal interpretation schema, trade lifecycle state machine,
// deterministic risk engine and idempotent execution planning.
//
// Core boundary (must hold across the package):
//   - The AI is only responsible for understanding what the channel author
//     said (SourceInterpretation). It never computes quantities and never
//     bypasses risk rules.
//   - Historical messages are only used to complete parameters and correlate
//     trades; they never trigger execution by themselves.
//   - The database stores state; the exchange is the final source of truth
//     for account facts.
package copytrader

import "time"

// Classification is the top-level result of AI source interpretation.
type Classification string

const (
	ClassificationSignal       Classification = "SIGNAL"        // actionable trading instruction
	ClassificationIgnore       Classification = "IGNORE"        // chat / analysis / result posting, no action
	ClassificationNeedsContext Classification = "NEEDS_CONTEXT" // could be actionable but target trade cannot be determined
	ClassificationAmbiguous    Classification = "AMBIGUOUS"     // conflicting or unclear semantics, do not execute
	ClassificationUnsupported  Classification = "UNSUPPORTED"   // understood but not executable (instrument/price spec/...)
)

// Action is what the channel author wants done.
type Action string

const (
	ActionOpen     Action = "OPEN"
	ActionAdd      Action = "ADD"
	ActionReduce   Action = "REDUCE"
	ActionClose    Action = "CLOSE"
	ActionCancel   Action = "CANCEL" // cancel pending (unfilled) entry
	ActionUpdateSL Action = "UPDATE_SL"
	ActionUpdateTP Action = "UPDATE_TP"
	ActionIgnore   Action = "IGNORE"
)

// Direction of a trade.
type Direction string

const (
	DirectionLong  Direction = "LONG"
	DirectionShort Direction = "SHORT"
)

// CloseMode distinguishes full vs partial close instructions.
type CloseMode string

const (
	CloseModeFull    CloseMode = "FULL"
	CloseModePartial CloseMode = "PARTIAL"
)

// PriceSpecType describes how a price is expressed by the source.
// V1 executes FIXED / MARKET / ENTRY / BREAKEVEN / RANGE; other types are
// parsed and recorded but skipped as unsupported (no schema change needed later).
type PriceSpecType string

const (
	PriceFixed         PriceSpecType = "FIXED"          // absolute price
	PriceMarket        PriceSpecType = "MARKET"         // current market price (CMP)
	PriceEntry         PriceSpecType = "ENTRY"          // the trade's entry price ("SL to entry")
	PriceBreakeven     PriceSpecType = "BREAKEVEN"      // breakeven (~entry, incl. fees) — treated as entry in V1
	PriceRange         PriceSpecType = "RANGE"          // price zone, e.g. "62000-61500"
	PriceRMultiple     PriceSpecType = "R_MULTIPLE"     // e.g. "TP at 2R" (V1: skip-unsupported)
	PricePercentOffset PriceSpecType = "PERCENT_OFFSET" // e.g. "move SL +0.2%" (V1: skip-unsupported)
	PriceUnknown       PriceSpecType = "UNKNOWN"
)

// PriceSpec is a structured price expression.
type PriceSpec struct {
	Type      PriceSpecType `json:"type"`
	Price     float64       `json:"price,omitempty"`      // FIXED
	RangeLow  float64       `json:"range_low,omitempty"`  // RANGE (lower bound)
	RangeHigh float64       `json:"range_high,omitempty"` // RANGE (upper bound)
	Offset    float64       `json:"offset,omitempty"`     // R_MULTIPLE multiplier or percent offset
}

// EntryOrderType is how the entry should be submitted.
type EntryOrderType string

const (
	EntryMarket EntryOrderType = "MARKET"
	EntryLimit  EntryOrderType = "LIMIT"
)

// EntryOrder is one entry leg of an OPEN/ADD instruction.
type EntryOrder struct {
	OrderType EntryOrderType `json:"order_type"`
	Price     PriceSpec      `json:"price"`
}

// TPLevel is one take-profit target.
// Ratio is the position percentage to close at this level (0-100).
// nil means the author did not specify a ratio; the deterministic TP policy
// decides the final allocation (never the AI).
type TPLevel struct {
	Price       PriceSpec `json:"price"`
	Ratio       *float64  `json:"ratio,omitempty"`
	RatioSource string    `json:"ratio_source,omitempty"` // "explicit" | "unspecified"
}

// SLLevel is a stop-loss instruction.
// Conditional carries soft-stop semantics verbatim (e.g. "2h close under"),
// V1 executes the hard price and records a warning for conditional stops.
type SLLevel struct {
	Price       PriceSpec `json:"price"`
	Conditional string    `json:"conditional,omitempty"`
}

// ConditionType for conditional follow-up rules stated by the author.
type ConditionType string

const (
	ConditionTPFilled ConditionType = "TP_FILLED"
)

// ConditionalRule captures instructions like "after TP1, move SL to breakeven".
// Author-stated rules take priority over the trader's local defaults.
type ConditionalRule struct {
	Condition      ConditionType `json:"condition"`
	ConditionLevel int           `json:"condition_level,omitempty"` // e.g. TP level index (1-based)
	Action         Action        `json:"action"`
	Price          PriceSpec     `json:"price"`
}

// TradeReference correlates the message to an existing trade.
type TradeReference struct {
	RootMessageID  string  `json:"root_message_id,omitempty"`  // anchor message of the trade
	ReplyMessageID string  `json:"reply_message_id,omitempty"` // direct reply / linked message id
	Confidence     float64 `json:"confidence,omitempty"`
}

// SourceInfo records provenance about what the AI actually consumed.
type SourceInfo struct {
	HasImage         bool `json:"has_image"`
	ImageCount       int  `json:"image_count"`
	TextPriorityUsed bool `json:"text_priority_used"` // text params took priority over image content
	UsedLinkedMsg    bool `json:"used_linked_message"`
	UsedTradeContext bool `json:"used_trade_context"`
}

// SourceInterpretation is the standard output of AI signal parsing.
// It answers exactly one question: "what did the author say?".
type SourceInterpretation struct {
	Classification Classification `json:"classification"`
	Action         Action         `json:"action"`
	Symbol         string         `json:"symbol,omitempty"`    // raw symbol as stated ("BTC", "NQ", "BTC/USDT")
	Direction      Direction      `json:"direction,omitempty"` // LONG / SHORT

	CloseMode    CloseMode `json:"close_mode,omitempty"`
	CloseRatio   *float64  `json:"close_ratio,omitempty"`    // PARTIAL close percentage (0-100)
	CloseTPLevel *int      `json:"close_tp_level,omitempty"` // "TP2 hit" style partial closes

	EntryOrders      []EntryOrder      `json:"entry_orders,omitempty"`
	TakeProfitLevels []TPLevel         `json:"take_profit_levels,omitempty"`
	StopLossLevels   []SLLevel         `json:"stop_loss_levels,omitempty"`
	ConditionalRules []ConditionalRule `json:"conditional_rules,omitempty"`

	TradeReference TradeReference     `json:"trade_reference,omitempty"`
	Confidence     map[string]float64 `json:"confidence,omitempty"` // classification/symbol/direction/entry/stop_loss
	Reasoning      string             `json:"reasoning,omitempty"`
	Warnings       []string           `json:"warnings,omitempty"`
	SourceInfo     SourceInfo         `json:"source_info"`

	// Instructions carries the per-trade instructions of a multi-instruction
	// message (one post managing several tracked trades, e.g. "SEI SL to BE,
	// SUI SL to BE"). Each element uses the same per-trade fields as the top
	// level; classification/reasoning/source_info stay message-level (the
	// parser copies them onto every element). Empty for single-instruction
	// messages, whose per-trade fields live directly on the top level.
	Instructions []*SourceInterpretation `json:"instructions,omitempty"`
}

// IsActionable reports whether the interpretation should enter the
// decision/execution pipeline at all.
func (si *SourceInterpretation) IsActionable() bool {
	if si == nil {
		return false
	}
	if len(si.Instructions) > 0 {
		for _, ins := range si.Instructions {
			if ins.IsActionable() {
				return true
			}
		}
		return false
	}
	return si.Classification == ClassificationSignal && si.Action != ActionIgnore && si.Action != ""
}

// Flatten returns the executable instruction views of this interpretation:
// the instructions themselves for a multi-instruction message, or the
// interpretation itself for the classic single-instruction shape. Every
// element is a self-contained *SourceInterpretation the validation and
// routing layers can consume unchanged.
func (si *SourceInterpretation) Flatten() []*SourceInterpretation {
	if si == nil {
		return nil
	}
	if len(si.Instructions) == 0 {
		return []*SourceInterpretation{si}
	}
	return si.Instructions
}

// SkipReason are terminal non-error outcomes of processing a signal.
// They are recorded on the signal record; none of them is an execution error.
type SkipReason string

const (
	SkipNone                  SkipReason = ""
	SkipNotSignal             SkipReason = "NOT_SIGNAL"
	SkipNeedsContext          SkipReason = "NEEDS_CONTEXT"
	SkipAmbiguous             SkipReason = "AMBIGUOUS"
	SkipUnsupportedInstrument SkipReason = "UNSUPPORTED_INSTRUMENT"
	SkipUnsupportedPriceSpec  SkipReason = "UNSUPPORTED_PRICE_SPEC"
	SkipNoPosition            SkipReason = "SKIPPED_NO_POSITION"
	SkipAlreadyFlat           SkipReason = "NOOP_ALREADY_FLAT"
	SkipDuplicate             SkipReason = "DUPLICATE_PROTECTION"
	SkipExpired               SkipReason = "SIGNAL_EXPIRED"
	SkipSanityCheck           SkipReason = "SANITY_CHECK_FAILED"
	SkipPaused                SkipReason = "TRADING_PAUSED"
	SkipRiskRejected          SkipReason = "RISK_REJECTED"
	SkipMaxPositions          SkipReason = "MAX_POSITIONS_REACHED"
)

// IsExpired reports whether a signal is too old to act on.
// OPEN signals must use a strict TTL; management signals a looser one.
func IsExpired(messageTime, now time.Time, ttl time.Duration) bool {
	if ttl <= 0 {
		return false
	}
	return now.Sub(messageTime) > ttl
}
