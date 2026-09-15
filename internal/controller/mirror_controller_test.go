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
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	"sigs.k8s.io/yaml"

	mirrorv1alpha1 "github.com/ZJUSCT/falcon/api/v1alpha1"
)

func TestAtomicPublicationUsesStableSyncPVCAndSnapshotClone(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 25, 18, 0, 0, 0, time.UTC)
	mirror := testMirror()
	mirror.CreationTimestamp = metav1.NewTime(now.Add(-time.Hour))
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
		testSnapshotName(current) != fmt.Sprintf("smoke-snap-%d", now.Unix()) {
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
	assertNotFound(t, ctx, fakeClient, client.ObjectKey{Namespace: mirror.Namespace, Name: testSnapshotName(current)}, &snapshotv1.VolumeSnapshot{})
	assertNotFound(t, ctx, fakeClient, client.ObjectKey{Namespace: mirror.Namespace, Name: testSnapshotName(current)}, &corev1.PersistentVolumeClaim{})

	job.Status.Succeeded = 1
	job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
	completion := metav1.NewTime(now.Add(10 * time.Minute))
	job.Status.StartTime = timePtr(now.Add(time.Minute))
	now = now.Add(20 * time.Minute)
	job.Status.CompletionTime = &completion
	if err := fakeClient.Status().Update(ctx, job); err != nil {
		t.Fatalf("mark Job complete: %v", err)
	}
	// After Job success the transaction timestamp is simply reused: no
	// separate allocation step is needed.
	reconcile(t, ctx, reconciler, request) // post-sync snapshot
	observed := getMirror(t, ctx, fakeClient, request.NamespacedName)
	if observed.Status.LastSync == nil || !observed.Status.LastSync.FinishedAt.Equal(&completion) || observed.Status.LastAttempt != nil || observed.Status.CurrentSync == nil || observed.Status.CurrentSync.Phase != mirrorv1alpha1.SyncPhaseSnapshotting || observed.Status.Publication != nil || observed.Status.LastPublishedAt != nil {
		t.Fatalf("Job result must be durable before publication: %#v", observed.Status)
	}
	snapshot := &snapshotv1.VolumeSnapshot{}
	get(t, ctx, fakeClient, client.ObjectKey{Namespace: mirror.Namespace, Name: testSnapshotName(current)}, snapshot)
	if snapshot.Spec.Source.PersistentVolumeClaimName == nil || *snapshot.Spec.Source.PersistentVolumeClaimName != current.Status.WorkPVC {
		t.Fatalf("snapshot source was not the stable sync PVC: %#v", snapshot.Spec.Source)
	}

	ready := true
	snapshot.Status = &snapshotv1.VolumeSnapshotStatus{ReadyToUse: &ready}
	if err := fakeClient.Status().Update(ctx, snapshot); err != nil {
		t.Fatalf("mark snapshot ready: %v", err)
	}
	reconcile(t, ctx, reconciler, request) // durable ready snapshot handoff
	reconcile(t, ctx, reconciler, request) // publish PVC clone and workload
	publishClaim := &corev1.PersistentVolumeClaim{}
	get(t, ctx, fakeClient, client.ObjectKey{Namespace: mirror.Namespace, Name: testSnapshotName(current)}, publishClaim)
	if publishClaim.Spec.DataSource == nil || publishClaim.Spec.DataSource.Kind != "VolumeSnapshot" || publishClaim.Spec.DataSource.Name != testSnapshotName(current) {
		t.Fatalf("publish PVC was not cloned from the completed snapshot: %#v", publishClaim.Spec.DataSource)
	}
	if publishClaim.Name != publishClaim.Spec.DataSource.Name {
		t.Fatalf("publish PVC %q must carry the same name as its snapshot, got datasource %#v", publishClaim.Name, publishClaim.Spec.DataSource)
	}
	if publishClaim.Spec.StorageClassName == nil || *publishClaim.Spec.StorageClassName != "delete-class" {
		t.Fatalf("publish PVC storage class = %v; expected disposable snapshot class", publishClaim.Spec.StorageClassName)
	}
	// The workload already exists; Kubernetes binds the PVC before running its Pods.
	addBoundPublishPVC(t, ctx, fakeClient, mirror, testSnapshotName(current))
	reconcile(t, ctx, reconciler, request) // Deployment + Service
	deployment := &appsv1.Deployment{}
	get(t, ctx, fakeClient, client.ObjectKey{Namespace: mirror.Namespace, Name: "smoke-publish-http"}, deployment)
	dataVolume := findVolume(deployment.Spec.Template.Spec.Volumes, "mirror-data")
	if dataVolume == nil || dataVolume.PersistentVolumeClaim == nil || dataVolume.PersistentVolumeClaim.ClaimName != testSnapshotName(current) {
		t.Fatalf("publish Deployment must inject the clone PVC %q as mirror-data volume, got %#v", testSnapshotName(current), dataVolume)
	}
	if !dataVolume.PersistentVolumeClaim.ReadOnly {
		t.Fatal("the mirror-data volume source must be read-only")
	}
	// The controller injects the volume only: no mount is added, mounting is
	// the user template's declaration.
	if dataMount := findMount(deployment.Spec.Template.Spec.Containers[0], "mirror-data"); dataMount != nil {
		t.Fatalf("the controller must not mount mirror-data itself, got %#v", dataMount)
	}
	// Declared container ports remain operator-owned; the first one is the
	// numeric Service target.
	containerPorts := deployment.Spec.Template.Spec.Containers[0].Ports
	if len(containerPorts) != 1 || containerPorts[0].Name != "web" || containerPorts[0].ContainerPort != 8080 {
		t.Fatalf("publish container ports = %#v; want the single declared port preserved", containerPorts)
	}
	if deployment.Spec.Template.Spec.NodeName != "" {
		t.Fatalf("publish Deployment bypasses the scheduler with spec.nodeName %q", deployment.Spec.Template.Spec.NodeName)
	}
	// No placement is injected any more: volume locality is enforced by the
	// scheduler through the bound clone PV's nodeAffinity, not by Falcon.
	if len(deployment.Spec.Template.Spec.NodeSelector) != 0 {
		t.Fatalf("publish Deployment must carry no controller-injected nodeSelector, got %#v", deployment.Spec.Template.Spec.NodeSelector)
	}
	if deployment.Spec.Template.Spec.Affinity != nil {
		t.Fatalf("publish Deployment must carry no controller-injected affinity, got %#v", deployment.Spec.Template.Spec.Affinity)
	}

	// Explicit rollout failure must be visible even on the first publication,
	// while preserving the successful Job and allowing this rollout to recover.
	deployment.Status.ObservedGeneration = deployment.Generation
	deployment.Status.Conditions = []appsv1.DeploymentCondition{{
		Type: appsv1.DeploymentProgressing, Status: corev1.ConditionFalse,
		Reason: "ProgressDeadlineExceeded", Message: "new ReplicaSet has not become available",
	}}
	if err := fakeClient.Status().Update(ctx, deployment); err != nil {
		t.Fatal(err)
	}
	reconcile(t, ctx, reconciler, request)
	current = getMirror(t, ctx, fakeClient, request.NamespacedName)
	degraded := findCondition(current.Status.Conditions, conditionDegraded)
	if degraded == nil || degraded.Status != metav1.ConditionTrue || degraded.Reason != "PublishDeploymentFailed" || !strings.Contains(degraded.Message, "ProgressDeadlineExceeded") {
		t.Fatalf("missing Deployment failure: %#v", degraded)
	}
	if current.Status.CurrentSync != nil || current.Status.Publication == nil || current.Status.LastPublishedAt != nil || current.Status.LastAttempt == nil || current.Status.LastSync.Phase != mirrorv1alpha1.SyncPhaseSucceeded || current.Status.ConsecutiveFailures != 0 {
		t.Fatalf("rollout failure must preserve the transaction and successful Job: %#v", current.Status)
	}
	get(t, ctx, fakeClient, client.ObjectKeyFromObject(deployment), deployment)
	deployment.Status.Conditions = nil
	deployment.Status.ObservedGeneration = deployment.Generation
	deployment.Status.AvailableReplicas = 1
	deployment.Status.UpdatedReplicas = 1
	deployment.Status.Replicas = 1
	if err := fakeClient.Status().Update(ctx, deployment); err != nil {
		t.Fatalf("mark Deployment available: %v", err)
	}
	markRouteAccepted(t, ctx, fakeClient, mirror.Namespace, "smoke-publish")
	reconcile(t, ctx, reconciler, request) // publish status
	current = getMirror(t, ctx, fakeClient, request.NamespacedName)
	if current.Status.LastSync == nil || !current.Status.LastSync.FinishedAt.Equal(&completion) || !current.Status.LastSuccessfulSyncAt.Equal(&completion) || !current.Status.LastPublishedAt.Equal(timePtr(now)) || !current.Status.LastAttempt.FinishedAt.Equal(timePtr(now)) {
		t.Fatalf("sync completion and publication must retain distinct times: %#v", current.Status)
	}
	if current.Status.ActivePVC != publishClaim.Name || current.Status.ActiveSnapshot != snapshot.Name {
		t.Fatalf("unexpected published status: %#v", current.Status)
	}
	degraded = findCondition(current.Status.Conditions, conditionDegraded)
	if degraded == nil || degraded.Status != metav1.ConditionFalse {
		t.Fatalf("recovered rollout must clear degradation: %#v", degraded)
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
	markRouteAccepted(t, ctx, fakeClient, mirror.Namespace, route.Name)
	reconcile(t, ctx, reconciler, request)
	current = getMirror(t, ctx, fakeClient, request.NamespacedName)
	assertMirrorZLifecycleStatus(t, fakeClient, fmt.Sprintf("S%dX%dN%d", completion.Unix(), current.Status.NextSyncAt.Unix(), mirror.CreationTimestamp.Unix()))

	// A fresh reconciler proves success history survives a controller restart.
	restarted := *reconciler
	restarted.SyncLimiter = NewSyncLimiter(0)
	reconciler = &restarted
	now = now.Add(time.Hour)
	current.Annotations = map[string]string{SyncRequestAnnotation: "true"}
	if err := fakeClient.Update(ctx, current); err != nil {
		t.Fatal(err)
	}
	reconcile(t, ctx, reconciler, request)
	assertMirrorZLifecycleStatus(t, fakeClient, fmt.Sprintf("D%dO%dN%d", now.Unix(), completion.Unix(), mirror.CreationTimestamp.Unix()))
	reconcile(t, ctx, reconciler, request)
	current = getMirror(t, ctx, fakeClient, request.NamespacedName)
	get(t, ctx, fakeClient, client.ObjectKey{Namespace: mirror.Namespace, Name: currentSyncJobName(current)}, job)
	started := timePtr(now.Add(time.Minute))
	job.Status.StartTime = started
	if err := fakeClient.Status().Update(ctx, job); err != nil {
		t.Fatal(err)
	}
	reconcile(t, ctx, reconciler, request)
	assertMirrorZLifecycleStatus(t, fakeClient, fmt.Sprintf("Y%dO%dN%d", started.Unix(), completion.Unix(), mirror.CreationTimestamp.Unix()))

	now = now.Add(10 * time.Minute)
	job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, LastTransitionTime: metav1.NewTime(now)}}
	if err := fakeClient.Status().Update(ctx, job); err != nil {
		t.Fatal(err)
	}
	reconcile(t, ctx, reconciler, request)
	current = getMirror(t, ctx, fakeClient, request.NamespacedName)
	assertMirrorZLifecycleStatus(t, fakeClient, fmt.Sprintf("F%dO%dX%dN%d", started.Unix(), completion.Unix(), current.Status.NextSyncAt.Unix(), mirror.CreationTimestamp.Unix()))
}

