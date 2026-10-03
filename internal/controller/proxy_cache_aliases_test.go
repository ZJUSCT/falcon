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
	p.Spec.Publish.Aliases = []mirrorv1alpha1.MirrorAlias{
		{Path: "/PyPI"},
		{Path: "/packages/python"},
		{Path: "/pypi-subset", Subset: &mirrorv1alpha1.MirrorAliasSubset{SubPath: "simple"}},
	}
	if errs := validateMirror(p); len(errs) != 0 {
		t.Fatal(errs)
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(p).Build()
	r := &MirrorReconciler{Client: c, Scheme: testScheme(t), Config: testConfig()}
	if err := ensurePublishedMirrorRoute(t.Context(), r, p); err != nil {
		t.Fatal(err)
	}
	route := &gatewayv1.HTTPRoute{}
	get(t, t.Context(), c, client.ObjectKey{Namespace: p.Namespace, Name: "pypi-proxy-publish"}, route)
	if len(route.Spec.Rules) != 4 || len(route.Spec.Rules[0].Matches) != 1 || *route.Spec.Rules[0].Matches[0].Path.Value != "/pypi-proxy" {
		t.Fatalf("proxy canonical rule not rendered: %#v", route.Spec.Rules)
	}
	for i, want := range []string{"/PyPI", "/packages/python", "/pypi-subset"} {
		alias := route.Spec.Rules[i+1]
		if len(alias.Matches) != 1 || *alias.Matches[0].Path.Value != want {
			t.Fatalf("proxy alias rule %d not rendered: %#v", i, alias)
		}
		if len(alias.BackendRefs) != 0 || len(alias.Filters) != 1 {
			t.Fatalf("proxy alias must redirect without a backend: %#v", alias)
		}
		target := "/pypi-proxy"
		if want == "/pypi-subset" {
			target += "/simple"
		}
		redirect := alias.Filters[0].RequestRedirect
		if redirect == nil || redirect.Path == nil || *redirect.StatusCode != 301 || *redirect.Path.ReplacePrefixMatch != target {
			t.Fatalf("proxy alias must permanently redirect to the canonical path: %#v", alias)
		}
	}
	invalid := [][]mirrorv1alpha1.MirrorAlias{
		{{Path: "/pypi-proxy"}},
		{{Path: "/duplicate"}, {Path: "/duplicate"}},
		{{Path: "missing-slash"}},
		{{Path: "/trailing/"}},
		// Subsets share the same path and catalog-name validation.
		{{Path: "/pypi-subset", Subset: &mirrorv1alpha1.MirrorAliasSubset{SubPath: "../simple"}}},
	}
	for _, aliases := range invalid {
		p.Spec.Publish.Aliases = aliases
		if errs := validateMirror(p); len(errs) == 0 {
			t.Fatalf("invalid aliases accepted: %v", aliases)
		}
	}
}

func TestProxyCachePresenceControlsStorage(t *testing.T) {
	p := testProxyMirror()
	p.Spec.Publish.HTTP = nil
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(p).Build()
	r := &MirrorReconciler{Client: c, Scheme: testScheme(t), Config: testConfig()}
	if err := r.ensureProxyCachePVC(t.Context(), p); err != nil {
		t.Fatal(err)
	}
	key := client.ObjectKey{Namespace: p.Namespace, Name: "pypi-proxy-cache"}
	get(t, t.Context(), c, key, &corev1.PersistentVolumeClaim{})
	if err := r.cleanupProxyChildren(t.Context(), p); err != nil {
		t.Fatal(err)
	}
	get(t, t.Context(), c, key, &corev1.PersistentVolumeClaim{})
	p.Spec.Storage.PVCSpec = corev1.PersistentVolumeClaimSpec{}
	if errs := validateMirror(p); len(errs) == 0 {
		t.Fatal("cache must require a usable PVC template even without HTTP")
	}
}
