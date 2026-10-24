package manager

import (
	"sync/atomic"
	"testing"
	"time"

	"nofx/store"
	"nofx/trader"
)

func TestLockTraderLifecycleSerializesSameID(t *testing.T) {
	tm := NewTraderManager()
	unlock := tm.LockTraderLifecycle("a")

	var acquired atomic.Bool
	done := make(chan struct{})
	go func() {
		release := tm.LockTraderLifecycle("a")
		acquired.Store(true)
		release()
		close(done)
	}()

	// A different trader ID is independent.
	otherDone := make(chan struct{})
	go func() {
		release := tm.LockTraderLifecycle("b")
		release()
		close(otherDone)
	}()
	select {
	case <-otherDone:
	case <-time.After(2 * time.Second):
		t.Fatal("lock for another trader ID blocked")
	}

	time.Sleep(50 * time.Millisecond)
	if acquired.Load() {
		t.Fatal("second lifecycle lock for the same ID acquired while held")
	}
	unlock()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("second lifecycle lock never acquired after release")
	}
}

func TestLockTraderLifecycleZeroValueManager(t *testing.T) {
	tm := &TraderManager{}
	tm.LockTraderLifecycle("x")()
}

func TestSyncStoppedStatusAfterRunFailure(t *testing.T) {
	st, err := store.New(t.TempDir() + "/lifecycle.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.Trader().Create(&store.Trader{ID: "t1", UserID: "u1", Name: "n", AIModelID: "m", ExchangeID: "e", IsRunning: true}); err != nil {
		t.Fatal(err)
	}
	isRunning := func() bool {
		row, err := st.Trader().GetByID("t1")
		if err != nil {
			t.Fatal(err)
		}
		return row.IsRunning
	}

	tm := NewTraderManager()
	failed := &trader.AutoTrader{}
	insertTrader(tm, "t1", failed)
	tm.SyncStoppedStatusAfterRunFailure(st, "u1", "t1", failed)
	if isRunning() {
		t.Fatal("failed run must reset persisted is_running")
	}

	// A replaced (stale) instance failing still resets the flag when the
	// current instance is not running.
	if err := st.Trader().UpdateStatus("u1", "t1", true); err != nil {
		t.Fatal(err)
	}
	insertTrader(tm, "t1", &trader.AutoTrader{})
	tm.SyncStoppedStatusAfterRunFailure(st, "u1", "t1", failed)
	if isRunning() {
		t.Fatal("stale failure with idle current instance must reset is_running")
	}

	// nil store is a no-op.
	tm.SyncStoppedStatusAfterRunFailure(nil, "u1", "t1", failed)
}
