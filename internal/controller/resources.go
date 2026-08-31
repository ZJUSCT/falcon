package controller

import (
	"context"
	"errors"
	"fmt"
	"path"
	"sort"
	"strconv"
	"strings"
	"unicode"

	snapshotv1 "github.com/kubernetes-csi/external-snapshotter/client/v8/apis/volumesnapshot/v1"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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

func (r *MirrorReconciler) ensureSnapshot(ctx context.Context, mirror *mirrorv1alpha1.Mirror) (bool, string, error) {
	key := types.NamespacedName{Namespace: mirror.Namespace, Name: mirror.Status.PendingSnapshot}
	snapshot := &snapshotv1.VolumeSnapshot{}
	if err := r.Get(ctx, key, snapshot); err != nil {
		if !apierrors.IsNotFound(err) {
			return false, "", err
		}
		labels, err := childLabels(mirror, mirror.Status.PendingSyncTimestamp, "snapshot")
		if err != nil {
			return false, "", err
		}
		snapshot = &snapshotv1.VolumeSnapshot{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: mirror.Namespace,
				Name:      mirror.Status.PendingSnapshot,
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
	key := types.NamespacedName{Namespace: mirror.Namespace, Name: mirror.Status.PendingPVC}
	claim := &corev1.PersistentVolumeClaim{}
	if err := r.Get(ctx, key, claim); err == nil {
		if claim.DeletionTimestamp.IsZero() {
			return nil
		}
		return fmt.Errorf("publish PVC %s is still terminating", claim.Name)
	} else if !apierrors.IsNotFound(err) {
		return err
	}

	claim, err := newDataClaim(mirror, mirror.Status.PendingPVC, mirror.Status.PendingSyncTimestamp, "publish-data")
	if err != nil {
		return err
	}
	claim.Spec.DataSource = &corev1.TypedLocalObjectReference{
		APIGroup: stringPtr(snapshotv1.GroupName),
		Kind:     "VolumeSnapshot",
		Name:     mirror.Status.PendingSnapshot,
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
	if role == "publish-data" && mirror.Spec.Storage.ServingStorageClassName != "" {
		storageClassName = mirror.Spec.Storage.ServingStorageClassName
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
	key := types.NamespacedName{Namespace: mirror.Namespace, Name: mirror.Status.PendingJob}
	job := &batchv1.Job{}
	err := r.Get(ctx, key, job)
	switch {
	case apierrors.IsNotFound(err):
		if !r.SyncLimiter.Acquire(mirror.Status.PendingJob, false) {
			return nil, errSyncQueued
		}
		if err := r.checkSyncTimestampConflict(ctx, mirror); err != nil {
			r.SyncLimiter.Release(mirror.Status.PendingJob)
			return nil, err
		}
		if err := r.createSyncJob(ctx, mirror); err != nil {
			r.SyncLimiter.Release(mirror.Status.PendingJob)
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
	ts := mirror.Status.PendingSyncTimestamp
	base, err := childBase(mirror.Name)
	if err != nil {
		return err
	}
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
	snapshotName, err := resourceName(base, fmt.Sprintf("snap-%d", ts))
	if err != nil {
		return err
	}
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
func (r *MirrorReconciler) createSyncJob(ctx context.Context, mirror *mirrorv1alpha1.Mirror) error {
	dataMountPath := mirror.Spec.Sync.DataMountPath
	if dataMountPath == "" {
		dataMountPath = "/data"
	}
	pullPolicy := mirror.Spec.Sync.ImagePullPolicy
	if pullPolicy == "" {
		pullPolicy = corev1.PullIfNotPresent
	}
	deadline := int64(mirror.Spec.Sync.Timeout.Duration.Seconds())
	if deadline < 1 {
		deadline = 1
	}
	backoffLimit := int32(0)
	terminationGrace := int64(30)
	automountToken := false
	allowPrivilegeEscalation := false
	// Sync pods must satisfy the Pod Security "restricted" profile: sync images
	// never need root (data dirs are writable by the mirror-scoped uid).
	runAsNonRoot := true
	runAsUser := int64(65532)

	volumes := []corev1.Volume{{
		Name: "mirror-data",
		VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
			ClaimName: mirror.Status.WorkPVC,
		}},
	}}
	mounts := []corev1.VolumeMount{{Name: "mirror-data", MountPath: dataMountPath}}
	for _, input := range mirror.Spec.Sync.Volumes {
		volume := corev1.Volume{Name: input.Name}
		switch {
		case input.ConfigMap != nil:
			volume.VolumeSource.ConfigMap = input.ConfigMap.DeepCopy()
		case input.Secret != nil:
			volume.VolumeSource.Secret = input.Secret.DeepCopy()
		}
		volumes = append(volumes, volume)
		// Input volumes are ConfigMap/Secret only and are always mounted
		// read-only (the CR-level ReadOnly override was removed on purpose).
		mounts = append(mounts, corev1.VolumeMount{
			Name:      input.Name,
			MountPath: input.MountPath,
			SubPath:   input.SubPath,
			ReadOnly:  true,
		})
	}

	// No implicit env injection: the data location is configured via
	// spec.sync.dataMountPath and everything else via spec.sync.env.
	env := append([]corev1.EnvVar(nil), mirror.Spec.Sync.Env...)

	// The Job carries the sync-timestamp label from creation: it embeds the
	// same Unix seconds timestamp as its name and is the identity the
	// snapshot and publish PVC reuse after success.
	labels, err := childLabels(mirror, mirror.Status.PendingSyncTimestamp, "sync")
	if err != nil {
		return err
	}
	nodeName, nodeSelector := workloadPlacement(mirror)
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: mirror.Namespace,
			Name:      mirror.Status.PendingJob,
			Labels:    labels,
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:          &backoffLimit,
			ActiveDeadlineSeconds: &deadline,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					RestartPolicy:                 corev1.RestartPolicyNever,
					AutomountServiceAccountToken:  &automountToken,
					TerminationGracePeriodSeconds: &terminationGrace,
					NodeName:                      nodeName,
					NodeSelector:                  nodeSelector,
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot:   &runAsNonRoot,
						RunAsUser:      &runAsUser,
						SeccompProfile: &corev1.SeccompProfile{
							Type: corev1.SeccompProfileTypeRuntimeDefault,
						},
					},
					Containers: []corev1.Container{{
						Name:            "sync",
						Image:           mirror.Spec.Sync.Image,
						ImagePullPolicy: pullPolicy,
						Command:         append([]string(nil), mirror.Spec.Sync.Command...),
						Args:            append([]string(nil), mirror.Spec.Sync.Args...),
						Env:             env,
						EnvFrom:         append([]corev1.EnvFromSource(nil), mirror.Spec.Sync.EnvFrom...),
						Resources:       *mirror.Spec.Sync.Resources.DeepCopy(),
						VolumeMounts:    mounts,
						SecurityContext: &corev1.SecurityContext{
							AllowPrivilegeEscalation: &allowPrivilegeEscalation,
							Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
						},
					}},
					Volumes: volumes,
				},
			},
		},
	}
	if err := controllerutil.SetControllerReference(mirror, job, r.Scheme); err != nil {
		return err
	}
	return r.Create(ctx, job)
}

// ensurePublish maintains the Service and Deployment of every declared
// spec.services[] entry for the given claim. syncTimestamp (> 0)
// annotates the pod templates with the snapshot's Unix seconds timestamp;
// 0 leaves the annotation untouched/absent. It reports readiness across all
// entries.
func (r *MirrorReconciler) ensurePublish(ctx context.Context, mirror *mirrorv1alpha1.Mirror, claimName string, syncTimestamp int64) (bool, error) {
	ready := true
	for i := range mirror.Spec.Services {
		ok, err := r.ensurePublishEntry(ctx, mirror, &mirror.Spec.Services[i], claimName, syncTimestamp)
		if err != nil {
			return false, err
		}
		if !ok {
			ready = false
		}
	}
	return ready, nil
}

// ensurePublishEntry maintains one publish service entry: a Deployment and a
// Service, both named <base>-publish-<protocol> (http/rsync/git), with
// per-service pod labels so each Service selects only its own pods. The data
// PVC is mounted read-only at <MountPath>/<mirror name> for EVERY service
// type: rsyncd module paths and git http-backend roots point there exactly
// like the http web root.
func (r *MirrorReconciler) ensurePublishEntry(ctx context.Context, mirror *mirrorv1alpha1.Mirror, service *mirrorv1alpha1.MirrorServingService, claimName string, syncTimestamp int64) (bool, error) {
	base, err := childBase(mirror.Name)
	if err != nil {
		return false, err
	}
	childName, err := publishChildName(base, service.Name)
	if err != nil {
		return false, err
	}
	role := publishRole(service.Name)
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: mirror.Namespace, Name: childName}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, svc, func() error {
		labels, err := childLabels(mirror, 0, role)
		if err != nil {
			return err
		}
		svc.Labels = labels
		svc.Spec.Selector = map[string]string{MirrorLabel: base, RoleLabel: role}
		svc.Spec.Ports = []corev1.ServicePort{{
			Name:        service.Name,
			Port:        publishServicePort,
			TargetPort:  intstr.FromString(service.Name),
			Protocol:    corev1.ProtocolTCP,
			AppProtocol: publishAppProtocol(service.Name),
		}}
		return controllerutil.SetControllerReference(mirror, svc, r.Scheme)
	}); err != nil {
		return false, err
	}

	replicas := serviceReplicas(service)
	pullPolicy := service.ImagePullPolicy
	if pullPolicy == "" {
		pullPolicy = corev1.PullIfNotPresent
	}
	mountPath := service.MountPath
	if mountPath == "" {
		mountPath = "/srv/mirror"
	}
	// The publish route prefix is /<mirror name>, so the data PVC is mounted
	// one level below the configured root: PVC-root content (index.html,
	// dists/...) is served under /<name>/ exactly as the route expects.
	dataMountPath := path.Join(mountPath, mirror.Name)
	readinessPath := service.ReadinessPath
	if readinessPath == "" {
		readinessPath = "/"
	}
	ports := make([]corev1.ContainerPort, len(service.Ports))
	copy(ports, service.Ports)
	// The first container port is the Service target; it is (re)named after
	// the service so the Service and the probes can reference it as a named
	// port ("http", "rsync" or "git").
	ports[0].Name = service.Name
	maxUnavailable := intstr.FromInt32(0)
	maxSurge := intstr.FromInt32(1)
	allowPrivilegeEscalation := false
	readOnlyRootFilesystem := true
	runAsNonRoot := true
	automountToken := false
	nodeName, nodeSelector := workloadPlacement(mirror)
	podLabels := map[string]string{MirrorLabel: base, RoleLabel: role}
	podAnnotations := map[string]string{ActivePVCAnnotation: claimName}
	if syncTimestamp > 0 {
		podAnnotations[SyncTimestampLabel] = strconv.FormatInt(syncTimestamp, 10)
	}

	deployment := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: mirror.Namespace, Name: childName}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, deployment, func() error {
		labels, err := childLabels(mirror, 0, role)
		if err != nil {
			return err
		}
		deployment.Labels = labels
		deployment.Spec.Replicas = &replicas
		deployment.Spec.Strategy = appsv1.DeploymentStrategy{
			Type: appsv1.RollingUpdateDeploymentStrategyType,
			RollingUpdate: &appsv1.RollingUpdateDeployment{
				MaxUnavailable: &maxUnavailable,
				MaxSurge:       &maxSurge,
			},
		}
		deployment.Spec.Selector = &metav1.LabelSelector{MatchLabels: podLabels}
		deployment.Spec.Template = corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{Labels: podLabels, Annotations: podAnnotations},
			Spec: corev1.PodSpec{
				AutomountServiceAccountToken: &automountToken,
				NodeName:                     nodeName,
				NodeSelector:                 nodeSelector,
				SecurityContext: &corev1.PodSecurityContext{
					RunAsNonRoot:   &runAsNonRoot,
					SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
				},
				Containers: []corev1.Container{{
					Name:            "server",
					Image:           service.Image,
					ImagePullPolicy: pullPolicy,
					Command:         append([]string(nil), service.Command...),
					Args:            append([]string(nil), service.Args...),
					Ports:           ports,
					Resources:       *service.Resources.DeepCopy(),
					ReadinessProbe: &corev1.Probe{
						ProbeHandler:     corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: readinessPath, Port: intstr.FromString(service.Name)}},
						PeriodSeconds:    5,
						TimeoutSeconds:   2,
						FailureThreshold: 3,
					},
					LivenessProbe: &corev1.Probe{
						ProbeHandler:     corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: readinessPath, Port: intstr.FromString(service.Name)}},
						PeriodSeconds:    10,
						TimeoutSeconds:   2,
						FailureThreshold: 3,
					},
					SecurityContext: &corev1.SecurityContext{
						AllowPrivilegeEscalation: &allowPrivilegeEscalation,
						ReadOnlyRootFilesystem:   &readOnlyRootFilesystem,
						Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
					},
					VolumeMounts: []corev1.VolumeMount{
						{Name: "mirror-data", MountPath: dataMountPath, ReadOnly: true},
						{Name: "tmp", MountPath: "/tmp"},
					},
				}},
				Volumes: []corev1.Volume{
					{Name: "mirror-data", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: claimName, ReadOnly: true}}},
					{Name: "tmp", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
				},
			},
		}
		return controllerutil.SetControllerReference(mirror, deployment, r.Scheme)
	}); err != nil {
		return false, err
	}

	if deployment.Generation != deployment.Status.ObservedGeneration {
		return false, nil
	}
	return deployment.Status.AvailableReplicas >= replicas && deployment.Status.UpdatedReplicas >= replicas, nil
}

