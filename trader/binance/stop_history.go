package binance

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"nofx/trader/types"

	"github.com/adshao/go-binance/v2/common"
)

var _ types.StopOrderReader = (*FuturesTrader)(nil)

func (t *FuturesTrader) GetStopOrder(symbol, orderID, clientID string) (*types.StopOrderResult, error) {
	svc := t.client.NewGetAlgoOrderService()
	if orderID != "" {
		id, err := strconv.ParseInt(orderID, 10, 64)
		if err != nil || id <= 0 {
			return nil, fmt.Errorf("invalid algo order identity")
		}
		svc.AlgoID(id)
	} else if clientID != "" {
		svc.ClientAlgoID(clientID)
	} else {
		return nil, fmt.Errorf("stop order identity required")
	}
	o, err := svc.Do(context.Background())
	if err != nil {
		var apiErr *common.APIError
		if errors.As(err, &apiErr) && apiErr.Code == -2013 {
			return nil, fmt.Errorf("%w: stop history unavailable", types.ErrManagedOrderNotFound)
		}
		return nil, &stopHistoryError{cause: err}
	}
	id := strconv.FormatInt(o.AlgoId, 10)
	if o.Symbol != symbol || (orderID != "" && id != orderID) || (clientID != "" && o.ClientAlgoId != clientID) {
		return nil, fmt.Errorf("stop history identity mismatch")
	}
	result := &types.StopOrderResult{OpenOrder: types.OpenOrder{OrderID: id, ClientID: o.ClientAlgoId, OrderKind: "ALGO", Symbol: o.Symbol, Type: string(o.OrderType), Side: string(o.Side), PositionSide: string(o.PositionSide), StopPrice: managedNumber(o.TriggerPrice), Quantity: managedNumber(o.Quantity), ClosePosition: o.ClosePosition}, Status: o.AlgoStatus, ActualOrderID: o.ActualOrderId}
	// A triggered algo may have a still-live child. Never infer terminality from
	// the algo status alone or cancel only the parent and abandon the child.
	if o.ActualOrderId != "" && o.ActualOrderId != "0" {
		child, err := t.GetManagedOrder(symbol, o.ActualOrderId, "")
		if err != nil {
			return nil, fmt.Errorf("stop child history: %w", &stopHistoryError{cause: err})
		}
		if child.OrderID != o.ActualOrderId || child.Symbol != symbol || child.Side != string(o.Side) || child.PositionSide != string(o.PositionSide) {
			return nil, fmt.Errorf("stop child identity mismatch")
		}
		result.ActualOrderStatus = child.Status
	}
	return result, nil
}

// net/url errors can contain a signed request URL; diagnostics need only the
// category/code. Keep the cause for errors.Is/As without rendering credentials.
type stopHistoryError struct{ cause error }

func (e *stopHistoryError) Error() string {
	var apiErr *common.APIError
	if errors.As(e.cause, &apiErr) {
		return fmt.Sprintf("stop history request rejected (code=%d)", apiErr.Code)
	}
	return fmt.Sprintf("stop history request failed (%T)", e.cause)
}
func (e *stopHistoryError) Unwrap() error { return e.cause }