func TestNextSnapshotStillWritesOnlyToSyncPVC(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 25, 19, 0, 0, 0, time.UTC)
	mirror := testMirror()
	mirror.Finalizers = []string{MirrorFinalizer}
	mirror.Annotations = map[string]string{SyncRequestAnnotation: "true"}
	mirror.Status = mirrorv1alpha1.MirrorStatus{
		ObservedGeneration: mirror.Generation,
		WorkPVC:            "smoke-sync",
		ActivePVC:          "smoke-snap-1756147200",
		ActiveSnapshot:     "smoke-snap-1756147200",
	}
	syncClaim := newDataClaim(mirror, mirror.Status.WorkPVC, 0, "sync")
	publishClaim := newDataClaim(mirror, mirror.Status.ActivePVC, 1756147200, "publish-data")
	// The fake client never binds PVCs: preset the volumeName the real binder
	// sets once the clone's PV exists, so publish workload creation proceeds.
	publishClaim.Spec.VolumeName = mirror.Status.ActivePVC + "-pv"
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
	deployment.Status.Replicas = 1
	if err := fakeClient.Status().Update(ctx, deployment); err != nil {
		t.Fatalf("mark Deployment available: %v", err)
	}
	markRouteAccepted(t, ctx, fakeClient, mirror.Namespace, "smoke-publish")
	reconcile(t, ctx, reconciler, request) // start the next synchronization run
	current := getMirror(t, ctx, fakeClient, request.NamespacedName)
	if currentSyncJobName(current) == "" || currentSyncTimestamp(current) != now.Unix() ||
		testSnapshotName(current) != fmt.Sprintf("smoke-snap-%d", now.Unix()) {
		t.Fatalf("expected a current run with names derived from the new task timestamp, got %#v", current.Status)
	}
	reconcile(t, ctx, reconciler, request) // create Job
	job := &batchv1.Job{}
	get(t, ctx, fakeClient, client.ObjectKey{Namespace: mirror.Namespace, Name: currentSyncJobName(current)}, job)
	jobData := findVolume(job.Spec.Template.Spec.Volumes, "sync-data")
	if jobData == nil || jobData.PersistentVolumeClaim == nil || jobData.PersistentVolumeClaim.ClaimName != mirror.Status.WorkPVC {
		t.Fatalf("next synchronization Job must mount the stable sync PVC %q as sync-data, got %#v", mirror.Status.WorkPVC, jobData)
	}
	assertNotFound(t, ctx, fakeClient, client.ObjectKey{Namespace: mirror.Namespace, Name: testSnapshotName(current)}, &snapshotv1.VolumeSnapshot{})
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
				Description: "Controller smoke test",
				Upstream:    "generated locally",
			},
			Sync: mirrorv1alpha1.MirrorSyncSpec{
				Interval:          metav1.Duration{Duration: time.Hour},
				RetryInterval:     metav1.Duration{Duration: 15 * time.Minute},
				Timeout:           metav1.Duration{Duration: 10 * time.Minute},
				FailureRetryLimit: 3,
				KeepJobs:          3,
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
				PVCSpec: corev1.PersistentVolumeClaimSpec{
					AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
					Resources:   corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")}},
				},
				SyncStorageClassName:    "retain-class",
				PublishStorageClassName: "delete-class",
				VolumeSnapshotClassName: "snapshot-class",
				Retention:               1,
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
		CurrentSync:        &mirrorv1alpha1.MirrorCurrentSyncStatus{QueuedAt: timePtr(now)},
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
				corev1.ResourceStorage: mirror.Spec.Storage.PVCSpec.Resources.Requests[corev1.ResourceStorage].DeepCopy(),
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
	mirror.Annotations = map[string]string{SyncRequestAnnotation: "true"}
	mirror.Status = mirrorv1alpha1.MirrorStatus{
		ObservedGeneration: mirror.Generation,
		WorkPVC:            "smoke-sync",
		ActivePVC:          "smoke-snap-1756147200",
	}
	scheme := testScheme(t)
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&mirrorv1alpha1.Mirror{}, &batchv1.Job{}, &snapshotv1.VolumeSnapshot{}, &appsv1.Deployment{}).
		WithObjects(mirror).
		Build()
	addBoundPublishPVC(t, ctx, fakeClient, mirror, mirror.Status.ActivePVC)
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
	markRouteAccepted(t, ctx, fakeClient, mirror.Namespace, "smoke-publish")

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
	get(t, ctx, fakeClient, client.ObjectKey{Namespace: mirror.Namespace, Name: testSnapshotName(current)}, snapshot)
	ready := true
	snapshot.Status = &snapshotv1.VolumeSnapshotStatus{ReadyToUse: &ready}
	if err := fakeClient.Status().Update(ctx, snapshot); err != nil {
		t.Fatalf("mark snapshot ready: %v", err)
	}
	reconcile(t, ctx, reconciler, request) // clone publish PVC and create workload
	addBoundPublishPVC(t, ctx, fakeClient, mirror, testSnapshotName(current))
	reconcile(t, ctx, reconciler, request) // publish rollout
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

