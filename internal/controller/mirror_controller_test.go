package controller

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	snapshotv1 "github.com/kubernetes-csi/external-snapshotter/client/v8/apis/volumesnapshot/v1"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	"sigs.k8s.io/yaml"

	mirrorv1alpha1 "github.com/ZJUSCT/falcon/api/v1alpha1"
)

func TestAtomicPublicationUsesStableSyncPVCAndSnapshotClone(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 25, 18, 0, 0, 0, time.UTC)
	mirror := testMirror()
	scheme := testScheme(t)
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(
			&mirrorv1alpha1.Mirror{},
			&batchv1.Job{},
			&snapshotv1.VolumeSnapshot{},
			&appsv1.Deployment{},
		).
		WithObjects(mirror).
		Build()
	reconciler := &MirrorReconciler{
		Client:      fakeClient,
		Scheme:      scheme,
		Recorder:    record.NewFakeRecorder(20),
		Now:         func() time.Time { return now },
		Config:      testConfig(),
		SyncLimiter: NewSyncLimiter(0),
	}
	request := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: mirror.Namespace, Name: mirror.Name}}

	reconcile(t, ctx, reconciler, request) // finalizer
	reconcile(t, ctx, reconciler, request) // initialize synchronization run
	current := getMirror(t, ctx, fakeClient, request.NamespacedName)
	if current.Status.WorkPVC != "smoke-sync" {
		t.Fatalf("expected stable sync PVC smoke-sync, got %q", current.Status.WorkPVC)
	}
	// The timestamp is allocated ONCE at sync task creation and propagates to
	// the sync Job name and to the snapshot and publish PVC (which share the
	// name <base>-snap-<ts>).
	if currentSyncJobName(current) != fmt.Sprintf("smoke-sync-%d", now.Unix()) {
		t.Fatalf("expected current sync Job smoke-sync-<ts>, got %#v", current.Status)
	}
	if currentSyncTimestamp(current) != now.Unix() ||
		currentSyncSnapshotName(current) != fmt.Sprintf("smoke-snap-%d", now.Unix()) {
		t.Fatalf("expected timestamped names derived from the task creation timestamp, got %#v", current.Status)
	}

	reconcile(t, ctx, reconciler, request) // sync PVC + sync Job
	workClaim := &corev1.PersistentVolumeClaim{}
	get(t, ctx, fakeClient, client.ObjectKey{Namespace: mirror.Namespace, Name: current.Status.WorkPVC}, workClaim)
	job := &batchv1.Job{}
	get(t, ctx, fakeClient, client.ObjectKey{Namespace: mirror.Namespace, Name: currentSyncJobName(current)}, job)
	// The writable sync-data volume is forced as a VOLUME (PVC source without
	// ReadOnly); mounting it is the user template's own declaration.
	jobData := findVolume(job.Spec.Template.Spec.Volumes, "sync-data")
	if jobData == nil || jobData.PersistentVolumeClaim == nil || jobData.PersistentVolumeClaim.ClaimName != current.Status.WorkPVC {
		t.Fatalf("sync Job must inject the sync PVC %q as writable sync-data volume, got %#v", current.Status.WorkPVC, jobData)
	}
	if jobData.PersistentVolumeClaim.ReadOnly {
		t.Fatal("sync-data is the sync OUTPUT volume: its volume source must not be read-only")
	}
	// The user-declared sync-data mount is preserved verbatim (the controller
	// adds no mounts of its own).
	jobMount := findMount(job.Spec.Template.Spec.Containers[0], "sync-data")
	if jobMount == nil || jobMount.MountPath != "/data" || jobMount.ReadOnly {
		t.Fatalf("the user-declared sync-data mount must be preserved verbatim, got %#v", jobMount)
	}
	// The user-declared input volume from the pod template is preserved.
	jobMounts := job.Spec.Template.Spec.Containers[0].VolumeMounts
	if jobMounts[0].Name != "sync-config" || jobMounts[0].MountPath != "/etc/sync" || !jobMounts[0].ReadOnly {
		t.Fatalf("user input volume must be preserved read-only, got %#v", jobMounts[0])
	}
	if job.Spec.Template.Spec.RestartPolicy != corev1.RestartPolicyNever {
		t.Fatalf("sync pod restartPolicy must be Never, got %q", job.Spec.Template.Spec.RestartPolicy)
	}
	if job.Spec.Template.Spec.NodeName != "" {
		t.Fatalf("sync Job bypasses the scheduler with spec.nodeName %q", job.Spec.Template.Spec.NodeName)
	}
	if len(job.Spec.Template.Spec.NodeSelector) != 0 {
		t.Fatalf("sync Job must carry no controller-injected nodeSelector (placement is scheduler-native), got %#v", job.Spec.Template.Spec.NodeSelector)
	}
	assertNotFound(t, ctx, fakeClient, client.ObjectKey{Namespace: mirror.Namespace, Name: currentSyncSnapshotName(current)}, &snapshotv1.VolumeSnapshot{})
	addBoundSyncPVC(t, ctx, fakeClient, mirror, "smoke-sync", "s3.mirrors.zjusct.io", hostnameAffinity("s3.mirrors.zjusct.io"))
	assertNotFound(t, ctx, fakeClient, client.ObjectKey{Namespace: mirror.Namespace, Name: currentSyncSnapshotName(current)}, &corev1.PersistentVolumeClaim{})

	job.Status.Succeeded = 1
	job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
	completion := metav1.NewTime(now)
	job.Status.CompletionTime = &completion
	if err := fakeClient.Status().Update(ctx, job); err != nil {
		t.Fatalf("mark Job complete: %v", err)
	}
	// After Job success the transaction timestamp is simply reused: no
	// separate allocation step is needed.
	reconcile(t, ctx, reconciler, request) // post-sync snapshot
	snapshot := &snapshotv1.VolumeSnapshot{}
	get(t, ctx, fakeClient, client.ObjectKey{Namespace: mirror.Namespace, Name: currentSyncSnapshotName(current)}, snapshot)
	if snapshot.Spec.Source.PersistentVolumeClaimName == nil || *snapshot.Spec.Source.PersistentVolumeClaimName != current.Status.WorkPVC {
		t.Fatalf("snapshot source was not the stable sync PVC: %#v", snapshot.Spec.Source)
	}

	ready := true
	snapshot.Status = &snapshotv1.VolumeSnapshotStatus{ReadyToUse: &ready}
	if err := fakeClient.Status().Update(ctx, snapshot); err != nil {
		t.Fatalf("mark snapshot ready: %v", err)
	}
	reconcile(t, ctx, reconciler, request) // publish PVC clone + Deployment + Service
	publishClaim := &corev1.PersistentVolumeClaim{}
	get(t, ctx, fakeClient, client.ObjectKey{Namespace: mirror.Namespace, Name: currentSyncSnapshotName(current)}, publishClaim)
	if publishClaim.Spec.DataSource == nil || publishClaim.Spec.DataSource.Kind != "VolumeSnapshot" || publishClaim.Spec.DataSource.Name != currentSyncSnapshotName(current) {
		t.Fatalf("publish PVC was not cloned from the completed snapshot: %#v", publishClaim.Spec.DataSource)
	}
	if publishClaim.Name != publishClaim.Spec.DataSource.Name {
		t.Fatalf("publish PVC %q must carry the same name as its snapshot, got datasource %#v", publishClaim.Name, publishClaim.Spec.DataSource)
	}
	if publishClaim.Spec.StorageClassName == nil || *publishClaim.Spec.StorageClassName != "delete-class" {
		t.Fatalf("publish PVC storage class = %v; expected disposable snapshot class", publishClaim.Spec.StorageClassName)
	}
	deployment := &appsv1.Deployment{}
	get(t, ctx, fakeClient, client.ObjectKey{Namespace: mirror.Namespace, Name: "smoke-publish-http"}, deployment)
	dataVolume := findVolume(deployment.Spec.Template.Spec.Volumes, "mirror-data")
	if dataVolume == nil || dataVolume.PersistentVolumeClaim == nil || dataVolume.PersistentVolumeClaim.ClaimName != currentSyncSnapshotName(current) {
		t.Fatalf("publish Deployment must inject the clone PVC %q as mirror-data volume, got %#v", currentSyncSnapshotName(current), dataVolume)
	}
	if !dataVolume.PersistentVolumeClaim.ReadOnly {
		t.Fatal("the mirror-data volume source must be read-only")
	}
	// The controller injects the volume only: no mount is added, mounting is
	// the user template's declaration.
	if dataMount := findMount(deployment.Spec.Template.Spec.Containers[0], "mirror-data"); dataMount != nil {
		t.Fatalf("the controller must not mount mirror-data itself, got %#v", dataMount)
	}
	// The first container port is the Service target; the controller renames
	// it to the service key so the Service and probes reference a named port.
	containerPorts := deployment.Spec.Template.Spec.Containers[0].Ports
	if len(containerPorts) != 1 || containerPorts[0].Name != "http" || containerPorts[0].ContainerPort != 8080 {
		t.Fatalf("publish container ports = %#v; want the single declared port renamed to http", containerPorts)
	}
	if deployment.Spec.Template.Spec.NodeName != "" {
		t.Fatalf("publish Deployment bypasses the scheduler with spec.nodeName %q", deployment.Spec.Template.Spec.NodeName)
	}
	// The node constraint is derived from the sync PVC's bound local PV: the
	// hostname selector comes from the PV nodeAffinity, not from any spec field.
	if got := deployment.Spec.Template.Spec.NodeSelector[corev1.LabelHostname]; got != "s3.mirrors.zjusct.io" {
		t.Fatalf("publish Deployment hostname selector = %q; expected the PV-derived storage node", got)
	}

	deployment.Status.ObservedGeneration = deployment.Generation
	deployment.Status.AvailableReplicas = 1
	deployment.Status.UpdatedReplicas = 1
	if err := fakeClient.Status().Update(ctx, deployment); err != nil {
		t.Fatalf("mark Deployment available: %v", err)
	}
	reconcile(t, ctx, reconciler, request) // publish status
	current = getMirror(t, ctx, fakeClient, request.NamespacedName)
	if current.Status.ActivePVC != publishClaim.Name || current.Status.ActiveSnapshot != snapshot.Name {
		t.Fatalf("unexpected published status: %#v", current.Status)
	}
	reconcile(t, ctx, reconciler, request) // published Mirror: publish route ensured

	// The published Mirror is served through the protocol-named Service of
	// the "http" service entry and a controller-generated publish HTTPRoute.
	service := &corev1.Service{}
	get(t, ctx, fakeClient, client.ObjectKey{Namespace: mirror.Namespace, Name: "smoke-publish-http"}, service)
	if got := service.Spec.Ports[0].Port; got != 80 {
		t.Fatalf("publish Service port = %d, want 80", got)
	}
	if got := service.Spec.Selector[ComponentLabel]; got != "publish-http" {
		t.Fatalf("publish Service selector component = %q, want publish-http (per-service pods)", got)
	}
	route := &gatewayv1.HTTPRoute{}
	get(t, ctx, fakeClient, client.ObjectKey{Namespace: mirror.Namespace, Name: "smoke-publish"}, route)
	assertPublishRouteShape(t, route, mirror, "/smoke", "smoke-publish-http")
}

