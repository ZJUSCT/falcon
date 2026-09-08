package controller

import (
	"context"
	"sort"

	snapshotv1 "github.com/kubernetes-csi/external-snapshotter/client/v8/apis/volumesnapshot/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	mirrorv1alpha1 "github.com/ZJUSCT/falcon/api/v1alpha1"
)

const volumeSnapshotKind = "VolumeSnapshot"

// Retention counts ready snapshots, including generations never cloned for
// publication. Live inputs and mounted clones are protected outside that window.
// Delete clones first; their source snapshots survive until all clones are gone.
func (r *MirrorReconciler) pruneOldSnapshots(ctx context.Context, m *mirrorv1alpha1.Mirror) error {
	labels := client.MatchingLabels{MirrorLabel: childBase(m.Name)}
	snapshots := &snapshotv1.VolumeSnapshotList{}
	if err := r.List(ctx, snapshots, client.InNamespace(m.Namespace), labels); err != nil {
		return err
	}
	claims := &corev1.PersistentVolumeClaimList{}
	if err := r.List(ctx, claims, client.InNamespace(m.Namespace)); err != nil {
		return err
	}
	pods := &corev1.PodList{}
	if err := r.List(ctx, pods, client.InNamespace(m.Namespace)); err != nil {
		return err
	}
	protected := map[string]bool{}
	if m.Status.LastSnapshot != nil {
		protected[m.Status.LastSnapshot.Name] = true
	}
	if m.Status.CurrentSync != nil {
		protected[currentSyncSnapshotName(m)] = true
	}
	if p := m.Status.Publication; p != nil {
		protected[p.Snapshot], protected[p.PVC] = true, true
	}
	if publishEnabled(m) {
		protected[m.Status.ActiveSnapshot], protected[m.Status.ActivePVC] = true, true
	}
	for _, pod := range pods.Items {
		for _, volume := range pod.Spec.Volumes {
			if volume.PersistentVolumeClaim != nil {
				protected[volume.PersistentVolumeClaim.ClaimName] = true
			}
		}
	}
	// Track all clone references, including external consumers and terminating
	// PVCs. Only controller-owned disposable clones may be deleted by retention.
	clones := map[string][]*corev1.PersistentVolumeClaim{}
	for i := range claims.Items {
		claim := &claims.Items[i]
		source := ""
		if ds := claim.Spec.DataSource; ds != nil && ds.Kind == volumeSnapshotKind && ds.APIGroup != nil && *ds.APIGroup == snapshotv1.GroupName {
			source = ds.Name
		}
		if ds := claim.Spec.DataSourceRef; ds != nil && ds.Kind == volumeSnapshotKind && ds.APIGroup != nil && *ds.APIGroup == snapshotv1.GroupName && (ds.Namespace == nil || *ds.Namespace == m.Namespace) {
			source = ds.Name
		}
		if source == "" {
			continue
		}
		clones[source] = append(clones[source], claim)
		if protected[claim.Name] || !metav1.IsControlledBy(claim, m) {
			protected[source] = true
		}
	}
	var ready []*snapshotv1.VolumeSnapshot
	existing := map[int64]bool{}
	for i := range snapshots.Items {
		snapshot := &snapshots.Items[i]
		ts, ok := objectTimestamp(snapshot.Labels)
		if !ok || !metav1.IsControlledBy(snapshot, m) {
			continue
		}
		existing[ts] = true
		if snapshot.DeletionTimestamp.IsZero() && snapshot.Status != nil && snapshot.Status.ReadyToUse != nil && *snapshot.Status.ReadyToUse {
			ready = append(ready, snapshot)
		}
	}
	sort.Slice(ready, func(i, j int) bool {
		a, _ := objectTimestamp(ready[i].Labels)
		b, _ := objectTimestamp(ready[j].Labels)
		return a > b
	})
	keep := max(1, int(m.Spec.Storage.Retention)+1)
	if len(ready) == 0 {
		return nil
	}
	floor, _ := objectTimestamp(ready[min(keep, len(ready))-1].Labels)
	for _, snapshot := range ready[:min(keep, len(ready))] {
		protected[snapshot.Name] = true
	}
	protectedTimes := map[int64]bool{}
	for _, snapshot := range snapshots.Items {
		if protected[snapshot.Name] {
			if ts, ok := objectTimestamp(snapshot.Labels); ok {
				protectedTimes[ts] = true
			}
		}
	}
	if m.Status.CurrentSync != nil {
		protectedTimes[currentSyncTimestamp(m)] = true
	}
	if m.Status.LastSnapshot != nil && m.Status.LastSnapshot.QueuedAt != nil {
		protectedTimes[m.Status.LastSnapshot.QueuedAt.Unix()] = true
	}
	if p := m.Status.Publication; p != nil && p.QueuedAt != nil {
		protectedTimes[p.QueuedAt.Unix()] = true
	}
	for _, snapshot := range ready {
		if protected[snapshot.Name] {
			continue
		}
		for _, claim := range clones[snapshot.Name] {
			if claim.DeletionTimestamp.IsZero() {
				if err := client.IgnoreNotFound(r.Delete(ctx, claim)); err != nil {
					return err
				}
			}
		}
		if len(clones[snapshot.Name]) != 0 {
			continue
		}
		if err := client.IgnoreNotFound(r.Delete(ctx, snapshot)); err != nil {
			return err
		}
	}
	jobs := &batchv1.JobList{}
	if err := r.List(ctx, jobs, client.InNamespace(m.Namespace), labels); err != nil {
		return err
	}
	for i := range jobs.Items {
		job := &jobs.Items[i]
		ts, ok := objectTimestamp(job.Labels)
		// Failed Jobs retain their independent keepFailedJobs policy. Keep the
		// successful Job until its old snapshot has actually disappeared.
		if !ok || ts >= floor || protectedTimes[ts] || existing[ts] || !metav1.IsControlledBy(job, m) || !jobSucceeded(job) {
			continue
		}
		policy := metav1.DeletePropagationBackground
		if err := client.IgnoreNotFound(r.Delete(ctx, job, &client.DeleteOptions{PropagationPolicy: &policy})); err != nil {
			return err
		}
	}
	return nil
}
