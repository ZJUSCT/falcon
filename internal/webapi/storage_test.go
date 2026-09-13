package webapi

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// storage1CapReport extends storage1Report with the pool capacity and health
// fields (as served by the agent since the capacity extension).
const storage1CapReport = `{
  "node": "storage-1", "generatedAt": "2026-08-31T12:00:00Z",
  "pools": [{"name": "tank",
    "sizeBytes": 1000, "allocatedBytes": 400, "freeBytes": 600,
    "capacityPercent": 40, "fragmentationPercent": 5, "health": "ONLINE",
    "datasets": [
      {"name": "tank/pvc-1", "pvc": {"namespace": "mirror", "name": "ubuntu-sync"},
       "usedBytes": 100, "logicalUsedBytes": 200, "referencedBytes": 90, "writtenBytes": 5,
       "snapshots": []}
    ]}]
}`

// TestStorageAggregation: GET /api/storage returns the per-node reports
// merged from all agents, sorted by node, sharing /api/usage's aggregation
// (one fetch, degraded completeness on agent failures).
func TestStorageAggregation(t *testing.T) {
	ep1, _ := fakeAgent(t, http.StatusOK, storage1CapReport, 0, "storage-1")
	ep2, _ := fakeAgent(t, http.StatusOK, storage2Report, 0, "storage-2")
	a := newTestAggregator(&fakeAgents{endpoints: []AgentEndpoint{ep1, ep2}})

	resp, err := a.Storage(t.Context())
	if err != nil {
		t.Fatalf("Storage: %v", err)
	}
	if !resp.Complete || len(resp.Errors) != 0 {
		t.Errorf("complete = %v, errors = %v, want clean aggregation", resp.Complete, resp.Errors)
	}
	if len(resp.Nodes) != 2 {
		t.Fatalf("nodes = %+v, want storage-1 and storage-2", resp.Nodes)
	}
	// Sorted by node name regardless of endpoint order.
	if resp.Nodes[0].Node != "storage-1" || resp.Nodes[1].Node != "storage-2" {
		t.Errorf("node order = %s, %s", resp.Nodes[0].Node, resp.Nodes[1].Node)
	}
	tank := resp.Nodes[0].Pools[0]
	if tank.Name != "tank" || tank.SizeBytes != 1000 || tank.AllocatedBytes != 400 ||
		tank.FreeBytes != 600 || tank.CapacityPercent != 40 ||
		tank.FragmentationPercent != 5 || tank.Health != "ONLINE" {
		t.Errorf("tank = %+v", tank)
	}
	if len(tank.Datasets) != 1 || tank.Datasets[0].LogicalUsedBytes != 200 {
		t.Errorf("tank datasets = %+v", tank.Datasets)
	}
	// storage-2's report has no capacity fields: zeros mean unknown.
	if s2 := resp.Nodes[1].Pools[0]; s2.SizeBytes != 0 || s2.Health != "" {
		t.Errorf("storage-2 tank = %+v, want unknown capacity", s2)
	}
}

// TestStorageDegraded: a failing agent flips complete and is absent from the
// node list; the healthy nodes are still served.
func TestStorageDegraded(t *testing.T) {
	ep1, _ := fakeAgent(t, http.StatusOK, storage1CapReport, 0, "storage-1")
	ep2, _ := fakeAgent(t, http.StatusInternalServerError, `{"error":"boom"}`, 0, "storage-2")
	a := newTestAggregator(&fakeAgents{endpoints: []AgentEndpoint{ep1, ep2}})

	resp, err := a.Storage(t.Context())
	if err != nil {
		t.Fatalf("Storage: %v", err)
	}
	if resp.Complete || len(resp.Errors) != 1 {
		t.Fatalf("complete = %v, errors = %v, want degraded", resp.Complete, resp.Errors)
	}
	if len(resp.Nodes) != 1 || resp.Nodes[0].Node != "storage-1" {
		t.Errorf("nodes = %+v, want only storage-1", resp.Nodes)
	}
}

// TestStorageDisabledIs404: like /api/usage, the endpoint is wired only when
// ZFS_AGENT_SERVICE is set.
func TestStorageDisabledIs404(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	srv := httptest.NewServer((&Server{Client: c}).AdminHandler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/storage")
	if err != nil {
		t.Fatalf("GET /api/storage: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body: %s)", resp.StatusCode, body)
	}
	var got struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &got); err != nil || got.Error != "storage aggregation is disabled" {
		t.Errorf("body = %s, want {\"error\": \"storage aggregation is disabled\"} (err: %v)", body, err)
	}
}

// TestStoragePinsWireShape: the frozen /api/storage response shape.
func TestStoragePinsWireShape(t *testing.T) {
	ep, _ := fakeAgent(t, http.StatusOK, storage1CapReport, 0, "storage-1")
	a := newTestAggregator(&fakeAgents{endpoints: []AgentEndpoint{ep}})
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	srv := httptest.NewServer((&Server{Client: c, Usage: a}).AdminHandler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/storage")
	if err != nil {
		t.Fatalf("GET /api/storage: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", resp.StatusCode, body)
	}

	var raw struct {
		GeneratedAt string   `json:"generatedAt"`
		Complete    bool     `json:"complete"`
		Errors      []string `json:"errors"`
		Nodes       []struct {
			Node  string `json:"node"`
			Pools []struct {
				Name     string `json:"name"`
				Datasets []struct {
					Name string `json:"name"`
				} `json:"datasets"`
			} `json:"pools"`
		} `json:"nodes"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatalf("body is not the storage document: %v (%s)", err, body)
	}
	if raw.GeneratedAt == "" || !raw.Complete || raw.Errors == nil {
		t.Errorf("header = %+v", raw)
	}
	if len(raw.Nodes) != 1 || raw.Nodes[0].Node != "storage-1" {
		t.Fatalf("nodes = %s", body)
	}
	if len(raw.Nodes[0].Pools) != 1 || raw.Nodes[0].Pools[0].Name != "tank" {
		t.Errorf("pools = %s", body)
	}
	if len(raw.Nodes[0].Pools[0].Datasets) != 1 {
		t.Errorf("datasets = %s", body)
	}
}
