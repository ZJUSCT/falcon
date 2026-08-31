package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	snapshotv1 "github.com/kubernetes-csi/external-snapshotter/client/v8/apis/volumesnapshot/v1"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	mirrorv1alpha1 "github.com/ZJUSCT/falcon/api/v1alpha1"
	"github.com/ZJUSCT/falcon/internal/config"
)

const (
	MirrorFinalizer       = "mirrors.zjusct.io/storage-cleanup"
	SyncRequestAnnotation = "mirrors.zjusct.io/sync-request"
	MirrorLabel           = "mirrors.zjusct.io/mirror"
	// SyncTimestampLabel carries the Unix seconds timestamp of a sync task
	// (allocated once when the controller creates the task) on every
	// snapshot-scoped child: the sync Job, the VolumeSnapshot and the publish
	// PVC.
	SyncTimestampLabel  = "mirrors.zjusct.io/sync-timestamp"
	ActivePVCAnnotation = "mirrors.zjusct.io/active-pvc"
	RoleLabel           = "mirrors.zjusct.io/role"

	conditionReady       = "Ready"
	conditionProgressing = "Progressing"
	conditionDegraded    = "Degraded"
)

type MirrorReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
	Now      func() time.Time
	// Config is the loaded controller configuration (required). The serving
	// section (config serving.*) gates publish HTTPRoute generation, the sync
	// section the global concurrency cap.
	Config *config.Config
	// SyncLimiter enforces the global cap of concurrently running sync Jobs
	// (config sync.maxConcurrent). Required.
	SyncLimiter *SyncLimiter
}

// RBAC (rendered into the namespaced Role in config/rbac; the controller only
// ever touches its own namespace):
//
// +kubebuilder:rbac:groups=mirrors.zjusct.io,resources=mirrors,verbs=get;list;watch;patch;update
// +kubebuilder:rbac:groups=mirrors.zjusct.io,resources=mirrors/status,verbs=get;patch;update
// +kubebuilder:rbac:groups=mirrors.zjusct.io,resources=mirrors/finalizers,verbs=get;patch;update
// +kubebuilder:rbac:groups=mirrors.zjusct.io,resources=proxymirrors,verbs=get;list;watch;patch;update
// +kubebuilder:rbac:groups=mirrors.zjusct.io,resources=proxymirrors/status,verbs=get;patch;update
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch;create;patch;update;delete
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;patch;update;delete
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods/log,verbs=get
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch;update
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch;update
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;patch;update;delete
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;patch;update;delete
// +kubebuilder:rbac:groups=snapshot.storage.k8s.io,resources=volumesnapshots,verbs=get;list;watch;create;patch;update;delete
// +kubebuilder:rbac:groups=snapshot.storage.k8s.io,resources=volumesnapshots/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=httproutes,verbs=get;list;watch;create;patch;update;delete
// +kubebuilder:rbac:groups=coordination.k8s.io,resources=leases,verbs=get;list;watch;create;patch;update;delete

func (r *MirrorReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&mirrorv1alpha1.Mirror{}).
		Owns(&corev1.PersistentVolumeClaim{}).
		Owns(&snapshotv1.VolumeSnapshot{}).
		Owns(&batchv1.Job{}).
		Owns(&corev1.Service{}).
		Owns(&appsv1.Deployment{}).
		Owns(&gatewayv1.HTTPRoute{}).
		Named("mirror").
		Complete(r)
}

