package controller

import (
	"testing"
	"time"

	snapshotv1 "github.com/kubernetes-csi/external-snapshotter/client/v8/apis/volumesnapshot/v1"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	mirrorv1alpha1 "github.com/ZJUSCT/falcon/api/v1alpha1"
)

func TestServiceShutdownDoesNotWaitForCancellation(t *testing.T) {
	m := abortRequestMirror()
	m.Spec.Publish = mirrorv1alpha1.MirrorServicesSpec{}
	m.Spec.Sync.Interval.Duration = 0 // Shutdown and abort also work with an invalid spec.
	m.Status.CurrentSync.Phase = mirrorv1alpha1.SyncPhaseCancelling
	setCondition(m, conditionReady, metav1.ConditionTrue, "Published", "previously serving")
	owner := []metav1.OwnerReference{*metav1.NewControllerRef(m, mirrorv1alpha1.GroupVersion.WithKind("Mirror"))}
	deployment := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: m.Namespace, Name: "smoke-publish-http", OwnerReferences: owner}}
	service := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: m.Namespace, Name: deployment.Name, OwnerReferences: owner}}
	route := &gatewayv1.HTTPRoute{ObjectMeta: metav1.ObjectMeta{Namespace: m.Namespace, Name: "smoke-publish", OwnerReferences: owner}}
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Namespace: m.Namespace, Name: currentSyncJobName(m), Finalizers: []string{"test/stuck-writer"}}}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithStatusSubresource(&mirrorv1alpha1.Mirror{}).WithObjects(m, deployment, service, route, job).Build()
	r := &MirrorReconciler{Client: c, Scheme: testScheme(t), Config: testConfig(), SyncLimiter: NewSyncLimiter(1)}
	for range 2 {
		reconcile(t, t.Context(), r, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(m)})
	}
	for _, obj := range []client.Object{deployment, service, route} {
		assertNotFound(t, t.Context(), c, client.ObjectKeyFromObject(obj), obj)
	}
	m = getMirror(t, t.Context(), c, client.ObjectKeyFromObject(m))
	if m.Status.CurrentSync == nil || m.Status.CurrentSync.Phase != mirrorv1alpha1.SyncPhaseCancelling || !m.AbortRequested() || r.SyncLimiter.Held() != 1 {
		t.Fatal("shutdown must preserve unfinished cancellation and its concurrency slot")
	}
	if ready := findCondition(m.Status.Conditions, conditionReady); ready == nil || ready.Status != metav1.ConditionFalse {
		t.Fatal("disabled service still reports Ready during cancellation")
	}
	get(t, t.Context(), c, client.ObjectKeyFromObject(job), job)
	if job.DeletionTimestamp.IsZero() {
		t.Fatal("service shutdown prevented cancellation from deleting the Job")
	}
}

func TestMirrorDeletionPreservesForeignLabeledResources(t *testing.T) {
	m := testMirror()
	m.DeletionTimestamp = timePtr(time.Now())
	m.Finalizers = []string{MirrorFinalizer}
	owner := []metav1.OwnerReference{*metav1.NewControllerRef(m, mirrorv1alpha1.GroupVersion.WithKind("Mirror"))}
	foreignOwner := []metav1.OwnerReference{*metav1.NewControllerRef(m, mirrorv1alpha1.GroupVersion.WithKind("Mirror"))}
	foreignOwner[0].UID = "previous-mirror-uid"
	var objects []client.Object
	var owned, foreign []client.Object
	for _, prototype := range []client.Object{&batchv1.Job{}, &appsv1.Deployment{}, &corev1.PersistentVolumeClaim{}, &snapshotv1.VolumeSnapshot{}} {
		for _, name := range []string{"owned", "foreign", "unowned"} {
			obj := prototype.DeepCopyObject().(client.Object)
			obj.SetName(name)
			obj.SetNamespace(m.Namespace)
			obj.SetUID("child-uid")
			obj.SetLabels(map[string]string{MirrorLabel: childBase(m.Name)})
			switch name {
			case "owned":
				obj.SetOwnerReferences(owner)
				owned = append(owned, obj)
			case "foreign":
				obj.SetOwnerReferences(foreignOwner)
				foreign = append(foreign, obj)
			default:
				foreign = append(foreign, obj)
			}
			objects = append(objects, obj)
		}
	}
	objects = append(objects, m)
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objects...).Build()
	recorded := &recordDeleteOptionsClient{Client: c, options: map[string]client.DeleteOptions{}}
	r := &MirrorReconciler{Client: recorded}
	for range 4 {
		reconcile(t, t.Context(), r, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(m)})
		for _, options := range recorded.options {
			if options.Preconditions == nil || options.Preconditions.UID == nil || *options.Preconditions.UID != "child-uid" {
				t.Fatal("deletion did not protect against replacement of the observed child")
			}
		}
	}
	for _, obj := range owned {
		assertNotFound(t, t.Context(), c, client.ObjectKeyFromObject(obj), obj)
	}
	for _, obj := range foreign {
		get(t, t.Context(), c, client.ObjectKeyFromObject(obj), obj)
	}
	assertNotFound(t, t.Context(), c, client.ObjectKeyFromObject(m), m)
}
