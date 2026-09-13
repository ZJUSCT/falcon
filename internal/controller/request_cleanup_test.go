package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	mirrorv1alpha1 "github.com/ZJUSCT/falcon/api/v1alpha1"
)

func TestCompletedRequestCleanupRecoversAcrossRestart(t *testing.T) {
	for _, afterRemoval := range []bool{false, true} {
		t.Run(map[bool]string{false: "before metadata removal", true: "after metadata removal"}[afterRemoval], func(t *testing.T) {
			m := abortRequestMirror()
			m.SetSyncPaused(true)
			m.Spec.Publish = mirrorv1alpha1.MirrorServicesSpec{}
			m.Annotations[SyncRequestAnnotation] = "true"
			m.Status.CurrentSync.Manual = true
			m.Status.CurrentSync.Phase = mirrorv1alpha1.SyncPhaseCancelling
			c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithStatusSubresource(&mirrorv1alpha1.Mirror{}).WithObjects(m).Build()
			injected := errors.New("simulated API outage")
			broken := interceptor.NewClient(c, interceptor.Funcs{
				Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
					if !afterRemoval {
						return injected
					}
					return c.Patch(ctx, obj, patch, opts...)
				},
				SubResourcePatch: func(ctx context.Context, c client.Client, sub string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
					if afterRemoval && obj.(*mirrorv1alpha1.Mirror).Status.RequestCleanup == nil {
						return injected
					}
					return c.SubResource(sub).Patch(ctx, obj, patch, opts...)
				},
			})
			r := &MirrorReconciler{Client: broken, SyncLimiter: NewSyncLimiter(1)}
			if _, err := r.reconcileCancellation(t.Context(), m, publicationHealth{}); !errors.Is(err, injected) {
				t.Fatalf("expected interrupted cleanup, got %v", err)
			}
			m = getMirror(t, t.Context(), c, client.ObjectKeyFromObject(m))
			if m.Status.CurrentSync != nil || m.Status.LastAttempt.Phase != mirrorv1alpha1.SyncPhaseCancelled || m.Status.RequestCleanup == nil {
				t.Fatal("completion and cleanup receipt must survive together")
			}
			if m.SyncRequested() == afterRemoval || m.AbortRequested() == afterRemoval {
				t.Fatal("unexpected metadata state at crash boundary")
			}
			if afterRemoval {
				// A genuinely new request after removal must survive receipt replay.
				m.Annotations[SyncRequestAnnotation] = "true"
				m.Annotations[mirrorv1alpha1.AbortRequestAnnotation] = "true"
				if err := c.Update(t.Context(), m); err != nil {
					t.Fatal(err)
				}
			}
			r = &MirrorReconciler{Client: c, Scheme: testScheme(t), Config: testConfig(), SyncLimiter: NewSyncLimiter(1)}
			req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(m)}
			reconcile(t, t.Context(), r, req)
			m = getMirror(t, t.Context(), c, req.NamespacedName)
			if m.Status.RequestCleanup != nil || m.Status.CurrentSync != nil || m.SyncRequested() != afterRemoval || m.AbortRequested() != afterRemoval {
				t.Fatal("recovery replayed completed work or removed a newer request")
			}
			if !afterRemoval {
				reconcile(t, t.Context(), r, req)
				if getMirror(t, t.Context(), c, req.NamespacedName).Status.CurrentSync != nil {
					t.Fatal("cancelled manual request restarted")
				}
			}
		})
	}
}

func TestRequestCleanupConflictsWithConcurrentMetadataEdit(t *testing.T) {
	m := abortRequestMirror()
	m.Status.CurrentSync = nil
	m.Status.RequestCleanup = &mirrorv1alpha1.MirrorRequestCleanup{Token: "completion", Abort: true}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithStatusSubresource(&mirrorv1alpha1.Mirror{}).WithObjects(m).Build()
	stale := getMirror(t, t.Context(), c, client.ObjectKeyFromObject(m))
	m = stale.DeepCopy()
	m.Annotations[mirrorv1alpha1.AbortRequestAnnotation] = "invalid-new-value"
	if err := c.Update(t.Context(), m); err != nil {
		t.Fatal(err)
	}
	r := &MirrorReconciler{Client: c}
	if _, err := r.reconcileRequestCleanup(t.Context(), stale); !apierrors.IsConflict(err) {
		t.Fatalf("stale cleanup must conflict: %v", err)
	}
	m = getMirror(t, t.Context(), c, client.ObjectKeyFromObject(m))
	if m.Annotations[mirrorv1alpha1.AbortRequestAnnotation] != "invalid-new-value" || m.Status.RequestCleanup == nil {
		t.Fatal("stale cleanup erased concurrent edit")
	}
}

func TestBooleanRequestsCoalesceAndAbortIsProcessedFirst(t *testing.T) {
	m := testMirror()
	m.Finalizers = []string{MirrorFinalizer}
	m.Spec.Publish = mirrorv1alpha1.MirrorServicesSpec{}
	m.Annotations = map[string]string{SyncRequestAnnotation: "true", mirrorv1alpha1.AbortRequestAnnotation: "true"}
	m.SetSyncPaused(true)
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithStatusSubresource(&mirrorv1alpha1.Mirror{}, &batchv1.Job{}).WithObjects(m).Build()
	now := time.Unix(1789000000, 0)
	r := &MirrorReconciler{Client: c, Scheme: testScheme(t), Config: testConfig(), SyncLimiter: NewSyncLimiter(1), Now: func() time.Time { return now }}
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(m)}
	reconcile(t, t.Context(), r, req)
	m = getMirror(t, t.Context(), c, req.NamespacedName)
	if m.AbortRequested() || !m.SyncRequested() || m.Status.CurrentSync != nil {
		t.Fatal("idle abort must be discarded before accepting sync")
	}
	reconcile(t, t.Context(), r, req)
	m = getMirror(t, t.Context(), c, req.NamespacedName)
	queued := m.Status.CurrentSync.QueuedAt.DeepCopy()
	for range 3 {
		m.Annotations[SyncRequestAnnotation] = "true"
		if err := c.Update(t.Context(), m); err != nil {
			t.Fatal(err)
		}
		reconcile(t, t.Context(), r, req)
		m = getMirror(t, t.Context(), c, req.NamespacedName)
		if !m.SyncRequested() || !m.Status.CurrentSync.Manual || !m.Status.CurrentSync.QueuedAt.Equal(queued) {
			t.Fatal("repeated true request replaced or consumed active run")
		}
	}
	job := &batchv1.Job{}
	get(t, t.Context(), c, client.ObjectKey{Namespace: m.Namespace, Name: currentSyncJobName(m)}, job)
	job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, LastTransitionTime: metav1.NewTime(now)}}
	if err := c.Status().Update(t.Context(), job); err != nil {
		t.Fatal(err)
	}
	reconcile(t, t.Context(), r, req)
	m = getMirror(t, t.Context(), c, req.NamespacedName)
	if m.SyncRequested() || m.Status.CurrentSync != nil || m.Status.LastSync.Phase != mirrorv1alpha1.SyncPhaseFailed {
		t.Fatal("failed Job did not complete and consume manual request")
	}
	now = now.Add(24 * time.Hour)
	reconcile(t, t.Context(), r, req)
	m = getMirror(t, t.Context(), c, req.NamespacedName)
	if m.Status.CurrentSync != nil {
		t.Fatal("coalesced requests or automatic retries bypassed pause after failure")
	}
}
