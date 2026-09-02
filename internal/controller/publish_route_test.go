package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	mirrorv1alpha1 "github.com/ZJUSCT/falcon/api/v1alpha1"
	"github.com/ZJUSCT/falcon/internal/config"
)

// testConfig returns a config with serving enabled and an unlimited sync cap.
func testConfig() *config.Config {
	cfg := config.Default()
	cfg.Site.URL = "https://mirrors.zjusct.io"
	cfg.Catalog.Enabled = true
	cfg.Serving.GatewayRef = config.GatewayRef{Name: "nginx-gateway", SectionName: "https"}
	cfg.Serving.Hostnames = []string{"mirrors.zjusct.io", "mirror.zju.edu.cn"}
	cfg.Serving.Labels = map[string]string{"serving.zone": "campus"}
	cfg.Serving.Annotations = map[string]string{"serving.example.com/note": "stamped"}
	if err := cfg.Validate(); err != nil {
		panic(err)
	}
	return cfg
}

// assertPublishRouteShape pins the generated HTTPRoute shape: owner ref,
// merged labels, stamped annotations, gateway parentRef, serving hostnames,
// and a single PathPrefix /<name> -> <base>-publish-http:80 rule.
func assertPublishRouteShape(t *testing.T, route *gatewayv1.HTTPRoute, owner client.Object, wantPath, wantService string) {
	t.Helper()
	if len(route.OwnerReferences) != 1 || route.OwnerReferences[0].UID != owner.GetUID() || !ptr.Deref(route.OwnerReferences[0].Controller, false) {
		t.Fatalf("route must be owned (controller=true) by the CR: %#v", route.OwnerReferences)
	}
	if route.Labels["serving.zone"] != "campus" || route.Labels[RoleLabel] != "publish" {
		t.Errorf("route labels must merge child + serving.labels: %v", route.Labels)
	}
	if route.Annotations["serving.example.com/note"] != "stamped" {
		t.Errorf("route annotations must carry serving.annotations: %v", route.Annotations)
	}
	if len(route.Spec.ParentRefs) != 1 {
		t.Fatalf("want exactly one parentRef, got %#v", route.Spec.ParentRefs)
	}
	parent := route.Spec.ParentRefs[0]
	if string(parent.Name) != "nginx-gateway" || string(ptr.Deref(parent.Kind, "")) != "Gateway" {
		t.Errorf("parentRef wrong: %#v", parent)
	}
	// gatewayRef.namespace is empty -> omitted (same namespace as the CR).
	if parent.Namespace != nil {
		t.Errorf("parentRef namespace must be omitted for a same-namespace gateway, got %q", *parent.Namespace)
	}
	if len(route.Spec.Hostnames) != 2 || route.Spec.Hostnames[0] != "mirrors.zjusct.io" || route.Spec.Hostnames[1] != "mirror.zju.edu.cn" {
		t.Errorf("hostnames wrong: %v", route.Spec.Hostnames)
	}
	if len(route.Spec.Rules) != 1 || len(route.Spec.Rules[0].Matches) < 1 {
		t.Fatalf("want one rule with at least the canonical match, got %#v", route.Spec.Rules)
	}
	match := route.Spec.Rules[0].Matches[0]
	if ptr.Deref(match.Path.Type, "") != gatewayv1.PathMatchPathPrefix || ptr.Deref(match.Path.Value, "") != wantPath {
		t.Errorf("path match wrong: %#v", match.Path)
	}
	backends := route.Spec.Rules[0].BackendRefs
	if len(backends) != 1 {
		t.Fatalf("want one backendRef, got %#v", backends)
	}
	backend := backends[0].BackendRef
	if string(backend.Name) != wantService || ptr.Deref(backend.Kind, "") != "Service" || ptr.Deref(backend.Port, 0) != 80 {
		t.Errorf("backendRef wrong: %#v", backend)
	}
}

// TestMirrorPausedKeepsPublishRoute: a paused Mirror with a published
// snapshot keeps being served — route and workload stay untouched.
func TestMirrorPausedKeepsPublishRoute(t *testing.T) {
	ctx := context.Background()
	mirror := testMirror()
	mirror.Spec.Paused = true
	mirror.Finalizers = []string{MirrorFinalizer}
	mirror.Status = mirrorv1alpha1.MirrorStatus{
		ObservedGeneration: mirror.Generation,
		Phase:              mirrorv1alpha1.PhaseReady,
		WorkPVC:            "smoke-sync",
		ActivePVC:          "smoke-snap-1756147200",
		ActiveSnapshot:     "smoke-snap-1756147200",
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

	reconcile(t, ctx, reconciler, request) // ensures publish workload + route
	markPublishDeploymentAvailable(t, ctx, fakeClient, mirror.Namespace)
	reconcile(t, ctx, reconciler, request) // settles into Paused

	current := getMirror(t, ctx, fakeClient, request.NamespacedName)
	if current.Status.Phase != mirrorv1alpha1.PhasePaused {
		t.Fatalf("expected Paused, got %s", current.Status.Phase)
	}
	route := &gatewayv1.HTTPRoute{}
	get(t, ctx, fakeClient, client.ObjectKey{Namespace: mirror.Namespace, Name: "smoke-publish"}, route)
	assertPublishRouteShape(t, route, mirror, "/smoke", "smoke-publish-http")
	deployment := &appsv1.Deployment{}
	get(t, ctx, fakeClient, client.ObjectKey{Namespace: mirror.Namespace, Name: "smoke-publish-http"}, deployment)
}

// TestServingDisabledSkipsRouteGeneration: with empty serving.hostnames no
// route is generated for a published Mirror (and nothing else breaks).
func TestServingDisabledSkipsRouteGeneration(t *testing.T) {
	ctx := context.Background()
	cfg := testConfig()
	cfg.Serving.Hostnames = nil
	cfg.Serving.GatewayRef = config.GatewayRef{}
	mirror := testMirror()
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
	addBoundSyncPVC(t, ctx, fakeClient, mirror, "", "s3.mirrors.zjusct.io", hostnameAffinity("s3.mirrors.zjusct.io"))
	reconciler := &MirrorReconciler{
		Client: fakeClient, Scheme: scheme, Recorder: record.NewFakeRecorder(20),
		Now:    func() time.Time { return time.Now().UTC() },
		Config: cfg, SyncLimiter: NewSyncLimiter(0),
	}
	request := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: mirror.Namespace, Name: mirror.Name}}

	reconcile(t, ctx, reconciler, request) // ensures the publish workload
	markPublishDeploymentAvailable(t, ctx, fakeClient, mirror.Namespace)
	reconcile(t, ctx, reconciler, request) // idle again

	current := getMirror(t, ctx, fakeClient, request.NamespacedName)
	if current.Status.Phase != mirrorv1alpha1.PhaseReady {
		t.Fatalf("expected Ready, got %s", current.Status.Phase)
	}
	assertNotFound(t, ctx, fakeClient, client.ObjectKey{Namespace: mirror.Namespace, Name: "smoke-publish"}, &gatewayv1.HTTPRoute{})
	// The publish workload itself is unaffected.
	get(t, ctx, fakeClient, client.ObjectKey{Namespace: mirror.Namespace, Name: "smoke-publish-http"}, &corev1.Service{})
}

