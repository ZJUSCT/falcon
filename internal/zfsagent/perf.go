package zfsagent

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// kstatRoot is where the agent finds the ZFS module's kstats: the DaemonSet
// mounts the host root at /host, so <kstatRoot>/<pool>/objset-0x* can be read
// directly — no chroot exec needed (/host is a read-only mount; reading is
// fine). The directory simply does not exist in local development
// environments without a host mount, which degrades to empty perf data.
const kstatRoot = "/host/proc/spl/kstat/zfs"

// arcKeySet are the arcstats entries kept in PerfSample.Arc. The full
// arcstats file has about a hundred keys; only the ones backing exported
// metrics are retained.
var arcKeySet = map[string]bool{
	"hits": true, "misses": true, "l2_hits": true, "l2_misses": true, "size": true,
}

// DatasetIO is one mounted dataset's logical I/O counters, from the pool's
// objset kstats. The counters are cumulative since the dataset was mounted
// (an unmounted dataset has no file; a remount starts the counters at zero —
// normal kstat semantics that rate() on the backend tolerates).
type DatasetIO struct {
	Pool, Dataset string
	// Reads and Writes are operation counts.
	Reads, Writes int64
	// NreadBytes and NwrittenBytes are byte counts.
	NreadBytes, NwrittenBytes int64
}

// VdevIO is the physical I/O of a pool or one of its vdevs, from
// `zpool iostat -Hp -v -y -T u <pool> 15`: rates averaged over a full collection
// interval, never cumulative counters. Leaf vdevs
// report 0 for alloc/free (only the top-level rows account space).
type VdevIO struct {
	Pool, Vdev string // Vdev equals Pool on the pool's own summary row
	// AllocBytes and FreeBytes are the row's space accounting columns.
	AllocBytes, FreeBytes int64
	// ReadOps and WriteOps are operations per second.
	ReadOps, WriteOps int64
	// ReadBytes and WriteBytes are bytes per second.
	ReadBytes, WriteBytes int64
}

// PerfSample is everything one performance collection gathered. Like Report
// it is best-effort: parts that could not be read are absent, not errors.
type PerfSample struct {
	// Arc is the retained subset of /proc/spl/kstat/zfs/arcstats.
	Arc map[string]int64
	// Datasets holds one entry per mounted dataset with an objset kstat.
	Datasets []DatasetIO
	// Vdevs holds the pool row and every vdev row of interval `zpool iostat`,
	// per pool.
	Vdevs []VdevIO
}

// CollectPerf gathers the node's ZFS performance counters: ARC hit/miss and
// size kstats, per-dataset logical I/O from the objset kstats, and per-pool /
// per-vdev physical I/O from zpool iostat. Every part degrades independently
// (warning + skip), mirroring Report's philosophy: perf data is telemetry, so
// a missing part must never fail the whole sample.
func (c *Collector) CollectPerf(ctx context.Context) *PerfSample {
	sample := &PerfSample{Arc: map[string]int64{}}

	root := kstatRoot
	if c.kstatDir != "" {
		root = c.kstatDir
	}

	pools, err := c.perfPools(root)
	if err != nil {
		c.log().WarnContext(ctx, "skipping ZFS perf collection", "path", root, "error", err.Error())
	} else {
		sample.Vdevs = c.collectIostat(ctx, pools)
		for _, pool := range pools {
			c.collectObjsets(sample, root, pool)
		}
	}

	// Read instantaneous kstats after the interval commands finish, so they
	// are fresh when the completed sample reaches the exporter.
	if out, err := os.ReadFile(filepath.Join(root, "arcstats")); err != nil {
		c.log().WarnContext(ctx, "skipping ARC stats", "path", root, "error", err.Error())
	} else {
		for key, value := range parseKstatNamed(out) {
			if arcKeySet[key] {
				sample.Arc[key] = value
			}
		}
	}
	return sample
}

// perfPools enumerates the pool directories under the kstat root, filtered by
// the collector's explicit pool list when set (same semantics as Report).
func (c *Collector) perfPools(root string) ([]string, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	var pools []string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if len(c.Pools) > 0 && !slices.Contains(c.Pools, entry.Name()) {
			continue
		}
		pools = append(pools, entry.Name())
	}
	return pools, nil
}

