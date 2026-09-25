package binance

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/adshao/go-binance/v2/common"
	"github.com/adshao/go-binance/v2/futures"
	"nofx/trader/types"
)

var _ types.ManagedOrderTrader = (*FuturesTrader)(nil)

func managedNumber(v interface{}) float64 {
	s := fmt.Sprint(v)
	n, _ := strconv.ParseFloat(s, 64)
	return n
}

func (t *FuturesTrader) MarketRules(symbol string) (*types.ManagedMarketRules, error) {
	info, err := t.client.NewExchangeInfoService().Do(context.Background())
	if err != nil {
		return nil, err
	}
	for _, s := range info.Symbols {
		if s.Symbol != symbol {
			continue
		}
		r := &types.ManagedMarketRules{Symbol: s.Symbol, Status: s.Status, ContractType: string(s.ContractType), QuoteAsset: s.QuoteAsset}
		for _, f := range s.Filters {
			switch f["filterType"] {
			case "LOT_SIZE":
				r.QuantityStep = managedNumber(f["stepSize"])
				r.MinQuantity = managedNumber(f["minQty"])
				r.MaxQuantity = managedNumber(f["maxQty"])
			case "MARKET_LOT_SIZE":
				r.MarketQuantityStep = managedNumber(f["stepSize"])
				r.MarketMinQuantity = managedNumber(f["minQty"])
				r.MarketMaxQuantity = managedNumber(f["maxQty"])
			case "PRICE_FILTER":
				r.PriceTick = managedNumber(f["tickSize"])
				r.MinPrice = managedNumber(f["minPrice"])
				r.MaxPrice = managedNumber(f["maxPrice"])
			case "MIN_NOTIONAL":
				r.MinNotional = managedNumber(f["notional"])
			}
		}
		if r.Status != "TRADING" || r.ContractType != "PERPETUAL" || r.QuoteAsset != "USDT" || r.QuantityStep <= 0 || r.PriceTick <= 0 {
			return nil, fmt.Errorf("%s is not a supported tradable USDT perpetual or has incomplete rules", symbol)
		}
		return r, nil
	}
	return nil, fmt.Errorf("%s is not listed on Binance Futures", symbol)
}

func (t *FuturesTrader) SubmitManagedOrder(req *types.ManagedOrderRequest) (*types.ManagedOrderResult, error) {
	if req == nil || req.Rules == nil || req.Rules.Symbol != req.Symbol || req.Rules.Status != "TRADING" {
		return nil, fmt.Errorf("verified market rules required")
	}
	if req.ClientID == "" || len(req.ClientID) > 32 || strings.ContainsAny(req.ClientID, " \t\n") {
		return nil, fmt.Errorf("invalid stable client ID")
	}
	if (req.Side != "BUY" && req.Side != "SELL") || (req.PositionSide != "LONG" && req.PositionSide != "SHORT") {
		return nil, fmt.Errorf("explicit order and position sides required")
	}
	closing := (req.Side == "SELL" && req.PositionSide == "LONG") || (req.Side == "BUY" && req.PositionSide == "SHORT")
	if req.ReduceOnly != closing {
		return nil, fmt.Errorf("reduce intent contradicts position side")
	}
	market := req.Type == "MARKET"
	if !market && req.Type != "LIMIT" {
		return nil, fmt.Errorf("unsupported managed order type")
	}
	qty, err := req.Rules.FloorQuantity(req.Quantity, market)
	if err != nil {
		return nil, err
	}
	price := req.ReferencePrice
	if !market {
		price, err = req.Rules.NormalizePrice(req.Price)
		if err != nil {
			return nil, err
		}
	}
	if !req.ReduceOnly && (price <= 0 || qty*price < req.Rules.MinNotional) {
		return nil, fmt.Errorf("order below minimum notional")
	}
	svc := t.client.NewCreateOrderService().Symbol(req.Symbol).Side(futures.SideType(req.Side)).PositionSide(futures.PositionSideType(req.PositionSide)).Type(futures.OrderType(req.Type)).Quantity(strconv.FormatFloat(qty, 'f', -1, 64)).NewClientOrderID(req.ClientID)
	if !market {
		svc.TimeInForce(futures.TimeInForceTypeGTC).Price(strconv.FormatFloat(price, 'f', -1, 64))
	}
	// In hedge mode side+positionSide encode a close; reduceOnly is prohibited.
	o, err := svc.Do(context.Background())
	if err != nil {
		var apiErr *common.APIError
		if errors.As(err, &apiErr) {
			switch apiErr.Code {
			case -1100, -1102, -1111, -1116, -1117, -1121, -2019, -2020, -2021, -2022, -2027, -4003, -4004, -4005, -4014, -4023, -4164:
				return nil, fmt.Errorf("%w: %v", types.ErrManagedOrderRejected, err)
			}
		}
		return nil, err
	}
	t.InvalidatePositionCache()
	return &types.ManagedOrderResult{OrderID: strconv.FormatInt(o.OrderID, 10), ClientID: o.ClientOrderID, Symbol: o.Symbol, Type: string(o.Type), Side: string(o.Side), PositionSide: string(o.PositionSide), Status: string(o.Status), Quantity: managedNumber(o.OrigQuantity), ExecutedQty: managedNumber(o.ExecutedQuantity), AvgPrice: managedNumber(o.AvgPrice), Price: managedNumber(o.Price)}, nil
}

func (t *FuturesTrader) GetManagedOrder(symbol, orderID, clientID string) (*types.ManagedOrderResult, error) {
	s := t.client.NewGetOrderService().Symbol(symbol)
	if orderID != "" {
		id, err := strconv.ParseInt(orderID, 10, 64)
		if err != nil {
			return nil, err
		}
		s.OrderID(id)
	} else if clientID != "" {
		s.OrigClientOrderID(clientID)
	} else {
		return nil, fmt.Errorf("order identity required")
	}
	o, err := s.Do(context.Background())
	if err != nil {
		var apiErr *common.APIError
		if errors.As(err, &apiErr) && apiErr.Code == -2013 {
			return nil, fmt.Errorf("%w: %s", types.ErrManagedOrderNotFound, clientID)
		}
		return nil, err
	}
	return &types.ManagedOrderResult{OrderID: strconv.FormatInt(o.OrderID, 10), ClientID: o.ClientOrderID, Symbol: o.Symbol, Type: string(o.Type), Side: string(o.Side), PositionSide: string(o.PositionSide), Status: string(o.Status), Quantity: managedNumber(o.OrigQuantity), ExecutedQty: managedNumber(o.ExecutedQuantity), AvgPrice: managedNumber(o.AvgPrice), Price: managedNumber(o.Price)}, nil
}

func (t *FuturesTrader) CancelManagedOrder(symbol, orderID, clientID string) (*types.ManagedOrderResult, error) {
	s := t.client.NewCancelOrderService().Symbol(symbol)
	if orderID != "" {
		id, err := strconv.ParseInt(orderID, 10, 64)
		if err != nil {
			return nil, err
		}
		s.OrderID(id)
	} else if clientID != "" {
		s.OrigClientOrderID(clientID)
	} else {
		return nil, fmt.Errorf("order identity required")
	}
	_, err := s.Do(context.Background())
	if err != nil {
		return nil, err
	}
	t.InvalidatePositionCache()
	return t.GetManagedOrder(symbol, orderID, clientID)
}

func (t *FuturesTrader) GetFreshPositions() ([]map[string]interface{}, error) {
	t.InvalidatePositionCache()
	return t.GetPositions()
}
