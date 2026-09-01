package webapi

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	mirrorv1alpha1 "github.com/ZJUSCT/falcon/api/v1alpha1"
	snapshotv1 "github.com/kubernetes-csi/external-snapshotter/client/v8/apis/volumesnapshot/v1"
)

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	sch := runtime.NewScheme()
	if err := mirrorv1alpha1.AddToScheme(sch); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}
	if err := corev1.AddToScheme(sch); err != nil {
		t.Fatalf("corev1.AddToScheme: %v", err)
	}
	if err := snapshotv1.AddToScheme(sch); err != nil {
		t.Fatalf("snapshotv1.AddToScheme: %v", err)
	}
	return sch
}

func mirrorPhase(t *testing.T, start *time.Time, finish *time.Time, phase string) *mirrorv1alpha1.MirrorSyncStatus {
	t.Helper()
	var started, finished *metav1.Time
	if start != nil {
		started = &metav1.Time{Time: *start}
	}
	if finish != nil {
		finished = &metav1.Time{Time: *finish}
	}
	return &mirrorv1alpha1.MirrorSyncStatus{JobName: "j", Phase: phase, StartedAt: started, FinishedAt: finished}
}

func TestLegacyStatusForMirrorPhase(t *testing.T) {
	cases := map[string]string{
		mirrorv1alpha1.PhasePending:      "Waiting",
		mirrorv1alpha1.PhaseInitializing: "Running",
		mirrorv1alpha1.PhaseSyncing:      "Running",
		mirrorv1alpha1.PhasePublishing:   "Running",
		mirrorv1alpha1.PhaseReady:        "Waiting",
		mirrorv1alpha1.PhasePaused:       "Paused",
		mirrorv1alpha1.PhaseDegraded:     "Waiting",
		"":                               "Waiting",
	}
	for phase, want := range cases {
		if got := legacyStatusForMirrorPhase(phase); got != want {
			t.Errorf("legacyStatusForMirrorPhase(%q) = %q, want %q", phase, got, want)
		}
	}
}

