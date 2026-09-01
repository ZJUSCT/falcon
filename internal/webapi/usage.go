package webapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"

	mirrorv1alpha1 "github.com/ZJUSCT/falcon/api/v1alpha1"
	"github.com/ZJUSCT/falcon/internal/zfsagent"
	snapshotv1 "github.com/kubernetes-csi/external-snapshotter/client/v8/apis/volumesnapshot/v1"
)

// The zfs-agent contract is deliberately not configurable: the port and the
// timings are code constants shared with the chart templates (the headless
// Service targets 9474, exactly like the probe ports of the controller) and
// with the agent itself. The only configuration is the service name, passed
// through the ZFS_AGENT_SERVICE environment variable (see cmd/controller).
const (
	// zfsAgentPort is the port every zfs-agent listens on.
	zfsAgentPort = 9474
	// zfsAgentFetchTimeout bounds a single agent request so one hung node
	// cannot stall the aggregation.
	zfsAgentFetchTimeout = 5 * time.Second
	// usageCacheTTL is how long an aggregation (successful or degraded) is
	// served from cache: /api/usage is polled by dashboards, and fan-out to
	// every storage node per request would be wasteful.
	usageCacheTTL = 30 * time.Second
)

// The webapi aggregator reads the EndpointSlices of the zfs-agent headless
// Service to discover the per-node agents. (Rendered into the namespaced Role
// only when the chart's zfsAgent is enabled.)
//
// +kubebuilder:rbac:groups=discovery.k8s.io,resources=endpointslices,verbs=get;list

// AgentEndpoint is one ready zfs-agent instance discovered through the
// service's EndpointSlices.
type AgentEndpoint struct {
	// Addr is a dialable address (the agent pod IP).
	Addr string
	// NodeName is the Kubernetes node the agent pod runs on, used to label
	// that agent's failures in the usage response. It falls back to Addr
	// when the slice does not carry it.
	NodeName string
}

// AgentAddresser lists the ready zfs-agent endpoints. Abstracted so the
// aggregator can be tested without a cluster.
type AgentAddresser interface {
	AgentEndpoints(ctx context.Context) ([]AgentEndpoint, error)
}

// EndpointSliceAddresser is the production AgentAddresser: it lists the
// EndpointSlices of one Service in one namespace and collects the addresses
// of the ready endpoints (deduplicated — EndpointSlices may overlap while
// being updated).
type EndpointSliceAddresser struct {
	Client    kubernetes.Interface
	Namespace string
	Service   string
}

// AgentEndpoints implements AgentAddresser.
func (a *EndpointSliceAddresser) AgentEndpoints(ctx context.Context) ([]AgentEndpoint, error) {
	slices, err := a.Client.DiscoveryV1().EndpointSlices(a.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: discoveryv1.LabelServiceName + "=" + a.Service,
	})
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var endpoints []AgentEndpoint
	for i := range slices.Items {
		slice := &slices.Items[i]
		for j := range slice.Endpoints {
			ep := &slice.Endpoints[j]
			// Ready == nil means unknown state, which the API docs say
			// consumers should interpret as ready (terminating endpoints
			// explicitly carry Ready=false).
			if ep.Conditions.Ready != nil && !*ep.Conditions.Ready {
				continue
			}
			nodeName := ""
			if ep.NodeName != nil {
				nodeName = *ep.NodeName
			}
			for _, addr := range ep.Addresses {
				if addr == "" || seen[addr] {
					continue
				}
				seen[addr] = true
				endpoints = append(endpoints, AgentEndpoint{Addr: addr, NodeName: nodeName})
			}
		}
	}
	sort.Slice(endpoints, func(i, j int) bool { return endpoints[i].Addr < endpoints[j].Addr })
	return endpoints, nil
}

// UsageSync is the per-Mirror sync PVC section of GET /api/usage.
type UsageSync struct {
	PVC             string `json:"pvc"`
	ReferencedBytes int64  `json:"referencedBytes"`
}

