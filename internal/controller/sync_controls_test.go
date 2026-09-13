package controller

import (
	"context"
	"testing"
	"time"

	snapshotv1 "github.com/kubernetes-csi/external-snapshotter/client/v8/apis/volumesnapshot/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	mirrorv1alpha1 "github.com/ZJUSCT/falcon/api/v1alpha1"
)

func TestManualModeAndQueuedAutomaticWork(t *testing.T) {
	for _, manual := range []bool{false, true} {
		t.Run(map[bool]string{false: "automatic queue held", true: "manual request runs"}[manual], func(t *testing.T) {
			m := testMirror()
			m.Finalizers = []string{MirrorFinalizer}
			if manual {
				m.Annotations = map[string]string{SyncRequestAnnotation: "true"}
			}
			m.SetSyncPaused(true)
			c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithStatusSubresource(&mirrorv1alpha1.Mirror{}).WithObjects(m).Build()
			r := &MirrorReconciler{Client: c, Scheme: testScheme(t), Config: testConfig(), SyncLimiter: NewSyncLimiter(1)}
			req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(m)}
			reconcile(t, t.Context(), r, req)
			m = getMirror(t, t.Context(), c, req.NamespacedName)
			if manual {
				if m.Status.CurrentSync == nil || !m.Status.CurrentSync.Manual || m.Status.PausedAt != nil {
					t.Fatalf("manual request not accepted: %#v", m.Status)
				}
				// A restart preserves its manual identity and concurrency still applies.
				r.SyncLimiter = NewSyncLimiter(1)
				r.SyncLimiter.Acquire("other", false)
				reconcile(t, t.Context(), r, req)
				jobs := &batchv1.JobList{}
				if err := c.List(t.Context(), jobs); err != nil {
					t.Fatal(err)
				}
				if len(jobs.Items) != 0 {
					t.Fatal("manual request bypassed concurrency cap")
				}
				r.SyncLimiter.Release("other")
				reconcile(t, t.Context(), r, req)
				if err := c.List(t.Context(), jobs); err != nil {
					t.Fatal(err)
				}
				if len(jobs.Items) != 1 {
					t.Fatal("manual request did not create Job while paused")
				}
				// Pausing does not terminate or suspend the existing Job.
				reconcile(t, t.Context(), r, req)
				job := &batchv1.Job{}
				get(t, t.Context(), c, client.ObjectKeyFromObject(&jobs.Items[0]), job)
				if !job.DeletionTimestamp.IsZero() || job.Spec.Suspend != nil && *job.Spec.Suspend {
					t.Fatal("pause interrupted Job")
				}
			} else {
				if m.Status.CurrentSync != nil || m.Status.PausedAt == nil {
					t.Fatal("paused bootstrap must remain idle")
				}
				// Simulate automatic work accepted before pause, carrying no manual ID.
				m.Status.WorkPVC = m.Name + "-sync"
				m.Status.CurrentSync = &mirrorv1alpha1.MirrorCurrentSyncStatus{QueuedAt: timePtr(time.Now()), Phase: mirrorv1alpha1.SyncPhasePending}
				if err := c.Status().Update(t.Context(), m); err != nil {
					t.Fatal(err)
				}
				reconcile(t, t.Context(), r, req)
				jobs := &batchv1.JobList{}
				if err := c.List(t.Context(), jobs); err != nil {
					t.Fatal(err)
				}
				if len(jobs.Items) != 0 {
					t.Fatal("paused queued automatic work created Job")
				}
				m = getMirror(t, t.Context(), c, req.NamespacedName)
				if m.Status.CurrentSync == nil || m.Status.PausedAt == nil {
					t.Fatal("queued work was discarded")
				}
				m.SetSyncPaused(false)
				if err := c.Update(t.Context(), m); err != nil {
					t.Fatal(err)
				}
				reconcile(t, t.Context(), r, req)
				if err := c.List(t.Context(), jobs); err != nil {
					t.Fatal(err)
				}
				if len(jobs.Items) != 1 {
					t.Fatalf("resuming failed to release automatic queue: %#v", getMirror(t, t.Context(), c, req.NamespacedName).Status)
				}
			}
		})
	}
}

