package controller

import "sync"

type syncSlot struct {
	revision uint64
	observed bool
}

// SyncLimiter caps the number of concurrently running sync Jobs across all
// Mirrors (config sync.maxConcurrent). A slot is acquired under a job's name
// before its Job object is created and released once the Job reaches a
// terminal state (Succeeded or Failed); both operations are idempotent per
// name, so repeated reconciles of the same Job neither leak nor double-free
// slots.
//
// The limiter is in-memory: after a controller restart the slots of already
// running Jobs and orphaned writers are restored from live API state before
// new admissions. Disappeared workloads release their observed occupancy.
//
// A queued sync may therefore start later than status.nextSyncAt: the
// schedule stays per-Mirror, the global concurrency cap is enforced on top.
type SyncLimiter struct {
	max      int // <= 0: unlimited
	mu       sync.Mutex
	held     map[string]syncSlot
	revision uint64
}

// NewSyncLimiter returns a limiter admitting at most max concurrent sync
// Jobs. max <= 0 means unlimited.
func NewSyncLimiter(max int) *SyncLimiter {
	return &SyncLimiter{max: max, held: make(map[string]syncSlot)}
}

// Acquire registers the named sync Job as holding a concurrency slot. For a
// new Job (existing=false) it returns false when the cap is already reached —
// the caller must not create the Job and requeue instead. For an existing Job
// (already running, e.g. created before a controller restart) the cap is
// bypassed: the Job is registered so its terminal transition frees a slot.
// Acquiring an already-known name returns true without consuming another slot;
// observing an existing Job also refreshes its occupancy revision.
func (l *SyncLimiter) Acquire(job string, existing bool) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, ok := l.held[job]; ok {
		if existing {
			l.revision++
			l.held[job] = syncSlot{revision: l.revision, observed: true}
		}
		return true
	}
	if !existing && l.max > 0 && len(l.held) >= l.max {
		return false
	}
	l.revision++
	l.held[job] = syncSlot{revision: l.revision, observed: existing}
	return true
}

// Release frees the slot held by the named sync Job. Releasing an unknown
// name is a no-op, so multiple terminal-state reconciles are safe.
func (l *SyncLimiter) Release(job string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.held, job)
}

// Held reports the number of currently held slots.
func (l *SyncLimiter) Held() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.held)
}

// observedSnapshot identifies occupancy eligible for stale-entry removal. New
// reservations are excluded: their Job creation may still be in flight.
func (l *SyncLimiter) observedSnapshot() map[string]uint64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	snapshot := make(map[string]uint64)
	for name, slot := range l.held {
		if slot.observed {
			snapshot[name] = slot.revision
		}
	}
	return snapshot
}

// reconcileObserved forgets disappeared workloads only if their occupancy has
// not been refreshed since the live API listing began. This protects concurrent
// admission and creation while reclaiming slots whose Jobs/Pods disappeared.
func (l *SyncLimiter) reconcileObserved(snapshot map[string]uint64, occupied map[string]struct{}) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for name, revision := range snapshot {
		if _, exists := occupied[name]; exists {
			continue
		}
		if slot, exists := l.held[name]; exists && slot.observed && slot.revision == revision {
			delete(l.held, name)
		}
	}
	for name := range occupied {
		l.revision++
		l.held[name] = syncSlot{revision: l.revision, observed: true}
	}
}
