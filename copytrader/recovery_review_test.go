package copytrader

import (
	"errors"
	"gorm.io/gorm"
	"nofx/store"
	"nofx/trader/types"
	"testing"
	"time"
)

func reviewFixture(t *testing.T) (*Executor, *historyStopVenue, *store.CopyTradeContext) {
	x, v, c := historyFixture(t)
	if err := x.st.GormDB().AutoMigrate(&store.CopyTradeRecoveryReview{}); err != nil {
		t.Fatal(err)
	}
	v.qty = 0
	v.result = nil
	v.historyErr = types.ErrManagedOrderNotFound
	if err := x.persistContext(c, map[string]interface{}{"exchange_id": "account-1", "quantity": 0}); err != nil {
		t.Fatal(err)
	}
	return x, v, c
}
func TestRecoveryReviewAuditedAndReadOnly(t *testing.T) {
	x, v, c := reviewFixture(t)
	preview, err := x.RecoveryReview(c.ID, "owner", "", "")
	if err != nil {
		t.Fatal(err)
	}
	// Reconcile heartbeat must not invalidate a preview.
	if err := x.persistContext(c, map[string]interface{}{"last_error": "repeated recovery notice"}); err != nil {
		t.Fatal(err)
	}
	done, err := x.RecoveryReview(c.ID, "owner", preview.ID, "checked exchange manually")
	if err != nil {
		t.Fatal(err)
	}
	if done.ConfirmedAt == nil {
		t.Fatal("no durable audit")
	}
	ctx, _ := x.st.CopyTrade().GetContext(c.ID)
	if ctx.State != "CLOSED" || ctx.StopIntentJSON != "" || done.OriginalContextJSON == "" {
		t.Fatalf("incomplete local closure %+v", ctx)
	}
	if v.submits != 0 || v.cancels != 0 {
		t.Fatal("review mutated exchange")
	}
	if _, err := x.RecoveryReview(c.ID, "owner", preview.ID, "repeat"); err != nil {
		t.Fatal("idempotent confirmation", err)
	}
	events, err := x.st.CopyTrade().GetEventsByTrace(x.traderID, "reconcile-"+c.ID)
	if err != nil || len(events) != 1 || events[0].Event != "copytrade.recovery.manual_review_completed" {
		t.Fatalf("manual closure must produce one linked audit event: %+v, %v", events, err)
	}
}
func TestRecoveryReviewRejectsUnsafeChanges(t *testing.T) {
	for _, scenario := range []string{"other_actor", "expired", "identity", "position", "query_failure", "history_available", "open_order", "pending_action", "reappeared", "binding"} {
		t.Run(scenario, func(t *testing.T) {
			x, v, c := reviewFixture(t)
			ticket, err := x.RecoveryReview(c.ID, "owner", "", "")
			if err != nil {
				t.Fatal(err)
			}
			actor := "owner"
			switch scenario {
			case "other_actor":
				actor = "intruder"
			case "expired":
				x.st.GormDB().Model(ticket).Update("expires_at", time.Now().Add(-time.Second))
			case "identity":
				x.persistContext(c, map[string]interface{}{"stop_intent_json": "{}"})
			case "position":
				v.qty = 1
			case "query_failure":
				v.historyErr = errors.New("offline")
			case "history_available":
				v.historyErr = nil
				v.result = &types.StopOrderResult{Status: "CANCELED"}
			case "open_order":
				v.openOrders = []types.OpenOrder{{Symbol: c.Symbol, OrderID: "external"}}
			case "pending_action":
				x.st.GormDB().Create(&store.CopyTradeAction{ID: "pending", TraderID: c.TraderID, Status: "uncertain"})
			case "reappeared":
				v.reappearAt = v.reads + 2
			case "binding":
				x.st.GormDB().Model(&store.Trader{}).Where("id = ?", c.TraderID).Update("exchange_id", "changed")
			}
			if _, err = x.RecoveryReview(c.ID, actor, ticket.ID, "confirm"); err == nil {
				t.Fatal("unsafe review accepted")
			}
			ctx, _ := x.st.CopyTrade().GetContext(c.ID)
			if ctx.State == "CLOSED" || ctx.StopIntentJSON == "" {
				t.Fatal("failed review changed business state")
			}
			if v.submits != 0 || v.cancels != 0 {
				t.Fatal("review mutated exchange")
			}
		})
	}
}

func TestRecoveryReviewTransactionFailurePreservesUnknown(t *testing.T) {
	x, _, c := reviewFixture(t)
	ticket, err := x.RecoveryReview(c.ID, "owner", "", "")
	if err != nil {
		t.Fatal(err)
	}
	db := x.st.GormDB()
	db.Callback().Update().Before("gorm:update").Register("review_disk_failure", func(tx *gorm.DB) {
		if tx.Statement.Table == "copytrade_trade_contexts" {
			tx.AddError(errors.New("disk failure"))
		}
	})
	defer db.Callback().Update().Remove("review_disk_failure")
	if _, err = x.RecoveryReview(c.ID, "owner", ticket.ID, "verified"); err == nil {
		t.Fatal("failed commit reported success")
	}
	fresh, _ := x.st.CopyTrade().GetContext(c.ID)
	if fresh.StopIntentJSON == "" || fresh.State == "CLOSED" {
		t.Fatal("failed review changed context")
	}
	var audit store.CopyTradeRecoveryReview
	if err = db.First(&audit, "id = ?", ticket.ID).Error; err != nil {
		t.Fatal(err)
	}
	if audit.ConfirmedAt != nil {
		t.Fatal("audit committed without context closure")
	}
}
func TestRecoveryReviewBlocksOtherAccountTask(t *testing.T) {
	x, _, c := reviewFixture(t)
	db := x.st.GormDB()
	if err := db.Exec("INSERT INTO traders(id,user_id,name,ai_model_id,exchange_id,initial_balance) VALUES('other','owner','other','m','account-1',0)").Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&store.CopyTradeAction{ID: "other-pending", TraderID: "other", Status: "uncertain"}).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := x.RecoveryReview(c.ID, "owner", "", ""); err == nil {
		t.Fatal("ignored account's other pending action")
	}
}