func TestCancellationDrainsWritersAndRetainsPublication(t *testing.T) {
	for _, started := range []bool{false, true} {
		t.Run(map[bool]string{false: "cancel queue", true: "abort writer"}[started], func(t *testing.T) {
			m := testMirror()
			previous := metav1.Unix(1788000000, 0)
			now := previous.Add(time.Hour)
			if !started {
				m.Status.PausedAt = previous.DeepCopy()
			}
			m.Status.ActivePVC = "old-publication"
			m.Status.LastSuccessfulSyncAt = &previous
			m.Status.LastSync = &mirrorv1alpha1.MirrorSyncStatus{Phase: mirrorv1alpha1.SyncPhaseSucceeded, FinishedAt: &previous}
			m.Annotations = map[string]string{SyncRequestAnnotation: "true", mirrorv1alpha1.AbortRequestAnnotation: "true"}
			m.SetSyncPaused(true)
			m.Status.ConsecutiveFailures = 2
			m.Status.CurrentSync = &mirrorv1alpha1.MirrorCurrentSyncStatus{QueuedAt: timePtr(now), Phase: mirrorv1alpha1.SyncPhaseCancelling, Manual: true}
			c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithStatusSubresource(&mirrorv1alpha1.Mirror{}, &corev1.Pod{}).WithObjects(m).Build()
			r := &MirrorReconciler{Client: c, Scheme: testScheme(t), Config: testConfig(), SyncLimiter: NewSyncLimiter(1), Now: func() time.Time { return now.Add(time.Minute) }}
			jobName := currentSyncJobName(m)
			if started {
				job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: jobName, Namespace: m.Namespace}, Status: batchv1.JobStatus{StartTime: timePtr(now)}}
				if err := c.Create(t.Context(), job); err != nil {
					t.Fatal(err)
				}
				pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "writer", Namespace: m.Namespace, Labels: map[string]string{MirrorLabel: m.Name, ComponentLabel: "sync", SyncTimestampLabel: "1788003600"}}, Status: corev1.PodStatus{Phase: corev1.PodRunning}}
				if err := c.Create(t.Context(), pod); err != nil {
					t.Fatal(err)
				}
				if _, err := r.reconcileCancellation(t.Context(), m, publicationHealth{ready: true}); err != nil {
					t.Fatal(err)
				}
				m = getMirror(t, t.Context(), c, client.ObjectKeyFromObject(m))
				// Fake clients do not run GC. Job removal alone must not free its slot.
				if _, err := r.reconcileCancellation(t.Context(), m, publicationHealth{ready: true}); err != nil {
					t.Fatal(err)
				}
				if r.SyncLimiter.Held() != 1 || m.Status.CurrentSync == nil || !m.SyncRequested() || !m.AbortRequested() {
					t.Fatal("slot freed while writer remains")
				}
				// Restart with an empty limiter still observes the remaining writer.
				r.SyncLimiter = NewSyncLimiter(1)
				if _, err := r.reconcileCancellation(t.Context(), m, publicationHealth{ready: true}); err != nil {
					t.Fatal(err)
				}
				if r.SyncLimiter.Held() != 1 {
					t.Fatal("restart lost cancelling writer slot")
				}
				pod.Status.Phase = corev1.PodFailed
				if err := c.Status().Update(t.Context(), pod); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := r.reconcileCancellation(t.Context(), m, publicationHealth{ready: true}); err != nil {
				t.Fatal(err)
			}
			m = getMirror(t, t.Context(), c, client.ObjectKeyFromObject(m))
			if m.Status.CurrentSync != nil || m.Status.ActivePVC != "old-publication" || !m.Status.LastSuccessfulSyncAt.Equal(&previous) || m.Status.ConsecutiveFailures != 2 || r.SyncLimiter.Held() != 0 {
				t.Fatalf("cancellation damaged state: %#v", m.Status)
			}
			if m.Status.LastAttempt.Phase != mirrorv1alpha1.SyncPhaseCancelled || !m.Status.NextSyncAt.Equal(timePtr(now.Add(time.Minute+m.Spec.Sync.Interval.Duration))) {
				t.Fatal("cancellation must acknowledge request and use regular interval")
			}
			want := mirrorv1alpha1.SyncPhaseSucceeded
			if started {
				want = mirrorv1alpha1.SyncPhaseCancelled
			}
			if m.Status.LastSync.Phase != want {
				t.Fatal("cancellation rewrote incorrect sync history")
			}
			wantPaused := &previous
			if started {
				wantPaused = timePtr(now.Add(time.Minute))
			}
			if m.SyncRequested() || m.AbortRequested() || m.Status.RequestCleanup != nil {
				t.Fatal("cancellation did not consume both associated requests")
			}
			if !m.Status.PausedAt.Equal(wantPaused) {
				t.Fatal("cancellation changed the wrong effective pause timestamp")
			}
			if started && !m.Status.LastSync.StartedAt.Equal(timePtr(now)) {
				t.Fatal("real Job start time was lost")
			}
		})
	}
}

