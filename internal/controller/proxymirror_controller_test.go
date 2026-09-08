package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
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

	mirrorv1alpha1 "github.com/ZJUSCT/falcon/api/v1alpha1"
)

func testProxyMirror() *mirrorv1alpha1.ProxyMirror {
	return &mirrorv1alpha1.ProxyMirror{
		TypeMeta: metav1.TypeMeta{APIVersion: mirrorv1alpha1.GroupVersion.String(), Kind: "ProxyMirror"},
		ObjectMeta: metav1.ObjectMeta{
			Namespace:  "mirrors",
			Name:       "pypi-proxy",
			UID:        types.UID("test-proxymirror-uid"),
			Generation: 1,
		},
		Spec: mirrorv1alpha1.ProxyMirrorSpec{
			Info: mirrorv1alpha1.ProxyMirrorInfo{
				Description: "Caching proxy in front of PyPI",
				Upstream:    "https://pypi.org/simple/",
			},
			Cache: &mirrorv1alpha1.ProxyMirrorCacheSpec{
				PVCSpec: corev1.PersistentVolumeClaimSpec{
					StorageClassName: ptr.To("delete-class"),
					AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
					Resources:        corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("100Gi")}},
				},
			},
			Publish: mirrorv1alpha1.ProxyMirrorServicesSpec{
				HTTP: &mirrorv1alpha1.ProxyMirrorServiceSpec{MirrorServiceSpec: mirrorv1alpha1.MirrorServiceSpec{
					PodTemplate: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
						Containers: []corev1.Container{{
							Name:  "proxy",
							Image: "nginxinc/nginx-unprivileged:1.31.0-alpine",
							Ports: []corev1.ContainerPort{{Name: "web", ContainerPort: 8080, Protocol: corev1.ProtocolTCP}},
						}},
					}},
				}},
			},
		},
	}
}

func testProxyScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("add core scheme: %v", err)
	}
	if err := gatewayv1.Install(scheme); err != nil {
		t.Fatalf("add gateway scheme: %v", err)
	}
	if err := mirrorv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add mirror scheme: %v", err)
	}
	return scheme
}

func reconcileProxy(t *testing.T, ctx context.Context, reconciler *ProxyMirrorReconciler, request ctrl.Request) {
	t.Helper()
	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
}

func getProxyMirror(t *testing.T, ctx context.Context, c client.Client, key client.ObjectKey) *mirrorv1alpha1.ProxyMirror {
	t.Helper()
	value := &mirrorv1alpha1.ProxyMirror{}
	get(t, ctx, c, key, value)
	return value
}

