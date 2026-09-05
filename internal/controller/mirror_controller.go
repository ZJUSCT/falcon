package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"

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
	SyncTimestampLabel = "mirrors.zjusct.io/sync-timestamp"
	// ComponentLabel is the standard recommended component label (values:
	// sync/snapshot/publish-data/publish-http/publish-rsync/proxy-cache); it
	// replaced the custom mirrors.zjusct.io/role label. Publish children use
	// the per-service-key value (publish-<key>) so each Service selects only
	// its own pods.
	ComponentLabel = "app.kubernetes.io/component"

	conditionReady       = "Ready"
	conditionProgressing = "Progressing"
	conditionDegraded    = "Degraded"
)

type MirrorReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
	Now      func() time.Time
	// Config is the loaded controller configuration (required). The publish
	// section (config publish.*) gates publish HTTPRoute generation, the sync
	// section the global concurrency cap.
	Config *config.Config
	// SyncLimiter enforces the global cap of concurrently running sync Jobs
	// (config sync.maxConcurrent). Required.
	SyncLimiter *SyncLimiter
	// UsageReader optionally reports the on-disk usage of a publish PVC as
	// seen by the kubelet running its publish pod; it backs
	// status.sizeBytes (best-effort accounting, see publishPVCUsage). When
	// nil, size accounting is skipped entirely.
	UsageReader PVCUsageReader
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
//
// nodes/proxy is cluster-scoped and cannot live in the namespaced Role: the
// chart grants it through the controller.rbac.nodeStats ClusterRole
// (kubelet stats summary via the API server node proxy — the source of
// status.sizeBytes; the apiserver only registers a nodes/proxy subresource,
// there is no nodes/stats route).
// +kubebuilder:rbac:groups="",resources=nodes/proxy,verbs=get

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

func (r *MirrorReconciler) Reconcile(ctx context.Context, req ctrl.Request) (result ctrl.Result, reconcileErr error) {
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
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}
	defer func() {
		result, reconcileErr = r.handleDerivedResourceInvalid(ctx, mirror, result, reconcileErr)
	}()

	// Removing a service is an operational request, not merely a validation
	// concern. Honour it even when another part of the new spec is invalid so
	// a broken sync template cannot accidentally keep an endpoint online.
	if err := r.cleanupDisabledPublishChildren(ctx, mirror); err != nil {
		return ctrl.Result{}, err
	}

	if errs := validateMirror(mirror); len(errs) > 0 {
		message := errs.ToAggregate().Error()
		logger.Info("Mirror specification is invalid", "errors", message)
		return r.patchStatus(ctx, mirror, func() {
			mirror.Status.ObservedGeneration = mirror.Generation
			setCondition(mirror, conditionReady, conditionStatus(mirrorWasReady(mirror)), "InvalidSpec", message)
			setCondition(mirror, conditionProgressing, metav1.ConditionFalse, "InvalidSpec", message)
			setCondition(mirror, conditionDegraded, metav1.ConditionTrue, "InvalidSpec", message)
		})
	}

	publication, err := r.reconcileActivePublication(ctx, mirror)
	if err != nil {
		return ctrl.Result{}, err
	}
	if mirror.Status.CurrentSync != nil {
		return r.reconcilePendingSnapshot(ctx, mirror, publication)
	}

	if mirror.Spec.Sync.Paused {
		return r.patchStatus(ctx, mirror, func() {
			mirror.Status.ObservedGeneration = mirror.Generation
			applyMirrorConditions(mirror, publication, false, "Paused", "new synchronization runs are paused", nil)
		})
	}

	now := r.now()
	request := mirror.Annotations[SyncRequestAnnotation]
	manualDue := request != "" && request != mirror.Status.LastHandledSyncRequest
	specDue := mirror.Status.ActivePVC != "" && mirror.Status.ObservedGeneration != mirror.Generation
	bootstrapDue := mirror.Status.ActivePVC == "" && mirror.Status.LastSync == nil
	scheduleDue := mirror.Status.NextSyncAt != nil && !mirror.Status.NextSyncAt.After(now)

	if manualDue || specDue || bootstrapDue || scheduleDue {
		return r.startSync(ctx, mirror, request, publication)
	}

	if mirror.Status.ActivePVC != "" {
		if err := r.pruneOldSnapshots(ctx, mirror); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Publish PVC content is immutable, so the kubelet-reported usage recorded
	// once stays accurate forever: backfill only while sizeBytes is still
	// unset, best-effort as everywhere else.
	var pvcUsage int64
	pvcUsageOK := false
	if mirror.Status.ActivePVC != "" && mirror.Status.SizeBytes == 0 {
		pvcUsage, pvcUsageOK = r.publishPVCUsage(ctx, mirror, mirror.Status.ActivePVC)
	}

	nextResult := ctrl.Result{}
	if mirror.Status.NextSyncAt != nil {
		nextResult.RequeueAfter = time.Until(mirror.Status.NextSyncAt.Time)
		if nextResult.RequeueAfter < time.Second {
			nextResult.RequeueAfter = time.Second
		}
	}
	return r.patchStatusWithResult(ctx, mirror, nextResult, func() {
		mirror.Status.ObservedGeneration = mirror.Generation
		if pvcUsageOK {
			mirror.Status.SizeBytes = pvcUsage
		}
		applyMirrorConditions(mirror, publication, false, "Idle", "no synchronization is running", nil)
	})
}

