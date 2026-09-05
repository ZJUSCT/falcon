package webapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	snapshotv1 "github.com/kubernetes-csi/external-snapshotter/client/v8/apis/volumesnapshot/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	mirrorv1alpha1 "github.com/ZJUSCT/falcon/api/v1alpha1"
	"github.com/ZJUSCT/falcon/internal/zfsagent"
)

// fakeAgents is an AgentAddresser over a canned endpoint list.
type fakeAgents struct {
	endpoints []AgentEndpoint
	err       error
}

func (f *fakeAgents) AgentEndpoints(context.Context) ([]AgentEndpoint, error) {
	return f.endpoints, f.err
}

// newTestAggregator returns an aggregator over the given addresser with test
// timings (production fetch, so fake agents are exercised over real HTTP).
func newTestAggregator(agents AgentAddresser) *UsageAggregator {
	a := &UsageAggregator{
		service: "falcon-zfs-agent",
		agents:  agents,
		port:    zfsAgentPort,
		timeout: zfsAgentFetchTimeout,
		ttl:     usageCacheTTL,
		now:     time.Now,
	}
	a.fetch = a.fetchReport
	return a
}

// fakeAgent runs an httptest zfs-agent answering /v1/zfs with status/payload
// (after delay). The returned endpoint carries its own host:port so the
// aggregator dials the test server directly; requests are counted.
func fakeAgent(t *testing.T, status int, payload string, delay time.Duration, node string) (AgentEndpoint, *atomic.Int64) {
	t.Helper()
	var requests atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		if delay > 0 {
			time.Sleep(delay)
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, payload)
	}))
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	// u.Host already carries the port, so agentURL uses the address as-is.
	return AgentEndpoint{Addr: u.Host, NodeName: node}, &requests
}

const (
	// storage-1 report: ubuntu's sync PVC with two snapshots (created by
	// Falcon and a manual one), plus a foreign dataset without a PVC ref.
	storage1Report = `{
	  "node": "storage-1", "generatedAt": "2026-08-31T12:00:00Z",
	  "pools": [{"name": "tank", "datasets": [
	    {"name": "tank/pvc-1", "pvc": {"namespace": "mirror", "name": "ubuntu-sync"},
	     "usedBytes": 100, "referencedBytes": 90, "writtenBytes": 5,
	     "snapshots": [
	       {"name": "tank/pvc-1@s1", "volumeSnapshot": {"namespace": "mirror", "name": "ubuntu-snap-1000"},
	        "writtenBytes": 5, "referencedBytes": 90, "createdAt": 1000},
	       {"name": "tank/pvc-1@s0", "volumeSnapshot": {"namespace": "mirror", "name": "ubuntu-snap-500"},
	        "writtenBytes": 2, "referencedBytes": 88, "createdAt": 500}
	     ]},
	    {"name": "tank/foreign", "usedBytes": 7, "referencedBytes": 7, "writtenBytes": 0, "snapshots": []}
	  ]}]
	}`
	// storage-2 report: debian's sync PVC.
	storage2Report = `{
	  "node": "storage-2", "generatedAt": "2026-08-31T11:59:30Z",
	  "pools": [{"name": "tank", "datasets": [
	    {"name": "tank/pvc-2", "pvc": {"namespace": "mirror", "name": "debian-sync"},
	     "usedBytes": 30, "referencedBytes": 20, "writtenBytes": 0, "snapshots": []}
	  ]}]
	}`
)

