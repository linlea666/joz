package binance

import (
	"context"
	"fmt"
	"nofx/logger"
	"nofx/trader/types"
	"strconv"
	"time"
)

// GetBalance gets account balance (with cache)
func (t *FuturesTrader) GetBalance() (map[string]interface{}, error) {
	// First check if cache is valid
	t.balanceCacheMutex.RLock()
	if t.cachedBalance != nil && time.Since(t.balanceCacheTime) < t.cacheDuration {
		cacheAge := time.Since(t.balanceCacheTime)
		t.balanceCacheMutex.RUnlock()
		logger.Infof("✓ Using cached account balance (cache age: %.1f seconds ago)", cacheAge.Seconds())
		return t.cachedBalance, nil
	}
	t.balanceCacheMutex.RUnlock()

	// Cache expired or doesn't exist, call API
	logger.Infof("🔄 Cache expired, calling Binance API to get account balance...")
	account, err := t.client.NewGetAccountService().Do(context.Background())
	if err != nil {
		logger.Infof("❌ Binance API call failed: %v", err)
		return nil, fmt.Errorf("failed to get account info: %w", err)
	}

	result := make(map[string]interface{})
	for field, value := range map[string]string{
		"totalWalletBalance":    account.TotalWalletBalance,
		"availableBalance":      account.AvailableBalance,
		"totalUnrealizedProfit": account.TotalUnrealizedProfit,
	} {
		parsed, parseErr := types.ParseFloatField(field, value)
		if parseErr != nil {
			return nil, parseErr
		}
		result[field] = parsed
	}

	logger.Infof("✓ Binance API returned: total balance=%s, available=%s, unrealized PnL=%s",
		account.TotalWalletBalance,
		account.AvailableBalance,
		account.TotalUnrealizedProfit)

	// Update cache
	t.balanceCacheMutex.Lock()
	t.cachedBalance = result
	t.balanceCacheTime = time.Now()
	t.balanceCacheMutex.Unlock()

	return result, nil
}

// GetClosedPnL retrieves recent closing trades from Binance Futures
// Note: Binance does NOT have a position history API, only trade history.
// This returns individual closing trades (realizedPnl != 0) for real-time position closure detection.
// NOT suitable for historical position reconstruction - use only for matching recent closures.
func (t *FuturesTrader) GetClosedPnL(startTime time.Time, limit int) ([]types.ClosedPnLRecord, error) {
	trades, err := t.GetTrades(startTime, limit)
	if err != nil {
		return nil, err
	}

	// Filter only closing trades (realizedPnl != 0) and convert to ClosedPnLRecord
	var records []types.ClosedPnLRecord
	for _, trade := range trades {
		if trade.RealizedPnL == 0 {
			continue // Skip opening trades
		}

		// Determine side from trade
		side := "long"
		if trade.PositionSide == "SHORT" || trade.PositionSide == "short" {
			side = "short"
		} else if trade.PositionSide == "BOTH" || trade.PositionSide == "" {
			// One-way mode: selling closes long, buying closes short
			if trade.Side == "SELL" || trade.Side == "Sell" {
				side = "long"
			} else {
				side = "short"
			}
		}

		// Calculate entry price from PnL (mathematically accurate for this trade)
		var entryPrice float64
		if trade.Quantity > 0 {
			if side == "long" {
				entryPrice = trade.Price - trade.RealizedPnL/trade.Quantity
			} else {
				entryPrice = trade.Price + trade.RealizedPnL/trade.Quantity
			}
		}

		records = append(records, types.ClosedPnLRecord{
			Symbol:      trade.Symbol,
			Side:        side,
			EntryPrice:  entryPrice,
			ExitPrice:   trade.Price,
			Quantity:    trade.Quantity,
			RealizedPnL: trade.RealizedPnL,
			Fee:         trade.Fee,
			ExitTime:    trade.Time,
			EntryTime:   trade.Time, // Approximate
			OrderID:     trade.TradeID,
			ExchangeID:  trade.TradeID,
			CloseType:   "unknown",
		})
	}

	return records, nil
}

