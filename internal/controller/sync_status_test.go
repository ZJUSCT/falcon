package controller

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	snapshotv1 "github.com/kubernetes-csi/external-snapshotter/client/v8/apis/volumesnapshot/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	mirrorv1alpha1 "github.com/ZJUSCT/falcon/api/v1alpha1"
	"github.com/ZJUSCT/falcon/internal/webapi"
)

func TestSyncJobResultSurvivesTransactionFailure(t *testing.T) {
	for _, failed := range []bool{false, true} {
		name := "snapshot failure after successful sync"
		if failed {
			name = "failed sync retains previous success"
		}
		t.Run(name, func(t *testing.T) {
			queued := metav1.Unix(1788000000, 0)
			started := metav1.Unix(1788000100, 0)
			finished := metav1.Unix(1788000200, 0)
			previous := metav1.Unix(1787900000, 0)
			now := finished.Add(time.Hour)
			mirror := testMirror()
			mirror.Status.CurrentSync = &mirrorv1alpha1.MirrorCurrentSyncStatus{QueuedAt: &queued, Phase: mirrorv1alpha1.SyncPhasePending}
			mirror.Status.LastSuccessfulSyncAt = &previous
			scheme := testScheme(t)
			c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&mirrorv1alpha1.Mirror{}).WithObjects(mirror).Build()
			r := &MirrorReconciler{Client: c, Scheme: scheme, Now: func() time.Time { return now }}
			job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: currentSyncJobName(mirror)}, Status: batchv1.JobStatus{StartTime: &started}}
			phase := mirrorv1alpha1.SyncPhaseSucceeded
			condition := batchv1.JobComplete
			wantSuccess := &finished
			if failed {
				phase, condition, wantSuccess = mirrorv1alpha1.SyncPhaseFailed, batchv1.JobFailed, &previous
			}
			// Failed Jobs use the terminal condition; successful Jobs carry completionTime.
			if !failed {
				job.Status.CompletionTime = &finished
			}
			job.Status.Conditions = []batchv1.JobCondition{{Type: condition, Status: corev1.ConditionTrue, LastTransitionTime: finished}}
			if err := r.observeSyncJob(t.Context(), mirror, job); err != nil {
				t.Fatal(err)
			}
			if !failed {
				snapshot := &snapshotv1.VolumeSnapshot{ObjectMeta: metav1.ObjectMeta{Name: currentSyncSnapshotName(mirror), Namespace: mirror.Namespace}, Status: &snapshotv1.VolumeSnapshotStatus{Error: &snapshotv1.VolumeSnapshotError{Message: stringPtr("failure details")}}}
				if err := c.Create(t.Context(), snapshot); err != nil {
					t.Fatal(err)
				}
				if _, err := r.reconcileSyncSnapshot(t.Context(), mirror, publicationHealth{}); err != nil {
					t.Fatal(err)
				}
			}
			current := getMirror(t, t.Context(), c, client.ObjectKeyFromObject(mirror))
			st := current.Status
			if st.LastSync == nil || st.LastSync.Phase != phase || (failed && (st.CurrentSync != nil || st.LastAttempt == nil || st.LastAttempt.Phase != phase)) || (!failed && (st.CurrentSync == nil || st.CurrentSync.Phase != mirrorv1alpha1.SyncPhaseSnapshotting || st.LastAttempt != nil)) {
				t.Fatalf("unexpected Job/transaction results: %#v", st)
			}
			if !st.LastSync.StartedAt.Equal(&started) || !st.LastSync.FinishedAt.Equal(&finished) || !st.LastSuccessfulSyncAt.Equal(wantSuccess) {
				t.Fatalf("Job, queue and transaction times must stay independent: %#v", st)
			}
			if condition := findCondition(current.Status.Conditions, conditionDegraded); condition == nil || condition.Status != metav1.ConditionTrue {
				t.Fatal("transaction failure must remain degraded while idle")
			}
		})
	}
}

