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
	if current.Status.PendingJob != fmt.Sprintf("smoke-sync-%d", now.Unix()) {
		t.Fatalf("expected pending sync Job smoke-sync-<ts>, got %#v", current.Status)
	}
	if current.Status.PendingSyncTimestamp != now.Unix() ||
		current.Status.PendingPVC != fmt.Sprintf("smoke-snap-%d", now.Unix()) ||
		current.Status.PendingSnapshot != fmt.Sprintf("smoke-snap-%d", now.Unix()) {
		t.Fatalf("expected timestamped names derived from the task creation timestamp, got %#v", current.Status)
	}

	reconcile(t, ctx, reconciler, request) // sync PVC + sync Job
	workClaim := &corev1.PersistentVolumeClaim{}
	get(t, ctx, fakeClient, client.ObjectKey{Namespace: mirror.Namespace, Name: current.Status.WorkPVC}, workClaim)
	job := &batchv1.Job{}
	get(t, ctx, fakeClient, client.ObjectKey{Namespace: mirror.Namespace, Name: current.Status.PendingJob}, job)
	jobClaim := job.Spec.Template.Spec.Volumes[0].PersistentVolumeClaim.ClaimName
	if jobClaim != current.Status.WorkPVC {
		t.Fatalf("sync Job mounted %q; expected sync PVC %q", jobClaim, current.Status.WorkPVC)
	}
	// Input volumes (ConfigMap/Secret) must always be mounted read-only; the
	// CR-level readOnly override no longer exists.
	jobMounts := job.Spec.Template.Spec.Containers[0].VolumeMounts
	if len(jobMounts) != 2 {
		t.Fatalf("want data + input mounts, got %#v", jobMounts)
	}
	if jobMounts[1].Name != "sync-config" || jobMounts[1].MountPath != "/etc/sync" || !jobMounts[1].ReadOnly {
		t.Fatalf("input volume must be mounted read-only, got %#v", jobMounts[1])
	}
	if job.Spec.Template.Spec.NodeName != "" {
		t.Fatalf("sync Job bypasses the scheduler with spec.nodeName %q", job.Spec.Template.Spec.NodeName)
	}
	if got := job.Spec.Template.Spec.NodeSelector[corev1.LabelHostname]; got != "s3.mirrors.zjusct.io" {
		t.Fatalf("sync Job hostname selector = %q; expected storage node", got)
	}
	assertNotFound(t, ctx, fakeClient, client.ObjectKey{Namespace: mirror.Namespace, Name: current.Status.PendingSnapshot}, &snapshotv1.VolumeSnapshot{})
	assertNotFound(t, ctx, fakeClient, client.ObjectKey{Namespace: mirror.Namespace, Name: current.Status.PendingPVC}, &corev1.PersistentVolumeClaim{})

	job.Status.Succeeded = 1
	job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
	completion := metav1.NewTime(now)
	job.Status.CompletionTime = &completion
	if err := fakeClient.Status().Update(ctx, job); err != nil {
		t.Fatalf("mark Job complete: %v", err)
	}
	// After Job success the pending timestamp is simply reused: no separate
	// allocation step, the persisted names go straight into child objects.
	reconcile(t, ctx, reconciler, request) // post-sync snapshot
	snapshot := &snapshotv1.VolumeSnapshot{}
	get(t, ctx, fakeClient, client.ObjectKey{Namespace: mirror.Namespace, Name: current.Status.PendingSnapshot}, snapshot)
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
	get(t, ctx, fakeClient, client.ObjectKey{Namespace: mirror.Namespace, Name: current.Status.PendingPVC}, publishClaim)
	if publishClaim.Spec.DataSource == nil || publishClaim.Spec.DataSource.Kind != "VolumeSnapshot" || publishClaim.Spec.DataSource.Name != current.Status.PendingSnapshot {
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
	deploymentClaim := deployment.Spec.Template.Spec.Volumes[0].PersistentVolumeClaim.ClaimName
	if deploymentClaim != current.Status.PendingPVC {
		t.Fatalf("publish Deployment mounted %q; expected clone PVC %q", deploymentClaim, current.Status.PendingPVC)
	}
	if !deployment.Spec.Template.Spec.Volumes[0].PersistentVolumeClaim.ReadOnly || !deployment.Spec.Template.Spec.Containers[0].VolumeMounts[0].ReadOnly {
		t.Fatal("publish clone must be mounted read-only in both the volume source and container mount")
	}
	if got := deployment.Spec.Template.Spec.Containers[0].VolumeMounts[0].MountPath; got != "/usr/share/nginx/html/smoke" {
		t.Fatalf("data PVC must be mounted at <mountPath>/<name> so PVC-root content is served under the /smoke route prefix, got %q", got)
	}
	// The first container port is (re)named after the service so the Service
	// and probes reference a named port; the Service targets it on port 80.
	containerPorts := deployment.Spec.Template.Spec.Containers[0].Ports
	if len(containerPorts) != 1 || containerPorts[0].Name != "http" || containerPorts[0].ContainerPort != 8080 {
		t.Fatalf("publish container ports = %#v; want the single declared port renamed to http", containerPorts)
	}
	if deployment.Spec.Template.Spec.NodeName != "" {
		t.Fatalf("publish Deployment bypasses the scheduler with spec.nodeName %q", deployment.Spec.Template.Spec.NodeName)
	}
	if got := deployment.Spec.Template.Spec.NodeSelector[corev1.LabelHostname]; got != "s3.mirrors.zjusct.io" {
		t.Fatalf("publish Deployment hostname selector = %q; expected storage node", got)
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
	if current.Status.Phase != mirrorv1alpha1.PhaseReady {
		t.Fatalf("expected Ready, got %s", current.Status.Phase)
	}
	reconcile(t, ctx, reconciler, request) // published Mirror: publish route ensured

	// The published Mirror is served through the protocol-named Service of
	// the "http" service entry and a controller-generated publish HTTPRoute.
	service := &corev1.Service{}
	get(t, ctx, fakeClient, client.ObjectKey{Namespace: mirror.Namespace, Name: "smoke-publish-http"}, service)
	if got := service.Spec.Ports[0].Port; got != 80 {
		t.Fatalf("publish Service port = %d, want 80", got)
	}
	if got := service.Spec.Selector[RoleLabel]; got != "publish-http" {
		t.Fatalf("publish Service selector role = %q, want publish-http (per-service pods)", got)
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
		Phase:                  mirrorv1alpha1.PhaseReady,
		WorkPVC:                "smoke-sync",
		ActivePVC:              "smoke-snap-1756147200",
		ActiveSnapshot:         "smoke-snap-1756147200",
		LastHandledSyncRequest: "first-run",
	}
	syncClaim, err := newDataClaim(mirror, mirror.Status.WorkPVC, 0, "sync")
	if err != nil {
		t.Fatalf("build sync claim: %v", err)
	}
	publishClaim, err := newDataClaim(mirror, mirror.Status.ActivePVC, 1756147200, "publish-data")
	if err != nil {
		t.Fatalf("build publish claim: %v", err)
	}
	scheme := testScheme(t)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&mirrorv1alpha1.Mirror{}, &batchv1.Job{}, &snapshotv1.VolumeSnapshot{}, &appsv1.Deployment{}).
		WithObjects(mirror, syncClaim, publishClaim).
		Build()
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
	if current.Status.PendingJob == "" || current.Status.PendingSyncTimestamp != now.Unix() ||
		current.Status.PendingPVC != fmt.Sprintf("smoke-snap-%d", now.Unix()) ||
		current.Status.PendingSnapshot != fmt.Sprintf("smoke-snap-%d", now.Unix()) {
		t.Fatalf("expected a pending run with names derived from the new task timestamp, got %#v", current.Status)
	}
	reconcile(t, ctx, reconciler, request) // create Job
	job := &batchv1.Job{}
	get(t, ctx, fakeClient, client.ObjectKey{Namespace: mirror.Namespace, Name: current.Status.PendingJob}, job)
	if got := job.Spec.Template.Spec.Volumes[0].PersistentVolumeClaim.ClaimName; got != mirror.Status.WorkPVC {
		t.Fatalf("next synchronization Job mounted %q; expected stable sync PVC %q", got, mirror.Status.WorkPVC)
	}
	assertNotFound(t, ctx, fakeClient, client.ObjectKey{Namespace: mirror.Namespace, Name: current.Status.PendingSnapshot}, &snapshotv1.VolumeSnapshot{})
}

