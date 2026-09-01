// Package zfsagent implements the zfs-agent: a read-only ZFS usage reporter
// that runs as one DaemonSet pod per storage node and answers GET /v1/zfs
// with the usage of every dataset and snapshot the node's ZFS pools hold.
//
// The agent never talks to the Kubernetes API. Datasets and snapshots are
// attributed to Kubernetes objects purely through the OpenEBS zfs-localpv
// user properties (openebs.io:pvc-name / openebs.io:pvc-namespace on
// datasets, openebs.io:vs-name / openebs.io:vs-namespace on snapshots, whose
// values are the Kubernetes object names); the controller's webapi joins the
// per-node reports against Mirror CRs (see internal/webapi/usage.go).
package zfsagent

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"
)

// ObjectRef identifies the Kubernetes object a ZFS dataset or snapshot
// belongs to, recovered from the zfs-localpv user properties. It is null on
// the wire when the property pair is unset (dataset not managed by
// zfs-localpv — e.g. a pool root or a foreign dataset).
type ObjectRef struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

// Snapshot is one ZFS snapshot of a dataset.
type Snapshot struct {
	// Name is the full ZFS snapshot name (<dataset>@<snapshot>).
	Name string `json:"name"`
	// VolumeSnapshot is the VolumeSnapshot object the snapshot belongs to
	// (from the openebs.io:vs-* properties); null when unset.
	VolumeSnapshot *ObjectRef `json:"volumeSnapshot"`
	// WrittenBytes is the zfs written property: the delta against the
	// previous snapshot, not the snapshot's total size.
	WrittenBytes int64 `json:"writtenBytes"`
	// ReferencedBytes is the zfs referenced property (the snapshot's full
	// on-disk reference).
	ReferencedBytes int64 `json:"referencedBytes"`
	// CreatedAt is the zfs creation property in Unix seconds.
	CreatedAt int64 `json:"createdAt"`
}

// Dataset is one ZFS dataset. zfs-localpv provisions one dedicated dataset
// (<pool>/pvc-<uid>) per PersistentVolumeClaim.
type Dataset struct {
	Name string `json:"name"`
	// PVC is the PersistentVolumeClaim the dataset backs (from the
	// openebs.io:pvc-* properties); null when unset.
	PVC *ObjectRef `json:"pvc"`
	// UsedBytes is the zfs used property (space held by the dataset
	// including its snapshots' shared accounting).
	UsedBytes int64 `json:"usedBytes"`
	// ReferencedBytes is the zfs referenced property (the data the dataset
	// itself refers to).
	ReferencedBytes int64      `json:"referencedBytes"`
	WrittenBytes    int64      `json:"writtenBytes"`
	Snapshots       []Snapshot `json:"snapshots"`
}

// Pool is one ZFS pool with the datasets collected from it.
type Pool struct {
	Name     string    `json:"name"`
	Datasets []Dataset `json:"datasets"`
}

// Report is the wire shape of GET /v1/zfs.
type Report struct {
	// Node is the Kubernetes node name the agent reports for.
	Node        string    `json:"node"`
	GeneratedAt time.Time `json:"generatedAt"`
	Pools       []Pool    `json:"pools"`
}

// zfsGetProps are the properties fetched for every dataset and snapshot. The
// openebs.io:* user properties are written by zfs-localpv; pools from other
// backends simply report "-" for them.
const zfsGetProps = "used,referenced,written,creation," +
	"openebs.io:pvc-name,openebs.io:pvc-namespace," +
	"openebs.io:vs-name,openebs.io:vs-namespace"

// Collector builds Reports by executing the host's zfs/zpool binaries.
type Collector struct {
	// Node is reported verbatim as Report.Node (the Kubernetes node name).
	Node string
	// Pools limits collection to these pools. Empty means all pools,
	// enumerated with zpool list.
	Pools []string
	// Runner executes zfs/zpool (chrooted into the host root in containers).
	Runner Runner
	// Log receives one warning per pool that could not be read. Nil uses a
	// discarding logger.
	Log *slog.Logger

	now func() time.Time // injectable for tests
}

// NewCollector returns a collector reporting for node.
func NewCollector(node string, pools []string, runner Runner) *Collector {
	return &Collector{Node: node, Pools: pools, Runner: runner, now: time.Now}
}

