package controller

import (
	"fmt"
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
)

func retentionSnapshot(m *mirrorv1alpha1.Mirror, ts int64) *snapshotv1.VolumeSnapshot {
	ready := true
	return &snapshotv1.VolumeSnapshot{
		ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("%s-snap-%d", m.Name, ts), Namespace: m.Namespace, Labels: childLabels(m, ts, "snapshot"), OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(m, mirrorv1alpha1.GroupVersion.WithKind("Mirror"))}},
		Status:     &snapshotv1.VolumeSnapshotStatus{ReadyToUse: &ready},
	}
}

func TestRetentionCountsReadySnapshotsAndProtectsLiveGenerations(t *testing.T) {
	for _, protect := range []string{"none", "active", "handoff", "latest", "pod", "foreign-clone"} {
		t.Run(protect, func(t *testing.T) {
			m := testMirror()
			m.UID = "mirror-uid"
			m.Spec.Storage.Retention = 1
			old, previous, latest := retentionSnapshot(m, 100), retentionSnapshot(m, 200), retentionSnapshot(m, 300)
			unready := retentionSnapshot(m, 400)
			no := false
			unready.Status.ReadyToUse = &no
			foreign := retentionSnapshot(m, 50)
			foreign.OwnerReferences = nil
			objects := []client.Object{m, old, previous, latest, unready, foreign}
			switch protect {
			case "active":
				m.Status.ActiveSnapshot = old.Name
			case "handoff":
				m.Status.Publication = &mirrorv1alpha1.MirrorPublicationStatus{Snapshot: old.Name, QueuedAt: timePtr(time.Unix(100, 0))}
			case "latest":
				m.Status.LastSnapshot = &mirrorv1alpha1.MirrorSnapshotStatus{Name: old.Name, QueuedAt: timePtr(time.Unix(100, 0))}
			case "pod", "foreign-clone":
				claim := newDataClaim(m, old.Name, 100, "publish-data")
				claim.OwnerReferences = old.OwnerReferences
				claim.Spec.DataSource = &corev1.TypedLocalObjectReference{APIGroup: stringPtr(snapshotv1.GroupName), Kind: "VolumeSnapshot", Name: old.Name}
				if protect == "pod" {
					objects = append(objects, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "draining", Namespace: m.Namespace}, Spec: corev1.PodSpec{Volumes: []corev1.Volume{{Name: "data", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: claim.Name}}}}}})
				} else {
					claim.OwnerReferences = nil
				}
				objects = append(objects, claim)
			}
			c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objects...).Build()
			r := &MirrorReconciler{Client: c}
			if err := r.pruneOldSnapshots(t.Context(), m); err != nil {
				t.Fatal(err)
			}
			for _, snap := range []*snapshotv1.VolumeSnapshot{previous, latest, unready, foreign} {
				get(t, t.Context(), c, client.ObjectKeyFromObject(snap), &snapshotv1.VolumeSnapshot{})
			}
			if protect == "none" {
				assertNotFound(t, t.Context(), c, client.ObjectKeyFromObject(old), &snapshotv1.VolumeSnapshot{})
			} else {
				get(t, t.Context(), c, client.ObjectKeyFromObject(old), &snapshotv1.VolumeSnapshot{})
			}
		})
	}
}

func TestRetentionWaitsForCloneDeletionAndNeverPrunesJobs(t *testing.T) {
	m := testMirror()
	m.UID = "mirror-uid"
	m.Spec.Storage.Retention = 1
	old, previous, latest := retentionSnapshot(m, 100), retentionSnapshot(m, 200), retentionSnapshot(m, 300)
	claim := newDataClaim(m, old.Name, 100, "publish-data")
	claim.OwnerReferences = old.OwnerReferences
	claim.Finalizers = []string{"test/storage-cleanup"}
	claim.Spec.DataSource = &corev1.TypedLocalObjectReference{APIGroup: stringPtr(snapshotv1.GroupName), Kind: "VolumeSnapshot", Name: old.Name}
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "old-job", Namespace: m.Namespace, Labels: childLabels(m, 100, "sync"), OwnerReferences: old.OwnerReferences}, Status: batchv1.JobStatus{Conditions: []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}}}
	failed := job.DeepCopy()
	failed.Name = "failed-job"
	failed.Status.Conditions[0].Type = batchv1.JobFailed
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(m, old, previous, latest, claim, job, failed).Build()
	r := &MirrorReconciler{Client: c}
	for i := 0; i < 2; i++ {
		if err := r.pruneOldSnapshots(t.Context(), m); err != nil {
			t.Fatal(err)
		}
	}
	get(t, t.Context(), c, client.ObjectKeyFromObject(old), &snapshotv1.VolumeSnapshot{})
	get(t, t.Context(), c, client.ObjectKeyFromObject(job), &batchv1.Job{})
	get(t, t.Context(), c, client.ObjectKeyFromObject(claim), claim)
	if claim.DeletionTimestamp.IsZero() {
		t.Fatal("old clone was not requested for deletion")
	}
	claim.Finalizers = nil
	if err := c.Update(t.Context(), claim); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := r.pruneOldSnapshots(t.Context(), m); err != nil {
			t.Fatal(err)
		}
	}
	assertNotFound(t, t.Context(), c, client.ObjectKeyFromObject(old), &snapshotv1.VolumeSnapshot{})
	// Jobs are synchronization history and outlive their snapshot generation:
	// snapshot retention never deletes them (pruneJobs owns that policy).
	get(t, t.Context(), c, client.ObjectKeyFromObject(job), &batchv1.Job{})
	get(t, t.Context(), c, client.ObjectKeyFromObject(failed), &batchv1.Job{})
}

func TestSyncOnlyRetentionRunsWhilePaused(t *testing.T) {
	m := testMirror()
	m.UID = "mirror-uid"
	m.Finalizers = []string{MirrorFinalizer}
	m.Spec.Sync.Paused = true
	m.Spec.Publish = mirrorv1alpha1.MirrorServicesSpec{}
	m.Spec.Storage.Retention = 1
	old, previous, latest := retentionSnapshot(m, 100), retentionSnapshot(m, 200), retentionSnapshot(m, 300)
	m.Status.LastSnapshot = &mirrorv1alpha1.MirrorSnapshotStatus{Name: latest.Name, QueuedAt: timePtr(time.Unix(300, 0))}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithStatusSubresource(&mirrorv1alpha1.Mirror{}).WithObjects(m, old, previous, latest).Build()
	r := &MirrorReconciler{Client: c, Scheme: testScheme(t), Config: testConfig()}
	reconcile(t, t.Context(), r, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(m)})
	assertNotFound(t, t.Context(), c, client.ObjectKeyFromObject(old), &snapshotv1.VolumeSnapshot{})
	get(t, t.Context(), c, client.ObjectKeyFromObject(previous), &snapshotv1.VolumeSnapshot{})
	current := getMirror(t, t.Context(), c, client.ObjectKeyFromObject(m))
	if current.Status.CurrentSync != nil || current.Status.LastSnapshot.Name != latest.Name {
		t.Fatal("paused cleanup started synchronization or lost the latest snapshot")
	}
}
