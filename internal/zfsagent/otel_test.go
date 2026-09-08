package zfsagent

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// The pusher must export the LAST observed cumulative value, not the sum of
// every Push: the sources (kstats, zpool iostat) are already-cumulative
// counters, and summing snapshots would inflate every rate() by an order of
// magnitude. This is why the instruments are observable (precomputed sums),
// and this test pins that semantics.
func TestOTLPPusherExportsLastSampleNotSum(t *testing.T) {
	reader := metric.NewManualReader()
	pusher, err := newPusher("storage-1", reader)
	if err != nil {
		t.Fatalf("newPusher: %v", err)
	}

	pusher.Push(&PerfSample{Arc: map[string]int64{"hits": 100, "size": 4096}}, nil)
	var md metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &md); err != nil {
		t.Fatalf("first Collect: %v", err)
	}

	// Second, larger snapshot: the export must read 150, never 250.
	pusher.Push(&PerfSample{Arc: map[string]int64{"hits": 150, "size": 8192}}, nil)
	if err := reader.Collect(context.Background(), &md); err != nil {
		t.Fatalf("second Collect: %v", err)
	}

	byName := map[string]metricdata.Metrics{}
	for _, sm := range md.ScopeMetrics {
		for _, m := range sm.Metrics {
			byName[m.Name] = m
		}
	}
	assertSum := func(name string, want int64) {
		t.Helper()
		m, ok := byName[name]
		if !ok {
			t.Fatalf("metric %q missing from export; got %v", name, byName)
		}
		sum, ok := m.Data.(metricdata.Sum[int64])
		if !ok {
			t.Fatalf("metric %q is %T, want Sum", name, m.Data)
		}
		if len(sum.DataPoints) != 1 {
			t.Fatalf("metric %q has %d data points, want 1", name, len(sum.DataPoints))
		}
		if got := sum.DataPoints[0].Value; got != want {
			t.Fatalf("metric %q = %d, want %d (sum-of-snapshots regression?)", name, got, want)
		}
	}
	assertSum("zfs_arc_hits", 150)

	gauge, ok := byName["zfs_arc_size_bytes"]
	if !ok {
		t.Fatalf("gauge zfs_arc_size_bytes missing from export")
	}
	g, ok := gauge.Data.(metricdata.Gauge[int64])
	if !ok || len(g.DataPoints) != 1 || g.DataPoints[0].Value != 8192 {
		t.Fatalf("zfs_arc_size_bytes = %+v, want one point 8192", gauge.Data)
	}
}

// Dataset series carry the PVC attribute resolved at Push time, so the
// metrics join against Kubernetes objects in the backend.
func TestOTLPPusherDatasetPVCAttribute(t *testing.T) {
	reader := metric.NewManualReader()
	pusher, err := newPusher("storage-1", reader)
	if err != nil {
		t.Fatalf("newPusher: %v", err)
	}

	pusher.Push(&PerfSample{Datasets: []DatasetIO{{
		Pool: "tank", Dataset: "tank/pvc-abc", Reads: 7, NreadBytes: 700,
	}}}, func(_, dataset string) string { return "mirrors/arch-sync" })

	var md metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &md); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	for _, sm := range md.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "zfs_dataset_reads" {
				continue
			}
			sum := m.Data.(metricdata.Sum[int64])
			if len(sum.DataPoints) != 1 {
				t.Fatalf("zfs_dataset_reads has %d data points, want 1", len(sum.DataPoints))
			}
			attrs := sum.DataPoints[0].Attributes
			pvc, _ := attrs.Value("pvc")
			if pvc.AsString() != "mirrors/arch-sync" {
				t.Fatalf("pvc attribute = %q, want mirrors/arch-sync", pvc.AsString())
			}
			dataset, _ := attrs.Value("dataset")
			if dataset.AsString() != "tank/pvc-abc" {
				t.Fatalf("dataset attribute = %q, want tank/pvc-abc", dataset.AsString())
			}
			return
		}
	}
	t.Fatal("zfs_dataset_reads missing from export")
}