func assertMirrorZLifecycleStatus(t *testing.T, c client.Client, want string) {
	t.Helper()
	s := &webapi.Server{Client: c, CatalogEnabled: true, Site: webapi.SiteConfig{URL: "https://example.org", Abbr: "TEST"}}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/mirrorz.json", nil))
	var doc struct {
		Mirrors []struct {
			Status string `json:"status"`
		} `json:"mirrors"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if w.Code != http.StatusOK || len(doc.Mirrors) != 1 || doc.Mirrors[0].Status != want {
		t.Fatalf("want %s, got HTTP %d: %s", want, w.Code, w.Body.String())
	}
	var complete map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &complete); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(complete)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("MIRRORZ_CATALOG %s", encoded)
}

func TestSuccessfulJobRequiresCompletionBeforeSnapshot(t *testing.T) {
	mirror := testMirror()
	scheme := testScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&mirrorv1alpha1.Mirror{}, &batchv1.Job{}).WithObjects(mirror).Build()
	r := &MirrorReconciler{Client: c, Scheme: scheme, Config: testConfig(), SyncLimiter: NewSyncLimiter(0)}
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(mirror)}
	for i := 0; i < 3; i++ {
		reconcile(t, t.Context(), r, req)
	}
	mirror = getMirror(t, t.Context(), c, req.NamespacedName)
	job := &batchv1.Job{}
	get(t, t.Context(), c, client.ObjectKey{Namespace: mirror.Namespace, Name: currentSyncJobName(mirror)}, job)
	job.Status.StartTime = timePtr(time.Now().UTC())
	job.Status.Succeeded = 1
	if err := c.Status().Update(t.Context(), job); err != nil {
		t.Fatal(err)
	}
	reconcile(t, t.Context(), r, req)
	assertNotFound(t, t.Context(), c, client.ObjectKey{Namespace: mirror.Namespace, Name: testSnapshotName(mirror)}, &snapshotv1.VolumeSnapshot{})
	job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
	if err := c.Status().Update(t.Context(), job); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(t.Context(), req); err == nil || !strings.Contains(err.Error(), "completionTime") {
		t.Fatalf("expected explicit completionTime error, got %v", err)
	}
	mirror = getMirror(t, t.Context(), c, req.NamespacedName)
	if mirror.Status.LastSuccessfulSyncAt != nil || mirror.Status.ActivePVC != "" || mirror.Status.LastSync != nil {
		t.Fatalf("invalid Job advanced publication: %#v", mirror.Status)
	}
	if cond := findCondition(mirror.Status.Conditions, conditionDegraded); cond == nil || cond.Reason != "SyncJobStatusInvalid" {
		t.Fatalf("missing diagnostic: %#v", mirror.Status.Conditions)
	}
	assertNotFound(t, t.Context(), c, client.ObjectKey{Namespace: mirror.Namespace, Name: testSnapshotName(mirror)}, &snapshotv1.VolumeSnapshot{})
	job.Status.CompletionTime = timePtr(job.Status.StartTime.Add(time.Minute))
	if err := c.Status().Update(t.Context(), job); err != nil {
		t.Fatal(err)
	}
	reconcile(t, t.Context(), r, req)
	mirror = getMirror(t, t.Context(), c, req.NamespacedName)
	if !mirror.Status.LastSuccessfulSyncAt.Equal(job.Status.CompletionTime) {
		t.Fatalf("corrected completion was not persisted: %#v", mirror.Status)
	}
	get(t, t.Context(), c, client.ObjectKey{Namespace: mirror.Namespace, Name: testSnapshotName(mirror)}, &snapshotv1.VolumeSnapshot{})
	previous := mirror.Status.LastSuccessfulSyncAt.DeepCopy()
	// A subsequent inconsistent observation must not erase the saved history.
	job.Status.CompletionTime = nil
	if err := r.observeSyncJob(t.Context(), mirror, job); err == nil {
		t.Fatal("expected inconsistent Job error")
	}
	mirror = getMirror(t, t.Context(), c, req.NamespacedName)
	if !mirror.Status.LastSuccessfulSyncAt.Equal(previous) || !mirror.Status.LastSync.FinishedAt.Equal(previous) {
		t.Fatal("invalid Job erased successful completion")
	}
}
