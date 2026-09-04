package core

import (
	"sync"
	"time"
)

// Heartbeat is the small emit-bookkeeping a plugin keeps for its own liveness: a
// monotonic emit count and the wall-clock time of the last emit, both mutex-guarded
// so a plugin's Start goroutine and a concurrent CursorReporter read never race.
// Plugins embed it instead of re-implementing the same counter+mutex in every source.
type Heartbeat struct {
	mu    sync.Mutex
	count int
	last  time.Time
}

// Mark records one emit stamped at the current UTC time and returns the new running
// count together with that timestamp, so the caller stamps its envelope's TS with
// the exact time recorded here.
func (h *Heartbeat) Mark() (count int, at time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.count++
	h.last = time.Now().UTC()
	return h.count, h.last
}

// Count returns the number of emits recorded so far.
func (h *Heartbeat) Count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.count
}

// Last returns the time of the most recent emit, or the zero time if none yet.
func (h *Heartbeat) Last() time.Time {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.last
}
