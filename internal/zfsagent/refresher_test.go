package zfsagent

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestRefresherSnapshotLifecycle: Snapshot errors before the first successful
// collection, succeeds after one, and a failing refresh keeps the previous
// report (identical pointer — the cache is not disturbed by failures).
func TestRefresherSnapshotLifecycle(t *testing.T) {
	var fail atomic.Bool
	// Pool enumeration fails on demand: that is the only way Report itself
	// errors (per-pool zfs failures merely degrade a report, which would
	// legitimately replace the cache).
	runner := &fakeRunner{respond: func(command string) ([]byte, error) {
		if fail.Load() {
			return nil, errors.New("no working zfs binary")
		}
		if command == "zpool list -H -o name" {
			return []byte("tank\n"), nil
		}
		return []byte("tank\tused\t1\n"), nil
	}}
	r := NewRefresher(NewCollector("storage-1", nil, runner))

	if _, err := r.Snapshot(); err == nil {
		t.Fatal("Snapshot must error before the first successful collection")
	}

	r.refresh(context.Background())
	snap, err := r.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot after refresh: %v", err)
	}
	if snap.Node != "storage-1" || len(snap.Pools) != 1 || snap.Pools[0].Name != "tank" {
		t.Errorf("snapshot = %+v", snap)
	}

	fail.Store(true)
	r.refresh(context.Background())
	again, err := r.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot after failed refresh: %v", err)
	}
	if again != snap {
		t.Error("a failed refresh must keep serving the previous report")
	}
}

// TestRefresherRunCollectsImmediately: Run does its first collection right
// away (not after the first tick). The counter tracks the one zfs get per
// Report, not every command the collector issues.
func TestRefresherRunCollectsImmediately(t *testing.T) {
	var collections atomic.Int64
	runner := &fakeRunner{respond: func(command string) ([]byte, error) {
		if strings.HasPrefix(command, "zfs get ") {
			collections.Add(1)
		}
		return []byte("tank\tused\t1\n"), nil
	}}
	r := NewRefresher(NewCollector("storage-1", []string{"tank"}, runner))
	r.interval = time.Hour // no second collection within this test

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		r.Run(ctx)
		close(done)
	}()
	defer func() {
		cancel()
		<-done
	}()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := r.Snapshot(); err == nil {
			if got := collections.Load(); got != 1 {
				t.Errorf("collections = %d, want 1", got)
			}
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("Run did not make the first report available promptly")
}