func TestNextSnapshotStillWritesOnlyToSyncPVC(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 25, 19, 0, 0, 0, time.UTC)
	mirror := testMirror()
	mirror.Finalizers = []string{MirrorFinalizer}
	mirror.Annotations = map[string]string{SyncRequestAnnotation: "second-run"}
	mirror.Status = mirrorv1alpha1.MirrorStatus{
		ObservedGeneration:     mirror.Generation,
		WorkPVC:                "smoke-sync",
		ActivePVC:              "smoke-snap-1756147200",
		ActiveSnapshot:         "smoke-snap-1756147200",
		LastHandledSyncRequest: "first-run",
	}
	syncClaim := newDataClaim(mirror, mirror.Status.WorkPVC, 0, "sync")
	publishClaim := newDataClaim(mirror, mirror.Status.ActivePVC, 1756147200, "publish-data")
	scheme := testScheme(t)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&mirrorv1alpha1.Mirror{}, &batchv1.Job{}, &snapshotv1.VolumeSnapshot{}, &appsv1.Deployment{}).
		WithObjects(mirror, syncClaim, publishClaim).
		Build()
	addBoundSyncPVC(t, ctx, fakeClient, mirror, "smoke-sync", "s3.mirrors.zjusct.io", hostnameAffinity("s3.mirrors.zjusct.io"))
	reconciler := &MirrorReconciler{
		Client:      fakeClient,
		Scheme:      scheme,
		Now:         func() time.Time { return now },
		Config:      testConfig(),
		SyncLimiter: NewSyncLimiter(0),
	}
	request := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: mirror.Namespace, Name: mirror.Name}}

	reconcile(t, ctx, reconciler, request) // repair/create publish workload first
	deployment := &appsv1.Deployment{}
	get(t, ctx, fakeClient, client.ObjectKey{Namespace: mirror.Namespace, Name: "smoke-publish-http"}, deployment)
	deployment.Status.ObservedGeneration = deployment.Generation
	deployment.Status.AvailableReplicas = 1
	deployment.Status.UpdatedReplicas = 1
	if err := fakeClient.Status().Update(ctx, deployment); err != nil {
		t.Fatalf("mark Deployment available: %v", err)
	}
	reconcile(t, ctx, reconciler, request) // start the next synchronization run
	current := getMirror(t, ctx, fakeClient, request.NamespacedName)
	if currentSyncJobName(current) == "" || currentSyncTimestamp(current) != now.Unix() ||
		currentSyncSnapshotName(current) != fmt.Sprintf("smoke-snap-%d", now.Unix()) {
		t.Fatalf("expected a current run with names derived from the new task timestamp, got %#v", current.Status)
	}
	reconcile(t, ctx, reconciler, request) // create Job
	job := &batchv1.Job{}
	get(t, ctx, fakeClient, client.ObjectKey{Namespace: mirror.Namespace, Name: currentSyncJobName(current)}, job)
	jobData := findVolume(job.Spec.Template.Spec.Volumes, "sync-data")
	if jobData == nil || jobData.PersistentVolumeClaim == nil || jobData.PersistentVolumeClaim.ClaimName != mirror.Status.WorkPVC {
		t.Fatalf("next synchronization Job must mount the stable sync PVC %q as sync-data, got %#v", mirror.Status.WorkPVC, jobData)
	}
	assertNotFound(t, ctx, fakeClient, client.ObjectKey{Namespace: mirror.Namespace, Name: currentSyncSnapshotName(current)}, &snapshotv1.VolumeSnapshot{})
}

