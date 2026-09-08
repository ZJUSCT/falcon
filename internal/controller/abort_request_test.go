package controller

import (
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	mirrorv1alpha1 "github.com/ZJUSCT/falcon/api/v1alpha1"
)

func abortRequestMirror() *mirrorv1alpha1.Mirror {
	m := testMirror()
	m.UID = "mirror-uid"
	m.Finalizers = []string{MirrorFinalizer}
	m.Status.CurrentSync = &mirrorv1alpha1.MirrorCurrentSyncStatus{Phase: mirrorv1alpha1.SyncPhasePending, QueuedAt: timePtr(time.Unix(1789000000, 0))}
	m.Annotations = map[string]string{mirrorv1alpha1.AbortRequestAnnotation: "true"}
	return m
}

func TestAbortAnnotationIgnoresStaleAndCompletedTargets(t *testing.T) {
	for _, name := range []string{"old-mirror", "old-generation", "empty", "snapshotting", "cancelling", "complete", "failed", "success-criteria", "failure-target"} {
		t.Run(name, func(t *testing.T) {
			m := abortRequestMirror()
			job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: currentSyncJobName(m), Namespace: m.Namespace}}
			switch name {
			case "old-mirror":
				m.Annotations[mirrorv1alpha1.AbortRequestAnnotation] = "old-uid/1789000000"
			case "old-generation":
				m.Annotations[mirrorv1alpha1.AbortRequestAnnotation] = "mirror-uid/1788999999"
			case "empty":
				m.Annotations[mirrorv1alpha1.AbortRequestAnnotation] = ""
			case "snapshotting":
				m.Status.CurrentSync.Phase = mirrorv1alpha1.SyncPhaseSnapshotting
			case "cancelling":
				m.Status.CurrentSync.Phase = mirrorv1alpha1.SyncPhaseCancelling
			default:
				condition := map[string]batchv1.JobConditionType{"complete": batchv1.JobComplete, "failed": batchv1.JobFailed, "success-criteria": batchv1.JobSuccessCriteriaMet, "failure-target": batchv1.JobFailureTarget}[name]
				job.Status.Conditions = []batchv1.JobCondition{{Type: condition, Status: corev1.ConditionTrue}}
			}
			c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithStatusSubresource(&mirrorv1alpha1.Mirror{}).WithObjects(m, job).Build()
			original := getMirror(t, t.Context(), c, client.ObjectKeyFromObject(m))
			r := &MirrorReconciler{Client: c}
			if handled, err := r.reconcileAbortRequest(t.Context(), m); err != nil || handled != (name != "cancelling") {
				t.Fatalf("inapplicable request cleanup: %v, %v", handled, err)
			}
			current := getMirror(t, t.Context(), c, client.ObjectKeyFromObject(m))
			if current.Status.CurrentSync.Phase != original.Status.CurrentSync.Phase {
				t.Fatal("ignored request changed state")
			}
			get(t, t.Context(), c, client.ObjectKeyFromObject(job), &batchv1.Job{})
		})
	}
}

