package controller

import (
	"context"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	mirrorv1alpha1 "github.com/ZJUSCT/falcon/api/v1alpha1"
)

const (
	// ProxyCacheRoleLabel is the role value for the cache PVC child.
	ProxyCacheRoleLabel = "proxy-cache"
	// ProxyCacheVolumeName is the reserved name of the cache PVC volume the
	// controller injects into the proxy pod when the cache is enabled. Users
	// must not declare a volume of this name themselves (they may still add
	// their own mount of the injected volume at another path).
	ProxyCacheVolumeName = "proxy-cache"
	// ProxyCacheMountPath is where the cache PVC is mounted. The container
	// image must configure nginx proxy_cache to use this directory; the
	// controller intentionally does not generate nginx configuration.
	ProxyCacheMountPath = "/var/cache/nginx/proxy"
)

// ensureCachePVC provisions the cache PVC when spec.proxy.cache.enabled is
// true. The claim grows on capacity increases, mirroring ensureSyncPVC. It
// returns the claim name so the caller can persist it inside a status patch.
func (r *ProxyMirrorReconciler) ensureCachePVC(ctx context.Context, proxy *mirrorv1alpha1.ProxyMirror) (string, error) {
	if !proxyCacheEnabled(proxy) {
		return "", nil
	}
	base, err := childBase(proxy.Name)
	if err != nil {
		return "", err
	}
	name, err := resourceName(base, "cache")
	if err != nil {
		return "", err
	}
	claim := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Namespace: proxy.Namespace, Name: name}}
	if err := r.Get(ctx, types.NamespacedName{Namespace: proxy.Namespace, Name: name}, claim); err == nil {
		if !claim.DeletionTimestamp.IsZero() {
			return claim.Name, nil
		}
		current := claim.Spec.Resources.Requests[corev1.ResourceStorage]
		if current.Cmp(proxy.Spec.Proxy.Cache.Size) < 0 {
			before := claim.DeepCopy()
			claim.Spec.Resources.Requests[corev1.ResourceStorage] = proxy.Spec.Proxy.Cache.Size.DeepCopy()
			return claim.Name, r.Patch(ctx, claim, client.MergeFrom(before))
		}
		return claim.Name, nil
	} else if !apierrors.IsNotFound(err) {
		return "", err
	}

	claim = &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: proxy.Namespace,
			Name:      name,
			Labels:    objectLabels(base, ProxyCacheRoleLabel),
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			StorageClassName: stringPtr(proxy.Spec.Proxy.Cache.StorageClassName),
			Resources: corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{
				corev1.ResourceStorage: proxy.Spec.Proxy.Cache.Size.DeepCopy(),
			}},
		},
	}
	if err := controllerutil.SetControllerReference(proxy, claim, r.Scheme); err != nil {
		return "", err
	}
	return claim.Name, r.Create(ctx, claim)
}

// ensureProxyPublish maintains the Deployment and Service of the ENABLED
// spec.services.http entry of a ProxyMirror. It mirrors the Mirror publish
// entry, minus the data volume: a proxy has no snapshot-derived data PVC and
// therefore no mirrorMountPath — only the optional writable cache (injected at
// ProxyCacheMountPath when enabled). The pair is named `<base>-publish-http`
// with per-service pod labels. It returns readiness plus the name of the http
// Service (empty when the http service is disabled — a disabled proxy deploys
// nothing and gets no publish HTTPRoute), so the caller can persist both
// inside a status patch.
func (r *ProxyMirrorReconciler) ensureProxyPublish(ctx context.Context, proxy *mirrorv1alpha1.ProxyMirror) (bool, string, error) {
	http := proxy.Spec.Services.HTTP
	if !http.Enable {
		return true, "", nil
	}
	base, err := childBase(proxy.Name)
	if err != nil {
		return false, "", err
	}
	ready, err := r.ensureProxyPublishEntry(ctx, proxy, base, &http)
	if err != nil {
		return false, "", err
	}
	httpServiceName, err := publishChildName(base, PublishProtocolHTTP)
	if err != nil {
		return false, "", err
	}
	return ready, httpServiceName, nil
}

// ensureProxyPublishEntry maintains the http publish entry of a ProxyMirror:
// Deployment/Service `<base>-publish-http`, the optional cache volume
// (injected when the cache is enabled) plus the default injections from
// applyPublishPodDefaults, and per-service pod labels.
func (r *ProxyMirrorReconciler) ensureProxyPublishEntry(ctx context.Context, proxy *mirrorv1alpha1.ProxyMirror, base string, service *mirrorv1alpha1.ProxyMirrorServiceSpec) (bool, error) {
	role := publishRole(PublishProtocolHTTP)

	template := service.PodTemplate.DeepCopy()
	if template.Labels == nil {
		template.Labels = map[string]string{}
	}
	for label, value := range map[string]string{MirrorLabel: base, RoleLabel: role} {
		template.Labels[label] = value
	}
	spec := &template.Spec

	// A ProxyMirror has no placement fields: scheduling is the user's choice.

	applyPublishPodDefaults(spec, PublishProtocolHTTP)

	// The cache PVC is injected when enabled (read-write — it IS a cache).
	if proxyCacheEnabled(proxy) {
		cacheName, err := resourceName(base, "cache")
		if err != nil {
			return false, err
		}
		if len(spec.Containers) > 0 {
			mounted := false
			for i := range spec.Containers[0].VolumeMounts {
				mount := &spec.Containers[0].VolumeMounts[i]
				if mount.Name == ProxyCacheVolumeName && mount.MountPath == ProxyCacheMountPath {
					mounted = true
				}
			}
			if !mounted {
				spec.Containers[0].VolumeMounts = append(spec.Containers[0].VolumeMounts, corev1.VolumeMount{
					Name:      ProxyCacheVolumeName,
					MountPath: ProxyCacheMountPath,
				})
			}
		}
		spec.Volumes = append(spec.Volumes, corev1.Volume{
			Name: ProxyCacheVolumeName,
			VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
				ClaimName: cacheName,
			}},
		})
	}

	return ensurePublishServiceAndDeployment(ctx, r.Client, r.Scheme, proxy, base, PublishProtocolHTTP, replicasOrDefault(service.Replicas), *template)
}

// objectLabels builds the deterministic labels shared by Mirror and
// ProxyMirror children.
func objectLabels(base, role string) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":       "falcon",
		"app.kubernetes.io/managed-by": "falcon-controller",
		MirrorLabel:                    base,
		RoleLabel:                      role,
	}
}