func testMirror() *mirrorv1alpha1.Mirror {
	return &mirrorv1alpha1.Mirror{
		TypeMeta: metav1.TypeMeta{APIVersion: mirrorv1alpha1.GroupVersion.String(), Kind: "Mirror"},
		ObjectMeta: metav1.ObjectMeta{
			Namespace:  "mirrors",
			Name:       "smoke",
			UID:        types.UID("test-mirror-uid"),
			Generation: 1,
		},
		Spec: mirrorv1alpha1.MirrorSpec{
			Info: mirrorv1alpha1.MirrorInfo{
				Name:        mirrorv1alpha1.LocalizedString{"en": "Smoke"},
				Description: mirrorv1alpha1.LocalizedString{"en": "Controller smoke test"},
				Upstream:    "generated locally",
			},
			Sync: mirrorv1alpha1.MirrorSyncSpec{
				Interval:          metav1.Duration{Duration: time.Hour},
				RetryInterval:     metav1.Duration{Duration: 15 * time.Minute},
				Timeout:           metav1.Duration{Duration: 10 * time.Minute},
				FailureRetryLimit: 3,
				KeepFailedJobs:    1,
				PodTemplate: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:    "sync",
						Image:   "busybox:1.37.0",
						Command: []string{"sh", "-c"},
						Args:    []string{"date -u > /data/index.html"},
						VolumeMounts: []corev1.VolumeMount{
							{
								Name:      "sync-config",
								MountPath: "/etc/sync",
								ReadOnly:  true,
							},
							{
								// The controller injects the sync-data
								// volume only; mounting it (and where) is
								// the user's declaration.
								Name:      "sync-data",
								MountPath: "/data",
							},
						},
					}},
					Volumes: []corev1.Volume{{
						Name: "sync-config",
						VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
							LocalObjectReference: corev1.LocalObjectReference{Name: "sync-config"},
						}},
					}},
				}},
			},
			Storage: mirrorv1alpha1.MirrorStorageSpec{
				StorageClassName:        "retain-class",
				PublishStorageClassName: "delete-class",
				Capacity:                resource.MustParse("1Gi"),
				AccessMode:              corev1.ReadWriteOnce,
				VolumeSnapshotClassName: "snapshot-class",
				Retention:               mirrorv1alpha1.MirrorRetentionSpec{PreviousSnapshots: 1},
			},
			Publish: mirrorv1alpha1.MirrorServicesSpec{
				HTTP: &mirrorv1alpha1.MirrorHTTPServiceSpec{
					MirrorServiceSpec: mirrorv1alpha1.MirrorServiceSpec{
						PodTemplate: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
							Containers: []corev1.Container{{
								Name:  "web",
								Image: "nginxinc/nginx-unprivileged:1.31.0-alpine",
								Ports: []corev1.ContainerPort{{Name: "web", ContainerPort: 8080, Protocol: corev1.ProtocolTCP}},
							}},
						}},
					},
				},
			},
		},
	}
}

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	for name, add := range map[string]func(*runtime.Scheme) error{
		"core":     clientgoscheme.AddToScheme,
		"snapshot": snapshotv1.AddToScheme,
		"gateway":  gatewayv1.Install,
		"mirror":   mirrorv1alpha1.AddToScheme,
	} {
		if err := add(scheme); err != nil {
			t.Fatalf("add %s scheme: %v", name, err)
		}
	}
	return scheme
}

func reconcile(t *testing.T, ctx context.Context, reconciler *MirrorReconciler, request ctrl.Request) {
	t.Helper()
	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
}

func getMirror(t *testing.T, ctx context.Context, c client.Client, key client.ObjectKey) *mirrorv1alpha1.Mirror {
	t.Helper()
	value := &mirrorv1alpha1.Mirror{}
	get(t, ctx, c, key, value)
	return value
}

func get(t *testing.T, ctx context.Context, c client.Client, key client.ObjectKey, value client.Object) {
	t.Helper()
	if err := c.Get(ctx, key, value); err != nil {
		t.Fatalf("get %T %s: %v", value, key, err)
	}
}

func assertNotFound(t *testing.T, ctx context.Context, c client.Client, key client.ObjectKey, value client.Object) {
	t.Helper()
	if err := c.Get(ctx, key, value); err == nil {
		t.Fatalf("expected %T %s not to exist", value, key)
	}
}

// TestSnapshotTimestampConflictDegradesAndKeepsTransaction: when the timestamp
// allocated at sync task creation is already taken (here by a leftover PVC
// carrying the derived publish-PVC name from a previous same-second run), the
// Job cannot be created: the reconcile stops with a
// Degraded/SnapshotTimestampConflict condition and a Warning event, keeps the
// current transaction intact, and does not clear anything.
func TestSnapshotTimestampConflictDegradesAndKeepsTransaction(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 25, 20, 0, 0, 0, time.UTC)
	mirror := testMirror()
	mirror.Finalizers = []string{MirrorFinalizer}
	mirror.Status = mirrorv1alpha1.MirrorStatus{
		ObservedGeneration: mirror.Generation,
		WorkPVC:            "smoke-sync",
		CurrentSync:        &mirrorv1alpha1.MirrorCurrentSyncStatus{StartedAt: timePtr(now)},
	}
	scheme := testScheme(t)
	conflictingPVC := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: mirror.Namespace,
			Name:      fmt.Sprintf("smoke-snap-%d", now.Unix()),
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{
				corev1.ResourceStorage: mirror.Spec.Storage.Capacity.DeepCopy(),
			}},
		},
	}
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&mirrorv1alpha1.Mirror{}, &batchv1.Job{}).
		WithObjects(mirror, conflictingPVC).
		Build()
	// No Job object exists yet: lookupOrCreateSyncJob takes the creation path
	// and hits the timestamp check before creating anything.
	recorder := record.NewFakeRecorder(20)
	reconciler := &MirrorReconciler{
		Client:      fakeClient,
		Scheme:      scheme,
		Recorder:    recorder,
		Now:         func() time.Time { return now },
		Config:      testConfig(),
		SyncLimiter: NewSyncLimiter(0),
	}
	request := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: mirror.Namespace, Name: mirror.Name}}

	reconcile(t, ctx, reconciler, request) // hits the conflict branch at Job creation

	current := getMirror(t, ctx, fakeClient, request.NamespacedName)
	cond := findCondition(current.Status.Conditions, conditionDegraded)
	if cond == nil || cond.Reason != "SnapshotTimestampConflict" {
		t.Fatalf("expected SnapshotTimestampConflict condition, got %#v", current.Status.Conditions)
	}
	// The current transaction is kept: a later reconcile (after 1 minute) retries
	// Job creation instead of discarding the run.
	if currentSyncJobName(current) == "" || currentSyncTimestamp(current) != now.Unix() {
		t.Fatalf("current transaction must be kept for the retry, got %#v", current.Status)
	}
	assertNotFound(t, ctx, fakeClient, client.ObjectKey{Namespace: mirror.Namespace, Name: currentSyncJobName(current)}, &batchv1.Job{})
	select {
	case event := <-recorder.Events:
		if !strings.Contains(event, "SnapshotTimestampConflict") {
			t.Fatalf("expected a SnapshotTimestampConflict event, got %q", event)
		}
	default:
		t.Fatal("expected a Warning event for the timestamp conflict")
	}
}

