package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ProxyMirrorInfo carries the catalog metadata of a publish-only mirror.
// Unlike MirrorInfo, it has no `type` field: the CRD kind itself is the type
// ("proxy"). `upstream` deliberately lives here instead of under spec.proxy,
// mirroring Mirror.info where the upstream origin already resides; spec.proxy
// then only holds the proxy-specific knobs (caching).
type ProxyMirrorInfo struct {
	Name        LocalizedString `json:"name"`
	Description LocalizedString `json:"description"`
	// Upstream is the backend origin the proxy forwards cache misses to,
	// e.g. https://pypi.org/simple/. The public path is the CR name
	// (publish route PathPrefix /<name>), so there is no URL field.
	Upstream string `json:"upstream"`
}

// ProxyMirrorCacheSpec configures the nginx proxy_cache-shaped disk cache in
// front of the upstream. The controller provisions the cache PVC and injects
// it as the reserved `proxy-cache` volume (a WRITABLE volume source); the
// user mounts it in the pod template — the nginx proxy_cache conventional
// directory /var/cache/nginx/proxy is applied by the image/user, the
// controller generates no nginx configuration and no mounts.
type ProxyMirrorCacheSpec struct {
	// +kubebuilder:default=false
	Enabled *bool `json:"enabled,omitempty"`
	// StorageClassName provisions the cache PVC via a standard StorageClass.
	// Required when Enabled is true; validated by the controller (Degraded).
	// +optional
	StorageClassName string `json:"storageClassName,omitempty"`
	// Size is the requested cache PVC capacity. Required when Enabled is true.
	// +optional
	Size resource.Quantity `json:"size,omitempty"`
}

// ProxyMirrorProxySpec groups proxy-specific behaviour. Cache bypass lists and
// other fine-tuning are intentionally not modelled yet: the controller does
// not generate nginx configuration, so a bypass field would be dead schema.
type ProxyMirrorProxySpec struct {
	Cache ProxyMirrorCacheSpec `json:"cache,omitempty"`
}

// ProxyMirrorServiceSpec is the HTTP publish service of a ProxyMirror,
// addressed by the fixed "http" key under spec.publish. The key is enabled
// when it appears (a valid enable requires podTemplate.spec, CEL-enforced) and disabled when absent. A proxy
// has no data volume to mount; when the cache is enabled the WRITABLE
// `proxy-cache` PVC volume is injected into spec.volumes only — mounting it,
// and where, is the user's own declaration.
// +kubebuilder:validation:XValidation:rule="has(self.podTemplate.spec)",message="podTemplate.spec is required when the service key is declared"
type ProxyMirrorServiceSpec struct {
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=3
	Replicas *int32 `json:"replicas,omitempty"`
	// PodTemplate is the FULL pod template of the publish Deployment
	// (Deployment .spec.template). The controller forces the naming/label/
	// selector identity and injects defaults only where the template is
	// silent (TCP readiness probe on the first container port, a /tmp
	// emptyDir, readOnlyRootFilesystem and the restricted-profile security
	// defaults); the optional cache PVC is still provisioned and injected as
	// the reserved `proxy-cache` volume (volumes only — no mount) when
	// spec.proxy.cache.enabled is true.
	// +optional
	PodTemplate corev1.PodTemplateSpec `json:"podTemplate,omitempty"`
}

// ProxyMirrorServicesSpec holds the fixed publish service keys of a
// ProxyMirror: only "http" — a proxy is an HTTP publisher by definition. The
// key is enabled when present and disabled when absent.
type ProxyMirrorServicesSpec struct {
	HTTP *ProxyMirrorServiceSpec `json:"http,omitempty"`
}

// ProxyMirrorSpec groups the proxy mirror configuration. ProxyMirror has no
// paused concept; removing publish.http takes its endpoint
// offline while preserving the CR.
type ProxyMirrorSpec struct {
	Info  ProxyMirrorInfo      `json:"info"`
	Proxy ProxyMirrorProxySpec `json:"proxy,omitempty"`
	// Publish declares the publish workload through the fixed "http" key
	// (see ProxyMirrorServicesSpec). With the key absent (including an
	// entirely absent services object) nothing is deployed: the proxy is not
	// published. An enabled http service gets Deployment/Service
	// `<name>-publish-http` plus the publish HTTPRoute (once Ready).
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