// UsageSnapshot is one VolumeSnapshot's usage on GET /api/usage.
type UsageSnapshot struct {
	Name         string `json:"name"`
	WrittenBytes int64  `json:"writtenBytes"`
	CreatedAt    int64  `json:"createdAt"`
}

// UsageMirror is one Mirror's entry of GET /api/usage.
type UsageMirror struct {
	Name string `json:"name"`
	// Sync is null when no agent reported the Mirror's sync PVC (never
	// synced, sync in flight on a node without an agent, or aggregation
	// incomplete).
	Sync       *UsageSync      `json:"sync"`
	Snapshots  []UsageSnapshot `json:"snapshots"`
	TotalBytes int64           `json:"totalBytes"`
	// Complete mirrors the aggregation completeness: false when any agent
	// failed or no agent is ready, in which case Errors is non-empty for
	// every Mirror.
	Complete bool     `json:"complete"`
	Errors   []string `json:"errors"`
}

// UsageResponse is the wire shape of GET /api/usage.
type UsageResponse struct {
	GeneratedAt time.Time     `json:"generatedAt"`
	Mirrors     []UsageMirror `json:"mirrors"`
}

// usageAggregate is the cached, merged view of all agent reports.
type usageAggregate struct {
	// generatedAt is when the aggregation was computed (the served data is
	// at most one cache TTL older). Node clock skews do not leak into it.
	generatedAt time.Time
	// fetchedAt drives the cache TTL.
	fetchedAt time.Time
	complete  bool
	errors    []string
	// byPVC indexes datasets carrying an openebs PVC reference, keyed by
	// "<namespace>/<pvc name>".
	byPVC map[string]*zfsagent.Dataset
	// byVolumeName indexes every dataset by the final component of its ZFS
	// name. OpenEBS <= 2.11 does not write the openebs.io:pvc-* user
	// properties, but its dataset's final component is the CSI volume name,
	// which is also PVC.spec.volumeName. This fallback needs no cluster-scoped
	// PV or VolumeSnapshotContent permissions.
	byVolumeName map[string]*zfsagent.Dataset
}

// UsageAggregator merges the per-node ZFS reports of all zfs-agents and joins
// them against Mirror CRs for GET /api/usage. Zero configuration by design:
// wired up purely by the ZFS_AGENT_SERVICE environment variable.
type UsageAggregator struct {
	// service is the zfs-agent Service name (ZFS_AGENT_SERVICE), used for
	// discovery and error messages.
	service string
	agents  AgentAddresser
	// fetch retrieves one agent's report; injectable for tests. Production
	// GETs http://<addr>:<port>/v1/zfs.
	fetch func(ctx context.Context, ep AgentEndpoint) (*zfsagent.Report, error)
	port  int
	// timeout bounds a single agent request; ttl the aggregation cache.
	// Both default to the code constants, shortened in tests.
	timeout time.Duration
	ttl     time.Duration
	now     func() time.Time

	mu    sync.Mutex
	cache *usageAggregate
}

// NewUsageAggregator returns the production aggregator: agent endpoints are
// discovered through the EndpointSlices of <namespace>/<service> using the
// controller's clientset.
func NewUsageAggregator(clientset kubernetes.Interface, namespace, service string) *UsageAggregator {
	a := &UsageAggregator{
		service: service,
		port:    zfsAgentPort,
		timeout: zfsAgentFetchTimeout,
		ttl:     usageCacheTTL,
		now:     time.Now,
	}
	a.agents = &EndpointSliceAddresser{Client: clientset, Namespace: namespace, Service: service}
	a.fetch = a.fetchReport
	return a
}