// TestFailureRetryIntervals pins the failure retry semantics: while
// status.consecutiveFailures is below spec.sync.failureRetryLimit a failed run
// is queued after spec.sync.retryInterval; afterwards the counter stops
// incrementing and the next attempt waits for spec.sync.interval. A successful
// publication resets the counter to zero.
func TestFailureRetryIntervals(t *testing.T) {
	ctx := context.Background()
	base := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	clock := base
	mirror := testMirror()
	mirror.Finalizers = []string{MirrorFinalizer}
	mirror.Spec.Sync.Interval = metav1.Duration{Duration: time.Hour}
	mirror.Spec.Sync.RetryInterval = metav1.Duration{Duration: 15 * time.Minute}
	mirror.Spec.Sync.FailureRetryLimit = 2
	mirror.Annotations = map[string]string{SyncRequestAnnotation: "failing-run"}
	mirror.Status = mirrorv1alpha1.MirrorStatus{
		ObservedGeneration:     mirror.Generation,
		WorkPVC:                "smoke-sync",
		ActivePVC:              "smoke-snap-1756147200",
		LastHandledSyncRequest: "previous",
	}
	scheme := testScheme(t)
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&mirrorv1alpha1.Mirror{}, &batchv1.Job{}, &snapshotv1.VolumeSnapshot{}, &appsv1.Deployment{}).
		WithObjects(mirror).
		Build()
	addBoundSyncPVC(t, ctx, fakeClient, mirror, "smoke-sync", "s3.mirrors.zjusct.io", hostnameAffinity("s3.mirrors.zjusct.io"))
	reconciler := &MirrorReconciler{
		Client: fakeClient, Scheme: scheme, Recorder: record.NewFakeRecorder(20),
		Now:    func() time.Time { return clock },
		Config: testConfig(), SyncLimiter: NewSyncLimiter(0),
	}
	request := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: mirror.Namespace, Name: mirror.Name}}

	// One full failed run: startSync (allocates the timestamp) -> Job ->
	// Failed condition -> failure path.
	failRun := func(t *testing.T) {
		t.Helper()
		reconcile(t, ctx, reconciler, request) // startSync
		current := getMirror(t, ctx, fakeClient, request.NamespacedName)
		if currentSyncJobName(current) == "" {
			t.Fatalf("expected a current Job, got %#v", current.Status)
		}
		reconcile(t, ctx, reconciler, request) // create the Job
		job := &batchv1.Job{}
		get(t, ctx, fakeClient, client.ObjectKey{Namespace: mirror.Namespace, Name: currentSyncJobName(current)}, job)
		job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Reason: "Error", Message: "upstream unreachable"}}
		if err := fakeClient.Status().Update(ctx, job); err != nil {
			t.Fatalf("mark Job failed: %v", err)
		}
		reconcile(t, ctx, reconciler, request) // failure path
	}

	reconcile(t, ctx, reconciler, request) // publish workload
	markPublishDeploymentAvailable(t, ctx, fakeClient, mirror.Namespace)

	// Failure #1: fast retry queued (retryInterval).
	failRun(t)
	current := getMirror(t, ctx, fakeClient, request.NamespacedName)
	if current.Status.ConsecutiveFailures != 1 {
		t.Fatalf("consecutiveFailures = %d, want 1", current.Status.ConsecutiveFailures)
	}
	if !current.Status.NextSyncAt.Time.Equal(base.Add(15 * time.Minute)) {
		t.Fatalf("nextSyncAt = %v, want retryInterval-based %v", current.Status.NextSyncAt, base.Add(15*time.Minute))
	}
	cond := findCondition(current.Status.Conditions, conditionDegraded)
	if cond == nil || cond.Status != metav1.ConditionTrue || !strings.Contains(cond.Message, "upstream unreachable") {
		t.Fatalf("Degraded must retain the completed synchronization failure, got %#v", cond)
	}

	// Failure #2: still below the limit of 2 -> fast retry again.
	clock = base.Add(15 * time.Minute)
	failRun(t)
	current = getMirror(t, ctx, fakeClient, request.NamespacedName)
	if current.Status.ConsecutiveFailures != 2 {
		t.Fatalf("consecutiveFailures = %d, want 2", current.Status.ConsecutiveFailures)
	}
	if !current.Status.NextSyncAt.Time.Equal(base.Add(30 * time.Minute)) {
		t.Fatalf("nextSyncAt = %v, want retryInterval-based %v", current.Status.NextSyncAt, base.Add(30*time.Minute))
	}

	// Failure #3: limit reached -> the counter stops incrementing and the
	// next attempt waits for the regular interval.
	clock = base.Add(30 * time.Minute)
	failRun(t)
	current = getMirror(t, ctx, fakeClient, request.NamespacedName)
	if current.Status.ConsecutiveFailures != 2 {
		t.Fatalf("consecutiveFailures = %d, want it frozen at the limit 2", current.Status.ConsecutiveFailures)
	}
	if !current.Status.NextSyncAt.Time.Equal(base.Add(90 * time.Minute)) {
		t.Fatalf("nextSyncAt = %v, want interval-based %v", current.Status.NextSyncAt, base.Add(90*time.Minute))
	}

	// A successful publication resets the counter.
	clock = base.Add(90 * time.Minute)
	reconcile(t, ctx, reconciler, request) // startSync
	current = getMirror(t, ctx, fakeClient, request.NamespacedName)
	jobName := currentSyncJobName(current)
	reconcile(t, ctx, reconciler, request) // create the Job
	job := &batchv1.Job{}
	get(t, ctx, fakeClient, client.ObjectKey{Namespace: mirror.Namespace, Name: jobName}, job)
	job.Status.Succeeded = 1
	job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
	completion := metav1.NewTime(clock)
	job.Status.CompletionTime = &completion
	if err := fakeClient.Status().Update(ctx, job); err != nil {
		t.Fatalf("mark Job complete: %v", err)
	}
	reconcile(t, ctx, reconciler, request) // create snapshot (transaction timestamp reused)
	snapshot := &snapshotv1.VolumeSnapshot{}
	current = getMirror(t, ctx, fakeClient, request.NamespacedName)
	get(t, ctx, fakeClient, client.ObjectKey{Namespace: mirror.Namespace, Name: currentSyncSnapshotName(current)}, snapshot)
	ready := true
	snapshot.Status = &snapshotv1.VolumeSnapshotStatus{ReadyToUse: &ready}
	if err := fakeClient.Status().Update(ctx, snapshot); err != nil {
		t.Fatalf("mark snapshot ready: %v", err)
	}
	reconcile(t, ctx, reconciler, request) // clone publish PVC + publish rollout
	markPublishDeploymentAvailable(t, ctx, fakeClient, mirror.Namespace)
	reconcile(t, ctx, reconciler, request) // publish

	current = getMirror(t, ctx, fakeClient, request.NamespacedName)
	if current.Status.ActivePVC == "" {
		t.Fatalf("expected a published mirror, got %#v", current.Status)
	}
	if current.Status.ConsecutiveFailures != 0 {
		t.Fatalf("consecutiveFailures = %d, want 0 after success", current.Status.ConsecutiveFailures)
	}
	if !current.Status.NextSyncAt.Time.Equal(clock.Add(time.Hour)) {
		t.Fatalf("nextSyncAt = %v, want interval-based %v", current.Status.NextSyncAt, clock.Add(time.Hour))
	}
}

