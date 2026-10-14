package store

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// Business audit, deliberately excluded from diagnostic-log retention.
type CopyTradeRecoveryReview struct {
	ID                  string     `gorm:"primaryKey" json:"id"`
	ContextID           string     `gorm:"index;not null" json:"context_id"`
	TraderID            string     `gorm:"index;not null" json:"trader_id"`
	ActorID             string     `json:"-"`
	ExchangeID          string     `json:"-"`
	Fingerprint         string     `json:"-"`
	OriginalContextJSON string     `gorm:"type:text" json:"-"`
	EvidenceJSON        string     `gorm:"type:text" json:"evidence_json"`
	Reason              string     `json:"reason,omitempty"`
	ExpiresAt           time.Time  `json:"expires_at"`
	ConfirmedAt         *time.Time `json:"confirmed_at,omitempty"`
	CreatedAt           time.Time  `json:"created_at"`
}

func recoveryFingerprint(c CopyTradeContext) string {
	// Reconcile heartbeats and repeated errors do not alter the semantic identity.
	c.Version = 0
	c.UpdatedAt = time.Time{}
	c.LastError = ""
	b, _ := json.Marshal(c)
	return fmt.Sprintf("%x", sha256.Sum256(b))
}

// RecoveryReview fences the account during both remote verification and local
// commit. verify is read-only; no automatic trading cleanup is called here.
func (s *CopyTradeStore) RecoveryReview(traderID, contextID, actor, ticket, reason string, verify func(*CopyTradeContext) (string, error)) (*CopyTradeRecoveryReview, error) {
	var out CopyTradeRecoveryReview
	err := s.db.Transaction(func(tx *gorm.DB) error {
		var initial CopyTradeContext
		if err := tx.First(&initial, "id = ? AND trader_id = ?", contextID, traderID).Error; err != nil {
			return err
		}
		if initial.ExchangeID == "" {
			return fmt.Errorf("missing persisted account binding")
		}
		if err := lockCopyTradeAccount(tx, initial.ExchangeID); err != nil {
			return err
		}
		var c CopyTradeContext
		if err := tx.First(&c, "id = ? AND trader_id = ?", contextID, traderID).Error; err != nil {
			return err
		}
		binding, err := NewCopyTradeStore(tx).TraderExchangeID(traderID)
		if err != nil {
			return err
		}
		if binding != c.ExchangeID {
			return fmt.Errorf("account binding changed")
		}
		now := time.Now().UTC()
		if ticket != "" {
			if err := tx.First(&out, "id = ? AND context_id = ? AND trader_id = ? AND actor_id = ?", ticket, contextID, traderID, actor).Error; err != nil {
				return err
			}
			if out.ConfirmedAt != nil {
				return nil
			}
			if !now.Before(out.ExpiresAt) {
				return fmt.Errorf("preview expired; verify again")
			}
			if out.Fingerprint != recoveryFingerprint(c) || out.ExchangeID != binding {
				return fmt.Errorf("recovery state changed; verify again")
			}
			if strings.TrimSpace(reason) == "" || len(reason) > 1000 {
				return fmt.Errorf("confirmation reason required (maximum 1000 bytes)")
			}
		}
		if c.State == "CLOSED" || c.StopIntentJSON == "" || c.ClosePendingJSON != "" || c.TPUpdateIntentJSON != "" || c.EntryWorking {
			return fmt.Errorf("context is not eligible for historical stop review")
		}
		var n int64
		accountTraders := tx.Table("traders").Select("id").Where("exchange_id = ?", binding)
		if err := tx.Model(&CopyTradeAction{}).Where("trader_id IN (?) AND status IN ?", accountTraders, []string{"executing", "uncertain"}).Count(&n).Error; err != nil {
			return err
		}
		if n > 0 {
			return fmt.Errorf("execution actions still pending")
		}
		if err := tx.Model(&CopyTradeOrder{}).Where("trader_id IN (?) AND status IN ?", accountTraders, []string{"PLANNED", "SUBMITTING", "UNKNOWN"}).Count(&n).Error; err != nil {
			return err
		}
		if n > 0 {
			return fmt.Errorf("order submissions still pending")
		}
		evidence, err := verify(&c)
		if err != nil {
			return err
		}
		if ticket == "" {
			original, _ := json.Marshal(c)
			out = CopyTradeRecoveryReview{ID: uuid.NewString(), ContextID: c.ID, TraderID: traderID, ActorID: actor, ExchangeID: binding, Fingerprint: recoveryFingerprint(c), OriginalContextJSON: string(original), EvidenceJSON: evidence, ExpiresAt: time.Now().UTC().Add(60 * time.Second), CreatedAt: now}
			return tx.Create(&out).Error
		}
		// A slow remote lookup cannot extend an expired authorization.
		if !time.Now().Before(out.ExpiresAt) {
			return fmt.Errorf("preview expired during verification")
		}
		out.ConfirmedAt = &now
		out.Reason = strings.TrimSpace(reason)
		out.EvidenceJSON = evidence
		if err := tx.Save(&out).Error; err != nil {
			return err
		}
		result := tx.Model(&CopyTradeContext{}).Where("id = ? AND version = ?", c.ID, c.Version).Updates(map[string]interface{}{"state": "CLOSED", "quantity": 0, "closed_at": now, "stop_intent_json": "", "last_error": "", "last_action": "MANUAL_RECOVERY_REVIEW", "version": c.Version + 1})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return fmt.Errorf("context changed during verification")
		}
		if err := tx.Where("context_id = ? AND trader_id = ? AND exchange_id = ?", c.ID, traderID, binding).Delete(&CopyTradeOwnership{}).Error; err != nil {
			return err
		}
		// Keep the closure visible in the existing trace. The original unknown
		// request and actor remain in the permanent business audit above.
		context, _ := json.Marshal(map[string]interface{}{"context_id": c.ID, "review_id": out.ID, "reason": RedactLogText(out.Reason), "evidence": evidence, "submission_outcome": "unknown", "local_recovery": "manually_closed"})
		return NewCopyTradeStore(tx).AppendEvent(&CopyTradeEvent{TraderID: traderID, TraceID: "reconcile-" + c.ID, Level: "info", Event: "copytrade.recovery.manual_review_completed", Message: "人工核对完成，已结束本地恢复；原交易所提交结果仍为未知", ContextJSON: SanitizeLogJSON(string(context)), OccurredAt: now})
	})
	return &out, err
}
