package trader

import (
	"nofx/trader/types"
)

// Re-export types for backward compatibility
type (
	ClosedPnLRecord   = types.ClosedPnLRecord
	TradeRecord       = types.TradeRecord
	Trader            = types.Trader
	OpenOrder         = types.OpenOrder
	LimitOrderRequest = types.LimitOrderRequest
	LimitOrderResult  = types.LimitOrderResult
	GridTrader        = types.GridTrader
	// GridTraderAdapter is implemented once in trader/types.
	GridTraderAdapter = types.GridTraderAdapter
)

// NewGridTraderAdapter creates an adapter for basic Trader.
func NewGridTraderAdapter(t Trader) *GridTraderAdapter {
	return types.NewGridTraderAdapter(t)
}