// TestKeepFailedJobsPrunesOldestFailed: after a sync terminal state the
// controller keeps only the newest spec.sync.keepFailedJobs failed Jobs (by
// creation time) and never touches succeeded Jobs (they are pruned with their
// snapshot generation).
func TestKeepFailedJobsPrunesOldestFailed(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 30, 14, 0, 0, 0, time.UTC)
	mirror := testMirror()
	mirror.Finalizers = []string{MirrorFinalizer}
	mirror.Annotations = map[string]string{SyncRequestAnnotation: "fail-again"}
	mirror.Status = mirrorv1alpha1.MirrorStatus{
		ObservedGeneration:     mirror.Generation,
		WorkPVC:                "smoke-sync",
		CurrentSync:            &mirrorv1alpha1.MirrorCurrentSyncStatus{StartedAt: timePtr(now)},
		LastHandledSyncRequest: "previous",
	}
	scheme := testScheme(t)
	objects := []client.Object{mirror}
	currentJob := currentSyncJobName(mirror)
	failedNames := []string{"smoke-sync-oldest", "smoke-sync-middle", currentJob}
	for i, name := range failedNames {
		job := &batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{
				Namespace:         mirror.Namespace,
				Name:              name,
				Labels:            map[string]string{MirrorLabel: "smoke"},
				CreationTimestamp: metav1.NewTime(now.Add(time.Duration(i) * time.Minute)),
				OwnerReferences: []metav1.OwnerReference{{
					APIVersion: mirrorv1alpha1.GroupVersion.String(), Kind: "Mirror",
					Name: mirror.Name, UID: mirror.UID, Controller: ptr.To(true),
				}},
			},
			Status: batchv1.JobStatus{Conditions: []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue}}},
		}
		objects = append(objects, job)
	}
	// A succeeded Job with a sync-timestamp label: never touched by the
	// failed-Job pruning.
	objects = append(objects, &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:         mirror.Namespace,
			Name:              "smoke-sync-1756147200",
			Labels:            map[string]string{MirrorLabel: "smoke", SyncTimestampLabel: "1756147200"},
			CreationTimestamp: metav1.NewTime(now.Add(-time.Hour)),
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: mirrorv1alpha1.GroupVersion.String(), Kind: "Mirror",
				Name: mirror.Name, UID: mirror.UID, Controller: ptr.To(true),
			}},
		},
		Status: batchv1.JobStatus{Succeeded: 1, Conditions: []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}},
	})
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&mirrorv1alpha1.Mirror{}, &batchv1.Job{}).
		WithObjects(objects...).
		Build()
	reconciler := &MirrorReconciler{
		Client: fakeClient, Scheme: scheme, Recorder: record.NewFakeRecorder(20),
		Now:    func() time.Time { return now },
		Config: testConfig(), SyncLimiter: NewSyncLimiter(0),
	}
	request := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: mirror.Namespace, Name: mirror.Name}}

	reconcile(t, ctx, reconciler, request) // current Job failed -> failure path + pruning

	current := getMirror(t, ctx, fakeClient, request.NamespacedName)
	if degraded := findCondition(current.Status.Conditions, conditionDegraded); degraded == nil || degraded.Status != metav1.ConditionTrue || current.Status.ConsecutiveFailures != 1 {
		t.Fatalf("expected the failure path to record the failure, got %#v", current.Status)
	}
	for _, name := range []string{"smoke-sync-oldest", "smoke-sync-middle"} {
		assertNotFound(t, ctx, fakeClient, client.ObjectKey{Namespace: mirror.Namespace, Name: name}, &batchv1.Job{})
	}
	get(t, ctx, fakeClient, client.ObjectKey{Namespace: mirror.Namespace, Name: currentJob}, &batchv1.Job{})
	get(t, ctx, fakeClient, client.ObjectKey{Namespace: mirror.Namespace, Name: "smoke-sync-1756147200"}, &batchv1.Job{})
}

// TestVolumeSnapshotClassNameIsRequired: the CRD no longer defaults
// spec.storage.volumeSnapshotClassName; a spec without it lands in
// Degraded/InvalidSpec before anything is created.
func TestVolumeSnapshotClassNameIsRequired(t *testing.T) {
	ctx := context.Background()
	mirror := testMirror()
	mirror.Spec.Storage.VolumeSnapshotClassName = ""
	scheme := testScheme(t)
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&mirrorv1alpha1.Mirror{}).
		WithObjects(mirror).
		Build()
	reconciler := &MirrorReconciler{
		Client: fakeClient, Scheme: scheme, Recorder: record.NewFakeRecorder(20),
		Config: testConfig(), SyncLimiter: NewSyncLimiter(0),
	}
	request := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: mirror.Namespace, Name: mirror.Name}}
	reconcile(t, ctx, reconciler, request) // finalizer
	reconcile(t, ctx, reconciler, request) // validation

	current := getMirror(t, ctx, fakeClient, request.NamespacedName)
	cond := findCondition(current.Status.Conditions, conditionDegraded)
	if cond == nil || !strings.Contains(cond.Message, "volumeSnapshotClassName") {
		t.Fatalf("expected a validation error naming volumeSnapshotClassName, got %#v", cond)
	}
}

// waitForEvent drains the FakeRecorder until an event containing substr is
// seen (route/other events may precede it), failing after the buffer empties.
func waitForEvent(t *testing.T, recorder *record.FakeRecorder, substr string) {
	t.Helper()
	for {
		select {
		case event := <-recorder.Events:
			if strings.Contains(event, substr) {
				return
			}
		default:
			t.Fatalf("expected an event containing %q, none in the buffer", substr)
		}
	}
}