func testMirror() *mirrorv1alpha1.Mirror {
	replicas := int32(1)
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
				Type:        "sync",
				Upstream:    "generated locally",
			},
			Sync: mirrorv1alpha1.MirrorSyncSpec{
				Interval:          metav1.Duration{Duration: time.Hour},
				RetryInterval:     metav1.Duration{Duration: 15 * time.Minute},
				Timeout:           metav1.Duration{Duration: 10 * time.Minute},
				FailureRetryLimit: 3,
				KeepFailedJobs:    1,
				Image:             "busybox:1.37.0",
				Command:           []string{"sh", "-c"},
				Args:              []string{"date -u > /data/index.html"},
				Volumes: []mirrorv1alpha1.MirrorInputVolume{{
					Name:      "sync-config",
					MountPath: "/etc/sync",
					ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: "sync-config"}},
				}},
				DataMountPath: "/data",
			},
			Storage: mirrorv1alpha1.MirrorStorageSpec{
				StorageClassName:        "retain-class",
				ServingStorageClassName: "delete-class",
				Capacity:                resource.MustParse("1Gi"),
				AccessMode:              corev1.ReadWriteOnce,
				VolumeSnapshotClassName: "snapshot-class",
				NodeName:                "s3.mirrors.zjusct.io",
				Retention:               mirrorv1alpha1.MirrorRetentionSpec{PreviousSnapshots: 1},
			},
			Services: []mirrorv1alpha1.MirrorServingService{{
				Name:          "http",
				Image:         "nginxinc/nginx-unprivileged:1.31.0-alpine",
				Replicas:      &replicas,
				Ports:         []corev1.ContainerPort{{Name: "web", ContainerPort: 8080, Protocol: corev1.ProtocolTCP}},
				MountPath:     "/usr/share/nginx/html",
				ReadinessPath: "/",
			}},
		},
	}
}

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	for name, add := range map[string]func(*runtime.Scheme) error{
		"core":     clientgoscheme.AddToScheme,
		"snapshot": snapshotv1.AddToScheme,
		"gateway":  gatewayv1.AddToScheme,
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

// TestChildBaseNamingRules pins the childBase/resourceName contract: charset
// mapping, the defensive empty fallback, and the hard 63-character limit that
// replaced the former truncation+sha256 shortening.
func TestChildBaseNamingRules(t *testing.T) {
	cases := []struct {
		name string
		want string
	}{
		{name: "smoke", want: "smoke"},
		{name: "Debian.Archive_2", want: "debian-archive-2"},
		{name: "a.b.c", want: "a-b-c"},
		{name: "...", want: "mirror"}, // everything dropped -> defensive fallback
	}
	for _, tc := range cases {
		got, err := childBase(tc.name)
		if err != nil {
			t.Fatalf("childBase(%q): %v", tc.name, err)
		}
		if got != tc.want {
			t.Fatalf("childBase(%q) = %q, want %q", tc.name, got, tc.want)
		}
	}

	if _, err := childBase(strings.Repeat("a", 63)); err != nil {
		t.Fatalf("63-character base must be accepted: %v", err)
	}
	_, err := childBase(strings.Repeat("a", 64))
	if err == nil {
		t.Fatal("64-character base must be rejected")
	}
	if !strings.Contains(err.Error(), "64") || !strings.Contains(err.Error(), "63") {
		t.Fatalf("error must name the offending length and the limit: %v", err)
	}

	if _, err := resourceName(strings.Repeat("a", 58), "sync"); err != nil {
		t.Fatalf("63-character name must be accepted: %v", err)
	}
	if _, err := resourceName(strings.Repeat("a", 59), "sync"); err == nil {
		t.Fatal("64-character name must be rejected")
	}
}

// TestOversizedMirrorNameIsInvalidSpec: a CR whose derived child names would
// exceed the 63-character DNS label limit lands in the usual
// Degraded/InvalidSpec path instead of being silently shortened.
func TestOversizedMirrorNameIsInvalidSpec(t *testing.T) {
	ctx := context.Background()
	mirror := testMirror()
	mirror.Name = strings.Repeat("mirror", 11) // 66 characters
	scheme := testScheme(t)
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&mirrorv1alpha1.Mirror{}).
		WithObjects(mirror).
		Build()
	reconciler := &MirrorReconciler{
		Client:      fakeClient,
		Scheme:      scheme,
		Recorder:    record.NewFakeRecorder(20),
		Config:      testConfig(),
		SyncLimiter: NewSyncLimiter(0),
	}
	request := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: mirror.Namespace, Name: mirror.Name}}
	reconcile(t, ctx, reconciler, request) // finalizer
	reconcile(t, ctx, reconciler, request) // validation

	current := getMirror(t, ctx, fakeClient, request.NamespacedName)
	if current.Status.Phase != mirrorv1alpha1.PhaseDegraded {
		t.Fatalf("expected Degraded, got %s", current.Status.Phase)
	}
	for _, conditionType := range []string{conditionReady, conditionProgressing, conditionDegraded} {
		cond := findCondition(current.Status.Conditions, conditionType)
		if cond == nil || cond.Reason != "InvalidSpec" {
			t.Fatalf("expected InvalidSpec condition for %s, got %#v", conditionType, cond)
		}
	}
	if current.Status.PendingJob != "" {
		t.Fatalf("no sync must start for an invalid spec, got %#v", current.Status)
	}
}