func (r *MirrorReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	mirror := &mirrorv1alpha1.Mirror{}
	if err := r.Get(ctx, req.NamespacedName, mirror); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !mirror.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, mirror)
	}
	if !controllerutil.ContainsFinalizer(mirror, MirrorFinalizer) {
		before := mirror.DeepCopy()
		controllerutil.AddFinalizer(mirror, MirrorFinalizer)
		if err := r.Patch(ctx, mirror, client.MergeFrom(before)); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	if errs := validateMirror(mirror); len(errs) > 0 {
		message := errs.ToAggregate().Error()
		logger.Info("Mirror specification is invalid", "errors", message)
		return r.patchStatus(ctx, mirror, func() {
			mirror.Status.ObservedGeneration = mirror.Generation
			mirror.Status.Phase = mirrorv1alpha1.PhaseDegraded
			setCondition(mirror, conditionReady, metav1.ConditionFalse, "InvalidSpec", message)
			setCondition(mirror, conditionProgressing, metav1.ConditionFalse, "InvalidSpec", message)
			setCondition(mirror, conditionDegraded, metav1.ConditionTrue, "InvalidSpec", message)
		})
	}

	if mirror.Status.PendingJob != "" {
		return r.reconcilePendingSnapshot(ctx, mirror)
	}

	if mirror.Status.ActivePVC != "" && publishEnabled(mirror) {
		// A published Mirror is served. Only the "http" service entry is
		// routed through the Gateway API (rsync/git are Service-only); the
		// config switch still gates all route generation — see ServingEnabled.
		if publishHTTPEnabled(mirror) {
			if err := ensurePublishedMirrorRoute(ctx, r, mirror); err != nil {
				return ctrl.Result{}, err
			}
		}
		ready, err := r.ensurePublish(ctx, mirror, mirror.Status.ActivePVC, int64(0))
		if err != nil {
			return ctrl.Result{}, err
		}
		if !ready {
			return r.patchStatusWithResult(ctx, mirror, ctrl.Result{RequeueAfter: 5 * time.Second}, func() {
				mirror.Status.Phase = mirrorv1alpha1.PhasePublishing
				setCondition(mirror, conditionReady, metav1.ConditionFalse, "ServingRollout", "waiting for the publish Deployment to become available")
				setCondition(mirror, conditionProgressing, metav1.ConditionTrue, "ServingRollout", "updating the publish Deployment")
			})
		}
	}

	if mirror.Spec.Paused {
		return r.patchStatus(ctx, mirror, func() {
			mirror.Status.ObservedGeneration = mirror.Generation
			mirror.Status.Phase = mirrorv1alpha1.PhasePaused
			setCondition(mirror, conditionReady, conditionStatus(mirror.Status.ActivePVC != ""), "Paused", "new synchronization runs are paused")
			setCondition(mirror, conditionProgressing, metav1.ConditionFalse, "Paused", "new synchronization runs are paused")
			setCondition(mirror, conditionDegraded, metav1.ConditionFalse, "Paused", "")
		})
	}

	now := r.now()
	request := mirror.Annotations[SyncRequestAnnotation]
	manualDue := request != "" && request != mirror.Status.LastHandledSyncRequest
	specDue := mirror.Status.ActivePVC != "" && mirror.Status.ObservedGeneration != mirror.Generation
	bootstrapDue := mirror.Status.ActivePVC == "" && mirror.Status.LastSync == nil
	scheduleDue := mirror.Status.NextSyncAt != nil && !mirror.Status.NextSyncAt.Time.After(now)

	if manualDue || specDue || bootstrapDue || scheduleDue {
		return r.startSync(ctx, mirror, request)
	}

	if mirror.Status.ActivePVC != "" {
		if err := r.pruneOldSnapshots(ctx, mirror); err != nil {
			return ctrl.Result{}, err
		}
	}

	result := ctrl.Result{}
	if mirror.Status.NextSyncAt != nil {
		result.RequeueAfter = time.Until(mirror.Status.NextSyncAt.Time)
		if result.RequeueAfter < time.Second {
			result.RequeueAfter = time.Second
		}
	}
	return r.patchStatusWithResult(ctx, mirror, result, func() {
		mirror.Status.ObservedGeneration = mirror.Generation
		if mirror.Status.ActivePVC != "" {
			mirror.Status.Phase = mirrorv1alpha1.PhaseReady
			setCondition(mirror, conditionReady, metav1.ConditionTrue, "Published", fmt.Sprintf("PVC %s is published", mirror.Status.ActivePVC))
		} else if mirror.Status.Phase == "" {
			mirror.Status.Phase = mirrorv1alpha1.PhasePending
			setCondition(mirror, conditionReady, metav1.ConditionFalse, "Pending", "waiting for the initial synchronization")
		}
		setCondition(mirror, conditionProgressing, metav1.ConditionFalse, "Idle", "no synchronization is running")
	})
}