// fetchReport is the production fetch: one GET against one agent.
func (a *UsageAggregator) fetchReport(ctx context.Context, ep AgentEndpoint) (*zfsagent.Report, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.agentURL(ep), nil)
	if err != nil {
		return nil, err
	}
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status %q", resp.Status)
	}
	report := &zfsagent.Report{}
	if err := json.NewDecoder(resp.Body).Decode(report); err != nil {
		return nil, fmt.Errorf("decode report: %w", err)
	}
	return report, nil
}

// agentURL builds the agent's report URL. EndpointSlice addresses are bare
// (pod) IPs, so the fixed agent port is appended (IPv6-safe); an address that
// already carries its own port is used as-is (tests against httptest
// servers).
func (a *UsageAggregator) agentURL(ep AgentEndpoint) string {
	host := ep.Addr
	if _, _, err := net.SplitHostPort(ep.Addr); err != nil {
		host = net.JoinHostPort(ep.Addr, strconv.Itoa(a.port))
	}
	return "http://" + host + "/v1/zfs"
}

// Usage returns the /api/usage payload: the (cached) aggregation of all
// agents joined against the Mirrors visible to reader. An error is returned
// only when the aggregation cannot be computed at all (EndpointSlice or
// Kubernetes object listing failed); per-agent failures degrade the result
// instead.
func (a *UsageAggregator) Usage(ctx context.Context, reader client.Reader) (*UsageResponse, error) {
	agg, err := a.aggregate(ctx)
	if err != nil {
		return nil, err
	}
	var mirrors mirrorv1alpha1.MirrorList
	if err := reader.List(ctx, &mirrors); err != nil {
		return nil, err
	}
	var claims corev1.PersistentVolumeClaimList
	if err := reader.List(ctx, &claims); err != nil {
		return nil, err
	}
	var snapshots snapshotv1.VolumeSnapshotList
	if err := reader.List(ctx, &snapshots); err != nil {
		return nil, err
	}
	return joinUsageWithStorageObjects(&mirrors, &claims, &snapshots, agg), nil
}

// aggregate returns the cached aggregation while fresh, recomputing it after
// usageCacheTTL. Degraded results (agent failures) are cached like successful
// ones: flapping agents must not turn /api/usage into a retry storm.
func (a *UsageAggregator) aggregate(ctx context.Context) (*usageAggregate, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.cache != nil && a.now().Sub(a.cache.fetchedAt) < a.ttl {
		return a.cache, nil
	}
	agg, err := a.collect(ctx)
	if err != nil {
		// Discovery failures are not cached: the next request retries.
		return nil, err
	}
	a.cache = agg
	return agg, nil
}

// collect fetches all agent reports concurrently and merges them.
func (a *UsageAggregator) collect(ctx context.Context) (*usageAggregate, error) {
	endpoints, err := a.agents.AgentEndpoints(ctx)
	if err != nil {
		return nil, err
	}
	agg := &usageAggregate{
		generatedAt:  a.now(),
		fetchedAt:    a.now(),
		complete:     true,
		errors:       []string{},
		byPVC:        map[string]*zfsagent.Dataset{},
		byVolumeName: map[string]*zfsagent.Dataset{},
	}
	if len(endpoints) == 0 {
		// No ready agent (DaemonSet not rolled out yet, no storage nodes
		// selected): vacuously all agents answered, but reporting complete
		// usage without any data would be misleading.
		agg.complete = false
		agg.errors = append(agg.errors, "no ready zfs-agent endpoints for service "+a.service)
		return agg, nil
	}
	// Defensive dedupe by address (EndpointSliceAddresser already dedupes,
	// but the addresser is pluggable): one fetch per unique address, in the
	// deterministic (address-sorted) order.
	seen := map[string]bool{}
	unique := make([]AgentEndpoint, 0, len(endpoints))
	for _, ep := range endpoints {
		if seen[ep.Addr] {
			continue
		}
		seen[ep.Addr] = true
		unique = append(unique, ep)
	}

	type agentResult struct {
		ep     AgentEndpoint
		report *zfsagent.Report
		err    error
	}
	results := make([]agentResult, len(unique))
	var wg sync.WaitGroup
	for i, ep := range unique {
		wg.Add(1)
		go func(i int, ep AgentEndpoint) {
			defer wg.Done()
			fetchCtx, cancel := context.WithTimeout(ctx, a.timeout)
			defer cancel()
			report, err := a.fetch(fetchCtx, ep)
			results[i] = agentResult{ep: ep, report: report, err: err}
		}(i, ep)
	}
	wg.Wait()

	for _, result := range results {
		if result.err != nil {
			agg.complete = false
			agg.errors = append(agg.errors, fmt.Sprintf("%s: %v", agentLabel(result.ep), result.err))
			continue
		}
		for _, pool := range result.report.Pools {
			for i := range pool.Datasets {
				ds := &pool.Datasets[i]
				volumeName := zfsDatasetLeaf(ds.Name)
				if volumeName != "" {
					if _, exists := agg.byVolumeName[volumeName]; !exists {
						agg.byVolumeName[volumeName] = ds
					}
				}
				if ds.PVC != nil {
					key := ds.PVC.Namespace + "/" + ds.PVC.Name
					// A dataset lives on exactly one node; should a key ever
					// collide (stale agent, duplicated address), the first
					// report wins deterministically (endpoints are sorted).
					if _, exists := agg.byPVC[key]; !exists {
						agg.byPVC[key] = ds
					}
				}
			}
		}
	}
	sort.Strings(agg.errors)
	return agg, nil
}

