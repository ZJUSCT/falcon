package controller

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	snapshotv1 "github.com/kubernetes-csi/external-snapshotter/client/v8/apis/volumesnapshot/v1"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	mirrorv1alpha1 "github.com/ZJUSCT/falcon/api/v1alpha1"
)

// errSyncQueued signals that the pending sync Job was not created because the
// global sync.maxConcurrent cap is reached; the caller persists a queued
// status and requeues.
var errSyncQueued = errors.New("sync queued: global concurrency limit reached")

// errSnapshotTimestampConflict signals that the Unix seconds timestamp
// allocated when the sync task was created is already taken by an existing
// Job/PVC/VolumeSnapshot (a same-second leftover). The caller persists a
// Degraded condition with reason SnapshotTimestampConflict and retries after
// a minute.
var errSnapshotTimestampConflict = errors.New("snapshot timestamp conflict")

const (
	// SyncDataVolumeName is the reserved name of the WRITABLE sync PVC volume
	// the controller injects into every sync pod's spec.volumes. Users must
	// not declare a volume of this name themselves; mounting it, and where,
	// is the user's own declaration. Unlike the publish-side mirror-data it
	// has no read-only constraint — it is the sync Job's output volume.
	SyncDataVolumeName = "sync-data"
	// PublishDataVolumeName is the reserved name of the read-only publish PVC
	// volume the controller injects into every Mirror publish pod (a
	// read-only volume source in spec.volumes). Users must not declare a
	// volume of this name themselves, and any mount of it — declared entirely
	// by the user — must be read-only.
	PublishDataVolumeName = "mirror-data"
	// PublishTmpVolumeName is the name of the default /tmp emptyDir the
	// controller injects into a publish pod only when the user template does
	// not declare a volume of this name and does not mount /tmp itself.
	PublishTmpVolumeName = "tmp"
)

func (r *MirrorReconciler) ensureSnapshot(ctx context.Context, mirror *mirrorv1alpha1.Mirror) (bool, string, error) {
	snapshotName := currentSyncSnapshotName(mirror)
	timestamp := currentSyncTimestamp(mirror)
	key := types.NamespacedName{Namespace: mirror.Namespace, Name: snapshotName}
	snapshot := &snapshotv1.VolumeSnapshot{}
	if err := r.Get(ctx, key, snapshot); err != nil {
		if !apierrors.IsNotFound(err) {
			return false, "", err
		}
		labels, err := childLabels(mirror, timestamp, "snapshot")
		if err != nil {
			return false, "", err
		}
		snapshot = &snapshotv1.VolumeSnapshot{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: mirror.Namespace,
				Name:      snapshotName,
				Labels:    labels,
			},
			Spec: snapshotv1.VolumeSnapshotSpec{
				Source:                  snapshotv1.VolumeSnapshotSource{PersistentVolumeClaimName: stringPtr(mirror.Status.WorkPVC)},
				VolumeSnapshotClassName: stringPtr(mirror.Spec.Storage.VolumeSnapshotClassName),
			},
		}
		if err := controllerutil.SetControllerReference(mirror, snapshot, r.Scheme); err != nil {
			return false, "", err
		}
		if err := r.Create(ctx, snapshot); err != nil && !apierrors.IsAlreadyExists(err) {
			return false, "", err
		}
		return false, "", nil
	}
	if snapshot.Status != nil && snapshot.Status.Error != nil {
		message := "the CSI snapshot controller reported an error"
		if snapshot.Status.Error.Message != nil {
			message = *snapshot.Status.Error.Message
		}
		return false, message, nil
	}
	return snapshot.Status != nil && snapshot.Status.ReadyToUse != nil && *snapshot.Status.ReadyToUse, "", nil
}

func (r *MirrorReconciler) ensureSyncPVC(ctx context.Context, mirror *mirrorv1alpha1.Mirror) error {
	key := types.NamespacedName{Namespace: mirror.Namespace, Name: mirror.Status.WorkPVC}
	claim := &corev1.PersistentVolumeClaim{}
	if err := r.Get(ctx, key, claim); err == nil {
		if !claim.DeletionTimestamp.IsZero() {
			return fmt.Errorf("sync PVC %s is still terminating", claim.Name)
		}
		current := claim.Spec.Resources.Requests[corev1.ResourceStorage]
		if current.Cmp(mirror.Spec.Storage.Capacity) < 0 {
			before := claim.DeepCopy()
			claim.Spec.Resources.Requests[corev1.ResourceStorage] = mirror.Spec.Storage.Capacity.DeepCopy()
			return r.Patch(ctx, claim, client.MergeFrom(before))
		}
		return nil
	} else if !apierrors.IsNotFound(err) {
		return err
	}

	claim, err := newDataClaim(mirror, mirror.Status.WorkPVC, 0, "sync")
	if err != nil {
		return err
	}
	if err := controllerutil.SetControllerReference(mirror, claim, r.Scheme); err != nil {
		return err
	}
	if err := r.Create(ctx, claim); err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}
	return nil
}