// startSync begins a new synchronization run. The Unix seconds timestamp is
// allocated ONCE here, when the controller creates the sync task, and
// propagates from status.pendingSyncTimestamp into every derived name: the
// sync Job `<base>-sync-<ts>`, the VolumeSnapshot and the publish PVC (which
// share the name `<base>-snap-<ts>`). The sync PVC has the fixed name
// `<base>-sync` (no timestamp) and is reused across runs. Whether the
// timestamp is free (no existing Job/PVC/VolumeSnapshot carrying it) is
// checked when the sync Job is created — see lookupOrCreateSyncJob.
func (r *MirrorReconciler) startSync(ctx context.Context, mirror *mirrorv1alpha1.Mirror, request string) (ctrl.Result, error) {
	now := r.now()
	timestamp := now.Unix()
	base, err := childBase(mirror.Name)
	if err != nil {
		return ctrl.Result{}, err
	}
	syncPVCName := mirror.Status.WorkPVC
	if syncPVCName == "" {
		syncPVCName, err = resourceName(base, "sync")
		if err != nil {
			return ctrl.Result{}, err
		}
	}
	jobName, err := resourceName(base, fmt.Sprintf("sync-%d", timestamp))
	if err != nil {
		return ctrl.Result{}, err
	}
	snapshotName, err := resourceName(base, fmt.Sprintf("snap-%d", timestamp))
	if err != nil {
		return ctrl.Result{}, err
	}

	if r.Recorder != nil {
		r.Recorder.Eventf(mirror, corev1.EventTypeNormal, "SynchronizationStarted", "Starting synchronization run with Job %s", jobName)
	}
	return r.patchStatusWithResult(ctx, mirror, ctrl.Result{Requeue: true}, func() {
		mirror.Status.ObservedGeneration = mirror.Generation
		mirror.Status.Phase = mirrorv1alpha1.PhaseInitializing
		mirror.Status.WorkPVC = syncPVCName
		mirror.Status.PendingSyncTimestamp = timestamp
		mirror.Status.PendingPVC = snapshotName
		mirror.Status.PendingSnapshot = snapshotName
		mirror.Status.PendingJob = jobName
		mirror.Status.PendingSyncRequest = request
		mirror.Status.LastSync = &mirrorv1alpha1.MirrorSyncStatus{
			JobName:   jobName,
			Phase:     mirrorv1alpha1.SyncPhaseRunning,
			StartedAt: timePtr(now),
		}
		setCondition(mirror, conditionReady, conditionStatus(mirror.Status.ActivePVC != ""), "SynchronizationStarted", "preparing synchronization run")
		setCondition(mirror, conditionProgressing, metav1.ConditionTrue, "SynchronizationStarted", "preparing synchronization run")
		setCondition(mirror, conditionDegraded, metav1.ConditionFalse, "SynchronizationStarted", "")
	})
}