func TestProxyMirrorHappyPathPublishesAndProvisionsCache(t *testing.T) {
	ctx := context.Background()
	proxy := testProxyMirror()
	scheme := testProxyScheme(t)
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&mirrorv1alpha1.ProxyMirror{}, &appsv1.Deployment{}).
		WithObjects(proxy).
		Build()
	reconciler := &ProxyMirrorReconciler{
		Client:   fakeClient,
		Scheme:   scheme,
		Recorder: record.NewFakeRecorder(20),
		Now:      func() time.Time { return time.Now().UTC() },
		Config:   testConfig(),
	}
	request := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: proxy.Namespace, Name: proxy.Name}}

	reconcileProxy(t, ctx, reconciler, request) // cache PVC + Service + Deployment
	claim := &corev1.PersistentVolumeClaim{}
	get(t, ctx, fakeClient, client.ObjectKey{Namespace: proxy.Namespace, Name: "pypi-proxy-cache"}, claim)
	if claim.Spec.StorageClassName == nil || *claim.Spec.StorageClassName != "delete-class" {
		t.Fatalf("cache PVC storage class = %v; expected delete-class", claim.Spec.StorageClassName)
	}
	if got := claim.Spec.Resources.Requests[corev1.ResourceStorage]; got.Cmp(resource.MustParse("100Gi")) != 0 {
		t.Fatalf("cache PVC capacity = %s; expected 100Gi", got.String())
	}
	if len(claim.OwnerReferences) != 1 || claim.OwnerReferences[0].Kind != "ProxyMirror" {
		t.Fatalf("cache PVC must be owned by the ProxyMirror: %#v", claim.OwnerReferences)
	}

	service := &corev1.Service{}
	get(t, ctx, fakeClient, client.ObjectKey{Namespace: proxy.Namespace, Name: "pypi-proxy-publish-http"}, service)
	if got := service.Spec.Ports[0].Port; got != 80 {
		t.Fatalf("proxy publish Service port = %d, want 80", got)
	}
	deployment := &appsv1.Deployment{}
	get(t, ctx, fakeClient, client.ObjectKey{Namespace: proxy.Namespace, Name: "pypi-proxy-publish-http"}, deployment)
	if got := *deployment.Spec.Replicas; got != 1 {
		t.Fatalf("expected 1 replica, got %d", got)
	}
	// The proxy-cache volume is injected (writable PVC source pointing at the
	// cache PVC); mounting it is the user template's declaration.
	cacheVolume := false
	for _, volume := range deployment.Spec.Template.Spec.Volumes {
		if volume.Name == "proxy-cache" && volume.PersistentVolumeClaim != nil && volume.PersistentVolumeClaim.ClaimName == "pypi-proxy-cache" {
			cacheVolume = true
		}
	}
	if !cacheVolume {
		t.Fatal("proxy Deployment must inject the cache PVC as the proxy-cache volume")
	}
	if len(deployment.Spec.Template.Spec.Volumes) != 1 { // cache only, no data volume
		t.Fatalf("proxy Deployment must have no data volume, got %#v", deployment.Spec.Template.Spec.Volumes)
	}
	// Falcon preserves operator workload fields; only the cache volume is injected.
	first := deployment.Spec.Template.Spec.Containers[0]
	if first.ReadinessProbe != nil || first.SecurityContext != nil {
		t.Fatalf("Falcon must not inject workload defaults: %#v", first)
	}
	// The controller adds no mounts: proxy-cache is volumes-only (the nginx
	// proxy_cache directory is the user's declaration).
	if cacheMount := findMount(first, "proxy-cache"); cacheMount != nil {
		t.Fatalf("the controller must not mount proxy-cache itself, got %#v", cacheMount)
	}

	deployment.Status.ObservedGeneration = deployment.Generation
	deployment.Status.Conditions = []appsv1.DeploymentCondition{{
		Type: appsv1.DeploymentReplicaFailure, Status: corev1.ConditionTrue,
		Reason: "FailedCreate", Message: "exceeded quota",
	}}
	if err := fakeClient.Status().Update(ctx, deployment); err != nil {
		t.Fatal(err)
	}
	reconcileProxy(t, ctx, reconciler, request)
	failed := getProxyMirror(t, ctx, fakeClient, request.NamespacedName)
	degraded := findCondition(failed.Status.Conditions, conditionDegraded)
	if degraded == nil || degraded.Status != metav1.ConditionTrue || degraded.Reason != "PublishDeploymentFailed" || !strings.Contains(degraded.Message, "exceeded quota") {
		t.Fatalf("proxy lost Deployment failure: %#v", degraded)
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
	reconcileProxy(t, ctx, reconciler, request) // create route; verdict pending
	markRouteAccepted(t, ctx, fakeClient, proxy.Namespace, "pypi-proxy-publish")
	reconcileProxy(t, ctx, reconciler, request) // ready
	current := getProxyMirror(t, ctx, fakeClient, request.NamespacedName)
	if condReady := findCondition(current.Status.Conditions, conditionReady); condReady == nil || condReady.Status != metav1.ConditionTrue {
		t.Fatalf("expected Ready=True, got %#v", current.Status.Conditions)
	}
	degraded = findCondition(current.Status.Conditions, conditionDegraded)
	if degraded == nil || degraded.Status != metav1.ConditionFalse {
		t.Fatalf("proxy retained a recovered failure: %#v", degraded)
	}
	// A Ready proxy is published: its publish HTTPRoute must exist.
	route := &gatewayv1.HTTPRoute{}
	get(t, ctx, fakeClient, client.ObjectKey{Namespace: proxy.Namespace, Name: "pypi-proxy-publish"}, route)
	assertPublishRouteShape(t, route, proxy, "/pypi-proxy", "pypi-proxy-publish-http")
}

