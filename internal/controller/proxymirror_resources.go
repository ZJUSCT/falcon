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
	// ProxyCacheRoleLabel is the component label value for the cache PVC
	// child.
	ProxyCacheRoleLabel = "proxy-cache"
	// ProxyCacheVolumeName is the reserved name of the cache PVC volume the
	// controller injects into the proxy pod's spec.volumes when the cache is
	// enabled. Users must not declare a volume of this name themselves;
	// mounting it, and where, is the user's own declaration (the nginx
	// proxy_cache conventional directory is /var/cache/nginx/proxy).
	ProxyCacheVolumeName = "proxy-cache"
)

// ensureCachePVC provisions the cache PVC when spec.proxy.cache.enabled is
// true. The claim grows on capacity increases, mirroring ensureSyncPVC.
func (r *ProxyMirrorReconciler) ensureCachePVC(ctx context.Context, proxy *mirrorv1alpha1.ProxyMirror) error {
	if !proxyCacheEnabled(proxy) {
		return nil
	}
	base := childBase(proxy.Name)
	name := resourceName(base, "cache")
	claim := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Namespace: proxy.Namespace, Name: name}}
	if err := r.Get(ctx, types.NamespacedName{Namespace: proxy.Namespace, Name: name}, claim); err == nil {
		if !claim.DeletionTimestamp.IsZero() {
			return nil
		}
		current := claim.Spec.Resources.Requests[corev1.ResourceStorage]
		if current.Cmp(proxy.Spec.Proxy.Cache.Size) < 0 {
			before := claim.DeepCopy()
			claim.Spec.Resources.Requests[corev1.ResourceStorage] = proxy.Spec.Proxy.Cache.Size.DeepCopy()
			return r.Patch(ctx, claim, client.MergeFrom(before))
		}
		return nil
	} else if !apierrors.IsNotFound(err) {
		return err
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
		return err
	}
	return r.Create(ctx, claim)
}

func (r *ProxyMirrorReconciler) cleanupDisabledProxyChildren(ctx context.Context, proxy *mirrorv1alpha1.ProxyMirror) error {
	if proxy.Spec.Publish.HTTP == nil {
		if err := deletePublishEntry(ctx, r.Client, proxy, PublishProtocolHTTP); err != nil {
			return err
		}
	}
	if proxy.Spec.Publish.HTTP == nil || !r.Config.PublishEnabled() {
		if err := deletePublishRouteFor(ctx, r.Client, proxy); err != nil {
			return err
		}
	}
	if proxy.Spec.Publish.HTTP == nil || !proxyCacheEnabled(proxy) {
		claim := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
			Namespace: proxy.Namespace,
			Name:      resourceName(childBase(proxy.Name), "cache"),
		}}
		if err := r.Get(ctx, client.ObjectKeyFromObject(claim), claim); apierrors.IsNotFound(err) {
			return nil
		} else if err != nil {
			return err
		} else if metav1.IsControlledBy(claim, proxy) {
			if err := r.Delete(ctx, claim); err != nil && !apierrors.IsNotFound(err) {
				return err
			}
		}
	}
	return nil
}

// ensureProxyPublish maintains the Deployment and Service of the ENABLED
// spec.publish.http entry of a ProxyMirror (the key is enabled when present).
// It mirrors the Mirror publish entry, minus the data volume: a proxy has no
// snapshot-derived data PVC — only the optional writable cache (injected as
// the reserved `proxy-cache` volume, volumes only, when enabled). The pair is
// named `<base>-publish-http` with per-service pod labels. It reports the
// Deployment's rollout readiness.
func (r *ProxyMirrorReconciler) ensureProxyPublish(ctx context.Context, proxy *mirrorv1alpha1.ProxyMirror) (bool, error) {
	http := proxy.Spec.Publish.HTTP
	if http == nil {
		return true, nil
	}
	base := childBase(proxy.Name)
	ready, err := r.ensureProxyPublishEntry(ctx, proxy, base, http)
	if err != nil {
		return false, err
	}
	return ready, nil
}

// ensureProxyPublishEntry maintains the http publish entry of a ProxyMirror:
// Deployment/Service `<base>-publish-http`, the optional cache volume
// (injected volumes-only when the cache is enabled) plus the default
// injections from applyPublishPodDefaults, and per-service pod labels.
func (r *ProxyMirrorReconciler) ensureProxyPublishEntry(ctx context.Context, proxy *mirrorv1alpha1.ProxyMirror, base string, service *mirrorv1alpha1.ProxyMirrorServiceSpec) (bool, error) {
	role := publishRole(PublishProtocolHTTP)

	template := service.PodTemplate.DeepCopy()
	if template.Labels == nil {
		template.Labels = map[string]string{}
	}
	for label, value := range map[string]string{MirrorLabel: base, ComponentLabel: role} {
		template.Labels[label] = value
	}
	spec := &template.Spec

	// A ProxyMirror has no placement fields: scheduling is the user's choice.

	applyPublishPodDefaults(spec, PublishProtocolHTTP)

	// The cache PVC is injected when enabled — as a VOLUME only (writable:
	// it IS a cache). Mounting it, and where, is the user's own declaration.
	if proxyCacheEnabled(proxy) {
		cacheName := resourceName(base, "cache")
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
		ComponentLabel:                 role,
	}
}