func (r *MirrorReconciler) reconcilePendingSnapshot(ctx context.Context, mirror *mirrorv1alpha1.Mirror) (ctrl.Result, error) {
	if err := r.ensureSyncPVC(ctx, mirror); err != nil {
		return ctrl.Result{}, err
	}
	job, err := r.lookupOrCreateSyncJob(ctx, mirror)
	if errors.Is(err, errSyncQueued) {
		// The global sync concurrency cap (sync.maxConcurrent) is reached:
		// leave the Job uncreated and retry shortly. The queued sync may
		// start later than status.nextSyncAt.
		return r.patchStatusWithResult(ctx, mirror, ctrl.Result{RequeueAfter: 5 * time.Second}, func() {
			setCondition(mirror, conditionProgressing, metav1.ConditionTrue, "SyncQueued",
				fmt.Sprintf("sync Job %s is queued: global sync concurrency limit (%d) reached", mirror.Status.PendingJob, r.Config.Sync.MaxConcurrent))
		})
	}
	if errors.Is(err, errSnapshotTimestampConflict) {
		// The timestamp allocated at sync task creation is already taken by an
		// existing Job/PVC/VolumeSnapshot: stop this reconcile without clearing
		// the pending pipeline. Degraded + Warning event tell the operator to
		// check for the leftover same-second object and remove it if safe;
		// retrying after a minute avoids a requeue storm.
		if r.Recorder != nil {
			r.Recorder.Eventf(mirror, corev1.EventTypeWarning, "SnapshotTimestampConflict",
				"Synchronization run cannot start: %s. Check for the leftover same-second Job/PVC/VolumeSnapshot and remove it if safe.", err.Error())
		}
		return r.patchStatusWithResult(ctx, mirror, ctrl.Result{RequeueAfter: time.Minute}, func() {
			mirror.Status.Phase = mirrorv1alpha1.PhaseDegraded
			message := err.Error()
			setCondition(mirror, conditionReady, conditionStatus(mirror.Status.ActivePVC != ""), "SnapshotTimestampConflict", message)
			setCondition(mirror, conditionProgressing, metav1.ConditionFalse, "SnapshotTimestampConflict", message)
			setCondition(mirror, conditionDegraded, metav1.ConditionTrue, "SnapshotTimestampConflict", message)
		})
	}
	if err != nil {
		return ctrl.Result{}, err
	}

	terminal := jobFailed(job) || jobSucceeded(job)
	if terminal {
		// The Job reached a terminal state: its concurrency slot is freed.
		// Release is idempotent, so later reconciles that still see the Job
		// while the publication completes cannot free someone else's slot.
		r.SyncLimiter.Release(job.Name)
	} else {
		// Non-terminal Job (just created here, or found running — e.g. after
		// a controller restart): make sure it counts against the cap.
		r.SyncLimiter.Acquire(job.Name, true)
	}

	if jobFailed(job) {
		message := jobFailureMessage(job)
		return r.failPendingSnapshot(ctx, mirror, "SyncJobFailed", message)
	}
	if !jobSucceeded(job) {
		return r.patchStatusWithResult(ctx, mirror, ctrl.Result{RequeueAfter: 5 * time.Second}, func() {
			mirror.Status.Phase = mirrorv1alpha1.PhaseSyncing
			setCondition(mirror, conditionProgressing, metav1.ConditionTrue, "SyncJobRunning", fmt.Sprintf("Job %s is running", job.Name))
		})
	}

	// The Job succeeded: the timestamp allocated at task creation is simply
	// reused — the snapshot and publish PVC names are already persisted in
	// status (they share `<base>-snap-<ts>`), so publication proceeds directly.
	ready, message, err := r.ensureSnapshot(ctx, mirror)
	if err != nil {
		return ctrl.Result{}, err
	}
	if message != "" {
		return r.failPendingSnapshot(ctx, mirror, "SnapshotFailed", message)
	}
	if !ready {
		return r.patchStatusWithResult(ctx, mirror, ctrl.Result{RequeueAfter: 5 * time.Second}, func() {
			mirror.Status.Phase = mirrorv1alpha1.PhasePublishing
			setCondition(mirror, conditionProgressing, metav1.ConditionTrue, "Snapshotting", fmt.Sprintf("snapshotting completed sync PVC %s", mirror.Status.WorkPVC))
		})
	}

	if err := r.ensurePublishPVC(ctx, mirror); err != nil {
		return ctrl.Result{}, err
	}

	if publishEnabled(mirror) {
		ready, err := r.ensurePublish(ctx, mirror, mirror.Status.PendingPVC, mirror.Status.PendingSyncTimestamp)
		if err != nil {
			return ctrl.Result{}, err
		}
		if !ready {
			return r.patchStatusWithResult(ctx, mirror, ctrl.Result{RequeueAfter: 5 * time.Second}, func() {
				mirror.Status.Phase = mirrorv1alpha1.PhasePublishing
				setCondition(mirror, conditionProgressing, metav1.ConditionTrue, "ServingRollout", fmt.Sprintf("publishing PVC %s", mirror.Status.PendingPVC))
			})
		}
	}

	return r.publishPendingSnapshot(ctx, mirror)
}