// markPublishDeploymentAvailable flips the publish Deployment status to
// Available so the next reconcile proceeds past the rollout wait.
func markPublishDeploymentAvailable(t *testing.T, ctx context.Context, c client.Client, namespace string) {
	t.Helper()
	markDeploymentAvailable(t, ctx, c, namespace, "smoke-publish-http")
}

func markDeploymentAvailable(t *testing.T, ctx context.Context, c client.Client, namespace, name string) {
	t.Helper()
	deployment := &appsv1.Deployment{}
	get(t, ctx, c, client.ObjectKey{Namespace: namespace, Name: name}, deployment)
	replicas := int32(1)
	if deployment.Spec.Replicas != nil {
		replicas = *deployment.Spec.Replicas
	}
	deployment.Status.ObservedGeneration = deployment.Generation
	deployment.Status.AvailableReplicas = replicas
	deployment.Status.UpdatedReplicas = replicas
	if err := c.Status().Update(ctx, deployment); err != nil {
		t.Fatalf("mark Deployment available: %v", err)
	}
}

// TestMaxConcurrentQueuesSyncJob: with a cap of 1 already consumed by another
// mirror's Job, the second mirror's sync is queued (no Job object, SyncQueued
// condition); once the slot is freed the Job is created.
func TestMaxConcurrentQueuesSyncJob(t *testing.T) {
	ctx := context.Background()
	mirror := testMirror()
	mirror.Finalizers = []string{MirrorFinalizer}
	mirror.Annotations = map[string]string{SyncRequestAnnotation: "run-now"}
	mirror.Status = mirrorv1alpha1.MirrorStatus{
		ObservedGeneration: mirror.Generation,
		Phase:              mirrorv1alpha1.PhaseReady,
		WorkPVC:            "smoke-sync",
		ActivePVC:          "smoke-snap-1756147200",
		LastSync: &mirrorv1alpha1.MirrorSyncStatus{
			JobName: "smoke-sync-old", Phase: mirrorv1alpha1.SyncPhaseSucceeded,
		},
		LastHandledSyncRequest: "previous",
	}
	scheme := testScheme(t)
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&mirrorv1alpha1.Mirror{}, &batchv1.Job{}).
		WithObjects(mirror).
		Build()
	addBoundSyncPVC(t, ctx, fakeClient, mirror, "", "s3.mirrors.zjusct.io", hostnameAffinity("s3.mirrors.zjusct.io"))
	limiter := NewSyncLimiter(1)
	limiter.Acquire("other-mirror-sync-job", false) // the whole budget is busy
	reconciler := &MirrorReconciler{
		Client: fakeClient, Scheme: scheme, Recorder: record.NewFakeRecorder(20),
		Now:    func() time.Time { return time.Now().UTC() },
		Config: testConfig(), SyncLimiter: limiter,
	}
	request := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: mirror.Namespace, Name: mirror.Name}}

	reconcile(t, ctx, reconciler, request) // repair publish workload first
	markPublishDeploymentAvailable(t, ctx, fakeClient, mirror.Namespace)
	reconcile(t, ctx, reconciler, request) // startSync persists the pending Job name
	current := getMirror(t, ctx, fakeClient, request.NamespacedName)
	if current.Status.PendingJob == "" {
		t.Fatalf("expected a pending Job name, got %#v", current.Status)
	}
	jobName := current.Status.PendingJob

	reconcile(t, ctx, reconciler, request) // cap reached: queued, no Job object
	assertNotFound(t, ctx, fakeClient, client.ObjectKey{Namespace: mirror.Namespace, Name: jobName}, &batchv1.Job{})
	current = getMirror(t, ctx, fakeClient, request.NamespacedName)
	cond := findCondition(current.Status.Conditions, conditionProgressing)
	if cond == nil || cond.Reason != "SyncQueued" {
		t.Fatalf("expected SyncQueued condition, got %#v", current.Status.Conditions)
	}

	limiter.Release("other-mirror-sync-job") // a slot frees up
	reconcile(t, ctx, reconciler, request)   // now the Job is created
	job := &batchv1.Job{}
	get(t, ctx, fakeClient, client.ObjectKey{Namespace: mirror.Namespace, Name: jobName}, job)
	if limiter.Held() != 1 {
		t.Fatalf("limiter must hold exactly the new Job's slot, held=%d", limiter.Held())
	}

	// Job succeeds: the slot is released even though publication is still
	// pending (later reconciles must not re-consume it).
	job.Status.Succeeded = 1
	job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
	if err := fakeClient.Status().Update(ctx, job); err != nil {
		t.Fatalf("mark Job complete: %v", err)
	}
	reconcile(t, ctx, reconciler, request)
	if limiter.Held() != 0 {
		t.Fatalf("terminal Job must free its slot, held=%d", limiter.Held())
	}
	reconcile(t, ctx, reconciler, request) // publication continues without the slot
	if limiter.Held() != 0 {
		t.Fatalf("later reconciles must not re-acquire a terminal Job's slot, held=%d", limiter.Held())
	}
}