// collectObjsets reads one pool directory's objset-0x* kstat files into
// sample. Unreadable single files are skipped with a warning.
func (c *Collector) collectObjsets(sample *PerfSample, root, pool string) {
	entries, err := os.ReadDir(filepath.Join(root, pool))
	if err != nil {
		c.log().Warn("skipping dataset I/O stats", "pool", pool, "error", err.Error())
		return
	}
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, "objset-0x") {
			continue
		}
		out, err := os.ReadFile(filepath.Join(root, pool, name))
		if err != nil {
			c.log().Warn("skipping objset kstat", "pool", pool, "file", name, "error", err.Error())
			continue
		}
		ints, strs := parseKstat(out)
		ds := strs["dataset_name"]
		if ds == "" {
			// Cannot attribute the counters; not recoverable from the file
			// name (objset-0x* carries the object ID, not the name).
			continue
		}
		// A missing or malformed counter is not a measured zero: emitting it
		// would manufacture a reset and an inflated rate on the next sample.
		complete := true
		for _, key := range []string{"writes", "nwritten", "reads", "nread"} {
			if _, ok := ints[key]; !ok {
				complete = false
			}
		}
		if !complete {
			c.log().Warn("skipping incomplete objset kstat", "pool", pool, "file", name)
			continue
		}
		sample.Datasets = append(sample.Datasets, DatasetIO{
			Pool:          pool,
			Dataset:       ds,
			Writes:        ints["writes"],
			NwrittenBytes: ints["nwritten"],
			Reads:         ints["reads"],
			NreadBytes:    ints["nread"],
		})
	}
}

// parseKstatNamed parses the data rows of a named kstat file
// (/proc/spl/kstat/zfs/...): line 1 is the header, line 2 the column names
// (name, type, data), every further line is space-aligned
// `name type value`. Only the uint64 entries (type 4) are returned; string
// entries (e.g. dataset_name) are skipped — parseKstat returns both.
func parseKstatNamed(out []byte) map[string]int64 {
	ints, _ := parseKstat(out)
	return ints
}

// parseKstat is the two-map core behind parseKstatNamed: uint64 rows (type 4)
// in ints, every other typed row's raw value in strs. Malformed rows are
// skipped; the two leading header lines are not data.
//
// The columns are SPACE-aligned (kstat_seq_show_named pads with %-31s/%-4s),
// not tab-separated — verified against a live /proc/spl/kstat/zfs. String
// values keep any embedded spaces (numeric ones never have any) by joining
// everything after the type column.
func parseKstat(out []byte) (ints map[string]int64, strs map[string]string) {
	ints, strs = map[string]int64{}, map[string]string{}
	for i, line := range strings.Split(string(out), "\n") {
		if i < 2 { // header + column-name lines
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		name, typ := fields[0], fields[1]
		value := strings.Join(fields[2:], " ")
		if typ == "4" {
			if v, err := strconv.ParseInt(value, 10, 64); err == nil {
				ints[name] = v
			}
			continue
		}
		strs[name] = value
	}
	return ints, strs
}

// parseZpoolIostat parses the output of
//
//	zpool iostat -Hp -v -y -T u <pool> 15
//
// One row per vdev, the
// pool's own summary row first (its name column equals the pool name), with
// tab-separated columns name/alloc/free/read_ops/write_ops/read_bytes/
// write_bytes — bare integers; I/O values are rates per second. The CLI
// truncates fractional rates. Capacity fields can be "-" on leaf vdevs.
// Rows with the wrong column count or unparsable numbers are skipped.
func parseZpoolIostat(pool string, out []byte) []VdevIO {
	var vdevs []VdevIO
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Split(strings.TrimRight(line, "\r"), "\t")
		if len(fields) != 7 {
			continue
		}
		nums := make([]int64, 6)
		ok := true
		for i, field := range fields[1:] {
			if i < 2 && field == "-" {
				continue
			}
			v, err := strconv.ParseInt(field, 10, 64)
			if err != nil || v < 0 {
				ok = false
				break
			}
			nums[i] = v
		}
		if !ok {
			continue
		}
		vdevs = append(vdevs, VdevIO{
			Pool:       pool,
			Vdev:       fields[0],
			AllocBytes: nums[0],
			FreeBytes:  nums[1],
			ReadOps:    nums[2],
			WriteOps:   nums[3],
			ReadBytes:  nums[4],
			WriteBytes: nums[5],
		})
	}
	return vdevs
}