func TestUsageAggregatorMergesAgentReports(t *testing.T) {
	ep1, reqs1 := fakeAgent(t, http.StatusOK, storage1Report, 0, "storage-1")
	ep2, reqs2 := fakeAgent(t, http.StatusOK, storage2Report, 0, "storage-2")
	a := newTestAggregator(&fakeAgents{endpoints: []AgentEndpoint{ep1, ep2}})

	agg, err := a.aggregate(t.Context())
	if err != nil {
		t.Fatalf("aggregate: %v", err)
	}
	if !agg.complete || len(agg.errors) != 0 {
		t.Errorf("complete = %v, errors = %v, want clean aggregation", agg.complete, agg.errors)
	}
	if len(agg.byPVC) != 2 {
		t.Fatalf("byPVC = %v, want ubuntu-sync and debian-sync", keysOfPVC(agg.byPVC))
	}
	ubuntu := agg.byPVC["mirror/ubuntu-sync"]
	if ubuntu == nil || ubuntu.ReferencedBytes != 90 {
		t.Errorf("ubuntu-sync dataset = %+v", ubuntu)
	}
	if len(ubuntu.Snapshots) != 2 {
		t.Errorf("ubuntu-sync snapshots = %d, want 2", len(ubuntu.Snapshots))
	}
	if agg.byPVC["mirror/debian-sync"] == nil {
		t.Error("debian-sync missing from merged data")
	}
	// generatedAt is the aggregation time (node clock skews do not leak in).
	if time.Since(agg.generatedAt) > time.Minute {
		t.Errorf("generatedAt = %v, want the aggregation time", agg.generatedAt)
	}
	if reqs1.Load() != 1 || reqs2.Load() != 1 {
		t.Errorf("request counts = %d/%d, want 1/1", reqs1.Load(), reqs2.Load())
	}
}

// TestUsageAggregatorDeduplicatesAddresses: overlapping EndpointSlices may
// list the same address twice; each agent must be fetched exactly once.
func TestUsageAggregatorDeduplicatesAddresses(t *testing.T) {
	ep, reqs := fakeAgent(t, http.StatusOK, storage1Report, 0, "storage-1")
	a := newTestAggregator(&fakeAgents{endpoints: []AgentEndpoint{ep, ep}})
	if _, err := a.aggregate(t.Context()); err != nil {
		t.Fatalf("aggregate: %v", err)
	}
	if reqs.Load() != 1 {
		t.Errorf("agent fetched %d times, want 1 (addresses deduplicated)", reqs.Load())
	}
}

// TestUsageAggregatorDegradesOnAgentFailure: one broken agent must not block
// the others; its error is labeled with the node name and flips complete.
func TestUsageAggregatorDegradesOnAgentFailure(t *testing.T) {
	ep1, _ := fakeAgent(t, http.StatusOK, storage1Report, 0, "storage-1")
	ep2, _ := fakeAgent(t, http.StatusInternalServerError, `{"error":"boom"}`, 0, "storage-2")
	ep3, _ := fakeAgent(t, http.StatusOK, "not json", 0, "storage-3")
	a := newTestAggregator(&fakeAgents{endpoints: []AgentEndpoint{ep1, ep2, ep3}})

	agg, err := a.aggregate(t.Context())
	if err != nil {
		t.Fatalf("aggregate: %v", err)
	}
	if agg.complete {
		t.Error("complete = true, want false with failing agents")
	}
	if len(agg.errors) != 2 {
		t.Fatalf("errors = %v, want 2", agg.errors)
	}
	if !strings.HasPrefix(agg.errors[0], "storage-2: ") || !strings.Contains(agg.errors[0], "500") {
		t.Errorf("error[0] = %q, want \"storage-2: ... 500 ...\"", agg.errors[0])
	}
	if !strings.HasPrefix(agg.errors[1], "storage-3: ") {
		t.Errorf("error[1] = %q, want \"storage-3: ...\"", agg.errors[1])
	}
	// The healthy agent's data is still merged.
	if agg.byPVC["mirror/ubuntu-sync"] == nil {
		t.Error("healthy agent data must survive degraded aggregation")
	}
}

// TestUsageAggregatorTimeout: a hung agent hits the per-request timeout; the
// error is labeled with the node name; the other agent is unaffected.
func TestUsageAggregatorTimeout(t *testing.T) {
	ep1, _ := fakeAgent(t, http.StatusOK, storage1Report, 0, "storage-1")
	hung, _ := fakeAgent(t, http.StatusOK, storage2Report, 500*time.Millisecond, "storage-2")
	a := newTestAggregator(&fakeAgents{endpoints: []AgentEndpoint{ep1, hung}})
	a.timeout = 50 * time.Millisecond

	agg, err := a.aggregate(t.Context())
	if err != nil {
		t.Fatalf("aggregate: %v", err)
	}
	if agg.complete || len(agg.errors) != 1 {
		t.Fatalf("complete = %v, errors = %v", agg.complete, agg.errors)
	}
	if !strings.HasPrefix(agg.errors[0], "storage-2: ") {
		t.Errorf("error = %q, want storage-2-prefixed timeout", agg.errors[0])
	}
	if agg.byPVC["mirror/ubuntu-sync"] == nil {
		t.Error("healthy agent data must survive the hung agent")
	}
}

