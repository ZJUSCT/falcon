package controller

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	mirrorv1alpha1 "github.com/ZJUSCT/falcon/api/v1alpha1"
)

func TestProxyAliasesShareMirrorValidationAndRouteBehavior(t *testing.T) {
	p := testProxyMirror()
	p.Spec.Publish.HTTP.Aliases = []mirrorv1alpha1.MirrorHTTPAlias{"/PyPI", "/packages/python"}
	if errs := validateProxyMirror(p); len(errs) != 0 {
		t.Fatal(errs)
	}
	c := fake.NewClientBuilder().WithScheme(testProxyScheme(t)).WithObjects(p).Build()
	r := &ProxyMirrorReconciler{Client: c, Scheme: testProxyScheme(t), Config: testConfig()}
	if err := ensureReadyProxyRoute(t.Context(), r, p); err != nil {
		t.Fatal(err)
	}
	route := &gatewayv1.HTTPRoute{}
	get(t, t.Context(), c, client.ObjectKey{Namespace: p.Namespace, Name: "pypi-proxy-publish"}, route)
	matches := route.Spec.Rules[0].Matches
	if len(matches) != 3 || *matches[0].Path.Value != "/pypi-proxy" || *matches[1].Path.Value != "/PyPI" || *matches[2].Path.Value != "/packages/python" {
		t.Fatalf("proxy aliases not rendered: %#v", matches)
	}
	for _, aliases := range [][]mirrorv1alpha1.MirrorHTTPAlias{{"/pypi-proxy"}, {"/duplicate", "/duplicate"}, {"missing-slash"}, {"/trailing/"}} {
		p.Spec.Publish.HTTP.Aliases = aliases
		if errs := validateProxyMirror(p); len(errs) == 0 {
			t.Fatalf("invalid aliases accepted: %v", aliases)
		}
	}
}

func TestProxyCachePresenceControlsStorage(t *testing.T) {
	p := testProxyMirror()
	p.Spec.Publish.HTTP = nil
	c := fake.NewClientBuilder().WithScheme(testProxyScheme(t)).WithObjects(p).Build()
	r := &ProxyMirrorReconciler{Client: c, Scheme: testProxyScheme(t), Config: testConfig()}
	if err := r.ensureCachePVC(t.Context(), p); err != nil {
		t.Fatal(err)
	}
	key := client.ObjectKey{Namespace: p.Namespace, Name: "pypi-proxy-cache"}
	get(t, t.Context(), c, key, &corev1.PersistentVolumeClaim{})
	p.Spec.Cache = nil
	if errs := validateProxyMirror(p); len(errs) != 0 {
		t.Fatal(errs)
	}
	if err := r.cleanupDisabledProxyChildren(t.Context(), p); err != nil {
		t.Fatal(err)
	}
	assertNotFound(t, t.Context(), c, key, &corev1.PersistentVolumeClaim{})
	p.Spec.Cache = &mirrorv1alpha1.ProxyMirrorCacheSpec{}
	if errs := validateProxyMirror(p); len(errs) == 0 {
		t.Fatal("present cache must require a usable PVC template even without HTTP")
	}
}