func TestProxyMirrorInvalidCacheSpecIsDegraded(t *testing.T) {
	ctx := context.Background()
	proxy := testProxyMirror()
	proxy.Spec.Cache.PVCSpec.AccessModes = nil
	scheme := testProxyScheme(t)
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&mirrorv1alpha1.ProxyMirror{}).
		WithObjects(proxy).
		Build()
	reconciler := &ProxyMirrorReconciler{
		Client: fakeClient,
		Scheme: scheme,
		Now:    func() time.Time { return time.Now().UTC() },
		Config: testConfig(),
	}
	request := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: proxy.Namespace, Name: proxy.Name}}

	reconcileProxy(t, ctx, reconciler, request)
	current := getProxyMirror(t, ctx, fakeClient, request.NamespacedName)
	if degraded := findCondition(current.Status.Conditions, conditionDegraded); degraded == nil || degraded.Status != metav1.ConditionTrue {
		t.Fatalf("expected Degraded=True, got %#v", current.Status.Conditions)
	}
	assertNotFound(t, ctx, fakeClient, client.ObjectKey{Namespace: proxy.Namespace, Name: "pypi-proxy-cache"}, &corev1.PersistentVolumeClaim{})
	// A proxy that never became Ready gets no publish route.
	assertNotFound(t, ctx, fakeClient, client.ObjectKey{Namespace: proxy.Namespace, Name: "pypi-proxy-publish"}, &gatewayv1.HTTPRoute{})
}

// TestProxyMirrorDisabledHTTPDeploysNothing: with the http key absent —
// whether the whole services object or just the key (absent = disabled) —
// no publish workload or cache is retained.
func TestProxyMirrorDisabledHTTPDeploysNothing(t *testing.T) {
	for name, mutate := range map[string]func(*mirrorv1alpha1.ProxyMirror){
		"absent-services": func(p *mirrorv1alpha1.ProxyMirror) { p.Spec.Publish = mirrorv1alpha1.ProxyMirrorServicesSpec{} },
		"absent-http-key": func(p *mirrorv1alpha1.ProxyMirror) { p.Spec.Publish.HTTP = nil },
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			proxy := testProxyMirror()
			mutate(proxy)
			scheme := testProxyScheme(t)
			fakeClient := fake.NewClientBuilder().
				WithScheme(scheme).
				WithStatusSubresource(&mirrorv1alpha1.ProxyMirror{}, &appsv1.Deployment{}).
				WithObjects(proxy).
				Build()
			reconciler := &ProxyMirrorReconciler{
				Client:   fakeClient,
				Scheme:   scheme,
				Recorder: record.NewFakeRecorder(20),
				Now:      func() time.Time { return time.Now().UTC() },
				Config:   testConfig(),
			}
			request := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: proxy.Namespace, Name: proxy.Name}}

			reconcileProxy(t, ctx, reconciler, request)

			assertNotFound(t, ctx, fakeClient, client.ObjectKey{Namespace: proxy.Namespace, Name: "pypi-proxy-publish-http"}, &appsv1.Deployment{})
			assertNotFound(t, ctx, fakeClient, client.ObjectKey{Namespace: proxy.Namespace, Name: "pypi-proxy-publish-http"}, &corev1.Service{})
			assertNotFound(t, ctx, fakeClient, client.ObjectKey{Namespace: proxy.Namespace, Name: "pypi-proxy-publish"}, &gatewayv1.HTTPRoute{})
			current := getProxyMirror(t, ctx, fakeClient, request.NamespacedName)
			if ready := findCondition(current.Status.Conditions, conditionReady); ready == nil || ready.Status != metav1.ConditionFalse {
				t.Fatalf("disabled HTTP must report Ready=False, got %#v", current.Status.Conditions)
			}
			get(t, ctx, fakeClient, client.ObjectKey{Namespace: proxy.Namespace, Name: "pypi-proxy-cache"}, &corev1.PersistentVolumeClaim{})
		})
	}
}

// TestProxyMirrorReservedCacheVolumeRejected: a user pod template may not
// declare a volume named proxy-cache (the controller injects it itself).
func TestProxyMirrorReservedCacheVolumeRejected(t *testing.T) {
	proxy := testProxyMirror()
	proxy.Spec.Publish.HTTP.PodTemplate.Spec.Volumes = []corev1.Volume{{
		Name:         "proxy-cache",
		VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
	}}
	errs := validateProxyMirror(proxy)
	if len(errs) == 0 || !strings.Contains(errs.ToAggregate().Error(), "reserved") {
		t.Fatalf("a user volume named proxy-cache must be rejected as reserved, got %v", errs)
	}
}

func findCondition(conditions []metav1.Condition, conditionType string) *metav1.Condition {
	for i := range conditions {
		if conditions[i].Type == conditionType {
			return &conditions[i]
		}
	}
	return nil
}