func (r *MirrorReconciler) ensurePublishPVC(ctx context.Context, mirror *mirrorv1alpha1.Mirror) error {
	snapshotName := currentSyncSnapshotName(mirror)
	timestamp := currentSyncTimestamp(mirror)
	key := types.NamespacedName{Namespace: mirror.Namespace, Name: snapshotName}
	claim := &corev1.PersistentVolumeClaim{}
	if err := r.Get(ctx, key, claim); err == nil {
		if claim.DeletionTimestamp.IsZero() {
			return nil
		}
		return fmt.Errorf("publish PVC %s is still terminating", claim.Name)
	} else if !apierrors.IsNotFound(err) {
		return err
	}

	claim, err := newDataClaim(mirror, snapshotName, timestamp, "publish-data")
	if err != nil {
		return err
	}
	claim.Spec.DataSource = &corev1.TypedLocalObjectReference{
		APIGroup: stringPtr(snapshotv1.GroupName),
		Kind:     "VolumeSnapshot",
		Name:     snapshotName,
	}
	if err := controllerutil.SetControllerReference(mirror, claim, r.Scheme); err != nil {
		return err
	}
	if err := r.Create(ctx, claim); err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}
	return nil
}

// newDataClaim builds a PVC. syncTimestamp is the Unix seconds timestamp
// embedded in snapshot-derived PVC names; 0 means the claim is not
// snapshot-scoped (the stable sync PVC).
func newDataClaim(mirror *mirrorv1alpha1.Mirror, name string, syncTimestamp int64, role string) (*corev1.PersistentVolumeClaim, error) {
	labels, err := childLabels(mirror, syncTimestamp, role)
	if err != nil {
		return nil, err
	}
	accessMode := mirror.Spec.Storage.AccessMode
	if accessMode == "" {
		accessMode = corev1.ReadWriteOnce
	}
	storageClassName := mirror.Spec.Storage.StorageClassName
	if role == "publish-data" && mirror.Spec.Storage.PublishStorageClassName != "" {
		storageClassName = mirror.Spec.Storage.PublishStorageClassName
	}
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: mirror.Namespace,
			Name:      name,
			Labels:    labels,
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      []corev1.PersistentVolumeAccessMode{accessMode},
			StorageClassName: stringPtr(storageClassName),
			Resources: corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{
				corev1.ResourceStorage: mirror.Spec.Storage.Capacity.DeepCopy(),
			}},
		},
	}, nil
}

// lookupOrCreateSyncJob returns the pending sync Job, creating it if it does
// not exist yet. Creation is gated by the SyncLimiter: when the global
// sync.maxConcurrent cap is reached the Job is not created and a "queued"
// status with SyncQueued condition is persisted; the next reconciles retry
// until a running Job terminates and frees its slot. A queued sync therefore
// may start later than status.nextSyncAt.
//
// Creation is also gated by the timestamp check: the Job's name and the
// sync-timestamp label carry the Unix seconds timestamp allocated at sync
// task creation, and a leftover Job/PVC/VolumeSnapshot already carrying that
// timestamp is a same-second collision — the Job is not created and
// errSnapshotTimestampConflict is returned instead (Degraded + RetryAfter 1m
// upstream, pending pipeline preserved).
func (r *MirrorReconciler) lookupOrCreateSyncJob(ctx context.Context, mirror *mirrorv1alpha1.Mirror) (*batchv1.Job, error) {
	jobName := currentSyncJobName(mirror)
	key := types.NamespacedName{Namespace: mirror.Namespace, Name: jobName}
	job := &batchv1.Job{}
	err := r.Get(ctx, key, job)
	switch {
	case apierrors.IsNotFound(err):
		if !r.SyncLimiter.Acquire(jobName, false) {
			return nil, errSyncQueued
		}
		if err := r.checkSyncTimestampConflict(ctx, mirror); err != nil {
			r.SyncLimiter.Release(jobName)
			return nil, err
		}
		if err := r.createSyncJob(ctx, mirror); err != nil {
			r.SyncLimiter.Release(jobName)
			return nil, err
		}
		if err := r.Get(ctx, key, job); err != nil {
			return nil, err
		}
		return job, nil
	case err != nil:
		return nil, err
	}
	return job, nil
}

