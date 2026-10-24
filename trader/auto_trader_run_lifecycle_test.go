package trader

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"nofx/copytrader"
	"nofx/discord"
	"nofx/trader/binance"
)

// The Binance adapter must keep satisfying the copy-trading position-mode gate.
var _ dualSidePositionChecker = (*binance.FuturesTrader)(nil)

const validCopyTradingConfig = `{"primary_channel_id":"123456789012345678"}`

func newLifecycleTestTrader(cfg AutoTraderConfig, tr Trader) *AutoTrader {
	return &AutoTrader{id: "lifecycle", name: "lifecycle", config: cfg, trader: tr, stopMonitorCh: make(chan struct{})}
}

// positionModeStub is a Trader whose only implemented method is the position
// mode probe; any other call would panic on the nil embedded interface, which
// also proves that startup is refused before the exchange is touched.
type positionModeStub struct {
	Trader
	dual  bool
	err   error
	calls atomic.Int32
}

func (s *positionModeStub) IsDualSidePosition(context.Context) (bool, error) {
	s.calls.Add(1)
	return s.dual, s.err
}

func TestBeginRunAllowsExactlyOneConcurrentClaim(t *testing.T) {
	at := newLifecycleTestTrader(AutoTraderConfig{}, nil)
	const n = 32
	var wg sync.WaitGroup
	var wins, dup atomic.Int32
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, _, err := at.beginRun()
			switch {
			case err == nil:
				wins.Add(1)
			case errors.Is(err, ErrAlreadyRunning):
				dup.Add(1)
			default:
				t.Errorf("unexpected error %v", err)
			}
		}()
	}
	close(start)
	wg.Wait()
	if wins.Load() != 1 || dup.Load() != n-1 {
		t.Fatalf("wins=%d dup=%d", wins.Load(), dup.Load())
	}
	if !at.IsRunning() {
		t.Fatal("claimed trader must report running")
	}
	if err := at.Run(); !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("Run on a running trader returned %v, want ErrAlreadyRunning", err)
	}
	if err := at.StartAsync(nil); !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("StartAsync on a running trader returned %v, want ErrAlreadyRunning", err)
	}
}

func TestRunConcurrentOnlyOneActive(t *testing.T) {
	// Copy-trading with an invalid config would exit immediately, so hold one
	// run open via the claim API and race real Run calls against it.
	at := newLifecycleTestTrader(AutoTraderConfig{TraderType: string(copytrader.TraderTypeCopy), CopyTradingConfig: "{not json"}, nil)
	stopCh, done, err := at.beginRun()
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := at.Run(); !errors.Is(err, ErrAlreadyRunning) {
				t.Errorf("concurrent Run returned %v", err)
			}
		}()
	}
	wg.Wait()
	// Release the held run as Run would.
	at.finishRun(stopCh, done)
	if at.IsRunning() {
		t.Fatal("released run must report stopped")
	}
}

func TestCopyTradingStartFailureClearsRunning(t *testing.T) {
	if discord.Global() != nil {
		t.Skip("global Discord source initialized in this process")
	}
	for name, cfg := range map[string]AutoTraderConfig{
		"invalid config":     {TraderType: string(copytrader.TraderTypeCopy), CopyTradingConfig: "{not json"},
		"discord not inited": {TraderType: string(copytrader.TraderTypeCopy), CopyTradingConfig: validCopyTradingConfig, Exchange: "binance"},
	} {
		t.Run(name, func(t *testing.T) {
			at := newLifecycleTestTrader(cfg, nil)
			if err := at.Run(); err == nil {
				t.Fatal("expected start failure")
			}
			if at.IsRunning() {
				t.Fatal("isRunning must be false after a failed start")
			}
			if running, _ := at.GetStatus()["is_running"].(bool); running {
				t.Fatal("status must report stopped after a failed start")
			}
			// Stop after a failed start is a no-op and must not block or panic.
			stopped := make(chan struct{})
			go func() { at.Stop(); close(stopped) }()
			select {
			case <-stopped:
			case <-time.After(2 * time.Second):
				t.Fatal("Stop blocked after failed start")
			}
			// A retry is accepted (and fails the same way) instead of "already running".
			if err := at.Run(); err == nil || errors.Is(err, ErrAlreadyRunning) {
				t.Fatalf("retry returned %v", err)
			}
		})
	}
}

