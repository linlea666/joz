package store

import (
	"fmt"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// One row per physical account configuration, symbol and position side. The
// primary key enforces exclusivity across processes, not only engine goroutines.
type CopyTradeOwnership struct {
	ExchangeID string `gorm:"primaryKey"`
	Symbol     string `gorm:"primaryKey"`
	Direction  string `gorm:"primaryKey"`
	ContextID  string `gorm:"not null;index"`
	TraderID   string `gorm:"not null;index"`
}

// Account fences serialize credential/binding changes against admission. A
// write takes the SQLite writer lock; row locking also supports PostgreSQL.
type CopyTradeAccountFence struct {
	ExchangeID string `gorm:"primaryKey"`
	Revision   int64
}

func lockCopyTradeAccount(tx *gorm.DB, id string) error {
	f := CopyTradeAccountFence{ExchangeID: id}
	if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&f).Error; err != nil {
		return err
	}
	return tx.Model(&CopyTradeAccountFence{}).Where("exchange_id = ?", id).UpdateColumn("revision", gorm.Expr("revision + 1")).Error
}

func (s *CopyTradeStore) TraderExchangeID(traderID string) (string, error) {
	var t Trader
	if err := s.db.Select("exchange_id").First(&t, "id = ?", traderID).Error; err != nil {
		return "", err
	}
	if t.ExchangeID == "" {
		return "", fmt.Errorf("trader has no exchange binding")
	}
	return t.ExchangeID, nil
}

func claimCopyTradeOwnership(tx *gorm.DB, ctx *CopyTradeContext) error {
	// Empty binding exists only in historical records. New execution resolves it
	// before any exchange mutation; historical imports remain readable.
	if ctx.ExchangeID == "" {
		return nil
	}
	if err := lockCopyTradeAccount(tx, ctx.ExchangeID); err != nil {
		return err
	}
	var binding Trader
	if err := tx.Select("exchange_id").First(&binding, "id = ?", ctx.TraderID).Error; err != nil {
		return err
	}
	if binding.ExchangeID != ctx.ExchangeID {
		return fmt.Errorf("exchange binding changed during admission")
	}

	var active int64
	if err := tx.Table("copytrade_trade_contexts AS c").Joins("LEFT JOIN traders AS t ON t.id=c.trader_id").Where("COALESCE(NULLIF(c.exchange_id, ''), t.exchange_id) = ? AND c.symbol = ? AND c.direction = ? AND c.state IN ?", ctx.ExchangeID, ctx.Symbol, ctx.Direction, activeStates).Count(&active).Error; err != nil {
		return err
	}
	if active > 0 {
		return fmt.Errorf("account/symbol/direction already owned")
	}
	// Release only terminal contexts with no unresolved side effects or orders.
	if err := tx.Where("exchange_id = ? AND symbol = ? AND direction = ? AND context_id IN (SELECT id FROM copytrade_trade_contexts WHERE state NOT IN ?) AND context_id NOT IN (SELECT context_id FROM copytrade_actions WHERE status IN ?) AND context_id NOT IN (SELECT context_id FROM copytrade_orders WHERE status IN ?)", ctx.ExchangeID, ctx.Symbol, ctx.Direction, activeStates, []string{"executing", "uncertain"}, []string{"PLANNED", "SUBMITTING", "UNKNOWN", "NEW", "PARTIALLY_FILLED"}).Delete(&CopyTradeOwnership{}).Error; err != nil {
		return err
	}
	owner := CopyTradeOwnership{ExchangeID: ctx.ExchangeID, Symbol: ctx.Symbol, Direction: ctx.Direction, ContextID: ctx.ID, TraderID: ctx.TraderID}
	r := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&owner)
	if r.Error != nil {
		return r.Error
	}
	if r.RowsAffected != 1 {
		return fmt.Errorf("account/symbol/direction ownership conflict")
	}
	return nil
}

// AccountBusy includes orphan actions: an unresolved acknowledgement must
// freeze credentials even when no trade context has been created yet.
func (s *CopyTradeStore) AccountBusy(exchangeID string) (bool, error) {
	var n int64
	if err := s.db.Table("copytrade_trade_contexts AS c").Joins("LEFT JOIN traders AS t ON t.id=c.trader_id").Where("COALESCE(NULLIF(c.exchange_id,''),t.exchange_id) = ? AND (c.state IN ? OR c.entry_working = ? OR c.stop_intent_json <> '' OR c.tp_update_intent_json <> '')", exchangeID, activeStates, true).Count(&n).Error; err != nil {
		return false, err
	}
	if n > 0 {
		return true, nil
	}
	if err := s.db.Table("copytrade_orders AS o").Joins("LEFT JOIN copytrade_trade_contexts AS c ON c.id=o.context_id").Joins("LEFT JOIN traders AS t ON t.id=o.trader_id").Where("COALESCE(NULLIF(c.exchange_id,''),t.exchange_id) = ? AND o.status IN ?", exchangeID, []string{"PLANNED", "SUBMITTING", "UNKNOWN", "NEW", "PARTIALLY_FILLED"}).Count(&n).Error; err != nil {
		return false, err
	}
	if n > 0 {
		return true, nil
	}

	err := s.db.Table("copytrade_actions AS a").Joins("JOIN traders AS t ON t.id=a.trader_id").Where("t.exchange_id = ? AND a.status IN ?", exchangeID, []string{"executing", "uncertain"}).Count(&n).Error
	return n > 0, err
}

func (s *CopyTradeStore) TerminalPendingContexts(traderID string) ([]*CopyTradeContext, error) {
	var rows []*CopyTradeContext
	err := s.db.Where("trader_id = ? AND state NOT IN ? AND (entry_working = ? OR stop_intent_json <> '' OR tp_update_intent_json <> '' OR id IN (SELECT context_id FROM copytrade_orders WHERE trader_id = ? AND status IN ?))", traderID, activeStates, true, traderID, []string{"PLANNED", "SUBMITTING", "UNKNOWN", "NEW", "PARTIALLY_FILLED"}).Limit(100).Find(&rows).Error
	return rows, err
}
