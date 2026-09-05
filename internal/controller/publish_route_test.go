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

// testConfig returns a config with publishing enabled and an unlimited sync cap.
func testConfig() *config.Config {
	cfg := config.Default()
	cfg.Site.URL = "https://mirrors.zjusct.io"
	cfg.Catalog.Enabled = true
	cfg.Publish.GatewayRef = config.GatewayRef{Name: "nginx-gateway", SectionName: "https"}
	cfg.Publish.Hostnames = []string{"mirrors.zjusct.io", "mirror.zju.edu.cn"}
	cfg.Publish.Labels = map[string]string{"publish.zone": "campus"}
	cfg.Publish.Annotations = map[string]string{"publish.example.com/note": "stamped"}
	if err := cfg.Validate(); err != nil {
		panic(err)
	}
	return cfg
}

// assertPublishRouteShape pins the generated HTTPRoute shape: owner ref,
// merged labels, stamped annotations, gateway parentRef, publish hostnames,
// and a single PathPrefix /<name> -> <base>-publish-http:80 rule.
func assertPublishRouteShape(t *testing.T, route *gatewayv1.HTTPRoute, owner client.Object, wantPath, wantService string) {
	t.Helper()
	if len(route.OwnerReferences) != 1 || route.OwnerReferences[0].UID != owner.GetUID() || !ptr.Deref(route.OwnerReferences[0].Controller, false) {
		t.Fatalf("route must be owned (controller=true) by the CR: %#v", route.OwnerReferences)
	}
	if route.Labels["publish.zone"] != "campus" || route.Labels[ComponentLabel] != "publish-http" {
		t.Errorf("route labels must merge child + publish.labels: %v", route.Labels)
	}
	if route.Annotations["publish.example.com/note"] != "stamped" {
		t.Errorf("route annotations must carry publish.annotations: %v", route.Annotations)
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
	mirror.Spec.Sync.Paused = true
	mirror.Finalizers = []string{MirrorFinalizer}
	mirror.Status = mirrorv1alpha1.MirrorStatus{
		ObservedGeneration: mirror.Generation,
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

	route := &gatewayv1.HTTPRoute{}
	get(t, ctx, fakeClient, client.ObjectKey{Namespace: mirror.Namespace, Name: "smoke-publish"}, route)
	assertPublishRouteShape(t, route, mirror, "/smoke", "smoke-publish-http")
	deployment := &appsv1.Deployment{}
	get(t, ctx, fakeClient, client.ObjectKey{Namespace: mirror.Namespace, Name: "smoke-publish-http"}, deployment)
}

// TestPublishDisabledSkipsRouteGeneration: with empty publish.hostnames no
// route is generated for a published Mirror (and nothing else breaks).
func TestPublishDisabledSkipsRouteGeneration(t *testing.T) {
	ctx := context.Background()
	cfg := testConfig()
	cfg.Publish.Hostnames = nil
	cfg.Publish.GatewayRef = config.GatewayRef{}
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
	if degraded := findCondition(current.Status.Conditions, conditionDegraded); degraded == nil || degraded.Status != metav1.ConditionTrue {
		t.Fatalf("disabled route generation must report Degraded=True, got %#v", current.Status.Conditions)
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
	reconcile(t, ctx, reconciler, request) // startSync persists the transaction identity
	current := getMirror(t, ctx, fakeClient, request.NamespacedName)
	if currentSyncJobName(current) == "" {
		t.Fatalf("expected a current Job name, got %#v", current.Status)
	}
	jobName := currentSyncJobName(current)

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

// TestMirrorDataVolumeInjectedVolumeOnly: the publish PVC is injected as the
// reserved mirror-data VOLUME with a read-only volume source; the controller
// adds no mounts — mounting it, and where, is the user template's own
// declaration (a user-declared mount is preserved verbatim).
func TestMirrorDataVolumeInjectedVolumeOnly(t *testing.T) {
	ctx := context.Background()
	mirror := testMirror()
	// The user template mounts the injected mirror-data volume itself.
	mirror.Spec.Publish.HTTP.PodTemplate.Spec.Containers[0].VolumeMounts = []corev1.VolumeMount{
		{Name: "mirror-data", MountPath: "/srv/www/debian", ReadOnly: true},
	}
	mirror.Finalizers = []string{MirrorFinalizer}
	mirror.Status = mirrorv1alpha1.MirrorStatus{
		ObservedGeneration: mirror.Generation,
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
	volume := findVolume(deployment.Spec.Template.Spec.Volumes, "mirror-data")
	if volume == nil || volume.PersistentVolumeClaim == nil || !volume.PersistentVolumeClaim.ReadOnly {
		t.Fatalf("mirror-data volume source must be a read-only PVC reference, got %#v", volume)
	}
	// The user-declared mount is preserved verbatim; the controller appended
	// nothing except the default /tmp emptyDir mount.
	mount := findMount(deployment.Spec.Template.Spec.Containers[0], "mirror-data")
	if mount == nil || mount.MountPath != "/srv/www/debian" || !mount.ReadOnly {
		t.Fatalf("user mirror-data mount = %#v, want preserved read-only /srv/www/debian", mount)
	}
	mounts := deployment.Spec.Template.Spec.Containers[0].VolumeMounts
	if len(mounts) != 2 || mounts[0].MountPath != "/srv/www/debian" || mounts[1].MountPath != "/tmp" {
		t.Fatalf("the controller must append only the default /tmp mount, got %#v", mounts)
	}

	// A template silent about mirror-data gets the volume anyway — and no
	// mount at all.
	bare := testMirror()
	bare.Finalizers = []string{MirrorFinalizer}
	bare.Status = mirrorv1alpha1.MirrorStatus{
		ObservedGeneration: bare.Generation,
		WorkPVC:            "smoke-sync",
		ActivePVC:          "smoke-sync-1756147200",
	}
	scheme = testScheme(t)
	bareClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&mirrorv1alpha1.Mirror{}, &appsv1.Deployment{}).
		WithObjects(bare).
		Build()
	addBoundSyncPVC(t, ctx, bareClient, bare, "", "s3.mirrors.zjusct.io", hostnameAffinity("s3.mirrors.zjusct.io"))
	bareReconciler := &MirrorReconciler{
		Client: bareClient, Scheme: scheme,
		Config: testConfig(), SyncLimiter: NewSyncLimiter(0),
	}
	reconcile(t, ctx, bareReconciler, request)
	bareDeployment := &appsv1.Deployment{}
	get(t, ctx, bareClient, client.ObjectKey{Namespace: bare.Namespace, Name: "smoke-publish-http"}, bareDeployment)
	if v := findVolume(bareDeployment.Spec.Template.Spec.Volumes, "mirror-data"); v == nil || v.PersistentVolumeClaim == nil || !v.PersistentVolumeClaim.ReadOnly {
		t.Fatalf("a silent template must still get the read-only mirror-data volume, got %#v", v)
	}
	if m := findMount(bareDeployment.Spec.Template.Spec.Containers[0], "mirror-data"); m != nil {
		t.Fatalf("a silent template must get no mirror-data mount, got %#v", m)
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
	mirror.Spec.Publish.Rsync = &mirrorv1alpha1.MirrorServiceSpec{
		Replicas: ptr.To(int32(2)),
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

	// Both deployments exist and carry per-service labels/ports and the
	// shared read-only mirror-data volume.
	httpDeployment := &appsv1.Deployment{}
	get(t, ctx, fakeClient, client.ObjectKey{Namespace: mirror.Namespace, Name: "smoke-publish-http"}, httpDeployment)
	rsyncDeployment := &appsv1.Deployment{}
	get(t, ctx, fakeClient, client.ObjectKey{Namespace: mirror.Namespace, Name: "smoke-publish-rsync"}, rsyncDeployment)
	if got := *rsyncDeployment.Spec.Replicas; got != 2 {
		t.Fatalf("rsync replicas = %d, want 2", got)
	}
	if got := rsyncDeployment.Spec.Selector.MatchLabels[ComponentLabel]; got != "publish-rsync" {
		t.Fatalf("rsync selector component = %q, want publish-rsync (per-service pods)", got)
	}
	rsyncContainer := rsyncDeployment.Spec.Template.Spec.Containers[0]
	rsyncPorts := rsyncContainer.Ports
	if len(rsyncPorts) != 2 || rsyncPorts[0].Name != "rsync" || rsyncPorts[0].ContainerPort != 8730 || rsyncPorts[1].Name != "metrics" {
		t.Fatalf("rsync container ports = %#v; want the first port renamed to rsync and the second kept", rsyncPorts)
	}
	// The controller injects the mirror-data volume only: no mount of it is
	// added to the rsync container either.
	if rsyncMount := findMount(rsyncContainer, "mirror-data"); rsyncMount != nil {
		t.Fatalf("the controller must not mount mirror-data itself, got %#v", rsyncMount)
	}
	if v := findVolume(rsyncDeployment.Spec.Template.Spec.Volumes, "mirror-data"); v == nil || v.PersistentVolumeClaim == nil || !v.PersistentVolumeClaim.ReadOnly {
		t.Fatalf("every service key must get the read-only mirror-data volume, got %#v", v)
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
	markRouteAccepted(t, ctx, fakeClient, mirror.Namespace, "smoke-publish")
	reconcile(t, ctx, reconciler, request)
	current := getMirror(t, ctx, fakeClient, request.NamespacedName)
	if ready := findCondition(current.Status.Conditions, conditionReady); ready == nil || ready.Status != metav1.ConditionTrue {
		t.Fatalf("expected Ready=True once every service and the route are available, got %#v", current.Status.Conditions)
	}
}

// TestRsyncOnlyMirrorGetsNoRoute: a mirror publishing only rsync gets a
// Deployment and a ClusterIP Service but no HTTPRoute at all (no TCPRoute
// either).
func TestRsyncOnlyMirrorGetsNoRoute(t *testing.T) {
	ctx := context.Background()
	mirror := testMirror()
	mirror.Spec.Publish.HTTP = nil
	mirror.Spec.Publish.Rsync = &mirrorv1alpha1.MirrorServiceSpec{
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
// enabled service — an entirely absent services object, an absent http key
// (absent = disabled), or a Mirror that has never published a snapshot —
// creates no publish children. All these shapes flow through the same
// present-keys filter, so they are pinned in one place.
func TestAbsentOrDisabledServicesCreateNoWorkload(t *testing.T) {
	ctx := context.Background()

	// An entirely absent services object: the sync/snapshot pipeline still
	// runs (the Mirror settles Ready), but no publish workload or route is
	// created.
	mirror := testMirror()
	mirror.Spec.Publish = mirrorv1alpha1.MirrorServicesSpec{}
	mirror.Finalizers = []string{MirrorFinalizer}
	mirror.Status = mirrorv1alpha1.MirrorStatus{
		ObservedGeneration: mirror.Generation,
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
	if ready := findCondition(current.Status.Conditions, conditionReady); ready == nil || ready.Status != metav1.ConditionTrue {
		t.Fatalf("expected sync-only Mirror to be Ready, got %#v", current.Status.Conditions)
	}
	assertNotFound(t, ctx, fakeClient, client.ObjectKey{Namespace: mirror.Namespace, Name: "smoke-publish-http"}, &corev1.Service{})
	assertNotFound(t, ctx, fakeClient, client.ObjectKey{Namespace: mirror.Namespace, Name: "smoke-publish-http"}, &appsv1.Deployment{})
	assertNotFound(t, ctx, fakeClient, client.ObjectKey{Namespace: mirror.Namespace, Name: "smoke-publish"}, &gatewayv1.HTTPRoute{})

	// An absent key is as good as disabled — no workload for it, while the
	// present key still publishes.
	mirror = testMirror()
	mirror.Spec.Publish.HTTP = &mirrorv1alpha1.MirrorHTTPServiceSpec{
		MirrorServiceSpec: mirrorv1alpha1.MirrorServiceSpec{
			PodTemplate: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
				Containers: []corev1.Container{{
					Name:  "web",
					Image: "nginxinc/nginx-unprivileged:1.31.0-alpine",
					Ports: []corev1.ContainerPort{{Name: "web", ContainerPort: 8080, Protocol: corev1.ProtocolTCP}},
				}},
			}},
		},
	}
	mirror.Spec.Publish.Rsync = nil
	mirror.Finalizers = []string{MirrorFinalizer}
	mirror.Status = mirrorv1alpha1.MirrorStatus{
		ObservedGeneration: mirror.Generation,
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

	// The present http key publishes; the absent rsync key does not.
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
	mirror.Spec.Publish.HTTP.PodTemplate = corev1.PodTemplateSpec{
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
	if deployment.Spec.Template.Labels[ComponentLabel] != "publish-http" {
		t.Fatalf("forced component label must win: %#v", deployment.Spec.Template.Labels)
	}
	if got := deployment.Spec.Template.Annotations["example.com/owner"]; got != "webteam" {
		t.Fatalf("user pod annotation lost: %#v", deployment.Spec.Template.Annotations)
	}
}

// TestMirrorDataMountsAreForcedReadOnly: any user mount of the injected
// mirror-data volume is allowed only read-only; a writable one is InvalidSpec,
// and a user volume named mirror-data is rejected as reserved.
func TestMirrorDataMountsAreForcedReadOnly(t *testing.T) {
	mirror := testMirror()
	mirror.Spec.Publish.HTTP.PodTemplate.Spec.Containers = append(mirror.Spec.Publish.HTTP.PodTemplate.Spec.Containers, corev1.Container{
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
	mirror.Spec.Publish.HTTP.PodTemplate.Spec.Containers[0].VolumeMounts = []corev1.VolumeMount{
		{Name: "mirror-data", MountPath: "/srv/mirror/smoke/index", ReadOnly: true},
	}
	if errs := validateMirror(mirror); len(errs) != 0 {
		t.Fatalf("a read-only extra mirror-data mount must be accepted, got %v", errs)
	}

	// A user-declared volume named mirror-data collides with the injection.
	mirror = testMirror()
	mirror.Spec.Publish.HTTP.PodTemplate.Spec.Volumes = []corev1.Volume{{
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
	broken.Spec.Publish.HTTP.PodTemplate.Spec.Containers[0].Ports = nil
	errs := validateMirror(broken)
	if len(errs) == 0 || !strings.Contains(errs.ToAggregate().Error(), "ports") {
		t.Fatalf("an enabled service without container ports must be InvalidSpec, got %v", errs)
	}

	// No containers at all.
	broken = mirror.DeepCopy()
	broken.Spec.Publish.HTTP.PodTemplate.Spec.Containers = nil
	errs = validateMirror(broken)
	if len(errs) == 0 || !strings.Contains(errs.ToAggregate().Error(), "containers") {
		t.Fatalf("an enabled service without containers must be InvalidSpec, got %v", errs)
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
	base := childBase(name)
	mirror.Spec.Publish.HTTP = &mirrorv1alpha1.MirrorHTTPServiceSpec{
		MirrorServiceSpec: mirrorv1alpha1.MirrorServiceSpec{
			PodTemplate: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
				Containers: []corev1.Container{{
					Name:  "web",
					Image: "nginxinc/nginx-unprivileged:1.31.0-alpine",
					Ports: []corev1.ContainerPort{{Name: "web", ContainerPort: 8080, Protocol: corev1.ProtocolTCP}},
				}},
			}},
		},
	}
	mirror.Spec.Publish.HTTP.Aliases = aliases
	mirror.Finalizers = []string{MirrorFinalizer}
	mirror.Status = mirrorv1alpha1.MirrorStatus{
		ObservedGeneration: mirror.Generation,
		WorkPVC:            base + "-sync",
		ActivePVC:          base + "-snap-1756147200",
	}
	scheme := testScheme(t)
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&mirrorv1alpha1.Mirror{}, &appsv1.Deployment{}, &gatewayv1.HTTPRoute{}).
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
	if backends := route.Spec.Rules[0].BackendRefs; len(backends) != 1 || string(backends[0].Name) != "smoke-publish-http" {
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
	valid.Spec.Publish.HTTP.Aliases = []mirrorv1alpha1.MirrorHTTPAlias{
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
		broken.Spec.Publish.HTTP.Aliases = []mirrorv1alpha1.MirrorHTTPAlias{alias}
		if errs := validateMirror(broken); len(errs) == 0 {
			t.Fatalf("%s: alias %q must be InvalidSpec", name, alias)
		}
	}

	// An ABSENT http key is never validated — and with enable gone, aliases
	// (which live on the key) simply cannot be parked anywhere else.
	absent := testMirror()
	absent.Spec.Publish.HTTP = nil
	if errs := validateMirror(absent); len(errs) != 0 {
		t.Fatalf("aliases of an absent http key must not be validated, got %v", errs.ToAggregate())
	}
}

// routeParentStatus builds HTTPRoute parent status carrying a single
// Accepted condition with the given verdict.
func routeParentStatus(route *gatewayv1.HTTPRoute, status metav1.ConditionStatus, reason, message string) []gatewayv1.RouteParentStatus {
	return []gatewayv1.RouteParentStatus{{
		ControllerName: gatewayv1.GatewayController("gateway.networking.k8s.io/nginx-gateway"),
		ParentRef:      route.Spec.ParentRefs[0],
		Conditions: []metav1.Condition{
			{
				Type:               string(gatewayv1.RouteConditionAccepted),
				Status:             status,
				ObservedGeneration: route.Generation,
				LastTransitionTime: metav1.NewTime(time.Date(2026, 9, 3, 8, 0, 0, 0, time.UTC)),
				Reason:             reason,
				Message:            message,
			},
			{
				Type:               string(gatewayv1.RouteConditionResolvedRefs),
				Status:             metav1.ConditionTrue,
				ObservedGeneration: route.Generation,
				LastTransitionTime: metav1.NewTime(time.Date(2026, 9, 3, 8, 0, 0, 0, time.UTC)),
				Reason:             "ResolvedRefs",
			},
		},
	}}
}

func markRouteAccepted(t *testing.T, ctx context.Context, c client.Client, namespace, name string) {
	t.Helper()
	route := &gatewayv1.HTTPRoute{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, route); err != nil {
		routes := &gatewayv1.HTTPRouteList{}
		_ = c.List(ctx, routes)
		t.Fatalf("get HTTPRoute %s/%s: %v; existing routes: %#v", namespace, name, err, routes.Items)
	}
	route.Status.Parents = routeParentStatus(route, metav1.ConditionTrue, "Accepted", "Route is accepted")
	if err := c.Update(ctx, route); err != nil {
		t.Fatalf("mark HTTPRoute accepted: %v", err)
	}
}

func TestCurrentSyncObservesActivePublicationWithoutRevertingRollout(t *testing.T) {
	ctx := context.Background()
	mirror := testMirror()
	mirror.Finalizers = []string{MirrorFinalizer}
	mirror.Status = mirrorv1alpha1.MirrorStatus{
		ObservedGeneration: mirror.Generation,
		WorkPVC:            "smoke-sync",
		ActivePVC:          "smoke-snap-old",
		CurrentSync:        &mirrorv1alpha1.MirrorCurrentSyncStatus{StartedAt: timePtr(time.Unix(1788393600, 0))},
	}
	scheme := testScheme(t)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&mirrorv1alpha1.Mirror{}, &appsv1.Deployment{}).
		WithObjects(mirror).Build()
	addBoundSyncPVC(t, ctx, fakeClient, mirror, "smoke-sync", "s3.mirrors.zjusct.io", hostnameAffinity("s3.mirrors.zjusct.io"))
	reconciler := &MirrorReconciler{Client: fakeClient, Scheme: scheme, Config: testConfig(), SyncLimiter: NewSyncLimiter(0)}
	if _, err := reconciler.ensurePublish(ctx, mirror, mirror.Status.ActivePVC); err != nil {
		t.Fatalf("create active publication: %v", err)
	}
	if err := ensurePublishedMirrorRoute(ctx, reconciler, mirror); err != nil {
		t.Fatalf("create route: %v", err)
	}

	deployment := &appsv1.Deployment{}
	get(t, ctx, fakeClient, client.ObjectKey{Namespace: mirror.Namespace, Name: "smoke-publish-http"}, deployment)
	findVolume(deployment.Spec.Template.Spec.Volumes, PublishDataVolumeName).PersistentVolumeClaim.ClaimName = "smoke-snap-new"
	if err := fakeClient.Update(ctx, deployment); err != nil {
		t.Fatalf("stage new publication PVC: %v", err)
	}
	deployment.Status.ObservedGeneration = deployment.Generation
	deployment.Status.AvailableReplicas = 1 // old pod remains available during rollout
	if err := fakeClient.Status().Update(ctx, deployment); err != nil {
		t.Fatalf("mark old publication available: %v", err)
	}
	markRouteAccepted(t, ctx, fakeClient, mirror.Namespace, "smoke-publish")

	health, err := reconciler.reconcileActivePublication(ctx, mirror)
	if err != nil {
		t.Fatalf("observe active publication: %v", err)
	}
	if !health.ready || !health.progressing {
		t.Fatalf("rolling publication should remain ready while progressing, got %#v", health)
	}
	get(t, ctx, fakeClient, client.ObjectKey{Namespace: mirror.Namespace, Name: deployment.Name}, deployment)
	if got := findVolume(deployment.Spec.Template.Spec.Volumes, PublishDataVolumeName).PersistentVolumeClaim.ClaimName; got != "smoke-snap-new" {
		t.Fatalf("active observation reverted the in-flight PVC to %q", got)
	}
}

func TestDisabledChildCleanupPrecedesValidationAndPreservesForeignObjects(t *testing.T) {
	ctx := context.Background()
	mirror := testMirror()
	mirror.Finalizers = []string{MirrorFinalizer}
	mirror.Spec.Sync.Interval.Duration = 0 // unrelated invalid field
	mirror.Spec.Publish = mirrorv1alpha1.MirrorServicesSpec{}
	owner := *metav1.NewControllerRef(mirror, mirrorv1alpha1.GroupVersion.WithKind("Mirror"))
	controlled := []client.Object{
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: mirror.Namespace, Name: "smoke-publish-http", OwnerReferences: []metav1.OwnerReference{owner}}},
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: mirror.Namespace, Name: "smoke-publish-http", OwnerReferences: []metav1.OwnerReference{owner}}},
		&gatewayv1.HTTPRoute{ObjectMeta: metav1.ObjectMeta{Namespace: mirror.Namespace, Name: "smoke-publish", OwnerReferences: []metav1.OwnerReference{owner}}},
	}
	foreign := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: mirror.Namespace, Name: "smoke-publish-rsync"}}
	objects := []client.Object{mirror, foreign}
	objects = append(objects, controlled...)
	scheme := testScheme(t)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&mirrorv1alpha1.Mirror{}).WithObjects(objects...).Build()
	reconciler := &MirrorReconciler{Client: fakeClient, Scheme: scheme, Config: testConfig(), SyncLimiter: NewSyncLimiter(0)}
	reconcile(t, ctx, reconciler, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(mirror)})

	for _, object := range controlled {
		assertNotFound(t, ctx, fakeClient, client.ObjectKeyFromObject(object), object.DeepCopyObject().(client.Object))
	}
	get(t, ctx, fakeClient, client.ObjectKeyFromObject(foreign), &appsv1.Deployment{})
}

// TestGatewayRejectionDegradesMirrorWithPassthrough: an Accepted=False
// condition on the controller-owned HTTPRoute degrades the Mirror with the
// gateway's reason/message passed through VERBATIM (message gets a context
// prefix); the route and the publish workload are untouched. Overlapping
// paths (canonical /git vs alias /git/linux.git) coexist — the route is
// created unconditionally and the gateway, not falcon, adjudicates.
func TestGatewayRejectionDegradesMirrorWithPassthrough(t *testing.T) {
	// A second Mirror whose canonical path /git overlaps smoke's alias
	// /git/linux.git: both routes are generated — falcon has no say.
	gitMirror := testMirror()
	gitMirror.Name = "git"
	gitMirror.UID = types.UID("uid-git")
	gitMirror.Spec.Publish.HTTP = &mirrorv1alpha1.MirrorHTTPServiceSpec{
		MirrorServiceSpec: mirrorv1alpha1.MirrorServiceSpec{
			PodTemplate: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
				Containers: []corev1.Container{{
					Name: "web", Image: "nginxinc/nginx-unprivileged:1.31.0-alpine",
					Ports: []corev1.ContainerPort{{Name: "web", ContainerPort: 8080, Protocol: corev1.ProtocolTCP}},
				}},
			}},
		},
	}
	gitMirror.Finalizers = []string{MirrorFinalizer}
	gitMirror.Status = mirrorv1alpha1.MirrorStatus{
		ObservedGeneration: gitMirror.Generation,
		WorkPVC:            "git-sync",
		ActivePVC:          "git-snap-1756147200",
	}

	mirror, reconciler, fakeClient, request, ctx := publishedMirrorFixture(t, "smoke", "/linux.git", "/git/linux.git")
	if err := fakeClient.Create(ctx, gitMirror); err != nil {
		t.Fatalf("create overlapping mirror: %v", err)
	}
	reconcile(t, ctx, reconciler, request) // creates the workload AND the route
	markDeploymentAvailable(t, ctx, fakeClient, mirror.Namespace, "smoke-publish-http")
	reconcile(t, ctx, reconciler, request) // settles Ready

	// The gateway rejects the route: Accepted=False with its own reason.
	route := &gatewayv1.HTTPRoute{}
	get(t, ctx, fakeClient, client.ObjectKey{Namespace: mirror.Namespace, Name: "smoke-publish"}, route)
	route.Status.Parents = routeParentStatus(route, metav1.ConditionFalse, "PathConflict", "overlaps with route foo-publish")
	if err := fakeClient.Status().Update(ctx, route); err != nil {
		t.Fatalf("set route status: %v", err)
	}
	recorder, ok := reconciler.Recorder.(*record.FakeRecorder)
	if !ok {
		t.Fatal("recorder must be a FakeRecorder")
	}
	reconcile(t, ctx, reconciler, request)

	// The gateway's verdict makes the endpoint unavailable and degraded.
	current := getMirror(t, ctx, fakeClient, request.NamespacedName)
	wantMessage := "HTTPRoute smoke-publish Accepted=False (PathConflict): overlaps with route foo-publish"
	degraded := findCondition(current.Status.Conditions, conditionDegraded)
	if degraded == nil || degraded.Status != metav1.ConditionTrue || degraded.Reason != "HTTPRouteRejected" || degraded.Message != wantMessage {
		t.Fatalf("expected the route rejection in Degraded, got %#v", current.Status.Conditions)
	}
	// ...while the route and the publish workload are untouched.
	get(t, ctx, fakeClient, client.ObjectKey{Namespace: mirror.Namespace, Name: "smoke-publish"}, &gatewayv1.HTTPRoute{})
	deployment := &appsv1.Deployment{}
	get(t, ctx, fakeClient, client.ObjectKey{Namespace: mirror.Namespace, Name: "smoke-publish-http"}, deployment)
	if got := deployment.Spec.Template.Spec.NodeSelector[corev1.LabelHostname]; got != "s3.mirrors.zjusct.io" {
		t.Fatalf("publish workload must be unaffected by the route verdict, got %#v", deployment.Spec.Template.Spec)
	}
	waitForEvent(t, recorder, "Warning HTTPRouteRejected "+wantMessage)
}

// TestGatewayReadinessRequiresAcceptedAndResolvedRefs pins the endpoint
// contract: both current-generation route conditions are required.
func TestGatewayReadinessRequiresAcceptedAndResolvedRefs(t *testing.T) {
	mirror, reconciler, fakeClient, request, ctx := publishedMirrorFixture(t, "smoke")
	reconcile(t, ctx, reconciler, request)
	markDeploymentAvailable(t, ctx, fakeClient, mirror.Namespace, "smoke-publish-http")
	reconcile(t, ctx, reconciler, request) // Ready

	// Accepted=True and ResolvedRefs=True make the endpoint Ready.
	route := &gatewayv1.HTTPRoute{}
	get(t, ctx, fakeClient, client.ObjectKey{Namespace: mirror.Namespace, Name: "smoke-publish"}, route)
	route.Status.Parents = routeParentStatus(route, metav1.ConditionTrue, "Accepted", "Route is accepted")
	if err := fakeClient.Status().Update(ctx, route); err != nil {
		t.Fatalf("set route status: %v", err)
	}
	reconcile(t, ctx, reconciler, request)
	current := getMirror(t, ctx, fakeClient, request.NamespacedName)
	cond := findCondition(current.Status.Conditions, conditionReady)
	if cond == nil || cond.Status != metav1.ConditionTrue {
		t.Fatalf("expected Ready=True for an accepted and resolved route, got %#v", cond)
	}

	// Conditions from different parent-status entries cannot be combined.
	get(t, ctx, fakeClient, client.ObjectKey{Namespace: mirror.Namespace, Name: "smoke-publish"}, route)
	desiredParent := route.Spec.ParentRefs[0]
	otherParent := desiredParent
	otherParent.Name = "another-gateway"
	route.Status.Parents = []gatewayv1.RouteParentStatus{
		{ParentRef: desiredParent, Conditions: []metav1.Condition{{
			Type: string(gatewayv1.RouteConditionAccepted), Status: metav1.ConditionTrue,
			ObservedGeneration: route.Generation, Reason: "Accepted",
		}}},
		{ParentRef: otherParent, Conditions: []metav1.Condition{{
			Type: string(gatewayv1.RouteConditionResolvedRefs), Status: metav1.ConditionTrue,
			ObservedGeneration: route.Generation, Reason: "ResolvedRefs",
		}}},
	}
	if err := fakeClient.Status().Update(ctx, route); err != nil {
		t.Fatalf("set split route status: %v", err)
	}
	reconcile(t, ctx, reconciler, request)
	current = getMirror(t, ctx, fakeClient, request.NamespacedName)
	ready := findCondition(current.Status.Conditions, conditionReady)
	progressing := findCondition(current.Status.Conditions, conditionProgressing)
	if ready == nil || ready.Status != metav1.ConditionFalse || progressing == nil || progressing.Status != metav1.ConditionTrue {
		t.Fatalf("split parent conditions must be Ready=False/Progressing=True, got %#v", current.Status.Conditions)
	}
}

// TestGatewayRejectionSelfHeals: a False->True verdict flip clears the
// rejection — the Mirror returns to Ready through the normal flow.
func TestGatewayRejectionSelfHeals(t *testing.T) {
	mirror, reconciler, fakeClient, request, ctx := publishedMirrorFixture(t, "smoke")
	reconcile(t, ctx, reconciler, request)
	markDeploymentAvailable(t, ctx, fakeClient, mirror.Namespace, "smoke-publish-http")
	reconcile(t, ctx, reconciler, request) // Ready

	route := &gatewayv1.HTTPRoute{}
	get(t, ctx, fakeClient, client.ObjectKey{Namespace: mirror.Namespace, Name: "smoke-publish"}, route)
	route.Status.Parents = routeParentStatus(route, metav1.ConditionFalse, "PathConflict", "overlaps with route foo-publish")
	if err := fakeClient.Status().Update(ctx, route); err != nil {
		t.Fatalf("set route status: %v", err)
	}
	reconcile(t, ctx, reconciler, request)
	current := getMirror(t, ctx, fakeClient, request.NamespacedName)
	if degraded := findCondition(current.Status.Conditions, conditionDegraded); degraded == nil || degraded.Status != metav1.ConditionTrue {
		t.Fatalf("expected Degraded=True on rejection, got %#v", current.Status.Conditions)
	}

	// The gateway flips to Accepted: the next reconcile (Owns(HTTPRoute)
	// watch in production) self-heals back to Ready.
	route.Status.Parents = routeParentStatus(route, metav1.ConditionTrue, "Accepted", "Route is accepted")
	if err := fakeClient.Status().Update(ctx, route); err != nil {
		t.Fatalf("flip route status: %v", err)
	}
	reconcile(t, ctx, reconciler, request)
	current = getMirror(t, ctx, fakeClient, request.NamespacedName)
	cond := findCondition(current.Status.Conditions, conditionDegraded)
	if cond == nil || cond.Status != metav1.ConditionFalse {
		t.Fatalf("stale rejection condition must be cleared, got %#v", cond)
	}
	ready := findCondition(current.Status.Conditions, conditionReady)
	if ready == nil || ready.Reason != "Published" || ready.Status != metav1.ConditionTrue {
		t.Fatalf("expected Ready=True/Published after self-heal, got %#v", ready)
	}
}
