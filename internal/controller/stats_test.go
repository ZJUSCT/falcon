package controller

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	snapshotv1 "github.com/kubernetes-csi/external-snapshotter/client/v8/apis/volumesnapshot/v1"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	mirrorv1alpha1 "github.com/ZJUSCT/falcon/api/v1alpha1"
)

// cannedSummary mirrors the kubelet stats summary shape the reader consumes.
const cannedSummary = `{
  "pods": [
    {"podRef": {"name": "smoke-publish-http", "namespace": "mirrors"},
     "volume": [{"name": "mirror-data",
                 "pvcRef": {"name": "smoke-snap-1756158000", "namespace": "mirrors"},
                 "usedBytes": 640141257728}]},
    {"podRef": {"name": "other-publish-http", "namespace": "other"},
     "volume": [{"name": "mirror-data",
                 "pvcRef": {"name": "smoke-snap-1756158000", "namespace": "mirrors"},
                 "usedBytes": 1}]},
    {"podRef": {"name": "no-ref", "namespace": "mirrors"},
     "volume": [{"name": "mirror-data", "usedBytes": 123}]},
    {"podRef": {"name": "no-usage", "namespace": "mirrors"},
     "volume": [{"name": "mirror-data",
                 "pvcRef": {"name": "pending-usage", "namespace": "mirrors"}}]}
  ]
}`

// stubSummarySource is a KubeletUsageReader over a canned fetch function that
// counts fetches; fail, if set, makes fetches fail.
func stubSummarySource(t *testing.T, body string, fail *bool) (*KubeletUsageReader, *int) {
	t.Helper()
	calls := 0
	fetchErr := errors.New("stats fetch failed")
	reader := &KubeletUsageReader{
		ttl:     time.Minute,
		now:     func() time.Time { return time.Unix(1756158000, 0) },
		entries: map[string]statsSummaryEntry{},
	}
	reader.fetch = func(_ context.Context, _ string) ([]byte, error) {
		calls++
		if fail != nil && *fail {
			return nil, fetchErr
		}
		return []byte(body), nil
	}
	return reader, &calls
}

// TestKubeletUsageReaderMatchesPVC: the reader picks the volumeStats entry by
// (namespace, name) pvcRef — the same PVC referenced from another namespace
// and volumes without a pvcRef must not match, and a volume without
// usedBytes is reported as unknown, not as zero.
func TestKubeletUsageReaderMatchesPVC(t *testing.T) {
	reader, _ := stubSummarySource(t, cannedSummary, nil)

	size, ok, err := reader.PVCUsedBytes(context.Background(), "s3.mirrors.zjusct.io", "mirrors", "smoke-snap-1756158000")
	if err != nil || !ok {
		t.Fatalf("expected the matching PVC usage, got ok=%v err=%v", ok, err)
	}
	if size != 640141257728 {
		t.Fatalf("size = %d, want 640141257728", size)
	}

	// Same PVC name, wrong namespace: no match.
	if _, ok, _ := reader.PVCUsedBytes(context.Background(), "s3.mirrors.zjusct.io", "other", "smoke-snap-1756158000"); ok {
		t.Fatal("a same-name PVC in another namespace must not match")
	}
	// Volume without a pvcRef: no match.
	if _, ok, _ := reader.PVCUsedBytes(context.Background(), "s3.mirrors.zjusct.io", "mirrors", "no-ref"); ok {
		t.Fatal("a volume without pvcRef must not match")
	}
	// Matching PVCRef without usedBytes: unknown, not zero.
	if _, ok, _ := reader.PVCUsedBytes(context.Background(), "s3.mirrors.zjusct.io", "mirrors", "pending-usage"); ok {
		t.Fatal("a matching volume without usedBytes must be reported as unknown")
	}
	// PVC entirely absent from the summary.
	if _, ok, _ := reader.PVCUsedBytes(context.Background(), "s3.mirrors.zjusct.io", "mirrors", "absent"); ok {
		t.Fatal("an absent PVC must be reported as unknown")
	}
}

