package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	mirrorv1alpha1 "github.com/ZJUSCT/falcon/api/v1alpha1"
)

// TestPublishChildNameDNS1035Mapping pins the publish workload naming rule:
// Service names are DNS-1035 labels and cannot carry the CR name's dots, so
// the publish Deployment and Service (which always share a name) map dots to
// '-'; every other derived kind keeps the CR name as-is.
func TestPublishChildNameDNS1035Mapping(t *testing.T) {
	cases := []struct {
		name     string
		base     string
		protocol string
		want     string
	}{
		{"plain name unchanged", "smoke", PublishProtocolHTTP, "smoke-publish-http"},
		{"dots mapped for the workload pair", "crates.io-index", PublishProtocolHTTP, "crates-io-index-publish-http"},
		{"every dot maps", "crates.io-index.git", PublishProtocolRsync, "crates-io-index-git-publish-rsync"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := publishChildName(tc.base, tc.protocol); got != tc.want {
				t.Fatalf("publishChildName(%q, %q) = %q, want %q", tc.base, tc.protocol, got, tc.want)
			}
		})
	}
}

// pausedPublishedMirror is testMirror paused with a published active
// snapshot — the standing setup of the paused-publish tests; name selects
// the CR name (e.g. a dotted one for the naming tests).
func pausedPublishedMirror(t *testing.T, name string) *mirrorv1alpha1.Mirror {
	t.Helper()
	mirror := testMirror()
	mirror.Name = name
	mirror.SetSyncPaused(true)
	mirror.Finalizers = []string{MirrorFinalizer}
	mirror.Status = mirrorv1alpha1.MirrorStatus{
		ObservedGeneration: mirror.Generation,
		WorkPVC:            name + "-sync",
		ActivePVC:          name + "-snap-1756147200",
		ActiveSnapshot:     name + "-snap-1756147200",
	}
	return mirror
}

// TestDottedMirrorPublishChildren: a dotted CR name (crates.io-index) is a
// valid DNS subdomain, so storage children and the HTTPRoute keep it as-is,
// while the publish Deployment/Service pair serves under the DNS-1035 name
// and the route's backendRef points at the transformed name.
func TestDottedMirrorPublishChildren(t *testing.T) {
	ctx := context.Background()
	mirror := pausedPublishedMirror(t, "crates.io-index")
	scheme := testScheme(t)
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&mirrorv1alpha1.Mirror{}, &appsv1.Deployment{}).
		WithObjects(mirror).
		Build()
	addBoundPublishPVC(t, ctx, fakeClient, mirror, mirror.Status.ActivePVC)
	reconciler := &MirrorReconciler{
		Client: fakeClient, Scheme: scheme, Recorder: record.NewFakeRecorder(20),
		Now:    func() time.Time { return time.Now().UTC() },
		Config: testConfig(), SyncLimiter: NewSyncLimiter(0),
	}
	request := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: mirror.Namespace, Name: mirror.Name}}

	reconcile(t, ctx, reconciler, request) // ensures publish workload + route
	markDeploymentAvailable(t, ctx, fakeClient, mirror.Namespace, "crates-io-index-publish-http")
	reconcile(t, ctx, reconciler, request) // settles into Paused

	get(t, ctx, fakeClient, client.ObjectKey{Namespace: mirror.Namespace, Name: "crates-io-index-publish-http"}, &corev1.Service{})
	deployment := &appsv1.Deployment{}
	get(t, ctx, fakeClient, client.ObjectKey{Namespace: mirror.Namespace, Name: "crates-io-index-publish-http"}, deployment)
	if label := deployment.Spec.Template.Labels[MirrorLabel]; label != "crates.io-index" {
		t.Fatalf("pod selector labels keep the CR name (dots included), got %q", label)
	}
	route := &gatewayv1.HTTPRoute{}
	get(t, ctx, fakeClient, client.ObjectKey{Namespace: mirror.Namespace, Name: "crates.io-index-publish"}, route)
	assertPublishRouteShape(t, route, mirror, "/crates.io-index", "crates-io-index-publish-http")
}

// TestDottedNameCollisionReportsDegraded: crates.io-index and crates-io-index
// normalize to the same publish workload names. Falcon must not adopt the
// other mirror's child; the conflict projects as Degraded/DerivedResourceInvalid.
func TestDottedNameCollisionReportsDegraded(t *testing.T) {
	ctx := context.Background()
	mirror := pausedPublishedMirror(t, "crates.io-index")
	scheme := testScheme(t)
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&mirrorv1alpha1.Mirror{}).
		WithObjects(mirror).
		Build()
	addBoundPublishPVC(t, ctx, fakeClient, mirror, mirror.Status.ActivePVC)
	foreign := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: mirror.Namespace,
			Name:      "crates-io-index-publish-http",
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: mirrorv1alpha1.GroupVersion.String(),
				Kind:       "Mirror",
				Name:       "crates-io-index",
				UID:        "other-mirror-uid",
				Controller: ptr.To(true),
			}},
		},
	}
	if err := fakeClient.Create(ctx, foreign); err != nil {
		t.Fatal(err)
	}
	recorder := record.NewFakeRecorder(20)
	reconciler := &MirrorReconciler{
		Client: fakeClient, Scheme: scheme, Recorder: recorder,
		Now:    func() time.Time { return time.Now().UTC() },
		Config: testConfig(), SyncLimiter: NewSyncLimiter(0),
	}
	request := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: mirror.Namespace, Name: mirror.Name}}

	reconcile(t, ctx, reconciler, request)

	current := getMirror(t, ctx, fakeClient, request.NamespacedName)
	condition := findCondition(current.Status.Conditions, conditionDegraded)
	if condition == nil || condition.Status != metav1.ConditionTrue || condition.Reason != derivedResourceInvalid {
		t.Fatalf("expected DerivedResourceInvalid degradation on the name collision, got %#v", current.Status.Conditions)
	}
	if !strings.Contains(condition.Message, "crates-io-index-publish-http") || !strings.Contains(condition.Message, "crates-io-index") {
		t.Fatalf("expected the colliding workload name and owner in the condition, got %q", condition.Message)
	}
	waitForEvent(t, recorder, derivedResourceInvalid)
	// The foreign child keeps its original owner untouched.
	svc := &corev1.Service{}
	get(t, ctx, fakeClient, client.ObjectKey{Namespace: mirror.Namespace, Name: "crates-io-index-publish-http"}, svc)
	if svc.OwnerReferences[0].UID != "other-mirror-uid" {
		t.Fatalf("the foreign child must keep its original owner, got %#v", svc.OwnerReferences)
	}
}