func TestListJobsMirrorEntry(t *testing.T) {
	started := time.Date(2026, 8, 30, 8, 0, 0, 0, time.UTC)
	finished := started.Add(30 * time.Minute)

	m := &mirrorv1alpha1.Mirror{
		ObjectMeta: metav1.ObjectMeta{Name: "debian", Namespace: "mirrors"},
		Status: mirrorv1alpha1.MirrorStatus{
			Phase:      mirrorv1alpha1.PhaseReady,
			ActivePVC:  "debian-sync-1756521600",
			NextSyncAt: &metav1.Time{Time: finished.Add(6 * time.Hour)},
			LastSync:   mirrorPhase(t, &started, &finished, mirrorv1alpha1.SyncPhaseSucceeded),
		},
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(m).Build()
	s := &Server{Client: c}

	entries, err := s.listJobs(t.Context())
	if err != nil {
		t.Fatalf("listJobs: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(entries))
	}
	got := entries[0]
	want := JobEntry{
		ID:               "debian",
		Status:           "Waiting", // Ready = idle until next interval
		Kind:             "Mirror",
		Namespace:        "mirrors",
		Phase:            "Ready",
		ActivePVC:        "debian-sync-1756521600",
		Actions:          []string{},
		UpdatedAt:        finished,
		LastSuccessAt:    finished,
		LastAttemptAt:    started,
		LastFinishedAt:   finished,
		NextAttemptAt:    finished.Add(6 * time.Hour),
		LastActionStatus: "Succeeded",
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("job entry mismatch (-want +got):\n%s", diff)
	}
}

func TestListJobsFailedSyncMapsLastFailure(t *testing.T) {
	started := time.Date(2026, 8, 30, 9, 0, 0, 0, time.UTC)
	finished := started.Add(5 * time.Minute)
	m := &mirrorv1alpha1.Mirror{
		ObjectMeta: metav1.ObjectMeta{Name: "arch", Namespace: "mirrors"},
		Status: mirrorv1alpha1.MirrorStatus{
			Phase:    mirrorv1alpha1.PhaseDegraded,
			LastSync: mirrorPhase(t, &started, &finished, mirrorv1alpha1.SyncPhaseFailed),
		},
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(m).Build()
	entries, err := (&Server{Client: c}).listJobs(t.Context())
	if err != nil {
		t.Fatalf("listJobs: %v", err)
	}
	got := entries[0]
	if got.Status != "Waiting" {
		t.Errorf("Degraded -> status %q, want Waiting (legacy has no Failed state)", got.Status)
	}
	if !got.LastFailureAt.Equal(finished) {
		t.Errorf("last_failure_at = %v, want %v", got.LastFailureAt, finished)
	}
	if !got.LastSuccessAt.IsZero() {
		t.Errorf("last_success_at = %v, want zero", got.LastSuccessAt)
	}
	if got.LastActionStatus != "Failed" {
		t.Errorf("last_action_status = %q, want Failed", got.LastActionStatus)
	}
}

func TestListJobsUnsyncedMirrorHasZeroTimestamps(t *testing.T) {
	m := &mirrorv1alpha1.Mirror{
		ObjectMeta: metav1.ObjectMeta{Name: "fresh", Namespace: "mirrors"},
		Status:     mirrorv1alpha1.MirrorStatus{Phase: mirrorv1alpha1.PhasePending},
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(m).Build()
	entries, err := (&Server{Client: c}).listJobs(t.Context())
	if err != nil {
		t.Fatalf("listJobs: %v", err)
	}
	got := entries[0]
	for name, ts := range map[string]time.Time{
		"updated_at":      got.UpdatedAt,
		"last_success_at": got.LastSuccessAt,
		"last_failure_at": got.LastFailureAt,
		"last_attempt_at": got.LastAttemptAt,
		"next_attempt_at": got.NextAttemptAt,
	} {
		if !ts.IsZero() {
			t.Errorf("%s = %v, want zero time (never synced)", name, ts)
		}
	}
}

func TestListJobsIncludesProxyMirror(t *testing.T) {
	p := &mirrorv1alpha1.ProxyMirror{
		ObjectMeta: metav1.ObjectMeta{Name: "pypi-proxy", Namespace: "mirrors"},
		Status:     mirrorv1alpha1.ProxyMirrorStatus{Phase: mirrorv1alpha1.PhaseReady},
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(p).Build()
	entries, err := (&Server{Client: c}).listJobs(t.Context())
	if err != nil {
		t.Fatalf("listJobs: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(entries))
	}
	got := entries[0]
	if got.Kind != "ProxyMirror" || got.ID != "pypi-proxy" || got.Status != "Ready" {
		t.Errorf("unexpected proxy entry: %+v", got)
	}
	if !got.NextAttemptAt.IsZero() || !got.LastAttemptAt.IsZero() {
		t.Errorf("proxy entries must carry zero timestamps, got %+v", got)
	}
}

// TestHandleJobsLegacyFieldNames pins the wire field names to the legacy
// (pre-Kubernetes, Docker/SQLite) /api/jobs response shape.
func TestHandleJobsLegacyFieldNames(t *testing.T) {
	m := &mirrorv1alpha1.Mirror{
		ObjectMeta: metav1.ObjectMeta{Name: "debian", Namespace: "mirrors"},
		Status: mirrorv1alpha1.MirrorStatus{
			Phase:     mirrorv1alpha1.PhaseSyncing,
			ActivePVC: "debian-sync-1",
		},
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(m).Build()
	srv := httptest.NewServer((&Server{Client: c}).Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/jobs")
	if err != nil {
		t.Fatalf("GET /api/jobs: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("content type = %q, want application/json", ct)
	}

	var raw []map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatalf("body is not a JSON list: %v (%s)", err, body)
	}
	if len(raw) != 1 {
		t.Fatalf("got %d entries, want 1", len(raw))
	}
	// Legacy field names that must survive verbatim.
	for _, key := range []string{
		"id", "status", "updated_at", "last_success_at", "last_failure_at",
		"last_attempt_at", "next_attempt_at", "last_action_status", "actions",
	} {
		if _, ok := raw[0][key]; !ok {
			t.Errorf("legacy field %q missing from /api/jobs entry", key)
		}
	}
	// New fields.
	for _, key := range []string{"kind", "namespace", "phase", "active_pvc", "last_finished_at"} {
		if _, ok := raw[0][key]; !ok {
			t.Errorf("new field %q missing from /api/jobs entry", key)
		}
	}
}
