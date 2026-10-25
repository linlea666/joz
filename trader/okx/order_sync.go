package okx

import (
	"encoding/json"
	"fmt"
	"math"
	"nofx/logger"
	"nofx/market"
	"nofx/store"
	"nofx/trader/syncloop"
	"nofx/trader/types"
	"sort"
	"strconv"
	"strings"
	"time"

	"gorm.io/gorm"
)

// OKXTrade represents a trade record from OKX fills history
type OKXTrade struct {
	InstID      string
	Symbol      string
	TradeID     string
	OrderID     string
	Side        string // buy or sell
	PosSide     string // long or short
	FillPrice   float64
	FillQty     float64 // In contracts
	FillQtyBase float64 // In base asset (BTC, ETH, etc)
	Fee         float64
	FeeAsset    string
	ExecTime    time.Time
	IsMaker     bool
	OrderType   string
	OrderAction string  // open_long, open_short, close_long, close_short; "" for net (one-way) fills
	FillPnL     float64 // fillPnl reported by OKX (used to classify net-mode fills)
}

// okxHistoryOrderActionReview marks net-mode fills whose direction cannot be
// determined safely; they are stored but do not move positions.
const okxHistoryOrderActionReview = "review"

func parseOKXHistoryNumber(field, raw string, optional bool) (float64, error) {
	if raw == "" && optional {
		return 0, nil
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
		return 0, fmt.Errorf("malformed fill %s %q", field, raw)
	}
	return v, nil
}

// GetTrades retrieves trade/fill records from OKX. Fill sizes are contracts;
// they are converted with the instrument's contract value, and the whole batch
// fails when that value is unavailable rather than recording contracts as coins.
func (t *OKXTrader) GetTrades(startTime time.Time, limit int) ([]OKXTrade, error) {
	if limit <= 0 {
		limit = 100
	}
	if limit > 100 {
		limit = 100 // OKX max limit is 100
	}

	// Build query path
	// OKX fills-history endpoint for historical fills
	path := fmt.Sprintf("/api/v5/trade/fills-history?instType=SWAP&limit=%d", limit)
	if !startTime.IsZero() {
		path += fmt.Sprintf("&begin=%d", startTime.UnixMilli())
	}

	data, err := t.doRequest("GET", path, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to get fills history: %w", err)
	}

	var fills []struct {
		InstID   string `json:"instId"`   // e.g., "BTC-USDT-SWAP"
		TradeID  string `json:"tradeId"`  // Trade ID
		OrdID    string `json:"ordId"`    // Order ID
		BillID   string `json:"billId"`   // Bill ID
		Side     string `json:"side"`     // buy or sell
		PosSide  string `json:"posSide"`  // long, short, or net
		FillPx   string `json:"fillPx"`   // Fill price
		FillSz   string `json:"fillSz"`   // Fill size (contracts)
		FillPnl  string `json:"fillPnl"`  // Realized PnL of a closing fill
		Fee      string `json:"fee"`      // Fee (negative for cost)
		FeeCcy   string `json:"feeCcy"`   // Fee currency
		Ts       string `json:"ts"`       // Trade timestamp (ms)
		ExecType string `json:"execType"` // T: taker, M: maker
		Tag      string `json:"tag"`      // Order tag
	}

	if err := json.Unmarshal(data, &fills); err != nil {
		return nil, fmt.Errorf("failed to parse fills: %w", err)
	}

	trades := make([]OKXTrade, 0, len(fills))
	ctVals := map[string]float64{}

	for _, fill := range fills {
		fillPrice, err := parseOKXHistoryNumber("price", fill.FillPx, false)
		if err != nil {
			return nil, err
		}
		fillSz, err := parseOKXHistoryNumber("size", fill.FillSz, false)
		if err != nil {
			return nil, err
		}
		fee, err := parseOKXHistoryNumber("fee", fill.Fee, true)
		if err != nil {
			return nil, err
		}
		fillPnL, err := parseOKXHistoryNumber("pnl", fill.FillPnl, true)
		if err != nil {
			return nil, err
		}
		ts, err := strconv.ParseInt(fill.Ts, 10, 64)
		if err != nil || ts <= 0 {
			return nil, fmt.Errorf("malformed fill timestamp %q", fill.Ts)
		}
		if fill.TradeID == "" {
			return nil, fmt.Errorf("fill without trade ID")
		}

		// Convert symbol: BTC-USDT-SWAP -> BTCUSDT
		symbol := t.convertSymbolBack(fill.InstID)

		// Convert contract count to base asset quantity. Without the contract
		// value the base quantity is unknown: fail the batch (retried next cycle).
		ctVal, ok := ctVals[symbol]
		if !ok {
			inst, err := t.getInstrument(symbol)
			if err != nil {
				return nil, fmt.Errorf("contract value unavailable for %s: %w", fill.InstID, err)
			}
			if inst == nil || inst.CtVal <= 0 || math.IsNaN(inst.CtVal) || math.IsInf(inst.CtVal, 0) {
				return nil, fmt.Errorf("contract value unavailable for %s", fill.InstID)
			}
			ctVal = inst.CtVal
			ctVals[symbol] = ctVal
		}
		fillQtyBase := fillSz * ctVal

		// Determine order action based on side and posSide
		// OKX long/short mode:
		// - buy + long = open long
		// - sell + long = close long
		// - sell + short = open short
		// - buy + short = close short
		// Net (one-way) fills are classified against the ledger when persisted.
		orderAction := ""
		posSide := strings.ToLower(fill.PosSide)
		side := strings.ToLower(fill.Side)
		if side != "buy" && side != "sell" {
			return nil, fmt.Errorf("malformed fill side %q", fill.Side)
		}

		if posSide == "long" {
			if side == "buy" {
				orderAction = "open_long"
			} else {
				orderAction = "close_long"
			}
		} else if posSide == "short" {
			if side == "sell" {
				orderAction = "open_short"
			} else {
				orderAction = "close_short"
			}
		}

		trade := OKXTrade{
			InstID:      fill.InstID,
			Symbol:      symbol,
			TradeID:     fill.TradeID,
			OrderID:     fill.OrdID,
			Side:        fill.Side,
			PosSide:     fill.PosSide,
			FillPrice:   fillPrice,
			FillQty:     fillSz,
			FillQtyBase: fillQtyBase,
			Fee:         -fee, // OKX returns negative fee
			FeeAsset:    fill.FeeCcy,
			ExecTime:    time.UnixMilli(ts).UTC(),
			IsMaker:     fill.ExecType == "M",
			OrderType:   "MARKET",
			OrderAction: orderAction,
			FillPnL:     fillPnL,
		}

		trades = append(trades, trade)
	}

	return trades, nil
}