func TestQueuedAbortAnnotationSurvivesRestartAndInvalidSpec(t *testing.T) {
	m := abortRequestMirror()
	m.Spec.Sync.Interval.Duration = 0 // Cancellation must not wait for spec repair.
	m.Status.LastSync = &mirrorv1alpha1.MirrorSyncStatus{JobName: "previous", Phase: mirrorv1alpha1.SyncPhaseSucceeded}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithStatusSubresource(&mirrorv1alpha1.Mirror{}).WithObjects(m).Build()
	restart := func() *MirrorReconciler {
		return &MirrorReconciler{Client: c, Scheme: testScheme(t), SyncLimiter: NewSyncLimiter(1), Config: testConfig()}
	}
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(m)}
	reconcile(t, t.Context(), restart(), req)
	m = getMirror(t, t.Context(), c, req.NamespacedName)
	if m.Status.CurrentSync.Phase != mirrorv1alpha1.SyncPhaseCancelling || m.Status.Sync.Phase != mirrorv1alpha1.SyncStateCancelling {
		t.Fatal("annotation did not persist cancellation")
	}
	reconcile(t, t.Context(), restart(), req)
	m = getMirror(t, t.Context(), c, req.NamespacedName)
	if m.Status.CurrentSync != nil || m.Status.LastAttempt.Phase != mirrorv1alpha1.SyncPhaseCancelled || m.Status.LastSync.JobName != "previous" {
		t.Fatal("queued cancellation lost completion/history")
	}
	// Completion consumes the request before the next generation is admitted.
	m.Status.CurrentSync = &mirrorv1alpha1.MirrorCurrentSyncStatus{Phase: mirrorv1alpha1.SyncPhasePending, QueuedAt: timePtr(time.Unix(1789000001, 0))}
	if err := c.Status().Update(t.Context(), m); err != nil {
		t.Fatal(err)
	}
	if handled, err := restart().reconcileAbortRequest(t.Context(), m); err != nil || handled {
		t.Fatal("stale annotation cancelled later generation")
	}
}

func TestRunningAbortAnnotationPersistsBeforeForegroundDeletion(t *testing.T) {
	m := abortRequestMirror()
	m.Status.CurrentSync.Phase = mirrorv1alpha1.SyncPhaseRunning
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: currentSyncJobName(m), Namespace: m.Namespace, UID: "job-uid", Finalizers: []string{"test/hold"}}, Status: batchv1.JobStatus{StartTime: timePtr(time.Unix(1789000010, 0))}}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithStatusSubresource(&mirrorv1alpha1.Mirror{}).WithObjects(m, job).Build()
	recorded := &recordDeleteOptionsClient{Client: c, options: map[string]client.DeleteOptions{}}
	r := &MirrorReconciler{Client: recorded, Scheme: testScheme(t), SyncLimiter: NewSyncLimiter(1), Config: testConfig()}
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(m)}
	reconcile(t, t.Context(), r, req)
	if len(recorded.options) != 0 {
		t.Fatal("Job deleted before durable cancellation")
	}
	m = getMirror(t, t.Context(), c, req.NamespacedName)
	if !m.Status.CurrentSync.StartedAt.Equal(job.Status.StartTime) {
		t.Fatal("live Job start time lost")
	}
	reconcile(t, t.Context(), r, req)
	options := recorded.options[job.Name]
	if options.PropagationPolicy == nil || *options.PropagationPolicy != metav1.DeletePropagationForeground || options.Preconditions == nil || *options.Preconditions.UID != job.UID {
		t.Fatal("cancellation did not target foreground deletion of the observed Job")
	}
	if r.SyncLimiter.Acquire("another", false) {
		t.Fatal("cancellation released the Job slot before workload deletion")
	}
}

func TestAbortAnnotationBlocksJobAdmissionAndStaleStatusWrites(t *testing.T) {
	m := abortRequestMirror()
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithStatusSubresource(&mirrorv1alpha1.Mirror{}).WithObjects(m).Build()
	r := &MirrorReconciler{Client: c, Scheme: testScheme(t), SyncLimiter: NewSyncLimiter(1)}
	if _, err := r.lookupOrCreateSyncJob(t.Context(), m); !apierrors.IsConflict(err) {
		t.Fatalf("abort intent allowed Job admission: %v", err)
	}
	stale := m.DeepCopy()
	m.Status.CurrentSync.QueuedAt = timePtr(time.Unix(1789000001, 0))
	if err := c.Status().Update(t.Context(), m); err != nil {
		t.Fatal(err)
	}
	if _, err := r.reconcileAbortRequest(t.Context(), stale); !apierrors.IsConflict(err) {
		t.Fatalf("stale abort overwrote new generation: %v", err)
	}
}