// pruneFailedJobs deletes failed sync Jobs of this Mirror beyond
// spec.sync.keepFailedJobs, keeping the newest N by creation time. It runs
// after every sync terminal state (success and failure). Succeeded Jobs are
// untouched: they carry a sync-timestamp label and are pruned with their
// snapshot generation by pruneOldSnapshots.
func (r *MirrorReconciler) pruneFailedJobs(ctx context.Context, mirror *mirrorv1alpha1.Mirror) error {
	base, err := childBase(mirror.Name)
	if err != nil {
		return err
	}
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
	base, err := childBase(mirror.Name)
	if err != nil {
		return err
	}
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

func workloadPlacement(mirror *mirrorv1alpha1.Mirror) (string, map[string]string) {
	nodeName := mirror.Spec.Storage.NodeName
	if nodeName == "" {
		nodeName = mirror.Spec.Sync.NodeName
	}
	selector := make(map[string]string, len(mirror.Spec.Storage.NodeSelector)+len(mirror.Spec.Sync.NodeSelector))
	for key, value := range mirror.Spec.Storage.NodeSelector {
		selector[key] = value
	}
	for key, value := range mirror.Spec.Sync.NodeSelector {
		selector[key] = value
	}
	if nodeName != "" {
		// pod.spec.nodeName bypasses the scheduler, so a WaitForFirstConsumer PVC
		// would remain Pending forever. A hostname selector preserves exact
		// placement while allowing normal scheduler volume binding.
		selector[corev1.LabelHostname] = nodeName
	}
	if len(selector) == 0 {
		selector = nil
	}
	return "", selector
}

// childLabels labels a child object. syncTimestamp > 0 marks a
// snapshot-scoped child with the Unix seconds sync-start timestamp.
func childLabels(mirror *mirrorv1alpha1.Mirror, syncTimestamp int64, role string) (map[string]string, error) {
	base, err := childBase(mirror.Name)
	if err != nil {
		return nil, err
	}
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

// maxDNSLabelLen is the Kubernetes limit for names embedded in labels and
// most resource names (DNS-1123 label). Derived child names that would exceed
// it are rejected instead of being silently shortened.
const maxDNSLabelLen = 63

// maxSyncTimestampLen bounds the decimal Unix seconds timestamp embedded in
// sync Job, VolumeSnapshot and publish PVC names: 10 digits cover every
// timestamp until 2286-11-20, so the token can never grow beyond that while
// time fits in the foreseeable range.
const maxSyncTimestampLen = 10

// childBase normalizes a CR name into the base of every derived child object
// name: lowercased; letters, digits and '-' kept; '.' and '_' each mapped to
// '-'; everything else dropped; leading/trailing '-' trimmed. An empty result
// falls back to "mirror" — dead code for valid DNS-1123 CR names (those always
// contain at least one letter or digit), kept as a defensive default.
//
// Unlike the controller's early versions there is no truncation: a base that
// exceeds maxDNSLabelLen is an error (validation reports it as InvalidSpec).
func childBase(name string) (string, error) {
	var builder strings.Builder
	for _, r := range strings.ToLower(name) {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-':
			builder.WriteRune(r)
		case r == '.' || r == '_':
			builder.WriteByte('-')
		}
	}
	value := strings.Trim(builder.String(), "-")
	if value == "" {
		value = "mirror"
	}
	if len(value) > maxDNSLabelLen {
		return "", fmt.Errorf("child name base %q (derived from %q) is %d characters, exceeding the %d-character limit; shorten the CR name", value, name, len(value), maxDNSLabelLen)
	}
	return value, nil
}

// resourceName joins a base and a role suffix into a child object name,
// rejecting results that exceed maxDNSLabelLen.
func resourceName(base, suffix string) (string, error) {
	raw := strings.Trim(base+"-"+suffix, "-")
	if len(raw) > maxDNSLabelLen {
		return "", fmt.Errorf("child name %q is %d characters, exceeding the %d-character limit; shorten the CR name", raw, len(raw), maxDNSLabelLen)
	}
	return raw, nil
}

// serviceReplicas returns the declared replica count of a publish service
// entry (1 when unset).
func serviceReplicas(service *mirrorv1alpha1.MirrorServingService) int32 {
	if service.Replicas != nil {
		return *service.Replicas
	}
	return 1
}

func stringPtr(value string) *string { return &value }