// TestKubeletUsageReaderCacheTTL: per-node summaries are reused within the
// TTL and refetched after it expires; fetch failures are not cached.
func TestKubeletUsageReaderCacheTTL(t *testing.T) {
	reader, calls := stubSummarySource(t, cannedSummary, nil)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		if _, ok, err := reader.PVCUsedBytes(ctx, "node-a", "mirrors", "smoke-snap-1756158000"); err != nil || !ok {
			t.Fatalf("call %d: ok=%v err=%v", i, ok, err)
		}
	}
	if got := *calls; got != 1 {
		t.Fatalf("fetches = %d, want 1 (TTL cache must serve repeats)", got)
	}

	// A different node has no cached summary: one more fetch.
	reader.PVCUsedBytes(ctx, "node-b", "mirrors", "smoke-snap-1756158000")
	if got := *calls; got != 2 {
		t.Fatalf("fetches after second node = %d, want 2", got)
	}

	// Advance the clock past the TTL: the next call refetches node-a.
	before := *calls
	reader.now = func() time.Time { return time.Unix(1756158000, 0).Add(2 * reader.ttl) }
	reader.PVCUsedBytes(ctx, "node-a", "mirrors", "smoke-snap-1756158000")
	if got := *calls; got != before+1 {
		t.Fatalf("fetches after TTL expiry = %d, want %d", got, before+1)
	}

	// A failed fetch is not cached: the immediate retry refetches.
	fail := true
	reader2, calls2 := stubSummarySource(t, cannedSummary, &fail)
	if _, _, err := reader2.PVCUsedBytes(ctx, "node-a", "mirrors", "smoke-snap-1756158000"); err == nil {
		t.Fatal("expected the fetch error to surface")
	}
	fail = false
	if _, ok, err := reader2.PVCUsedBytes(ctx, "node-a", "mirrors", "smoke-snap-1756158000"); err != nil || !ok {
		t.Fatalf("retry after failure: ok=%v err=%v", ok, err)
	}
	if got := *calls2; got != 2 {
		t.Fatalf("fetches across failure+retry = %d, want 2 (failures must not be cached)", got)
	}
}

// stubUsageReader is a PVCUsageReader returning a fixed answer for one
// (node, namespace, pvc) key and counting the calls.
type stubUsageReader struct {
	node, namespace, pvc string
	size                 int64
	calls                int
}

func (s *stubUsageReader) PVCUsedBytes(_ context.Context, node, namespace, pvc string) (int64, bool, error) {
	s.calls++
	if node == s.node && namespace == s.namespace && pvc == s.pvc {
		return s.size, true, nil
	}
	return 0, false, nil
}

// runningPublishPod builds the pod publishPVCUsage locates: a Running pod of
// one publish service entry, carrying the mirror/component labels and a
// nodeName.
func runningPublishPod(mirror *mirrorv1alpha1.Mirror, protocol, nodeName string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: mirror.Namespace,
			Name:      "smoke-publish-" + protocol + "-abcde",
			Labels:    map[string]string{MirrorLabel: "smoke", ComponentLabel: publishRole(protocol)},
		},
		Spec: corev1.PodSpec{NodeName: nodeName},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{{
				Type:               corev1.PodReady,
				Status:             corev1.ConditionTrue,
				LastTransitionTime: metav1.NewTime(time.Unix(1756158000, 0)),
			}},
		},
	}
}

// TestPublishPVCUsageBestEffort: usage accounting resolves the node from a
// running publish pod; without one (sync-only mirror, pod pending) or with
// no UsageReader it reports unknown and never errors.
func TestPublishPVCUsageBestEffort(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme(t)
	mirror := testMirror()
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&mirrorv1alpha1.Mirror{}, &appsv1.Deployment{}).
		WithObjects(mirror, runningPublishPod(mirror, "http", "s3.mirrors.zjusct.io")).
		Build()
	reconciler := &MirrorReconciler{Client: fakeClient, Scheme: scheme}

	usage := &stubUsageReader{node: "s3.mirrors.zjusct.io", namespace: "mirrors", pvc: "smoke-snap-1", size: 1234}
	reconciler.UsageReader = usage
	size, ok := reconciler.publishPVCUsage(ctx, mirror, "smoke-snap-1")
	if !ok || size != 1234 {
		t.Fatalf("size=%d ok=%v, want 1234/true", size, ok)
	}
	if usage.calls != 1 {
		t.Fatalf("usage calls = %d, want 1", usage.calls)
	}

	// No running publish pod (sync-only mirror): unknown, no reader call.
	if err := fakeClient.Delete(ctx, runningPublishPod(mirror, "http", "s3.mirrors.zjusct.io")); err != nil {
		t.Fatalf("delete publish pod: %v", err)
	}
	if _, ok := reconciler.publishPVCUsage(ctx, mirror, "smoke-snap-1"); ok {
		t.Fatal("usage without a running publish pod must be unknown")
	}
	if usage.calls != 1 {
		t.Fatalf("usage calls after pod removal = %d, want 1", usage.calls)
	}

	// Nil UsageReader: accounting disabled, immediately unknown.
	reconciler.UsageReader = nil
	if _, ok := reconciler.publishPVCUsage(ctx, mirror, "smoke-snap-1"); ok {
		t.Fatal("usage without a reader must be unknown")
	}
}

