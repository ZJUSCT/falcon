package controller

import (
	"testing"
	"time"

	snapshotv1 "github.com/kubernetes-csi/external-snapshotter/client/v8/apis/volumesnapshot/v1"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	mirrorv1alpha1 "github.com/ZJUSCT/falcon/api/v1alpha1"
)

func TestPublicationFailureRecoveryAndDrainBlockNextSyncAcrossRestart(t *testing.T) {
	ctx := t.Context()
	now := time.Unix(1789000000, 0)
	m := testMirror()
	m.Finalizers = []string{MirrorFinalizer}
	m.Annotations = map[string]string{SyncRequestAnnotation: "true"}
	m.SetSyncPaused(true)
	m.Status = mirrorv1alpha1.MirrorStatus{
		ObservedGeneration: m.Generation, WorkPVC: "smoke-sync", ActivePVC: "smoke-snap-old", ActiveSnapshot: "smoke-snap-old",
		LastSync:             &mirrorv1alpha1.MirrorSyncStatus{JobName: "smoke-sync-completed", Phase: "Succeeded", FinishedAt: timePtr(now.Add(-time.Hour))},
		LastSuccessfulSyncAt: timePtr(now.Add(-time.Hour)), NextSyncAt: timePtr(now.Add(-time.Minute)),
		CurrentSync: &mirrorv1alpha1.MirrorCurrentSyncStatus{StartedAt: timePtr(now.Add(-time.Hour)), QueuedAt: timePtr(now.Add(-2 * time.Hour)), Phase: "Snapshotting", Manual: true},
	}
	name := currentSyncSnapshotName(m)
	snapshot := &snapshotv1.VolumeSnapshot{
		ObjectMeta: metav1.ObjectMeta{Namespace: m.Namespace, Name: name},
		Status:     &snapshotv1.VolumeSnapshotStatus{ReadyToUse: ptr.To(false), Error: &snapshotv1.VolumeSnapshotError{Message: ptr.To("storage unavailable")}},
	}
	scheme := testScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&mirrorv1alpha1.Mirror{}, &appsv1.Deployment{}, &snapshotv1.VolumeSnapshot{}).WithObjects(m, snapshot).Build()
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(m)}
	// Recreate the reconciler for every observation: only API state survives.
	restart := func() *MirrorReconciler {
		return &MirrorReconciler{Client: c, Scheme: scheme, Config: testConfig(), SyncLimiter: NewSyncLimiter(1), Now: func() time.Time { return now }}
	}
	step := func() *mirrorv1alpha1.Mirror {
		reconcile(t, ctx, restart(), request)
		return getMirror(t, ctx, c, request.NamespacedName)
	}
	assertBlocked := func(current *mirrorv1alpha1.Mirror) {
		t.Helper()
		if !current.SyncRequested() {
			t.Fatal("outstanding manual request lost")
		}
		if (current.Status.CurrentSync == nil && current.Status.Publication == nil) || current.Status.ConsecutiveFailures != 0 || current.Status.LastSync.Phase != "Succeeded" {
			t.Fatalf("publication must preserve sync success and block the next request: %#v", current.Status)
		}
		jobs := &batchv1.JobList{}
		if err := c.List(ctx, jobs); err != nil || len(jobs.Items) != 0 {
			t.Fatalf("unexpected new writer: %v, %#v", err, jobs.Items)
		}
	}
	current := step()
	assertBlocked(current)
	if !current.SyncRequested() {
		t.Fatal("snapshot failure consumed unfinished manual request")
	}
	if condition := findCondition(current.Status.Conditions, "Degraded"); condition == nil || condition.Reason != "SnapshotFailed" {
		t.Fatalf("missing snapshot diagnostic: %#v", condition)
	}
	get(t, ctx, c, client.ObjectKeyFromObject(snapshot), snapshot)
	snapshot.Status = &snapshotv1.VolumeSnapshotStatus{ReadyToUse: ptr.To(true)}
	if err := c.Status().Update(ctx, snapshot); err != nil {
		t.Fatal(err)
	}
	current = step() // durable snapshot handoff
	if current.Status.CurrentSync != nil || current.Status.LastSnapshot == nil || current.SyncRequested() || current.Status.Publication == nil {
		t.Fatal("snapshot handoff was not durable")
	}
	current.Annotations = map[string]string{SyncRequestAnnotation: "true"}
	if err := c.Update(ctx, current); err != nil {
		t.Fatal(err)
	}
	current = step() // clone and workload
	assertBlocked(current)
	if current.Status.Publication.Snapshot != name || current.Status.Publication.PVC != "" || current.Status.ActivePVC != "smoke-snap-old" {
		t.Fatalf("candidate readiness overwrote the served generation: %#v", current.Status)
	}
	deployment := &appsv1.Deployment{}
	get(t, ctx, c, client.ObjectKey{Namespace: m.Namespace, Name: "smoke-publish-http"}, deployment)
	// The consumer exists even though the clone has not bound.
	addBoundPublishPVC(t, ctx, c, m, name)
	markRouteAccepted(t, ctx, c, m.Namespace, "smoke-publish")
	deployment.UID = "publish-deployment"
	if err := c.Update(ctx, deployment); err != nil {
		t.Fatal(err)
	}
	deployment.Status = appsv1.DeploymentStatus{ObservedGeneration: deployment.Generation, Replicas: 2, UpdatedReplicas: 1, AvailableReplicas: 1}
	if err := c.Status().Update(ctx, deployment); err != nil {
		t.Fatal(err)
	}
	current = step()
	if current.Status.ActivePVC != "smoke-snap-old" {
		t.Fatal("old availability must not certify the new Pod")
	}
	markPublishDeploymentAvailable(t, ctx, c, m.Namespace)
	oldSet := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Namespace: m.Namespace, Name: "smoke-publish-http-old", UID: "old-set", Labels: objectLabels(m.Name, "publish-http"), OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(deployment, appsv1.SchemeGroupVersion.WithKind("Deployment"))}}}
	oldPod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: m.Namespace, Name: "old-pod", Labels: objectLabels(m.Name, "publish-http"), Finalizers: []string{"test/drain"}, OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(oldSet, appsv1.SchemeGroupVersion.WithKind("ReplicaSet"))}}}
	if err := c.Create(ctx, oldSet); err != nil {
		t.Fatal(err)
	}
	if err := c.Create(ctx, oldPod); err != nil {
		t.Fatal(err)
	}
	if err := c.Delete(ctx, oldPod); err != nil {
		t.Fatal(err)
	}
	current = step()
	assertBlocked(current)
	if current.Status.ActivePVC != name || current.Status.Publication.Phase != "Draining" || current.Status.LastPublishedAt == nil {
		t.Fatalf("new serving generation not recorded: %#v", current.Status)
	}
	published := current.Status.LastPublishedAt.DeepCopy()
	// A deleted ReplicaSet must not hide a still-terminating Pod.
	if err := c.Delete(ctx, oldSet); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	current = step()
	assertBlocked(current)
	if !current.Status.LastPublishedAt.Equal(published) || findCondition(current.Status.Conditions, "Ready").Status != metav1.ConditionTrue || findCondition(current.Status.Conditions, "Progressing").Status != metav1.ConditionTrue {
		t.Fatal("serving readiness and draining must coexist without changing publication time")
	}
	get(t, ctx, c, client.ObjectKeyFromObject(oldPod), oldPod)
	oldPod.Finalizers = nil
	if err := c.Update(ctx, oldPod); err != nil {
		t.Fatal(err)
	}
	current = step()
	if current.Status.Publication != nil {
		t.Fatal("publication did not complete after Pod cleanup")
	}
	current = step()
	if current.Status.CurrentSync == nil || !current.Status.CurrentSync.Manual {
		t.Fatal("pending manual request was lost during publication")
	}
}
