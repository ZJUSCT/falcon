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
		labels := childLabels(mirror, timestamp, "snapshot")
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
	return snapshot.DeletionTimestamp.IsZero() && snapshot.Status != nil && snapshot.Status.ReadyToUse != nil && *snapshot.Status.ReadyToUse, "", nil
}

func (r *MirrorReconciler) ensureSyncPVC(ctx context.Context, mirror *mirrorv1alpha1.Mirror) error {
	key := types.NamespacedName{Namespace: mirror.Namespace, Name: mirror.Status.WorkPVC}
	claim := &corev1.PersistentVolumeClaim{}
	if err := r.Get(ctx, key, claim); err == nil {
		if !claim.DeletionTimestamp.IsZero() {
			return fmt.Errorf("sync PVC %s is still terminating", claim.Name)
		}
		current := claim.Spec.Resources.Requests[corev1.ResourceStorage]
		desired := mirror.Spec.Storage.PVCSpec.Resources.Requests[corev1.ResourceStorage]
		if current.Cmp(desired) < 0 {
			before := claim.DeepCopy()
			claim.Spec.Resources.Requests[corev1.ResourceStorage] = desired.DeepCopy()
			return r.Patch(ctx, claim, client.MergeFrom(before))
		}
		return nil
	} else if !apierrors.IsNotFound(err) {
		return err
	}

	claim = newDataClaim(mirror, mirror.Status.WorkPVC, 0, "sync")
	if err := controllerutil.SetControllerReference(mirror, claim, r.Scheme); err != nil {
		return err
	}
	if err := r.Create(ctx, claim); err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}
	return nil
}