// checkSyncTimestampConflict reports whether the pending sync timestamp is
// already taken in this namespace: any Job/PVC/VolumeSnapshot carrying the
// sync-timestamp label value (or matching the derived `<base>-sync-<ts>` /
// `<base>-snap-<ts>` names without the label) is a same-second collision.
// Like the rest of the pipeline it is an error, not a silent shift to the
// next second.
func (r *MirrorReconciler) checkSyncTimestampConflict(ctx context.Context, mirror *mirrorv1alpha1.Mirror) error {
	ts := currentSyncTimestamp(mirror)
	base := childBase(mirror.Name)
	labels := client.MatchingLabels{MirrorLabel: base}

	claims := &corev1.PersistentVolumeClaimList{}
	if err := r.List(ctx, claims, client.InNamespace(mirror.Namespace), labels); err != nil {
		return err
	}
	for i := range claims.Items {
		if existing, ok := objectTimestamp(claims.Items[i].Labels); ok && existing == ts {
			return fmt.Errorf("%w: a PVC already carries sync-timestamp %d (%s)", errSnapshotTimestampConflict, ts, claims.Items[i].Name)
		}
	}
	snapshots := &snapshotv1.VolumeSnapshotList{}
	if err := r.List(ctx, snapshots, client.InNamespace(mirror.Namespace), labels); err != nil {
		return err
	}
	for i := range snapshots.Items {
		if existing, ok := objectTimestamp(snapshots.Items[i].Labels); ok && existing == ts {
			return fmt.Errorf("%w: a VolumeSnapshot already carries sync-timestamp %d (%s)", errSnapshotTimestampConflict, ts, snapshots.Items[i].Name)
		}
	}
	jobs := &batchv1.JobList{}
	if err := r.List(ctx, jobs, client.InNamespace(mirror.Namespace), labels); err != nil {
		return err
	}
	for i := range jobs.Items {
		if existing, ok := objectTimestamp(jobs.Items[i].Labels); ok && existing == ts {
			return fmt.Errorf("%w: a Job already carries sync-timestamp %d (%s)", errSnapshotTimestampConflict, ts, jobs.Items[i].Name)
		}
	}
	// Cover leftover objects that exist without the sync-timestamp label.
	snapshotName := resourceName(base, fmt.Sprintf("snap-%d", ts))
	snapKey := types.NamespacedName{Namespace: mirror.Namespace, Name: snapshotName}
	if err := r.Get(ctx, snapKey, &snapshotv1.VolumeSnapshot{}); err == nil {
		return fmt.Errorf("%w: VolumeSnapshot %s already exists at timestamp %d", errSnapshotTimestampConflict, snapshotName, ts)
	} else if !apierrors.IsNotFound(err) {
		return err
	}
	pvcKey := types.NamespacedName{Namespace: mirror.Namespace, Name: snapshotName}
	if err := r.Get(ctx, pvcKey, &corev1.PersistentVolumeClaim{}); err == nil {
		return fmt.Errorf("%w: PVC %s already exists at timestamp %d", errSnapshotTimestampConflict, snapshotName, ts)
	} else if !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

// createSyncJob builds and creates the pending sync Job (which must not exist
// yet; an AlreadyExists error is re-raised for the caller to re-read).
//
// The pod template is the user's spec.sync.podTemplate with
//
//   - forced sync pipeline identity: the WRITABLE `sync-data` PVC volume
//     injected into spec.volumes (mounting it, and where, is the user's own
//     declaration), restartPolicy Never, terminationGracePeriodSeconds 30,
//     the sync labels;
//   - defaults injected only where the template is silent (see
//     applySyncPodDefaults).
//
// Job-level: backoffLimit 0 and activeDeadlineSeconds = spec.sync.timeout.
// Placement is NOT injected (see the spec comment inside).
func (r *MirrorReconciler) createSyncJob(ctx context.Context, mirror *mirrorv1alpha1.Mirror) error {
	deadline := int64(mirror.Spec.Sync.Timeout.Duration.Seconds())
	if deadline < 1 {
		deadline = 1
	}
	backoffLimit := int32(0)
	terminationGrace := int64(30)

	// The Job carries the sync-timestamp label from creation: it embeds the
	// same Unix seconds timestamp as its name and is the identity the
	// snapshot and publish PVC reuse after success.
	labels, err := childLabels(mirror, currentSyncTimestamp(mirror), "sync")
	if err != nil {
		return err
	}

	template := mirror.Spec.Sync.PodTemplate.DeepCopy()
	if template.Labels == nil {
		template.Labels = map[string]string{}
	}
	for label, value := range labels {
		template.Labels[label] = value
	}
	spec := &template.Spec
	// Pipeline identity fields are controller-owned. Placement is NOT: sync
	// pods reference the sync PVC, so the scheduler handles volume locality
	// natively (WaitForFirstConsumer on first supply, then the bound PV's
	// nodeAffinity pins every later sync pod). pod.spec.nodeName stays unset —
	// it would bypass the scheduler and break WaitForFirstConsumer binding.
	spec.RestartPolicy = corev1.RestartPolicyNever
	spec.TerminationGracePeriodSeconds = &terminationGrace

	applySyncPodDefaults(spec)

	// The writable sync data volume is forced, never optional — as a VOLUME
	// only: mounting it, and where, is the user's own declaration in the pod
	// template. Like the publish-side mirror-data the volume name is
	// reserved; unlike it there is no read-only constraint (it is the sync
	// Job's output volume).
	spec.Volumes = append(spec.Volumes, corev1.Volume{
		Name: SyncDataVolumeName,
		VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
			ClaimName: mirror.Status.WorkPVC,
		}},
	})

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: mirror.Namespace,
			Name:      currentSyncJobName(mirror),
			Labels:    labels,
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:          &backoffLimit,
			ActiveDeadlineSeconds: &deadline,
			Template:              *template,
		},
	}
	if err := controllerutil.SetControllerReference(mirror, job, r.Scheme); err != nil {
		return err
	}
	return r.Create(ctx, job)
}

