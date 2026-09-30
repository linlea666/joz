package copytrader

import (
	"encoding/json"
	"fmt"
	"math"
	"nofx/store"
	"regexp"
	"strconv"
	"strings"
)

const MessageRulesVersion = 2

type MessageRules struct {
	EntryPolicy   string  `json:"entry_policy,omitempty"`
	Version       int     `json:"version"`
	Profile       string  `json:"interpretation_profile"`
	Notes         string  `json:"channel_notes"`
	DualPriceMode string  `json:"market_dual_price_mode"`
	ReduceRatio   float64 `json:"default_reduce_ratio"`
}

func (c *CopyTradingConfig) MessageRules() MessageRules {
	policy := c.EntryPolicy
	if policy == "" {
		policy = EntryPolicyLegacy
	}
	profile := c.InterpretationProfile
	if profile == "" {
		profile = "default"
	}
	return MessageRules{Version: MessageRulesVersion, Profile: profile, Notes: c.ChannelNotes, DualPriceMode: c.MarketDualPriceMode, ReduceRatio: c.ReduceRatio(), EntryPolicy: policy}
}
func (r MessageRules) Snapshot() string { b, _ := json.Marshal(r); return string(b) }
func (e *Engine) rulesForSignal(signalID string) (MessageRules, error) {
	r := e.cfg.MessageRules()
	if signalID != "" {
		sig, err := e.st.CopyTrade().GetSignal(signalID)
		if err != nil {
			return r, fmt.Errorf("load message rule snapshot: %w", err)
		}
		if sig != nil && sig.RulesSnapshotJSON == "" {
			return MessageRules{}, fmt.Errorf("historical signal has no rule snapshot; cannot safely reinterpret or submit")
		}
		if sig != nil && sig.RulesSnapshotJSON != "" {
			r = MessageRules{}
			if err = json.Unmarshal([]byte(sig.RulesSnapshotJSON), &r); err != nil {
				return r, err
			}
			if (r.Version != 1 && r.Version != MessageRulesVersion) || !finite(r.ReduceRatio) || r.ReduceRatio <= 0 || r.ReduceRatio > 100 {
				return r, fmt.Errorf("unsupported or invalid message rule snapshot")
			}
		}
	}
	if _, ok := LookupInterpretationPreset(r.Profile); !ok {
		return r, fmt.Errorf("unknown interpretation preset %q", r.Profile)
	}
	switch r.DualPriceMode {
	case "", DualPriceLegacy, DualPriceRange, DualPriceSplit, DualPriceReject:
	default:
		return r, fmt.Errorf("invalid frozen dual-price mode")
	}
	if r.Version >= 2 && r.EntryPolicy != EntryPolicyLegacy && r.EntryPolicy != EntryPolicySplit {
		return r, fmt.Errorf("invalid frozen entry policy")
	}
	return r, nil
}

var dualMarketPrices = regexp.MustCompile(`(?:進場|进场|入場|入场)\s*[:：]?\s*市[價价]\s*([0-9]+(?:\.[0-9]+)?)\s*(?:附近)?\s*[—–－-]\s*([0-9]+(?:\.[0-9]+)?)`)
var explicitEntryRange = regexp.MustCompile(`(?:區間|区间|(?i:range))`)
var explicitEntryTwo = regexp.MustCompile(`(?:兩筆|两笔|分批|(?i:two orders))`)
var authorPositionParameters = regexp.MustCompile(`(?i)^\s*[0-9]+(?:\.[0-9]+)?\s*[%％]\s*(?:[～~—–-]\s*[0-9]+(?:\.[0-9]+)?\s*[%％])?\s*[倉仓]位\s*(?:[｜|,，]\s*[0-9]+(?:\s*[～~—–-]\s*[0-9]+)?\s*(?:x|倍))?\s*$`)