// TestKeepJobsPrunesOldestRegardlessOfOutcome: after a sync terminal state
// the controller keeps only the newest spec.sync.keepJobs Jobs (by creation
// time) — succeeded Jobs are history too — and never touches foreign ones.
func TestKeepJobsPrunesOldestRegardlessOfOutcome(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 30, 14, 0, 0, 0, time.UTC)
	mirror := testMirror()
	mirror.Spec.Sync.KeepJobs = 2
	mirror.Finalizers = []string{MirrorFinalizer}
	mirror.Annotations = map[string]string{SyncRequestAnnotation: "true"}
	mirror.Status = mirrorv1alpha1.MirrorStatus{
		ObservedGeneration: mirror.Generation,
		WorkPVC:            "smoke-sync",
		CurrentSync:        &mirrorv1alpha1.MirrorCurrentSyncStatus{QueuedAt: timePtr(now), Manual: true},
	}
	scheme := testScheme(t)
	objects := []client.Object{mirror}
	currentJob := currentSyncJobName(mirror)
	owned := []struct {
		name   string
		age    time.Duration
		failed bool
	}{
		{currentJob, 0, true},
		{"smoke-sync-succeeded", time.Minute, false},
		{"smoke-sync-oldest", 2 * time.Minute, true},
	}
	for _, entry := range owned {
		condition := batchv1.JobCondition{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}
		if entry.failed {
			condition = batchv1.JobCondition{Type: batchv1.JobFailed, Status: corev1.ConditionTrue}
		}
		objects = append(objects, &batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{
				Namespace:         mirror.Namespace,
				Name:              entry.name,
				Labels:            map[string]string{MirrorLabel: "smoke"},
				CreationTimestamp: metav1.NewTime(now.Add(-entry.age)),
				OwnerReferences: []metav1.OwnerReference{{
					APIVersion: mirrorv1alpha1.GroupVersion.String(), Kind: "Mirror",
					Name: mirror.Name, UID: mirror.UID, Controller: ptr.To(true),
				}},
			},
			Status: batchv1.JobStatus{Conditions: []batchv1.JobCondition{condition}},
		})
	}
	foreign := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{
		Namespace: mirror.Namespace, Name: "smoke-sync-foreign",
		Labels:            map[string]string{MirrorLabel: "smoke"},
		CreationTimestamp: metav1.NewTime(now.Add(-3 * time.Minute)),
	}}
	objects = append(objects, foreign)
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
	// keepJobs=2 retains the newest two regardless of outcome; only the
	// oldest owned Job goes, and the foreign one is never touched.
	assertNotFound(t, ctx, fakeClient, client.ObjectKey{Namespace: mirror.Namespace, Name: "smoke-sync-oldest"}, &batchv1.Job{})
	get(t, ctx, fakeClient, client.ObjectKey{Namespace: mirror.Namespace, Name: currentJob}, &batchv1.Job{})
	get(t, ctx, fakeClient, client.ObjectKey{Namespace: mirror.Namespace, Name: "smoke-sync-succeeded"}, &batchv1.Job{})
	get(t, ctx, fakeClient, client.ObjectKeyFromObject(foreign), &batchv1.Job{})
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

