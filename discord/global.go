package discord

import (
	"sync"

	"nofx/store"
)

var (
	globalMu     sync.Mutex
	globalPoller *PollerManager
)

// InitGlobal creates (once) and returns the process-wide poller singleton.
// Call from main before traders load; subsequent calls return the same instance.
func InitGlobal(st *store.Store) *PollerManager {
	globalMu.Lock()
	defer globalMu.Unlock()
	if globalPoller == nil {
		globalPoller = NewPollerManager(st)
	}
	return globalPoller
}

// Global returns the poller singleton (nil before InitGlobal).
func Global() *PollerManager {
	globalMu.Lock()
	defer globalMu.Unlock()
	return globalPoller
}