// ApplyMessageRules resolves only current, symbol-scoped author text. Reference
// cards and performance recaps never gain opening authority through a setting.
func ApplyMessageRules(interp *SourceInterpretation, msg *store.DiscordMessage, sources []SourceSegment, rules MessageRules) *SourceInterpretation {
	if interp == nil {
		return applyPresetPolicy(nil, msg, sources, rules)
	}
	for _, ins := range interp.Flatten() {
		if ins.Action == ActionClose && ins.CloseMode == CloseModePartial {
			ins.Action = ActionReduce
		}
		if ins.Action == ActionReduce {
			ins.CloseMode = CloseModePartial
		}
		var scope string
		for _, s := range sources {
			if s.Role == "current" && !s.Image {
				text := evidenceScopeForSymbol(s.Text, ins.Symbol)
				if text != "" {
					scope += "\n" + text
				}
			}
		}
		if scope == "" {
			if ins.Action == ActionReduce {
				applyReductionRatio(ins, scope, sources, rules.ReduceRatio)
			}
			continue
		}
		if ins.Action == ActionReduce {
			applyReductionRatio(ins, scope, sources, rules.ReduceRatio)
		}

		if ins.Action == ActionClose && scope != "" {
			v, explicit, err := explicitReductionRatio(scope)
			if err != nil || (explicit && v < 100 && ins.CloseMode != CloseModePartial) {
				ins.Classification = ClassificationAmbiguous
				ins.Reasoning = "full close conflicts with explicit partial-exit evidence"
			} else if explicit {
				ins.CloseRatio = &v
			}
		}

		if ins.Action == ActionReduce || ins.Action == ActionClose || ins.Action == ActionUpdateSL {
			level, ambiguous := requiredTPFill(scope)
			if ambiguous || strings.Contains(scope, "倉位重") || strings.Contains(scope, "仓位重") {
				ins.Classification = ClassificationNeedsContext
			}
			if level > 0 {
				if ins.Action == ActionReduce || ins.Action == ActionClose {
					ins.RequiresTPFill = level
				}
				if ins.Action == ActionUpdateSL {
					for i := range ins.ConditionalRules {
						if ins.ConditionalRules[i].Condition == ConditionTPFilled {
							ins.ConditionalRules[i].ConditionLevel = level
						}
					}
				}
			}
		}

		if ins.Action != ActionOpen && ins.Action != ActionAdd {
			continue
		}
		filtered := make([]string, 0, len(ins.EligibilityConditions))
		for _, c := range ins.EligibilityConditions {
			if !authorPositionParameters.MatchString(c) {
				filtered = append(filtered, c)
			}
		}
		hadConditions := len(ins.EligibilityConditions) > 0
		ins.EligibilityConditions = filtered
		if hadConditions && len(filtered) == 0 && ins.Classification == ClassificationNeedsContext && len(ins.EntryOrders) > 0 && len(ins.StopLossLevels) > 0 && len(ins.TakeProfitLevels) > 0 && ins.Direction != "" {
			ins.Classification = ClassificationSignal
			normalizeActionEvidence(ins, sources)
		}
		if len(ins.EntryOrders) > 2 {
			ins.Classification = ClassificationUnsupported
			ins.Reasoning = "unsupported entry combination; no legs discarded"
			continue
		}
		mode := rules.DualPriceMode
		// Explicit author meaning precedes the ambiguity preference, including
		// legacy mode. An AI range/two-leg misreading cannot override the text.
		explicit := false
		for _, line := range strings.Split(scope, "\n") {
			if !dualMarketPrices.MatchString(line) {
				continue
			}
			isRange, isTwo := explicitEntryRange.MatchString(line), explicitEntryTwo.MatchString(line)
			if isRange && isTwo {
				mode = DualPriceReject
				explicit = true
			} else if isRange {
				mode = DualPriceRange
				explicit = true
			} else if isTwo {
				mode = DualPriceSplit
				explicit = true
			}
		}
		if !explicit && (mode == "" || mode == DualPriceLegacy) {
			continue
		}
		matches := dualMarketPrices.FindAllStringSubmatch(scope, -1)
		if len(matches) != 1 || terminalStatusBlocksOpen(scope) || sourceNegation.MatchString(scope) {
			continue
		}
		a, _ := strconv.ParseFloat(matches[0][1], 64)
		b, _ := strconv.ParseFloat(matches[0][2], 64)
		if a <= 0 || b <= 0 || !finite(a) || !finite(b) {
			continue
		}
		switch mode {
		case DualPriceReject:
			ins.Classification = ClassificationAmbiguous
			ins.EntryOrders = nil
		case DualPriceRange:
			ins.EntryOrders = []EntryOrder{{OrderType: EntryLimit, Price: PriceSpec{Type: PriceRange, RangeLow: math.Min(a, b), RangeHigh: math.Max(a, b)}}}
		case DualPriceSplit:
			ins.EntryOrders = []EntryOrder{{OrderType: EntryMarket, Price: PriceSpec{Type: PriceMarket, Price: a}}, {OrderType: EntryLimit, Price: PriceSpec{Type: PriceFixed, Price: b}}}
		}
		if mode != DualPriceReject && ins.Classification == ClassificationAmbiguous && len(ins.StopLossLevels) > 0 && len(ins.TakeProfitLevels) > 0 && ins.Direction != "" && len(ins.EligibilityConditions) == 0 {
			ins.Classification = ClassificationSignal
		}
		normalizeActionEvidence(ins, sources)
		ins.Warnings = append(ins.Warnings, "dual-price semantics: "+mode)
	}
	return applyPresetPolicy(interp, msg, sources, rules)
}

var tpAfterEnglish = regexp.MustCompile(`(?i)after\s+(?:a\s+)?TP\s*([1-9][0-9]*)`)
var tpAfterChinese = regexp.MustCompile(`(?i)TP\s*([1-9][0-9]*)[^\n，,;。]{0,20}[後后]`)

func requiredTPFill(scope string) (int, bool) {
	for _, pattern := range []*regexp.Regexp{tpAfterEnglish, tpAfterChinese} {
		if m := pattern.FindStringSubmatch(scope); len(m) > 1 {
			n, _ := strconv.Atoi(m[1])
			return n, false
		}
	}
	if !(strings.Contains(scope, "止盈後") || strings.Contains(scope, "止盈后")) {
		return 0, false
	}
	ids := map[int]bool{}
	for _, m := range sourceTPLevel.FindAllStringSubmatch(scope, -1) {
		n, _ := strconv.Atoi(m[1])
		ids[n] = true
	}
	if len(ids) == 1 {
		for n := range ids {
			return n, false
		}
	}
	return 0, true
}