// TestMirrorMountPathUsedVerbatim: the data PVC is mounted read-only at
// exactly spec.services.http.mirrorMountPath — the controller no longer
// appends the mirror name (the user template owns the full path layout).
func TestMirrorMountPathUsedVerbatim(t *testing.T) {
	ctx := context.Background()
	mirror := testMirror()
	mirror.Spec.Services.HTTP.MirrorMountPath = "/srv/www/debian"
	mirror.Finalizers = []string{MirrorFinalizer}
	mirror.Status = mirrorv1alpha1.MirrorStatus{
		ObservedGeneration: mirror.Generation,
		Phase:              mirrorv1alpha1.PhaseReady,
		WorkPVC:            "smoke-sync",
		ActivePVC:          "smoke-sync-1756147200",
	}
	scheme := testScheme(t)
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&mirrorv1alpha1.Mirror{}, &appsv1.Deployment{}).
		WithObjects(mirror).
		Build()
	addBoundSyncPVC(t, ctx, fakeClient, mirror, "", "s3.mirrors.zjusct.io", hostnameAffinity("s3.mirrors.zjusct.io"))
	reconciler := &MirrorReconciler{
		Client: fakeClient, Scheme: scheme,
		Config: testConfig(), SyncLimiter: NewSyncLimiter(0),
	}
	request := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: mirror.Namespace, Name: mirror.Name}}
	reconcile(t, ctx, reconciler, request) // creates Service + publish Deployment

	deployment := &appsv1.Deployment{}
	get(t, ctx, fakeClient, client.ObjectKey{Namespace: mirror.Namespace, Name: "smoke-publish-http"}, deployment)
	mount := findMount(deployment.Spec.Template.Spec.Containers[0], "mirror-data")
	if mount == nil || mount.MountPath != "/srv/www/debian" {
		t.Fatalf("mirror-data mount = %#v, want read-only /srv/www/debian (verbatim mirrorMountPath)", mount)
	}
	if !mount.ReadOnly {
		t.Fatal("mirror-data mount must stay read-only")
	}
	volume := findVolume(deployment.Spec.Template.Spec.Volumes, "mirror-data")
	if volume == nil || volume.PersistentVolumeClaim == nil || !volume.PersistentVolumeClaim.ReadOnly {
		t.Fatalf("mirror-data volume source must be a read-only PVC reference, got %#v", volume)
	}

	// An enabled http service without mirrorMountPath is invalid: the mount
	// point is the one thing the user must declare (no default exists any
	// more).
	broken := mirror.DeepCopy()
	broken.Spec.Services.HTTP.MirrorMountPath = ""
	if errs := validateMirror(broken); len(errs) == 0 || !strings.Contains(errs.ToAggregate().Error(), "mirrorMountPath") {
		t.Fatalf("enabled service without mirrorMountPath must be InvalidSpec, got %v", errs)
	}
	// ... and a relative path is rejected as well.
	broken = mirror.DeepCopy()
	broken.Spec.Services.HTTP.MirrorMountPath = "srv/relative"
	if errs := validateMirror(broken); len(errs) == 0 || !strings.Contains(errs.ToAggregate().Error(), "absolute path") {
		t.Fatalf("relative mirrorMountPath must be InvalidSpec, got %v", errs)
	}
}

// TestServicesRenderPerKey: every ENABLED fixed key gets its own Deployment
// and Service named <base>-publish-<key> with per-service pod labels, but only
// the "http" key gets the publish HTTPRoute. The first container port is
// renamed to the key and targeted by the Service; the rsync Service carries no
// appProtocol.
func TestServicesRenderPerKey(t *testing.T) {
	ctx := context.Background()
	mirror := testMirror()
	mirror.Spec.Services.Rsync = mirrorv1alpha1.MirrorServiceSpec{
		Enable:          true,
		Replicas:        ptr.To(int32(2)),
		MirrorMountPath: "/export/mirror/smoke",
		PodTemplate: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
			Containers: []corev1.Container{{
				Name:  "rsyncd",
				Image: "docker.io/library/busybox:1.37.0",
				Ports: []corev1.ContainerPort{
					{Name: "ignored", ContainerPort: 8730, Protocol: corev1.ProtocolTCP},
					{Name: "metrics", ContainerPort: 9100, Protocol: corev1.ProtocolTCP},
				},
			}},
		}},
	}
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
	addBoundSyncPVC(t, ctx, fakeClient, mirror, "", "s3.mirrors.zjusct.io", hostnameAffinity("s3.mirrors.zjusct.io"))
	reconciler := &MirrorReconciler{
		Client: fakeClient, Scheme: scheme, Recorder: record.NewFakeRecorder(20),
		Now:    func() time.Time { return time.Now().UTC() },
		Config: testConfig(), SyncLimiter: NewSyncLimiter(0),
	}
	request := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: mirror.Namespace, Name: mirror.Name}}

	reconcile(t, ctx, reconciler, request) // creates both Service/Deployment pairs and the http route

	// Both deployments exist and carry per-service labels/ports/mounts.
	httpDeployment := &appsv1.Deployment{}
	get(t, ctx, fakeClient, client.ObjectKey{Namespace: mirror.Namespace, Name: "smoke-publish-http"}, httpDeployment)
	rsyncDeployment := &appsv1.Deployment{}
	get(t, ctx, fakeClient, client.ObjectKey{Namespace: mirror.Namespace, Name: "smoke-publish-rsync"}, rsyncDeployment)
	if got := *rsyncDeployment.Spec.Replicas; got != 2 {
		t.Fatalf("rsync replicas = %d, want 2", got)
	}
	if got := rsyncDeployment.Spec.Selector.MatchLabels[RoleLabel]; got != "publish-rsync" {
		t.Fatalf("rsync selector role = %q, want publish-rsync (per-service pods)", got)
	}
	rsyncContainer := rsyncDeployment.Spec.Template.Spec.Containers[0]
	rsyncPorts := rsyncContainer.Ports
	if len(rsyncPorts) != 2 || rsyncPorts[0].Name != "rsync" || rsyncPorts[0].ContainerPort != 8730 || rsyncPorts[1].Name != "metrics" {
		t.Fatalf("rsync container ports = %#v; want the first port renamed to rsync and the second kept", rsyncPorts)
	}
	rsyncMount := findMount(rsyncContainer, "mirror-data")
	if rsyncMount == nil || rsyncMount.MountPath != "/export/mirror/smoke" || !rsyncMount.ReadOnly {
		t.Fatalf("rsync data mount = %#v; want read-only /export/mirror/smoke (same data PVC for every service key)", rsyncMount)
	}

	// Both services exist; only the http one is routed and carries appProtocol.
	httpService := &corev1.Service{}
	get(t, ctx, fakeClient, client.ObjectKey{Namespace: mirror.Namespace, Name: "smoke-publish-http"}, httpService)
	if httpService.Spec.Ports[0].AppProtocol == nil || *httpService.Spec.Ports[0].AppProtocol != "http" {
		t.Fatalf("http Service appProtocol = %v, want http", httpService.Spec.Ports[0].AppProtocol)
	}
	rsyncService := &corev1.Service{}
	get(t, ctx, fakeClient, client.ObjectKey{Namespace: mirror.Namespace, Name: "smoke-publish-rsync"}, rsyncService)
	if rsyncService.Spec.Ports[0].AppProtocol != nil {
		t.Fatalf("rsync Service appProtocol = %v, want unset", rsyncService.Spec.Ports[0].AppProtocol)
	}
	if rsyncService.Spec.Ports[0].TargetPort.Type != intstr.String || rsyncService.Spec.Ports[0].TargetPort.StrVal != "rsync" {
		t.Fatalf("rsync Service targetPort = %#v, want named port rsync", rsyncService.Spec.Ports[0].TargetPort)
	}

	// The single route (created with the workload) targets the http service.
	route := &gatewayv1.HTTPRoute{}
	get(t, ctx, fakeClient, client.ObjectKey{Namespace: mirror.Namespace, Name: "smoke-publish"}, route)
	assertPublishRouteShape(t, route, mirror, "/smoke", "smoke-publish-http")

	// Readiness requires every enabled key: flip both, the Mirror settles Ready.
	markDeploymentAvailable(t, ctx, fakeClient, mirror.Namespace, "smoke-publish-http")
	markDeploymentAvailable(t, ctx, fakeClient, mirror.Namespace, "smoke-publish-rsync")
	reconcile(t, ctx, reconciler, request)
	current := getMirror(t, ctx, fakeClient, request.NamespacedName)
	if current.Status.Phase != mirrorv1alpha1.PhaseReady {
		t.Fatalf("expected Ready once every service rolled out, got %s (%#v)", current.Status.Phase, current.Status.Conditions)
	}
}

