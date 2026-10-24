package binance

import (
	"context"
	"fmt"
	"time"
)

// positionModeQueryTimeout bounds the GET /fapi/v1/positionSide/dual call so a
// hung connection cannot stall trader startup.
const positionModeQueryTimeout = 15 * time.Second

// IsDualSidePosition reports whether the Binance futures account runs in
// hedge (dual-side) position mode, via GET /fapi/v1/positionSide/dual.
//
// Copy-trading orders always carry positionSide=LONG/SHORT, which Binance
// rejects (-4061) on one-way accounts, so callers must refuse to start when
// this returns false or an error.
func (t *FuturesTrader) IsDualSidePosition(ctx context.Context) (bool, error) {
	if t == nil || t.client == nil {
		return false, fmt.Errorf("binance client not initialized")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, positionModeQueryTimeout)
	defer cancel()
	res, err := t.client.NewGetPositionModeService().Do(ctx)
	if err != nil {
		return false, fmt.Errorf("query position mode: %w", err)
	}
	if res == nil {
		return false, fmt.Errorf("query position mode: empty response")
	}
	return res.DualSidePosition, nil
}
