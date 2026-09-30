package okx

import (
	"fmt"
	"nofx/trader/types"
)

// MarketRules supplies precision in the same base units as GetPositions.
// This optional reader does not opt OKX into Binance's managed entry protocol.
func (t *OKXTrader) MarketRules(symbol string) (*types.ManagedMarketRules, error) {
	i, err := t.getInstrument(symbol)
	if err != nil {
		return nil, err
	}
	if i.CtVal <= 0 || i.LotSz <= 0 || i.TickSz <= 0 {
		return nil, fmt.Errorf("incomplete market rules for %s", symbol)
	}
	return &types.ManagedMarketRules{Symbol: symbol, QuantityStep: types.SanitizeBaseQuantity(i.LotSz * i.CtVal), MinQuantity: types.SanitizeBaseQuantity(i.MinSz * i.CtVal), MarketQuantityStep: types.SanitizeBaseQuantity(i.LotSz * i.CtVal), MarketMinQuantity: types.SanitizeBaseQuantity(i.MinSz * i.CtVal), MarketMaxQuantity: types.SanitizeBaseQuantity(i.MaxMktSz * i.CtVal), PriceTick: i.TickSz}, nil
}