func TestCancellationCannotBeOverwrittenByStaleReconcile(t *testing.T) {
	m := testMirror()
	m.Status.CurrentSync = &mirrorv1alpha1.MirrorCurrentSyncStatus{Phase: mirrorv1alpha1.SyncPhaseRunning, QueuedAt: timePtr(time.Now())}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithStatusSubresource(&mirrorv1alpha1.Mirror{}).WithObjects(m).Build()
	stale := getMirror(t, t.Context(), c, client.ObjectKeyFromObject(m))
	fresh := stale.DeepCopy()
	fresh.Status.CurrentSync.Phase = mirrorv1alpha1.SyncPhaseCancelling
	if err := c.Status().Update(t.Context(), fresh); err != nil {
		t.Fatal(err)
	}
	r := &MirrorReconciler{Client: c}
	_, err := r.patchStatus(t.Context(), stale, func() { stale.Status.CurrentSync.Phase = mirrorv1alpha1.SyncPhaseSucceeded })
	if !apierrors.IsConflict(err) {
		t.Fatalf("stale success must conflict before snapshotting: %v", err)
	}
}

func TestAdmissionRestoresExistingJobsBeforeQueuedMirrors(t *testing.T) {
	m := testMirror()
	m.Status.CurrentSync = &mirrorv1alpha1.MirrorCurrentSyncStatus{Phase: mirrorv1alpha1.SyncPhasePending, QueuedAt: timePtr(time.Now())}
	old := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "other-sync", Namespace: m.Namespace, Labels: map[string]string{ComponentLabel: "sync"}}}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(m, old).Build()
	r := &MirrorReconciler{Client: c, Scheme: testScheme(t), SyncLimiter: NewSyncLimiter(1)}
	if _, err := r.lookupOrCreateSyncJob(t.Context(), m); err != errSyncQueued {
		t.Fatalf("restart admission bypassed running job: %v", err)
	}
}