// TestUsageAggregatorAddressFallbackLabel: an endpoint without a node name is
// labeled by its address.
func TestUsageAggregatorAddressFallbackLabel(t *testing.T) {
	ep, _ := fakeAgent(t, http.StatusForbidden, "denied", 0, "")
	a := newTestAggregator(&fakeAgents{endpoints: []AgentEndpoint{ep}})
	agg, err := a.aggregate(t.Context())
	if err != nil {
		t.Fatalf("aggregate: %v", err)
	}
	if len(agg.errors) != 1 || !strings.HasPrefix(agg.errors[0], ep.Addr+": ") {
		t.Errorf("errors = %v, want address-labeled error", agg.errors)
	}
}

// TestUsageAggregatorCacheTTL: the aggregation is cached for the TTL; failed
// agents are cached too (no retry storm) and discovery failures are not.
func TestUsageAggregatorCacheTTL(t *testing.T) {
	ep, reqs := fakeAgent(t, http.StatusOK, storage1Report, 0, "storage-1")
	a := newTestAggregator(&fakeAgents{endpoints: []AgentEndpoint{ep}})
	now := time.Unix(1756630000, 0)
	a.now = func() time.Time { return now }
	a.ttl = 30 * time.Second

	reader := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()

	if _, err := a.Usage(t.Context(), reader); err != nil {
		t.Fatalf("Usage: %v", err)
	}
	if _, err := a.Usage(t.Context(), reader); err != nil {
		t.Fatalf("Usage (cached): %v", err)
	}
	if reqs.Load() != 1 {
		t.Fatalf("agent fetched %d times, want 1 (cache hit)", reqs.Load())
	}
	// TTL expiry recomputes.
	now = now.Add(31 * time.Second)
	if _, err := a.Usage(t.Context(), reader); err != nil {
		t.Fatalf("Usage after TTL: %v", err)
	}
	if reqs.Load() != 2 {
		t.Errorf("agent fetched %d times, want 2 after TTL expiry", reqs.Load())
	}
}

// TestUsageAggregatorDiscoveryErrorNotCached: an EndpointSlice listing
// failure returns an error and is retried on the next request.
func TestUsageAggregatorDiscoveryErrorNotCached(t *testing.T) {
	ep, reqs := fakeAgent(t, http.StatusOK, storage1Report, 0, "storage-1")
	agents := &fakeAgents{err: errors.New("endpointslices is forbidden")}
	a := newTestAggregator(agents)

	reader := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	if _, err := a.Usage(t.Context(), reader); err == nil {
		t.Fatal("expected discovery failure to surface")
	}
	agents.err = nil
	agents.endpoints = []AgentEndpoint{ep}
	if _, err := a.Usage(t.Context(), reader); err != nil {
		t.Fatalf("Usage after recovery: %v", err)
	}
	if reqs.Load() != 1 {
		t.Errorf("agent fetched %d times, want 1 (discovery errors are not cached)", reqs.Load())
	}
}

func TestUsageAggregatorNoReadyEndpoints(t *testing.T) {
	a := newTestAggregator(&fakeAgents{})
	agg, err := a.aggregate(t.Context())
	if err != nil {
		t.Fatalf("aggregate: %v", err)
	}
	if agg.complete {
		t.Error("complete = true with zero endpoints, want false")
	}
	if len(agg.errors) != 1 || !strings.Contains(agg.errors[0], "no ready zfs-agent endpoints") {
		t.Errorf("errors = %v, want the no-endpoint notice", agg.errors)
	}
}