func (r *MirrorReconciler) publishPendingSnapshot(ctx context.Context, mirror *mirrorv1alpha1.Mirror) (ctrl.Result, error) {
	now := r.now()
	interval := mirror.Spec.Sync.Interval.Duration
	// A successful publication ends the failure streak: fast retries (if any
	// were queued) restart from zero. Failed Jobs are also pruned down to
	// spec.sync.keepFailedJobs — every terminal state is a pruning point.
	if err := r.pruneFailedJobs(ctx, mirror); err != nil {
		return ctrl.Result{}, err
	}
	if r.Recorder != nil {
		r.Recorder.Eventf(mirror, corev1.EventTypeNormal, "SnapshotPublished", "Published PVC %s", mirror.Status.PendingPVC)
	}
	return r.patchStatusWithResult(ctx, mirror, ctrl.Result{RequeueAfter: interval}, func() {
		pvc := mirror.Status.PendingPVC
		snapshot := mirror.Status.PendingSnapshot
		mirror.Status.ActivePVC = pvc
		mirror.Status.ActiveSnapshot = snapshot
		mirror.Status.PendingSyncTimestamp = 0
		mirror.Status.PendingPVC = ""
		mirror.Status.PendingSnapshot = ""
		mirror.Status.PendingJob = ""
		mirror.Status.LastHandledSyncRequest = mirror.Status.PendingSyncRequest
		mirror.Status.PendingSyncRequest = ""
		mirror.Status.Phase = mirrorv1alpha1.PhaseReady
		mirror.Status.LastPublishedAt = timePtr(now)
		mirror.Status.NextSyncAt = timePtr(now.Add(interval))
		mirror.Status.ConsecutiveFailures = 0
		if mirror.Status.LastSync != nil {
			mirror.Status.LastSync.Phase = mirrorv1alpha1.SyncPhaseSucceeded
			mirror.Status.LastSync.FinishedAt = timePtr(now)
			mirror.Status.LastSync.Message = fmt.Sprintf("published PVC %s", pvc)
		}
		setCondition(mirror, conditionReady, metav1.ConditionTrue, "Published", fmt.Sprintf("PVC %s is published", pvc))
		setCondition(mirror, conditionProgressing, metav1.ConditionFalse, "Published", "synchronization and publication completed")
		setCondition(mirror, conditionDegraded, metav1.ConditionFalse, "Published", "")
	})
}