// TestPublishCreatesConsumerBeforeClaimBound prevents a WFFC circular wait:
// the workload must exist so Kubernetes can schedule the clone's first consumer.
func TestPublishCreatesConsumerBeforeClaimBound(t *testing.T) {
	ctx := context.Background()
	mirror := testMirror()
	mirror.Finalizers = []string{MirrorFinalizer}
	mirror.Status = mirrorv1alpha1.MirrorStatus{
		ObservedGeneration: mirror.Generation,
		WorkPVC:            "smoke-sync",
		ActivePVC:          "smoke-snap-1756147200",
	}
	// The publish PVC exists but is still unbound (the clone is provisioning).
	unbound := newDataClaim(mirror, mirror.Status.ActivePVC, 1756147200, "publish-data")
	scheme := testScheme(t)
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&mirrorv1alpha1.Mirror{}, &appsv1.Deployment{}).
		WithObjects(mirror, unbound).
		Build()
	recorder := record.NewFakeRecorder(20)
	reconciler := &MirrorReconciler{
		Client: fakeClient, Scheme: scheme, Recorder: recorder,
		Now:    func() time.Time { return time.Now().UTC() },
		Config: testConfig(), SyncLimiter: NewSyncLimiter(0),
	}
	request := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: mirror.Namespace, Name: mirror.Name}}
	reconcile(t, ctx, reconciler, request) // create the consumer while the PVC is unbound

	current := getMirror(t, ctx, fakeClient, request.NamespacedName)
	cond := findCondition(current.Status.Conditions, conditionProgressing)
	if cond == nil || cond.Status != metav1.ConditionTrue {
		t.Fatalf("expected publication convergence to report Progressing=True, got %#v", cond)
	}
	deployment := &appsv1.Deployment{}
	get(t, ctx, fakeClient, client.ObjectKey{Namespace: mirror.Namespace, Name: "smoke-publish-http"}, deployment)
	volume := findVolume(deployment.Spec.Template.Spec.Volumes, PublishDataVolumeName)
	if volume == nil || volume.PersistentVolumeClaim == nil || volume.PersistentVolumeClaim.ClaimName != mirror.Status.ActivePVC {
		t.Fatalf("publish Deployment must reference the unbound clone PVC, got %#v", volume)
	}
}

