package controller

import (
	"context"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
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
	if len(route.Spec.Rules) != 1 || len(route.Spec.Rules[0].Matches) != 1 {
		t.Fatalf("want one rule with one match, got %#v", route.Spec.Rules)
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

// TestMirrorUnsyncedHasNoPublishRoute: a Mirror that never published anything
// gets no HTTPRoute.
func TestMirrorUnsyncedHasNoPublishRoute(t *testing.T) {
	ctx := context.Background()
	mirror := testMirror()
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
	reconcile(t, ctx, reconciler, request) // start first sync (initializing)

	assertNotFound(t, ctx, fakeClient, client.ObjectKey{Namespace: mirror.Namespace, Name: "smoke-publish"}, &gatewayv1.HTTPRoute{})
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

// TestPublishDataMountPathAppendsMirrorName: the data PVC is mounted one
// level below the configured web root (<mountPath>/<name>) so PVC-root
// content is served under the /<name> publish route prefix.
func TestPublishDataMountPathAppendsMirrorName(t *testing.T) {
	cases := []struct {
		name      string
		mountPath string
		want      string
	}{
		{name: "custom-webroot", mountPath: "/usr/share/nginx/html", want: "/usr/share/nginx/html/smoke"},
		{name: "default-webroot", mountPath: "", want: "/srv/mirror/smoke"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			mirror := testMirror()
			mirror.Spec.Services[0].MountPath = tc.mountPath
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
			reconciler := &MirrorReconciler{
				Client: fakeClient, Scheme: scheme,
				Config: testConfig(), SyncLimiter: NewSyncLimiter(0),
			}
			request := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: mirror.Namespace, Name: mirror.Name}}
			reconcile(t, ctx, reconciler, request) // creates Service + publish Deployment

			deployment := &appsv1.Deployment{}
			get(t, ctx, fakeClient, client.ObjectKey{Namespace: mirror.Namespace, Name: "smoke-publish-http"}, deployment)
			mounts := deployment.Spec.Template.Spec.Containers[0].VolumeMounts
			if len(mounts) != 2 {
				t.Fatalf("want data + tmp mounts, got %#v", mounts)
			}
			if mounts[0].Name != "mirror-data" || mounts[0].MountPath != tc.want {
				t.Fatalf("data PVC mountPath = %q, want %q (web root + route prefix /smoke)", mounts[0].MountPath, tc.want)
			}
			if !mounts[0].ReadOnly {
				t.Fatal("data PVC must stay mounted read-only")
			}
			if mounts[1].Name != "tmp" || mounts[1].MountPath != "/tmp" {
				t.Fatalf("tmp mount wrong: %#v", mounts[1])
			}
		})
	}
}

// TestServicesRenderPerEntry: every services[] entry gets its own
// Deployment and Service named <base>-publish-<protocol> with per-service pod
// labels, but only the "http" entry gets the publish HTTPRoute. The first
// container port is (re)named after the protocol and targeted by the Service;
// non-http Service ports carry no appProtocol.
func TestServicesRenderPerEntry(t *testing.T) {
	ctx := context.Background()
	mirror := testMirror()
	two := int32(2)
	mirror.Spec.Services = append(mirror.Spec.Services, mirrorv1alpha1.MirrorServingService{
		Name:     "rsync",
		Image:    "docker.io/library/busybox:1.37.0",
		Replicas: &two,
		Ports: []corev1.ContainerPort{
			{Name: "ignored", ContainerPort: 8730, Protocol: corev1.ProtocolTCP},
			{Name: "metrics", ContainerPort: 9100, Protocol: corev1.ProtocolTCP},
		},
		MountPath: "/export/mirror",
	})
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
	rsyncPorts := rsyncDeployment.Spec.Template.Spec.Containers[0].Ports
	if len(rsyncPorts) != 2 || rsyncPorts[0].Name != "rsync" || rsyncPorts[0].ContainerPort != 8730 || rsyncPorts[1].Name != "metrics" {
		t.Fatalf("rsync container ports = %#v; want the first port renamed to rsync and the second kept", rsyncPorts)
	}
	rsyncMounts := rsyncDeployment.Spec.Template.Spec.Containers[0].VolumeMounts
	if rsyncMounts[0].MountPath != "/export/mirror/smoke" || !rsyncMounts[0].ReadOnly {
		t.Fatalf("rsync data mount = %#v; want read-only /export/mirror/smoke (same data PVC for every service type)", rsyncMounts[0])
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

	// Readiness requires every entry: flip both, the Mirror settles Ready.
	markDeploymentAvailable(t, ctx, fakeClient, mirror.Namespace, "smoke-publish-http")
	markDeploymentAvailable(t, ctx, fakeClient, mirror.Namespace, "smoke-publish-rsync")
	reconcile(t, ctx, reconciler, request)
	current := getMirror(t, ctx, fakeClient, request.NamespacedName)
	if current.Status.Phase != mirrorv1alpha1.PhaseReady {
		t.Fatalf("expected Ready once every service entry rolled out, got %s (%#v)", current.Status.Phase, current.Status.Conditions)
	}
}

// TestRsyncOnlyMirrorGetsNoRoute: a mirror publishing only rsync/git gets
// Deployments and Services but no HTTPRoute at all (no TCPRoute either).
func TestRsyncOnlyMirrorGetsNoRoute(t *testing.T) {
	ctx := context.Background()
	mirror := testMirror()
	mirror.Spec.Services[0].Name = "rsync"
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
		Config: testConfig(), SyncLimiter: NewSyncLimiter(0),
	}
	request := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: mirror.Namespace, Name: mirror.Name}}
	reconcile(t, ctx, reconciler, request)

	get(t, ctx, fakeClient, client.ObjectKey{Namespace: mirror.Namespace, Name: "smoke-publish-rsync"}, &appsv1.Deployment{})
	get(t, ctx, fakeClient, client.ObjectKey{Namespace: mirror.Namespace, Name: "smoke-publish-rsync"}, &corev1.Service{})
	assertNotFound(t, ctx, fakeClient, client.ObjectKey{Namespace: mirror.Namespace, Name: "smoke-publish"}, &gatewayv1.HTTPRoute{})
}

// TestServicesOmittedDisablePublishing: without spec.services the sync/
// snapshot pipeline still runs, but no publish workload or route is created.
func TestServicesOmittedDisablePublishing(t *testing.T) {
	ctx := context.Background()
	mirror := testMirror()
	mirror.Spec.Services = nil
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
}
