package copytrader

import (
	"fmt"
	"nofx/trader/types"
	"time"
)

// These are recognition diagnostics, never execution authorization. Market and
// contract checks describe the replay time, not the historical message time.
type ReplayCheck struct {
	Status string `json:"status"`
	Code   string `json:"code,omitempty"`
	Detail string `json:"detail,omitempty"`
}
type ReplayEvaluation struct {
	Action      Action      `json:"action"`
	Symbol      string      `json:"symbol"`
	Canonical   string      `json:"canonical,omitempty"`
	Source      ReplayCheck `json:"source"`
	Parameters  ReplayCheck `json:"parameters"`
	Market      ReplayCheck `json:"market"`
	Contract    ReplayCheck `json:"contract"`
	MarketPrice float64     `json:"market_price,omitempty"`
	CheckedAt   time.Time   `json:"checked_at"`
}

func uncheckedReplay(ins *SourceInterpretation) ReplayEvaluation {
	return ReplayEvaluation{Action: ins.Action, Symbol: ins.Symbol, CheckedAt: time.Now().UTC(),
		Source: ReplayCheck{Status: "not_checked"}, Parameters: ReplayCheck{Status: "not_checked"},
		Market: ReplayCheck{Status: "not_checked"}, Contract: ReplayCheck{Status: "not_checked"}}
}

func validationCheck(ins *SourceInterpretation, price float64) ReplayCheck {
	skip, err := ValidateInterpretation(ins, price)
	if err != nil {
		return ReplayCheck{Status: "failed", Code: "VALIDATION_FAILED", Detail: err.Error()}
	}
	if skip != SkipNone {
		return ReplayCheck{Status: "failed", Code: string(skip), Detail: string(skip)}
	}
	return ReplayCheck{Status: "passed"}
}

func (e *Engine) replayChecks(ins *SourceInterpretation) (ReplayEvaluation, string, string) {
	ev := uncheckedReplay(ins)
	if !ins.IsActionable() {
		ev.Parameters = validationCheck(ins, 0)
		if ev.Parameters.Code == "VALIDATION_FAILED" {
			return ev, VerdictInvalid, ev.Parameters.Detail
		}
		ev.Parameters.Status = "not_required"
		return ev, VerdictSkip, ev.Parameters.Detail
	}
	canonical, err := ResolveInstrument(ins.Symbol)
	if err != nil {
		ev.Parameters = ReplayCheck{Status: "failed", Code: string(SkipUnsupportedInstrument), Detail: err.Error()}
		return ev, VerdictSkip, ev.Parameters.Detail
	}
	ev.Canonical = canonical
	// Convert only the validation copy's author references, never the plan.
	parameters := *ins
	parameters.EntryOrders = append([]EntryOrder(nil), ins.EntryOrders...)
	for i := range parameters.EntryOrders {
		p := &parameters.EntryOrders[i].Price
		if p.Type == PriceMarket && p.Price > 0 {
			p.Type = PriceFixed
		}
	}
	ev.Parameters = validationCheck(&parameters, 0)
	if ev.Parameters.Code == string(SkipUnsupportedPriceSpec) && len(ins.EntryOrders) > 0 && ins.EntryOrders[0].Price.Type == PriceMarket && ins.EntryOrders[0].Price.Price == 0 {
		ev.Parameters = ReplayCheck{Status: "needs_price", Code: "NO_AUTHOR_ENTRY_PRICE", Detail: "no author entry reference; historical entry/SL/TP relationships cannot be verified"}
	}
	if ev.Parameters.Status == "failed" {
		return ev, VerdictInvalid, ev.Parameters.Detail
	}
	newRisk := ins.Action == ActionOpen || ins.Action == ActionAdd
	needsMarket := newRisk
	for _, sl := range ins.StopLossLevels {
		needsMarket = needsMarket || sl.Price.Type == PriceFixed
	}
	for _, tp := range ins.TakeProfitLevels {
		needsMarket = needsMarket || tp.Price.Type == PriceFixed
	}
	if !newRisk {
		ev.Contract = ReplayCheck{Status: "not_required", Detail: "management target state is not simulated"}
	}
	if newRisk {
		if reader, ok := e.exec.ex.(types.MarketRulesReader); ok {
			rules, err := reader.MarketRules(canonical)
			switch {
			case rules != nil && rules.Status != "TRADING":
				ev.Contract = ReplayCheck{Status: "failed", Code: "CONTRACT_NOT_TRADING", Detail: rules.Status}
			case rules != nil && err != nil:
				ev.Contract = ReplayCheck{Status: "failed", Code: "CONTRACT_RULES_REJECTED", Detail: err.Error()}
			case err != nil:
				ev.Contract = ReplayCheck{Status: "unavailable", Code: "CONTRACT_CHECK_FAILED", Detail: err.Error()}
			case rules == nil:
				ev.Contract = ReplayCheck{Status: "unavailable", Code: "EMPTY_CONTRACT_RULES"}
			default:
				ev.Contract = ReplayCheck{Status: "passed", Detail: rules.Status}
			}
		}
	}
	if !needsMarket {
		ev.Market = ReplayCheck{Status: "not_required", Detail: "no current-price-dependent recognition check"}
		return ev, VerdictExecute, "management recognized; historical target state and execution not simulated"
	}
	price, err := e.exec.ex.GetMarketPrice(canonical)
	if err != nil || !finite(price) || price <= 0 {
		ev.Market = ReplayCheck{Status: "unavailable", Code: "PRICE_UNAVAILABLE", Detail: "current market price unavailable; this does not establish an interpretation error"}
		return ev, VerdictSkip, ev.Market.Detail
	}
	ev.MarketPrice = price
	ev.Market = validationCheck(ins, price)
	if ev.Contract.Status == "failed" || ev.Contract.Status == "unavailable" {
		return ev, VerdictSkip, ev.Contract.Code + ": " + ev.Contract.Detail
	}
	if ev.Market.Status != "passed" {
		return ev, VerdictSkip, "CURRENT_MARKET_CHECK: " + ev.Market.Detail
	}
	return ev, VerdictExecute, fmt.Sprintf("%s %s %s; recognition only, execution gates not evaluated", ins.Action, ins.Direction, canonical)
}