// TestSnapshotTimestampConflictDegradesAndKeepsPending: when the timestamp
// allocated at sync task creation is already taken (here by a leftover PVC
// carrying the derived publish-PVC name from a previous same-second run), the
// Job cannot be created: the reconcile stops with a
// Degraded/SnapshotTimestampConflict condition and a Warning event, keeps the
// pending pipeline intact, and does not clear anything.
func TestSnapshotTimestampConflictDegradesAndKeepsPending(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 25, 20, 0, 0, 0, time.UTC)
	mirror := testMirror()
	mirror.Finalizers = []string{MirrorFinalizer}
	mirror.Status = mirrorv1alpha1.MirrorStatus{
		ObservedGeneration:   mirror.Generation,
		Phase:                mirrorv1alpha1.PhaseInitializing,
		WorkPVC:              "smoke-sync",
		PendingJob:           fmt.Sprintf("smoke-sync-%d", now.Unix()),
		PendingSyncTimestamp: now.Unix(),
		PendingPVC:           fmt.Sprintf("smoke-snap-%d", now.Unix()),
		PendingSnapshot:      fmt.Sprintf("smoke-snap-%d", now.Unix()),
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
	if current.Status.Phase != mirrorv1alpha1.PhaseDegraded {
		t.Fatalf("expected Degraded, got %s", current.Status.Phase)
	}
	cond := findCondition(current.Status.Conditions, conditionDegraded)
	if cond == nil || cond.Reason != "SnapshotTimestampConflict" {
		t.Fatalf("expected SnapshotTimestampConflict condition, got %#v", current.Status.Conditions)
	}
	// The pending pipeline is kept: a later reconcile (after 1 minute) retries
	// Job creation instead of discarding the run.
	if current.Status.PendingJob == "" || current.Status.PendingSyncTimestamp != now.Unix() {
		t.Fatalf("pending pipeline must be kept for the retry, got %#v", current.Status)
	}
	assertNotFound(t, ctx, fakeClient, client.ObjectKey{Namespace: mirror.Namespace, Name: current.Status.PendingJob}, &batchv1.Job{})
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
		Phase:                  mirrorv1alpha1.PhaseReady,
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
		if current.Status.PendingJob == "" {
			t.Fatalf("expected a pending Job, got %#v", current.Status)
		}
		reconcile(t, ctx, reconciler, request) // create the Job
		job := &batchv1.Job{}
		get(t, ctx, fakeClient, client.ObjectKey{Namespace: mirror.Namespace, Name: current.Status.PendingJob}, job)
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
	cond := findCondition(current.Status.Conditions, conditionProgressing)
	if cond == nil || !strings.Contains(cond.Message, "retry queued") {
		t.Fatalf("Progressing condition must mention the queued retry, got %#v", cond)
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
	jobName := current.Status.PendingJob
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
	reconcile(t, ctx, reconciler, request) // create snapshot (pending timestamp reused)
	snapshot := &snapshotv1.VolumeSnapshot{}
	current = getMirror(t, ctx, fakeClient, request.NamespacedName)
	get(t, ctx, fakeClient, client.ObjectKey{Namespace: mirror.Namespace, Name: current.Status.PendingSnapshot}, snapshot)
	ready := true
	snapshot.Status = &snapshotv1.VolumeSnapshotStatus{ReadyToUse: &ready}
	if err := fakeClient.Status().Update(ctx, snapshot); err != nil {
		t.Fatalf("mark snapshot ready: %v", err)
	}
	reconcile(t, ctx, reconciler, request) // clone publish PVC + publish rollout
	markPublishDeploymentAvailable(t, ctx, fakeClient, mirror.Namespace)
	reconcile(t, ctx, reconciler, request) // publish

	current = getMirror(t, ctx, fakeClient, request.NamespacedName)
	if current.Status.Phase != mirrorv1alpha1.PhaseReady || current.Status.ActivePVC == "" {
		t.Fatalf("expected a published Ready mirror, got %#v", current.Status)
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
		Phase:                  mirrorv1alpha1.PhaseDegraded,
		WorkPVC:                "smoke-sync",
		PendingJob:             "smoke-sync-newest",
		LastHandledSyncRequest: "previous",
	}
	scheme := testScheme(t)
	objects := []client.Object{mirror}
	failedNames := []string{"smoke-sync-oldest", "smoke-sync-middle", "smoke-sync-newest"}
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

	reconcile(t, ctx, reconciler, request) // pending Job failed -> failure path + pruning

	current := getMirror(t, ctx, fakeClient, request.NamespacedName)
	if current.Status.Phase != mirrorv1alpha1.PhaseDegraded || current.Status.ConsecutiveFailures != 1 {
		t.Fatalf("expected the failure path to record the failure, got %#v", current.Status)
	}
	for _, name := range []string{"smoke-sync-oldest", "smoke-sync-middle"} {
		assertNotFound(t, ctx, fakeClient, client.ObjectKey{Namespace: mirror.Namespace, Name: name}, &batchv1.Job{})
	}
	get(t, ctx, fakeClient, client.ObjectKey{Namespace: mirror.Namespace, Name: "smoke-sync-newest"}, &batchv1.Job{})
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
	if current.Status.Phase != mirrorv1alpha1.PhaseDegraded {
		t.Fatalf("expected Degraded, got %s", current.Status.Phase)
	}
	cond := findCondition(current.Status.Conditions, conditionDegraded)
	if cond == nil || !strings.Contains(cond.Message, "volumeSnapshotClassName") {
		t.Fatalf("expected a validation error naming volumeSnapshotClassName, got %#v", cond)
	}
}

// TestMultiReplicaPublishRequiresStorageNodeName: publishing with any service
// replicas > 1 while spec.storage.nodeName is empty is an InvalidSpec — with a
// ReadWriteOnce data PVC all publish pods must be pinned to the storage node.
func TestMultiReplicaPublishRequiresStorageNodeName(t *testing.T) {
	ctx := context.Background()
	mirror := testMirror()
	mirror.Spec.Storage.NodeName = ""
	mirror.Spec.Services[0].Replicas = ptr.To(int32(2))
	mirror.Finalizers = []string{MirrorFinalizer}
	mirror.Status = mirrorv1alpha1.MirrorStatus{
		ObservedGeneration: mirror.Generation,
		Phase:              mirrorv1alpha1.PhaseReady,
		WorkPVC:            "smoke-sync",
		ActivePVC:          "smoke-snap-1756147200",
	}
	scheme := testScheme(t)
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&mirrorv1alpha1.Mirror{}, &appsv1.Deployment{}).
		WithObjects(mirror).
		Build()
	reconciler := &MirrorReconciler{
		Client: fakeClient, Scheme: scheme, Recorder: record.NewFakeRecorder(20),
		Now:    func() time.Time { return time.Now().UTC() },
		Config: testConfig(), SyncLimiter: NewSyncLimiter(0),
	}
	request := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: mirror.Namespace, Name: mirror.Name}}
	reconcile(t, ctx, reconciler, request) // finalizer
	reconcile(t, ctx, reconciler, request) // validation

	current := getMirror(t, ctx, fakeClient, request.NamespacedName)
	if current.Status.Phase != mirrorv1alpha1.PhaseDegraded {
		t.Fatalf("expected Degraded, got %s", current.Status.Phase)
	}
	cond := findCondition(current.Status.Conditions, conditionDegraded)
	if cond == nil || !strings.Contains(cond.Message, "storage.nodeName") {
		t.Fatalf("expected a validation error naming storage.nodeName, got %#v", cond)
	}

	// Pinning the node satisfies the co-location requirement: the spec
	// validates and the publish Deployment rolls out with the declared
	// replica count.
	if errs := validateMirror(func() *mirrorv1alpha1.Mirror {
		pinned := mirror.DeepCopy()
		pinned.Spec.Storage.NodeName = "s3.mirrors.zjusct.io"
		return pinned
	}()); len(errs) > 0 {
		t.Fatalf("pinned node must satisfy the co-location requirement, got %v", errs.ToAggregate())
	}
	current = getMirror(t, ctx, fakeClient, request.NamespacedName)
	current.Spec.Storage.NodeName = "s3.mirrors.zjusct.io"
	if err := fakeClient.Update(ctx, current); err != nil {
		t.Fatalf("pin storage node: %v", err)
	}
	reconcile(t, ctx, reconciler, request) // valid spec: publish workload ensured
	deployment := &appsv1.Deployment{}
	get(t, ctx, fakeClient, client.ObjectKey{Namespace: mirror.Namespace, Name: "smoke-publish-http"}, deployment)
	if got := *deployment.Spec.Replicas; got != 2 {
		t.Fatalf("deployment replicas = %d, want 2", got)
	}
}

