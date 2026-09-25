package types

import (
	"errors"
	"fmt"
	"math"
	"math/big"
	"strconv"
)

var ErrManagedOrderNotFound = errors.New("managed order not found")
var ErrManagedOrderRejected = errors.New("managed order definitely rejected")

// ManagedOrderTrader is optional: ordinary Trader/Grid callers keep their
// existing behavior. These methods never implicitly cancel another order.
type ManagedOrderTrader interface {
	MarketRules(symbol string) (*ManagedMarketRules, error)
	SubmitManagedOrder(*ManagedOrderRequest) (*ManagedOrderResult, error)
	GetManagedOrder(symbol, orderID, clientID string) (*ManagedOrderResult, error)
	CancelManagedOrder(symbol, orderID, clientID string) (*ManagedOrderResult, error)
}

type FreshPositionReader interface {
	GetFreshPositions() ([]map[string]interface{}, error)
}

type ManagedMarketRules struct {
	Symbol, Status, ContractType, QuoteAsset                 string
	QuantityStep, MinQuantity, MaxQuantity                   float64
	MarketQuantityStep, MarketMinQuantity, MarketMaxQuantity float64
	PriceTick, MinPrice, MaxPrice, MinNotional               float64
}

type ManagedOrderRequest struct {
	Symbol, Type, Side, PositionSide, ClientID string
	Quantity, Price, ReferencePrice            float64
	ReduceOnly                                 bool
	Rules                                      *ManagedMarketRules
}

type ManagedOrderResult struct {
	OrderID, ClientID, Symbol, Type, Side, PositionSide, Status string
	Quantity, ExecutedQty, AvgPrice, Price                      float64
}

// DecimalFloor avoids both binary step drift and rounding risk upwards.
func DecimalFloor(value, step float64) (float64, error) {
	if value <= 0 || step <= 0 || math.IsInf(value, 0) || math.IsNaN(value) || math.IsInf(step, 0) || math.IsNaN(step) {
		return 0, fmt.Errorf("invalid value/step")
	}
	v, _ := new(big.Rat).SetString(strconv.FormatFloat(value, 'f', -1, 64))
	s, _ := new(big.Rat).SetString(strconv.FormatFloat(step, 'f', -1, 64))
	n := new(big.Rat).Quo(v, s)
	whole := new(big.Int).Quo(n.Num(), n.Denom())
	result, _ := new(big.Rat).Mul(new(big.Rat).SetInt(whole), s).Float64()
	return result, nil
}

func (r *ManagedMarketRules) FloorQuantity(q float64, market bool) (float64, error) {
	if r == nil {
		return 0, fmt.Errorf("market rules unavailable")
	}
	step, min, max := r.QuantityStep, r.MinQuantity, r.MaxQuantity
	if market && r.MarketQuantityStep > 0 {
		step, min, max = r.MarketQuantityStep, r.MarketMinQuantity, r.MarketMaxQuantity
	}
	v, err := DecimalFloor(q, step)
	if err != nil {
		return 0, err
	}
	if v <= 0 || v < min || (max > 0 && v > max) {
		return 0, fmt.Errorf("quantity %.12g outside exchange limits", v)
	}
	return v, nil
}

func (r *ManagedMarketRules) NormalizePrice(p float64) (float64, error) {
	if r == nil {
		return 0, fmt.Errorf("market rules unavailable")
	}
	v, err := DecimalFloor(p, r.PriceTick)
	if err != nil {
		return 0, err
	}
	if v <= 0 || v < r.MinPrice || (r.MaxPrice > 0 && v > r.MaxPrice) {
		return 0, fmt.Errorf("price outside exchange limits")
	}
	return v, nil
}
