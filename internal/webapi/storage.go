package webapi

import (
	"context"
	"net/http"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	mirrorv1alpha1 "github.com/ZJUSCT/falcon/api/v1alpha1"
	"github.com/ZJUSCT/falcon/internal/zfsagent"
)

// StorageNode is one node's ZFS report, verbatim from its zfs-agent: every
// pool with its datasets, capacity and health fields.
type StorageNode struct {
	Node  string          `json:"node"`
	Pools []zfsagent.Pool `json:"pools"`
}

// StorageResponse is the wire shape of GET /api/storage.
type StorageResponse struct {
	// GeneratedAt is when the aggregation was computed (the served data is
	// at most one cache TTL older).
	GeneratedAt time.Time `json:"generatedAt"`
	// Complete is false when any agent failed or no agent is ready; Errors
	// carries the per-agent reasons.
	Complete bool     `json:"complete"`
	Errors   []string `json:"errors"`
	// Nodes are the reporting nodes, sorted by node name.
	Nodes []StorageNode `json:"nodes"`
}

// Storage returns the /api/storage payload: the (cached) per-node ZFS reports
// of all zfs-agents, sharing /api/usage's aggregation and cache, with every
// dataset attributed to its owning Mirror where possible. An error is
// returned only when the aggregation cannot be computed at all; per-agent
// failures degrade the result instead.
func (a *UsageAggregator) Storage(ctx context.Context, reader client.Reader) (*StorageResponse, error) {
	agg, err := a.aggregate(ctx)
	if err != nil {
		return nil, err
	}
	mirrorOfDataset, err := mirrorsByDataset(ctx, reader, agg)
	if err != nil {
		return nil, err
	}
	// The aggregate is cache-shared (and /api/usage may serve it
	// concurrently): enrich a copy so nothing mutates the cached reports.
	nodes := make([]StorageNode, len(agg.nodes))
	for i, node := range agg.nodes {
		enriched := StorageNode{Node: node.Node, Pools: make([]zfsagent.Pool, len(node.Pools))}
		for j, pool := range node.Pools {
			p := pool
			p.Datasets = make([]zfsagent.Dataset, len(pool.Datasets))
			for k, ds := range pool.Datasets {
				ds.Mirror = mirrorOfDataset[ds.Name]
				p.Datasets[k] = ds
			}
			enriched.Pools[j] = p
		}
		nodes[i] = enriched
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].Node < nodes[j].Node })
	return &StorageResponse{
		GeneratedAt: agg.generatedAt,
		Complete:    agg.complete,
		Errors:      agg.errors,
		Nodes:       nodes,
	}, nil
}

// mirrorsByDataset maps a ZFS dataset name to the Mirror (or ProxyMirror)
// that owns it. The join goes dataset → PVC → Mirror: a dataset's leaf is the
// CSI volume name, which is claim.Spec.VolumeName (OpenEBS <= 2.11 without
// user properties) or the claim the openebs.io:pvc-* properties name (newer
// drivers); the PVC name then matches the controller's child naming —
// <base>-sync, <base>-snap-<timestamp>, <base>-cache — or the Mirror status
// pointers.
func mirrorsByDataset(ctx context.Context, reader client.Reader, agg *usageAggregate) (map[string]string, error) {
	var mirrors mirrorv1alpha1.MirrorList
	if err := reader.List(ctx, &mirrors); err != nil {
		return nil, err
	}
	var proxies mirrorv1alpha1.ProxyMirrorList
	if err := reader.List(ctx, &proxies); err != nil {
		return nil, err
	}
	var claims corev1.PersistentVolumeClaimList
	if err := reader.List(ctx, &claims); err != nil {
		return nil, err
	}

	// exact: PVC names the controller status/naming pins deterministically.
	// bases: (derived child base, mirror name) for <base>-snap-<ts> history.
	exact := map[string]string{}
	type baseEntry struct{ base, mirror string }
	var bases []baseEntry
	addMirror := func(crName, mirror string) {
		base := strings.TrimSuffix(deriveSyncPVCName(crName), "-sync")
		bases = append(bases, baseEntry{base, mirror})
	}
	for i := range mirrors.Items {
		m := &mirrors.Items[i]
		if m.Status.WorkPVC != "" {
			exact[m.Status.WorkPVC] = m.Name
		}
		if m.Status.ActivePVC != "" {
			exact[m.Status.ActivePVC] = m.Name
		}
		addMirror(m.Name, m.Name)
	}
	for i := range proxies.Items {
		p := &proxies.Items[i]
		exact[strings.TrimSuffix(deriveSyncPVCName(p.Name), "-sync")+"-cache"] = p.Name
	}

	claimByVolume := map[string]*corev1.PersistentVolumeClaim{}
	claimByNSName := map[string]*corev1.PersistentVolumeClaim{}
	for i := range claims.Items {
		claim := &claims.Items[i]
		if claim.Spec.VolumeName != "" {
			claimByVolume[claim.Spec.VolumeName] = claim
		}
		claimByNSName[claim.Namespace+"/"+claim.Name] = claim
	}
	lookupMirror := func(pvcName string) string {
		if mirror, ok := exact[pvcName]; ok {
			return mirror
		}
		for _, b := range bases {
			if b.base != "" && strings.HasPrefix(pvcName, b.base+"-snap-") {
				return b.mirror
			}
		}
		return ""
	}

	mirrorOfDataset := map[string]string{}
	for leaf, ds := range agg.byVolumeName {
		claim := claimByVolume[leaf]
		if claim == nil && ds.PVC != nil {
			claim = claimByNSName[ds.PVC.Namespace+"/"+ds.PVC.Name]
		}
		if claim != nil {
			if mirror := lookupMirror(claim.Name); mirror != "" {
				mirrorOfDataset[ds.Name] = mirror
			}
		}
	}
	return mirrorOfDataset, nil
}

// handleStorage serves GET /api/storage. Like /api/usage, the endpoint is
// gated on wiring: with ZFS_AGENT_SERVICE unset no aggregator exists and the
// endpoint answers 404.
func (s *Server) handleStorage(w http.ResponseWriter, r *http.Request) {
	if s.Usage == nil {
		writeJSONError(w, http.StatusNotFound, "storage aggregation is disabled")
		return
	}
	storage, err := s.Usage.Storage(r.Context(), s.Client)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, storage)
}
