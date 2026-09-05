package controller

import "testing"

// TestSyncLimiter covers the limiter's four semantics in one place: capping
// new jobs, idempotency per job name, the unlimited (cap <= 0) mode, and the
// bypass for jobs that already existed at reconcile time.
func TestSyncLimiter(t *testing.T) {
	// Capping: acquires beyond the cap are rejected and register nothing;
	// a released slot becomes available again.
	l := NewSyncLimiter(2)
	if !l.Acquire("job-a", false) || !l.Acquire("job-b", false) {
		t.Fatalf("first two acquires must succeed, held=%d", l.Held())
	}
	if l.Acquire("job-c", false) {
		t.Fatal("third acquire must be rejected at cap 2")
	}
	if l.Held() != 2 {
		t.Fatalf("held = %d, want 2 (rejected acquire must not register)", l.Held())
	}
	l.Release("job-a")
	if !l.Acquire("job-c", false) {
		t.Fatal("acquire after release must succeed")
	}

	// Idempotency: reconciles repeatedly see the same Job, so re-acquiring
	// its name must not consume a second slot, and terminal-state reconciles
	// may release several times without opening a slot for someone else.
	l = NewSyncLimiter(2)
	if !l.Acquire("job-a", false) {
		t.Fatal("acquire job-a failed")
	}
	if l.Acquire("job-a", false) {
		t.Fatal("duplicate acquire must be rejected")
	}
	if l.Held() != 1 {
		t.Fatalf("held = %d, want 1", l.Held())
	}
	l.Release("job-a")
	l.Release("job-a")
	if l.Held() != 0 {
		t.Fatalf("held = %d, want 0 (double release must not underflow)", l.Held())
	}

	// Unlimited: maxConcurrent <= 0 accepts everything.
	l = NewSyncLimiter(0)
	for i := 0; i < 100; i++ {
		if !l.Acquire(string(rune('a'+i)), false) {
			t.Fatalf("unlimited limiter rejected acquire %d", i)
		}
	}
	if !NewSyncLimiter(-1).Acquire("job", false) {
		t.Fatal("negative cap must behave as unlimited")
	}

	// Existing bypass: job-b was created before a controller restart and is
	// already running at reconcile time, so it registers even though the cap
	// is full. Its terminal release frees exactly one slot, after which a new
	// job still cannot squeeze in while job-a keeps running.
	l = NewSyncLimiter(1)
	if !l.Acquire("job-a", false) {
		t.Fatal("acquire job-a failed")
	}
	if !l.Acquire("job-b", true) {
		t.Fatal("existing job must bypass the cap")
	}
	if l.Held() != 2 {
		t.Fatalf("held = %d, want 2", l.Held())
	}
	l.Release("job-b")
	if l.Acquire("job-c", false) {
		t.Fatal("new job must wait while job-a holds the single slot")
	}
	l.Release("job-a")
	if !l.Acquire("job-c", false) {
		t.Fatal("acquire after all releases must succeed")
	}
}