// failPendingSnapshot records a failed synchronization run and queues the
// next attempt: while status.consecutiveFailures is below
// spec.sync.failureRetryLimit the retry is queued after spec.sync.retryInterval
// (fast retries); afterwards the next attempt waits for the regular
// spec.sync.interval and the counter stops incrementing.
func (r *MirrorReconciler) failPendingSnapshot(ctx context.Context, mirror *mirrorv1alpha1.Mirror, reason, message string) (ctrl.Result, error) {
	now := r.now()
	interval := mirror.Spec.Sync.Interval.Duration
	retryInterval := mirror.Spec.Sync.RetryInterval.Duration
	if retryInterval <= 0 {
		// Defensive default mirroring the CRD default (15m): a zero or
		// negative retryInterval must not collapse the schedule onto "now".
		retryInterval = interval
	}
	limit := mirror.Spec.Sync.FailureRetryLimit
	failures := mirror.Status.ConsecutiveFailures
	var nextAttempt time.Time
	if failures < limit {
		failures++
		nextAttempt = now.Add(retryInterval)
	} else {
		nextAttempt = now.Add(interval)
	}
	// Every terminal state is a failed-Job pruning point.
	if err := r.pruneFailedJobs(ctx, mirror); err != nil {
		return ctrl.Result{}, err
	}
	if r.Recorder != nil {
		r.Recorder.Eventf(mirror, corev1.EventTypeWarning, reason, "Synchronization run failed: %s", message)
	}
	progressingMessage := fmt.Sprintf("%s; %d consecutive failure(s); retry queued for %s", message, failures, nextAttempt.Format(time.RFC3339))
	if failures >= limit {
		progressingMessage = fmt.Sprintf("%s; retry limit %d reached after %d consecutive failure(s); next attempt scheduled for %s", message, limit, failures, nextAttempt.Format(time.RFC3339))
	}
	return r.patchStatusWithResult(ctx, mirror, ctrl.Result{RequeueAfter: nextAttempt.Sub(now)}, func() {
		mirror.Status.PendingSyncTimestamp = 0
		mirror.Status.PendingPVC = ""
		mirror.Status.PendingSnapshot = ""
		mirror.Status.PendingJob = ""
		mirror.Status.LastHandledSyncRequest = mirror.Status.PendingSyncRequest
		mirror.Status.PendingSyncRequest = ""
		mirror.Status.Phase = mirrorv1alpha1.PhaseDegraded
		mirror.Status.ConsecutiveFailures = failures
		mirror.Status.NextSyncAt = timePtr(nextAttempt)
		if mirror.Status.LastSync != nil {
			mirror.Status.LastSync.Phase = mirrorv1alpha1.SyncPhaseFailed
			mirror.Status.LastSync.FinishedAt = timePtr(now)
			mirror.Status.LastSync.Message = message
		}
		setCondition(mirror, conditionReady, conditionStatus(mirror.Status.ActivePVC != ""), reason, message)
		setCondition(mirror, conditionProgressing, metav1.ConditionFalse, reason, progressingMessage)
		setCondition(mirror, conditionDegraded, metav1.ConditionTrue, reason, message)
	})
}