// TestRsyncOnlyMirrorGetsNoRoute: a mirror publishing only rsync gets a
// Deployment and a ClusterIP Service but no HTTPRoute at all (no TCPRoute
// either).
func TestRsyncOnlyMirrorGetsNoRoute(t *testing.T) {
	ctx := context.Background()
	mirror := testMirror()
	mirror.Spec.Services.HTTP = mirrorv1alpha1.MirrorHTTPServiceSpec{}
	mirror.Spec.Services.Rsync = mirrorv1alpha1.MirrorServiceSpec{
		Enable:          true,
		MirrorMountPath: "/export/mirror/smoke",
		PodTemplate: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
			Containers: []corev1.Container{{
				Name:  "rsyncd",
				Image: "docker.io/library/busybox:1.37.0",
				Ports: []corev1.ContainerPort{{Name: "rsync", ContainerPort: 8730, Protocol: corev1.ProtocolTCP}},
			}},
		}},
	}
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
	addBoundSyncPVC(t, ctx, fakeClient, mirror, "", "s3.mirrors.zjusct.io", hostnameAffinity("s3.mirrors.zjusct.io"))
	reconciler := &MirrorReconciler{
		Client: fakeClient, Scheme: scheme, Recorder: record.NewFakeRecorder(20),
		Config: testConfig(), SyncLimiter: NewSyncLimiter(0),
	}
	request := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: mirror.Namespace, Name: mirror.Name}}
	reconcile(t, ctx, reconciler, request)

	get(t, ctx, fakeClient, client.ObjectKey{Namespace: mirror.Namespace, Name: "smoke-publish-rsync"}, &appsv1.Deployment{})
	get(t, ctx, fakeClient, client.ObjectKey{Namespace: mirror.Namespace, Name: "smoke-publish-rsync"}, &corev1.Service{})
	assertNotFound(t, ctx, fakeClient, client.ObjectKey{Namespace: mirror.Namespace, Name: "smoke-publish"}, &gatewayv1.HTTPRoute{})
}

