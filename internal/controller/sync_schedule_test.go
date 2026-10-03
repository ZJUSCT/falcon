package controller

import (
	"testing"
	"time"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	mirrorv1alpha1 "github.com/ZJUSCT/falcon/api/v1alpha1"
)

func TestAutomaticPhaseUsesFleetRate(t *testing.T) {
	now := time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC)
	first := testMirror()
	first.Spec.Publish = mirrorv1alpha1.MirrorServicesSpec{}
	scheme := testScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(first).Build()
	r := &MirrorReconciler{Client: c, Scheme: scheme}

	single, err := r.planAutomaticSync(t.Context(), first, now)
	if err != nil {
		t.Fatal(err)
	}
	second := first.DeepCopy()
	second.Name = "second"
	second.UID = "second-mirror-uid"
	second.ResourceVersion = ""
	if err := c.Create(t.Context(), second); err != nil {
		t.Fatal(err)
	}
	fleet, err := r.planAutomaticSync(t.Context(), first, now)
	if err != nil {
		t.Fatal(err)
	}
	if single.Equal(fleet) {
		t.Fatalf("adding a mirror did not change the fleet-derived phase: %v", single)
	}
	again, err := r.planAutomaticSync(t.Context(), first, now)
	if err != nil {
		t.Fatal(err)
	}
	if !fleet.Equal(again) {
		t.Fatalf("phase is not stable: first %v, second %v", fleet, again)
	}
}

func TestAutomaticBootstrapIsScheduledBeforeStarting(t *testing.T) {
	now := time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC)
	m := testMirror()
	m.Finalizers = []string{MirrorFinalizer}
	m.Spec.Publish = mirrorv1alpha1.MirrorServicesSpec{}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithStatusSubresource(&mirrorv1alpha1.Mirror{}).WithObjects(m).Build()
	clock := now
	r := &MirrorReconciler{Client: c, Scheme: testScheme(t), Config: testConfig(), Now: func() time.Time { return clock }}
	req := client.ObjectKeyFromObject(m)

	if _, err := r.Reconcile(t.Context(), reconcileRequest(req)); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(t.Context(), reconcileRequest(req)); err != nil {
		t.Fatal(err)
	}
	m = getMirror(t, t.Context(), c, req)
	if m.Status.CurrentSync != nil || m.Status.NextSyncAt == nil || !m.Status.NextSyncAt.After(now) {
		t.Fatalf("bootstrap was not scheduled: %#v", m.Status)
	}
	clock = m.Status.NextSyncAt.Time
	if _, err := r.Reconcile(t.Context(), reconcileRequest(req)); err != nil {
		t.Fatal(err)
	}
	m = getMirror(t, t.Context(), c, req)
	if m.Status.CurrentSync == nil || m.Status.CurrentSync.Manual {
		t.Fatalf("scheduled bootstrap did not start automatically: %#v", m.Status)
	}
}

func TestExistingMirrorWithoutPhaseIsScheduledAfterUpgrade(t *testing.T) {
	now := time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC)
	m := testMirror()
	m.Finalizers = []string{MirrorFinalizer}
	m.Spec.Publish = mirrorv1alpha1.MirrorServicesSpec{}
	m.Status.ActivePVC = "smoke-snap-previous"
	m.Status.LastAttempt = &mirrorv1alpha1.MirrorSyncStatus{Phase: mirrorv1alpha1.SyncPhaseSucceeded}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithStatusSubresource(&mirrorv1alpha1.Mirror{}).WithObjects(m).Build()
	r := &MirrorReconciler{Client: c, Scheme: testScheme(t), Config: testConfig(), Now: func() time.Time { return now }}
	req := client.ObjectKeyFromObject(m)

	if _, err := r.Reconcile(t.Context(), reconcileRequest(req)); err != nil {
		t.Fatal(err)
	}
	m = getMirror(t, t.Context(), c, req)
	if m.Status.CurrentSync != nil || m.Status.NextSyncAt == nil || !m.Status.NextSyncAt.After(now) {
		t.Fatalf("existing mirror was not assigned an automatic phase: %#v", m.Status)
	}
}

func TestManualSyncPreservesAutomaticPhase(t *testing.T) {
	now := time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC)
	m := testMirror()
	m.Finalizers = []string{MirrorFinalizer}
	m.Status.NextSyncAt = timePtr(now.Add(30 * time.Minute))
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithStatusSubresource(&mirrorv1alpha1.Mirror{}).WithObjects(m).Build()
	r := &MirrorReconciler{Client: c, Scheme: testScheme(t), Config: testConfig(), Now: func() time.Time { return now }}
	planned := m.Status.NextSyncAt.DeepCopy()
	if _, err := r.startSync(t.Context(), m, true, publicationHealth{}); err != nil {
		t.Fatal(err)
	}
	current := getMirror(t, t.Context(), c, client.ObjectKeyFromObject(m))
	if current.Status.CurrentSync == nil || !current.Status.CurrentSync.Manual {
		t.Fatalf("manual synchronization did not start: %#v", current.Status)
	}
	if !current.Status.NextSyncAt.Equal(planned) {
		t.Fatalf("manual synchronization moved automatic phase: got %v want %v", current.Status.NextSyncAt, planned)
	}
}

func reconcileRequest(key client.ObjectKey) ctrl.Request {
	return ctrl.Request{NamespacedName: key}
}
