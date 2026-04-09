package worker

import (
	"testing"
)

func TestParseDatasetList(t *testing.T) {
	input := "tank/mirrors/debian\t/data/mirrors/debian\t107374182400\t536870912000\t107374182400\t214748364800\t214748364800\t2.00x\t5368709120\t102005473280\t0\t1073741824\t1712000000\t0\t0\t0\t131072\tlz4\n"
	datasets := parseDatasetList(input)
	if len(datasets) != 1 {
		t.Fatalf("expected 1 dataset, got %d", len(datasets))
	}
	ds := datasets[0]
	if ds.Name != "tank/mirrors/debian" {
		t.Errorf("name = %q", ds.Name)
	}
	if ds.MountPoint != "/data/mirrors/debian" {
		t.Errorf("mountpoint = %q", ds.MountPoint)
	}
	if ds.Used != 107374182400 {
		t.Errorf("used = %d", ds.Used)
	}
	if ds.CompressRatio != "2.00x" {
		t.Errorf("compressratio = %q", ds.CompressRatio)
	}
	if ds.Compression != "lz4" {
		t.Errorf("compression = %q", ds.Compression)
	}
}

func TestParseDatasetListMultiple(t *testing.T) {
	input := "tank/mirrors\t/data/mirrors\t500000000000\t536870912000\t100000\t600000000000\t100000\t1.20x\t0\t100000\t499999900000\t0\t1700000000\t0\t0\t0\t131072\tlz4\n" +
		"tank/mirrors/debian\t/data/mirrors/debian\t107374182400\t536870912000\t107374182400\t214748364800\t214748364800\t2.00x\t5368709120\t102005473280\t0\t1073741824\t1712000000\t0\t0\t0\t131072\tlz4\n" +
		"tank/mirrors/ubuntu\t/data/mirrors/ubuntu\t53687091200\t536870912000\t53687091200\t80530636800\t80530636800\t1.50x\t1073741824\t52613349376\t0\t536870912\t1713000000\t107374182400\t0\t0\t131072\tzstd\n"
	datasets := parseDatasetList(input)
	if len(datasets) != 3 {
		t.Fatalf("expected 3 datasets, got %d", len(datasets))
	}
	if datasets[2].Compression != "zstd" {
		t.Errorf("datasets[2].compression = %q", datasets[2].Compression)
	}
	if datasets[2].Quota != 107374182400 {
		t.Errorf("datasets[2].quota = %d", datasets[2].Quota)
	}
}

func TestParsePoolList(t *testing.T) {
	input := "tank\t10737418240000\t5368709120000\t5368709120000\t12\t50\t1.00x\tONLINE\t-\n"
	pools := parsePoolList(input)
	if len(pools) != 1 {
		t.Fatalf("expected 1 pool, got %d", len(pools))
	}
	p := pools[0]
	if p.Name != "tank" {
		t.Errorf("name = %q", p.Name)
	}
	if p.Health != "ONLINE" {
		t.Errorf("health = %q", p.Health)
	}
	if p.Size != 10737418240000 {
		t.Errorf("size = %d", p.Size)
	}
	if p.AltRoot != "" {
		t.Errorf("altroot = %q, want empty", p.AltRoot)
	}
}

func TestParseSnapshotList(t *testing.T) {
	input := "tank/mirrors/debian@auto-2025-04-01\t1073741824\t107374182400\t1711929600\n"
	snaps := parseSnapshotList(input)
	if len(snaps) != 1 {
		t.Fatalf("expected 1 snapshot, got %d", len(snaps))
	}
	s := snaps[0]
	if s.Dataset != "tank/mirrors/debian" {
		t.Errorf("dataset = %q", s.Dataset)
	}
	if s.SnapName != "auto-2025-04-01" {
		t.Errorf("snap_name = %q", s.SnapName)
	}
	if s.Used != 1073741824 {
		t.Errorf("used = %d", s.Used)
	}
}

func TestParseInt64(t *testing.T) {
	tests := []struct {
		in   string
		want int64
	}{
		{"12345", 12345},
		{"-", 0},
		{"none", 0},
		{"", 0},
		{"50%", 50},
		{" 999 ", 999},
	}
	for _, tc := range tests {
		got := parseInt64(tc.in)
		if got != tc.want {
			t.Errorf("parseInt64(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestParseEmptyOutput(t *testing.T) {
	if ds := parseDatasetList(""); len(ds) != 0 {
		t.Errorf("expected 0 datasets from empty, got %d", len(ds))
	}
	if ps := parsePoolList(""); len(ps) != 0 {
		t.Errorf("expected 0 pools from empty, got %d", len(ps))
	}
	if ss := parseSnapshotList(""); len(ss) != 0 {
		t.Errorf("expected 0 snapshots from empty, got %d", len(ss))
	}
}
