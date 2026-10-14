package store

import (
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"testing"
	"time"
)

func TestDurableAttemptsAndEndToEndTiming(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err = db.AutoMigrate(&CopyTradeAIRun{}, &CopyTradeSignal{}); err != nil {
		t.Fatal(err)
	}
	s := NewCopyTradeStore(db)
	sig := &CopyTradeSignal{ID: "signal", TraderID: "t", ReceivedAt: time.Now().Add(-529 * time.Second), Status: "interpreting", ExecutionVersion: 1}
	if err = s.CreateSignal(sig); err != nil {
		t.Fatal(err)
	}
	run := &CopyTradeAIRun{TraderID: "t", SignalID: sig.ID, Status: "running", StartedAt: time.Now()}
	if err = s.CreateAIRun(run); err != nil {
		t.Fatal(err)
	}
	stats, _ := s.RecognitionStats("t")
	if stats.ModelCalls != 0 {
		t.Fatal("running attempt counted")
	}
	if err = s.InterruptAIRuns("t"); err != nil {
		t.Fatal(err)
	}
	run.Status = "completed"
	if err = s.FinishAIRun(run); err == nil {
		t.Fatal("late callback overwrote interrupted attempt")
	}
	saved, _ := s.GetSignal(sig.ID)
	if saved.Status != "retry_wait" || saved.NextRetryAt == nil {
		t.Fatal("interrupted signal not recoverable")
	}
	if err = s.UpdateSignal(sig.ID, map[string]interface{}{"status": "executed", "total_ms": 131000}); err != nil {
		t.Fatal(err)
	}
	saved, _ = s.GetSignal(sig.ID)
	if saved.EndToEndMs == nil || *saved.EndToEndMs < 529000 || *saved.EndToEndMs > 531000 {
		t.Fatalf("lost total latency %+v", saved)
	}
}

func TestRetryWaitIsRecordedOnlyOnce(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err = db.AutoMigrate(&CopyTradeSignal{}); err != nil {
		t.Fatal(err)
	}
	s := NewCopyTradeStore(db)
	started := time.Now().Add(-30 * time.Second)
	sig := &CopyTradeSignal{ID: "retry", TraderID: "t", Status: "retry_wait", RetryWaitStartedAt: &started}
	if err = s.CreateSignal(sig); err != nil {
		t.Fatal(err)
	}
	if err = s.FinishRetryWait(sig); err != nil {
		t.Fatal(err)
	}
	if err = s.FinishRetryWait(sig); err != nil {
		t.Fatal(err)
	}
	saved, _ := s.GetSignal(sig.ID)
	if saved.RetryWaitMs == nil || *saved.RetryWaitMs < 30000 || *saved.RetryWaitMs > 31000 || saved.RetryWaitStartedAt != nil {
		t.Fatalf("retry wait %+v", saved)
	}
}