// TestJoinUsage pins the Mirror join: workPVC matching, derived PVC names,
// the totalBytes formula, snapshot ordering/naming, and per-mirror semantics
// for Mirrors without agent data.
func TestJoinUsage(t *testing.T) {
	agg := &usageAggregate{
		generatedAt: time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC),
		complete:    false,
		errors:      []string{"storage-2: Get \"http://10.0.0.2:9474/v1/zfs\": context deadline exceeded"},
		byPVC: map[string]*zfsagent.Dataset{
			"mirror/ubuntu-sync": {
				Name:      "tank/pvc-1",
				UsedBytes: 120, ReferencedBytes: 90, WrittenBytes: 7,
				Snapshots: []zfsagent.Snapshot{
					{Name: "tank/pvc-1@s2", VolumeSnapshot: &zfsagent.ObjectRef{Namespace: "mirror", Name: "ubuntu-snap-2000"}, WrittenBytes: 30, ReferencedBytes: 60, CreatedAt: 2000},
					{Name: "tank/pvc-1@s1", VolumeSnapshot: &zfsagent.ObjectRef{Namespace: "mirror", Name: "ubuntu-snap-1000"}, WrittenBytes: 5, ReferencedBytes: 90, CreatedAt: 1000},
					// Manual snapshot without a vs-name: falls back to the
					// ZFS name.
					{Name: "tank/pvc-1@manual", WrittenBytes: 1, ReferencedBytes: 87, CreatedAt: 1500},
				},
			},
			"mirror/debian-sync": {Name: "tank/pvc-2", UsedBytes: 20, ReferencedBytes: 20},
			// Other namespace: must not match any mirror below.
			"other/ubuntu-sync": {Name: "zpool2/pvc-9", ReferencedBytes: 999},
		},
	}
	mirrors := &mirrorv1alpha1.MirrorList{Items: []mirrorv1alpha1.Mirror{
		{ObjectMeta: metav1.ObjectMeta{Name: "zebra", Namespace: "mirror"}}, // never synced: derived name matches nothing
		{ObjectMeta: metav1.ObjectMeta{Name: "ubuntu", Namespace: "mirror"},
			Status: mirrorv1alpha1.MirrorStatus{WorkPVC: "ubuntu-sync", ActiveSnapshot: "ubuntu-snap-2000"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "debian", Namespace: "mirror"},
			Status: mirrorv1alpha1.MirrorStatus{WorkPVC: "debian-sync"}},
		// ChildBase derivation: dots become dashes.
		{ObjectMeta: metav1.ObjectMeta{Name: "rocky.linux", Namespace: "mirror"},
			Status: mirrorv1alpha1.MirrorStatus{WorkPVC: "rockylinux-sync"}},
	}}
	// rocky's derived sync PVC has agent data (as if synced under the
	// derived name before status.workPVC was ever written).
	agg.byPVC["mirror/rockylinux-sync"] = &zfsagent.Dataset{Name: "tank/pvc-3", UsedBytes: 10, ReferencedBytes: 10}

	resp := joinUsage(mirrors, agg)

	if !resp.GeneratedAt.Equal(agg.generatedAt) {
		t.Errorf("generatedAt = %v, want %v", resp.GeneratedAt, agg.generatedAt)
	}
	if len(resp.Mirrors) != 4 {
		t.Fatalf("got %d mirrors, want 4 (every Mirror is reported)", len(resp.Mirrors))
	}
	// Mirrors sorted by name.
	wantOrder := []string{"debian", "rocky.linux", "ubuntu", "zebra"}
	for i, want := range wantOrder {
		if resp.Mirrors[i].Name != want {
			t.Errorf("mirrors[%d] = %s, want %s", i, resp.Mirrors[i].Name, want)
		}
	}

	ubuntu := resp.Mirrors[2]
	if ubuntu.ActiveSnapshot != "ubuntu-snap-2000" {
		t.Errorf("activeSnapshot = %q", ubuntu.ActiveSnapshot)
	}
	if ubuntu.Sync == nil || ubuntu.Sync.PVC != "ubuntu-sync" || ubuntu.Sync.ReferencedBytes != 90 {
		t.Errorf("ubuntu sync = %+v", ubuntu.Sync)
	}
	// Snapshots newest-first; oldest row carries the referenced baseline.
	wantSnaps := []UsageSnapshot{
		{Name: "ubuntu-snap-2000", WrittenBytes: 30, ReferencedBytes: 60, CreatedAt: 2000},
		{Name: "tank/pvc-1@manual", WrittenBytes: 1, ReferencedBytes: 87, CreatedAt: 1500},
		{Name: "ubuntu-snap-1000", WrittenBytes: 5, ReferencedBytes: 90, CreatedAt: 1000},
	}
	if diff := cmp.Diff(wantSnaps, ubuntu.Snapshots); diff != "" {
		t.Errorf("ubuntu snapshots (-want +got):\n%s", diff)
	}
	if ubuntu.Sync.WrittenBytes != 7 {
		t.Errorf("ubuntu sync writtenBytes = %d, want 7", ubuntu.Sync.WrittenBytes)
	}
	if ubuntu.TotalBytes != 120 {
		t.Errorf("ubuntu totalBytes = %d, want 120", ubuntu.TotalBytes)
	}

	debian := resp.Mirrors[0]
	if debian.Sync == nil || debian.Sync.ReferencedBytes != 20 || debian.TotalBytes != 20 {
		t.Errorf("debian = sync %+v total %d", debian.Sync, debian.TotalBytes)
	}

	rocky := resp.Mirrors[1]
	if rocky.Sync == nil || rocky.Sync.PVC != "rockylinux-sync" {
		t.Errorf("rocky sync = %+v, want derived-name match", rocky.Sync)
	}
	if rocky.TotalBytes != 10 {
		t.Errorf("rocky totalBytes = %d, want 10", rocky.TotalBytes)
	}

	zebra := resp.Mirrors[3]
	if zebra.Sync != nil || len(zebra.Snapshots) != 0 || zebra.TotalBytes != 0 {
		t.Errorf("zebra must have no data, got %+v", zebra)
	}
	// Degraded aggregation semantics propagate to every Mirror.
	for _, m := range resp.Mirrors {
		if m.Complete {
			t.Errorf("mirror %s: complete = true, want false (global)", m.Name)
		}
		if len(m.Errors) != 1 || !strings.HasPrefix(m.Errors[0], "storage-2: ") {
			t.Errorf("mirror %s: errors = %v", m.Name, m.Errors)
		}
	}
}