func TestManualCompletionReturnsToPausedAndConsumesRequest(t *testing.T) {
	m := testMirror()
	m.Spec.Publish = mirrorv1alpha1.MirrorServicesSpec{}
	now := time.Date(2026, 9, 7, 0, 0, 0, 0, time.UTC)
	m.Status.WorkPVC = m.Name + "-sync"
	m.Annotations = map[string]string{SyncRequestAnnotation: "true"}
	m.SetSyncPaused(true)
	m.Status.CurrentSync = &mirrorv1alpha1.MirrorCurrentSyncStatus{QueuedAt: timePtr(now.Add(-time.Hour)), Manual: true, Phase: mirrorv1alpha1.SyncPhaseSucceeded}
	m.Status.LastSync = &mirrorv1alpha1.MirrorSyncStatus{Phase: mirrorv1alpha1.SyncPhaseSucceeded, FinishedAt: timePtr(now.Add(-time.Minute))}
	m.Status.LastSuccessfulSyncAt = m.Status.LastSync.FinishedAt.DeepCopy()
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithStatusSubresource(&mirrorv1alpha1.Mirror{}).WithObjects(m).Build()
	r := &MirrorReconciler{Client: c, Scheme: testScheme(t), Config: testConfig(), Now: func() time.Time { return now }}
	if err := r.observeSyncJob(t.Context(), m, &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: currentSyncJobName(m)}, Status: batchv1.JobStatus{CompletionTime: timePtr(now), Conditions: []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}}}); err != nil {
		t.Fatal(err)
	}
	if !m.SyncRequested() {
		t.Fatal("successful Job consumed request before snapshot readiness")
	}
	completeTestSyncSnapshot(t, r, m)
	m = getMirror(t, t.Context(), c, client.ObjectKeyFromObject(m))
	if m.SyncRequested() || m.Status.RequestCleanup != nil || m.Status.CurrentSync != nil || !m.Status.PausedAt.Equal(timePtr(now)) || !m.SyncPaused() {
		t.Fatalf("manual completion must return to paused: %#v", m.Status)
	}
}

func TestInvalidRequestDoesNotTurnScheduledWorkIntoManualWork(t *testing.T) {
	m := testMirror()
	m.Finalizers = []string{MirrorFinalizer}
	m.Annotations = map[string]string{SyncRequestAnnotation: "old-token"}

	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithStatusSubresource(&mirrorv1alpha1.Mirror{}).WithObjects(m).Build()
	r := &MirrorReconciler{Client: c, Scheme: testScheme(t), Config: testConfig(), SyncLimiter: NewSyncLimiter(1)}
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(m)}
	reconcile(t, t.Context(), r, req) // discard invalid request
	reconcile(t, t.Context(), r, req)
	m = getMirror(t, t.Context(), c, req.NamespacedName)
	if m.Status.CurrentSync == nil || m.Status.CurrentSync.Manual {
		t.Fatal("invalid annotation incorrectly made bootstrap manual")
	}
	m.SetSyncPaused(true)
	if err := c.Update(t.Context(), m); err != nil {
		t.Fatal(err)
	}
	reconcile(t, t.Context(), r, req)
	jobs := &batchv1.JobList{}
	if err := c.List(t.Context(), jobs); err != nil {
		t.Fatal(err)
	}
	if len(jobs.Items) != 0 {
		t.Fatal("invalid annotation bypassed pause")
	}
}

type beforeJobCreateClient struct {
	client.Client
	beforeCreate func(*batchv1.Job) error
	afterCreate  func(*batchv1.Job) error
}

func (c beforeJobCreateClient) Create(ctx context.Context, object client.Object, options ...client.CreateOption) error {
	job, ok := object.(*batchv1.Job)
	if !ok {
		return c.Client.Create(ctx, object, options...)
	}
	if c.beforeCreate != nil {
		if err := c.beforeCreate(job); err != nil {
			return err
		}
	}
	if err := c.Client.Create(ctx, job, options...); err != nil {
		return err
	}
	if c.afterCreate != nil {
		return c.afterCreate(job)
	}
	return nil
}