// SyncOrdersFromOKX syncs OKX exchange order history to local database
// Also creates/updates position records to ensure orders/fills/positions data consistency
// exchangeID: Exchange account UUID (from exchanges.id)
// exchangeType: Exchange type ("okx")
func (t *OKXTrader) SyncOrdersFromOKX(traderID string, exchangeID string, exchangeType string, st *store.Store) error {
	if st == nil {
		return fmt.Errorf("store is nil")
	}

	// Get recent trades (last 24 hours)
	startTime := time.Now().Add(-24 * time.Hour)

	logger.Infof("🔄 Syncing OKX trades from: %s", startTime.Format(time.RFC3339))

	// Use GetTrades method to fetch trade records
	trades, err := t.GetTrades(startTime, 100)
	if err != nil {
		return fmt.Errorf("failed to get trades: %w", err)
	}

	logger.Infof("📥 Received %d trades from OKX", len(trades))

	// Sort trades by time ASC (oldest first) for proper position building
	sort.SliceStable(trades, func(i, j int) bool {
		return trades[i].ExecTime.UnixMilli() < trades[j].ExecTime.UnixMilli()
	})

	syncedCount := 0
	for _, trade := range trades {
		created, err := t.persistOKXTrade(st, traderID, exchangeID, exchangeType, trade)
		if err != nil {
			// Stop at the first failure: later fills must not be applied to the
			// position ledger out of order. Nothing of this fill was committed,
			// so the next cycle retries it.
			return fmt.Errorf("persist OKX trade %s: %w", trade.TradeID, err)
		}
		if created {
			syncedCount++
		}
	}

	logger.Infof("✅ OKX order sync completed: %d new trades synced", syncedCount)
	return nil
}