// TestAbsentOrDisabledServicesCreateNoWorkload: anything that is not an
// enabled service — an entirely absent services object, a key declared with
// enable: false, or a Mirror that has never published a snapshot — creates no
// publish children. All three shapes flow through the same enabled-keys
// filter, so they are pinned in one place.
func TestAbsentOrDisabledServicesCreateNoWorkload(t *testing.T) {
	ctx := context.Background()

	// An entirely absent services object: the sync/snapshot pipeline still
	// runs (the Mirror settles Ready), but no publish workload or route is
	// created.
	mirror := testMirror()
	mirror.Spec.Services = mirrorv1alpha1.MirrorServicesSpec{}
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
		WithStatusSubresource(&mirrorv1alpha1.Mirror{}).
		WithObjects(mirror).
		Build()
	addBoundSyncPVC(t, ctx, fakeClient, mirror, "", "s3.mirrors.zjusct.io", hostnameAffinity("s3.mirrors.zjusct.io"))
	reconciler := &MirrorReconciler{
		Client: fakeClient, Scheme: scheme, Recorder: record.NewFakeRecorder(20),
		Now:    func() time.Time { return time.Now().UTC() },
		Config: testConfig(), SyncLimiter: NewSyncLimiter(0),
	}
	request := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: mirror.Namespace, Name: mirror.Name}}
	reconcile(t, ctx, reconciler, request) // idle path: nothing to publish

	current := getMirror(t, ctx, fakeClient, request.NamespacedName)
	if current.Status.Phase != mirrorv1alpha1.PhaseReady {
		t.Fatalf("expected Ready, got %s", current.Status.Phase)
	}
	assertNotFound(t, ctx, fakeClient, client.ObjectKey{Namespace: mirror.Namespace, Name: "smoke-publish-http"}, &corev1.Service{})
	assertNotFound(t, ctx, fakeClient, client.ObjectKey{Namespace: mirror.Namespace, Name: "smoke-publish-http"}, &appsv1.Deployment{})
	assertNotFound(t, ctx, fakeClient, client.ObjectKey{Namespace: mirror.Namespace, Name: "smoke-publish"}, &gatewayv1.HTTPRoute{})

	// A key declared with enable: false is as good as absent — no workload
	// for it, while the other enabled key still publishes.
	mirror = testMirror()
	mirror.Spec.Services.Rsync = mirrorv1alpha1.MirrorServiceSpec{
		Enable:          false,
		MirrorMountPath: "/export/mirror/smoke",
		PodTemplate: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
			Containers: []corev1.Container{{
				Name:  "rsyncd",
				Image: "docker.io/library/busybox:1.37.0",
				Ports: []corev1.ContainerPort{{Name: "rsync", ContainerPort: 8730, Protocol: corev1.ProtocolTCP}},
			}},
		}},
	}
	mirror.Finalizers = []string{MirrorFinalizer}
	mirror.Status = mirrorv1alpha1.MirrorStatus{
		ObservedGeneration: mirror.Generation,
		Phase:              mirrorv1alpha1.PhaseReady,
		WorkPVC:            "smoke-sync",
		ActivePVC:          "smoke-snap-1756147200",
	}
	scheme = testScheme(t)
	fakeClient = fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&mirrorv1alpha1.Mirror{}, &appsv1.Deployment{}).
		WithObjects(mirror).
		Build()
	addBoundSyncPVC(t, ctx, fakeClient, mirror, "", "s3.mirrors.zjusct.io", hostnameAffinity("s3.mirrors.zjusct.io"))
	reconciler = &MirrorReconciler{
		Client: fakeClient, Scheme: scheme, Recorder: record.NewFakeRecorder(20),
		Config: testConfig(), SyncLimiter: NewSyncLimiter(0),
	}
	reconcile(t, ctx, reconciler, request)

	// The enabled http key publishes; the disabled rsync key does not.
	get(t, ctx, fakeClient, client.ObjectKey{Namespace: mirror.Namespace, Name: "smoke-publish-http"}, &appsv1.Deployment{})
	assertNotFound(t, ctx, fakeClient, client.ObjectKey{Namespace: mirror.Namespace, Name: "smoke-publish-rsync"}, &appsv1.Deployment{})
	assertNotFound(t, ctx, fakeClient, client.ObjectKey{Namespace: mirror.Namespace, Name: "smoke-publish-rsync"}, &corev1.Service{})

	// A Mirror that never published anything gets no HTTPRoute either.
	mirror = testMirror()
	scheme = testScheme(t)
	fakeClient = fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&mirrorv1alpha1.Mirror{}).
		WithObjects(mirror).
		Build()
	reconciler = &MirrorReconciler{
		Client: fakeClient, Scheme: scheme, Recorder: record.NewFakeRecorder(20),
		Config: testConfig(), SyncLimiter: NewSyncLimiter(0),
	}
	reconcile(t, ctx, reconciler, request) // finalizer
	reconcile(t, ctx, reconciler, request) // start first sync (initializing)

	assertNotFound(t, ctx, fakeClient, client.ObjectKey{Namespace: mirror.Namespace, Name: "smoke-publish"}, &gatewayv1.HTTPRoute{})
}

// TestPublishDefaultsInjectedWhereSilent pins the default injections into a
// bare user pod template: TCP readiness probe on the renamed first port, /tmp
// emptyDir, readOnlyRootFilesystem, allowPrivilegeEscalation false, drop ALL,
// runAsNonRoot, seccomp RuntimeDefault, automountServiceAccountToken false.
func TestPublishDefaultsInjectedWhereSilent(t *testing.T) {
	ctx := context.Background()
	mirror := testMirror()
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
	addBoundSyncPVC(t, ctx, fakeClient, mirror, "", "s3.mirrors.zjusct.io", hostnameAffinity("s3.mirrors.zjusct.io"))
	reconciler := &MirrorReconciler{
		Client: fakeClient, Scheme: scheme,
		Config: testConfig(), SyncLimiter: NewSyncLimiter(0),
	}
	request := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: mirror.Namespace, Name: mirror.Name}}
	reconcile(t, ctx, reconciler, request)

	deployment := &appsv1.Deployment{}
	get(t, ctx, fakeClient, client.ObjectKey{Namespace: mirror.Namespace, Name: "smoke-publish-http"}, deployment)
	podSpec := deployment.Spec.Template.Spec
	if ptr.Deref(podSpec.AutomountServiceAccountToken, true) {
		t.Fatal("automountServiceAccountToken must default to false")
	}
	if !ptr.Deref(podSpec.SecurityContext.RunAsNonRoot, false) {
		t.Fatal("runAsNonRoot must default to true")
	}
	if podSpec.SecurityContext.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Fatalf("seccomp must default to RuntimeDefault, got %v", podSpec.SecurityContext.SeccompProfile.Type)
	}
	first := podSpec.Containers[0]
	if !ptr.Deref(first.SecurityContext.ReadOnlyRootFilesystem, false) {
		t.Fatal("readOnlyRootFilesystem must default to true")
	}
	if ptr.Deref(first.SecurityContext.AllowPrivilegeEscalation, true) {
		t.Fatal("allowPrivilegeEscalation must default to false")
	}
	if len(first.SecurityContext.Capabilities.Drop) != 1 || first.SecurityContext.Capabilities.Drop[0] != "ALL" {
		t.Fatalf("capabilities must default to drop ALL, got %#v", first.SecurityContext.Capabilities)
	}
	if first.ReadinessProbe == nil || first.ReadinessProbe.TCPSocket == nil {
		t.Fatalf("a silent template must get the TCP readiness probe, got %#v", first.ReadinessProbe)
	}
	if got := first.ReadinessProbe.TCPSocket.Port.StrVal; got != "http" {
		t.Fatalf("TCP readiness probe port = %q, want the renamed service-key port http", got)
	}
	if first.LivenessProbe != nil {
		t.Fatalf("no liveness probe may be injected, got %#v", first.LivenessProbe)
	}
	if v := findVolume(podSpec.Volumes, "tmp"); v == nil || v.EmptyDir == nil {
		t.Fatalf("a silent template must get the /tmp emptyDir volume, got %#v", v)
	}
	if m := findMount(first, "tmp"); m == nil || m.MountPath != "/tmp" {
		t.Fatalf("a silent template must get the /tmp mount, got %#v", m)
	}
}