// applySyncPodDefaults injects the overridable sync defaults into a
// user-provided pod template: values are only filled in where the template is
// silent (nil fields / absent entries), so any explicit user setting wins.
//
//   - runAsUser 65532 (the mirror-scoped uid the sync data dirs are writable
//     by) on top of the shared restricted-profile defaults;
//   - first container: imagePullPolicy IfNotPresent when unset;
//   - a `/tmp` emptyDir volume + mount on the first container — ftpsync-style
//     scripts expect HOME=/tmp to stay writable, which the emptyDir guarantees
//     under the readOnlyRootFilesystem default.
//
// No probes are injected: a sync container has no service port to probe.
func applySyncPodDefaults(spec *corev1.PodSpec) {
	applyPodSecurityAndTmpDefaults(spec)
	if spec.SecurityContext != nil && spec.SecurityContext.RunAsUser == nil {
		spec.SecurityContext.RunAsUser = ptr.To(int64(65532))
	}
	if len(spec.Containers) > 0 && spec.Containers[0].ImagePullPolicy == "" {
		spec.Containers[0].ImagePullPolicy = corev1.PullIfNotPresent
	}
}

// ensurePublish maintains the Deployment and Service of every ENABLED
// spec.services key (a present key) for the given claim. It reports readiness
// across all enabled services.
//
// Node placement is DERIVED, not configured: before touching any Deployment
// the source PV of the sync PVC is read and its nodeAffinity turned into a
// pod constraint (see publishPlacement). While that derivation cannot be made
// (PVC/PV not bound yet — abnormal timing) no Deployment is created at all:
// publish pods must never run without their volume locality constraint.
func (r *MirrorReconciler) ensurePublish(ctx context.Context, mirror *mirrorv1alpha1.Mirror, claimName string) (bool, error) {
	nodeSelector, affinity, determined, err := r.publishPlacement(ctx, mirror)
	if err != nil {
		return false, err
	}
	if !determined {
		if r.Recorder != nil {
			r.Recorder.Eventf(mirror, corev1.EventTypeWarning, "PublishPlacementPending",
				"Publish workload deferred: node constraint cannot be derived yet (sync PVC/PV not bound); retrying")
		}
		return false, nil
	}
	ready := true
	services := mirror.Spec.Services
	for _, entry := range []struct {
		key  string
		spec *mirrorv1alpha1.MirrorServiceSpec
	}{
		{PublishProtocolHTTP, httpServiceSpec(services)},
		{PublishProtocolRsync, services.Rsync},
	} {
		if entry.spec == nil {
			continue
		}
		ok, err := r.ensurePublishEntry(ctx, mirror, entry.key, entry.spec, claimName, nodeSelector, affinity)
		if err != nil {
			return false, err
		}
		if !ok {
			ready = false
		}
	}
	return ready, nil
}

// observePublishChildren reports whether every requested service has a
// currently available pod and whether every Deployment has converged to its
// current generation. It never mutates children, which is essential while a
// newer PVC is rolling out: reconciling the old ActivePVC at that point would
// undo the in-flight publication.
func observePublishChildren(ctx context.Context, c client.Client, mirror *mirrorv1alpha1.Mirror) (available, converged bool, err error) {
	available = true
	converged = true
	base := childBase(mirror.Name)
	entries := make([]struct {
		key      string
		replicas int32
	}, 0, 2)
	if mirror.Spec.Services.HTTP != nil {
		entries = append(entries, struct {
			key      string
			replicas int32
		}{PublishProtocolHTTP, replicasOrDefault(mirror.Spec.Services.HTTP.Replicas)})
	}
	if mirror.Spec.Services.Rsync != nil {
		entries = append(entries, struct {
			key      string
			replicas int32
		}{PublishProtocolRsync, replicasOrDefault(mirror.Spec.Services.Rsync.Replicas)})
	}
	for _, entry := range entries {
		name := publishChildName(base, entry.key)
		service := &corev1.Service{}
		if err := c.Get(ctx, types.NamespacedName{Namespace: mirror.Namespace, Name: name}, service); err != nil {
			if apierrors.IsNotFound(err) {
				available, converged = false, false
				continue
			}
			return false, false, err
		}
		if !metav1.IsControlledBy(service, mirror) {
			available, converged = false, false
			continue
		}

		deployment := &appsv1.Deployment{}
		if err := c.Get(ctx, types.NamespacedName{Namespace: mirror.Namespace, Name: name}, deployment); err != nil {
			if apierrors.IsNotFound(err) {
				available, converged = false, false
				continue
			}
			return false, false, err
		}
		if !metav1.IsControlledBy(deployment, mirror) || deployment.Status.AvailableReplicas == 0 {
			available = false
		}
		if deployment.Status.ObservedGeneration != deployment.Generation || deployment.Status.UpdatedReplicas < entry.replicas || deployment.Status.AvailableReplicas < entry.replicas {
			converged = false
		}
	}
	return available, converged, nil
}

// deletePublishEntry removes the deterministic Deployment and Service for a
// service key that is no longer present in spec.services.
func deletePublishEntry(ctx context.Context, c client.Client, owner client.Object, serviceKey string) error {
	name := publishChildName(childBase(owner.GetName()), serviceKey)
	objects := []client.Object{
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: owner.GetNamespace(), Name: name}},
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: owner.GetNamespace(), Name: name}},
	}
	for _, object := range objects {
		if err := c.Get(ctx, client.ObjectKeyFromObject(object), object); apierrors.IsNotFound(err) {
			continue
		} else if err != nil {
			return err
		}
		if metav1.IsControlledBy(object, owner) {
			if err := c.Delete(ctx, object); err != nil && !apierrors.IsNotFound(err) {
				return err
			}
		}
	}
	return nil
}

