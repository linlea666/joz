package store

import (
	"encoding/json"
	"errors"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// CopyTradeAction is a durable intent. Its ID is semantic, not an instruction
// array index or message revision, so editing a recap cannot reduce twice.
type CopyTradeAction struct {
	ID          string    `gorm:"primaryKey" json:"id"`
	TraderID    string    `gorm:"index;not null" json:"trader_id"`
	SignalID    string    `gorm:"index" json:"signal_id"`
	MessageID   string    `gorm:"index" json:"message_id"`
	ContextID   string    `gorm:"index" json:"context_id"`
	Symbol      string    `json:"symbol"`
	Direction   string    `json:"direction"`
	Action      string    `json:"action"`
	Status      string    `gorm:"index" json:"status"`
	PayloadJSON string    `json:"payload_json"`
	Error       string    `json:"error,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

func (CopyTradeAction) TableName() string { return "copytrade_actions" }

// CopyTradeOrder contains actual cumulative fills, never planned quantities
// masquerading as fills. PLANNED has not been submitted; SUBMITTING/UNKNOWN
// requires querying the stable client ID before any retry.
type CopyTradeOrder struct {
	ID          string    `gorm:"primaryKey" json:"id"`
	TraderID    string    `gorm:"index;not null" json:"trader_id"`
	ContextID   string    `gorm:"index;not null" json:"context_id"`
	SignalID    string    `gorm:"index" json:"signal_id"`
	Role        string    `json:"role"`
	Symbol      string    `json:"symbol"`
	Direction   string    `json:"direction"`
	ClientID    string    `gorm:"uniqueIndex" json:"client_id"`
	OrderID     string    `json:"order_id"`
	OrderType   string    `json:"order_type"`
	Price       float64   `json:"price"`
	Quantity    float64   `json:"quantity"`
	ExecutedQty float64   `json:"executed_qty"`
	AvgPrice    float64   `json:"avg_price"`
	Status      string    `gorm:"index" json:"status"`
	LastError   string    `json:"last_error,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

func (CopyTradeOrder) TableName() string { return "copytrade_orders" }

func (s *CopyTradeStore) ClaimAction(a *CopyTradeAction) (bool, error) {
	r := s.db.Clauses(clause.OnConflict{DoNothing: true}).Create(a)
	return r.RowsAffected == 1, r.Error
}

func (s *CopyTradeStore) GetAction(id string) (*CopyTradeAction, error) {
	var a CopyTradeAction
	err := s.db.First(&a, "id = ?", id).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	return &a, err
}

func (s *CopyTradeStore) UpdateAction(id string, updates map[string]interface{}) error {
	return s.db.Model(&CopyTradeAction{}).Where("id = ?", id).Updates(updates).Error
}

func (s *CopyTradeStore) GetActionsForMessage(traderID, messageID string) ([]*CopyTradeAction, error) {
	var rows []*CopyTradeAction
	err := s.db.Where("trader_id = ? AND message_id = ?", traderID, messageID).Order("created_at ASC, id ASC").Find(&rows).Error
	return rows, err
}

func (s *CopyTradeStore) GetOrdersForContext(traderID, contextID string) ([]*CopyTradeOrder, error) {
	var rows []*CopyTradeOrder
	err := s.db.Where("trader_id = ? AND context_id = ?", traderID, contextID).Order("created_at ASC, id ASC").Find(&rows).Error
	return rows, err
}

func (s *CopyTradeStore) GetOrdersForSignal(traderID, signalID string) ([]*CopyTradeOrder, error) {
	var rows []*CopyTradeOrder
	err := s.db.Where("trader_id = ? AND signal_id = ?", traderID, signalID).Order("created_at ASC, id ASC").Find(&rows).Error
	return rows, err
}

func (s *CopyTradeStore) CreateManagedPlan(ctx *CopyTradeContext, orders []*CopyTradeOrder) error {
	return s.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(ctx).Error; err != nil {
			return err
		}
		if err := tx.Create(&orders).Error; err != nil {
			return err
		}
		if len(orders) > 0 {
			return tx.Model(&CopyTradeAction{}).Where("signal_id = ? AND symbol = ? AND direction = ? AND action IN ?", orders[0].SignalID, ctx.Symbol, ctx.Direction, []string{"OPEN", "ADD"}).Update("context_id", ctx.ID).Error
		}
		return nil
	})
}