func TestCancellationRacingJobCreationDrainsBeforeClearingAttempt(t *testing.T) {
	for _, succeeded := range []bool{false, true} {
		t.Run(map[bool]string{false: "writer running", true: "success races cancellation"}[succeeded], func(t *testing.T) {
			m := testMirror()
			m.Status.WorkPVC = m.Name + "-sync"
			m.Status.ActivePVC = "old-publication"
			queued := metav1.Unix(1788000000, 0)
			m.Status.CurrentSync = &mirrorv1alpha1.MirrorCurrentSyncStatus{QueuedAt: &queued, Phase: mirrorv1alpha1.SyncPhasePending}
			m.Status.LastSync = &mirrorv1alpha1.MirrorSyncStatus{Phase: mirrorv1alpha1.SyncPhaseSucceeded, FinishedAt: timePtr(queued.Add(-time.Hour))}
			m.Status.LastSuccessfulSyncAt = m.Status.LastSync.FinishedAt.DeepCopy()
			c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithStatusSubresource(&mirrorv1alpha1.Mirror{}, &batchv1.Job{}, &corev1.Pod{}).WithObjects(m).Build()
			stale := getMirror(t, t.Context(), c, client.ObjectKeyFromObject(m))
			writer := &corev1.Pod{}
			racing := beforeJobCreateClient{Client: c, beforeCreate: func(_ *batchv1.Job) error {
				// Persist cancellation after the final admission check, immediately before
				// the stale reconciliation's API create request reaches Kubernetes.
				latest := getMirror(t, t.Context(), c, client.ObjectKeyFromObject(m))
				latest.Status.CurrentSync.Phase = mirrorv1alpha1.SyncPhaseCancelling
				return c.Status().Update(t.Context(), latest)
			}, afterCreate: func(job *batchv1.Job) error {
				job.Status.StartTime = &queued
				if succeeded {
					job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
					job.Status.CompletionTime = timePtr(queued.Add(time.Second))
				}
				if err := c.Status().Update(t.Context(), job); err != nil {
					return err
				}
				if !succeeded {
					writer.ObjectMeta = metav1.ObjectMeta{Name: "writer", Namespace: m.Namespace, Labels: job.Spec.Template.Labels}
					writer.Status.Phase = corev1.PodRunning
					return c.Create(t.Context(), writer)
				}
				return nil
			}}
			r := &MirrorReconciler{Client: racing, APIReader: c, Scheme: testScheme(t), Config: testConfig(), SyncLimiter: NewSyncLimiter(1)}
			if _, err := r.reconcileSync(t.Context(), stale, publicationHealth{ready: true}); !apierrors.IsConflict(err) {
				t.Fatalf("stale reconciliation must conflict: %v", err)
			}
			current := getMirror(t, t.Context(), c, client.ObjectKeyFromObject(m))
			snapshots := &snapshotv1.VolumeSnapshotList{}
			if err := c.List(t.Context(), snapshots); err != nil {
				t.Fatal(err)
			}
			if current.Status.CurrentSync.Phase != mirrorv1alpha1.SyncPhaseCancelling || len(snapshots.Items) != 0 || current.Status.ActivePVC != "old-publication" {
				t.Fatal("late Job overwrote cancellation or entered publication")
			}
			r.Client = c
			if _, err := r.reconcileCancellation(t.Context(), current, publicationHealth{ready: true}); err != nil {
				t.Fatal(err)
			}
			current = getMirror(t, t.Context(), c, client.ObjectKeyFromObject(m))
			if !succeeded {
				if _, err := r.reconcileCancellation(t.Context(), current, publicationHealth{ready: true}); err != nil {
					t.Fatal(err)
				}
				if current.Status.CurrentSync == nil || r.SyncLimiter.Held() != 1 {
					t.Fatal("late-created writer was not drained before releasing attempt")
				}
				writer.Status.Phase = corev1.PodFailed
				if err := c.Status().Update(t.Context(), writer); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := r.reconcileCancellation(t.Context(), current, publicationHealth{ready: true}); err != nil {
				t.Fatal(err)
			}
			current = getMirror(t, t.Context(), c, client.ObjectKeyFromObject(m))
			if current.Status.CurrentSync != nil || current.Status.LastSync.Phase != mirrorv1alpha1.SyncPhaseCancelled || current.Status.ActivePVC != "old-publication" || r.SyncLimiter.Held() != 0 {
				t.Fatal("late Job did not finish cancellation safely")
			}
		})
	}
}

func TestAdmissionRejectsStalePauseOrCancellation(t *testing.T) {
	for _, cancel := range []bool{false, true} {
		t.Run(map[bool]string{false: "pause", true: "cancel"}[cancel], func(t *testing.T) {
			m := testMirror()
			m.Status.CurrentSync = &mirrorv1alpha1.MirrorCurrentSyncStatus{Phase: mirrorv1alpha1.SyncPhasePending, QueuedAt: timePtr(time.Now())}
			c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithStatusSubresource(&mirrorv1alpha1.Mirror{}).WithObjects(m).Build()
			stale := getMirror(t, t.Context(), c, client.ObjectKeyFromObject(m))
			latest := stale.DeepCopy()
			if cancel {
				latest.Status.CurrentSync.Phase = mirrorv1alpha1.SyncPhaseCancelling
				if err := c.Status().Update(t.Context(), latest); err != nil {
					t.Fatal(err)
				}
			} else {
				latest.SetSyncPaused(true)
				if err := c.Update(t.Context(), latest); err != nil {
					t.Fatal(err)
				}
			}
			r := &MirrorReconciler{Client: c, APIReader: c, Scheme: testScheme(t), SyncLimiter: NewSyncLimiter(1)}
			if _, err := r.lookupOrCreateSyncJob(t.Context(), stale); !apierrors.IsConflict(err) {
				t.Fatalf("stale admission must conflict: %v", err)
			}
			jobs := &batchv1.JobList{}
			if err := c.List(t.Context(), jobs); err != nil {
				t.Fatal(err)
			}
			if len(jobs.Items) != 0 || r.SyncLimiter.Held() != 0 {
				t.Fatal("stale state admitted a Job")
			}
		})
	}
}

func TestRestoreSyncSlotsReclaimsDisappearedWorkloads(t *testing.T) {
	m := testMirror()
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "old-sync", Namespace: m.Namespace, Labels: map[string]string{ComponentLabel: syncComponent}}}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "orphan", Namespace: m.Namespace, Labels: map[string]string{ComponentLabel: syncComponent, MirrorLabel: "orphan", SyncTimestampLabel: "1"}}, Status: corev1.PodStatus{Phase: corev1.PodRunning}}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(job, pod).Build()
	r := &MirrorReconciler{Client: c, SyncLimiter: NewSyncLimiter(2)}
	if err := r.restoreSyncSlots(t.Context(), m.Namespace); err != nil {
		t.Fatal(err)
	}
	if r.SyncLimiter.Held() != 2 {
		t.Fatal("live Job and orphan Pod must occupy slots")
	}
	if err := c.Delete(t.Context(), job); err != nil {
		t.Fatal(err)
	}
	if err := r.restoreSyncSlots(t.Context(), m.Namespace); err != nil {
		t.Fatal(err)
	}
	if r.SyncLimiter.Held() != 1 {
		t.Fatal("disappeared Job leaked slot or live orphan lost slot")
	}
	if err := c.Delete(t.Context(), pod); err != nil {
		t.Fatal(err)
	}
	if err := r.restoreSyncSlots(t.Context(), m.Namespace); err != nil {
		t.Fatal(err)
	}
	if r.SyncLimiter.Held() != 0 {
		t.Fatal("disappeared orphan Pod leaked slot")
	}
}