// Report collects the ZFS usage of every pool. A pool whose zfs get fails
// (exported, suspended, ...) is skipped with a warning — a single broken pool
// must not take down the whole node report. An error is only returned when
// collection cannot happen at all (pool enumeration failed).
func (c *Collector) Report(ctx context.Context) (*Report, error) {
	pools := c.Pools
	if len(pools) == 0 {
		var err error
		if pools, err = c.listPools(ctx); err != nil {
			return nil, fmt.Errorf("list ZFS pools: %w", err)
		}
	}

	all := map[string]*datasetProps{}
	for _, pool := range pools {
		out, err := c.Runner.Run(ctx, "zfs",
			"get", "-r", "-Hp", "-t", "filesystem,volume,snapshot",
			"-o", "name,property,value", zfsGetProps, pool,
		)
		if err != nil {
			c.log().WarnContext(ctx, "skipping ZFS pool", "pool", pool, "error", err.Error())
			continue
		}
		mergeProps(all, parseZfsGet(out))
	}
	return buildReport(c.Node, c.now(), pools, all), nil
}

// listPools enumerates all pools with zpool list.
func (c *Collector) listPools(ctx context.Context) ([]string, error) {
	out, err := c.Runner.Run(ctx, "zpool", "list", "-H", "-o", "name")
	if err != nil {
		return nil, err
	}
	var pools []string
	for _, line := range strings.Split(string(out), "\n") {
		if name := strings.TrimSpace(line); name != "" {
			pools = append(pools, name)
		}
	}
	return pools, nil
}

func (c *Collector) log() *slog.Logger {
	if c.Log == nil {
		return slog.New(slog.DiscardHandler)
	}
	return c.Log
}

// mergeProps folds one pool's parsed properties into the node-wide map. The
// per-pool namespaces are disjoint, so entries never collide.
func mergeProps(all, pool map[string]*datasetProps) {
	for name, p := range pool {
		all[name] = p
	}
}

// buildReport converts the flat property map into the wire report: datasets
// grouped per pool (in enumeration order), dataset names sorted, snapshots
// sorted by creation time.
func buildReport(node string, generatedAt time.Time, pools []string, all map[string]*datasetProps) *Report {
	names := make([]string, 0, len(all))
	for name := range all {
		names = append(names, name)
	}
	sort.Strings(names)

	report := &Report{Node: node, GeneratedAt: generatedAt, Pools: make([]Pool, 0, len(pools))}
	for _, pool := range pools {
		p := Pool{Name: pool, Datasets: []Dataset{}}
		for _, name := range names {
			// With -r <pool> zfs only reports the pool's own tree; the
			// prefix check keeps the grouping correct even if a stray line
			// ever escapes it.
			if name != pool && !strings.HasPrefix(name, pool+"/") {
				continue
			}
			p.Datasets = append(p.Datasets, newDataset(name, all[name]))
		}
		report.Pools = append(report.Pools, p)
	}
	return report
}

// newDataset converts one dataset's properties (including its snapshots) to
// the wire shape.
func newDataset(name string, p *datasetProps) Dataset {
	ds := Dataset{
		Name:            name,
		UsedBytes:       p.used,
		ReferencedBytes: p.referenced,
		WrittenBytes:    p.written,
		Snapshots:       make([]Snapshot, 0, len(p.snapshots)),
	}
	ds.PVC = openebsRef(p.pvcNamespace, p.pvcName)

	snaps := make([]string, 0, len(p.snapshots))
	for snapName := range p.snapshots {
		snaps = append(snaps, snapName)
	}
	sort.Slice(snaps, func(i, j int) bool {
		a, b := p.snapshots[snaps[i]], p.snapshots[snaps[j]]
		if a.creation != b.creation {
			return a.creation < b.creation
		}
		return snaps[i] < snaps[j]
	})
	for _, snapName := range snaps {
		sp := p.snapshots[snapName]
		snap := Snapshot{
			Name:            snapName,
			WrittenBytes:    sp.written,
			ReferencedBytes: sp.referenced,
			CreatedAt:       sp.creation,
		}
		snap.VolumeSnapshot = openebsRef(sp.vsNamespace, sp.vsName)
		ds.Snapshots = append(ds.Snapshots, snap)
	}
	return ds
}

// openebsRef recovers a Kubernetes object reference from the property pair
// written by zfs-localpv. Both properties must be set: a half-set pair cannot
// be joined against namespaced objects, so it counts as unset.
func openebsRef(namespace, name string) *ObjectRef {
	if namespace == "" || name == "" {
		return nil
	}
	return &ObjectRef{Namespace: namespace, Name: name}
}
