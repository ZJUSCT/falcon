package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ProxyMirrorInfo uses the same catalog metadata as Mirror.
type ProxyMirrorInfo = MirrorInfo

// ProxyMirrorCacheSpec configures the nginx proxy_cache-shaped disk cache in
// front of the upstream. The controller provisions the cache PVC and injects
// it as the reserved `proxy-cache` volume (a WRITABLE volume source); the
// user mounts it in the pod template — the nginx proxy_cache conventional
// directory /var/cache/nginx/proxy is applied by the image/user, the
// controller generates no nginx configuration and no mounts.
type ProxyMirrorCacheSpec struct {
	// PVCSpec is the canonical Kubernetes PVC specification for the cache.
	// Falcon manages the claim name and rejects binding/source fields that do
	// not apply to this generated cache PVC.
	PVCSpec corev1.PersistentVolumeClaimSpec `json:"pvcTemplate"`
}

// ProxyMirrorServiceSpec shares the serving workload shape with Mirror's
// http service (always in serving mode; the CEL rule lives on
// MirrorHTTPServiceSpec).
type ProxyMirrorServiceSpec = MirrorHTTPServiceSpec

// ProxyMirrorServicesSpec mirrors the Mirror publish declarations at the
// proxy's publish level: the same plain and subset aliases, the same
// publish-level redirect (suppressing the proxy workload), and the fixed
// "http" key. Subsets share the parent's proxy backend and optional cache.
type ProxyMirrorServicesSpec struct {
	// Aliases are additional public path prefixes, consumed like Mirror's
	// (subset paths are served by the same proxy backend and optional cache).
	// +kubebuilder:validation:MaxItems=8
	// +optional
	Aliases []MirrorAlias `json:"aliases,omitempty"`
	// Redirect is the publish-level 302 switch, with the same semantics as
	// Mirror's: while set it suppresses the proxy workload entirely and the
	// publish HTTPRoute redirects every public path to this hostname.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`
	// +optional
	Redirect string                  `json:"redirect,omitempty"`
	HTTP     *ProxyMirrorServiceSpec `json:"http,omitempty"`
}

// RedirectActive reports the publish-level redirect target and whether the
// proxy is in redirect mode, with the same presence-based semantics as
// MirrorServicesSpec.RedirectActive.
func (s *ProxyMirrorServicesSpec) RedirectActive() (string, bool) {
	if s == nil || s.Redirect == "" {
		return "", false
	}
	return s.Redirect, true
}

// ProxyMirrorSpec groups the proxy mirror configuration. ProxyMirror has no
// paused concept; removing publish.http takes its endpoint
// offline while preserving the CR.
type ProxyMirrorSpec struct {
	Info ProxyMirrorInfo `json:"info"`
	// Cache provisions storage only; proxy behavior belongs to the workload configuration.
	// +optional
	Cache *ProxyMirrorCacheSpec `json:"cache,omitempty"`
	// Publish declares the publish facet through the "http" key plus the
	// cross-protocol declarations (see ProxyMirrorServicesSpec). With the key
	// absent and no redirect nothing is deployed: the proxy is not
	// published. A serving http key gets Deployment/Service
	// `<name>-publish-http` (dots in the CR name map to '-': Service names
	// are DNS-1035 labels and forbid dots) plus the publish HTTPRoute (once
	// Ready). An active publish-level redirect deploys no workload and
	// 302-redirects the public paths through the route instead. The
	// optional cache PVC keeps being maintained across a temporary
	// redirect, so switching back to serving reuses the cached data.
	// +optional
	Publish ProxyMirrorServicesSpec `json:"publish,omitempty"`
}

type ProxyMirrorStatus struct {
	ObservedGeneration int64              `json:"observedGeneration,omitempty"`
	Conditions         []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type ProxyMirror struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ProxyMirrorSpec   `json:"spec"`
	Status ProxyMirrorStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type ProxyMirrorList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ProxyMirror `json:"items"`
}