// TestPublishStorageClassNameIsRequired: publishStorageClassName is a
// required, explicit operational choice — it is never inherited from
// syncStorageClassName. A spec without it lands in Degraded/InvalidSpec
// before anything is created.
func TestPublishStorageClassNameIsRequired(t *testing.T) {
	ctx := context.Background()
	mirror := testMirror()
	mirror.Spec.Storage.PublishStorageClassName = ""
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
	if cond == nil || !strings.Contains(cond.Message, "publishStorageClassName") {
		t.Fatalf("expected a validation error naming publishStorageClassName, got %#v", cond)
	}
}

// TestPVCTemplateVolumeNameAllowedOthersRejected: pvcTemplate.volumeName is
// accepted (it pre-binds the sync PVC to an existing PV, e.g. cross-instance
// migration), while the other Falcon-managed/unsupported fields —
// storageClassName, dataSource, dataSourceRef, and selector — stay rejected.
func TestPVCTemplateVolumeNameAllowedOthersRejected(t *testing.T) {
	prebound := testMirror()
	prebound.Spec.Storage.PVCSpec.VolumeName = "pvc-migrated-from-old-instance"
	if errs := validateMirror(prebound); len(errs) != 0 {
		t.Fatalf("pvcTemplate.volumeName must be accepted, got %v", errs.ToAggregate())
	}

	cases := map[string]func(*mirrorv1alpha1.Mirror){
		"storageClassName": func(m *mirrorv1alpha1.Mirror) { m.Spec.Storage.PVCSpec.StorageClassName = ptr.To("custom-class") },
		"dataSource": func(m *mirrorv1alpha1.Mirror) {
			m.Spec.Storage.PVCSpec.DataSource = &corev1.TypedLocalObjectReference{Kind: "PersistentVolumeClaim", Name: "other"}
		},
		"dataSourceRef": func(m *mirrorv1alpha1.Mirror) {
			m.Spec.Storage.PVCSpec.DataSourceRef = &corev1.TypedObjectReference{Kind: "PersistentVolumeClaim", Name: "other"}
		},
		"selector": func(m *mirrorv1alpha1.Mirror) {
			m.Spec.Storage.PVCSpec.Selector = &metav1.LabelSelector{MatchLabels: map[string]string{"app": "x"}}
		},
	}
	for name, breakIt := range cases {
		broken := testMirror()
		breakIt(broken)
		errs := validateMirror(broken)
		if len(errs) == 0 || !strings.Contains(errs.ToAggregate().Error(), name) {
			t.Fatalf("%s: pvcTemplate.%s must stay rejected, got %v", name, name, errs)
		}
	}
}