// TestPublishPlacementDerivedFromLocalPV: the publish Deployment's hostname
// node selector is derived from the sync PVC's bound local PV (not from any
// spec field); the hostname key overrides the user template's own value while
// other user keys merge; multi-replica publishing is no longer tied to any
// placement field.
func TestPublishPlacementDerivedFromLocalPV(t *testing.T) {
	ctx := context.Background()
	mirror := testMirror()
	mirror.Spec.Publish.HTTP.Replicas = ptr.To(int32(2))
	mirror.Spec.Publish.HTTP.PodTemplate.Spec.NodeSelector = map[string]string{
		corev1.LabelHostname: "user-chosen.example.com", // must be overridden
		"pool":               "edge",                    // must be merged
	}
	mirror.Finalizers = []string{MirrorFinalizer}
	mirror.Status = mirrorv1alpha1.MirrorStatus{
		ObservedGeneration: mirror.Generation,
		WorkPVC:            "smoke-sync",
		ActivePVC:          "smoke-snap-1756147200",
	}
	scheme := testScheme(t)
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&mirrorv1alpha1.Mirror{}, &appsv1.Deployment{}).
		WithObjects(mirror).
		Build()
	addBoundSyncPVC(t, ctx, fakeClient, mirror, "smoke-sync", "s3.mirrors.zjusct.io", hostnameAffinity("s3.mirrors.zjusct.io"))
	reconciler := &MirrorReconciler{
		Client: fakeClient, Scheme: scheme, Recorder: record.NewFakeRecorder(20),
		Now:    func() time.Time { return time.Now().UTC() },
		Config: testConfig(), SyncLimiter: NewSyncLimiter(0),
	}
	request := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: mirror.Namespace, Name: mirror.Name}}
	reconcile(t, ctx, reconciler, request) // valid spec: publish workload ensured

	if errs := validateMirror(mirror); len(errs) != 0 {
		t.Fatalf("multi-replica publishing needs no placement field any more, got %v", errs.ToAggregate())
	}
	deployment := &appsv1.Deployment{}
	get(t, ctx, fakeClient, client.ObjectKey{Namespace: mirror.Namespace, Name: "smoke-publish-http"}, deployment)
	if got := *deployment.Spec.Replicas; got != 2 {
		t.Fatalf("deployment replicas = %d, want 2", got)
	}
	selector := deployment.Spec.Template.Spec.NodeSelector
	if selector[corev1.LabelHostname] != "s3.mirrors.zjusct.io" {
		t.Fatalf("PV-derived hostname selector missing/overridden, got %#v", selector)
	}
	if selector["pool"] != "edge" {
		t.Fatalf("user nodeSelector keys must merge, got %#v", selector)
	}
	if deployment.Spec.Template.Spec.Affinity != nil && deployment.Spec.Template.Spec.Affinity.NodeAffinity != nil {
		t.Fatalf("no affinity may be injected when the PV carries a hostname selector, got %#v", deployment.Spec.Template.Spec.Affinity)
	}
}

// TestPublishPlacementSharedStorageStaysFree: a PV without nodeAffinity
// (shared storage) yields no constraint — publish pods schedule freely and
// multi-replica on RWX is legal.
func TestPublishPlacementSharedStorageStaysFree(t *testing.T) {
	ctx := context.Background()
	mirror := testMirror()
	mirror.Spec.Publish.HTTP.Replicas = ptr.To(int32(2))
	mirror.Finalizers = []string{MirrorFinalizer}
	mirror.Status = mirrorv1alpha1.MirrorStatus{
		ObservedGeneration: mirror.Generation,
		WorkPVC:            "smoke-sync",
		ActivePVC:          "smoke-snap-1756147200",
	}
	scheme := testScheme(t)
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&mirrorv1alpha1.Mirror{}, &appsv1.Deployment{}).
		WithObjects(mirror).
		Build()
	addBoundSyncPVC(t, ctx, fakeClient, mirror, "smoke-sync", "", nil)
	reconciler := &MirrorReconciler{
		Client: fakeClient, Scheme: scheme, Recorder: record.NewFakeRecorder(20),
		Now:    func() time.Time { return time.Now().UTC() },
		Config: testConfig(), SyncLimiter: NewSyncLimiter(0),
	}
	request := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: mirror.Namespace, Name: mirror.Name}}
	reconcile(t, ctx, reconciler, request)

	deployment := &appsv1.Deployment{}
	get(t, ctx, fakeClient, client.ObjectKey{Namespace: mirror.Namespace, Name: "smoke-publish-http"}, deployment)
	if len(deployment.Spec.Template.Spec.NodeSelector) != 0 {
		t.Fatalf("shared storage must not gain a nodeSelector, got %#v", deployment.Spec.Template.Spec.NodeSelector)
	}
	if deployment.Spec.Template.Spec.Affinity != nil && deployment.Spec.Template.Spec.Affinity.NodeAffinity != nil {
		t.Fatalf("shared storage must not gain a nodeAffinity, got %#v", deployment.Spec.Template.Spec.Affinity)
	}
}

// TestPublishPlacementNonHostnameAffinityCopied: a PV whose nodeAffinity has
// no hostname expression (another topology shape) copies its required terms
// verbatim into the pod affinity; a user-provided nodeAffinity is overridden
// with a Warning event (volume locality is authoritative).
func TestPublishPlacementNonHostnameAffinityCopied(t *testing.T) {
	ctx := context.Background()
	mirror := testMirror()
	mirror.Spec.Publish.HTTP.PodTemplate.Spec.Affinity = &corev1.Affinity{
		NodeAffinity: &corev1.NodeAffinity{RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
			NodeSelectorTerms: []corev1.NodeSelectorTerm{{
				MatchExpressions: []corev1.NodeSelectorRequirement{{Key: "wrong", Operator: corev1.NodeSelectorOpIn, Values: []string{"x"}}},
			}},
		}},
	}
	mirror.Finalizers = []string{MirrorFinalizer}
	mirror.Status = mirrorv1alpha1.MirrorStatus{
		ObservedGeneration: mirror.Generation,
		WorkPVC:            "smoke-sync",
		ActivePVC:          "smoke-snap-1756147200",
	}
	scheme := testScheme(t)
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&mirrorv1alpha1.Mirror{}, &appsv1.Deployment{}).
		WithObjects(mirror).
		Build()
	topology := &corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{{
		MatchExpressions: []corev1.NodeSelectorRequirement{{Key: "topology.example.com/zone", Operator: corev1.NodeSelectorOpIn, Values: []string{"z1"}}},
	}}}
	addBoundSyncPVC(t, ctx, fakeClient, mirror, "smoke-sync", "", topology)
	recorder := record.NewFakeRecorder(20)
	reconciler := &MirrorReconciler{
		Client: fakeClient, Scheme: scheme, Recorder: recorder,
		Now:    func() time.Time { return time.Now().UTC() },
		Config: testConfig(), SyncLimiter: NewSyncLimiter(0),
	}
	request := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: mirror.Namespace, Name: mirror.Name}}
	reconcile(t, ctx, reconciler, request)

	deployment := &appsv1.Deployment{}
	get(t, ctx, fakeClient, client.ObjectKey{Namespace: mirror.Namespace, Name: "smoke-publish-http"}, deployment)
	affinity := deployment.Spec.Template.Spec.Affinity
	if affinity == nil || affinity.NodeAffinity == nil || affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution == nil {
		t.Fatalf("PV nodeAffinity must be copied into the pod, got %#v", affinity)
	}
	terms := affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms
	if len(terms) != 1 || len(terms[0].MatchExpressions) != 1 || terms[0].MatchExpressions[0].Key != "topology.example.com/zone" {
		t.Fatalf("PV-derived affinity terms wrong: %#v", terms)
	}
	if len(deployment.Spec.Template.Spec.NodeSelector) != 0 {
		t.Fatalf("no hostname selector may be injected for a non-hostname topology, got %#v", deployment.Spec.Template.Spec.NodeSelector)
	}
	waitForEvent(t, recorder, "PublishNodeAffinityOverridden")
}

