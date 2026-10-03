package controller

import (
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	mirrorv1alpha1 "github.com/ZJUSCT/falcon/api/v1alpha1"
)

func proxyMirrorForTest(t *testing.T, cached bool) *mirrorv1alpha1.Mirror {
	t.Helper()
	m := testMirror()
	m.Spec.Sync = nil
	m.Spec.Storage = nil
	if cached {
		m.Spec.Storage = &mirrorv1alpha1.MirrorStorageSpec{
			PVCSpec: corev1.PersistentVolumeClaimSpec{
				AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
				Resources:   corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("20Gi")}},
			},
			CacheStorageClassName: "cache-class",
		}
	}
	return m
}

func TestMirrorProxyModesProvisionExpectedChildren(t *testing.T) {
	for _, cached := range []bool{false, true} {
		t.Run(map[bool]string{false: "proxy", true: "cache"}[cached], func(t *testing.T) {
			m := proxyMirrorForTest(t, cached)
			scheme := testScheme(t)
			c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&mirrorv1alpha1.Mirror{}, &appsv1.Deployment{}).WithObjects(m).Build()
			r := &MirrorReconciler{Client: c, Scheme: scheme, Config: testConfig()}
			req := reconcileRequest(client.ObjectKeyFromObject(m))
			reconcile(t, t.Context(), r, req) // finalizer
			reconcile(t, t.Context(), r, req) // proxy children

			deployment := &appsv1.Deployment{}
			get(t, t.Context(), c, client.ObjectKey{Namespace: m.Namespace, Name: publishChildName(m.Name, PublishProtocolHTTP)}, deployment)
			cacheVolume := findVolume(deployment.Spec.Template.Spec.Volumes, ProxyCacheVolumeName)
			if cached && (cacheVolume == nil || cacheVolume.PersistentVolumeClaim == nil || cacheVolume.PersistentVolumeClaim.ClaimName != "smoke-cache") {
				t.Fatalf("cache Mirror deployment did not inject smoke-cache: %#v", deployment.Spec.Template.Spec.Volumes)
			}
			if !cached && cacheVolume != nil {
				t.Fatalf("Proxy Mirror unexpectedly injected a cache volume: %#v", cacheVolume)
			}
			claim := &corev1.PersistentVolumeClaim{}
			err := c.Get(t.Context(), client.ObjectKey{Namespace: m.Namespace, Name: "smoke-cache"}, claim)
			if cached && err != nil {
				t.Fatalf("cache Mirror did not create cache PVC: %v", err)
			}
			if !cached && !apierrors.IsNotFound(err) {
				t.Fatalf("Proxy Mirror created an unexpected cache PVC: %v", err)
			}
			current := getMirror(t, t.Context(), c, client.ObjectKeyFromObject(m))
			if current.Status.CurrentSync != nil || current.Status.Sync.Phase != "" {
				t.Fatalf("proxy mode accepted synchronization state: %#v", current.Status)
			}
		})
	}
}

func TestMirrorProxyStorageClassFieldsAreImmutable(t *testing.T) {
	m := proxyMirrorForTest(t, true)
	scheme := testScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&mirrorv1alpha1.Mirror{}, &appsv1.Deployment{}).WithObjects(m).Build()
	r := &MirrorReconciler{Client: c, Scheme: scheme, Config: testConfig()}
	req := reconcileRequest(client.ObjectKeyFromObject(m))
	reconcile(t, t.Context(), r, req)
	reconcile(t, t.Context(), r, req)
	m = getMirror(t, t.Context(), c, client.ObjectKeyFromObject(m))
	m.Spec.Storage.CacheStorageClassName = "another-class"
	m.Generation++
	if err := c.Update(t.Context(), m); err != nil {
		t.Fatal(err)
	}
	reconcile(t, t.Context(), r, req)
	current := getMirror(t, t.Context(), c, client.ObjectKeyFromObject(m))
	condition := findCondition(current.Status.Conditions, conditionDegraded)
	if condition == nil || condition.Status != metav1.ConditionTrue || condition.Reason != "InvalidSpec" {
		t.Fatalf("StorageClass mutation was not rejected: %#v", current.Status)
	}
}
