package controller

import (
	"context"
	"strings"
	"time"

	snapshotv1 "github.com/kubernetes-csi/external-snapshotter/client/v8/apis/volumesnapshot/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	mirrorv1alpha1 "github.com/ZJUSCT/falcon/api/v1alpha1"
)

const publicationRestoring = "Restoring"

func publicationSnapshotName(m *mirrorv1alpha1.Mirror) string {
	return m.Status.Publication.Snapshot
}

// Publication retains its handoff until the new generation serves and all old
// publish Pods are gone. Failures stay attached to this generation; they never
// consume sync retries or admit another writer to the sync PVC.
func (r *MirrorReconciler) reconcilePublication(ctx context.Context, m *mirrorv1alpha1.Mirror, health publicationHealth) (ctrl.Result, error) {
	p := m.Status.Publication
	if p.QueuedAt == nil || p.JobName == "" {
		return r.publicationWaiting(ctx, m, health, "PublicationInvalid", "publication identity is incomplete", true)
	}
	if !publishEnabled(m) {
		drained, err := publishPodsDrained(ctx, r.Client, m)
		if err != nil {
			return ctrl.Result{}, err
		}
		if !drained {
			return r.publicationWaiting(ctx, m, health, "PublishDraining", "waiting for removed publish Pods to disappear", false)
		}
		return r.patchStatusWithResult(ctx, m, ctrl.Result{RequeueAfter: time.Second}, func() {
			m.Status.Publication = nil
			health.progressing = false
			applyMirrorConditions(m, health, "", "", nil)
		})
	}
	// Publication never reconstructs its input from the mutable work PVC.
	// A missing or damaged delivered snapshot needs infrastructure repair.
	name := publicationSnapshotName(m)
	snapshot := &snapshotv1.VolumeSnapshot{}
	err := r.Get(ctx, client.ObjectKey{Namespace: m.Namespace, Name: name}, snapshot)
	if apierrors.IsNotFound(err) || name == "" {
		return r.publicationWaiting(ctx, m, health, "PublicationSnapshotMissing", "the delivered snapshot is missing", true)
	}
	if err != nil {
		return ctrl.Result{}, err
	}
	if snapshot.Status != nil && snapshot.Status.Error != nil {
		message := "the delivered snapshot reports an error"
		if snapshot.Status.Error.Message != nil {
			message = *snapshot.Status.Error.Message
		}
		return r.publicationWaiting(ctx, m, health, "PublicationSnapshotFailed", message, true)
	}
	if !snapshot.DeletionTimestamp.IsZero() || snapshot.Status == nil || snapshot.Status.ReadyToUse == nil || !*snapshot.Status.ReadyToUse {
		return r.publicationWaiting(ctx, m, health, "PublicationSnapshotNotReady", "the delivered snapshot is no longer ready", true)
	}
	if err := r.ensurePublishPVC(ctx, m); err != nil {
		return ctrl.Result{}, err
	}
	workloadsReady := true
	if publishEnabled(m) {
		workloadsReady, err = r.ensurePublish(ctx, m, name)
		if err != nil {
			return ctrl.Result{}, err
		}
	}
	claim := &corev1.PersistentVolumeClaim{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: m.Namespace, Name: name}, claim); err != nil {
		return ctrl.Result{}, err
	}
	if claim.Status.Phase == corev1.ClaimLost {
		return r.publicationWaiting(ctx, m, health, "PublishVolumeLost", "publish PVC lost its volume", true)
	}
	if claim.Spec.VolumeName == "" || !claim.DeletionTimestamp.IsZero() {
		return r.publicationWaiting(ctx, m, health, publicationRestoring, "waiting for publish PVC binding", false)
	}
	if p.PVC == "" {
		if _, err := r.patchStatus(ctx, m, func() { p.PVC = name; p.Phase = "RollingOut" }); err != nil {
			return ctrl.Result{}, err
		}
	}
	// Re-observe after creating/updating children; on the first publication there
	// is no previous active generation from which to infer route availability.
	health, err = r.reconcileActivePublication(ctx, m)
	if err != nil {
		return ctrl.Result{}, err
	}
	if health.failure != nil {
		return r.publicationWaiting(ctx, m, health, health.failure.reason, health.failure.message, true)
	}
	if !workloadsReady || (publishHTTPEnabled(m) && !health.ready) {
		return r.publicationWaiting(ctx, m, health, "PublishRollout", "waiting for publish workloads and HTTPRoute readiness", false)
	}
	if p.Phase != "Draining" {
		usage, ok := r.publishPVCUsage(ctx, m, name)
		return r.patchStatusWithResult(ctx, m, ctrl.Result{RequeueAfter: time.Second}, func() {
			m.Status.ActiveSnapshot, m.Status.ActivePVC = p.Snapshot, p.PVC
			m.Status.LastPublishedAt = timePtr(r.now())
			m.Status.SizeBytes = 0
			if ok {
				m.Status.SizeBytes = usage
			}
			p.Phase = "Draining"
			health.progressing = true
			health.reason, health.message = "PublishDraining", "new generation is ready; waiting for old publish Pods to disappear"
			applyMirrorConditions(m, health, "", "", nil)
		})
	}
	drained, err := publishPodsDrained(ctx, r.Client, m)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !drained {
		return r.publicationWaiting(ctx, m, health, "PublishDraining", "waiting for old publish Pods to disappear", false)
	}
	return r.patchStatusWithResult(ctx, m, ctrl.Result{RequeueAfter: time.Second}, func() {
		m.Status.Publication = nil
		health.progressing = false
		applyMirrorConditions(m, health, "Published", "publication and old-Pod cleanup completed", nil)
	})
}

