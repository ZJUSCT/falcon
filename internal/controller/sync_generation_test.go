package controller

import (
	"testing"
	"time"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	mirrorv1alpha1 "github.com/ZJUSCT/falcon/api/v1alpha1"
)

func TestCancelledGenerationDefersSameSecondFollowupAcrossRestart(t *testing.T) {
	ctx := t.Context()
	now := time.Unix(1789000000, 100000000)
	m := testMirror()
	m.Finalizers = []string{MirrorFinalizer}
	m.Spec.Publish = mirrorv1alpha1.MirrorServicesSpec{}
	m.Spec.Sync.Paused = true
	m.Annotations = map[string]string{SyncRequestAnnotation: "true"}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithStatusSubresource(&mirrorv1alpha1.Mirror{}).WithObjects(m).Build()
	restart := func() *MirrorReconciler {
		return &MirrorReconciler{Client: c, Scheme: testScheme(t), Config: testConfig(), SyncLimiter: NewSyncLimiter(1), Now: func() time.Time { return now }}
	}
	r := restart()
	if _, err := r.startSync(ctx, m, true, publicationHealth{}); err != nil {
		t.Fatal(err)
	}
	first := m.Status.CurrentSync.QueuedAt.DeepCopy()
	if !first.Equal(timePtr(now.Truncate(time.Second))) {
		t.Fatal("generation must use second precision")
	}
	if _, err := r.startSync(ctx, m, true, publicationHealth{}); err != nil {
		t.Fatal(err)
	}
	if !m.Status.CurrentSync.Manual || !m.Status.CurrentSync.QueuedAt.Equal(first) {
		t.Fatal("duplicate trigger replaced active generation")
	}
	m.Status.CurrentSync.Phase = mirrorv1alpha1.SyncPhaseCancelling
	if err := c.Status().Update(ctx, m); err != nil {
		t.Fatal(err)
	}
	if _, err := r.reconcileCancellation(ctx, m, publicationHealth{}); err != nil {
		t.Fatal(err)
	}
	m = getMirror(t, ctx, c, client.ObjectKeyFromObject(m))
	m.Annotations[SyncRequestAnnotation] = "true"
	if err := c.Update(ctx, m); err != nil {
		t.Fatal(err)
	}
	now = time.Unix(first.Unix(), 800000000)
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(m)}
	result, err := restart().Reconcile(ctx, req)
	if err != nil || result.RequeueAfter != 200*time.Millisecond {
		t.Fatalf("follow-up was not deferred to next second: %#v, %v", result, err)
	}
	m = getMirror(t, ctx, c, req.NamespacedName)
	if m.Status.CurrentSync != nil || !m.Status.LastAcceptedSyncAt.Equal(first) {
		t.Fatal("same-second follow-up lost or reused generation")
	}
	now = time.Unix(first.Unix()+1, 0)
	reconcile(t, ctx, restart(), req)
	m = getMirror(t, ctx, c, req.NamespacedName)
	if m.Status.CurrentSync == nil || !m.Status.CurrentSync.Manual || m.Status.CurrentSync.QueuedAt.Unix() != first.Unix()+1 {
		t.Fatal("follow-up did not start in next second")
	}
}
