package controller

import (
	"testing"
	"time"

	snapshotv1 "github.com/kubernetes-csi/external-snapshotter/client/v8/apis/volumesnapshot/v1"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	mirrorv1alpha1 "github.com/ZJUSCT/falcon/api/v1alpha1"
)

func completeTestSyncSnapshot(t *testing.T, r *MirrorReconciler, m *mirrorv1alpha1.Mirror) {
	t.Helper()
	ready := true
	snapshot := &snapshotv1.VolumeSnapshot{
		ObjectMeta: metav1.ObjectMeta{Name: currentSyncSnapshotName(m), Namespace: m.Namespace},
		Status:     &snapshotv1.VolumeSnapshotStatus{ReadyToUse: &ready},
	}
	if err := r.Create(t.Context(), snapshot); err != nil {
		t.Fatal(err)
	}
	if _, err := r.reconcileSyncSnapshot(t.Context(), m, publicationHealth{}); err != nil {
		t.Fatal(err)
	}
}

func TestSyncOnlySnapshotCanBePublishedLaterWithoutAnotherJob(t *testing.T) {
	for _, paused := range []bool{false, true} {
		t.Run(map[bool]string{false: "scheduled", true: "manual"}[paused], func(t *testing.T) {
			ctx := t.Context()
			now := time.Unix(1789000000, 0)
			m := testMirror()
			publish := m.Spec.Publish
			m.Spec.Publish = mirrorv1alpha1.MirrorServicesSpec{}
			m.SetSyncPaused(paused)
			m.Finalizers = []string{MirrorFinalizer}
			scheme := testScheme(t)
			c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&mirrorv1alpha1.Mirror{}, &batchv1.Job{}, &snapshotv1.VolumeSnapshot{}, &appsv1.Deployment{}).WithObjects(m).Build()
			r := &MirrorReconciler{Client: c, Scheme: scheme, Config: testConfig(), SyncLimiter: NewSyncLimiter(1), Now: func() time.Time { return now }}
			if _, err := r.startSync(ctx, m, true, publicationHealth{}); err != nil {
				t.Fatal(err)
			}
			request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(m)}
			reconcile(t, ctx, r, request)
			job := &batchv1.Job{}
			get(t, ctx, c, client.ObjectKey{Namespace: m.Namespace, Name: currentSyncJobName(m)}, job)
			job.Status.StartTime = timePtr(now)
			job.Status.CompletionTime = timePtr(now.Add(time.Minute))
			job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
			if err := c.Status().Update(ctx, job); err != nil {
				t.Fatal(err)
			}
			now = now.Add(2 * time.Minute)
			reconcile(t, ctx, r, request)
			m = getMirror(t, ctx, c, request.NamespacedName)
			if m.Status.Sync.Phase != "Snapshotting" || m.Status.Publication != nil || m.Status.LastSnapshot != nil || m.Status.LastSync.Phase != "Succeeded" {
				t.Fatalf("premature handoff: %#v", m.Status)
			}
			if !r.SyncLimiter.Acquire("other-mirror", false) {
				t.Fatal("snapshot wait retained global Job slot")
			}
			if findCondition(m.Status.Conditions, "Progressing").Status != metav1.ConditionFalse {
				t.Fatal("snapshotting reported publication progress")
			}
			snapshot := &snapshotv1.VolumeSnapshot{}
			get(t, ctx, c, client.ObjectKey{Namespace: m.Namespace, Name: currentSyncSnapshotName(m)}, snapshot)
			ready := true
			snapshot.Status = &snapshotv1.VolumeSnapshotStatus{ReadyToUse: &ready}
			if err := c.Status().Update(ctx, snapshot); err != nil {
				t.Fatal(err)
			}
			reconcile(t, ctx, r, request)
			m = getMirror(t, ctx, c, request.NamespacedName)
			if m.Status.CurrentSync != nil || m.Status.Publication != nil || m.Status.LastSnapshot == nil || m.Status.LastSnapshot.Name != snapshot.Name || m.Status.ActivePVC != "" || m.Status.LastPublishedAt != nil {
				t.Fatalf("sync-only created a publication: %#v", m.Status)
			}
			assertNotFound(t, ctx, c, client.ObjectKeyFromObject(snapshot), &corev1.PersistentVolumeClaim{})
			assertNotFound(t, ctx, c, client.ObjectKey{Namespace: m.Namespace, Name: "smoke-publish-http"}, &appsv1.Deployment{})
			planned := m.Status.NextSyncAt.DeepCopy()
			acceptedHash := m.Status.LastAcceptedSpecHash
			m.Spec.Publish = publish
			m.Generation++
			if err := c.Update(ctx, m); err != nil {
				t.Fatal(err)
			}
			// A fresh reconciler must recover the snapshot without a Job slot.
			r = &MirrorReconciler{Client: c, Scheme: scheme, Config: testConfig(), SyncLimiter: NewSyncLimiter(1), Now: func() time.Time { return now }}
			reconcile(t, ctx, r, request)
			reconcile(t, ctx, r, request)
			m = getMirror(t, ctx, c, request.NamespacedName)
			if m.Status.CurrentSync != nil || m.Status.Publication == nil || m.Status.Publication.Snapshot != snapshot.Name || !m.Status.NextSyncAt.Equal(planned) || m.Status.LastAcceptedSpecHash != acceptedHash {
				t.Fatalf("publish edit triggered sync or lost retained snapshot: %#v", m.Status)
			}
			claim := &corev1.PersistentVolumeClaim{}
			get(t, ctx, c, client.ObjectKeyFromObject(snapshot), claim)
			if claim.Spec.DataSource.Name != snapshot.Name {
				t.Fatal("publication used a different snapshot")
			}
			jobs := &batchv1.JobList{}
			if err := c.List(ctx, jobs); err != nil || len(jobs.Items) != 1 {
				t.Fatalf("unexpected new sync Job: %v, %d", err, len(jobs.Items))
			}
			addBoundPublishPVC(t, ctx, c, m, snapshot.Name)
			markPublishDeploymentAvailable(t, ctx, c, m.Namespace)
			markRouteAccepted(t, ctx, c, m.Namespace, "smoke-publish")
			for i := 0; i < 4; i++ {
				reconcile(t, ctx, r, request)
			}
			m = getMirror(t, ctx, c, request.NamespacedName)
			if m.Status.CurrentSync != nil || m.Status.Publication != nil || m.Status.ActiveSnapshot != snapshot.Name {
				t.Fatalf("late publication did not converge independently: %#v", m.Status)
			}
		})
	}
}

func TestSyncConfigurationHashScope(t *testing.T) {
	m := testMirror()
	baseline, err := syncSpecHash(m)
	if err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*mirrorv1alpha1.Mirror){
		"pause":   func(m *mirrorv1alpha1.Mirror) { m.SetSyncPaused(!m.SyncPaused()) },
		"info":    func(m *mirrorv1alpha1.Mirror) { m.Spec.Info.CName = "another-name" },
		"publish": func(m *mirrorv1alpha1.Mirror) { m.Spec.Publish = mirrorv1alpha1.MirrorServicesSpec{} },
		"storage": func(m *mirrorv1alpha1.Mirror) { m.Spec.Storage.Retention++ },
	} {
		t.Run(name, func(t *testing.T) {
			changed := m.DeepCopy()
			mutate(changed)
			hash, err := syncSpecHash(changed)
			if err != nil || hash != baseline {
				t.Fatalf("unrelated configuration triggered sync: %v", err)
			}
		})
	}
	m.Spec.Sync.PodTemplate.Spec.Containers[0].Image = "new-sync-image"
	if hash, err := syncSpecHash(m); err != nil || hash == baseline {
		t.Fatalf("sync change was ignored: %v", err)
	}
}