// startSync begins a new synchronization run. The Unix seconds timestamp is
// allocated ONCE here, when the controller creates the sync task, and
// propagates from status.currentSync.startedAt into every derived name: the
// sync Job `<base>-sync-<ts>`, the VolumeSnapshot and the publish PVC (which
// share the name `<base>-snap-<ts>`). The sync PVC has the fixed name
// `<base>-sync` (no timestamp) and is reused across runs. Whether the
// timestamp is free (no existing Job/PVC/VolumeSnapshot carrying it) is
// checked when the sync Job is created — see lookupOrCreateSyncJob.
func (r *MirrorReconciler) startSync(ctx context.Context, mirror *mirrorv1alpha1.Mirror, request string, publication publicationHealth) (ctrl.Result, error) {
	now := r.now()
	timestamp := now.Unix()
	base := childBase(mirror.Name)
	syncPVCName := mirror.Status.WorkPVC
	if syncPVCName == "" {
		syncPVCName = resourceName(base, "sync")
	}
	jobName := resourceName(base, fmt.Sprintf("sync-%d", timestamp))

	if r.Recorder != nil {
		r.Recorder.Eventf(mirror, corev1.EventTypeNormal, "SynchronizationStarted", "Starting synchronization run with Job %s", jobName)
	}
	return r.patchStatusWithResult(ctx, mirror, ctrl.Result{RequeueAfter: time.Second}, func() {
		mirror.Status.ObservedGeneration = mirror.Generation
		mirror.Status.WorkPVC = syncPVCName
		mirror.Status.CurrentSync = &mirrorv1alpha1.MirrorCurrentSyncStatus{
			StartedAt:   timePtr(now),
			SyncRequest: request,
		}
		mirror.Status.NextSyncAt = nil
		applyMirrorConditions(mirror, publication, true, "SynchronizationStarted", "preparing synchronization run", nil)
	})
}

func currentSyncTimestamp(mirror *mirrorv1alpha1.Mirror) int64 {
	if mirror.Status.CurrentSync == nil || mirror.Status.CurrentSync.StartedAt == nil {
		return 0
	}
	return mirror.Status.CurrentSync.StartedAt.Unix()
}

func currentSyncJobName(mirror *mirrorv1alpha1.Mirror) string {
	return resourceName(childBase(mirror.Name), fmt.Sprintf("sync-%d", currentSyncTimestamp(mirror)))
}

func currentSyncSnapshotName(mirror *mirrorv1alpha1.Mirror) string {
	return resourceName(childBase(mirror.Name), fmt.Sprintf("snap-%d", currentSyncTimestamp(mirror)))
}