// TestJoinUsageFallsBackToOpenEBSStorageNames covers OpenEBS ZFS LocalPV
// <= 2.11. Those releases do not write the openebs.io:pvc-* and
// openebs.io:vs-* ZFS user properties, but the dataset and snapshot names
// still contain the CSI external-provisioner object identifiers.
func TestJoinUsageFallsBackToOpenEBSStorageNames(t *testing.T) {
	mirrors := &mirrorv1alpha1.MirrorList{Items: []mirrorv1alpha1.Mirror{{
		ObjectMeta: metav1.ObjectMeta{Name: "debian", Namespace: "mirror"},
		Status:     mirrorv1alpha1.MirrorStatus{WorkPVC: "debian-sync"},
	}}}
	claims := &corev1.PersistentVolumeClaimList{Items: []corev1.PersistentVolumeClaim{{
		ObjectMeta: metav1.ObjectMeta{Name: "debian-sync", Namespace: "mirror"},
		Spec:       corev1.PersistentVolumeClaimSpec{VolumeName: "pvc-abc"},
	}}}
	snapshots := &snapshotv1.VolumeSnapshotList{Items: []snapshotv1.VolumeSnapshot{{
		ObjectMeta: metav1.ObjectMeta{Name: "debian-snap-1000", Namespace: "mirror", UID: types.UID("snap-uid")},
	}}}
	agg := &usageAggregate{
		complete: true,
		byPVC:    map[string]*zfsagent.Dataset{},
		byVolumeName: map[string]*zfsagent.Dataset{
			"pvc-abc": {
				Name:            "tank/openebs/pvc-abc",
				UsedBytes:       95,
				ReferencedBytes: 90,
				Snapshots: []zfsagent.Snapshot{{
					Name:         "tank/openebs/pvc-abc@snapshot-snap-uid",
					WrittenBytes: 5,
					CreatedAt:    1000,
				}},
			},
		},
	}

	resp := joinUsageWithStorageObjects(mirrors, claims, snapshots, agg)
	if len(resp.Mirrors) != 1 {
		t.Fatalf("mirrors = %d, want 1", len(resp.Mirrors))
	}
	got := resp.Mirrors[0]
	if got.Sync == nil || got.Sync.PVC != "debian-sync" || got.Sync.ReferencedBytes != 90 {
		t.Errorf("sync = %+v", got.Sync)
	}
	if diff := cmp.Diff([]UsageSnapshot{{Name: "debian-snap-1000", WrittenBytes: 5, CreatedAt: 1000}}, got.Snapshots); diff != "" {
		t.Errorf("snapshots (-want +got):\n%s", diff)
	}
	if got.TotalBytes != 95 {
		t.Errorf("totalBytes = %d, want 95", got.TotalBytes)
	}
}

