package copytrader

import (
	"fmt"

	"nofx/store"
	"nofx/trader/types"
)

// Only exact authenticated-account history can resolve an unacknowledged stop.
// Flat positions and absent open-order lists are insufficient evidence.
func (x *Executor) resolveTerminalStop(ctx *store.CopyTradeContext, pending *stopIntent) error {
	reader, ok := x.ex.(types.StopOrderReader)
	if !ok {
		return fmt.Errorf("%w: stop history query unavailable; manual verification required", ErrProtectionUnconfirmed)
	}
	account, err := x.st.CopyTrade().TraderExchangeID(x.traderID)
	if err != nil {
		return err
	}
	if ctx.TraderID != x.traderID || (ctx.ExchangeID != "" && ctx.ExchangeID != account) {
		return fmt.Errorf("%w: stop account binding mismatch", ErrProtectionUnconfirmed)
	}
	if pending.ClientID == "" {
		return fmt.Errorf("%w: missing durable stop identity", ErrProtectionUnconfirmed)
	}
	result, err := reader.GetStopOrder(ctx.Symbol, "", pending.ClientID)
	if err != nil {
		return fmt.Errorf("%w: stop history lookup: %v", ErrProtectionUnconfirmed, err)
	}
	side := "SELL"
	if ctx.Direction == "SHORT" {
		side = "BUY"
	}
	if result == nil || result.ClientID != pending.ClientID || result.Symbol != ctx.Symbol || result.PositionSide != ctx.Direction || result.Side != side || !isStopOrder(result.OpenOrder) {
		return fmt.Errorf("%w: stop history identity/side mismatch", ErrProtectionUnconfirmed)
	}
	if !result.Terminal() {
		return fmt.Errorf("%w: stop history not terminal (%s)", ErrProtectionUnconfirmed, result.Status)
	}
	return nil
}