// TestPublishActivationRecordsSizeBytes walks the pipeline to publication
// with a running publish pod and a working reader: the activation patch
// carries status.sizeBytes, and the idle reconcile afterwards must not
// recompute it (the value is computed once — publish PVC content is
// immutable).
func TestPublishActivationRecordsSizeBytes(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 31, 18, 0, 0, 0, time.UTC)
	mirror := testMirror()
	scheme := testScheme(t)
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&mirrorv1alpha1.Mirror{}, &batchv1.Job{}, &snapshotv1.VolumeSnapshot{}, &appsv1.Deployment{}).
		WithObjects(mirror).
		Build()
	usage := &stubUsageReader{node: "s3.mirrors.zjusct.io", namespace: mirror.Namespace, pvc: fmt.Sprintf("smoke-snap-%d", now.Unix()), size: 640141257728}
	reconciler := &MirrorReconciler{
		Client:      fakeClient,
		Scheme:      scheme,
		Recorder:    record.NewFakeRecorder(20),
		Now:         func() time.Time { return now },
		Config:      testConfig(),
		SyncLimiter: NewSyncLimiter(0),
		UsageReader: usage,
	}
	request := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: mirror.Namespace, Name: mirror.Name}}
	addBoundSyncPVC(t, ctx, fakeClient, mirror, "smoke-sync", "s3.mirrors.zjusct.io", hostnameAffinity("s3.mirrors.zjusct.io"))

	reconcile(t, ctx, reconciler, request) // finalizer
	reconcile(t, ctx, reconciler, request) // startSync
	reconcile(t, ctx, reconciler, request) // sync PVC + Job
	current := getMirror(t, ctx, fakeClient, request.NamespacedName)
	job := &batchv1.Job{}
	get(t, ctx, fakeClient, client.ObjectKey{Namespace: mirror.Namespace, Name: currentSyncJobName(current)}, job)
	job.Status.Succeeded = 1
	job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
	if err := fakeClient.Status().Update(ctx, job); err != nil {
		t.Fatalf("mark Job complete: %v", err)
	}
	reconcile(t, ctx, reconciler, request) // create snapshot
	snapshot := &snapshotv1.VolumeSnapshot{}
	get(t, ctx, fakeClient, client.ObjectKey{Namespace: mirror.Namespace, Name: currentSyncSnapshotName(current)}, snapshot)
	ready := true
	snapshot.Status = &snapshotv1.VolumeSnapshotStatus{ReadyToUse: &ready}
	if err := fakeClient.Status().Update(ctx, snapshot); err != nil {
		t.Fatalf("mark snapshot ready: %v", err)
	}
	reconcile(t, ctx, reconciler, request) // publish PVC + Deployment
	deployment := &appsv1.Deployment{}
	get(t, ctx, fakeClient, client.ObjectKey{Namespace: mirror.Namespace, Name: "smoke-publish-http"}, deployment)
	deployment.Status.ObservedGeneration = deployment.Generation
	deployment.Status.AvailableReplicas = 1
	deployment.Status.UpdatedReplicas = 1
	if err := fakeClient.Status().Update(ctx, deployment); err != nil {
		t.Fatalf("mark Deployment available: %v", err)
	}
	// The publish rollout is available: a running publish pod exists on the storage
	// node, so activation can resolve the PVC usage.
	if err := fakeClient.Create(ctx, runningPublishPod(mirror, "http", "s3.mirrors.zjusct.io")); err != nil {
		t.Fatalf("create publish pod: %v", err)
	}

	reconcile(t, ctx, reconciler, request) // activation
	current = getMirror(t, ctx, fakeClient, request.NamespacedName)
	if current.Status.ActivePVC == "" {
		t.Fatalf("expected an activated publication, got %#v", current.Status)
	}
	if current.Status.SizeBytes != 640141257728 {
		t.Fatalf("activation sizeBytes = %d, want 640141257728", current.Status.SizeBytes)
	}
	if usage.calls != 1 {
		t.Fatalf("usage calls after activation = %d, want 1", usage.calls)
	}

	// The idle reconcile must not recompute: sizeBytes is already set.
	reconcile(t, ctx, reconciler, request)
	current = getMirror(t, ctx, fakeClient, request.NamespacedName)
	if current.Status.SizeBytes != 640141257728 {
		t.Fatalf("sizeBytes changed on the idle path: %d", current.Status.SizeBytes)
	}
	if usage.calls != 1 {
		t.Fatalf("usage calls after idle reconcile = %d, want 1 (backfill only when unset)", usage.calls)
	}
}

