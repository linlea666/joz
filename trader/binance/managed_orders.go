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

// MarketRules returns verified trading rules, reusing a per-symbol TTL cache
// (see market_rules_cache.go). Each call gets its own copy.
func (t *FuturesTrader) MarketRules(symbol string) (*types.ManagedMarketRules, error) {
	e, found, err := t.loadSymbol(symbol)
	if !found {
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("%w: %s is not listed on Binance Futures", types.ErrInstrumentUnavailable, symbol)
	}
	r := e.rules
	// Rejected rules are returned for diagnostics only; callers must still
	// honor the error and cannot use them for order submission.
	return &r, err
}

func (t *FuturesTrader) SubmitManagedOrder(req *types.ManagedOrderRequest) (*types.ManagedOrderResult, error) {
	svc, err := t.prepareManagedOrder(req)
	if err != nil {
		// Local validation failed: the request never reached Binance.
		return nil, types.NotSent(err)
	}
	// In hedge mode side+positionSide encode a close; reduceOnly is prohibited.
	o, err := svc.Do(context.Background())
	if err != nil {
		return nil, classifyManagedRejection(err)
	}
	t.InvalidatePositionCache()
	return &types.ManagedOrderResult{OrderID: strconv.FormatInt(o.OrderID, 10), ClientID: o.ClientOrderID, Symbol: o.Symbol, Type: string(o.Type), Side: string(o.Side), PositionSide: string(o.PositionSide), Status: string(o.Status), Quantity: managedNumber(o.OrigQuantity), ExecutedQty: managedNumber(o.ExecutedQuantity), AvgPrice: managedNumber(o.AvgPrice), Price: managedNumber(o.Price)}, nil
}

func (t *FuturesTrader) prepareManagedOrder(req *types.ManagedOrderRequest) (*futures.CreateOrderService, error) {
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
	return svc, nil
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

// definiteRejectionCodes are Binance responses that prove the request did not
// create an order. Codes absent here (-1007 backend timeout, 5xx bodies,
// transport errors) remain "outcome unknown" and must be reconciled by
// client ID. retryable marks transient causes where resubmitting the same
// intent later is safe and expected to succeed.
var definiteRejectionCodes = map[int64]bool{
	// Request validation / parameter errors.
	-1100: false, -1102: false, -1111: false, -1116: false, -1117: false, -1121: false,
	-2019: false, -2020: false, -2021: false, -2022: false, -2027: false,
	-4003: false, -4004: false, -4005: false, -4014: false, -4023: false, -4164: false, -4509: false,
	// Position side does not match the account position mode.
	-4061: false,
	// A closePosition stop/TP in this direction already exists.
	-4130: false,
	// Maximum number of stop/conditional orders reached.
	-4045: false,
	// Limit price above the mark-price cap / below the floor.
	-4016: false, -4024: false,
	// Counterparty best price outside PERCENT_PRICE protection.
	-4131: false,
	// Transient or account-level request refusals: never reached the engine.
	-1003: true, // too many requests / IP banned (HTTP 429/418 body)
	-1021: true, // timestamp outside recvWindow
	-1022: true, // invalid signature
	-2015: true, // invalid API key, IP or permissions
}

func classifyManagedRejection(err error) error {
	var apiErr *common.APIError
	if errors.As(err, &apiErr) && apiErr.Code != 0 {
		if retryable, ok := definiteRejectionCodes[apiErr.Code]; ok {
			return &types.ManagedRejection{Code: apiErr.Code, Cause: err, Retryable: retryable}
		}
	}
	return err
}
