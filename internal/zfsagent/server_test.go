package zfsagent

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func testCollector(t *testing.T) *Collector {
	t.Helper()
	return NewCollector("storage-1", []string{"tank"}, &fakeRunner{respond: func(string) ([]byte, error) {
		return []byte(cannedZfsGet), nil
	}})
}

// TestServeReport pins the frozen wire shape of GET /v1/zfs.
func TestServeReport(t *testing.T) {
	srv := httptest.NewServer((&Server{Collector: testCollector(t)}).Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v1/zfs")
	if err != nil {
		t.Fatalf("GET /v1/zfs: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("content type = %q, want application/json", ct)
	}

	// Key presence on the raw JSON level: the shape is frozen and shared with
	// the webapi aggregator.
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatalf("body is not a JSON object: %v (%s)", err, body)
	}
	for _, key := range []string{"node", "generatedAt", "pools"} {
		if _, ok := raw[key]; !ok {
			t.Errorf("report key %q missing", key)
		}
	}
	pool, ok := raw["pools"].([]any)[0].(map[string]any)
	if !ok {
		t.Fatalf("pools[0] is not an object: %s", body)
	}
	for _, key := range []string{"name", "datasets"} {
		if _, ok := pool[key]; !ok {
			t.Errorf("pools[0] key %q missing", key)
		}
	}
	ds, ok := pool["datasets"].([]any)[2].(map[string]any) // tank/pvc-0a1b (names sorted after tank, tank/foreign)
	if !ok {
		t.Fatalf("datasets[1] is not an object: %s", body)
	}
	for _, key := range []string{"name", "pvc", "usedBytes", "referencedBytes", "writtenBytes", "snapshots"} {
		if _, ok := ds[key]; !ok {
			t.Errorf("dataset key %q missing", key)
		}
	}
	if ds["name"] != "tank/pvc-0a1b" || ds["usedBytes"] != float64(137438953472) {
		t.Errorf("dataset mismatch: %v", ds)
	}
	snap, ok := ds["snapshots"].([]any)[0].(map[string]any)
	if !ok {
		t.Fatalf("snapshots[0] is not an object: %s", body)
	}
	for _, key := range []string{"name", "volumeSnapshot", "writtenBytes", "referencedBytes", "createdAt"} {
		if _, ok := snap[key]; !ok {
			t.Errorf("snapshot key %q missing", key)
		}
	}
	// Numeric fields must be integers on the wire (no unit suffixes, no
	// floats): createdAt is Unix seconds.
	if snap["createdAt"] != float64(1725086400) || snap["writtenBytes"] != float64(4294967296) {
		t.Errorf("snapshot numbers mismatch: %v", snap)
	}
}

func TestServeHealthz(t *testing.T) {
	srv := httptest.NewServer((&Server{Collector: testCollector(t)}).Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || strings.TrimSpace(string(body)) != "ok" {
		t.Errorf("healthz = %d %q, want 200 ok", resp.StatusCode, body)
	}
}

// TestServeMethodNotAllowed: the listener is strictly read-only, checked
// before routing — same contract as the webapi server.
func TestServeMethodNotAllowed(t *testing.T) {
	srv := httptest.NewServer((&Server{Collector: testCollector(t)}).Handler())
	defer srv.Close()

	for _, path := range []string{"/v1/zfs", "/healthz", "/anything"} {
		for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
			req, err := http.NewRequest(method, srv.URL+path, nil)
			if err != nil {
				t.Fatalf("new request: %v", err)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("%s %s: %v", method, path, err)
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusMethodNotAllowed {
				t.Errorf("%s %s: status = %d, want 405", method, path, resp.StatusCode)
			}
			if allow := resp.Header.Get("Allow"); allow != http.MethodGet {
				t.Errorf("%s %s: Allow = %q, want GET", method, path, allow)
			}
		}
	}
}

func TestServeUnknownPath(t *testing.T) {
	srv := httptest.NewServer((&Server{Collector: testCollector(t)}).Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/definitely-not-a-route")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

// TestServeCollectError: a collector that cannot run at all (pool enumeration
// fails) surfaces as a 500 JSON error.
func TestServeCollectError(t *testing.T) {
	collector := NewCollector("storage-1", nil, &fakeRunner{respond: func(string) ([]byte, error) {
		return nil, context.DeadlineExceeded
	}})
	srv := httptest.NewServer((&Server{Collector: collector}).Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v1/zfs")
	if err != nil {
		t.Fatalf("GET /v1/zfs: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (body: %s)", resp.StatusCode, body)
	}
	var errBody struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &errBody); err != nil || errBody.Error == "" {
		t.Errorf("body = %s, want {\"error\": ...} (err: %v)", body, err)
	}
}
