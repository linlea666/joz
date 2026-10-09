package discord

import (
	"sync"

	"nofx/store"
)

var (
	globalMu     sync.Mutex
	globalSource *SourceManager
)

// InitGlobal creates (once) and returns the process-wide source manager singleton.
// Call from main before traders load; subsequent calls return the same instance.
func InitGlobal(st *store.Store) *SourceManager {
	globalMu.Lock()
	defer globalMu.Unlock()
	if globalSource == nil {
		globalSource = NewSourceManager(st)
	}
	return globalSource
}

// Global returns the source manager singleton (nil before InitGlobal).
func Global() *SourceManager {
	globalMu.Lock()
	defer globalMu.Unlock()
	return globalSource
}
