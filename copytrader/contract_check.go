package copytrader

import (
	"errors"
	"fmt"

	"nofx/trader/types"
)

func (e *Engine) checkEntryContract(traceID, signalID, messageID, symbol string) error {
	reader, ok := e.exec.ex.(types.MarketRulesReader)
	if !ok {
		return nil
	}
	rules, err := reader.MarketRules(symbol)
	code := "CONTRACT_CHECK_FAILED"
	if rules != nil && rules.Status != "TRADING" {
		code = "CONTRACT_NOT_TRADING"
	} else if errors.Is(err, types.ErrInstrumentUnavailable) || (rules != nil && err != nil) {
		code = "CONTRACT_UNAVAILABLE"
	}
	if err == nil && rules != nil && rules.Status == "TRADING" {
		return nil
	}
	if err == nil {
		err = fmt.Errorf("missing or non-trading contract rules")
	}
	fields := map[string]interface{}{"symbol": symbol, "error_code": code, "stage": "contract_preflight"}
	if rules != nil {
		fields["contract_status"] = rules.Status
		fields["contract_type"] = rules.ContractType
		fields["quote_asset"] = rules.QuoteAsset
	}
	e.events.Warn(traceID, signalID, messageID, EvRiskRejected, "entry contract check blocked submission", fields)
	return fmt.Errorf("%s: contract capability: %w", code, err)
}
