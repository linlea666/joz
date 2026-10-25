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

// historyDiscoveryOverlap is how far symbol discovery looks behind the cursor.
const historyDiscoveryOverlap = 10 * time.Minute

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
	var syncNotes []string
	defer func() {
		updates := map[string]interface{}{"last_error": ""}
		if result == nil && len(syncNotes) > 0 {
			// Successful sync with caveats: keep them visible in sync status.
			detail := []rune(strings.Join(syncNotes, "；"))
			if len(detail) > 512 {
				detail = append(detail[:512], '…')
			}
			updates["last_error"] = "成交历史已同步，需注意：" + string(detail)
		}
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
	// The cursor is the newest fill across all symbols. Income records can lag
	// and another symbol's later fill may already have advanced the cursor, so
	// discovery re-scans a safety window behind it; trade IDs deduplicate.
	discoveryStartMs := lastSyncTimeMs - historyDiscoveryOverlap.Milliseconds()
	if discoveryStartMs < 0 {
		discoveryStartMs = 0
	}
	discoveryStart := time.UnixMilli(discoveryStartMs)

	// Method 1: COMMISSION income detection
	commissionSymbols, err := t.historyIncomeSymbolsContext(ctx, "COMMISSION", discoveryStart)
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
	recentSymbols, err := orderStore.GetRecentFillSymbolsByExchange(exchangeID, discoveryStartMs)
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
	pnlSymbols, err := t.historyIncomeSymbolsContext(ctx, "REALIZED_PNL", discoveryStart)
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
				batch, queryErr = t.getTradesSince(ctx, symbol, discoveryStart, 500)
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
		note, err := t.persistHistoryTradeNoted(st, traderID, exchangeID, exchangeType, trade)
		if err != nil {
			return err
		}
		if note != "" {
			logger.Warnf("Binance history sync note; account %s: %s", exchangeID, note)
			syncNotes = append(syncNotes, note)
		}
		syncedCount++
	}

	if len(failedSymbols) > 0 {
		return fmt.Errorf("history incomplete for %v; cursor retained", failedSymbols)
	}
	if len(allTrades) > 0 {
		latest := allTrades[len(allTrades)-1].Time.UTC().UnixMilli()
		if latest < lastSyncTimeMs {
			latest = lastSyncTimeMs // overlap re-reads never move the cursor back
		}
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
	_, err := t.persistHistoryTradeNoted(st, traderID, exchangeID, exchangeType, trade)
	return err
}

// historyFillLegs maps one fill to position-ledger effects. Hedge fills use
// their explicit positionSide. One-way (BOTH/empty) fills use realized PnL and
// the locally tracked open quantity; nil means the fill needs manual review.
func (t *FuturesTrader) historyFillLegs(tx *gorm.DB, traderID, symbol string, trade types.TradeRecord) ([]types.OneWayLeg, error) {
	if ps := strings.ToUpper(trade.PositionSide); ps == "LONG" || ps == "SHORT" {
		return []types.OneWayLeg{{Action: t.determineOrderAction(trade.Side, ps, trade.RealizedPnL), Quantity: trade.Quantity}}, nil
	}
	positions := store.NewPositionStore(tx)
	var open [2]float64
	for i, side := range []string{"LONG", "SHORT"} {
		p, err := positions.GetOpenPositionBySymbol(traderID, symbol, side)
		if err != nil {
			return nil, err
		}
		if p != nil && p.Quantity > 0 {
			open[i] = p.Quantity
		}
	}
	return types.ClassifyOneWayFill(trade.Side, trade.Quantity, trade.RealizedPnL, open[0], open[1]), nil
}

// historyOrderAction is stored for fills whose one-way direction cannot be
// determined. Such fills are kept (and deduplicated) but do not move positions.
const historyOrderActionReview = "review"