// TestUsageDisabledIs404: without ZFS_AGENT_SERVICE the endpoint is a 404
// (and still GET-only).
func TestUsageDisabledIs404(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	srv := httptest.NewServer((&Server{Client: c}).Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/usage")
	if err != nil {
		t.Fatalf("GET /api/usage: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body: %s)", resp.StatusCode, body)
	}
	var got struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &got); err != nil || got.Error != "usage aggregation is disabled" {
		t.Errorf("body = %s, want {\"error\": \"usage aggregation is disabled\"} (err: %v)", body, err)
	}

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/usage", nil)
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /api/usage: %v", err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST status = %d, want 405", resp2.StatusCode)
	}
}

// TestUsageEnabledPinsWireShape: the frozen /api/usage response shape the
// frontend is developed against.
func TestUsageEnabledPinsWireShape(t *testing.T) {
	ep, _ := fakeAgent(t, http.StatusOK, storage1Report, 0, "storage-1")
	a := newTestAggregator(&fakeAgents{endpoints: []AgentEndpoint{ep}})

	mirror := &mirrorv1alpha1.Mirror{
		ObjectMeta: metav1.ObjectMeta{Name: "ubuntu", Namespace: "mirror"},
		Status:     mirrorv1alpha1.MirrorStatus{WorkPVC: "ubuntu-sync"},
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(mirror).Build()
	srv := httptest.NewServer((&Server{Client: c, Usage: a}).Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/usage")
	if err != nil {
		t.Fatalf("GET /api/usage: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("content type = %q, want application/json", ct)
	}

	var raw struct {
		GeneratedAt string           `json:"generatedAt"`
		Mirrors     []map[string]any `json:"mirrors"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatalf("body is not the usage document: %v (%s)", err, body)
	}
	if raw.GeneratedAt == "" || len(raw.Mirrors) != 1 {
		t.Fatalf("document = %s", body)
	}
	entry := raw.Mirrors[0]
	for _, key := range []string{"name", "sync", "snapshots", "totalBytes", "complete", "errors"} {
		if _, ok := entry[key]; !ok {
			t.Errorf("mirror key %q missing from %s", key, body)
		}
	}
	sync := entry["sync"].(map[string]any)
	for _, key := range []string{"pvc", "referencedBytes", "writtenBytes"} {
		if _, ok := sync[key]; !ok {
			t.Errorf("sync key %q missing from %s", key, body)
		}
	}
	if sync["pvc"] != "ubuntu-sync" || sync["referencedBytes"] != float64(90) {
		t.Errorf("sync = %v", sync)
	}
	if entry["totalBytes"] != float64(100) {
		t.Errorf("totalBytes = %v, want 100", entry["totalBytes"])
	}
	if entry["complete"] != true {
		t.Errorf("complete = %v, want true", entry["complete"])
	}
}

func keysOfPVC(m map[string]*zfsagent.Dataset) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