// persistOKXTrade writes one fill's order, fill and position effects in a
// single transaction. It returns false for an already-recorded fill.
func (t *OKXTrader) persistOKXTrade(st *store.Store, traderID, exchangeID, exchangeType string, trade OKXTrade) (bool, error) {
	if trade.TradeID == "" || trade.Symbol == "" || trade.FillPrice <= 0 || trade.FillQtyBase <= 0 {
		return false, fmt.Errorf("invalid fill identity/quantity")
	}
	symbol := market.Normalize(trade.Symbol)
	side := strings.ToUpper(trade.Side)
	created, recordedAction := false, ""
	err := st.Transaction(func(tx *gorm.DB) error {
		created = false
		// Check if trade already exists (use exchangeID which is UUID, not exchange type)
		var count int64
		if err := tx.Model(&store.TraderOrder{}).Where("exchange_id = ? AND exchange_order_id = ?", exchangeID, trade.TradeID).Count(&count).Error; err != nil {
			return err
		}
		if count > 0 {
			return nil
		}
		if err := tx.Model(&store.TraderFill{}).Where("exchange_id = ? AND symbol = ? AND exchange_trade_id = ?", exchangeID, symbol, trade.TradeID).Count(&count).Error; err != nil {
			return err
		}
		if count > 0 {
			return nil
		}

		var legs []types.OneWayLeg
		if trade.OrderAction != "" {
			legs = []types.OneWayLeg{{Action: trade.OrderAction, Quantity: trade.FillQtyBase}}
		} else {
			positions := store.NewPositionStore(tx)
			var open [2]float64
			for i, ps := range []string{"LONG", "SHORT"} {
				p, err := positions.GetOpenPositionBySymbol(traderID, symbol, ps)
				if err != nil {
					return err
				}
				if p != nil && p.Quantity > 0 {
					open[i] = p.Quantity
				}
			}
			legs = types.ClassifyOneWayFill(side, trade.FillQtyBase, trade.FillPnL, open[0], open[1])
		}
		action, positionSide := okxHistoryOrderActionReview, "BOTH"
		if len(legs) > 0 {
			action = legs[0].Action
			positionSide = "LONG"
			if strings.Contains(action, "short") {
				positionSide = "SHORT"
			}
		} else {
			logger.Warnf("OKX history fill %s %s: net-mode direction undetermined; stored as review without position update", symbol, trade.TradeID)
		}

		// Create order record - use UTC time in milliseconds to avoid timezone issues
		execTimeMs := trade.ExecTime.UTC().UnixMilli()
		orderRecord := &store.TraderOrder{
			TraderID:        traderID,
			ExchangeID:      exchangeID,   // UUID
			ExchangeType:    exchangeType, // Exchange type
			ExchangeOrderID: trade.TradeID,
			Symbol:          symbol,
			Side:            side,
			PositionSide:    positionSide,
			Type:            trade.OrderType,
			OrderAction:     action,
			Quantity:        trade.FillQtyBase,
			Price:           trade.FillPrice,
			Status:          "FILLED",
			FilledQuantity:  trade.FillQtyBase,
			AvgFillPrice:    trade.FillPrice,
			Commission:      trade.Fee,
			FilledAt:        execTimeMs,
			CreatedAt:       execTimeMs,
			UpdatedAt:       execTimeMs,
		}
		if err := tx.Create(orderRecord).Error; err != nil {
			return err
		}

		// Create fill record - use UTC time in milliseconds
		fillRecord := &store.TraderFill{
			TraderID:        traderID,
			ExchangeID:      exchangeID,   // UUID
			ExchangeType:    exchangeType, // Exchange type
			OrderID:         orderRecord.ID,
			ExchangeOrderID: trade.OrderID,
			ExchangeTradeID: trade.TradeID,
			Symbol:          symbol,
			Side:            side,
			Price:           trade.FillPrice,
			Quantity:        trade.FillQtyBase,
			QuoteQuantity:   trade.FillPrice * trade.FillQtyBase,
			Commission:      trade.Fee,
			CommissionAsset: trade.FeeAsset,
			RealizedPnL:     0, // unchanged: PositionBuilder derives PnL for OKX
			IsMaker:         trade.IsMaker,
			CreatedAt:       execTimeMs,
		}
		if err := tx.Create(fillRecord).Error; err != nil {
			return err
		}

		// Create/update position record using PositionBuilder
		builder := store.NewPositionBuilder(store.NewPositionStore(tx))
		for _, leg := range legs {
			legSide := "LONG"
			if strings.Contains(leg.Action, "short") {
				legSide = "SHORT"
			}
			fee := trade.Fee * leg.Quantity / trade.FillQtyBase
			if err := builder.ProcessTrade(
				traderID, exchangeID, exchangeType,
				symbol, legSide, leg.Action,
				leg.Quantity, trade.FillPrice, fee, 0, // No per-trade PnL from OKX
				execTimeMs, trade.TradeID,
			); err != nil {
				return err
			}
		}
		created, recordedAction = true, action
		return nil
	})
	if err != nil {
		return false, err
	}
	if created {
		logger.Infof("  ✅ Synced trade: %s %s %s qty=%.6f price=%.6f fee=%.6f action=%s",
			trade.TradeID, trade.Symbol, side, trade.FillQtyBase, trade.FillPrice, trade.Fee, recordedAction)
	}
	return created, nil
}

// StartOrderSync starts background order sync task for OKX
func (t *OKXTrader) StartOrderSync(traderID string, exchangeID string, exchangeType string, st *store.Store, interval time.Duration, stop <-chan struct{}) {
	syncloop.Run(stop, interval, "OKX", func() error {
		return t.SyncOrdersFromOKX(traderID, exchangeID, exchangeType, st)
	})
}
