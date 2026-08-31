package controller

import "sync"

// SyncLimiter caps the number of concurrently running sync Jobs across all
// Mirrors (config sync.maxConcurrent). A slot is acquired under a job's name
// before its Job object is created and released once the Job reaches a
// terminal state (Succeeded or Failed); both operations are idempotent per
// name, so repeated reconciles of the same Job neither leak nor double-free
// slots.
//
// The limiter is in-memory: after a controller restart the slots of already
// running Jobs are re-registered when those Jobs are first reconciled
// (existing=true bypasses the cap, because those Jobs are running regardless).
//
// A queued sync may therefore start later than status.nextSyncAt: the
// schedule stays per-Mirror, the global concurrency cap is enforced on top.
type SyncLimiter struct {
	max  int // <= 0: unlimited
	mu   sync.Mutex
	held map[string]struct{}
}

// NewSyncLimiter returns a limiter admitting at most max concurrent sync
// Jobs. max <= 0 means unlimited.
func NewSyncLimiter(max int) *SyncLimiter {
	return &SyncLimiter{max: max, held: make(map[string]struct{})}
}

// Acquire registers the named sync Job as holding a concurrency slot. For a
// new Job (existing=false) it returns false when the cap is already reached —
// the caller must not create the Job and requeue instead. For an existing Job
// (already running, e.g. created before a controller restart) the cap is
// bypassed: the Job is registered so its terminal transition frees a slot.
// Acquiring an already-known name is a no-op returning true.
func (l *SyncLimiter) Acquire(job string, existing bool) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, ok := l.held[job]; ok {
		return true
	}
	if !existing && l.max > 0 && len(l.held) >= l.max {
		return false
	}
	l.held[job] = struct{}{}
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