// TestSyncPVCPrebindsVolumeNameButPublishCloneDropsIt: a pvcTemplate
// volumeName flows verbatim into the sync PVC (pre-binding an existing PV,
// e.g. cross-instance migration), but the publish clone PVC never carries it —
// the clone is provisioned through its VolumeSnapshot dataSource, and a preset
// volumeName would keep it Pending forever (the PV controller only takes the
// dynamic-provisioning path while volumeName is empty).
func TestSyncPVCPrebindsVolumeNameButPublishCloneDropsIt(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	mirror := testMirror()
	mirror.Spec.Storage.PVCSpec.VolumeName = "pvc-migrated-from-old-instance"
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
	reconcile(t, ctx, reconciler, request) // sync PVC + sync Job
	current := getMirror(t, ctx, fakeClient, request.NamespacedName)
	workClaim := &corev1.PersistentVolumeClaim{}
	get(t, ctx, fakeClient, client.ObjectKey{Namespace: mirror.Namespace, Name: current.Status.WorkPVC}, workClaim)
	if workClaim.Spec.VolumeName != mirror.Spec.Storage.PVCSpec.VolumeName {
		t.Fatalf("sync PVC must carry the pvcTemplate volumeName (pre-bound PV), got %q", workClaim.Spec.VolumeName)
	}

	job := &batchv1.Job{}
	get(t, ctx, fakeClient, client.ObjectKey{Namespace: mirror.Namespace, Name: currentSyncJobName(current)}, job)
	job.Status.Succeeded = 1
	job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
	completion := metav1.NewTime(now)
	job.Status.CompletionTime = &completion
	if err := fakeClient.Status().Update(ctx, job); err != nil {
		t.Fatalf("mark Job complete: %v", err)
	}
	reconcile(t, ctx, reconciler, request) // post-sync snapshot
	snapshot := &snapshotv1.VolumeSnapshot{}
	get(t, ctx, fakeClient, client.ObjectKey{Namespace: mirror.Namespace, Name: testSnapshotName(current)}, snapshot)
	ready := true
	snapshot.Status = &snapshotv1.VolumeSnapshotStatus{ReadyToUse: &ready}
	if err := fakeClient.Status().Update(ctx, snapshot); err != nil {
		t.Fatalf("mark snapshot ready: %v", err)
	}
	reconcile(t, ctx, reconciler, request) // durable ready snapshot handoff
	reconcile(t, ctx, reconciler, request) // publish PVC clone
	publishClaim := &corev1.PersistentVolumeClaim{}
	get(t, ctx, fakeClient, client.ObjectKey{Namespace: mirror.Namespace, Name: testSnapshotName(current)}, publishClaim)
	if publishClaim.Spec.DataSource == nil || publishClaim.Spec.DataSource.Kind != "VolumeSnapshot" {
		t.Fatalf("publish PVC must be cloned through its dataSource, got %#v", publishClaim.Spec.DataSource)
	}
	if publishClaim.Spec.VolumeName != "" {
		t.Fatalf("publish clone PVC must not carry a volumeName (it would stay Pending forever), got %q", publishClaim.Spec.VolumeName)
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

			// The declaration-time presence CEL rules: the http key admits
			// either a serving podTemplate.spec or a redirect; the rsync key
			// (validated at the services level) keeps requiring podTemplate.spec.
			rules, _ := http["x-kubernetes-validations"].([]interface{})
			messages := map[string]bool{}
			for _, rule := range rules {
				if r, ok := rule.(map[string]interface{}); ok {
					if m, _ := r["message"].(string); m != "" {
						messages[m] = true
					}
				}
			}
			if !messages["podTemplate.spec or redirect is required when the http service key is declared"] {
				t.Fatalf("http either-or CEL rule missing, got %#v", rules)
			}
			// Only the Mirror CRD has an rsync key; its podTemplate presence
			// rule moved to the services level.
			if crd == "mirrors.zjusct.io_mirrors.yaml" {
				servicesRules, _ := services["x-kubernetes-validations"].([]interface{})
				rsyncRule := false
				for _, rule := range servicesRules {
					if r, ok := rule.(map[string]interface{}); ok {
						if ruleText, _ := r["rule"].(string); ruleText == "!has(self.rsync) || has(self.rsync.podTemplate.spec)" {
							rsyncRule = true
						}
					}
				}
				if !rsyncRule {
					t.Fatalf("rsync podTemplate presence CEL rule missing, got %#v", servicesRules)
				}
			}
			// The redirect field carries the PreciseHostname shape so its
			// value maps onto the generated RequestRedirect filter verbatim.
			redirect, _ := httpProperties["redirect"].(map[string]interface{})
			if redirect == nil {
				t.Fatalf("services.http.redirect schema missing, got %v", httpProperties)
			}
			if pattern, _ := redirect["pattern"].(string); pattern != `^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$` {
				t.Fatalf("redirect must carry the PreciseHostname pattern, got %v", redirect["pattern"])
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

// addBoundPublishPVC puts the publish PVC (the snapshot clone published under
// the given name) into the BOUND state the fake client never produces itself:
// spec.volumeName is what the real binder sets once the clone's PV exists,
// and it is what gates publish workload creation (a pod must not exist before
// the PV whose nodeAffinity places it does). An existing claim is bound in
// place; claimName defaults to mirror.Status.ActivePVC.
func addBoundPublishPVC(t *testing.T, ctx context.Context, c client.Client, mirror *mirrorv1alpha1.Mirror, claimName string) {
	t.Helper()
	if claimName == "" {
		claimName = mirror.Status.ActivePVC
	}
	existing := &corev1.PersistentVolumeClaim{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: mirror.Namespace, Name: claimName}, existing); err != nil {
		if !apierrors.IsNotFound(err) {
			t.Fatalf("get publish PVC: %v", err)
		}
		claim := &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{Namespace: mirror.Namespace, Name: claimName},
			Spec: corev1.PersistentVolumeClaimSpec{
				VolumeName: claimName + "-pv",
				Resources: corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{
					corev1.ResourceStorage: resource.MustParse("1Gi"),
				}},
			},
		}
		if err := c.Create(ctx, claim); err != nil {
			t.Fatalf("create bound publish PVC: %v", err)
		}
		return
	}
	if existing.Spec.VolumeName == "" {
		existing.Spec.VolumeName = claimName + "-pv"
		if err := c.Update(ctx, existing); err != nil {
			t.Fatalf("bind publish PVC: %v", err)
		}
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
	// Workload fields remain exactly as declared by the operator.
	if first.ImagePullPolicy != "" || first.SecurityContext != nil || podSpec.SecurityContext != nil {
		t.Fatalf("Falcon must not inject workload defaults: %#v", podSpec)
	}
	if v := findVolume(podSpec.Volumes, "tmp"); v != nil {
		t.Fatalf("Falcon must not inject a /tmp volume: %#v", v)
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

// TestDeleteDrainsWorkloadsBeforePVCsBeforeSnapshots pins the phase order of
// reconcileDelete: the sync Job and the publish Deployment go first, then the
// labeled PVCs, then the VolumeSnapshots, and only after everything is gone
// is the finalizer removed. Deleting the workloads explicitly is what breaks
// the production deadlock — owner-reference GC would remove them only after
// the CR deletion completes, but that deletion is blocked by the finalizer,
// which waits for the PVCs, which pvc-protection holds for the workloads'
// pods. The fake client simulates none of that (no pvc-protection, no GC):
// object existence alone drives the state machine, one reconcile per phase.
func TestDeleteDrainsWorkloadsBeforePVCsBeforeSnapshots(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 6, 9, 0, 0, 0, time.UTC)
	mirror := testMirror()
	deletedAt := metav1.NewTime(now)
	mirror.DeletionTimestamp = &deletedAt
	mirror.Finalizers = []string{MirrorFinalizer}

	// The children as production leaves them: owner-referenced to the deleting
	// Mirror and selected by the mirrors.zjusct.io/mirror label.
	owner := []metav1.OwnerReference{{
		APIVersion: mirrorv1alpha1.GroupVersion.String(), Kind: "Mirror",
		Name: mirror.Name, UID: mirror.UID, Controller: ptr.To(true),
	}}
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{
		Namespace: mirror.Namespace, Name: "smoke-sync-1756147200",
		Labels: childLabels(mirror, 1756147200, "sync"), OwnerReferences: owner,
	}}
	deployment := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
		Namespace: mirror.Namespace, Name: "smoke-publish-http",
		Labels: objectLabels(childBase(mirror.Name), publishRole(PublishProtocolHTTP)), OwnerReferences: owner,
	}}
	syncClaim := newDataClaim(mirror, "smoke-sync", 0, "sync")
	publishClaim := newDataClaim(mirror, "smoke-snap-1756147200", 1756147200, "publish-data")
	syncClaim.OwnerReferences, publishClaim.OwnerReferences = owner, owner
	snapshot := &snapshotv1.VolumeSnapshot{ObjectMeta: metav1.ObjectMeta{
		Namespace: mirror.Namespace, Name: "smoke-snap-1756147200",
		Labels: childLabels(mirror, 1756147200, "snapshot"), OwnerReferences: owner,
	}}

	scheme := testScheme(t)
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(mirror, job, deployment, syncClaim, publishClaim, snapshot).
		Build()
	deletingClient := &recordDeleteOptionsClient{Client: fakeClient, options: make(map[string]client.DeleteOptions)}
	reconciler := &MirrorReconciler{
		Client: deletingClient, Scheme: scheme, Recorder: record.NewFakeRecorder(20),
		Now: func() time.Time { return now }, Config: testConfig(), SyncLimiter: NewSyncLimiter(0),
	}
	request := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: mirror.Namespace, Name: mirror.Name}}
	reconcileWithResult := func() ctrl.Result {
		t.Helper()
		result, err := reconciler.Reconcile(ctx, request)
		if err != nil {
			t.Fatalf("reconcile: %v", err)
		}
		return result
	}

	// Phase 1: the workloads are deleted, storage must survive the pass.
	if result := reconcileWithResult(); result.RequeueAfter != 2*time.Second {
		t.Fatalf("the workload phase must requeue until drained, got %#v", result)
	}
	for _, name := range []string{job.Name, deployment.Name} {
		options, found := deletingClient.options[name]
		if !found || options.PropagationPolicy == nil || *options.PropagationPolicy != metav1.DeletePropagationForeground {
			t.Fatalf("workload %s must delete dependent Pods via foreground GC; got %#v", name, options)
		}
		if options.GracePeriodSeconds != nil {
			t.Fatalf("workload %s must preserve normal Pod termination grace", name)
		}
	}
	assertNotFound(t, ctx, fakeClient, client.ObjectKey{Namespace: mirror.Namespace, Name: job.Name}, &batchv1.Job{})
	assertNotFound(t, ctx, fakeClient, client.ObjectKey{Namespace: mirror.Namespace, Name: deployment.Name}, &appsv1.Deployment{})
	get(t, ctx, fakeClient, client.ObjectKey{Namespace: mirror.Namespace, Name: syncClaim.Name}, &corev1.PersistentVolumeClaim{})
	get(t, ctx, fakeClient, client.ObjectKey{Namespace: mirror.Namespace, Name: publishClaim.Name}, &corev1.PersistentVolumeClaim{})
	get(t, ctx, fakeClient, client.ObjectKey{Namespace: mirror.Namespace, Name: snapshot.Name}, &snapshotv1.VolumeSnapshot{})

	// Phase 2 (workloads gone): the sync PVC and the publish clone are
	// deleted, the snapshot — the clone's ZFS origin — survives them.
	if result := reconcileWithResult(); result.RequeueAfter != 2*time.Second {
		t.Fatalf("the PVC phase must requeue until drained, got %#v", result)
	}
	assertNotFound(t, ctx, fakeClient, client.ObjectKey{Namespace: mirror.Namespace, Name: syncClaim.Name}, &corev1.PersistentVolumeClaim{})
	assertNotFound(t, ctx, fakeClient, client.ObjectKey{Namespace: mirror.Namespace, Name: publishClaim.Name}, &corev1.PersistentVolumeClaim{})
	get(t, ctx, fakeClient, client.ObjectKey{Namespace: mirror.Namespace, Name: snapshot.Name}, &snapshotv1.VolumeSnapshot{})

	// Phase 3 (PVCs gone): the snapshot is deleted; the finalizer stays on
	// until a final pass observes a fully drained namespace.
	if result := reconcileWithResult(); result.RequeueAfter != 2*time.Second {
		t.Fatalf("the snapshot phase must requeue until drained, got %#v", result)
	}
	assertNotFound(t, ctx, fakeClient, client.ObjectKey{Namespace: mirror.Namespace, Name: snapshot.Name}, &snapshotv1.VolumeSnapshot{})
	if !controllerutil.ContainsFinalizer(getMirror(t, ctx, fakeClient, request.NamespacedName), MirrorFinalizer) {
		t.Fatal("the finalizer must survive until every phase has drained")
	}

	// Everything gone: the finalizer is removed, which lets the (fake) API
	// server complete the deletion of the Mirror itself.
	if result := reconcileWithResult(); result.RequeueAfter != 0 {
		t.Fatalf("a fully drained deletion must not requeue, got %#v", result)
	}
	assertNotFound(t, ctx, fakeClient, request.NamespacedName, &mirrorv1alpha1.Mirror{})
}

// The fake API does not implement garbage collection. Record the actual
// deletion request so the lifecycle test catches API-dependent orphan defaults.
type recordDeleteOptionsClient struct {
	client.Client
	options map[string]client.DeleteOptions
}

func (c *recordDeleteOptionsClient) Delete(ctx context.Context, object client.Object, options ...client.DeleteOption) error {
	applied := client.DeleteOptions{}
	for _, option := range options {
		option.ApplyToDelete(&applied)
	}
	c.options[object.GetName()] = applied
	return c.Client.Delete(ctx, object, options...)
}

func testSnapshotName(m *mirrorv1alpha1.Mirror) string {
	if m.Status.Publication != nil {
		return publicationSnapshotName(m)
	}
	return currentSyncSnapshotName(m)
}
