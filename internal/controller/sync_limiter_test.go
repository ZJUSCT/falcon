package controller

import "testing"

func TestSyncLimiterCapsNewJobs(t *testing.T) {
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
}

func TestSyncLimiterIdempotentPerJob(t *testing.T) {
	l := NewSyncLimiter(2)
	if !l.Acquire("job-a", false) {
		t.Fatal("acquire job-a failed")
	}
	// Reconciles repeatedly see the same Job: re-acquiring its name must be a
	// no-op (true), and terminal-state reconciles may release several times.
	if !l.Acquire("job-a", false) {
		t.Fatal("re-acquire of the same job must succeed without consuming a second slot")
	}
	if l.Held() != 1 {
		t.Fatalf("held = %d, want 1", l.Held())
	}
	l.Release("job-a")
	l.Release("job-a") // double release must not open a slot for someone else
	if l.Held() != 0 {
		t.Fatalf("held = %d, want 0", l.Held())
	}
}

func TestSyncLimiterUnlimited(t *testing.T) {
	l := NewSyncLimiter(0)
	for i := 0; i < 100; i++ {
		if !l.Acquire(string(rune('a'+i)), false) {
			t.Fatalf("unlimited limiter rejected acquire %d", i)
		}
	}
	// maxConcurrent <= 0 means unlimited: negative values too.
	negative := NewSyncLimiter(-1)
	if !negative.Acquire("job", false) {
		t.Fatal("negative cap must behave as unlimited")
	}
}

func TestSyncLimiterExistingBypassesCap(t *testing.T) {
	l := NewSyncLimiter(1)
	if !l.Acquire("job-a", false) {
		t.Fatal("acquire job-a failed")
	}
	// job-b was created before a controller restart: at reconcile time it is
	// already running, so it is registered even though the cap is full.
	if !l.Acquire("job-b", true) {
		t.Fatal("existing job must bypass the cap")
	}
	if l.Held() != 2 {
		t.Fatalf("held = %d, want 2", l.Held())
	}
	// But its terminal release frees exactly one slot, after which a new job
	// still cannot squeeze in while job-a keeps running.
	l.Release("job-b")
	if l.Acquire("job-c", false) {
		t.Fatal("new job must wait while job-a holds the single slot")
	}
	l.Release("job-a")
	if !l.Acquire("job-c", false) {
		t.Fatal("acquire after all releases must succeed")
	}
}
