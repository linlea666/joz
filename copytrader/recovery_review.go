package copytrader

import (
	"encoding/json"
	"errors"
	"fmt"
	"nofx/store"
	"nofx/trader/types"
	"time"
)

func (e *Engine) RecoveryReview(contextID, actor, ticket, reason string) (*store.CopyTradeRecoveryReview, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.exec.RecoveryReview(contextID, actor, ticket, reason)
}
func (x *Executor) RecoveryReview(contextID, actor, ticket, reason string) (*store.CopyTradeRecoveryReview, error) {
	return x.st.CopyTrade().RecoveryReview(x.traderID, contextID, actor, ticket, reason, x.verifyHistoricalRecovery)
}
func (x *Executor) verifyHistoricalRecovery(c *store.CopyTradeContext) (string, error) {
	var pending stopIntent
	if err := json.Unmarshal([]byte(c.StopIntentJSON), &pending); err != nil {
		return "", err
	}
	if pending.Stage != "UNKNOWN" || pending.ClientID == "" || pending.ErrorAt != nil || pending.ErrorCode != 0 {
		return "", fmt.Errorf("only legacy unknown stop requests lacking historical evidence are eligible")
	}
	reader, ok := x.ex.(types.StopOrderReader)
	if !ok {
		return "", fmt.Errorf("exchange cannot verify stop history")
	}
	if err := x.verifyFlat(c); err != nil {
		return "", err
	}
	result, err := reader.GetStopOrder(c.Symbol, "", pending.ClientID)
	if !errors.Is(err, types.ErrManagedOrderNotFound) {
		if err != nil {
			return "", fmt.Errorf("stop history verification failed (%T)", err)
		}
		if result != nil {
			return "", fmt.Errorf("stop history is available; automatic reconciliation must resolve it")
		}
		return "", fmt.Errorf("empty stop history response is not proof of absence")
	}
	orders, err := x.ex.GetOpenOrders(c.Symbol)
	if err != nil {
		return "", fmt.Errorf("open-order verification failed (%T)", err)
	}
	// Include unattributed and opposite-side orders conservatively. They may
	// belong to an account mode that cannot safely be separated by direction.
	for _, o := range orders {
		if o.Symbol == "" || o.Symbol == c.Symbol {
			return "", fmt.Errorf("related or unattributed open orders remain")
		}
	}
	if err := x.verifyFlat(c); err != nil {
		return "", err
	}
	b, _ := json.Marshal(map[string]interface{}{"checked_at": time.Now().UTC(), "symbol": c.Symbol, "direction": c.Direction, "position_quantity": 0, "open_order_count": 0, "unknown_client_id": pending.ClientID, "history": "not_found; original submission outcome remains unknown"})
	return string(b), nil
}
func (x *Executor) verifyFlat(c *store.CopyTradeContext) error {
	pos, err := x.freshPosition(c.Symbol, c.Direction)
	if err != nil {
		return fmt.Errorf("position verification failed (%T)", err)
	}
	if pos != nil && pos.qty > 0 {
		return fmt.Errorf("position still exists")
	}
	return nil
}