// TestPublishDeferredUntilSourcePVReadable: without a bound sync PVC/PV the
// placement cannot be derived — no publish Deployment is created (never
// without the volume locality constraint), the Mirror waits in Publishing,
// and a Warning event explains the deferral.
func TestPublishDeferredUntilSourcePVReadable(t *testing.T) {
	ctx := context.Background()
	mirror := testMirror()
	mirror.Finalizers = []string{MirrorFinalizer}
	mirror.Status = mirrorv1alpha1.MirrorStatus{
		ObservedGeneration: mirror.Generation,
		WorkPVC:            "smoke-sync",
		ActivePVC:          "smoke-snap-1756147200",
	}
	scheme := testScheme(t)
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&mirrorv1alpha1.Mirror{}, &appsv1.Deployment{}).
		WithObjects(mirror).
		Build()
	recorder := record.NewFakeRecorder(20)
	reconciler := &MirrorReconciler{
		Client: fakeClient, Scheme: scheme, Recorder: recorder,
		Now:    func() time.Time { return time.Now().UTC() },
		Config: testConfig(), SyncLimiter: NewSyncLimiter(0),
	}
	request := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: mirror.Namespace, Name: mirror.Name}}
	reconcile(t, ctx, reconciler, request) // publish deferred

	assertNotFound(t, ctx, fakeClient, client.ObjectKey{Namespace: mirror.Namespace, Name: "smoke-publish-http"}, &appsv1.Deployment{})
	current := getMirror(t, ctx, fakeClient, request.NamespacedName)
	cond := findCondition(current.Status.Conditions, conditionProgressing)
	if cond == nil || cond.Status != metav1.ConditionTrue {
		t.Fatalf("expected publication convergence to report Progressing=True, got %#v", cond)
	}
	waitForEvent(t, recorder, "PublishPlacementPending")

	// Once the sync PVC shows up bound to a local PV, the next reconcile
	// creates the Deployment with the derived constraint.
	addBoundSyncPVC(t, ctx, fakeClient, mirror, "smoke-sync", "s3.mirrors.zjusct.io", hostnameAffinity("s3.mirrors.zjusct.io"))
	reconcile(t, ctx, reconciler, request)
	deployment := &appsv1.Deployment{}
	get(t, ctx, fakeClient, client.ObjectKey{Namespace: mirror.Namespace, Name: "smoke-publish-http"}, deployment)
	if got := deployment.Spec.Template.Spec.NodeSelector[corev1.LabelHostname]; got != "s3.mirrors.zjusct.io" {
		t.Fatalf("derived hostname selector missing after PVC bound, got %#v", deployment.Spec.Template.Spec.NodeSelector)
	}
}

// TestPublishSchemaInCRDs pins the generated schema contracts that a plain
// fake-client test cannot exercise: the fixed services keys with the
// replicas/podTemplate shape (no enable, no mirrorMountPath), the
// declaration-time podTemplate.spec presence CEL rule, and the absence of the
// old array shape (per-entry name/image/ports) — in both the Mirror and the
// ProxyMirror CRD.
func TestPublishSchemaInCRDs(t *testing.T) {
	// The committed chart CRDs are installed manually (helm does not upgrade
	// crds/), so this test only guards the realistic drift: the committed
	// YAML lagging behind a type change. It pins the fixed-key services shape,
	// the embedded corev1 pod template schema, and the CEL rule.
	for _, crd := range []string{"mirrors.zjusct.io_mirrors.yaml", "mirrors.zjusct.io_proxymirrors.yaml"} {
		t.Run(crd, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join("..", "..", "charts", "falcon", "crds", crd))
			if err != nil {
				t.Fatalf("read CRD: %v", err)
			}
			var doc map[string]interface{}
			if err := yaml.Unmarshal(raw, &doc); err != nil {
				t.Fatalf("parse CRD: %v", err)
			}
			services, ok := findSchemaNode(doc, "publish")
			if !ok {
				t.Fatal("publish schema not found in CRD")
			}
			properties, _ := services["properties"].(map[string]interface{})
			http, ok := properties["http"].(map[string]interface{})
			if !ok {
				t.Fatalf("publish.http schema missing, got %v", properties)
			}
			httpProperties, _ := http["properties"].(map[string]interface{})
			for _, required := range []string{"replicas", "podTemplate"} {
				if _, has := httpProperties[required]; !has {
					t.Fatalf("services.http.%s schema missing, got %v", required, httpProperties)
				}
			}
			for _, removed := range []string{"enable", "mirrorMountPath"} {
				if _, has := httpProperties[removed]; has {
					t.Fatalf("services.http.%s schema must be gone (2026-09 spec decision), got %v", removed, httpProperties)
				}
			}
			// The full corev1 pod template schema must be embedded.
			podTemplate, _ := httpProperties["podTemplate"].(map[string]interface{})
			templateProperties, _ := podTemplate["properties"].(map[string]interface{})
			spec, _ := templateProperties["spec"].(map[string]interface{})
			specProperties, _ := spec["properties"].(map[string]interface{})
			containers, _ := specProperties["containers"].(map[string]interface{})
			if _, has := containers["items"]; !has {
				t.Fatal("podTemplate.spec.containers must carry the full corev1 schema")
			}

			// The declaration-time presence CEL rule on the service spec
			// (key present -> podTemplate.spec present).
			rules, _ := http["x-kubernetes-validations"].([]interface{})
			messages := map[string]bool{}
			for _, rule := range rules {
				if r, ok := rule.(map[string]interface{}); ok {
					if m, _ := r["message"].(string); m != "" {
						messages[m] = true
					}
				}
			}
			if !messages["podTemplate.spec is required when the service key is declared"] {
				t.Fatalf("podTemplate presence CEL rule missing, got %#v", rules)
			}
		})
	}
}

// findVolume returns the pod volume with the given name (nil when absent).
func findVolume(volumes []corev1.Volume, name string) *corev1.Volume {
	for i := range volumes {
		if volumes[i].Name == name {
			return &volumes[i]
		}
	}
	return nil
}

// findMount returns the container volumeMount with the given volume name
// (nil when absent).
func findMount(container corev1.Container, name string) *corev1.VolumeMount {
	for i := range container.VolumeMounts {
		if container.VolumeMounts[i].Name == name {
			return &container.VolumeMounts[i]
		}
	}
	return nil
}