func (r *MirrorReconciler) cleanupDisabledPublishChildren(ctx context.Context, mirror *mirrorv1alpha1.Mirror) error {
	if mirror.Spec.Services.HTTP == nil {
		if err := deletePublishEntry(ctx, r.Client, mirror, PublishProtocolHTTP); err != nil {
			return err
		}
	}
	if mirror.Spec.Services.Rsync == nil {
		if err := deletePublishEntry(ctx, r.Client, mirror, PublishProtocolRsync); err != nil {
			return err
		}
	}
	if mirror.Spec.Services.HTTP == nil || !r.Config.PublishEnabled() {
		return deletePublishRouteFor(ctx, r.Client, mirror)
	}
	return nil
}

// publishPlacement derives the node constraint for publish pods from the
// source PV of the sync PVC (status.workPVC) — the scheduler does not follow
// the PVC→dataSource→VolumeSnapshot chain, so the snapshot clone's locality
// is falcon's job. At publish time the sync PVC is necessarily bound (the sync
// Job already ran on it), and the clone inherits the backend's topology, so
// the source PV describes where every publish pod must (or need not) run:
//
//   - determined=false: the PVC/PV chain is not resolvable (PVC missing or
//     not bound, PV object missing) — an abnormal timing window; the caller
//     must not create publish Deployments this reconcile.
//   - determined=true with nodeSelector: local PV with hostname topology
//     (nodeAffinity required term `kubernetes.io/hostname In [...]` — the
//     OpenEBS zfs local PV shape); the hostname becomes a forced pod
//     nodeSelector entry.
//   - determined=true with affinity: nodeAffinity in another topology shape;
//     the required selector terms are copied verbatim into the pod affinity.
//   - determined=true with neither: the PV carries no nodeAffinity (shared
//     storage) — free scheduling is CORRECT here, multi-replica publishing on
//     RWX is a legal extension.
func (r *MirrorReconciler) publishPlacement(ctx context.Context, mirror *mirrorv1alpha1.Mirror) (nodeSelector map[string]string, affinity *corev1.NodeSelector, determined bool, err error) {
	if mirror.Status.WorkPVC == "" {
		return nil, nil, false, nil
	}
	claim := &corev1.PersistentVolumeClaim{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: mirror.Namespace, Name: mirror.Status.WorkPVC}, claim); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil, false, nil
		}
		return nil, nil, false, err
	}
	if claim.Spec.VolumeName == "" {
		return nil, nil, false, nil
	}
	pv := &corev1.PersistentVolume{}
	// Cached read: PVs go through the manager informer — the chart's
	// pv-reader ClusterRole grants cluster-wide get/list/watch.
	if err := r.Get(ctx, types.NamespacedName{Name: claim.Spec.VolumeName}, pv); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil, false, nil
		}
		return nil, nil, false, err
	}
	if pv.Spec.NodeAffinity == nil || pv.Spec.NodeAffinity.Required == nil {
		// Shared storage: no locality constraint to inherit.
		return nil, nil, true, nil
	}
	required := pv.Spec.NodeAffinity.Required
	if hostname, ok := hostnameFromNodeSelectorTerms(required); ok {
		return map[string]string{corev1.LabelHostname: hostname}, nil, true, nil
	}
	return nil, required.DeepCopy(), true, nil
}

// hostnameFromNodeSelectorTerms extracts the single `kubernetes.io/hostname`
// In value from the first term that carries one (the shape local PV
// provisioners emit). ok=false for any other topology shape.
func hostnameFromNodeSelectorTerms(required *corev1.NodeSelector) (string, bool) {
	if required == nil {
		return "", false
	}
	for _, term := range required.NodeSelectorTerms {
		for _, expr := range term.MatchExpressions {
			if expr.Key == corev1.LabelHostname && expr.Operator == corev1.NodeSelectorOpIn && len(expr.Values) > 0 {
				return expr.Values[0], true
			}
		}
	}
	return "", false
}

