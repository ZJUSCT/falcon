package zfsagent

import (
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
)

func unix(seconds int64) time.Time { return time.Unix(seconds, 0).UTC() }

// cannedZfsGet mirrors a `zfs get -r -Hp -t filesystem,volume,snapshot
// -o name,property,value <props> tank` run: the pool root, one zfs-localpv
// PVC dataset with two snapshots (a VolumeSnapshot one and a manual one), and
// a foreign dataset without any openebs properties.
const cannedZfsGet = `tank	used	1000
tank	referenced	900
tank	written	100
tank	creation	1725080000
tank	openebs.io:pvc-name	-
tank	openebs.io:pvc-namespace	-
tank	openebs.io:vs-name	-
tank	openebs.io:vs-namespace	-
tank/pvc-0a1b	used	137438953472
tank/pvc-0a1b	referenced	128849018880
tank/pvc-0a1b	written	5368709120
tank/pvc-0a1b	creation	1725080100
tank/pvc-0a1b	openebs.io:pvc-name	ubuntu-sync
tank/pvc-0a1b	openebs.io:pvc-namespace	mirror
tank/pvc-0a1b	openebs.io:vs-name	-
tank/pvc-0a1b	openebs.io:vs-namespace	-
tank/pvc-0a1b@snapcontent-9f8e	used	5368709120
tank/pvc-0a1b@snapcontent-9f8e	referenced	128849018880
tank/pvc-0a1b@snapcontent-9f8e	written	5368709120
tank/pvc-0a1b@snapcontent-9f8e	creation	1725090000
tank/pvc-0a1b@snapcontent-9f8e	openebs.io:vs-name	ubuntu-snap-1725090000
tank/pvc-0a1b@snapcontent-9f8e	openebs.io:vs-namespace	mirror
tank/pvc-0a1b@snapcontent-1a2b	used	4294967296
tank/pvc-0a1b@snapcontent-1a2b	referenced	125414502400
tank/pvc-0a1b@snapcontent-1a2b	written	4294967296
tank/pvc-0a1b@snapcontent-1a2b	creation	1725086400
tank/pvc-0a1b@snapcontent-1a2b	openebs.io:vs-name	-
tank/pvc-0a1b@snapcontent-1a2b	openebs.io:vs-namespace	-
tank/foreign	used	42
tank/foreign	referenced	42
tank/foreign	written	-
tank/foreign	creation	1725080200
` + "\tgarbage-line-without-value\n" + `not-a-triple

tank/pvc-0a1b	unknown-prop	ignored
`

func TestParseZfsGet(t *testing.T) {
	props := parseZfsGet([]byte(cannedZfsGet))

	if len(props) != 3 {
		t.Fatalf("got %d top-level entries (%v), want 3 (tank, tank/pvc-0a1b, tank/foreign)", len(props), keysOf(props))
	}

	root := props["tank"]
	if root == nil {
		t.Fatal("tank missing")
	}
	if root.used != 1000 || root.referenced != 900 || root.written != 100 || root.creation != 1725080000 {
		t.Errorf("tank numeric props = %+v", root)
	}
	if root.pvcName != "" || root.pvcNamespace != "" || root.vsName != "" {
		t.Errorf("tank openebs props must be unset (value \"-\"), got %+v", root)
	}
	if len(root.snapshots) != 0 {
		t.Errorf("tank must have no snapshots, got %d", len(root.snapshots))
	}

	ds := props["tank/pvc-0a1b"]
	if ds == nil {
		t.Fatal("tank/pvc-0a1b missing")
	}
	if ds.used != 137438953472 || ds.referenced != 128849018880 || ds.written != 5368709120 {
		t.Errorf("tank/pvc-0a1b numeric props = %+v", ds)
	}
	if ds.pvcName != "ubuntu-sync" || ds.pvcNamespace != "mirror" {
		t.Errorf("tank/pvc-0a1b pvc props = %q/%q", ds.pvcNamespace, ds.pvcName)
	}
	if len(ds.snapshots) != 2 {
		t.Fatalf("got %d snapshots, want 2", len(ds.snapshots))
	}
	vsSnap := ds.snapshots["tank/pvc-0a1b@snapcontent-9f8e"]
	if vsSnap == nil {
		t.Fatal("VolumeSnapshot-backed snapshot missing")
	}
	if vsSnap.written != 5368709120 || vsSnap.referenced != 128849018880 || vsSnap.creation != 1725090000 {
		t.Errorf("snapshot numeric props = %+v", vsSnap)
	}
	if vsSnap.vsName != "ubuntu-snap-1725090000" || vsSnap.vsNamespace != "mirror" {
		t.Errorf("snapshot vs props = %q/%q", vsSnap.vsNamespace, vsSnap.vsName)
	}
	manual := ds.snapshots["tank/pvc-0a1b@snapcontent-1a2b"]
	if manual == nil {
		t.Fatal("manual snapshot missing")
	}
	if manual.vsName != "" || manual.vsNamespace != "" {
		t.Errorf("manual snapshot must have unset vs props, got %+v", manual)
	}

	foreign := props["tank/foreign"]
	if foreign == nil {
		t.Fatal("tank/foreign missing")
	}
	if foreign.written != 0 {
		t.Errorf("written \"-\" must count as unset (0), got %d", foreign.written)
	}
}