// GetTrades retrieves trade history from Binance Futures using Income API
// Note: Income API has delays (~minutes), for real-time use GetTradesForSymbol instead
func (t *FuturesTrader) GetTrades(startTime time.Time, limit int) ([]types.TradeRecord, error) {
	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}

	// Use Income API to get REALIZED_PNL records (all symbols)
	incomes, err := t.client.NewGetIncomeHistoryService().
		IncomeType("REALIZED_PNL").
		StartTime(startTime.UnixMilli()).
		Limit(int64(limit)).
		Do(context.Background())
	if err != nil {
		return nil, fmt.Errorf("failed to get income history: %w", err)
	}

	var trades []types.TradeRecord
	for _, income := range incomes {
		pnl, _ := strconv.ParseFloat(income.Income, 64)
		if pnl == 0 {
			continue // Skip zero PnL records
		}

		// Income API doesn't provide full trade details, create a minimal record
		// This is mainly used for detecting recent closures, not historical reconstruction
		trade := types.TradeRecord{
			TradeID:     strconv.FormatInt(income.TranID, 10),
			Symbol:      income.Symbol,
			RealizedPnL: pnl,
			Time:        time.UnixMilli(income.Time).UTC(),
			// Note: Income API doesn't provide price, quantity, side, fee
			// For accurate data, use GetTradesForSymbol with specific symbol
		}
		trades = append(trades, trade)
	}

	return trades, nil
}

// GetTradesForSymbol retrieves trade history for a specific symbol
// This is more reliable than using Income API which may have delays
func (t *FuturesTrader) GetTradesForSymbol(symbol string, startTime time.Time, limit int) ([]types.TradeRecord, error) {
	return t.getTradesSince(context.Background(), symbol, startTime, limit)
}
func (t *FuturesTrader) getTradesSince(ctx context.Context, symbol string, startTime time.Time, limit int) ([]types.TradeRecord, error) {
	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}

	accountTrades, err := t.client.NewListAccountTradeService().
		Symbol(symbol).
		StartTime(startTime.UnixMilli()).
		Limit(limit).
		Do(ctx)
	if err != nil {
		return nil, fmt.Errorf("trade history query %s: %w", symbol, &stopHistoryError{cause: err})
	}

	return decodeAccountTrades(accountTrades)
}

// GetTradesForSymbolFromID retrieves trade history for a specific symbol starting from a given trade ID
// This is used for incremental sync - only fetch new trades since last sync
func (t *FuturesTrader) GetTradesForSymbolFromID(symbol string, fromID int64, limit int) ([]types.TradeRecord, error) {
	return t.getTradesFromID(context.Background(), symbol, fromID, limit)
}
func (t *FuturesTrader) getTradesFromID(ctx context.Context, symbol string, fromID int64, limit int) ([]types.TradeRecord, error) {
	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}

	accountTrades, err := t.accountTradesFromID(ctx, symbol, fromID, limit)
	if err != nil {
		return nil, fmt.Errorf("trade history query %s from %d: %w", symbol, fromID, &stopHistoryError{cause: err})
	}

	return decodeAccountTrades(accountTrades)
}

// GetCommissionSymbols returns symbols that have new commission records since lastSyncTime
// COMMISSION income is generated for every trade, so this is more reliable than REALIZED_PNL
func (t *FuturesTrader) GetCommissionSymbols(lastSyncTime time.Time) ([]string, error) {
	return t.historyIncomeSymbols("COMMISSION", lastSyncTime)
}
func (t *FuturesTrader) GetPnLSymbols(lastSyncTime time.Time) ([]string, error) {
	return t.historyIncomeSymbols("REALIZED_PNL", lastSyncTime)
}

// The SDK has no income page parameter. Bisect full time windows instead;
// equal-millisecond saturation fails explicitly rather than skipping a boundary.
func (t *FuturesTrader) historyIncomeSymbols(kind string, start time.Time) ([]string, error) {
	return t.historyIncomeSymbolsContext(context.Background(), kind, start)
}
func (t *FuturesTrader) historyIncomeSymbolsContext(ctx context.Context, kind string, start time.Time) ([]string, error) {
	type window struct{ lo, hi int64 }
	pending := []window{{start.UnixMilli(), time.Now().UTC().UnixMilli()}}
	symbols := map[string]bool{}
	for requests := 0; len(pending) > 0; requests++ {
		if requests >= 100 {
			return nil, fmt.Errorf("income discovery incomplete: request budget reached")
		}
		w := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		records, err := t.client.NewGetIncomeHistoryService().IncomeType(kind).StartTime(w.lo).EndTime(w.hi).Limit(1000).Do(ctx)
		if err != nil {
			return nil, fmt.Errorf("income discovery failed: %w", &stopHistoryError{cause: err})
		}
		if len(records) >= 1000 {
			if w.lo >= w.hi {
				return nil, fmt.Errorf("income discovery incomplete: timestamp saturated")
			}
			mid := w.lo + (w.hi-w.lo)/2
			pending = append(pending, window{mid + 1, w.hi}, window{w.lo, mid})
			continue
		}
		for _, r := range records {
			if r.Symbol != "" {
				symbols[r.Symbol] = true
			}
		}
	}
	out := make([]string, 0, len(symbols))
	for s := range symbols {
		out = append(out, s)
	}
	return out, nil
}