// ensurePublishEntry maintains one enabled publish service of a Mirror: a
// Deployment and a Service, both named <base>-publish-<key>, with per-service
// pod labels so each Service selects only its own pods.
//
// The pod template is the user's spec.services.<key>.podTemplate with
//
//   - forced data-integrity constraints layered on top: the read-only
//     `mirror-data` publish PVC volume injected into spec.volumes (mounting
//     it, and where, is the user's own declaration), the PV-derived node
//     constraint and the pod identity labels;
//   - defaults injected only where the user template is silent (see
//     applyPublishPodDefaults).
func (r *MirrorReconciler) ensurePublishEntry(ctx context.Context, mirror *mirrorv1alpha1.Mirror, serviceKey string, service *mirrorv1alpha1.MirrorServiceSpec, claimName string, nodeSelector map[string]string, affinity *corev1.NodeSelector) (bool, error) {
	base := childBase(mirror.Name)
	role := publishRole(serviceKey)

	template := service.PodTemplate.DeepCopy()
	if template.Labels == nil {
		template.Labels = map[string]string{}
	}
	for label, value := range map[string]string{MirrorLabel: base, ComponentLabel: role} {
		template.Labels[label] = value
	}
	spec := &template.Spec

	// Node constraint derived from the source PV, forced (not a default): the
	// snapshot clone's locality is invisible to the scheduler, falcon must
	// supply it. The hostname key always wins over the user template; other
	// user nodeSelector keys merge.
	if len(nodeSelector) > 0 {
		if spec.NodeSelector == nil {
			spec.NodeSelector = map[string]string{}
		}
		for key, value := range nodeSelector {
			if existing, ok := spec.NodeSelector[key]; ok && existing != value && r.Recorder != nil {
				r.Recorder.Eventf(mirror, corev1.EventTypeWarning, "PublishNodeSelectorOverridden",
					"podTemplate nodeSelector %s=%q overridden by the PV-derived %q", key, existing, value)
			}
			spec.NodeSelector[key] = value
		}
	}
	if affinity != nil {
		if spec.Affinity == nil {
			spec.Affinity = &corev1.Affinity{}
		}
		if spec.Affinity.NodeAffinity != nil && spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution != nil && r.Recorder != nil {
			// Known edge: the user's own required node affinity cannot be
			// merged with falcon's (two Required term sets are ANDed and would
			// likely never both match) — the PV-derived terms win.
			r.Recorder.Eventf(mirror, corev1.EventTypeWarning, "PublishNodeAffinityOverridden",
				"podTemplate nodeAffinity.required overridden by the PV-derived nodeAffinity (volume locality is authoritative)")
		}
		spec.Affinity.NodeAffinity = &corev1.NodeAffinity{RequiredDuringSchedulingIgnoredDuringExecution: affinity.DeepCopy()}
	}

	applyPublishPodDefaults(spec, serviceKey)

	// The publish data volume is forced, never optional — as a VOLUME only,
	// with a read-only volume source: mounting it, and where, is the user's
	// own declaration. The controller never adds mounts; any user mount of
	// mirror-data must be read-only (validateMirrorService).
	spec.Volumes = append(spec.Volumes, corev1.Volume{
		Name: PublishDataVolumeName,
		VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
			ClaimName: claimName,
			ReadOnly:  true,
		}},
	})

	return ensurePublishServiceAndDeployment(ctx, r.Client, r.Scheme, mirror, base, serviceKey, replicasOrDefault(service.Replicas), *template)
}

// ensurePublishServiceAndDeployment maintains the Service/Deployment pair of
// one enabled publish service key ("http"/"rsync") for owner (a Mirror or a
// ProxyMirror): Service `<base>-publish-<key>` (port 80 -> named target port
// <key>), Deployment `<base>-publish-<key>` carrying the fully merged pod
// template. It reports the Deployment rollout readiness.
func ensurePublishServiceAndDeployment(ctx context.Context, c client.Client, scheme *runtime.Scheme, owner client.Object, base, serviceKey string, replicas int32, podTemplate corev1.PodTemplateSpec) (bool, error) {
	childName := publishChildName(base, serviceKey)
	role := publishRole(serviceKey)

	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: owner.GetNamespace(), Name: childName}}
	if _, err := controllerutil.CreateOrUpdate(ctx, c, svc, func() error {
		svc.Labels = objectLabels(base, role)
		svc.Spec.Selector = map[string]string{MirrorLabel: base, ComponentLabel: role}
		svc.Spec.Ports = []corev1.ServicePort{{
			Name:        serviceKey,
			Port:        publishServicePort,
			TargetPort:  intstr.FromString(serviceKey),
			Protocol:    corev1.ProtocolTCP,
			AppProtocol: publishAppProtocol(serviceKey),
		}}
		return controllerutil.SetControllerReference(owner, svc, scheme)
	}); err != nil {
		return false, err
	}

	// Rolling updates never drop below the desired replica count and surge by
	// exactly one pod: publish capacity is precious (snapshot clones are
	// immutable, there is nothing to "catch up" after a downgrade).
	maxUnavailable := intstr.FromInt32(0)
	maxSurge := intstr.FromInt32(1)
	deployment := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: owner.GetNamespace(), Name: childName}}
	if _, err := controllerutil.CreateOrUpdate(ctx, c, deployment, func() error {
		deployment.Labels = objectLabels(base, role)
		deployment.Spec.Replicas = &replicas
		deployment.Spec.Strategy = appsv1.DeploymentStrategy{
			Type: appsv1.RollingUpdateDeploymentStrategyType,
			RollingUpdate: &appsv1.RollingUpdateDeployment{
				MaxUnavailable: &maxUnavailable,
				MaxSurge:       &maxSurge,
			},
		}
		deployment.Spec.Selector = &metav1.LabelSelector{MatchLabels: map[string]string{MirrorLabel: base, ComponentLabel: role}}
		deployment.Spec.Template = podTemplate
		return controllerutil.SetControllerReference(owner, deployment, scheme)
	}); err != nil {
		return false, err
	}

	if deployment.Generation != deployment.Status.ObservedGeneration {
		return false, nil
	}
	return deployment.Status.AvailableReplicas >= replicas && deployment.Status.UpdatedReplicas >= replicas, nil
}