func (s *CopyTradeStore) UpdateOrder(id string, updates map[string]interface{}) error {
	return s.db.Model(&CopyTradeOrder{}).Where("id = ?", id).Updates(updates).Error
}

func (s *CopyTradeStore) LatestSignal(traderID, messageID string, revision int) (*CopyTradeSignal, error) {
	var row CopyTradeSignal
	err := s.db.Where("trader_id = ? AND message_id = ? AND message_revision = ?", traderID, messageID, revision).Order("created_at DESC").First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	return &row, err
}

func (s *CopyTradeStore) DueRetries(traderID string, now time.Time) ([]*CopyTradeSignal, error) {
	var rows []*CopyTradeSignal
	err := s.db.Where("trader_id = ? AND status = ? AND next_retry_at <= ? AND execution_version > 0", traderID, "retry_wait", now).Order("next_retry_at ASC").Find(&rows).Error
	return rows, err
}

// AccountContexts checks other owners of the same exchange configuration.
// Exchange position truth is additionally checked before a managed open.
func (s *CopyTradeStore) AccountContexts(traderID, symbol, direction string) ([]*CopyTradeContext, error) {
	var rows []*CopyTradeContext
	err := s.db.Table("copytrade_trade_contexts AS c").Select("c.*").
		Joins("JOIN traders AS t ON t.id = c.trader_id").
		Where("t.exchange_id = (SELECT exchange_id FROM traders WHERE id = ?) AND c.symbol = ? AND c.direction = ? AND c.state IN ?", traderID, symbol, direction, activeStates).
		Find(&rows).Error
	return rows, err
}

func (s *CopyTradeStore) CreateOrder(order *CopyTradeOrder) error { return s.db.Create(order).Error }

// Old successful side effects predate the action journal. Do not replay them
// simply because a legacy message receives a cosmetic edit after upgrading.
func (s *CopyTradeStore) LegacyExecutedAction(traderID, messageID, symbol, action string) (bool, error) {
	var rows []*CopyTradeSignal
	err := s.db.Where("trader_id = ? AND message_id = ? AND execution_version = 0 AND status = ?", traderID, messageID, SignalStatusExecuted).Find(&rows).Error
	for _, r := range rows {
		if r.Action == action && (r.Symbol == symbol || r.Symbol+"USDT" == symbol) {
			return true, err
		}
		var parsed struct {
			Instructions []struct{ Action, Symbol string } `json:"instructions"`
		}
		if json.Unmarshal([]byte(r.InterpretationJSON), &parsed) == nil {
			for _, child := range parsed.Instructions {
				s := strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(child.Symbol), "/", ""))
				if child.Action == action && (s == symbol || s+"USDT" == symbol) {
					return true, err
				}
			}
		}
	}
	return false, err
}

func (s *CopyTradeStore) CreateProtectionOrder(ctx *CopyTradeContext, order *CopyTradeOrder, tpJSON string) error {
	return s.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(order).Error; err != nil {
			return err
		}
		result := tx.Model(&CopyTradeContext{}).Where("id = ? AND version = ?", ctx.ID, ctx.Version).Updates(map[string]interface{}{"tp_plan_json": tpJSON, "version": ctx.Version + 1})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return errors.New("protection context version conflict")
		}
		return nil
	})
}