func (r *MirrorReconciler) reconcilePendingSnapshot(ctx context.Context, mirror *mirrorv1alpha1.Mirror, publication publicationHealth) (ctrl.Result, error) {
	if err := r.ensureSyncPVC(ctx, mirror); err != nil {
		return ctrl.Result{}, err
	}
	job, err := r.lookupOrCreateSyncJob(ctx, mirror)
	if errors.Is(err, errSyncQueued) {
		// The global sync concurrency cap (sync.maxConcurrent) is reached:
		// leave the Job uncreated and retry shortly. The queued sync may
		// start later than status.nextSyncAt.
		return r.patchStatusWithResult(ctx, mirror, ctrl.Result{RequeueAfter: 5 * time.Second}, func() {
			applyMirrorConditions(mirror, publication, true, "SyncQueued",
				fmt.Sprintf("sync Job %s is queued: global sync concurrency limit (%d) reached", currentSyncJobName(mirror), r.Config.Sync.MaxConcurrent), nil)
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
			message := err.Error()
			applyMirrorConditions(mirror, publication, false, "SnapshotTimestampConflict", message,
				&conditionFailure{reason: "SnapshotTimestampConflict", message: message})
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
		return r.failPendingSnapshot(ctx, mirror, publication, "SyncJobFailed", message)
	}
	if !jobSucceeded(job) {
		return r.patchStatusWithResult(ctx, mirror, ctrl.Result{RequeueAfter: 5 * time.Second}, func() {
			applyMirrorConditions(mirror, publication, true, "SyncJobRunning", fmt.Sprintf("Job %s is running", job.Name), nil)
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
		return r.failPendingSnapshot(ctx, mirror, publication, "SnapshotFailed", message)
	}
	if !ready {
		return r.patchStatusWithResult(ctx, mirror, ctrl.Result{RequeueAfter: 5 * time.Second}, func() {
			applyMirrorConditions(mirror, publication, true, "Snapshotting", fmt.Sprintf("snapshotting completed sync PVC %s", mirror.Status.WorkPVC), nil)
		})
	}

	if err := r.ensurePublishPVC(ctx, mirror); err != nil {
		return ctrl.Result{}, err
	}

	if publishEnabled(mirror) {
		ready, err := r.ensurePublish(ctx, mirror, currentSyncSnapshotName(mirror))
		if err != nil {
			return ctrl.Result{}, err
		}
		if !ready {
			return r.patchStatusWithResult(ctx, mirror, ctrl.Result{RequeueAfter: 5 * time.Second}, func() {
				applyMirrorConditions(mirror, publication, true, "PublishRollout", fmt.Sprintf("publishing PVC %s", currentSyncSnapshotName(mirror)), nil)
			})
		}
	}

	return r.publishPendingSnapshot(ctx, mirror, publication)
}

func (r *MirrorReconciler) publishPendingSnapshot(ctx context.Context, mirror *mirrorv1alpha1.Mirror, publication publicationHealth) (ctrl.Result, error) {
	now := r.now()
	interval := mirror.Spec.Sync.Interval.Duration
	hadActivePublication := mirror.Status.ActivePVC != ""
	result := ctrl.Result{RequeueAfter: interval}
	if publishHTTPEnabled(mirror) && !publication.ready {
		result.RequeueAfter = 5 * time.Second
	}
	// A successful publication ends the failure streak: fast retries (if any
	// were queued) restart from zero. Failed Jobs are also pruned down to
	// spec.sync.keepFailedJobs — every terminal state is a pruning point.
	if err := r.pruneFailedJobs(ctx, mirror); err != nil {
		return ctrl.Result{}, err
	}
	if r.Recorder != nil {
		r.Recorder.Eventf(mirror, corev1.EventTypeNormal, "SnapshotPublished", "Published PVC %s", currentSyncSnapshotName(mirror))
	}
	// Best-effort usage accounting of the freshly published PVC, folded into
	// the activation patch. It must never disturb publication: a miss (the
	// rollout not having mounted the new PVC yet) just leaves sizeBytes empty
	// for the idle path to backfill.
	pvc := currentSyncSnapshotName(mirror)
	jobName := currentSyncJobName(mirror)
	startedAt := mirror.Status.CurrentSync.StartedAt.DeepCopy()
	syncRequest := mirror.Status.CurrentSync.SyncRequest
	pvcUsage, pvcUsageOK := r.publishPVCUsage(ctx, mirror, pvc)
	return r.patchStatusWithResult(ctx, mirror, result, func() {
		mirror.Status.ActivePVC = pvc
		mirror.Status.ActiveSnapshot = pvc
		mirror.Status.CurrentSync = nil
		mirror.Status.LastHandledSyncRequest = syncRequest
		mirror.Status.LastPublishedAt = timePtr(now)
		mirror.Status.NextSyncAt = timePtr(now.Add(interval))
		mirror.Status.ConsecutiveFailures = 0
		if pvcUsageOK {
			mirror.Status.SizeBytes = pvcUsage
		}
		mirror.Status.LastSync = &mirrorv1alpha1.MirrorSyncStatus{
			JobName:    jobName,
			Phase:      mirrorv1alpha1.SyncPhaseSucceeded,
			StartedAt:  startedAt,
			FinishedAt: timePtr(now),
			Message:    fmt.Sprintf("published PVC %s", pvc),
		}
		if publishHTTPEnabled(mirror) && !publication.ready {
			reason := publication.reason
			message := publication.message
			if !hadActivePublication || reason == "Pending" {
				reason = "HTTPRoutePending"
				message = "waiting for HTTPRoute Accepted=True and ResolvedRefs=True"
			}
			setCondition(mirror, conditionReady, metav1.ConditionFalse, reason, message)
			setCondition(mirror, conditionProgressing, conditionStatus(publication.progressing || publication.failure == nil), reason, message)
			if publication.failure != nil {
				setCondition(mirror, conditionDegraded, metav1.ConditionTrue, publication.failure.reason, publication.failure.message)
			} else {
				setCondition(mirror, conditionDegraded, metav1.ConditionFalse, "AsExpected", "")
			}
		} else {
			setCondition(mirror, conditionReady, metav1.ConditionTrue, "Published", fmt.Sprintf("PVC %s is published", pvc))
			setCondition(mirror, conditionProgressing, metav1.ConditionFalse, "Published", "synchronization and publication completed")
			setCondition(mirror, conditionDegraded, metav1.ConditionFalse, "AsExpected", "")
		}
	})
}

// failPendingSnapshot records a failed synchronization run and queues the
// next attempt: while status.consecutiveFailures is below
// spec.sync.failureRetryLimit the retry is queued after spec.sync.retryInterval
// (fast retries); afterwards the next attempt waits for the regular
// spec.sync.interval and the counter stops incrementing.
func (r *MirrorReconciler) failPendingSnapshot(ctx context.Context, mirror *mirrorv1alpha1.Mirror, publication publicationHealth, reason, message string) (ctrl.Result, error) {
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
	jobName := currentSyncJobName(mirror)
	startedAt := mirror.Status.CurrentSync.StartedAt.DeepCopy()
	syncRequest := mirror.Status.CurrentSync.SyncRequest
	progressingMessage := fmt.Sprintf("%s; %d consecutive failure(s); retry queued for %s", message, failures, nextAttempt.Format(time.RFC3339))
	if failures >= limit {
		progressingMessage = fmt.Sprintf("%s; retry limit %d reached after %d consecutive failure(s); next attempt scheduled for %s", message, limit, failures, nextAttempt.Format(time.RFC3339))
	}
	return r.patchStatusWithResult(ctx, mirror, ctrl.Result{RequeueAfter: nextAttempt.Sub(now)}, func() {
		mirror.Status.CurrentSync = nil
		mirror.Status.LastHandledSyncRequest = syncRequest
		mirror.Status.ConsecutiveFailures = failures
		mirror.Status.NextSyncAt = timePtr(nextAttempt)
		mirror.Status.LastSync = &mirrorv1alpha1.MirrorSyncStatus{
			JobName:    jobName,
			Phase:      mirrorv1alpha1.SyncPhaseFailed,
			StartedAt:  startedAt,
			FinishedAt: timePtr(now),
			Message:    message,
		}
		applyMirrorConditions(mirror, publication, false, reason, progressingMessage,
			&conditionFailure{reason: reason, message: message})
	})
}

func (r *MirrorReconciler) reconcileDelete(ctx context.Context, mirror *mirrorv1alpha1.Mirror) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(mirror, MirrorFinalizer) {
		return ctrl.Result{}, nil
	}

	base := childBase(mirror.Name)

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

func mirrorWasReady(mirror *mirrorv1alpha1.Mirror) bool {
	condition := meta.FindStatusCondition(mirror.Status.Conditions, conditionReady)
	return condition != nil && condition.Status == metav1.ConditionTrue
}

func proxyWasReady(proxy *mirrorv1alpha1.ProxyMirror) bool {
	condition := meta.FindStatusCondition(proxy.Status.Conditions, conditionReady)
	return condition != nil && condition.Status == metav1.ConditionTrue
}

type conditionFailure struct {
	reason  string
	message string
}

// publicationHealth describes the currently active generation independently
// of a newer synchronization transaction. In particular, a rolling
// Deployment can be available (Ready) and still converging (Progressing).
type publicationHealth struct {
	ready       bool
	progressing bool
	reason      string
	message     string
	failure     *conditionFailure
}

// reconcileActivePublication reconciles an idle active generation, but only
// observes it while CurrentSync is non-nil. Re-applying ActivePVC during a
// pending publication would otherwise revert the Deployment away from the new
// PVC on every reconcile. Availability is intentionally weaker than rollout
// convergence: maxUnavailable=0 keeps an old pod serving while the new pod is
// coming up.
func (r *MirrorReconciler) reconcileActivePublication(ctx context.Context, mirror *mirrorv1alpha1.Mirror) (publicationHealth, error) {
	if mirror.Status.ActivePVC == "" {
		return publicationHealth{reason: "Pending", message: "waiting for the initial synchronization"}, nil
	}
	if !publishEnabled(mirror) {
		return publicationHealth{ready: true, reason: "Published", message: fmt.Sprintf("PVC %s is active", mirror.Status.ActivePVC)}, nil
	}

	if mirror.Status.CurrentSync == nil {
		if _, err := r.ensurePublish(ctx, mirror, mirror.Status.ActivePVC); err != nil {
			return publicationHealth{}, err
		}
	}

	available, converged, err := observePublishChildren(ctx, r.Client, mirror)
	if err != nil {
		return publicationHealth{}, err
	}
	health := publicationHealth{
		ready:       available,
		progressing: !converged,
		reason:      "Published",
		message:     "all requested publish Deployments are available",
	}
	if !available {
		health.reason = "PublishUnavailable"
		health.message = "waiting for all requested publish Deployments and Services to become available"
	} else if !converged {
		health.reason = "PublishRollout"
		health.message = "the active publication remains available while a publish Deployment is rolling out"
	}

	if !publishHTTPEnabled(mirror) {
		return health, nil
	}
	if !r.Config.PublishEnabled() {
		health.ready = false
		health.progressing = false
		health.reason = "HTTPRouteDisabled"
		health.message = "HTTP publishing is requested but route generation is disabled"
		health.failure = &conditionFailure{reason: health.reason, message: health.message}
		return health, nil
	}
	if err := ensurePublishedMirrorRoute(ctx, r, mirror); err != nil {
		return publicationHealth{}, err
	}
	routeState, routeMessage, err := publishRouteHealth(ctx, r.Client, mirror)
	if err != nil {
		return publicationHealth{}, err
	}
	switch routeState {
	case publishRouteRejected:
		health.ready = false
		health.progressing = false
		health.reason = "HTTPRouteRejected"
		health.message = routeMessage
		health.failure = &conditionFailure{reason: health.reason, message: routeMessage}
		if r.Recorder != nil {
			r.Recorder.Event(mirror, corev1.EventTypeWarning, "HTTPRouteRejected", routeMessage)
		}
	case publishRoutePending:
		health.ready = false
		health.progressing = true
		health.reason = "HTTPRoutePending"
		health.message = routeMessage
	}
	return health, nil
}

// applyMirrorConditions is the single condition projection for a valid
// Mirror. Publication health, current synchronization activity and the last
// completed synchronization are orthogonal facts, so Ready and Degraded may
// both legitimately be true.
func applyMirrorConditions(mirror *mirrorv1alpha1.Mirror, publication publicationHealth, syncProgressing bool, progressReason, progressMessage string, currentFailure *conditionFailure) {
	setCondition(mirror, conditionReady, conditionStatus(publication.ready), publication.reason, publication.message)
	switch {
	case syncProgressing:
		setCondition(mirror, conditionProgressing, metav1.ConditionTrue, progressReason, progressMessage)
	case publication.progressing:
		setCondition(mirror, conditionProgressing, metav1.ConditionTrue, publication.reason, publication.message)
	default:
		setCondition(mirror, conditionProgressing, metav1.ConditionFalse, progressReason, progressMessage)
	}

	failure := currentFailure
	if failure == nil {
		failure = publication.failure
	}
	if failure == nil && mirror.Status.LastSync != nil && mirror.Status.LastSync.Phase == mirrorv1alpha1.SyncPhaseFailed {
		failure = &conditionFailure{reason: "SynchronizationFailed", message: mirror.Status.LastSync.Message}
	}
	if failure != nil {
		setCondition(mirror, conditionDegraded, metav1.ConditionTrue, failure.reason, failure.message)
	} else {
		setCondition(mirror, conditionDegraded, metav1.ConditionFalse, "AsExpected", "")
	}
}

func validateMirror(mirror *mirrorv1alpha1.Mirror) field.ErrorList {
	path := field.NewPath("spec")
	var errs field.ErrorList
	if mirror.Spec.Sync.Interval.Duration <= 0 {
		errs = append(errs, field.Invalid(path.Child("sync", "interval"), mirror.Spec.Sync.Interval.Duration.String(), "must be greater than zero"))
	}
	if mirror.Spec.Sync.RetryInterval.Duration <= 0 {
		errs = append(errs, field.Invalid(path.Child("sync", "retryInterval"), mirror.Spec.Sync.RetryInterval.Duration.String(), "must be greater than zero"))
	}
	if mirror.Spec.Sync.Timeout.Duration <= 0 {
		errs = append(errs, field.Invalid(path.Child("sync", "timeout"), mirror.Spec.Sync.Timeout.Duration.String(), "must be greater than zero"))
	}
	// sync.podTemplate: at least one container with an image (the former
	// sync.image/sync.command requirements moved into the template), and no
	// user volume clashing with the injected writable sync-data volume.
	syncTemplatePath := path.Child("sync", "podTemplate")
	if len(mirror.Spec.Sync.PodTemplate.Spec.Containers) == 0 {
		errs = append(errs, field.Required(syncTemplatePath.Child("spec", "containers"), "must declare at least one container"))
	} else if mirror.Spec.Sync.PodTemplate.Spec.Containers[0].Image == "" {
		errs = append(errs, field.Required(syncTemplatePath.Child("spec", "containers").Index(0).Child("image"), "must not be empty"))
	}
	for i := range mirror.Spec.Sync.PodTemplate.Spec.Volumes {
		if mirror.Spec.Sync.PodTemplate.Spec.Volumes[i].Name == SyncDataVolumeName {
			errs = append(errs, field.Invalid(syncTemplatePath.Child("spec", "volumes").Index(i).Child("name"), SyncDataVolumeName,
				"this volume name is reserved: the controller injects it itself"))
		}
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
	services := mirror.Spec.Publish
	servicesPath := path.Child("services")
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
		errs = append(errs, validateMirrorService(entry.spec, servicesPath.Child(entry.key))...)
	}
	// Alias paths of an ENABLED http service (an absent key may park
	// anything).
	if services.HTTP != nil {
		errs = append(errs, validateHTTPAliases(services.HTTP, mirror.Name, servicesPath.Child("http", "aliases"))...)
	}
	// There is no placement validation: node locality is K8s-native on the
	// sync side (WFFC + bound-PV nodeAffinity) and PV-derived on the publish
	// side (publishPlacement). Multi-replica publishing on shared (RWX)
	// storage is a legal extension — no nodeName requirement exists any more.
	return errs
}

// publishEnabled reports whether at least one spec.publish key is enabled
// (a mirror with everything disabled syncs but publishes nothing).
func publishEnabled(mirror *mirrorv1alpha1.Mirror) bool {
	return mirror.Spec.Publish.AnyEnabled()
}

// httpServiceSpec returns the base service spec of the http key, or nil when
// the key is absent (absent = disabled).
func httpServiceSpec(services mirrorv1alpha1.MirrorServicesSpec) *mirrorv1alpha1.MirrorServiceSpec {
	if services.HTTP == nil {
		return nil
	}
	return &services.HTTP.MirrorServiceSpec
}

// validateMirrorService validates one ENABLED publish service of a Mirror
// (absent keys are skipped entirely — anything may be parked there). The CRD
// additionally enforces the declaration-time presence of podTemplate.spec at
// admission; the controller-side checks keep the InvalidSpec path complete
// for specs that bypassed it.
func validateMirrorService(service *mirrorv1alpha1.MirrorServiceSpec, path *field.Path) field.ErrorList {
	var errs field.ErrorList
	templatePath := path.Child("podTemplate")
	errs = append(errs, validatePublishPodTemplate(&service.PodTemplate, templatePath, PublishDataVolumeName)...)
	// Data integrity: every mount of the controller-injected mirror-data
	// volume — including extra user mounts in sidecars or init containers —
	// must be read-only. The controller forces the volume source read-only
	// regardless, but a writable mount attempt must not slip through silently.
	for i := range service.PodTemplate.Spec.Containers {
		container := &service.PodTemplate.Spec.Containers[i]
		errs = append(errs, validateMirrorDataMounts(container.VolumeMounts, templatePath.Child("spec", "containers").Index(i))...)
	}
	for i := range service.PodTemplate.Spec.InitContainers {
		container := &service.PodTemplate.Spec.InitContainers[i]
		errs = append(errs, validateMirrorDataMounts(container.VolumeMounts, templatePath.Child("spec", "initContainers").Index(i))...)
	}
	return errs
}

// validateMirrorDataMounts rejects writable volumeMounts of the injected
// mirror-data publish PVC volume.
func validateMirrorDataMounts(mounts []corev1.VolumeMount, path *field.Path) field.ErrorList {
	var errs field.ErrorList
	for i := range mounts {
		if mounts[i].Name == PublishDataVolumeName && !mounts[i].ReadOnly {
			errs = append(errs, field.Invalid(path.Child("volumeMounts").Index(i).Child("readOnly"), false,
				"mounts of the injected mirror-data publish PVC volume must always be read-only"))
		}
	}
	return errs
}

// validateHTTPAliases validates the additional public path prefixes of an
// ENABLED http service: no duplicate, no alias equal to the canonical
// /<mirror name> path, and the syntax rules (start with '/', no trailing '/',
// no '//', no whitespace). This controller-side check is the SOLE syntax
// enforcement (the CRD carries only the MaxItems/MaxLength bounds — a CEL
// mirror of these rules was deliberately dropped); keeping it in the
// InvalidSpec path buys precise error messages. Cross-route precedence is the
// gateway's business (see routeGatewayRejection). Case-sensitive on purpose:
// CR names are bound by DNS rules while alias paths are not.
func validateHTTPAliases(http *mirrorv1alpha1.MirrorHTTPServiceSpec, mirrorName string, path *field.Path) field.ErrorList {
	var errs field.ErrorList
	canonical := "/" + mirrorName
	seen := map[mirrorv1alpha1.MirrorHTTPAlias]bool{}
	for i, alias := range http.Aliases {
		aliasPath := path.Index(i)
		value := string(alias)
		if seen[alias] {
			errs = append(errs, field.Duplicate(aliasPath, value))
			continue
		}
		seen[alias] = true
		if value == canonical {
			errs = append(errs, field.Invalid(aliasPath, value, "must not equal the canonical path "+canonical))
			continue
		}
		switch {
		case !strings.HasPrefix(value, "/"):
			errs = append(errs, field.Invalid(aliasPath, value, "must start with '/'"))
		case strings.HasSuffix(value, "/"):
			errs = append(errs, field.Invalid(aliasPath, value, "must not end with '/'"))
		case strings.Contains(value, "//"):
			errs = append(errs, field.Invalid(aliasPath, value, "must not contain '//'"))
		case strings.ContainsFunc(value, unicode.IsSpace):
			errs = append(errs, field.Invalid(aliasPath, value, "must not contain whitespace"))
		}
	}
	return errs
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