// applyPodSecurityAndTmpDefaults injects the restricted-profile defaults
// shared by the sync and publish pod templates: values are only filled in
// where the template is silent (nil fields / absent entries), so any explicit
// user setting wins.
//
//   - automountServiceAccountToken: false (pod);
//   - runAsNonRoot: true and seccompProfile RuntimeDefault (pod security
//     context);
//   - per container: allowPrivilegeEscalation false, readOnlyRootFilesystem
//     true, capabilities drop ALL;
//   - a `/tmp` emptyDir volume + mount on the first container (nginx & friends
//     and ftpsync-style HOME=/tmp need a writable /tmp under
//     readOnlyRootFilesystem).
func applyPodSecurityAndTmpDefaults(spec *corev1.PodSpec) {
	if spec.AutomountServiceAccountToken == nil {
		spec.AutomountServiceAccountToken = ptr.To(false)
	}
	if spec.SecurityContext == nil {
		spec.SecurityContext = &corev1.PodSecurityContext{}
	}
	if spec.SecurityContext.RunAsNonRoot == nil {
		spec.SecurityContext.RunAsNonRoot = ptr.To(true)
	}
	if spec.SecurityContext.SeccompProfile == nil {
		spec.SecurityContext.SeccompProfile = &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}
	}
	for i := range spec.Containers {
		securityContext := spec.Containers[i].SecurityContext
		if securityContext == nil {
			securityContext = &corev1.SecurityContext{}
			spec.Containers[i].SecurityContext = securityContext
		}
		if securityContext.AllowPrivilegeEscalation == nil {
			securityContext.AllowPrivilegeEscalation = ptr.To(false)
		}
		if securityContext.ReadOnlyRootFilesystem == nil {
			securityContext.ReadOnlyRootFilesystem = ptr.To(true)
		}
		if securityContext.Capabilities == nil {
			securityContext.Capabilities = &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}}
		}
	}
	if len(spec.Containers) == 0 {
		return
	}
	first := &spec.Containers[0]
	hasTmpVolume := false
	for i := range spec.Volumes {
		if spec.Volumes[i].Name == PublishTmpVolumeName {
			hasTmpVolume = true
		}
	}
	hasTmpMount := false
	for i := range first.VolumeMounts {
		if first.VolumeMounts[i].MountPath == "/tmp" {
			hasTmpMount = true
		}
	}
	if !hasTmpVolume && !hasTmpMount {
		spec.Volumes = append(spec.Volumes, corev1.Volume{
			Name:         PublishTmpVolumeName,
			VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
		})
		first.VolumeMounts = append(first.VolumeMounts, corev1.VolumeMount{Name: PublishTmpVolumeName, MountPath: "/tmp"})
	}
}

// applyPublishPodDefaults layers the publish-specific defaults on top of the
// shared restricted-profile defaults.
func applyPublishPodDefaults(spec *corev1.PodSpec, serviceKey string) {
	applyPodSecurityAndTmpDefaults(spec)
	if len(spec.Containers) == 0 {
		return
	}
	first := &spec.Containers[0]
	// Port convention (kept from the previous shape): the first container
	// port of the first container is the Service target, so it is (re)named
	// after the service key and the Service plus the default probe can
	// reference it as a named port.
	if len(first.Ports) > 0 {
		first.Ports[0].Name = serviceKey
	}
	if first.ReadinessProbe == nil {
		first.ReadinessProbe = &corev1.Probe{
			ProbeHandler:     corev1.ProbeHandler{TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromString(serviceKey)}},
			PeriodSeconds:    5,
			TimeoutSeconds:   2,
			FailureThreshold: 3,
		}
	}
}