func (r *MirrorReconciler) ensurePublishPVC(ctx context.Context, mirror *mirrorv1alpha1.Mirror) error {
	snapshotName := publicationSnapshotName(mirror)
	timestamp := mirror.Status.Publication.QueuedAt.Unix()
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

	claim = newDataClaim(mirror, snapshotName, timestamp, "publish-data")
	claim.Spec.DataSource = &corev1.TypedLocalObjectReference{
		APIGroup: stringPtr(snapshotv1.GroupName),
		Kind:     volumeSnapshotKind,
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
// snapshot-scoped (the stable sync PVC). The StorageClass is always explicit:
// the sync claim uses SyncStorageClassName, the publish claims (snapshot
// clones) use PublishStorageClassName.
func newDataClaim(mirror *mirrorv1alpha1.Mirror, name string, syncTimestamp int64, role string) *corev1.PersistentVolumeClaim {
	labels := childLabels(mirror, syncTimestamp, role)
	spec := mirror.Spec.Storage.PVCSpec.DeepCopy()
	if role == "publish-data" {
		spec.StorageClassName = stringPtr(mirror.Spec.Storage.PublishStorageClassName)
		// The publish claim is cloned from a VolumeSnapshot through its
		// dataSource, so dynamic provisioning must run — and the PV
		// controller only takes that path while spec.volumeName is empty.
		// A volumeName inherited from pvcTemplate would pre-bind the clone
		// to a nonexistent PV and leave it Pending forever. volumeName in
		// pvcTemplate exists solely for the sync claim to pre-bind an
		// existing PV (e.g. cross-instance migration); the publish clone
		// never carries it.
		spec.VolumeName = ""
	} else {
		spec.StorageClassName = stringPtr(mirror.Spec.Storage.SyncStorageClassName)
	}
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: mirror.Namespace,
			Name:      name,
			Labels:    labels,
		},
		Spec: *spec,
	}
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
	err := r.liveReader().Get(ctx, key, job)
	switch {
	case apierrors.IsNotFound(err):
		if err := r.checkSyncAdmission(ctx, mirror); err != nil {
			return nil, err
		}
		if err := r.restoreSyncSlots(ctx, mirror.Namespace); err != nil {
			return nil, err
		}
		if !r.SyncLimiter.Acquire(jobName, false) {
			return nil, errSyncQueued
		}
		if err := r.checkSyncTimestampConflict(ctx, mirror); err != nil {
			r.SyncLimiter.Release(jobName)
			return nil, err
		}
		if err := r.checkSyncAdmission(ctx, mirror); err != nil {
			r.SyncLimiter.Release(jobName)
			return nil, err
		}
		if err := r.createSyncJob(ctx, mirror); err != nil {
			r.SyncLimiter.Release(jobName)
			return nil, err
		}
		if err := r.liveReader().Get(ctx, key, job); err != nil {
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
//   - Falcon-managed sync pipeline identity: the WRITABLE `sync-data` PVC volume
//     injected into spec.volumes (mounting it, and where, is the user's own
//     declaration), restartPolicy Never, and the sync labels;
//   - no workload defaults are injected: the PodTemplate is operator-owned.
//
// Job-level: backoffLimit 0 and activeDeadlineSeconds = spec.sync.timeout.
// Placement is NOT injected (see the spec comment inside).
func (r *MirrorReconciler) createSyncJob(ctx context.Context, mirror *mirrorv1alpha1.Mirror) error {
	deadline := int64(mirror.Spec.Sync.Timeout.Seconds())
	if deadline < 1 {
		deadline = 1
	}
	backoffLimit := int32(0)

	// The Job carries the sync-timestamp label from creation: it embeds the
	// same Unix seconds timestamp as its name and is the identity the
	// snapshot and publish PVC reuse after success.
	labels := childLabels(mirror, currentSyncTimestamp(mirror), "sync")

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

	// The writable sync data volume is Falcon-managed and always present — as a VOLUME
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

// ensurePublish maintains the Deployment and Service of every ENABLED
// spec.publish key (a present key) for the given claim. It reports readiness
// across all enabled services.
//
// No placement is derived or injected: the bound PV's nodeAffinity is
// enforced by the scheduler natively. Workload creation is gated on that
// binding — until the publish PVC is bound to its PV no Deployment is
// created, because a pod must not exist before the PV whose nodeAffinity it
// relies on does (cloning the snapshot takes seconds to minutes; the caller
// retries until then).
func (r *MirrorReconciler) ensurePublish(ctx context.Context, mirror *mirrorv1alpha1.Mirror, claimName string) (bool, error) {
	ready := true
	services := mirror.Spec.Publish
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
		ok, err := r.ensurePublishEntry(ctx, mirror, entry.key, entry.spec, claimName)
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
	if mirror.Spec.Publish.HTTP != nil {
		entries = append(entries, struct {
			key      string
			replicas int32
		}{PublishProtocolHTTP, replicasOrDefault(mirror.Spec.Publish.HTTP.Replicas)})
	}
	if mirror.Spec.Publish.Rsync != nil {
		entries = append(entries, struct {
			key      string
			replicas int32
		}{PublishProtocolRsync, replicasOrDefault(mirror.Spec.Publish.Rsync.Replicas)})
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
		if deployment.Status.ObservedGeneration != deployment.Generation || deployment.Status.UpdatedReplicas != entry.replicas || deployment.Status.Replicas != entry.replicas || deployment.Status.AvailableReplicas < entry.replicas {
			converged = false
		}
	}
	return available, converged, nil
}

// deletePublishEntry removes the deterministic Deployment and Service for a
// service key that is no longer present in spec.publish.
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
			if err := c.Delete(ctx, object, client.PropagationPolicy(metav1.DeletePropagationForeground)); err != nil && !apierrors.IsNotFound(err) {
				return err
			}
		}
	}
	return nil
}

func (r *MirrorReconciler) cleanupDisabledPublishChildren(ctx context.Context, mirror *mirrorv1alpha1.Mirror) error {
	if mirror.Spec.Publish.HTTP == nil {
		if err := deletePublishEntry(ctx, r.Client, mirror, PublishProtocolHTTP); err != nil {
			return err
		}
	}
	if mirror.Spec.Publish.Rsync == nil {
		if err := deletePublishEntry(ctx, r.Client, mirror, PublishProtocolRsync); err != nil {
			return err
		}
	}
	if mirror.Spec.Publish.HTTP == nil || !r.Config.PublishEnabled() {
		return deletePublishRouteFor(ctx, r.Client, mirror)
	}
	return nil
}

// ensurePublishEntry maintains one enabled publish service of a Mirror: a
// Deployment and a Service, both named <base>-publish-<key>, with per-service
// pod labels so each Service selects only its own pods.
//
// The pod template is the user's spec.publish.<key>.podTemplate with
//
//   - Falcon-managed data-integrity constraints layered on top: the read-only
//     `mirror-data` publish PVC volume injected into spec.volumes (mounting
//     it, and where, is the user's own declaration) and the pod identity
//     labels;
//   - no workload defaults are injected: the PodTemplate is operator-owned.
//     Placement is not touched either — volume locality is the scheduler's
//     job (the bound clone PV's nodeAffinity), never Falcon's.
func (r *MirrorReconciler) ensurePublishEntry(ctx context.Context, mirror *mirrorv1alpha1.Mirror, serviceKey string, service *mirrorv1alpha1.MirrorServiceSpec, claimName string) (bool, error) {
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

	// The publish data volume is Falcon-managed and always present — as a VOLUME only,
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
// ProxyMirror): Service `<base>-publish-<key>` (port 80 -> the first declared
// port on the operator-owned template), and Deployment `<base>-publish-<key>`.
// It reports the Deployment rollout readiness.
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
			TargetPort:  intstr.FromInt32(podTemplate.Spec.Containers[0].Ports[0].ContainerPort),
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
	return deployment.Status.AvailableReplicas >= replicas && deployment.Status.UpdatedReplicas == replicas && deployment.Status.Replicas == replicas, nil
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

// childLabels labels a child object. syncTimestamp > 0 marks a
// snapshot-scoped child with the Unix seconds sync-start timestamp.
func childLabels(mirror *mirrorv1alpha1.Mirror, syncTimestamp int64, role string) map[string]string {
	base := childBase(mirror.Name)
	labels := objectLabels(base, role)
	if syncTimestamp > 0 {
		labels[SyncTimestampLabel] = strconv.FormatInt(syncTimestamp, 10)
	}
	return labels
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