// TestPublishUserSettingsWin: the defaults only fill silence — an explicit
// readiness probe, an explicit readOnlyRootFilesystem=false, a user-provided
// /tmp volume and extra template labels/annotations are preserved.
func TestPublishUserSettingsWin(t *testing.T) {
	ctx := context.Background()
	mirror := testMirror()
	mirror.Spec.Services.HTTP.MirrorMountPath = "/srv/mirror/smoke"
	mirror.Spec.Services.HTTP.PodTemplate = corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{
			Labels:      map[string]string{"pool": "edge"},
			Annotations: map[string]string{"example.com/owner": "webteam"},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{
				Name:  "web",
				Image: "nginxinc/nginx-unprivileged:1.31.0-alpine",
				Ports: []corev1.ContainerPort{{Name: "web", ContainerPort: 8080, Protocol: corev1.ProtocolTCP}},
				ReadinessProbe: &corev1.Probe{
					ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/healthz", Port: intstr.FromInt32(8080)}},
				},
				SecurityContext: &corev1.SecurityContext{ReadOnlyRootFilesystem: ptr.To(false)},
				VolumeMounts:    []corev1.VolumeMount{{Name: "scratch", MountPath: "/tmp"}},
			}},
			Volumes: []corev1.Volume{{
				Name:         "scratch",
				VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
			}},
		},
	}
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
	addBoundSyncPVC(t, ctx, fakeClient, mirror, "", "s3.mirrors.zjusct.io", hostnameAffinity("s3.mirrors.zjusct.io"))
	reconciler := &MirrorReconciler{
		Client: fakeClient, Scheme: scheme,
		Config: testConfig(), SyncLimiter: NewSyncLimiter(0),
	}
	request := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: mirror.Namespace, Name: mirror.Name}}
	reconcile(t, ctx, reconciler, request)

	deployment := &appsv1.Deployment{}
	get(t, ctx, fakeClient, client.ObjectKey{Namespace: mirror.Namespace, Name: "smoke-publish-http"}, deployment)
	podSpec := deployment.Spec.Template.Spec
	first := podSpec.Containers[0]
	if first.ReadinessProbe == nil || first.ReadinessProbe.HTTPGet == nil || first.ReadinessProbe.HTTPGet.Path != "/healthz" {
		t.Fatalf("the user readiness probe must win, got %#v", first.ReadinessProbe)
	}
	if ptr.Deref(first.SecurityContext.ReadOnlyRootFilesystem, true) {
		t.Fatal("an explicit readOnlyRootFilesystem=false must be respected")
	}
	if findVolume(podSpec.Volumes, "tmp") != nil {
		t.Fatal("no tmp volume may be injected when the template provides its own /tmp mount")
	}
	// User metadata merges with the forced identity labels.
	if got := deployment.Spec.Template.Labels["pool"]; got != "edge" {
		t.Fatalf("user pod label lost: %#v", deployment.Spec.Template.Labels)
	}
	if deployment.Spec.Template.Labels[RoleLabel] != "publish-http" {
		t.Fatalf("forced role label must win: %#v", deployment.Spec.Template.Labels)
	}
	if got := deployment.Spec.Template.Annotations["example.com/owner"]; got != "webteam" {
		t.Fatalf("user pod annotation lost: %#v", deployment.Spec.Template.Annotations)
	}
	if got := deployment.Spec.Template.Annotations[ActivePVCAnnotation]; got != "smoke-snap-1756147200" {
		t.Fatalf("forced active-pvc annotation missing: %#v", deployment.Spec.Template.Annotations)
	}
}

// TestMirrorDataMountsAreForcedReadOnly: an extra user mount of the injected
// mirror-data volume is allowed only read-only; a writable one is InvalidSpec,
// and a user volume named mirror-data is rejected as reserved.
func TestMirrorDataMountsAreForcedReadOnly(t *testing.T) {
	mirror := testMirror()
	mirror.Spec.Services.HTTP.MirrorMountPath = "/srv/mirror/smoke"
	mirror.Spec.Services.HTTP.PodTemplate.Spec.Containers = append(mirror.Spec.Services.HTTP.PodTemplate.Spec.Containers, corev1.Container{
		Name:         "sidecar",
		Image:        "docker.io/library/busybox:1.37.0",
		VolumeMounts: []corev1.VolumeMount{{Name: "mirror-data", MountPath: "/data", ReadOnly: false}},
	})

	errs := validateMirror(mirror)
	if len(errs) == 0 || !strings.Contains(errs.ToAggregate().Error(), "mirror-data") {
		t.Fatalf("a writable mirror-data mount must be InvalidSpec, got %v", errs)
	}

	// Read-only extra mounts are fine.
	mirror = testMirror()
	mirror.Spec.Services.HTTP.MirrorMountPath = "/srv/mirror/smoke"
	mirror.Spec.Services.HTTP.PodTemplate.Spec.Containers[0].VolumeMounts = []corev1.VolumeMount{
		{Name: "mirror-data", MountPath: "/srv/mirror/smoke/index", ReadOnly: true},
	}
	if errs := validateMirror(mirror); len(errs) != 0 {
		t.Fatalf("a read-only extra mirror-data mount must be accepted, got %v", errs)
	}

	// A user-declared volume named mirror-data collides with the injection.
	mirror = testMirror()
	mirror.Spec.Services.HTTP.MirrorMountPath = "/srv/mirror/smoke"
	mirror.Spec.Services.HTTP.PodTemplate.Spec.Volumes = []corev1.Volume{{
		Name:         "mirror-data",
		VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
	}}
	errs = validateMirror(mirror)
	if len(errs) == 0 || !strings.Contains(errs.ToAggregate().Error(), "reserved") {
		t.Fatalf("a user volume named mirror-data must be rejected as reserved, got %v", errs)
	}
}

