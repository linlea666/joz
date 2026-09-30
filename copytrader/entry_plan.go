package copytrader

import (
	"encoding/json"
	"fmt"
)

type resolvedEntryPlan struct {
	SourceOrderType EntryOrderType `json:"source_order_type"`
	Decision        entryDecision  `json:"decision"`
	Origin          string         `json:"origin"`
	SplitReference  float64        `json:"second_limit,omitempty"`
	PolicyNote      string         `json:"policy_note"`
}

// Resolve the complete shape once. Explicit legs and the optional single-
// reference strategy are mutually exclusive branches, not successive rewrites.
func resolveEntryPlan(direction Direction, orders []EntryOrder, market, threshold float64, convert bool, policy string) (resolvedEntryPlan, SkipReason, error) {
	var p resolvedEntryPlan
	if len(orders) == 0 {
		return p, SkipUnsupportedPriceSpec, fmt.Errorf("entry orders missing")
	}
	if len(orders) > 1 && (len(orders) != 2 || orders[0].OrderType != EntryMarket || orders[0].Price.Type != PriceMarket || orders[1].OrderType != EntryLimit || orders[1].Price.Type != PriceFixed || !finite(orders[1].Price.Price) || orders[1].Price.Price <= 0) {
		return p, SkipUnsupportedPriceSpec, fmt.Errorf("unsupported entry combination; no legs submitted")
	}
	p.SourceOrderType = orders[0].OrderType
	d, err := decideEntry(direction, orders[0].Price, market, threshold, convert)
	p.Decision = d
	if err != nil {
		return p, SkipUnsupportedPriceSpec, err
	}
	if len(orders) == 2 {
		if d.OrderType != EntryPlanMarket {
			return p, SkipRiskRejected, fmt.Errorf("explicit first market leg failed admission; no fallback combination")
		}
		p.Origin = "explicit_market_limit"
		p.SplitReference = orders[1].Price.Price
		p.PolicyNote = "explicit legs take precedence; single-reference policy not applied"
		return p, SkipNone, nil
	}
	p.Origin = "single"
	p.PolicyNote = "single-reference split not applicable to this order type"
	if orders[0].OrderType == EntryMarket && orders[0].Price.Type == PriceMarket && orders[0].Price.Price > 0 {
		if policy == "" && d.Reason == "adverse_within_threshold" {
			return p, SkipNeedsContext, fmt.Errorf("legacy signal lacks frozen single-reference policy; no entry resubmission")
		}
		p.PolicyNote = "single-reference policy: " + policy + "; " + d.Reason
		if policy == "" {
			p.PolicyNote = "legacy policy unknown; both policies produce the same " + d.Reason + " result"
		}
		if policy == EntryPolicySplit && d.Reason == "adverse_within_threshold" {
			p.Origin = "single_reference_split"
			p.SplitReference = orders[0].Price.Price
		}
	} else if orders[0].Price.Type == PriceRange {
		p.Origin = "range"
		p.PolicyNote = "range remains one order; single-reference policy not applied"
	}
	return p, SkipNone, nil
}

func (x *Executor) logEntryPlan(traceID, signalID string, p *OpenPlan, legs []map[string]interface{}) {
	var shape resolvedEntryPlan
	_ = json.Unmarshal(p.EntryDecisionJSON, &shape)
	x.events.Info(traceID, signalID, p.RootMsgID, EvEntryDecision, "final entry plan; shared risk budget", map[string]interface{}{
		"plan": p.EntryDecisionJSON, "decision": shape.Decision,
		"source_order_type": shape.SourceOrderType, "symbol": p.Symbol,
		"legs": legs, "risk_budget": p.RiskBudget, "rules_snapshot": p.RulesSnapshotJSON,
	})
}