func (r *MirrorReconciler) publicationWaiting(ctx context.Context, m *mirrorv1alpha1.Mirror, health publicationHealth, reason, message string, failed bool) (ctrl.Result, error) {
	health.progressing = true
	health.reason, health.message = reason, message
	var failure *conditionFailure
	if failed {
		failure = &conditionFailure{reason: reason, message: message}
	}
	return r.patchStatusWithResult(ctx, m, ctrl.Result{RequeueAfter: 5 * time.Second}, func() {
		applyMirrorConditions(m, health, reason, message, failure)
	})
}

// Deployment counters intentionally exclude terminating Pods. Inspect the owned
// ReplicaSets and Pods too, so graceful shutdown (including stuck kubelets) is a
// real part of publication rather than hidden behind a completed rollout.
func publishPodsDrained(ctx context.Context, c client.Client, owner client.Object) (bool, error) {
	deployments := &appsv1.DeploymentList{}
	if err := c.List(ctx, deployments, client.InNamespace(owner.GetNamespace()), client.MatchingLabels{MirrorLabel: owner.GetName()}); err != nil {
		return false, err
	}
	sets := &appsv1.ReplicaSetList{}
	if err := c.List(ctx, sets, client.InNamespace(owner.GetNamespace()), client.MatchingLabels{MirrorLabel: owner.GetName()}); err != nil {
		return false, err
	}
	pods := &corev1.PodList{}
	if err := c.List(ctx, pods, client.InNamespace(owner.GetNamespace()), client.MatchingLabels{MirrorLabel: owner.GetName()}); err != nil {
		return false, err
	}
	oldSets := map[types.UID]bool{}
	existingSets := map[types.UID]bool{}
	for _, set := range sets.Items {
		existingSets[set.UID] = true
	}
	for _, deployment := range deployments.Items {
		if !metav1.IsControlledBy(&deployment, owner) {
			continue
		}
		if !deployment.DeletionTimestamp.IsZero() {
			return false, nil
		}
		for _, set := range sets.Items {
			if !metav1.IsControlledBy(&set, &deployment) {
				continue
			}
			template := set.Spec.Template.DeepCopy()
			delete(template.Labels, appsv1.DefaultDeploymentUniqueLabelKey)
			oldSets[set.UID] = !apiequality.Semantic.DeepEqual(*template, deployment.Spec.Template)
		}
	}
	for _, pod := range pods.Items {
		parent := metav1.GetControllerOf(&pod)
		if parent == nil {
			continue
		}
		if old, owned := oldSets[parent.UID]; owned && (old || !pod.DeletionTimestamp.IsZero()) {
			return false, nil
		}
		// Background GC may remove a ReplicaSet before its Pods disappear.
		// Retain the barrier for orphaned publication Pods whose parent identity
		// belongs to a generated Deployment; missing ancestry cannot prove cleanup.
		if parent.Kind == "ReplicaSet" && !existingSets[parent.UID] {
			for _, protocol := range []string{PublishProtocolHTTP, PublishProtocolRsync} {
				if strings.HasPrefix(parent.Name, publishChildName(owner.GetName(), protocol)+"-") {
					return false, nil
				}
			}
		}
	}
	return true, nil
}

func (r *MirrorReconciler) updateSyncPhase(m *mirrorv1alpha1.Mirror) {
	if current := m.Status.CurrentSync; current != nil {
		switch {
		case current.Phase == mirrorv1alpha1.SyncPhaseSnapshotting:
			m.Status.Sync.Phase = mirrorv1alpha1.SyncStateSnapshotting
		case current.Phase == mirrorv1alpha1.SyncPhaseCancelling:
			m.Status.Sync.Phase = mirrorv1alpha1.SyncStateCancelling
		case current.StartedAt != nil:
			m.Status.Sync.Phase = mirrorv1alpha1.SyncStateSyncing
		default:
			m.Status.Sync.Phase = mirrorv1alpha1.SyncStatePending
		}
		return
	}
	if m.SyncRequested() && m.Status.RequestCleanup == nil {
		m.Status.Sync.Phase = mirrorv1alpha1.SyncStatePending
		return
	}
	if !m.SyncPaused() && m.Status.NextSyncAt != nil && !m.Status.NextSyncAt.After(r.now()) {
		m.Status.Sync.Phase = mirrorv1alpha1.SyncStatePending
		return
	}
	if m.Status.Sync.Phase == mirrorv1alpha1.SyncStateRetrying && m.Status.NextSyncAt != nil {
		return
	}
	m.Status.Sync.Phase = mirrorv1alpha1.SyncStateWaiting
}
