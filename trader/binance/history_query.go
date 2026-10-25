package binance

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/adshao/go-binance/v2/common"
	"github.com/adshao/go-binance/v2/futures"
	"io"
	"math"
	"net/http"
	"net/url"
	"nofx/trader/types"
	"strconv"
	"strings"
	"time"
)

// go-binance v2.8.9 encodes AccountTrade.FromID as "fromID". Binance's
// /fapi/v1/userTrades requires "fromId". Keep this workaround endpoint-specific,
// using the same client, clock offset, signing implementation and timeout.
func (t *FuturesTrader) accountTradesFromID(ctx context.Context, symbol string, fromID int64, limit int) ([]*futures.AccountTrade, error) {
	if fromID < 1 || limit < 1 || limit > 1000 || symbol == "" {
		return nil, fmt.Errorf("invalid history cursor")
	}
	q := url.Values{"symbol": {symbol}, "fromId": {strconv.FormatInt(fromID, 10)}, "limit": {strconv.Itoa(limit)}, "timestamp": {strconv.FormatInt(time.Now().UnixMilli()-t.client.TimeOffset, 10)}, "recvWindow": {"5000"}}
	keyType := t.client.KeyType
	if keyType == "" {
		keyType = common.KeyTypeHmac
	}
	signer, err := common.SignFunc(keyType)
	if err != nil {
		return nil, fmt.Errorf("unsupported history signing configuration")
	}
	// Same layout as the SDK: sign the exact query string and append the
	// signature last, so the server verifies the bytes that were signed.
	qs := q.Encode()
	signature, err := signer(t.client.SecretKey, qs)
	if err != nil {
		return nil, fmt.Errorf("history signing failed")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(t.client.BaseURL, "/")+"/fapi/v1/userTrades?"+qs+"&signature="+url.QueryEscape(*signature), nil)
	if err != nil {
		return nil, fmt.Errorf("invalid history endpoint")
	}
	req.Header.Set("X-MBX-APIKEY", t.client.APIKey)
	client := t.client.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	response, err := client.Do(req)
	if err != nil {
		return nil, &stopHistoryError{cause: err}
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(response.Body, 4<<20))
	if err != nil {
		return nil, fmt.Errorf("history response read failed")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		var apiErr common.APIError
		if json.Unmarshal(payload, &apiErr) != nil {
			return nil, fmt.Errorf("history HTTP status %d", response.StatusCode)
		}
		return nil, &stopHistoryError{cause: &apiErr}
	}
	var rows []*futures.AccountTrade
	if err = json.Unmarshal(payload, &rows); err != nil {
		return nil, fmt.Errorf("malformed trade history response")
	}
	return rows, nil
}

func decodeAccountTrades(accountTrades []*futures.AccountTrade) ([]types.TradeRecord, error) {
	var trades []types.TradeRecord
	for _, at := range accountTrades {
		if at == nil {
			return nil, fmt.Errorf("empty history trade")
		}
		price, err := types.ParseFloatField("history price", at.Price)
		if err != nil {
			return nil, err
		}
		qty, err := types.ParseFloatField("history quantity", at.Quantity)
		if err != nil {
			return nil, err
		}
		fee, err := types.ParseFloatField("history commission", at.Commission)
		if err != nil {
			return nil, err
		}
		pnl, err := types.ParseFloatField("history realized PnL", at.RealizedPnl)
		if err != nil {
			return nil, err
		}
		for _, value := range []float64{price, qty, fee, pnl} {
			if math.IsNaN(value) || math.IsInf(value, 0) {
				return nil, fmt.Errorf("non-finite trade history field")
			}
		}

		trade := types.TradeRecord{
			TradeID:      strconv.FormatInt(at.ID, 10),
			Symbol:       at.Symbol,
			Side:         string(at.Side),
			PositionSide: string(at.PositionSide),
			Price:        price,
			Quantity:     qty,
			RealizedPnL:  pnl,
			Fee:          fee,
			Time:         time.UnixMilli(at.Time).UTC(),
			FeeAsset:     strings.ToUpper(strings.TrimSpace(at.CommissionAsset)),
		}
		if at.OrderID > 0 {
			trade.OrderID = strconv.FormatInt(at.OrderID, 10)
		}
		trades = append(trades, trade)
	}

	return trades, nil
}
