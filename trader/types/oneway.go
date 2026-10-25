package types

import "strings"

// OneWayLeg is one ledger effect of a fill on a one-way (Binance BOTH / OKX
// net) position. A reversing fill yields a close leg followed by an open leg.
type OneWayLeg struct {
	Action   string // open_long, open_short, close_long, close_short
	Quantity float64
}

// ClassifyOneWayFill decides whether a one-way-mode fill opens or closes,
// using the realized PnL the venue reported and the locally tracked open
// quantities for the symbol. A fill against the opposite open position closes
// it (and opens the remainder when it reverses); a fill with non-zero realized
// PnL closes even when the ledger never saw the position. It returns nil when
// the fill cannot be classified safely (invalid input, both sides open, or
// realized PnL while only the same direction is open); callers must flag such
// fills for review instead of recording them as an open.
func ClassifyOneWayFill(side string, quantity, realizedPnL, openLong, openShort float64) []OneWayLeg {
	if quantity <= 0 || openLong < 0 || openShort < 0 {
		return nil
	}
	var same, opposite float64
	var openAction, closeAction string
	switch strings.ToUpper(side) {
	case "BUY":
		same, opposite, openAction, closeAction = openLong, openShort, "open_long", "close_short"
	case "SELL":
		same, opposite, openAction, closeAction = openShort, openLong, "open_short", "close_long"
	default:
		return nil
	}
	if same > 0 && opposite > 0 {
		return nil // impossible in one-way mode: the ledger is inconsistent
	}
	if opposite > 0 {
		closeQty := quantity
		if opposite < closeQty {
			closeQty = opposite
		}
		legs := []OneWayLeg{{Action: closeAction, Quantity: closeQty}}
		if rest := quantity - closeQty; rest > quantity*1e-9 {
			legs = append(legs, OneWayLeg{Action: openAction, Quantity: rest})
		}
		return legs
	}
	if realizedPnL != 0 {
		if same > 0 {
			return nil // PnL on a fill that would add to the open side
		}
		return []OneWayLeg{{Action: closeAction, Quantity: quantity}}
	}
	return []OneWayLeg{{Action: openAction, Quantity: quantity}}
}
