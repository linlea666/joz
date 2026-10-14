package binance

import (
	"context"
	"errors"
	"fmt"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"math"
	"nofx/logger"
	"nofx/market"
	"nofx/store"
	"nofx/trader/syncloop"
	"nofx/trader/types"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Coalesce concurrent page/background requests for the same physical account.
type historyFlight struct {
	done chan struct{}
	err  error
}

var historyFlights sync.Map

// SyncOrdersFromBinance syncs Binance Futures trade history to local database
// Uses COMMISSION detection + fromId for efficient incremental sync
// Also creates/updates position records to ensure orders/fills/positions data consistency
func (t *FuturesTrader) SyncOrdersFromBinance(traderID string, exchangeID string, exchangeType string, st *store.Store) error {
	return t.syncOrdersContext(context.Background(), traderID, exchangeID, exchangeType, st)
}
func (t *FuturesTrader) syncOrdersContext(ctx context.Context, traderID, exchangeID, exchangeType string, st *store.Store) (result error) {
	flight := &historyFlight{done: make(chan struct{})}
	current, loaded := historyFlights.LoadOrStore(exchangeID, flight)
	if loaded {
		f := current.(*historyFlight)
		select {
		case <-f.done:
			return f.err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	defer func() { flight.err = result; close(flight.done); historyFlights.Delete(exchangeID) }()
	return t.syncHistory(ctx, traderID, exchangeID, exchangeType, st)
}
func (t *FuturesTrader) syncHistory(ctx context.Context, traderID string, exchangeID string, exchangeType string, st *store.Store) (result error) {
	if st == nil {
		return fmt.Errorf("store is nil")
	}

	orderStore := st.Order()

	progress, err := orderStore.HistorySyncState(exchangeID)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	nowMs := now.UnixMilli()
	lastSyncTimeMs := progress.CursorMS
	if lastSyncTimeMs <= 0 || lastSyncTimeMs > nowMs {
		last, err := orderStore.GetLastFillTimeByExchange(exchangeID)
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		lastSyncTimeMs = nowMs - 24*60*60*1000
		if last > 0 && last <= nowMs {
			lastSyncTimeMs = last
		} // inclusive boundary
	}
	progress.CursorMS = lastSyncTimeMs
	progress.LastAttempt = &now
	if err = st.GormDB().Clauses(clause.OnConflict{DoNothing: true}).Create(progress).Error; err != nil {
		return err
	}
	if err = st.GormDB().Model(progress).Update("last_attempt", now).Error; err != nil {
		return err
	}
	defer func() {
		updates := map[string]interface{}{"last_error": ""}
		if result != nil {
			// Exchange query errors have already been stripped of signed URLs.
			detail := []rune(result.Error())
			if len(detail) > 512 {
				detail = append(detail[:512], '…')
			}
			updates["last_error"] = "成交历史同步失败：" + string(detail)
			logger.Warnf("Binance history sync failed; account %s: %s", exchangeID, string(detail))
		} else {
			updates["last_success"] = time.Now().UTC()
		}
		if err := st.GormDB().Model(progress).Updates(updates).Error; err != nil {
			result = errors.Join(result, err)
		}
	}()

	logger.Infof("🔄 Syncing Binance trades from: %s (UTC) [ms: %d, now: %d]",
		time.UnixMilli(lastSyncTimeMs).UTC().Format("2006-01-02 15:04:05"), lastSyncTimeMs, nowMs)

	// Step 1: Get max trade IDs from local DB for incremental sync
	maxTradeIDs, err := orderStore.GetMaxTradeIDsByExchange(exchangeID)
	if err != nil {
		return fmt.Errorf("read history cursor: %w", err)
	}

	// Step 2: Detect symbols to sync using multiple methods
	// COMMISSION detection may miss trades (VIP users, BNB discount, 0-fee trades)
	symbolMap := make(map[string]bool)
	lastSyncTime := time.UnixMilli(lastSyncTimeMs) // Convert to time.Time for API calls

	// Method 1: COMMISSION income detection
	commissionSymbols, err := t.historyIncomeSymbolsContext(ctx, "COMMISSION", lastSyncTime)
	if err != nil {
		return fmt.Errorf("commission symbol query: %w", err)
	} else {
		logger.Infof("  📋 COMMISSION symbols found: %d - %v", len(commissionSymbols), commissionSymbols)
		for _, s := range commissionSymbols {
			symbolMap[s] = true
		}
	}

	// Method 2: Always include active positions (catches trades that COMMISSION missed)
	positionSymbols, err := t.getPositionSymbolsChecked(ctx)
	if err != nil {
		return err
	}
	logger.Infof("  📋 Position symbols found: %d - %v", len(positionSymbols), positionSymbols)
	for _, s := range positionSymbols {
		symbolMap[s] = true
	}

	// Method 3: Include symbols from recent fills in DB (in case some were partially synced)
	recentSymbols, err := orderStore.GetRecentFillSymbolsByExchange(exchangeID, lastSyncTimeMs)
	if err != nil {
		return err
	}
	logger.Infof("  📋 Recent fill symbols found: %d - %v", len(recentSymbols), recentSymbols)
	for _, s := range recentSymbols {
		symbolMap[s] = true
	}

	// Method 4: ALWAYS query REALIZED_PNL income to find symbols with closed trades
	// This catches trades that COMMISSION missed (VIP users, BNB fee discount)
	// IMPORTANT: Must run always, not just when symbolMap is empty,
	// because a position might be fully closed (no active position) but have PnL
	pnlSymbols, err := t.historyIncomeSymbolsContext(ctx, "REALIZED_PNL", lastSyncTime)
	if err != nil {
		return fmt.Errorf("PnL symbol query: %w", err)
	} else {
		logger.Infof("  📋 REALIZED_PNL symbols found: %d - %v", len(pnlSymbols), pnlSymbols)
		for _, s := range pnlSymbols {
			symbolMap[s] = true
		}
	}

	var changedSymbols []string
	for s := range symbolMap {
		changedSymbols = append(changedSymbols, s)
	}

	if len(changedSymbols) == 0 {
		logger.Infof("📭 No symbols with new trades to sync")
		// DON'T update lastSyncTime to current time here!
		// Keep using the last actual trade time from DB to avoid creating gaps
		// Inclusive boundary is intentional; fill identity handles duplicates.
		return nil
	}

	logger.Infof("📊 Found %d symbols with new trades: %v", len(changedSymbols), changedSymbols)

	// Step 3: Query trades for changed symbols using fromId (incremental) or time-based (new symbols)
	var allTrades []types.TradeRecord
	var failedSymbols []string
	apiCalls := 0
	for _, symbol := range changedSymbols {
		var trades []types.TradeRecord
		var queryErr error

		lastID := maxTradeIDs[symbol]
		for page := 0; page < 10; page++ {
			var batch []types.TradeRecord
			if lastID > 0 {
				batch, queryErr = t.getTradesFromID(ctx, symbol, lastID+1, 500)
			} else {
				batch, queryErr = t.getTradesSince(ctx, symbol, lastSyncTime, 500)
			}
			apiCalls++
			if queryErr != nil {
				break
			}
			previous := lastID
			for _, tr := range batch {
				id, e := strconv.ParseInt(tr.TradeID, 10, 64)
				if e != nil {
					queryErr = e
					break
				}
				if id > lastID {
					lastID = id
				}
			}
			trades = append(trades, batch...)
			if queryErr != nil || len(batch) < 500 {
				break
			}
			if lastID <= previous {
				queryErr = fmt.Errorf("non advancing history page")
				break
			}
			if page == 9 {
				failedSymbols = append(failedSymbols, symbol)
			} // persist partial pages; continue next cycle
		}

		if queryErr != nil {
			logger.Infof("  ⚠️ Failed to get trades for %s: %v", symbol, queryErr)
			failedSymbols = append(failedSymbols, symbol)
			continue
		}
		allTrades = append(allTrades, trades...)
	}

	logger.Infof("📥 Received %d trades from Binance (%d API calls)", len(allTrades), apiCalls)

	if len(allTrades) == 0 {
		// No trades returned, but symbols were detected - might be false positive from COMMISSION/PnL detection
		// Don't update lastSyncTime, keep using DB value
		if len(failedSymbols) > 0 {
			return fmt.Errorf("history queries incomplete for %v", failedSymbols)
		}
		return nil
	}

	// Sort trades by time ASC (oldest first) for proper position building
	sort.Slice(allTrades, func(i, j int) bool {
		if allTrades[i].Time.Equal(allTrades[j].Time) {
			a, _ := strconv.ParseInt(allTrades[i].TradeID, 10, 64)
			b, _ := strconv.ParseInt(allTrades[j].TradeID, 10, 64)
			return a < b
		}
		return allTrades[i].Time.Before(allTrades[j].Time)
	})

	syncedCount := 0
	for _, trade := range allTrades {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := t.persistHistoryTrade(st, traderID, exchangeID, exchangeType, trade); err != nil {
			return err
		}
		syncedCount++
	}

	if len(failedSymbols) > 0 {
		return fmt.Errorf("history incomplete for %v; cursor retained", failedSymbols)
	}
	if len(allTrades) > 0 {
		latest := allTrades[len(allTrades)-1].Time.UTC().UnixMilli()
		if err := st.GormDB().Model(progress).Update("cursor_ms", latest).Error; err != nil {
			return err
		}
	}
	logger.Infof("Binance history sync completed: %d checked fills", syncedCount)
	return nil
}

// Existing historical rows remain unchanged. New fill/order/position changes
// commit atomically so a failed position update cannot be hidden by dedup.
func (t *FuturesTrader) persistHistoryTrade(st *store.Store, traderID, exchangeID, exchangeType string, trade types.TradeRecord) error {
	if trade.TradeID == "" || trade.Symbol == "" || trade.Time.IsZero() || (trade.Side != "BUY" && trade.Side != "SELL") || trade.Price <= 0 || trade.Quantity <= 0 {
		return fmt.Errorf("invalid history fill identity/quantity")
	}
	for _, v := range []float64{trade.Price, trade.Quantity, trade.Fee, trade.RealizedPnL} {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return fmt.Errorf("non-finite history fill")
		}
	}
	return st.Transaction(func(tx *gorm.DB) error {

		symbol := market.Normalize(trade.Symbol)
		var count int64
		if err := tx.Model(&store.TraderFill{}).Where("exchange_id = ? AND symbol = ? AND exchange_trade_id = ?", exchangeID, symbol, trade.TradeID).Count(&count).Error; err != nil {
			return err
		}
		if count > 0 {
			return nil
		}
		if err := tx.Model(&store.TraderOrder{}).Where("exchange_id = ? AND symbol = ? AND exchange_order_id = ?", exchangeID, symbol, trade.TradeID).Count(&count).Error; err != nil {
			return err
		}
		if count > 0 {
			return fmt.Errorf("legacy incomplete history row requires review: %s %s", symbol, trade.TradeID)
		}

		action := t.determineOrderAction(trade.Side, trade.PositionSide, trade.RealizedPnL)
		side := trade.PositionSide
		if side == "" || side == "BOTH" {
			side = "SHORT"
			if strings.Contains(action, "long") {
				side = "LONG"
			}
		}
		at := trade.Time.UTC().UnixMilli()
		row := &store.TraderOrder{TraderID: traderID, ExchangeID: exchangeID, ExchangeType: exchangeType, ExchangeOrderID: trade.TradeID, Symbol: symbol, Side: strings.ToUpper(trade.Side), PositionSide: side, Type: "MARKET", OrderAction: action, Quantity: trade.Quantity, Price: trade.Price, Status: "FILLED", FilledQuantity: trade.Quantity, AvgFillPrice: trade.Price, Commission: trade.Fee, FilledAt: at, CreatedAt: at, UpdatedAt: at}
		if err := tx.Create(row).Error; err != nil {
			return err
		}
		f := &store.TraderFill{TraderID: traderID, ExchangeID: exchangeID, ExchangeType: exchangeType, OrderID: row.ID, ExchangeOrderID: trade.TradeID, ExchangeTradeID: trade.TradeID, Symbol: symbol, Side: row.Side, Price: trade.Price, Quantity: trade.Quantity, QuoteQuantity: trade.Price * trade.Quantity, Commission: trade.Fee, CommissionAsset: "USDT", RealizedPnL: trade.RealizedPnL, CreatedAt: at}
		if err := tx.Create(f).Error; err != nil {
			return err
		}
		return store.NewPositionBuilder(store.NewPositionStore(tx)).ProcessTrade(traderID, exchangeID, exchangeType, symbol, side, action, trade.Quantity, trade.Price, trade.Fee, trade.RealizedPnL, at, trade.TradeID)
	})
}
func (t *FuturesTrader) getPositionSymbolsChecked(ctx context.Context) ([]string, error) {
	positions, err := t.client.NewGetPositionRiskService().Do(ctx)
	if err != nil {
		return nil, fmt.Errorf("position discovery failed: %w", &stopHistoryError{cause: err})
	}
	var symbols []string
	for _, p := range positions {
		qty, err := strconv.ParseFloat(p.PositionAmt, 64)
		if err != nil {
			return nil, fmt.Errorf("malformed position amount")
		}
		if qty != 0 {
			symbols = append(symbols, p.Symbol)
		}
	}
	return symbols, nil
}

// getPositionSymbols returns list of symbols that have active positions
// Used as fallback when COMMISSION detection fails
func (t *FuturesTrader) getPositionSymbols() []string {
	positions, err := t.GetPositions()
	if err != nil {
		return nil
	}

	var symbols []string
	for _, pos := range positions {
		if symbol, ok := pos["symbol"].(string); ok && symbol != "" {
			symbols = append(symbols, symbol)
		}
	}
	return symbols
}

// determineOrderAction determines the order action based on trade data
func (t *FuturesTrader) determineOrderAction(side, positionSide string, realizedPnL float64) string {
	side = strings.ToUpper(side)
	if strings.EqualFold(positionSide, "LONG") {
		if side == "SELL" {
			return "close_long"
		}
		return "open_long"
	}
	if strings.EqualFold(positionSide, "SHORT") {
		if side == "BUY" {
			return "close_short"
		}
		return "open_short"
	}

	positionSide = strings.ToUpper(positionSide)

	// If there's realized PnL, it's likely a close trade
	isClose := realizedPnL != 0

	if positionSide == "LONG" || positionSide == "" {
		if side == "BUY" {
			if isClose {
				return "close_short" // Buying to close short
			}
			return "open_long"
		} else {
			if isClose {
				return "close_long" // Selling to close long
			}
			return "open_short"
		}
	} else if positionSide == "SHORT" {
		if side == "SELL" {
			if isClose {
				return "close_long"
			}
			return "open_short"
		} else {
			if isClose {
				return "close_short"
			}
			return "open_long"
		}
	}

	// Default fallback
	if side == "BUY" {
		return "open_long"
	}
	return "open_short"
}

// StartOrderSync starts background order sync task for Binance
func (t *FuturesTrader) StartOrderSync(traderID string, exchangeID string, exchangeType string, st *store.Store, interval time.Duration, stop <-chan struct{}) <-chan struct{} {
	ctx, cancel := context.WithCancel(context.Background())
	done := syncloop.Start(stop, interval, "Binance", true, func() error { return t.syncOrdersContext(ctx, traderID, exchangeID, exchangeType, st) })
	go func() {
		defer cancel()
		select {
		case <-stop:
		case <-done:
		}
	}()
	return done
}