// addBoundSyncPVC creates the stable sync PVC bound to a local PV whose
// nodeAffinity pins `hostname` (the OpenEBS zfs local PV shape), so the
// publish placement derivation can resolve. affinity == nil simulates shared
// storage (PV without nodeAffinity). pvcName defaults to <base>-sync.
func addBoundSyncPVC(t *testing.T, ctx context.Context, c client.Client, mirror *mirrorv1alpha1.Mirror, pvcName string, hostname string, affinity *corev1.NodeSelector) {
	t.Helper()
	if pvcName == "" {
		base := childBase(mirror.Name)
		pvcName = base + "-sync"
	}
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Namespace: mirror.Namespace, Name: pvcName},
		Spec: corev1.PersistentVolumeClaimSpec{
			VolumeName: pvcName + "-pv",
			Resources: corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{
				corev1.ResourceStorage: resource.MustParse("1Gi"),
			}},
		},
	}
	pv := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: pvcName + "-pv"},
		Spec:       corev1.PersistentVolumeSpec{NodeAffinity: &corev1.VolumeNodeAffinity{Required: affinity}},
	}
	// The fake client does not bind PVCs: upsert the volumeName the real
	// binder would set, so publishPlacement can resolve the PV.
	existing := &corev1.PersistentVolumeClaim{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: mirror.Namespace, Name: pvcName}, existing); err != nil {
		if !apierrors.IsNotFound(err) {
			t.Fatalf("get sync PVC: %v", err)
		}
		if err := c.Create(ctx, pvc); err != nil {
			t.Fatalf("create bound sync PVC: %v", err)
		}
	} else if existing.Spec.VolumeName == "" {
		existing.Spec.VolumeName = pvcName + "-pv"
		if err := c.Update(ctx, existing); err != nil {
			t.Fatalf("bind sync PVC: %v", err)
		}
	}
	if err := c.Create(ctx, pv); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("create sync PV: %v", err)
	}
}

// hostnameAffinity is the local-PV nodeAffinity shape: a required term with a
// single kubernetes.io/hostname In expression.
func hostnameAffinity(hostname string) *corev1.NodeSelector {
	return &corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{{
		MatchExpressions: []corev1.NodeSelectorRequirement{{
			Key: corev1.LabelHostname, Operator: corev1.NodeSelectorOpIn, Values: []string{hostname},
		}},
	}}}
}

// findSchemaNode walks a decoded YAML/JSON document and returns the first
// object stored under the given key.
func findSchemaNode(node interface{}, key string) (map[string]interface{}, bool) {
	switch value := node.(type) {
	case map[string]interface{}:
		if child, ok := value[key].(map[string]interface{}); ok {
			return child, true
		}
		for _, v := range value {
			if found, ok := findSchemaNode(v, key); ok {
				return found, true
			}
		}
	case []interface{}:
		for _, v := range value {
			if found, ok := findSchemaNode(v, key); ok {
				return found, true
			}
		}
	}
	return nil, false
}

// TestSyncPodTemplateDefaultsAndInjection: the sync Job is built from the
// user's sync.podTemplate with the writable sync-data volume forced (volumes
// only) and the silent-only defaults injected (runAsUser 65532, restricted
// profile, /tmp emptyDir, IfNotPresent pull policy). The published testMirror
// template already carries the user input volume, which must be preserved.
func TestSyncPodTemplateDefaultsAndInjection(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC)
	mirror := testMirror()
	mirror.Finalizers = []string{MirrorFinalizer}
	scheme := testScheme(t)
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&mirrorv1alpha1.Mirror{}, &batchv1.Job{}).
		WithObjects(mirror).
		Build()
	reconciler := &MirrorReconciler{
		Client: fakeClient, Scheme: scheme, Recorder: record.NewFakeRecorder(20),
		Now: func() time.Time { return now }, Config: testConfig(), SyncLimiter: NewSyncLimiter(0),
	}
	request := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: mirror.Namespace, Name: mirror.Name}}
	reconcile(t, ctx, reconciler, request) // finalizer
	reconcile(t, ctx, reconciler, request) // startSync
	reconcile(t, ctx, reconciler, request) // sync PVC + sync Job

	current := getMirror(t, ctx, fakeClient, request.NamespacedName)
	job := &batchv1.Job{}
	get(t, ctx, fakeClient, client.ObjectKey{Namespace: mirror.Namespace, Name: currentSyncJobName(current)}, job)
	podSpec := job.Spec.Template.Spec
	first := podSpec.Containers[0]
	// Silent fields got the sync defaults.
	if first.ImagePullPolicy != corev1.PullIfNotPresent {
		t.Fatalf("imagePullPolicy must default to IfNotPresent, got %q", first.ImagePullPolicy)
	}
	if got := ptr.Deref(podSpec.SecurityContext.RunAsUser, 0); got != 65532 {
		t.Fatalf("runAsUser must default to 65532, got %d", got)
	}
	if !ptr.Deref(first.SecurityContext.ReadOnlyRootFilesystem, false) || ptr.Deref(first.SecurityContext.AllowPrivilegeEscalation, true) {
		t.Fatalf("restricted-profile container defaults missing: %#v", first.SecurityContext)
	}
	if v := findVolume(podSpec.Volumes, "tmp"); v == nil || v.EmptyDir == nil {
		t.Fatalf("a silent template must get the /tmp emptyDir volume, got %#v", v)
	}
	// Job-level pipeline identity is forced.
	if podSpec.RestartPolicy != corev1.RestartPolicyNever {
		t.Fatalf("restartPolicy must be forced to Never, got %q", podSpec.RestartPolicy)
	}
	if job.Spec.BackoffLimit == nil || *job.Spec.BackoffLimit != 0 || job.Spec.ActiveDeadlineSeconds == nil || *job.Spec.ActiveDeadlineSeconds != 600 {
		t.Fatalf("Job backoffLimit/deadline wrong: %#v %#v", job.Spec.BackoffLimit, job.Spec.ActiveDeadlineSeconds)
	}
	// The user image/command flow through the template untouched.
	if first.Image != "busybox:1.37.0" || len(first.Command) != 2 || first.Command[0] != "sh" {
		t.Fatalf("user image/command lost: %#v", first)
	}
}

// TestSyncReservedDataVolumeRejected: a user volume named sync-data collides
// with the controller-injected writable sync PVC volume and is InvalidSpec; a
// sync template without containers or image is invalid as well.
func TestSyncReservedDataVolumeRejected(t *testing.T) {
	mirror := testMirror()

	broken := mirror.DeepCopy()
	broken.Spec.Sync.PodTemplate.Spec.Volumes = append(broken.Spec.Sync.PodTemplate.Spec.Volumes, corev1.Volume{
		Name:         "sync-data",
		VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
	})
	if errs := validateMirror(broken); len(errs) == 0 || !strings.Contains(errs.ToAggregate().Error(), "reserved") {
		t.Fatalf("a user volume named sync-data must be rejected as reserved, got %v", errs)
	}

	broken = mirror.DeepCopy()
	broken.Spec.Sync.PodTemplate = corev1.PodTemplateSpec{}
	if errs := validateMirror(broken); len(errs) == 0 || !strings.Contains(errs.ToAggregate().Error(), "containers") {
		t.Fatalf("a sync template without containers must be InvalidSpec, got %v", errs)
	}

	broken = mirror.DeepCopy()
	broken.Spec.Sync.PodTemplate.Spec.Containers[0].Image = ""
	if errs := validateMirror(broken); len(errs) == 0 || !strings.Contains(errs.ToAggregate().Error(), "image") {
		t.Fatalf("a sync container without an image must be InvalidSpec, got %v", errs)
	}
}