func TestStartAsyncReportsFailureToOnExit(t *testing.T) {
	at := newLifecycleTestTrader(AutoTraderConfig{TraderType: string(copytrader.TraderTypeCopy), CopyTradingConfig: "{not json"}, nil)
	got := make(chan error, 1)
	if err := at.StartAsync(func(err error) { got <- err }); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-got:
		if err == nil {
			t.Fatal("expected run error")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("onExit not called")
	}
	if at.IsRunning() {
		t.Fatal("trader must be stopped once onExit runs")
	}
}

func TestStopThenRunAgain(t *testing.T) {
	at := newLifecycleTestTrader(AutoTraderConfig{}, nil)
	for round := 0; round < 3; round++ {
		stopCh, done, err := at.beginRun()
		if err != nil {
			t.Fatalf("round %d: %v", round, err)
		}
		// Simulate the run body: block until stopped, like the copy-trading loop.
		at.monitorWg.Add(1)
		go func() {
			defer at.finishRun(stopCh, done)
			defer at.monitorWg.Done()
			<-stopCh
		}()
		stopped := make(chan struct{})
		go func() { at.Stop(); close(stopped) }()
		select {
		case <-stopped:
		case <-time.After(2 * time.Second):
			t.Fatalf("round %d: Stop blocked", round)
		}
		if at.IsRunning() {
			t.Fatalf("round %d: still running after Stop", round)
		}
	}
	// After Stop returns the slot is free immediately.
	if _, _, err := at.beginRun(); err != nil {
		t.Fatalf("claim after Stop: %v", err)
	}
}

func TestCopyTradingPositionModeGate(t *testing.T) {
	if discord.Global() != nil {
		t.Skip("global Discord source initialized in this process")
	}
	cfg := AutoTraderConfig{TraderType: string(copytrader.TraderTypeCopy), CopyTradingConfig: validCopyTradingConfig, Exchange: "binance"}

	t.Run("hedge passes the gate", func(t *testing.T) {
		stub := &positionModeStub{dual: true}
		at := newLifecycleTestTrader(cfg, stub)
		err := at.Run()
		// The gate passes; startup then stops at the (absent) Discord source.
		if err == nil || !strings.Contains(err.Error(), "Discord source not initialized") {
			t.Fatalf("want Discord error after passing gate, got %v", err)
		}
		if stub.calls.Load() != 1 {
			t.Fatalf("position mode queried %d times", stub.calls.Load())
		}
		if at.IsRunning() {
			t.Fatal("must be stopped after failed start")
		}
	})

	t.Run("one-way is refused", func(t *testing.T) {
		at := newLifecycleTestTrader(cfg, &positionModeStub{dual: false})
		err := at.Run()
		if !errors.Is(err, ErrCopyTradingOneWayPositionMode) {
			t.Fatalf("want one-way refusal, got %v", err)
		}
		if !strings.Contains(err.Error(), "单向持仓") || !strings.Contains(err.Error(), "one-way") {
			t.Fatalf("error must be bilingual: %v", err)
		}
		if at.IsRunning() {
			t.Fatal("must be stopped after refusal")
		}
	})

	t.Run("query failure is refused", func(t *testing.T) {
		at := newLifecycleTestTrader(cfg, &positionModeStub{err: errors.New("HTTP 500")})
		err := at.Run()
		if err == nil || !strings.Contains(err.Error(), "HTTP 500") || !strings.Contains(err.Error(), "refusing to start") {
			t.Fatalf("want fail-closed refusal, got %v", err)
		}
		if at.IsRunning() {
			t.Fatal("must be stopped after refusal")
		}
	})

	t.Run("exchanges without the probe are unaffected", func(t *testing.T) {
		at := newLifecycleTestTrader(cfg, nil)
		if err := at.verifyCopyTradingPositionMode(); err != nil {
			t.Fatalf("non-probing exchange must pass: %v", err)
		}
	})
}
