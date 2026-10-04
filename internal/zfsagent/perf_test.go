package zfsagent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/go-cmp/cmp"
)

// writeKstat creates one file under dir (parent directories included).
func writeKstat(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestCollectPerf reads ARC and dataset counters without executing commands.
// Only mounted datasets have objset files; the pool list filters other pools.
func TestCollectPerf(t *testing.T) {
	dir := t.TempDir()
	writeKstat(t, filepath.Join(dir, "arcstats"), cannedKstatNamed) // hits/misses/size rows
	writeKstat(t, filepath.Join(dir, "tank", "objset-0x163"), cannedKstatNamed+
		"reads 4 3\nnread 4 4096\nwrites 4 5\nnwritten 4 8192\n")
	// A second mounted dataset, no zfs-localpv PVC behind it.
	writeKstat(t, filepath.Join(dir, "tank", "objset-0x164"),
		"0 0 0 27 1112 1 1\nname                            type data\n"+
			"dataset_name                    7    tank/foreign\n"+
			"reads                           4    0\nnread                           4    0\n"+
			"writes                          4    0\nnwritten                        4    0\n")
	// An incomplete objset must not fabricate zero-valued counters.
	writeKstat(t, filepath.Join(dir, "tank", "objset-0x165"), cannedKstatNamed)
	// Not a pool directory member the collector should read.
	writeKstat(t, filepath.Join(dir, "tank", "not-objset"), "noise\n")
	// A pool outside c.Pools: must be filtered out.
	writeKstat(t, filepath.Join(dir, "other", "objset-0x1"),
		"0 0 0 27 1112 1 1\nname                            type data\n"+
			"dataset_name                    7    other/x\n"+
			"reads                           4    1\nnread                           4    1\n"+
			"writes                          4    1\nnwritten                        4    1\n")

	runner := &fakeRunner{respond: func(command string) ([]byte, error) {
		return nil, fmt.Errorf("unexpected command %q", command)
	}}
	c := NewCollector("storage-1", []string{"tank"}, runner)
	c.kstatDir = dir

	sample := c.CollectPerf(context.Background())

	wantArc := map[string]int64{"hits": 123456789, "misses": 9876543, "size": 17179869184}
	if diff := cmp.Diff(wantArc, sample.Arc); diff != "" {
		t.Errorf("arc (-want +got):\n%s", diff)
	}

	wantDatasets := []DatasetIO{
		{Pool: "tank", Dataset: "tank/pvc-0a1b", Reads: 3, NreadBytes: 4096, Writes: 5, NwrittenBytes: 8192},
		{Pool: "tank", Dataset: "tank/foreign"},
	}
	if len(sample.Datasets) != len(wantDatasets) {
		t.Fatalf("datasets = %+v, want %d entries", sample.Datasets, len(wantDatasets))
	}
	for i, want := range wantDatasets {
		if sample.Datasets[i] != want {
			t.Errorf("datasets[%d] = %+v, want %+v", i, sample.Datasets[i], want)
		}
	}

	if len(runner.commands) != 0 {
		t.Fatalf("performance collection executed commands: %v", runner.commands)
	}
}

// TestCollectPerfNoKstats: without a kstat tree (local development, no /host
// mount) the sample degrades to empty instead of failing, and no zpool is
// executed.
func TestCollectPerfNoKstats(t *testing.T) {
	runner := &fakeRunner{respond: func(command string) ([]byte, error) {
		return nil, fmt.Errorf("unexpected command %q", command)
	}}
	c := NewCollector("storage-1", nil, runner)
	c.kstatDir = filepath.Join(t.TempDir(), "missing")

	sample := c.CollectPerf(context.Background())
	if len(sample.Arc) != 0 || len(sample.Datasets) != 0 {
		t.Errorf("sample = %+v, want all parts empty", sample)
	}
	if len(runner.commands) != 0 {
		t.Errorf("commands = %v, want none", runner.commands)
	}
}

// TestCollectPerfSkipsBrokenParts: an unattributable objset is skipped
// while ARC remains available.
func TestCollectPerfSkipsBrokenParts(t *testing.T) {
	dir := t.TempDir()
	writeKstat(t, filepath.Join(dir, "arcstats"), cannedKstatNamed)
	writeKstat(t, filepath.Join(dir, "tank", "objset-0x1"), // no dataset_name row
		"0 0 0 27 1112 1 1\nname                            type data\n"+
			"reads                           4    5\n")

	runner := &fakeRunner{respond: func(string) ([]byte, error) {
		return nil, fmt.Errorf("unexpected command")
	}}
	c := NewCollector("storage-1", nil, runner)
	c.kstatDir = dir

	sample := c.CollectPerf(context.Background())
	if len(sample.Arc) != 3 {
		t.Errorf("arc = %v, want the ARC part collected anyway", sample.Arc)
	}
	if len(sample.Datasets) != 0 {
		t.Errorf("datasets = %+v, want the nameless objset skipped", sample.Datasets)
	}
}
