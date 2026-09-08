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

// TestCollectPerf walks a faked kstat tree plus canned iostat output: ARC
// subset, per-dataset counters from the objset kstats (only mounted datasets
// have files; the pool list filters other pools), and the pool/vdev rows.
func TestCollectPerf(t *testing.T) {
	dir := t.TempDir()
	writeKstat(t, filepath.Join(dir, "arcstats"), cannedKstatNamed) // hits/misses/size rows
	writeKstat(t, filepath.Join(dir, "tank", "objset-0x163"), cannedKstatNamed)
	// A second mounted dataset, no zfs-localpv PVC behind it.
	writeKstat(t, filepath.Join(dir, "tank", "objset-0x164"),
		"0 0 0 27 1112 1 1\nname                            type data\n"+
			"dataset_name                    7    tank/foreign\n"+
			"reads                           4    0\nnread                           4    0\n"+
			"writes                          4    0\nnwritten                        4    0\n")
	// Not a pool directory member the collector should read.
	writeKstat(t, filepath.Join(dir, "tank", "not-objset"), "noise\n")
	// A pool outside c.Pools: must be filtered out.
	writeKstat(t, filepath.Join(dir, "other", "objset-0x1"),
		"0 0 0 27 1112 1 1\nname                            type data\n"+
			"dataset_name                    7    other/x\n"+
			"reads                           4    1\nnread                           4    1\n"+
			"writes                          4    1\nnwritten                        4    1\n")

	runner := &fakeRunner{respond: func(command string) ([]byte, error) {
		if command == "zpool iostat -Hp -v tank" {
			return []byte(cannedIostat), nil
		}
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
		{Pool: "tank", Dataset: "tank/pvc-0a1b"}, // cannedKstatNamed has no I/O rows: zero counters
		{Pool: "tank", Dataset: "tank/foreign"},
	}
	if len(sample.Datasets) != len(wantDatasets) {
		t.Fatalf("datasets = %+v, want %d entries", sample.Datasets, len(wantDatasets))
	}
	for i, want := range wantDatasets {
		if sample.Datasets[i].Pool != want.Pool || sample.Datasets[i].Dataset != want.Dataset {
			t.Errorf("datasets[%d] = %+v, want %+v", i, sample.Datasets[i], want)
		}
	}

	if len(sample.Vdevs) != 4 || sample.Vdevs[0].Vdev != "tank" || sample.Vdevs[1].Vdev != "mirror-0" {
		t.Errorf("vdevs = %+v, want the canned iostat rows", sample.Vdevs)
	}
	if sample.Vdevs[0].ReadBytes != 999999999999 || sample.Vdevs[1].AllocBytes != 0 {
		t.Errorf("vdev counters mismatch: %+v", sample.Vdevs)
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
	if len(sample.Arc) != 0 || len(sample.Datasets) != 0 || len(sample.Vdevs) != 0 {
		t.Errorf("sample = %+v, want all parts empty", sample)
	}
	if len(runner.commands) != 0 {
		t.Errorf("commands = %v, want none", runner.commands)
	}
}

// TestCollectPerfSkipsBrokenParts: a failed iostat exec and an unattributable
// objset file are skipped with warnings; the rest of the sample survives.
func TestCollectPerfSkipsBrokenParts(t *testing.T) {
	dir := t.TempDir()
	writeKstat(t, filepath.Join(dir, "arcstats"), cannedKstatNamed)
	writeKstat(t, filepath.Join(dir, "tank", "objset-0x1"), // no dataset_name row
		"0 0 0 27 1112 1 1\nname                            type data\n"+
			"reads                           4    5\n")

	runner := &fakeRunner{respond: func(string) ([]byte, error) {
		return nil, fmt.Errorf("iostat exploded")
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
	if len(sample.Vdevs) != 0 {
		t.Errorf("vdevs = %+v, want the pool's iostat skipped", sample.Vdevs)
	}
}
