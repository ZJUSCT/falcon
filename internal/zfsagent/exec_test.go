package zfsagent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeScript creates an executable shell script (no zfs involved: the
// candidate-probing logic only needs real exec-able files).
func writeScript(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
}

// TestHostRunnerAdvancesCandidates: a candidate whose exec cannot start
// (binary missing) is skipped in order; the first working one is memoized.
func TestHostRunnerAdvancesCandidates(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "opt", "zfs", "bin", "zfs")
	// Count real executions so the memoization below is observable.
	writeScript(t, good, "echo executed >> "+filepath.Join(dir, "count")+"\necho tank\n")

	runner := NewHostRunner("", []string{"/definitely-missing/zfs", good})
	for i := 0; i < 2; i++ {
		out, err := runner.Run(context.Background(), "zfs", "list", "-H")
		if err != nil {
			t.Fatalf("Run %d: %v", i, err)
		}
		if string(out) != "tank\n" {
			t.Errorf("Run %d output = %q", i, out)
		}
	}
	if chosen := runner.chosen["zfs"]; chosen != good {
		t.Errorf("chosen candidate = %q, want %q", chosen, good)
	}
	count, err := os.ReadFile(filepath.Join(dir, "count"))
	if err != nil {
		t.Fatal(err)
	}
	if lines := strings.Count(string(count), "executed"); lines != 2 {
		t.Errorf("script executed %d times, want exactly 2 (one per Run)", lines)
	}
}

// TestHostRunnerDoesNotAdvanceOnRealError: a candidate that starts but exits
// non-zero is a real zfs error — the runner must return it (with stderr) and
// must not silently try the next candidate.
func TestHostRunnerDoesNotAdvanceOnRealError(t *testing.T) {
	dir := t.TempDir()
	broken := filepath.Join(dir, "a", "zfs")
	writeScript(t, broken, "echo cannot open 'tank' >&2\nexit 1\n")
	healthy := filepath.Join(dir, "b", "zfs")
	writeScript(t, healthy, "echo tank\n")

	runner := NewHostRunner("", []string{broken, healthy})
	_, err := runner.Run(context.Background(), "zfs", "get", "used", "tank")
	if err == nil {
		t.Fatal("expected the failing candidate's error")
	}
	// The shell strips the quotes, so the stderr line reads: cannot open tank.
	if !strings.Contains(err.Error(), "cannot open tank") {
		t.Errorf("error must carry the zfs stderr, got: %v", err)
	}
	if chosen := runner.chosen["zfs"]; chosen != "" {
		t.Errorf("a real zfs error must not memoize a candidate, got %q", chosen)
	}
}

// TestHostRunnerZpoolNextToZfs: zpool candidates derive from the zfs
// candidates' directory (a custom --zfs-bin implies a custom zpool there).
func TestHostRunnerZpoolNextToZfs(t *testing.T) {
	dir := t.TempDir()
	writeScript(t, filepath.Join(dir, "zfs"), "echo zfs-out\n")
	writeScript(t, filepath.Join(dir, "zpool"), "echo tank\n")

	runner := NewHostRunner("", []string{filepath.Join(dir, "zfs")})
	out, err := runner.Run(context.Background(), "zpool", "list", "-H", "-o", "name")
	if err != nil {
		t.Fatalf("Run zpool: %v", err)
	}
	if string(out) != "tank\n" {
		t.Errorf("zpool output = %q", out)
	}
}

func TestHostRunnerNoWorkingCandidate(t *testing.T) {
	runner := NewHostRunner("", []string{"/definitely-missing/zfs", "/also-missing/zfs"})
	_, err := runner.Run(context.Background(), "zfs", "list")
	if err == nil {
		t.Fatal("expected an error when every candidate fails to start")
	}
	if !strings.Contains(err.Error(), "no working zfs binary") {
		t.Errorf("error = %v, want the candidate summary", err)
	}
}

func TestHostRunnerDefaultCandidates(t *testing.T) {
	runner := NewHostRunner("", nil)
	got := runner.candidatesFor("zfs")
	want := strings.Join(DefaultZfsBinCandidates, ",")
	if strings.Join(got, ",") != want {
		t.Errorf("zfs candidates = %v, want %v", got, DefaultZfsBinCandidates)
	}
	zpool := runner.candidatesFor("zpool")
	if len(zpool) != len(DefaultZfsBinCandidates) {
		t.Fatalf("zpool candidates = %v", zpool)
	}
	for i, want := range []string{"/sbin/zpool", "/usr/sbin/zpool"} {
		if zpool[i] != want {
			t.Errorf("zpool candidate %d = %q, want %q", i, zpool[i], want)
		}
	}
}
