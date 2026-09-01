package v1alpha1

import (
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
// front of the upstream. The controller provisions and mounts the cache PVC;
// the container image is responsible for pointing proxy_cache at the mounted
// directory (CacheMountPath).
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

// ProxyMirrorSpec groups the proxy mirror configuration. ProxyMirror has no
// paused concept (unlike Mirror): to take a proxy mirror offline, delete the CR.
type ProxyMirrorSpec struct {
	Info  ProxyMirrorInfo      `json:"info"`
	Proxy ProxyMirrorProxySpec `json:"proxy,omitempty"`
	// Services uses the same shape as Mirror's spec.services: each entry gets
	// Deployment/Service `<name>-publish-<service name>`, and only an "http"
	// entry additionally gets the publish HTTPRoute (typically the single
	// "http" proxy service). Absent or empty services = the proxy is not
	// publishing (nothing is deployed). There is no data volume to mount: the
	// optional cache PVC is mounted at ProxyCacheMountPath instead. Service
	// names must be unique.
	// +optional
	// Same MaxItems rationale as Mirror.spec.services: bounds the uniqueness
	// CEL rule's estimated cost below the API server's CRD schema budget.
	// +kubebuilder:validation:MaxItems=3
	// +kubebuilder:validation:XValidation:rule="self.all(s, self.exists_one(e, e.name == s.name))",message="service names must be unique"
	Services []MirrorServingService `json:"services,omitempty"`
}

type ProxyMirrorStatus struct {
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// +kubebuilder:validation:Enum=Pending;Ready;Degraded
	Phase string `json:"phase,omitempty"`
	// PublishedServiceName is the Service fronting the http-type proxy
	// service (<name>-publish-http); empty when no "http" service is declared
	// (such a proxy is reachable only through its raw Services).
	PublishedServiceName string `json:"publishedServiceName,omitempty"`
	// CachePVC is the cache backing PVC when spec.proxy.cache.enabled is true.
	CachePVC   string             `json:"cachePVC,omitempty"`
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Cache PVC",type=string,JSONPath=`.status.cachePVC`
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