// zfsDatasetLeaf returns the CSI volume-name component of a ZFS dataset.
// OpenEBS lays volumes out as <pool>/<optional parent>/<volume name>.
func zfsDatasetLeaf(name string) string {
	if dataset, _, found := strings.Cut(name, "@"); found {
		name = dataset
	}
	if i := strings.LastIndexByte(name, '/'); i >= 0 {
		return name[i+1:]
	}
	return name
}

// zfsSnapshotLeaf returns the snapshot component after the final '@'.
func zfsSnapshotLeaf(name string) string {
	if i := strings.LastIndexByte(name, '@'); i >= 0 {
		return name[i+1:]
	}
	return ""
}

// agentLabel names a failing agent in the usage errors list: the Kubernetes
// node name when known, the bare address otherwise.
func agentLabel(ep AgentEndpoint) string {
	if ep.NodeName != "" {
		return ep.NodeName
	}
	return ep.Addr
}

// joinUsage maps every Mirror onto the aggregated agent data. Every Mirror is
// reported — including Mirrors that never synced (sync null, no snapshots).
func joinUsage(mirrors *mirrorv1alpha1.MirrorList, agg *usageAggregate) *UsageResponse {
	return joinUsageWithStorageObjects(mirrors, nil, nil, agg)
}

// joinUsageWithStorageObjects additionally supports OpenEBS <= 2.11, which
// exposes the CSI volume and snapshot names in the ZFS object names but does
// not attach Kubernetes object references as ZFS user properties.
func joinUsageWithStorageObjects(mirrors *mirrorv1alpha1.MirrorList, claims *corev1.PersistentVolumeClaimList, snapshots *snapshotv1.VolumeSnapshotList, agg *usageAggregate) *UsageResponse {
	volumeByPVC := map[string]string{}
	if claims != nil {
		for i := range claims.Items {
			claim := &claims.Items[i]
			if claim.Spec.VolumeName != "" {
				volumeByPVC[claim.Namespace+"/"+claim.Name] = claim.Spec.VolumeName
			}
		}
	}
	snapshotByStorageName := map[string]string{}
	if snapshots != nil {
		for i := range snapshots.Items {
			snapshot := &snapshots.Items[i]
			if snapshot.UID != "" {
				snapshotByStorageName["snapshot-"+string(snapshot.UID)] = snapshot.Name
			}
		}
	}

	resp := &UsageResponse{GeneratedAt: agg.generatedAt, Mirrors: make([]UsageMirror, 0, len(mirrors.Items))}
	for i := range mirrors.Items {
		mirror := &mirrors.Items[i]
		entry := UsageMirror{
			Name:      mirror.Name,
			Snapshots: []UsageSnapshot{},
			Complete:  agg.complete,
			Errors:    agg.errors,
		}

		pvcName := mirror.Status.WorkPVC
		if pvcName == "" {
			pvcName = deriveSyncPVCName(mirror.Name)
		}
		pvcKey := mirror.Namespace + "/" + pvcName
		ds := agg.byPVC[pvcKey]
		if ds == nil {
			ds = agg.byVolumeName[volumeByPVC[pvcKey]]
		}
		if ds != nil {
			entry.Sync = &UsageSync{PVC: pvcName, ReferencedBytes: ds.ReferencedBytes}
			for _, snap := range ds.Snapshots {
				// Prefer the VolumeSnapshot object name (userprop
				// openebs.io:vs-name) — it is what the rest of Falcon names
				// snapshots; the raw ZFS name is the fallback for manual
				// snapshots.
				name := snap.Name
				if snap.VolumeSnapshot != nil {
					name = snap.VolumeSnapshot.Name
				} else if objectName := snapshotByStorageName[zfsSnapshotLeaf(snap.Name)]; objectName != "" {
					name = objectName
				}
				entry.Snapshots = append(entry.Snapshots, UsageSnapshot{
					Name:         name,
					WrittenBytes: snap.WrittenBytes,
					CreatedAt:    snap.CreatedAt,
				})
			}
			// Ascending creation order regardless of the wire order (the
			// agent sorts too, but the join does not rely on it).
			sort.SliceStable(entry.Snapshots, func(i, j int) bool {
				if entry.Snapshots[i].CreatedAt != entry.Snapshots[j].CreatedAt {
					return entry.Snapshots[i].CreatedAt < entry.Snapshots[j].CreatedAt
				}
				return entry.Snapshots[i].Name < entry.Snapshots[j].Name
			})
		}

		entry.TotalBytes = 0
		if entry.Sync != nil {
			entry.TotalBytes += entry.Sync.ReferencedBytes
		}
		for _, snap := range entry.Snapshots {
			entry.TotalBytes += snap.WrittenBytes
		}
		resp.Mirrors = append(resp.Mirrors, entry)
	}
	sort.Slice(resp.Mirrors, func(i, j int) bool { return resp.Mirrors[i].Name < resp.Mirrors[j].Name })
	return resp
}

