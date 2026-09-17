package controller

import (
	"context"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	mirrorv1alpha1 "github.com/ZJUSCT/falcon/api/v1alpha1"
)

// reloaderStamp mimics the value Reloader writes into last-reloaded-from
// under the annotations reload strategy.
const reloaderStamp = `{"name":"mirrors/configmap/publish-listing-nginx"}`

// stampPublishDeployment simulates Reloader patching the pod template of a
// publish Deployment, and returns the deployment's resource version after
// the stamp.
func stampPublishDeployment(t *testing.T, ctx context.Context, c client.Client, namespace, name, value string) string {
	t.Helper()
	deployment := &appsv1.Deployment{}
	get(t, ctx, c, client.ObjectKey{Namespace: namespace, Name: name}, deployment)
	before := deployment.DeepCopy()
	if deployment.Spec.Template.Annotations == nil {
		deployment.Spec.Template.Annotations = map[string]string{}
	}
	deployment.Spec.Template.Annotations[reloaderStampAnnotation] = value
	if err := c.Patch(ctx, deployment, client.MergeFrom(before)); err != nil {
		t.Fatal(err)
	}
	return deployment.ResourceVersion
}

// hotReloadReconciler builds the standing reconciler of the paused-publish
// tests against a fixture client.
func hotReloadReconciler(t *testing.T, ctx context.Context, mirror *mirrorv1alpha1.Mirror) (*MirrorReconciler, client.Client, ctrl.Request) {
	t.Helper()
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
	return reconciler, fakeClient, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(mirror)}
}

// TestReloaderStampSurvivesReconcile: under the annotations reload strategy
// Reloader marks the publish pod template with last-reloaded-from; the
// controller's wholesale template replacement must preserve that foreign
// stamp and stay write-free when nothing else changed — otherwise every
// reloader-triggered rollout would be reverted and re-rolled. A value the
// CR declares for the reserved key never wins over the live stamp.
func TestReloaderStampSurvivesReconcile(t *testing.T) {
	ctx := context.Background()
	mirror := pausedPublishedMirror(t, "smoke")
	mirror.Spec.Publish.HTTP.PodTemplate.Annotations = map[string]string{
		reloaderStampAnnotation: "declared-on-the-cr-and-ignored",
	}
	reconciler, fakeClient, request := hotReloadReconciler(t, ctx, mirror)

	reconcile(t, ctx, reconciler, request)
	stampedAt := stampPublishDeployment(t, ctx, fakeClient, mirror.Namespace, "smoke-publish-http", reloaderStamp)
	reconcile(t, ctx, reconciler, request)

	deployment := &appsv1.Deployment{}
	get(t, ctx, fakeClient, client.ObjectKey{Namespace: mirror.Namespace, Name: "smoke-publish-http"}, deployment)
	if got := deployment.Spec.Template.Annotations[reloaderStampAnnotation]; got != reloaderStamp {
		t.Fatalf("the live stamp must win over the CR declaration, got %q", got)
	}
	if deployment.ResourceVersion != stampedAt {
		t.Fatalf("the reconcile must not write the deployment (it would re-roll the reloader rollout): rv %s -> %s", stampedAt, deployment.ResourceVersion)
	}
}

// TestReloaderStampCarriedOnOwnUpdate: a controller-owned template change (a
// new image) rolls the deployment while carrying the live stamp along — the
// stamp belongs to the template's identity, not to any single revision.
func TestReloaderStampCarriedOnOwnUpdate(t *testing.T) {
	ctx := context.Background()
	mirror := pausedPublishedMirror(t, "smoke")
	reconciler, fakeClient, request := hotReloadReconciler(t, ctx, mirror)

	reconcile(t, ctx, reconciler, request)
	stampPublishDeployment(t, ctx, fakeClient, mirror.Namespace, "smoke-publish-http", reloaderStamp)

	current := getMirror(t, ctx, fakeClient, request.NamespacedName)
	current.Spec.Publish.HTTP.PodTemplate.Spec.Containers[0].Image = "nginxinc/nginx-unprivileged:other"
	if err := fakeClient.Update(ctx, current); err != nil {
		t.Fatal(err)
	}
	reconcile(t, ctx, reconciler, request)

	deployment := &appsv1.Deployment{}
	get(t, ctx, fakeClient, client.ObjectKey{Namespace: mirror.Namespace, Name: "smoke-publish-http"}, deployment)
	if got := deployment.Spec.Template.Spec.Containers[0].Image; got != "nginxinc/nginx-unprivileged:other" {
		t.Fatalf("the controller-owned template change must land, got image %q", got)
	}
	if got := deployment.Spec.Template.Annotations[reloaderStampAnnotation]; got != reloaderStamp {
		t.Fatalf("the own update must carry the live stamp along, got %q", got)
	}
}