// pruneFailedJobs deletes failed sync Jobs of this Mirror beyond
// spec.sync.keepFailedJobs, keeping the newest N by creation time. It runs
// after every sync terminal state (success and failure). Succeeded Jobs are
// untouched: they carry a sync-timestamp label and are pruned with their
// snapshot generation by pruneOldSnapshots.
func (r *MirrorReconciler) pruneFailedJobs(ctx context.Context, mirror *mirrorv1alpha1.Mirror) error {
	base := childBase(mirror.Name)
	keep := int(mirror.Spec.Sync.KeepFailedJobs)
	if keep < 0 {
		keep = 0
	}
	jobs := &batchv1.JobList{}
	if err := r.List(ctx, jobs, client.InNamespace(mirror.Namespace), client.MatchingLabels{MirrorLabel: base}); err != nil {
		return err
	}
	var failed []*batchv1.Job
	for i := range jobs.Items {
		job := &jobs.Items[i]
		if jobFailed(job) && metav1.IsControlledBy(job, mirror) {
			failed = append(failed, job)
		}
	}
	// Newest first; equal creation timestamps are arbitrary among themselves,
	// which only affects which same-instant failures are kept.
	sort.Slice(failed, func(i, j int) bool {
		return failed[i].CreationTimestamp.After(failed[j].CreationTimestamp.Time)
	})
	for _, job := range failed[min(keep, len(failed)):] {
		propagation := metav1.DeletePropagationBackground
		if err := r.Delete(ctx, job, &client.DeleteOptions{PropagationPolicy: &propagation}); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

// pruneOldSnapshots retains the newest previousSnapshots+1 publish PVCs
// (the active one plus the configured number of previous ones) and deletes the
// rest. Snapshots are identified by the Unix seconds sync-start
// timestamp in their sync-timestamp label (mirrored in their names), so they
// are ordered by label, not by numeric snapshot floor.
//
// Deletion ordering is preserved: a VolumeSnapshot is only deleted once no
// publish PVC carrying the same timestamp exists any more, i.e. its clone PVC
// has fully disappeared.
func (r *MirrorReconciler) pruneOldSnapshots(ctx context.Context, mirror *mirrorv1alpha1.Mirror) error {
	base := childBase(mirror.Name)
	keep := int(mirror.Spec.Storage.Retention.PreviousSnapshots) + 1
	if keep < 1 {
		keep = 1
	}
	labels := client.MatchingLabels{MirrorLabel: base}

	claims := &corev1.PersistentVolumeClaimList{}
	if err := r.List(ctx, claims, client.InNamespace(mirror.Namespace), labels); err != nil {
		return err
	}
	var snapshotClaims []*corev1.PersistentVolumeClaim
	for i := range claims.Items {
		claim := &claims.Items[i]
		if _, ok := objectTimestamp(claim.Labels); ok && metav1.IsControlledBy(claim, mirror) {
			snapshotClaims = append(snapshotClaims, claim)
		}
	}
	// Newest first; ties cannot occur because checkSyncTimestampConflict
	// fails with errSnapshotTimestampConflict when the timestamp is already
	// taken.
	sort.Slice(snapshotClaims, func(i, j int) bool {
		tsi, _ := objectTimestamp(snapshotClaims[i].Labels)
		tsj, _ := objectTimestamp(snapshotClaims[j].Labels)
		return tsi > tsj
	})
	// floor is the timestamp of the oldest retained snapshot; child objects
	// (snapshots, jobs) strictly older than it are pruned.
	var floor int64
	if len(snapshotClaims) > keep {
		floor, _ = objectTimestamp(snapshotClaims[keep-1].Labels)
	}
	for _, claim := range snapshotClaims[min(keep, len(snapshotClaims)):] {
		if err := r.Delete(ctx, claim); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}

	snapshots := &snapshotv1.VolumeSnapshotList{}
	if err := r.List(ctx, snapshots, client.InNamespace(mirror.Namespace), labels); err != nil {
		return err
	}
	claimTimestampExists := make(map[int64]bool, len(snapshotClaims))
	for _, claim := range snapshotClaims {
		if ts, ok := objectTimestamp(claim.Labels); ok {
			claimTimestampExists[ts] = true
		}
	}
	for i := range snapshots.Items {
		snapshot := &snapshots.Items[i]
		ts, ok := objectTimestamp(snapshot.Labels)
		if !ok || !metav1.IsControlledBy(snapshot, mirror) {
			continue
		}
		// Only prune snapshots below the retained window whose clone PVC has
		// fully been deleted (no PVC with the same timestamp remains).
		if ts >= floor || claimTimestampExists[ts] {
			continue
		}
		if err := r.Delete(ctx, snapshot); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}

	jobs := &batchv1.JobList{}
	if err := r.List(ctx, jobs, client.InNamespace(mirror.Namespace), labels); err != nil {
		return err
	}
	for i := range jobs.Items {
		job := &jobs.Items[i]
		ts, ok := objectTimestamp(job.Labels)
		if !ok || ts >= floor || !metav1.IsControlledBy(job, mirror) {
			continue
		}
		propagation := metav1.DeletePropagationBackground
		if err := r.Delete(ctx, job, &client.DeleteOptions{PropagationPolicy: &propagation}); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

// childLabels labels a child object. syncTimestamp > 0 marks a
// snapshot-scoped child with the Unix seconds sync-start timestamp.
func childLabels(mirror *mirrorv1alpha1.Mirror, syncTimestamp int64, role string) (map[string]string, error) {
	base := childBase(mirror.Name)
	labels := objectLabels(base, role)
	if syncTimestamp > 0 {
		labels[SyncTimestampLabel] = strconv.FormatInt(syncTimestamp, 10)
	}
	return labels, nil
}

// objectTimestamp reads the Unix seconds sync-completion timestamp from a
// child object's labels.
func objectTimestamp(labels map[string]string) (int64, bool) {
	value := labels[SyncTimestampLabel]
	if value == "" {
		return 0, false
	}
	timestamp, err := strconv.ParseInt(value, 10, 64)
	return timestamp, err == nil
}

// childBase returns the base of every derived child object name: the CR name
// as-is, unconverted. CR names are already enforced to RFC 1123 subdomains by
// the API server (lowercase alphanumerics, '-' and '.'; dots allowed, e.g.
// `linux.git`), and dots are legal both in DNS subdomain child names
// (`linux.git-sync-<ts>`) and in label values, so there is nothing to map —
// the controller's early lowercasing/'.'→'-' normalization was unreachable
// for valid CR names and is gone. Falcon imposes no additional length limit;
// the API server validates each derived resource according to that resource's
// own name and label constraints.
func childBase(name string) string {
	return name
}

// resourceName joins a base and a role suffix into a child object name. Name
// validation belongs to the API server for the concrete child kind.
func resourceName(base, suffix string) string {
	return strings.Trim(base+"-"+suffix, "-")
}

// replicasOrDefault returns the declared replica count of a publish service
// (1 when unset).
func replicasOrDefault(replicas *int32) int32 {
	if replicas != nil {
		return *replicas
	}
	return 1
}

func stringPtr(value string) *string { return &value }