// TestParseZfsGetSnapshotWithoutDatasetRow: a snapshot whose dataset has no
// own row must still be attached to a (zero-valued) dataset entry.
func TestParseZfsGetSnapshotWithoutDatasetRow(t *testing.T) {
	out := "tank/pvc-dead@orphan\twritten\t7\n" +
		"tank/pvc-dead@orphan\tcreation\t1725099999\n"
	props := parseZfsGet([]byte(out))

	parent := props["tank/pvc-dead"]
	if parent == nil {
		t.Fatal("parent dataset entry not created")
	}
	snap := parent.snapshots["tank/pvc-dead@orphan"]
	if snap == nil {
		t.Fatal("snapshot not attached to parent")
	}
	if snap.written != 7 || snap.creation != 1725099999 {
		t.Errorf("snapshot props = %+v", snap)
	}
}

func TestBuildReportOrderingAndShape(t *testing.T) {
	props := parseZfsGet([]byte(cannedZfsGet))
	generated := unix(1725100000)
	report := buildReport("storage-1", generated, []string{"tank"}, props)

	if report.Node != "storage-1" || !report.GeneratedAt.Equal(generated) {
		t.Errorf("report header = %s %v", report.Node, report.GeneratedAt)
	}
	if len(report.Pools) != 1 || report.Pools[0].Name != "tank" {
		t.Fatalf("pools = %+v", report.Pools)
	}
	datasets := report.Pools[0].Datasets
	// Dataset names sorted; the pool root included.
	var names []string
	for _, ds := range datasets {
		names = append(names, ds.Name)
	}
	wantNames := []string{"tank", "tank/foreign", "tank/pvc-0a1b"}
	if diff := cmp.Diff(wantNames, names); diff != "" {
		t.Errorf("dataset order (-want +got):\n%s", diff)
	}

	// Snapshots ordered by creation time ascending, wire refs built from the
	// openebs props; half-set/absent pairs become nil.
	ubuntu := datasets[2]
	if len(ubuntu.Snapshots) != 2 {
		t.Fatalf("got %d snapshots, want 2", len(ubuntu.Snapshots))
	}
	if ubuntu.Snapshots[0].Name != "tank/pvc-0a1b@snapcontent-1a2b" ||
		ubuntu.Snapshots[1].Name != "tank/pvc-0a1b@snapcontent-9f8e" {
		t.Errorf("snapshots not sorted by creation time: %+v", ubuntu.Snapshots)
	}
	if ubuntu.Snapshots[0].VolumeSnapshot != nil {
		t.Errorf("manual snapshot must have nil volumeSnapshot, got %+v", ubuntu.Snapshots[0].VolumeSnapshot)
	}
	wantRef := &ObjectRef{Namespace: "mirror", Name: "ubuntu-snap-1725090000"}
	if diff := cmp.Diff(wantRef, ubuntu.Snapshots[1].VolumeSnapshot); diff != "" {
		t.Errorf("volumeSnapshot ref (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff(&ObjectRef{Namespace: "mirror", Name: "ubuntu-sync"}, ubuntu.PVC); diff != "" {
		t.Errorf("pvc ref (-want +got):\n%s", diff)
	}
	if foreign := datasets[1]; foreign.PVC != nil || len(foreign.Snapshots) != 0 {
		t.Errorf("foreign dataset must carry no refs/snapshots: %+v", foreign)
	}
}

// TestBuildReportPoolMembership: with several pools each pool only carries
// its own datasets.
func TestBuildReportPoolMembership(t *testing.T) {
	props := parseZfsGet([]byte(
		"tank/a\tused\t1\n" +
			"zpool2/a\tused\t2\n" +
			"zpool2\tused\t3\n"))
	report := buildReport("n", unix(1), []string{"tank", "zpool2"}, props)
	if len(report.Pools) != 2 {
		t.Fatalf("got %d pools, want 2", len(report.Pools))
	}
	if got := len(report.Pools[0].Datasets); got != 1 || report.Pools[0].Datasets[0].Name != "tank/a" {
		t.Errorf("tank datasets = %+v", report.Pools[0].Datasets)
	}
	if got := report.Pools[1].Datasets; len(got) != 2 || got[0].Name != "zpool2" || got[1].Name != "zpool2/a" {
		t.Errorf("zpool2 datasets = %+v", got)
	}
}

func keysOf(m map[string]*datasetProps) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