func (s *CopyTradeStore) CreateExitOrder(ctx *CopyTradeContext, order *CopyTradeOrder, intent string) error {
	return s.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(order).Error; err != nil {
			return err
		}
		result := tx.Model(&CopyTradeContext{}).Where("id = ? AND version = ?", ctx.ID, ctx.Version).Updates(map[string]interface{}{"state": "CLOSE_PENDING", "close_pending_json": intent, "version": ctx.Version + 1})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return errors.New("exit context version conflict")
		}
		return nil
	})
}
func (s *CopyTradeStore) BindOpenAction(signalID, symbol, direction, contextID string) error {
	return s.db.Model(&CopyTradeAction{}).Where("signal_id = ? AND symbol = ? AND direction = ? AND action IN ?", signalID, symbol, direction, []string{"OPEN", "ADD"}).Update("context_id", contextID).Error
}
func (s *CopyTradeStore) CompleteActionSignal(signalID, contextID string) error {
	return s.db.Model(&CopyTradeAction{}).Where("signal_id = ? AND context_id = ? AND action IN ?", signalID, contextID, []string{"REDUCE", "CLOSE"}).Updates(map[string]interface{}{"status": "done", "error": ""}).Error
}

// FinalizeExit commits the confirmed position lifecycle and action outcome
// together, including a crash immediately after a full close is confirmed.
func (s *CopyTradeStore) FinalizeExit(ctx *CopyTradeContext, signalID string, updates map[string]interface{}) error {
	return s.db.Transaction(func(tx *gorm.DB) error {
		child := &CopyTradeStore{db: tx}
		if err := child.UpdateContextVersioned(ctx.ID, ctx.Version, updates); err != nil {
			return err
		}
		return child.CompleteActionSignal(signalID, ctx.ID)
	})
}

type CopyTradeActionView struct {
	*CopyTradeAction
	TradeState string `json:"trade_state,omitempty"`
}

func (s *CopyTradeStore) attachExecutionViews(traderID string, views []*CopyTradeSignalView) error {
	ids := make([]string, 0, len(views))
	bySignal := map[string]*CopyTradeSignalView{}
	for _, v := range views {
		if v.TraderID == traderID {
			ids = append(ids, v.ID)
			bySignal[v.ID] = v
			_ = json.Unmarshal([]byte(v.InstructionResultsJSON), &v.InstructionResults)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	var actions []*CopyTradeAction
	if err := s.db.Where("trader_id = ? AND signal_id IN ?", traderID, ids).Order("created_at ASC").Find(&actions).Error; err != nil {
		return err
	}
	var orders []*CopyTradeOrder
	if err := s.db.Where("trader_id = ? AND signal_id IN ?", traderID, ids).Order("created_at ASC, role ASC").Find(&orders).Error; err != nil {
		return err
	}
	contextIDs := []string{}
	for _, a := range actions {
		if a.ContextID != "" {
			contextIDs = append(contextIDs, a.ContextID)
		}
	}
	states := map[string]string{}
	if len(contextIDs) > 0 {
		var contexts []*CopyTradeContext
		if err := s.db.Select("id", "state").Where("trader_id = ? AND id IN ?", traderID, contextIDs).Find(&contexts).Error; err != nil {
			return err
		}
		for _, c := range contexts {
			states[c.ID] = c.State
		}
	}
	for _, a := range actions {
		if v := bySignal[a.SignalID]; v != nil {
			v.ActionResults = append(v.ActionResults, &CopyTradeActionView{CopyTradeAction: a, TradeState: states[a.ContextID]})
		}
	}
	for _, o := range orders {
		if v := bySignal[o.SignalID]; v != nil {
			v.OrderLegs = append(v.OrderLegs, o)
		}
	}
	return nil
}

func (s *CopyTradeStore) PendingActions(traderID, contextID string) ([]*CopyTradeAction, error) {
	var rows []*CopyTradeAction
	err := s.db.Where("trader_id = ? AND context_id = ? AND status IN ?", traderID, contextID, []string{"executing", "uncertain"}).Order("created_at ASC").Find(&rows).Error
	return rows, err
}