func (r *MirrorReconciler) reconcileDelete(ctx context.Context, mirror *mirrorv1alpha1.Mirror) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(mirror, MirrorFinalizer) {
		return ctrl.Result{}, nil
	}

	base, err := childBase(mirror.Name)
	if err != nil {
		return ctrl.Result{}, err
	}

	claims := &corev1.PersistentVolumeClaimList{}
	if err := r.List(ctx, claims, client.InNamespace(mirror.Namespace), client.MatchingLabels{MirrorLabel: base}); err != nil {
		return ctrl.Result{}, err
	}
	if len(claims.Items) > 0 {
		for i := range claims.Items {
			if err := r.Delete(ctx, &claims.Items[i]); err != nil && !apierrors.IsNotFound(err) {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
	}

	snapshots := &snapshotv1.VolumeSnapshotList{}
	if err := r.List(ctx, snapshots, client.InNamespace(mirror.Namespace), client.MatchingLabels{MirrorLabel: base}); err != nil {
		return ctrl.Result{}, err
	}
	if len(snapshots.Items) > 0 {
		for i := range snapshots.Items {
			if err := r.Delete(ctx, &snapshots.Items[i]); err != nil && !apierrors.IsNotFound(err) {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
	}

	before := mirror.DeepCopy()
	controllerutil.RemoveFinalizer(mirror, MirrorFinalizer)
	return ctrl.Result{}, r.Patch(ctx, mirror, client.MergeFrom(before))
}

func (r *MirrorReconciler) patchStatus(ctx context.Context, mirror *mirrorv1alpha1.Mirror, mutate func()) (ctrl.Result, error) {
	return r.patchStatusWithResult(ctx, mirror, ctrl.Result{}, mutate)
}

func (r *MirrorReconciler) patchStatusWithResult(ctx context.Context, mirror *mirrorv1alpha1.Mirror, result ctrl.Result, mutate func()) (ctrl.Result, error) {
	before := mirror.DeepCopy()
	mutate()
	if err := r.Status().Patch(ctx, mirror, client.MergeFrom(before)); err != nil {
		return ctrl.Result{}, err
	}
	return result, nil
}

func (r *MirrorReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now().UTC()
	}
	return time.Now().UTC()
}

func timePtr(value time.Time) *metav1.Time {
	t := metav1.NewTime(value.UTC())
	return &t
}

func setCondition(mirror *mirrorv1alpha1.Mirror, conditionType string, status metav1.ConditionStatus, reason, message string) {
	meta.SetStatusCondition(&mirror.Status.Conditions, metav1.Condition{
		Type:               conditionType,
		Status:             status,
		ObservedGeneration: mirror.Generation,
		Reason:             reason,
		Message:            message,
	})
}

func conditionStatus(value bool) metav1.ConditionStatus {
	if value {
		return metav1.ConditionTrue
	}
	return metav1.ConditionFalse
}

// validateDerivedName rejects a CR name if the derived child object name for
// the given (longest) suffix would exceed the DNS-1123 label limit.
func validateDerivedName(name, longestSuffix string) *field.Error {
	base, err := childBase(name)
	if err != nil {
		return field.Invalid(field.NewPath("metadata", "name"), name, err.Error())
	}
	derived, err := resourceName(base, longestSuffix)
	if err != nil {
		return field.Invalid(field.NewPath("metadata", "name"), name, fmt.Sprintf("%s (derived name: %s)", err.Error(), derived))
	}
	return nil
}

// mirrorLongestNameSuffix is the longest suffix ever appended to a Mirror's
// child names: the decimal Unix seconds timestamp (bounded by
// maxSyncTimestampLen) prefixed with "sync-". Checking it covers every other
// derived name (snap-<ts>, publish-<protocol> are shorter or equal).
func mirrorLongestNameSuffix() string {
	return "sync-" + strings.Repeat("0", maxSyncTimestampLen)
}

func validateMirror(mirror *mirrorv1alpha1.Mirror) field.ErrorList {
	path := field.NewPath("spec")
	var errs field.ErrorList
	if err := validateDerivedName(mirror.Name, mirrorLongestNameSuffix()); err != nil {
		errs = append(errs, err)
	}
	if mirror.Spec.Info.Type != "sync" {
		errs = append(errs, field.NotSupported(path.Child("info", "type"), mirror.Spec.Info.Type, []string{"sync"}))
	}
	if mirror.Spec.Sync.Interval.Duration <= 0 {
		errs = append(errs, field.Invalid(path.Child("sync", "interval"), mirror.Spec.Sync.Interval.Duration.String(), "must be greater than zero"))
	}
	if mirror.Spec.Sync.RetryInterval.Duration <= 0 {
		errs = append(errs, field.Invalid(path.Child("sync", "retryInterval"), mirror.Spec.Sync.RetryInterval.Duration.String(), "must be greater than zero"))
	}
	if mirror.Spec.Sync.Timeout.Duration <= 0 {
		errs = append(errs, field.Invalid(path.Child("sync", "timeout"), mirror.Spec.Sync.Timeout.Duration.String(), "must be greater than zero"))
	}
	if mirror.Spec.Sync.Image == "" {
		errs = append(errs, field.Required(path.Child("sync", "image"), "must not be empty"))
	}
	if len(mirror.Spec.Sync.Command) == 0 {
		errs = append(errs, field.Required(path.Child("sync", "command"), "must contain an executable"))
	}
	if mirror.Spec.Storage.StorageClassName == "" {
		errs = append(errs, field.Required(path.Child("storage", "storageClassName"), "must not be empty"))
	}
	if mirror.Spec.Storage.Capacity.IsZero() || mirror.Spec.Storage.Capacity.Sign() < 0 {
		errs = append(errs, field.Invalid(path.Child("storage", "capacity"), mirror.Spec.Storage.Capacity.String(), "must be greater than zero"))
	}
	if mirror.Spec.Storage.VolumeSnapshotClassName == "" {
		errs = append(errs, field.Required(path.Child("storage", "volumeSnapshotClassName"), "is required for atomic publication"))
	}
	if mirror.Spec.Storage.NodeName != "" && mirror.Spec.Sync.NodeName != "" && mirror.Spec.Storage.NodeName != mirror.Spec.Sync.NodeName {
		errs = append(errs, field.Invalid(path.Child("sync", "nodeName"), mirror.Spec.Sync.NodeName, "must match storage.nodeName for a local PV"))
	}
	for key, storageValue := range mirror.Spec.Storage.NodeSelector {
		if syncValue, exists := mirror.Spec.Sync.NodeSelector[key]; exists && syncValue != storageValue {
			errs = append(errs, field.Invalid(path.Child("sync", "nodeSelector").Key(key), syncValue, "must match storage.nodeSelector for the same key"))
		}
	}
	effectiveNodeName := mirror.Spec.Storage.NodeName
	if effectiveNodeName == "" {
		effectiveNodeName = mirror.Spec.Sync.NodeName
	}
	if effectiveNodeName != "" {
		if hostname, exists := mirror.Spec.Storage.NodeSelector[corev1.LabelHostname]; exists && hostname != effectiveNodeName {
			errs = append(errs, field.Invalid(path.Child("storage", "nodeSelector").Key(corev1.LabelHostname), hostname, "must match the effective nodeName"))
		}
		if hostname, exists := mirror.Spec.Sync.NodeSelector[corev1.LabelHostname]; exists && hostname != effectiveNodeName {
			errs = append(errs, field.Invalid(path.Child("sync", "nodeSelector").Key(corev1.LabelHostname), hostname, "must match the effective nodeName"))
		}
	}
	if len(mirror.Spec.Services) > 0 {
		errs = append(errs, validateServices(mirror.Spec.Services, path.Child("services"))...)
		// Multi-replica publishing requires forced co-location: with a
		// ReadWriteOnce data PVC all publish pods must run on the storage
		// node, which the controller enforces only when spec.storage.nodeName
		// pins the node (it becomes a kubernetes.io/hostname node selector).
		if mirror.Spec.Storage.NodeName == "" {
			for i := range mirror.Spec.Services {
				if serviceReplicas(&mirror.Spec.Services[i]) > 1 {
					errs = append(errs, field.Invalid(path.Child("storage", "nodeName"), "",
						"must be set when any spec.services[].replicas is greater than 1: a ReadWriteOnce data PVC can only be mounted by pods on its node, so multi-replica publishing requires the controller to pin every publish pod to the storage node"))
					break
				}
			}
		}
	}
	for i := range mirror.Spec.Sync.Volumes {
		volume := &mirror.Spec.Sync.Volumes[i]
		volumePath := path.Child("sync", "volumes").Index(i)
		if volume.Name == "" {
			errs = append(errs, field.Required(volumePath.Child("name"), "must not be empty"))
		}
		if volume.MountPath == "" || volume.MountPath[0] != '/' {
			errs = append(errs, field.Invalid(volumePath.Child("mountPath"), volume.MountPath, "must be an absolute path"))
		}
		sources := 0
		if volume.ConfigMap != nil {
			sources++
		}
		if volume.Secret != nil {
			sources++
		}
		if sources != 1 {
			errs = append(errs, field.Invalid(volumePath, volume.Name, "exactly one of configMap or secret must be set"))
		}
	}
	return errs
}

// publishEnabled reports whether spec.services declares at least one entry
// (a mirror with no services syncs but publishes nothing).
func publishEnabled(mirror *mirrorv1alpha1.Mirror) bool {
	return len(mirror.Spec.Services) > 0
}

func jobSucceeded(job *batchv1.Job) bool {
	for _, condition := range job.Status.Conditions {
		if condition.Type == batchv1.JobComplete && condition.Status == corev1.ConditionTrue {
			return true
		}
	}
	return job.Status.Succeeded > 0
}

func jobFailed(job *batchv1.Job) bool {
	for _, condition := range job.Status.Conditions {
		if condition.Type == batchv1.JobFailed && condition.Status == corev1.ConditionTrue {
			return true
		}
	}
	return job.Status.Failed > 0 && job.Status.Active == 0
}

func jobFailureMessage(job *batchv1.Job) string {
	for _, condition := range job.Status.Conditions {
		if condition.Type == batchv1.JobFailed && condition.Status == corev1.ConditionTrue {
			if condition.Message != "" {
				return condition.Message
			}
			return condition.Reason
		}
	}
	return fmt.Sprintf("Job %s failed", job.Name)
}