func TestResumeHonorsScheduleAndRetainsPausedSpecEdits(t *testing.T) {
	for _, tc := range []struct {
		name          string
		editSpec, due bool
	}{
		{name: "resume before schedule"},
		{name: "resume after schedule", due: true},
		{name: "template edited while paused", editSpec: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Date(2026, 9, 8, 0, 0, 0, 0, time.UTC)
			m := testMirror()
			m.Finalizers = []string{MirrorFinalizer}
			m.Spec.Publish = mirrorv1alpha1.MirrorServicesSpec{}
			m.Status.ActivePVC = "old-publication"
			m.Status.ObservedGeneration = m.Generation
			m.Status.NextSyncAt = timePtr(now.Add(time.Hour))
			if tc.due {
				m.Status.NextSyncAt = timePtr(now.Add(-time.Minute))
			}
			initialNext := m.Status.NextSyncAt.DeepCopy()
			c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithStatusSubresource(&mirrorv1alpha1.Mirror{}).WithObjects(m).Build()
			r := &MirrorReconciler{Client: c, Scheme: testScheme(t), Config: testConfig(), Now: func() time.Time { return now }}
			// Establish an upgrade baseline while the exact spec is already observed.
			// Keep the first schedule future until pause has been applied.
			m = getMirror(t, t.Context(), c, client.ObjectKeyFromObject(m))
			m.Status.NextSyncAt = timePtr(now.Add(time.Hour))
			if err := c.Status().Update(t.Context(), m); err != nil {
				t.Fatal(err)
			}
			req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(m)}
			reconcile(t, t.Context(), r, req)
			m = getMirror(t, t.Context(), c, req.NamespacedName)
			initialHash := m.Status.LastAcceptedSpecHash
			if initialHash == "" || m.Status.CurrentSync != nil {
				t.Fatal("observed spec must establish a baseline without starting a run")
			}
			m.SetSyncPaused(true)
			m.Generation++
			if err := c.Update(t.Context(), m); err != nil {
				t.Fatal(err)
			}
			reconcile(t, t.Context(), r, req)
			m = getMirror(t, t.Context(), c, req.NamespacedName)
			if tc.editSpec {
				m.Spec.Sync.PodTemplate.Spec.Containers[0].Image = "sync:changed"
				m.Generation++
				if err := c.Update(t.Context(), m); err != nil {
					t.Fatal(err)
				}
				reconcile(t, t.Context(), r, req)
				m = getMirror(t, t.Context(), c, req.NamespacedName)
				if m.Status.CurrentSync != nil || m.Status.LastAcceptedSpecHash != initialHash {
					t.Fatal("pause consumed or started an edited sync configuration")
				}
			}
			m.Status.NextSyncAt = initialNext
			if err := c.Status().Update(t.Context(), m); err != nil {
				t.Fatal(err)
			}
			m.SetSyncPaused(false)
			m.Generation++
			if err := c.Update(t.Context(), m); err != nil {
				t.Fatal(err)
			}
			// A fresh reconciler demonstrates that schedule/configuration intent survives restart.
			r = &MirrorReconciler{Client: c, Scheme: testScheme(t), Config: testConfig(), Now: func() time.Time { return now }}
			reconcile(t, t.Context(), r, req)
			m = getMirror(t, t.Context(), c, req.NamespacedName)
			wantStart := tc.editSpec || tc.due
			if (m.Status.CurrentSync != nil) != wantStart {
				t.Fatalf("resume should start=%v: %#v", wantStart, m.Status)
			}
			if !wantStart && (!m.Status.NextSyncAt.Equal(initialNext) || m.Status.LastAcceptedSpecHash != initialHash) {
				t.Fatal("resume changed schedule or accepted configuration")
			}
			if tc.editSpec && m.Status.LastAcceptedSpecHash == initialHash {
				t.Fatal("new synchronization did not accept the changed template")
			}
		})
	}
}

