// Package clock provides a test-overridable millisecond clock.
package clock

import (
	"sync"
	"time"
)

var (
	mu  sync.RWMutex
	now = func() int64 { return time.Now().UnixMilli() }
)

// NowMillis returns the current time in Unix milliseconds.
func NowMillis() int64 {
	mu.RLock()
	defer mu.RUnlock()
	return now()
}

// SetForTesting overrides the clock. Call ResetForTesting to restore.
func SetForTesting(fn func() int64) {
	mu.Lock()
	defer mu.Unlock()
	now = fn
}

// ResetForTesting restores the real clock.
func ResetForTesting() {
	mu.Lock()
	defer mu.Unlock()
	now = func() int64 { return time.Now().UnixMilli() }
}
