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
tank	logicalused	1500
tank	creation	1725080000
tank	openebs.io:pvc-name	-
tank	openebs.io:pvc-namespace	-
tank	openebs.io:vs-name	-
tank	openebs.io:vs-namespace	-
tank/pvc-0a1b	used	137438953472
tank/pvc-0a1b	referenced	128849018880
tank/pvc-0a1b	written	5368709120
tank/pvc-0a1b	logicalused	214748364800
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
	if root.logicalused != 1500 {
		t.Errorf("tank logicalused = %d, want 1500", root.logicalused)
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
	if ds.logicalused != 214748364800 {
		t.Errorf("tank/pvc-0a1b logicalused = %d, want 214748364800", ds.logicalused)
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
	report := buildReport("storage-1", generated, []string{"tank"}, nil, props)

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
	if ubuntu.LogicalUsedBytes != 214748364800 {
		t.Errorf("ubuntu logicalUsedBytes = %d, want 214748364800", ubuntu.LogicalUsedBytes)
	}
	if foreign := datasets[1]; foreign.PVC != nil || foreign.LogicalUsedBytes != 0 || len(foreign.Snapshots) != 0 {
		t.Errorf("foreign dataset must carry no refs/logicalused/snapshots: %+v", foreign)
	}
}

// TestBuildReportPoolMembership: with several pools each pool only carries
// its own datasets.
func TestBuildReportPoolMembership(t *testing.T) {
	props := parseZfsGet([]byte(
		"tank/a\tused\t1\n" +
			"zpool2/a\tused\t2\n" +
			"zpool2\tused\t3\n"))
	report := buildReport("n", unix(1), []string{"tank", "zpool2"}, nil, props)
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

// cannedZpoolGet mirrors a `zpool get -Hp -o name,property,value
// size,allocated,free,capacity,fragmentation,health tank` run: exact bytes
// for size/allocated/free, bare capacity, fragmentation not yet computed
// (ZFS prints "-"), and the pool state.
const cannedZpoolGet = `tank	size	1099511627776
tank	allocated	137438953472
tank	free	962072674304
tank	capacity	12
tank	fragmentation	-
tank	health	ONLINE
`

func TestParseZpoolGet(t *testing.T) {
	props := parseZpoolGet([]byte(cannedZpoolGet))
	want := map[string]string{
		"size": "1099511627776", "allocated": "137438953472", "free": "962072674304",
		"capacity": "12", "fragmentation": "-", "health": "ONLINE",
	}
	if diff := cmp.Diff(want, props); diff != "" {
		t.Errorf("zpool get props (-want +got):\n%s", diff)
	}

	// Malformed lines are skipped, not fatal.
	props = parseZpoolGet([]byte("tank\tsize\t1\nnot-a-triple\ntank\thealth\tDEGRADED\n\n"))
	if len(props) != 2 || props["size"] != "1" || props["health"] != "DEGRADED" {
		t.Errorf("props = %v, want the two well-formed triples only", props)
	}
}

func TestParsePoolCapacity(t *testing.T) {
	tests := []struct {
		name  string
		props map[string]string
		want  poolCapacity
	}{
		{
			name:  "full",
			props: map[string]string{"size": "1099511627776", "allocated": "137438953472", "free": "962072674304", "capacity": "12", "fragmentation": "9", "health": "ONLINE"},
			want:  poolCapacity{size: 1099511627776, allocated: 137438953472, free: 962072674304, capacity: 12, fragmentation: 9, health: "ONLINE"},
		},
		{
			name:  "percent suffix tolerated",
			props: map[string]string{"capacity": "12%", "fragmentation": "9%"},
			want:  poolCapacity{capacity: 12, fragmentation: 9},
		},
		{
			name:  "dash means unknown",
			props: map[string]string{"fragmentation": "-", "health": "-"},
			want:  poolCapacity{},
		},
		{
			name:  "garbage and missing values mean unknown",
			props: map[string]string{"size": "one terabyte", "capacity": ""},
			want:  poolCapacity{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if diff := cmp.Diff(tt.want, parsePoolCapacity(tt.props), cmp.AllowUnexported(poolCapacity{})); diff != "" {
				t.Errorf("parsePoolCapacity (-want +got):\n%s", diff)
			}
		})
	}
}

// cannedIostat mirrors an interval `zpool iostat -Hp -v -y tank 15 1` run: the pool's summary
// row first, then one row per vdev (leaf rows carry 0 alloc/free).
const cannedIostat = `tank	137438953472	962072674304	123456	234567	999999999999	888888888888
mirror-0	0	0	123400	234500	999999999900	888888888800
sda-part2	0	0	61700	117250	499999999950	444444444400
sdb-part2	0	0	61700	117250	499999999950	444444444400
`

func TestParseZpoolIostat(t *testing.T) {
	vdevs := parseZpoolIostat("tank", []byte(cannedIostat))

	want := []VdevIO{
		{Pool: "tank", Vdev: "tank", AllocBytes: 137438953472, FreeBytes: 962072674304,
			ReadOps: 123456, WriteOps: 234567, ReadBytes: 999999999999, WriteBytes: 888888888888},
		{Pool: "tank", Vdev: "mirror-0", ReadOps: 123400, WriteOps: 234500,
			ReadBytes: 999999999900, WriteBytes: 888888888800},
		{Pool: "tank", Vdev: "sda-part2", ReadOps: 61700, WriteOps: 117250,
			ReadBytes: 499999999950, WriteBytes: 444444444400},
		{Pool: "tank", Vdev: "sdb-part2", ReadOps: 61700, WriteOps: 117250,
			ReadBytes: 499999999950, WriteBytes: 444444444400},
	}
	if diff := cmp.Diff(want, vdevs); diff != "" {
		t.Errorf("vdevs (-want +got):\n%s", diff)
	}

	// Some CLI versions emit "-" capacity fields for leaf vdevs. These
	// must not hide otherwise valid I/O rates.
	leaf := parseZpoolIostat("tank", []byte("sdc\t-\t-\t0\t9\t0\t4096\n"))
	if len(leaf) != 1 || leaf[0].WriteBytes != 4096 {
		t.Fatalf("leaf with absent capacity: %+v", leaf)
	}

	// Rows with the wrong column count or invalid I/O fields are skipped.
	broken := parseZpoolIostat("tank", []byte("tank\t1\t2\t3\t4\t5\n"+ // 6 columns
		"weird\t1\t2\tthree\t4\t5\t6\n"+ // unparsable number
		"negative\t1\t2\t-1\t4\t5\t6\n"+
		"missing\t0\t0\t-\t4\t5\t6\n"+
		"spaced 1 2 3 4 5 6\n")) // not tab-separated
	if len(broken) != 0 {
		t.Errorf("broken rows must be skipped, got %+v", broken)
	}
}

// cannedKstatNamed mirrors the named-kstat file layout as the kernel prints
// it (verified against a live /proc/spl/kstat/zfs): a header line, the
// "name type data" column line, then rows space-aligned via %-31s/%-4s. Type
// 4 is uint64; the string row (dataset_name) is for the string map.
const cannedKstatNamed = `24 1 0x01 96 9920 2952166985 6657350817580
name                            type data
hits                            4    123456789
misses                          4    9876543
size                            4    17179869184
not-a-number                    4    NaN
dataset_name                    7    tank/pvc-0a1b
`

func TestParseKstatNamed(t *testing.T) {
	ints := parseKstatNamed([]byte(cannedKstatNamed))
	want := map[string]int64{"hits": 123456789, "misses": 9876543, "size": 17179869184}
	if diff := cmp.Diff(want, ints); diff != "" {
		t.Errorf("kstat ints (-want +got):\n%s", diff)
	}

	// The two-map core: uint64 rows in ints, everything else in strs.
	ints, strs := parseKstat([]byte(cannedKstatNamed))
	if diff := cmp.Diff(want, ints); diff != "" {
		t.Errorf("kstat ints (-want +got):\n%s", diff)
	}
	if strs["dataset_name"] != "tank/pvc-0a1b" {
		t.Errorf("kstat strs = %v, want dataset_name", strs)
	}
}

func keysOf(m map[string]*datasetProps) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