// persistHistoryTradeNoted persists one fill and returns a human-readable note
// when the fill was stored with a caveat (non-USDT fee, direction review).
//
// Order rows are keyed by the venue's real orderId; several fills of one order
// share one aggregated row. Fills are deduplicated only by trade ID. Fills
// without an orderId (legacy callers/tests) keep the historical trade-ID key.
func (t *FuturesTrader) persistHistoryTradeNoted(st *store.Store, traderID, exchangeID, exchangeType string, trade types.TradeRecord) (string, error) {
	if trade.TradeID == "" || trade.Symbol == "" || trade.Time.IsZero() || (trade.Side != "BUY" && trade.Side != "SELL") || trade.Price <= 0 || trade.Quantity <= 0 {
		return "", fmt.Errorf("invalid history fill identity/quantity")
	}
	for _, v := range []float64{trade.Price, trade.Quantity, trade.Fee, trade.RealizedPnL} {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return "", fmt.Errorf("non-finite history fill")
		}
	}
	var notes []string
	err := st.Transaction(func(tx *gorm.DB) error {
		notes = notes[:0]
		symbol := market.Normalize(trade.Symbol)
		var count int64
		if err := tx.Model(&store.TraderFill{}).Where("exchange_id = ? AND symbol = ? AND exchange_trade_id = ?", exchangeID, symbol, trade.TradeID).Count(&count).Error; err != nil {
			return err
		}
		if count > 0 {
			return nil
		}
		// Legacy rows keyed the order by trade ID; one without its fill is an
		// interrupted pre-transaction write. Aggregated real-orderId rows always
		// carry at least one fill, so they never match here.
		if err := tx.Model(&store.TraderOrder{}).Where("exchange_id = ? AND symbol = ? AND exchange_order_id = ? AND NOT EXISTS (SELECT 1 FROM trader_fills f WHERE f.order_id = trader_orders.id)", exchangeID, symbol, trade.TradeID).Count(&count).Error; err != nil {
			return err
		}
		if count > 0 {
			return fmt.Errorf("legacy incomplete history row requires review: %s %s", symbol, trade.TradeID)
		}

		feeAsset := trade.FeeAsset
		if feeAsset == "" {
			feeAsset = "USDT"
		}
		// Order and position fees are summed as USDT. A fee paid in another
		// asset (e.g. BNB discount) keeps its raw value on the fill only.
		usdtFee := trade.Fee
		if feeAsset != "USDT" {
			usdtFee = 0
			if trade.Fee != 0 {
				notes = append(notes, fmt.Sprintf("%s 成交 %s 手续费 %.8g %s 未折算，未计入 USDT 手续费", symbol, trade.TradeID, trade.Fee, feeAsset))
			}
		}

		legs, err := t.historyFillLegs(tx, traderID, symbol, trade)
		if err != nil {
			return err
		}
		action, side := historyOrderActionReview, "BOTH"
		if len(legs) == 0 {
			notes = append(notes, fmt.Sprintf("%s 成交 %s 单向持仓方向无法判定，已记为 review、未更新持仓", symbol, trade.TradeID))
		} else {
			action = legs[0].Action
			side = trade.PositionSide
			if side == "" || side == "BOTH" {
				side = "SHORT"
				if strings.Contains(action, "long") {
					side = "LONG"
				}
			}
		}

		at := trade.Time.UTC().UnixMilli()
		orderKey, legacyKey := trade.OrderID, trade.OrderID == ""
		if legacyKey {
			orderKey = trade.TradeID
		}
		var row store.TraderOrder
		found := true
		if err := tx.Where("exchange_id = ? AND symbol = ? AND exchange_order_id = ?", exchangeID, symbol, orderKey).First(&row).Error; err != nil {
			if !errors.Is(err, gorm.ErrRecordNotFound) {
				return err
			}
			found = false
		}
		if found && !legacyKey {
			// Attach only to a row this code created for the same order: its
			// fills reference the order ID and carry distinct trade IDs. A
			// legacy trade-ID-keyed row that merely shares the number is not.
			if err := tx.Model(&store.TraderFill{}).Where("order_id = ? AND exchange_order_id = ? AND exchange_trade_id <> ?", row.ID, orderKey, orderKey).Count(&count).Error; err != nil {
				return err
			}
			if count == 0 {
				return fmt.Errorf("history order identity collides with a legacy row and requires review: %s order %s trade %s", symbol, orderKey, trade.TradeID)
			}
		} else if found {
			return fmt.Errorf("history order identity collides with an existing row and requires review: %s %s", symbol, orderKey)
		}
		if found {
			filled := row.FilledQuantity + trade.Quantity
			avg := trade.Price
			if filled > 0 {
				avg = (row.AvgFillPrice*row.FilledQuantity + trade.Price*trade.Quantity) / filled
			}
			updates := map[string]interface{}{"filled_quantity": filled, "quantity": filled, "avg_fill_price": avg, "price": avg, "commission": row.Commission + usdtFee}
			if at > row.FilledAt {
				updates["filled_at"] = at
			}
			if at > row.UpdatedAt {
				updates["updated_at"] = at
			}
			if at < row.CreatedAt {
				updates["created_at"] = at
			}
			if err := tx.Model(&store.TraderOrder{}).Where("id = ?", row.ID).Updates(updates).Error; err != nil {
				return err
			}
		} else {
			row = store.TraderOrder{TraderID: traderID, ExchangeID: exchangeID, ExchangeType: exchangeType, ExchangeOrderID: orderKey, Symbol: symbol, Side: strings.ToUpper(trade.Side), PositionSide: side, Type: "MARKET", OrderAction: action, Quantity: trade.Quantity, Price: trade.Price, Status: "FILLED", FilledQuantity: trade.Quantity, AvgFillPrice: trade.Price, Commission: usdtFee, CommissionAsset: "USDT", FilledAt: at, CreatedAt: at, UpdatedAt: at}
			if err := tx.Create(&row).Error; err != nil {
				return err
			}
		}
		f := &store.TraderFill{TraderID: traderID, ExchangeID: exchangeID, ExchangeType: exchangeType, OrderID: row.ID, ExchangeOrderID: orderKey, ExchangeTradeID: trade.TradeID, Symbol: symbol, Side: strings.ToUpper(trade.Side), Price: trade.Price, Quantity: trade.Quantity, QuoteQuantity: trade.Price * trade.Quantity, Commission: trade.Fee, CommissionAsset: feeAsset, RealizedPnL: trade.RealizedPnL, CreatedAt: at}
		if err := tx.Create(f).Error; err != nil {
			return err
		}
		builder := store.NewPositionBuilder(store.NewPositionStore(tx))
		for i, leg := range legs {
			legSide := "SHORT"
			if strings.Contains(leg.Action, "long") {
				legSide = "LONG"
			}
			if ps := strings.ToUpper(trade.PositionSide); ps == "LONG" || ps == "SHORT" {
				legSide = ps
			}
			fee := usdtFee * leg.Quantity / trade.Quantity
			pnl := 0.0
			if i == 0 {
				pnl = trade.RealizedPnL // a reversal's PnL belongs to its close leg
			}
			if err := builder.ProcessTrade(traderID, exchangeID, exchangeType, symbol, legSide, leg.Action, leg.Quantity, trade.Price, fee, pnl, at, orderKey); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return strings.Join(notes, "；"), nil
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

// determineOrderAction maps a hedge-mode fill (positionSide LONG/SHORT) to its
// action. One-way (BOTH/empty) fills need ledger context and are classified by
// historyFillLegs; here they resolve only when realized PnL proves a close,
// and otherwise return "" rather than guessing an open.
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
	if realizedPnL != 0 {
		switch side {
		case "BUY":
			return "close_short"
		case "SELL":
			return "close_long"
		}
	}
	return ""
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