func TestModeChangesDuringTransactionDoNotQueueAnotherRun(t *testing.T) {
	now := time.Date(2026, 9, 8, 0, 0, 0, 0, time.UTC)
	m := testMirror()
	m.Finalizers = []string{MirrorFinalizer}
	m.Spec.Publish = mirrorv1alpha1.MirrorServicesSpec{}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithStatusSubresource(&mirrorv1alpha1.Mirror{}).WithObjects(m).Build()
	r := &MirrorReconciler{Client: c, Scheme: testScheme(t), Config: testConfig(), Now: func() time.Time { return now }}
	if _, err := r.startSync(t.Context(), m, true, publicationHealth{}); err != nil {
		t.Fatal(err)
	}
	m.SetSyncPaused(true)
	m.Generation++
	if err := c.Update(t.Context(), m); err != nil {
		t.Fatal(err)
	}
	m.SetSyncPaused(false)
	m.Generation++
	if err := c.Update(t.Context(), m); err != nil {
		t.Fatal(err)
	}
	m.Status.CurrentSync.Phase = mirrorv1alpha1.SyncPhaseSucceeded
	m.Status.LastSuccessfulSyncAt = timePtr(now)
	m.Status.LastSync = &mirrorv1alpha1.MirrorSyncStatus{Phase: mirrorv1alpha1.SyncPhaseSucceeded, FinishedAt: timePtr(now)}
	if err := c.Status().Update(t.Context(), m); err != nil {
		t.Fatal(err)
	}
	if err := r.observeSyncJob(t.Context(), m, &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: currentSyncJobName(m)}, Status: batchv1.JobStatus{CompletionTime: timePtr(now), Conditions: []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}}}); err != nil {
		t.Fatal(err)
	}
	completeTestSyncSnapshot(t, r, m)
	reconcile(t, t.Context(), r, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(m)})
	m = getMirror(t, t.Context(), c, client.ObjectKeyFromObject(m))
	if m.Status.CurrentSync != nil || !m.Status.NextSyncAt.Equal(timePtr(now.Add(m.Spec.Sync.Interval.Duration))) {
		t.Fatal("pause/resume during active work caused an extra sync")
	}
}