// TestPublishTemplateWithoutPortsOrContainersIsInvalid: the Service targets
// the first container port of the first container, so both must exist on an
// enabled service.
func TestPublishTemplateWithoutPortsOrContainersIsInvalid(t *testing.T) {
	mirror := testMirror()

	// No ports on the first container.
	broken := mirror.DeepCopy()
	broken.Spec.Services.HTTP.PodTemplate.Spec.Containers[0].Ports = nil
	errs := validateMirror(broken)
	if len(errs) == 0 || !strings.Contains(errs.ToAggregate().Error(), "ports") {
		t.Fatalf("an enabled service without container ports must be InvalidSpec, got %v", errs)
	}

	// No containers at all.
	broken = mirror.DeepCopy()
	broken.Spec.Services.HTTP.PodTemplate.Spec.Containers = nil
	errs = validateMirror(broken)
	if len(errs) == 0 || !strings.Contains(errs.ToAggregate().Error(), "containers") {
		t.Fatalf("an enabled service without containers must be InvalidSpec, got %v", errs)
	}

	// A DISABLED key may park a template that would not validate.
	disabled := mirror.DeepCopy()
	disabled.Spec.Services.Rsync = mirrorv1alpha1.MirrorServiceSpec{
		Enable: false,
		PodTemplate: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "x", Image: "img"}},
		}},
	}
	if errs := validateMirror(disabled); len(errs) != 0 {
		t.Fatalf("a disabled key must not be validated, got %v", errs)
	}
}

// publishedMirrorFixture returns a published, Ready Mirror whose http service
// carries the given aliases, plus a reconciler wired to a fake client holding it.
func publishedMirrorFixture(t *testing.T, name string, aliases ...mirrorv1alpha1.MirrorHTTPAlias) (*mirrorv1alpha1.Mirror, *MirrorReconciler, client.Client, ctrl.Request, context.Context) {
	ctx := context.Background()
	mirror := testMirror()
	mirror.ObjectMeta = metav1.ObjectMeta{
		Namespace:  "mirrors",
		Name:       name,
		UID:        types.UID("uid-" + name),
		Generation: 1,
	}
	base, _ := childBase(name)
	mirror.Spec.Services.HTTP.MirrorServiceSpec = mirrorv1alpha1.MirrorServiceSpec{
		Enable:          true,
		MirrorMountPath: "/srv/mirror/" + base,
		PodTemplate: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
			Containers: []corev1.Container{{
				Name:  "web",
				Image: "nginxinc/nginx-unprivileged:1.31.0-alpine",
				Ports: []corev1.ContainerPort{{Name: "web", ContainerPort: 8080, Protocol: corev1.ProtocolTCP}},
			}},
		}},
	}
	mirror.Spec.Services.HTTP.Aliases = aliases
	mirror.Finalizers = []string{MirrorFinalizer}
	mirror.Status = mirrorv1alpha1.MirrorStatus{
		ObservedGeneration: mirror.Generation,
		Phase:              mirrorv1alpha1.PhaseReady,
		WorkPVC:            base + "-sync",
		ActivePVC:          base + "-snap-1756147200",
	}
	scheme := testScheme(t)
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&mirrorv1alpha1.Mirror{}, &appsv1.Deployment{}).
		WithObjects(mirror).
		Build()
	addBoundSyncPVC(t, ctx, fakeClient, mirror, base+"-sync", "s3.mirrors.zjusct.io", hostnameAffinity("s3.mirrors.zjusct.io"))
	reconciler := &MirrorReconciler{
		Client: fakeClient, Scheme: scheme, Recorder: record.NewFakeRecorder(20),
		Now:    func() time.Time { return time.Now().UTC() },
		Config: testConfig(), SyncLimiter: NewSyncLimiter(0),
	}
	return mirror, reconciler, fakeClient, ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: mirror.Namespace, Name: mirror.Name},
	}, ctx
}

// TestAliasesServeMultiMatchRoute: aliases append PathPrefix matches to the
// single rule after the canonical path (OR semantics, same backend).
func TestAliasesServeMultiMatchRoute(t *testing.T) {
	mirror, reconciler, fakeClient, request, ctx := publishedMirrorFixture(t, "smoke", "/linux.git", "/git/linux.git")
	reconcile(t, ctx, reconciler, request) // creates the workload (+ route attempt)
	markDeploymentAvailable(t, ctx, fakeClient, mirror.Namespace, "smoke-publish-http")
	reconcile(t, ctx, reconciler, request)

	route := &gatewayv1.HTTPRoute{}
	get(t, ctx, fakeClient, client.ObjectKey{Namespace: mirror.Namespace, Name: "smoke-publish"}, route)
	if len(route.Spec.Rules) != 1 {
		t.Fatalf("want exactly one rule, got %#v", route.Spec.Rules)
	}
	matches := route.Spec.Rules[0].Matches
	if len(matches) != 3 {
		t.Fatalf("want canonical + 2 alias matches, got %#v", matches)
	}
	wantPaths := []string{"/smoke", "/linux.git", "/git/linux.git"}
	for i, want := range wantPaths {
		if ptr.Deref(matches[i].Path.Type, "") != gatewayv1.PathMatchPathPrefix || ptr.Deref(matches[i].Path.Value, "") != want {
			t.Fatalf("match %d = %#v, want PathPrefix %s", i, matches[i], want)
		}
	}
	// All matches share the single http backend.
	if backends := route.Spec.Rules[0].BackendRefs; len(backends) != 1 || string(backends[0].BackendRef.Name) != "smoke-publish-http" {
		t.Fatalf("aliases must share the canonical backend, got %#v", backends)
	}
	assertPublishRouteShape(t, route, mirror, "/smoke", "smoke-publish-http")
}

