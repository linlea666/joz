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

// ErrManagedOrderNotSent marks a failure that happened before the order
// request reached the venue (market-rule lookup, local normalization). No
// order can exist for its client ID. It also satisfies ErrManagedOrderRejected
// so ledger callers record a terminal, never-created order instead of UNKNOWN.
var ErrManagedOrderNotSent = errors.New("managed order not sent")

// ErrManagedOrderRetryable marks an outcome that proves no order was created
// AND is expected to be transient (not sent, rate limit, clock skew, auth).
// Retrying with the same intent is safe; it never applies to UNKNOWN results.
var ErrManagedOrderRetryable = errors.New("managed order not executed; retry is safe")

// ManagedRejection preserves the venue error while retaining sentinel compatibility.
type ManagedRejection struct {
	Code      int64
	Cause     error
	Retryable bool
}

func (e *ManagedRejection) Error() string {
	return fmt.Sprintf("%v: %v", ErrManagedOrderRejected, e.Cause)
}
func (e *ManagedRejection) Unwrap() []error {
	if e.Retryable {
		return []error{ErrManagedOrderRejected, ErrManagedOrderRetryable, e.Cause}
	}
	return []error{ErrManagedOrderRejected, e.Cause}
}

// ManagedNotSent wraps a pre-send failure; see ErrManagedOrderNotSent.
type ManagedNotSent struct{ Cause error }

func (e *ManagedNotSent) Error() string {
	return fmt.Sprintf("%v: %v", ErrManagedOrderNotSent, e.Cause)
}
func (e *ManagedNotSent) Unwrap() []error {
	return []error{ErrManagedOrderNotSent, ErrManagedOrderRejected, ErrManagedOrderRetryable, e.Cause}
}

// NotSent wraps err as a pre-send failure (nil stays nil).
func NotSent(err error) error {
	if err == nil {
		return nil
	}
	return &ManagedNotSent{Cause: err}
}

// StopOrderReader is optional. A missing history row is NOT evidence of rejection.
// Implementations query using the executor's authenticated account credentials.
type StopOrderReader interface {
	GetStopOrder(symbol, orderID, clientID string) (*StopOrderResult, error)
}
type StopOrderResult struct {
	OpenOrder
	Status            string
	ActualOrderID     string
	ActualOrderStatus string
}

func (r *StopOrderResult) Terminal() bool {
	switch r.Status {
	case "CANCELED", "EXPIRED", "REJECTED", "FINISHED":
		if r.ActualOrderID != "" && r.ActualOrderID != "0" {
			switch r.ActualOrderStatus {
			case "FILLED", "CANCELED", "EXPIRED", "EXPIRED_IN_MATCH", "REJECTED":
				return true
			}
			return false
		}
		return r.Status != "FINISHED"
	}
	return false
}

var ErrInstrumentUnavailable = errors.New("instrument explicitly unavailable")

// MarketRulesReader is optional and does not imply managed entry support.
// Quantities are always base-asset units, including contract venues.
// Rejected observed rules may accompany an error for diagnostics only;
// execution callers must reject every non-nil error, even with non-nil rules.
type MarketRulesReader interface {
	MarketRules(symbol string) (*ManagedMarketRules, error)
}

// StopOrderTrader adds stable identities without changing legacy Trader users.
type StopOrderTrader interface {
	SetManagedStopLoss(symbol, side string, qty, price float64, clientID string) error
	CancelStopOrder(symbol string, order OpenOrder) error
}

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
	// PERCENT_PRICE multipliers relative to the mark price (0 = unknown).
	PercentPriceUp, PercentPriceDown float64
}

// LimitPriceAllowed reports whether a LIMIT price passes the static
// PRICE_FILTER bounds and the documented PERCENT_PRICE rule (BUY: price <=
// mark*multiplierUp; SELL: price >= mark*multiplierDown). Unknown rules or a
// non-positive reference price never block submission.
func (r *ManagedMarketRules) LimitPriceAllowed(side string, price, reference float64) bool {
	if r == nil || price <= 0 {
		return true
	}
	if (r.MinPrice > 0 && price < r.MinPrice) || (r.MaxPrice > 0 && price > r.MaxPrice) {
		return false
	}
	if reference <= 0 || math.IsNaN(reference) || math.IsInf(reference, 0) {
		return true
	}
	switch side {
	case "BUY":
		return r.PercentPriceUp <= 0 || price <= reference*r.PercentPriceUp
	case "SELL":
		return r.PercentPriceDown <= 0 || price >= reference*r.PercentPriceDown
	}
	return true
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