func TestOrphanCancellationUpdatesPublicationConditions(t *testing.T) {
	m := testMirror()
	m.Generation = 2
	now := time.Now().UTC().Truncate(time.Second)
	m.Status.CurrentSync = &mirrorv1alpha1.MirrorCurrentSyncStatus{QueuedAt: timePtr(now), StartedAt: timePtr(now), Phase: mirrorv1alpha1.SyncPhaseCancelling}
	m.Status.Conditions = []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue, Reason: "Published", ObservedGeneration: 1, LastTransitionTime: metav1.NewTime(now)}}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "orphan-writer", Namespace: m.Namespace, Labels: childLabels(m, now.Unix(), "sync")}, Status: corev1.PodStatus{Phase: corev1.PodPending}}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithStatusSubresource(&mirrorv1alpha1.Mirror{}).WithObjects(m, pod).Build()
	r := &MirrorReconciler{Client: c, SyncLimiter: NewSyncLimiter(1)}
	if _, err := r.reconcileCancellation(t.Context(), m, publicationHealth{ready: true}); err != nil {
		t.Fatal(err)
	}
	m = getMirror(t, t.Context(), c, client.ObjectKeyFromObject(m))
	for _, typ := range []string{"Ready"} {
		found := false
		for _, condition := range m.Status.Conditions {
			if condition.Type == typ {
				found = condition.Status == metav1.ConditionTrue && condition.ObservedGeneration == m.Generation
			}
		}
		if !found {
			t.Fatalf("%s must reflect retained publication and orphan cancellation at current generation: %#v", typ, m.Status.Conditions)
		}
	}
	if m.Status.Sync.Phase != "Cancelling" || findCondition(m.Status.Conditions, "Progressing").Status != metav1.ConditionFalse {
		t.Fatal("sync cancellation must not imply publication progress")
	}
	if m.Status.CurrentSync == nil || r.SyncLimiter.Held() != 1 {
		t.Fatal("orphan writer must retain cancellation and occupancy")
	}
}