// deriveSyncPVCName reproduces the controller's childBase naming rules (see
// internal/controller/resources.go — unexported there, and the webapi must
// stay decoupled from the reconcilers): lowercase; letters, digits and '-'
// kept; '.' and '_' replaced by '-'; everything else dropped; leading/trailing
// '-' trimmed; empty result falls back to "mirror". The sync PVC suffix is
// fixed ("-sync"). A CR name that would overlong the child name fails
// validation in the controller and can never have data — length is not
// checked here on purpose.
func deriveSyncPVCName(crName string) string {
	var builder strings.Builder
	for _, r := range strings.ToLower(crName) {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-':
			builder.WriteRune(r)
		case r == '.' || r == '_':
			builder.WriteByte('-')
		}
	}
	base := strings.Trim(builder.String(), "-")
	if base == "" {
		base = "mirror"
	}
	return base + "-sync"
}

// handleUsage serves GET /api/usage. Like the mirrorz catalog, the endpoint
// is gated on wiring: with ZFS_AGENT_SERVICE unset no aggregator exists and
// the endpoint answers 404.
func (s *Server) handleUsage(w http.ResponseWriter, r *http.Request) {
	if s.Usage == nil {
		writeJSONError(w, http.StatusNotFound, "usage aggregation is disabled")
		return
	}
	usage, err := s.Usage.Usage(r.Context(), s.Client)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, "application/json", usage)
}
