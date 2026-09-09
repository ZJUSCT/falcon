package zfsagent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
		if command == "zpool iostat -Hp -v -y -T u tank 15" {
			return []byte(cannedIostat), nil
		}
		return nil, fmt.Errorf("unexpected command %q", command)
	}}
	c := NewCollector("storage-1", []string{"tank"}, runner)
	c.kstatDir = dir
	defer c.ClosePerf()

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
	defer c.ClosePerf()

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

// A second pool must start sampling before the first finishes. Each gets
// a shared collection deadline so a wedged CLI cannot freeze later samples.
func TestCollectPerfSamplesPoolsConcurrently(t *testing.T) {
	dir := t.TempDir()
	writeKstat(t, filepath.Join(dir, "tank", "state"), "ONLINE")
	writeKstat(t, filepath.Join(dir, "other", "state"), "ONLINE")
	started := make(chan string, 2)
	release := make(chan struct{})
	runner := perfRunnerFunc(func(ctx context.Context, _ string, args ...string) ([]byte, error) {
		pool := args[6]
		started <- pool
		<-release
		return []byte(strings.ReplaceAll(cannedIostat, "tank", pool)), nil
	})
	c := NewCollector("storage-1", nil, runner)
	c.kstatDir = dir
	defer c.ClosePerf()
	done := make(chan *PerfSample, 1)
	go func() { done <- c.CollectPerf(context.Background()) }()
	for range 2 {
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			close(release)
			t.Fatal("pool collections serialized")
		}
	}
	close(release)
	sample := <-done
	if len(sample.Vdevs) != 8 || sample.Vdevs[0].Pool != "other" || sample.Vdevs[4].Pool != "tank" {
		t.Fatalf("pool results lost or interleaved: %+v", sample.Vdevs)
	}
}

type perfRunnerFunc func(context.Context, string, ...string) ([]byte, error)

func (f perfRunnerFunc) Run(ctx context.Context, bin string, args ...string) ([]byte, error) {
	return f(ctx, bin, args...)
}

func (f perfRunnerFunc) Stream(ctx context.Context, bin string, line func(string), args ...string) error {
	out, err := f(ctx, bin, args...)
	if err != nil {
		return err
	}
	line("100")
	for _, row := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		line(row)
	}
	line("115")
	<-ctx.Done()
	return ctx.Err()
}