// TestServicesSchemaInCRDs pins the generated schema contracts that a plain
// fake-client test cannot exercise: services without minItems (absent/empty
// services = sync-only mirror is legal), the name uniqueness CEL rule, the
// protocol enum, and the ports minItems 1 — in both the Mirror and the
// ProxyMirror CRD.
func TestServicesSchemaInCRDs(t *testing.T) {
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
			services, ok := findSchemaNode(doc, "services")
			if !ok {
				t.Fatal("services schema not found in CRD")
			}
			if _, has := services["minItems"]; has {
				t.Fatalf("services must not enforce minItems (empty services = sync-only mirror), got %v", services["minItems"])
			}
			rules, ok := services["x-kubernetes-validations"].([]interface{})
			if !ok || len(rules) == 0 {
				t.Fatalf("services must carry x-kubernetes-validations, got %#v", services)
			}
			foundUnique := false
			for _, rule := range rules {
				r, _ := rule.(map[string]interface{})
				if r["message"] == "service names must be unique" {
					foundUnique = true
				}
			}
			if !foundUnique {
				t.Fatalf("name-uniqueness CEL rule missing, got %#v", rules)
			}
			items, _ := services["items"].(map[string]interface{})
			properties, _ := items["properties"].(map[string]interface{})
			name, _ := properties["name"].(map[string]interface{})
			enum, _ := name["enum"].([]interface{})
			if len(enum) != 3 {
				t.Fatalf("name enum = %#v, want http/rsync/git", enum)
			}
			ports, _ := properties["ports"].(map[string]interface{})
			if ports["minItems"] != float64(1) {
				t.Fatalf("ports minItems = %v, want 1", ports["minItems"])
			}
		})
	}
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