// TestIdlePathBackfillsSizeBytes: a published Mirror whose sizeBytes was
// never determined (e.g. the rollout was not mounted at activation time) gets
// it filled by the idle reconcile, once.
func TestIdlePathBackfillsSizeBytes(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 31, 19, 0, 0, 0, time.UTC)
	mirror := testMirror()
	mirror.Finalizers = []string{MirrorFinalizer}
	mirror.Status = mirrorv1alpha1.MirrorStatus{
		ObservedGeneration: mirror.Generation,
		WorkPVC:            "smoke-sync",
		ActivePVC:          "smoke-snap-1756147200",
		ActiveSnapshot:     "smoke-snap-1756147200",
		LastSync:           &mirrorv1alpha1.MirrorSyncStatus{JobName: "smoke-sync-1756147200", Phase: mirrorv1alpha1.SyncPhaseSucceeded},
	}
	scheme := testScheme(t)
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&mirrorv1alpha1.Mirror{}, &appsv1.Deployment{}).
		WithObjects(mirror).
		Build()
	usage := &stubUsageReader{node: "s3.mirrors.zjusct.io", namespace: mirror.Namespace, pvc: "smoke-snap-1756147200", size: 42}
	reconciler := &MirrorReconciler{
		Client:      fakeClient,
		Scheme:      scheme,
		Recorder:    record.NewFakeRecorder(20),
		Now:         func() time.Time { return now },
		Config:      testConfig(),
		SyncLimiter: NewSyncLimiter(0),
		UsageReader: usage,
	}
	request := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: mirror.Namespace, Name: mirror.Name}}
	addBoundSyncPVC(t, ctx, fakeClient, mirror, "smoke-sync", "s3.mirrors.zjusct.io", hostnameAffinity("s3.mirrors.zjusct.io"))

	reconcile(t, ctx, reconciler, request) // ensure publish workload (not ready yet)
	deployment := &appsv1.Deployment{}
	get(t, ctx, fakeClient, client.ObjectKey{Namespace: mirror.Namespace, Name: "smoke-publish-http"}, deployment)
	deployment.Status.ObservedGeneration = deployment.Generation
	deployment.Status.AvailableReplicas = 1
	deployment.Status.UpdatedReplicas = 1
	if err := fakeClient.Status().Update(ctx, deployment); err != nil {
		t.Fatalf("mark Deployment available: %v", err)
	}
	if err := fakeClient.Create(ctx, runningPublishPod(mirror, "http", "s3.mirrors.zjusct.io")); err != nil {
		t.Fatalf("create publish pod: %v", err)
	}

	reconcile(t, ctx, reconciler, request) // idle path: backfill
	current := getMirror(t, ctx, fakeClient, request.NamespacedName)
	if current.Status.SizeBytes != 42 {
		t.Fatalf("idle backfill sizeBytes = %d, want 42", current.Status.SizeBytes)
	}
	if usage.calls != 1 {
		t.Fatalf("usage calls after backfill = %d, want 1", usage.calls)
	}

	reconcile(t, ctx, reconciler, request) // second idle pass: no recompute
	if usage.calls != 1 {
		t.Fatalf("usage calls after second idle pass = %d, want 1", usage.calls)
	}
	current = getMirror(t, ctx, fakeClient, request.NamespacedName)
	if current.Status.SizeBytes != 42 {
		t.Fatalf("sizeBytes changed on the second idle pass: %d", current.Status.SizeBytes)
	}
}
