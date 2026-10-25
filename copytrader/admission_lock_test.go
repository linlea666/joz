package copytrader

import (
	"testing"
	"time"

	"nofx/store"
)

// gatedExchange parks admission inside the lock (the fresh position read) until
// released, then reports an existing position so ExecuteOpen returns without
// trading.
type gatedExchange struct {
	*mockExchange
	entered chan string
	release chan struct{}
	name    string
}

func (g *gatedExchange) GetPositions() ([]map[string]interface{}, error) {
	g.entered <- g.name
	<-g.release
	return []map[string]interface{}{{"symbol": "BTCUSDT", "side": "long", "positionAmt": 1.0, "entryPrice": 100.0}}, nil
}

// Admission is serialized per exchange account: a different account admits in
// parallel, the same account waits for the holder to finish.
func TestManagedAdmissionLocksPerAccount(t *testing.T) {
	st := newTestStore(t) // trader-1 is bound to account-1
	for _, tr := range []*store.Trader{
		{ID: "trader-2", UserID: "test", Name: "other account", ExchangeID: "account-2"},
		{ID: "trader-3", UserID: "test", Name: "same account", ExchangeID: "account-1"},
	} {
		if err := st.Trader().Create(tr); err != nil {
			t.Fatal(err)
		}
	}
	entered := make(chan string, 3)
	newGated := func(traderID string) (*Executor, *gatedExchange) {
		g := &gatedExchange{mockExchange: &mockExchange{}, entered: entered, release: make(chan struct{}), name: traderID}
		return NewExecutor(traderID, g, st, NewEventLogger(st, traderID, "chan-1")), g
	}
	x1, g1 := newGated("trader-1")
	x2, g2 := newGated("trader-2")
	x3, g3 := newGated("trader-3")
	done := make(chan string, 3)
	open := func(x *Executor, name string) {
		_, err := x.ExecuteOpen("trace-"+name, "", &OpenPlan{Symbol: "BTCUSDT", Direction: DirectionLong, Quantity: 1})
		if err == nil {
			t.Errorf("%s: existing position must refuse admission", name)
		}
		done <- name
	}
	wait := func(want string) {
		t.Helper()
		select {
		case got := <-entered:
			if got != want {
				t.Fatalf("entered admission: got %s, want %s", got, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s did not enter admission", want)
		}
	}

	go open(x1, "trader-1")
	wait("trader-1") // account-1 lock held

	go open(x2, "trader-2")
	wait("trader-2") // account-2 is not blocked by account-1

	go open(x3, "trader-3")
	select {
	case got := <-entered:
		t.Fatalf("%s entered while account-1 admission was held", got)
	case <-time.After(200 * time.Millisecond):
	}

	close(g2.release)
	if got := <-done; got != "trader-2" {
		t.Fatalf("unexpected finisher %s", got)
	}
	close(g1.release)
	wait("trader-3") // same account proceeds only after the holder released
	close(g3.release)
	for i := 0; i < 2; i++ {
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("admission did not finish")
		}
	}
}