// TestAliasValidation: syntax rules and the canonical-path rule are enforced
// (mirroring the admission-time CEL); case-sensitive paths and uppercase are
// legal.
func TestAliasValidation(t *testing.T) {
	// Uppercase and multi-segment aliases are the point of the mechanism.
	valid := testMirror()
	valid.Spec.Services.HTTP.Aliases = []mirrorv1alpha1.MirrorHTTPAlias{
		"/Linux.Git", "/git/smoke.git", "/a/b/c",
	}
	if errs := validateMirror(valid); len(errs) != 0 {
		t.Fatalf("valid aliases rejected: %v", errs.ToAggregate())
	}

	cases := map[string]mirrorv1alpha1.MirrorHTTPAlias{
		"relative":       "linux.git",
		"trailing slash": "/linux.git/",
		"double slash":   "/git//linux.git",
		"whitespace":     "/git linux",
		"canonical path": "/smoke",
	}
	for name, alias := range cases {
		broken := testMirror()
		broken.Spec.Services.HTTP.Aliases = []mirrorv1alpha1.MirrorHTTPAlias{alias}
		if errs := validateMirror(broken); len(errs) == 0 {
			t.Fatalf("%s: alias %q must be InvalidSpec", name, alias)
		}
	}

	// A DISABLED http key may park invalid aliases.
	disabled := testMirror()
	disabled.Spec.Services.HTTP = mirrorv1alpha1.MirrorHTTPServiceSpec{}
	disabled.Spec.Services.HTTP.Aliases = []mirrorv1alpha1.MirrorHTTPAlias{"no-leading-slash", "/smoke"}
	if errs := validateMirror(disabled); len(errs) != 0 {
		t.Fatalf("aliases of a disabled http key must not be validated, got %v", errs.ToAggregate())
	}
}

// TestRouteConflictWithholdsRouteKeepsWorkload: when this Mirror's canonical
// path or an alias overlaps (equality or segment-boundary prefix) another
// Mirror's paths in the same namespace, the route is not created/updated and
// the Mirror degrades with reason RouteConflict — while the publish workload
// keeps running. Non-overlapping lookalike paths (/gitlinux vs /git) do not
// conflict.
func TestRouteConflictWithholdsRouteKeepsWorkload(t *testing.T) {
	// The "git" Mirror's canonical path /git prefix-overlaps smoke's alias
	// /git/linux.git (Gateway API PathPrefix is segment-aware).
	conflicting := testMirror()
	conflicting.Name = "git"
	conflicting.UID = types.UID("uid-git")
	conflicting.Spec.Services.HTTP.MirrorServiceSpec = mirrorv1alpha1.MirrorServiceSpec{
		Enable:          true,
		MirrorMountPath: "/srv/mirror/git",
		PodTemplate: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
			Containers: []corev1.Container{{
				Name: "web", Image: "nginxinc/nginx-unprivileged:1.31.0-alpine",
				Ports: []corev1.ContainerPort{{Name: "web", ContainerPort: 8080, Protocol: corev1.ProtocolTCP}},
			}},
		}},
	}
	conflicting.Finalizers = []string{MirrorFinalizer}
	conflicting.Status = mirrorv1alpha1.MirrorStatus{
		ObservedGeneration: conflicting.Generation,
		Phase:              mirrorv1alpha1.PhaseReady,
		WorkPVC:            "git-sync",
		ActivePVC:          "git-snap-1756147200",
	}

	mirror, reconciler, fakeClient, request, ctx := publishedMirrorFixture(t, "smoke", "/linux.git", "/git/linux.git")
	if err := fakeClient.Create(ctx, conflicting); err != nil {
		t.Fatalf("create conflicting mirror: %v", err)
	}
	reconcile(t, ctx, reconciler, request) // creates the workload, withholds the route
	markDeploymentAvailable(t, ctx, fakeClient, mirror.Namespace, "smoke-publish-http")
	reconcile(t, ctx, reconciler, request) // settles into RouteConflict

	// The route is withheld, the Mirror degrades with RouteConflict...
	assertNotFound(t, ctx, fakeClient, client.ObjectKey{Namespace: mirror.Namespace, Name: "smoke-publish"}, &gatewayv1.HTTPRoute{})
	current := getMirror(t, ctx, fakeClient, request.NamespacedName)
	if current.Status.Phase != mirrorv1alpha1.PhaseDegraded {
		t.Fatalf("expected Degraded on route conflict, got %s", current.Status.Phase)
	}
	cond := findCondition(current.Status.Conditions, conditionDegraded)
	if cond == nil || cond.Reason != "RouteConflict" || !strings.Contains(cond.Message, "/git/linux.git") || !strings.Contains(cond.Message, `"git"`) {
		t.Fatalf("expected RouteConflict condition naming the overlap, got %#v", cond)
	}
	// ...but the publish workload keeps running.
	get(t, ctx, fakeClient, client.ObjectKey{Namespace: mirror.Namespace, Name: "smoke-publish-http"}, &appsv1.Deployment{})
	get(t, ctx, fakeClient, client.ObjectKey{Namespace: mirror.Namespace, Name: "smoke-publish-http"}, &corev1.Service{})

	// A non-overlapping alias (/gitlinux does not segment-prefix /git) keeps
	// the route flowing.
	other, otherReconciler, otherClient, otherRequest, otherCtx := publishedMirrorFixture(t, "smoke", "/gitlinux")
	otherConflicting := conflicting.DeepCopy()
	otherConflicting.ResourceVersion = ""
	if err := otherClient.Create(otherCtx, otherConflicting); err != nil {
		t.Fatalf("create conflicting mirror: %v", err)
	}
	reconcile(t, otherCtx, otherReconciler, otherRequest)
	markDeploymentAvailable(t, otherCtx, otherClient, other.Namespace, "smoke-publish-http")
	reconcile(t, otherCtx, otherReconciler, otherRequest)
	get(t, otherCtx, otherClient, client.ObjectKey{Namespace: other.Namespace, Name: "smoke-publish"}, &gatewayv1.HTTPRoute{})
	current = getMirror(t, otherCtx, otherClient, otherRequest.NamespacedName)
	if current.Status.Phase != mirrorv1alpha1.PhaseReady {
		t.Fatalf("non-overlapping alias must not degrade, got %s (%#v)", current.Status.Phase, current.Status.Conditions)
	}
}
