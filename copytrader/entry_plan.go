package copytrader

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
)

type resolvedEntryPlan struct {
	EntryPolicy     string         `json:"entry_policy,omitempty"`
	SourceOrderType EntryOrderType `json:"source_order_type"`
	Decision        entryDecision  `json:"decision"`
	Origin          string         `json:"origin"`
	SplitReference  float64        `json:"second_limit,omitempty"`
	PolicyNote      string         `json:"policy_note"`
}

// Resolve the complete shape once. Explicit legs and the optional single-
// reference strategy are mutually exclusive branches, not successive rewrites.
func resolveEntryPlan(direction Direction, orders []EntryOrder, market, threshold float64, convert bool, policy string) (resolvedEntryPlan, SkipReason, error) {
	p := resolvedEntryPlan{EntryPolicy: policy}
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
		if policy == EntryPolicyParticipate {
			applied := false
			p.Decision.ThresholdApplied = &applied
			p.PolicyNote = "participation policy: favorable/equal full market; adverse shared-risk market plus reference limit; offset threshold not applied"
			if d.AdverseDeviationPct != nil && *d.AdverseDeviationPct > 0 {
				p.Origin = "single_reference_participation"
				p.SplitReference = orders[0].Price.Price
				p.Decision.OrderType, p.Decision.EntryPrice = EntryPlanMarket, market
				p.Decision.Reason = "participation_adverse_split"
			}
			return p, SkipNone, nil
		}
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

// logEntryPlan records the final plan as it will be submitted. risk_budget is
// the budget this plan is sized against: the by-loss budget, or (percent /
// fixed modes, which carry none) the risk the legs actually take.
// planned_risk_usd is always what the floored legs carry (qty x stop distance).
func (x *Executor) logEntryPlan(traceID, signalID string, p *OpenPlan, legs []map[string]interface{}) {
	var shape resolvedEntryPlan
	_ = json.Unmarshal(p.EntryDecisionJSON, &shape)
	used := 0.0
	kinds := make([]string, 0, len(legs))
	for _, leg := range legs {
		price, _ := leg["price"].(float64)
		qty, _ := leg["quantity"].(float64)
		used += qty * math.Abs(price-p.StopLoss)
		kinds = append(kinds, fmt.Sprintf("%v %.8g@%.8g", leg["type"], qty, price))
	}
	budget := p.RiskBudget
	if budget <= 0 {
		budget = used
	}
	origin, policy := shape.Origin, shape.EntryPolicy
	if origin == "" {
		origin = "single"
	}
	if policy == "" {
		policy = EntryPolicyLegacy
	}
	shapeText := "single leg"
	if len(legs) > 1 {
		shapeText = fmt.Sprintf("%d legs sharing one risk budget", len(legs))
	}
	msg := fmt.Sprintf("final entry plan: %s %s %s [%s], SL %.8g, risk $%.2f (origin %s, policy %s)",
		p.Symbol, p.Direction, shapeText, strings.Join(kinds, " + "), p.StopLoss, used, origin, policy)
	data := map[string]interface{}{
		"plan": p.EntryDecisionJSON, "decision": shape.Decision,
		"source_order_type": shape.SourceOrderType, "symbol": p.Symbol,
		"legs": legs, "risk_budget": budget, "planned_risk_usd": used,
		"origin": origin, "entry_policy": policy, "rules_snapshot": p.RulesSnapshotJSON,
	}
	x.events.Info(traceID, signalID, p.RootMsgID, EvEntryDecision, msg, data)
	// A shared-risk combination compressed by the notional / margin caps (or
	// by lot flooring beyond 1%) carries less risk than configured.
	if len(legs) > 1 && p.RiskBudget > 0 && used < p.RiskBudget*0.99 {
		x.events.Warn(traceID, signalID, p.RootMsgID, EvQuantityPlan,
			fmt.Sprintf("shared-risk legs compressed: configured risk $%.2f, actual risk $%.2f (notional cap $%.2f, margin budget $%.2f, lot flooring)",
				p.RiskBudget, used, EffectiveNotionalCap(p.MaxNotional), p.AvailableMargin*.9*float64(p.Leverage)),
			map[string]interface{}{"configured_risk_usd": p.RiskBudget, "actual_risk_usd": used, "notional_cap_usd": EffectiveNotionalCap(p.MaxNotional), "available_margin_usd": p.AvailableMargin})
	}
}
