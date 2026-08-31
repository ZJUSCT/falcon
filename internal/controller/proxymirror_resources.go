package controller

import (
	"context"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	mirrorv1alpha1 "github.com/ZJUSCT/falcon/api/v1alpha1"
)

const (
	// ProxyCacheRoleLabel is the role value for the cache PVC child.
	ProxyCacheRoleLabel = "proxy-cache"
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

// ensureProxyPublish maintains the Service and Deployment of every declared
// spec.services[] entry of a ProxyMirror. It mirrors the Mirror publish
// entries, minus the read-only data volume: a proxy has no snapshot-derived
// data PVC, only the optional writable cache (mounted where
// ProxyCacheMountPath points). Each entry gets Deployment/Service
// `<name>-publish-<protocol>` with per-service pod labels. It returns
// readiness, plus the name of the "http" service (empty when no "http" entry
// is declared — such a proxy gets no publish HTTPRoute and only its raw
// Services), so the caller can persist it inside a status patch. Absent or
// empty services = the proxy is not publishing (nothing is deployed).
func (r *ProxyMirrorReconciler) ensureProxyPublish(ctx context.Context, proxy *mirrorv1alpha1.ProxyMirror) (bool, string, error) {
	if len(proxy.Spec.Services) == 0 {
		return true, "", nil
	}
	base, err := childBase(proxy.Name)
	if err != nil {
		return false, "", err
	}
	ready := true
	httpServiceName := ""
	for i := range proxy.Spec.Services {
		ok, err := r.ensureProxyPublishEntry(ctx, proxy, base, &proxy.Spec.Services[i])
		if err != nil {
			return false, "", err
		}
		if !ok {
			ready = false
		}
		if proxy.Spec.Services[i].Name == PublishProtocolHTTP {
			httpServiceName, err = publishChildName(base, PublishProtocolHTTP)
			if err != nil {
				return false, "", err
			}
		}
	}
	return ready, httpServiceName, nil
}

// ensureProxyPublishEntry maintains one publish service entry of a
// ProxyMirror: Deployment/Service `<base>-publish-<protocol>`, cache volume
// (when enabled) plus the tmp EmptyDir, and per-service pod labels.
func (r *ProxyMirrorReconciler) ensureProxyPublishEntry(ctx context.Context, proxy *mirrorv1alpha1.ProxyMirror, base string, service *mirrorv1alpha1.MirrorServingService) (bool, error) {
	childName, err := publishChildName(base, service.Name)
	if err != nil {
		return false, err
	}
	role := publishRole(service.Name)
	appProtocol := publishAppProtocol(service.Name)
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: proxy.Namespace, Name: childName}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, svc, func() error {
		svc.Labels = objectLabels(base, role)
		svc.Spec.Selector = map[string]string{MirrorLabel: base, RoleLabel: role}
		svc.Spec.Ports = []corev1.ServicePort{{
			Name:        service.Name,
			Port:        publishServicePort,
			TargetPort:  intstr.FromString(service.Name),
			Protocol:    corev1.ProtocolTCP,
			AppProtocol: appProtocol,
		}}
		return controllerutil.SetControllerReference(proxy, svc, r.Scheme)
	}); err != nil {
		return false, err
	}

	replicas := serviceReplicas(service)
	pullPolicy := service.ImagePullPolicy
	if pullPolicy == "" {
		pullPolicy = corev1.PullIfNotPresent
	}
	readinessPath := service.ReadinessPath
	if readinessPath == "" {
		readinessPath = "/"
	}
	ports := make([]corev1.ContainerPort, len(service.Ports))
	copy(ports, service.Ports)
	// The first container port is the Service target; it is (re)named after
	// the service so the Service and the probes can reference it as a named
	// port ("http", "rsync" or "git").
	ports[0].Name = service.Name
	maxUnavailable := intstr.FromInt32(0)
	maxSurge := intstr.FromInt32(1)
	allowPrivilegeEscalation := false
	readOnlyRootFilesystem := true
	runAsNonRoot := true
	automountToken := false
	podLabels := map[string]string{MirrorLabel: base, RoleLabel: role}

	var mounts []corev1.VolumeMount
	var volumes []corev1.Volume
	if proxyCacheEnabled(proxy) {
		cacheName, err := resourceName(base, "cache")
		if err != nil {
			return false, err
		}
		mounts = append(mounts, corev1.VolumeMount{Name: "proxy-cache", MountPath: ProxyCacheMountPath})
		volumes = append(volumes, corev1.Volume{
			Name: "proxy-cache",
			VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
				ClaimName: cacheName,
			}},
		})
	}
	mounts = append(mounts, corev1.VolumeMount{Name: "tmp", MountPath: "/tmp"})
	volumes = append(volumes, corev1.Volume{Name: "tmp", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}})

	deployment := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: proxy.Namespace, Name: childName}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, deployment, func() error {
		deployment.Labels = objectLabels(base, role)
		deployment.Spec.Replicas = &replicas
		deployment.Spec.Strategy = appsv1.DeploymentStrategy{
			Type: appsv1.RollingUpdateDeploymentStrategyType,
			RollingUpdate: &appsv1.RollingUpdateDeployment{
				MaxUnavailable: &maxUnavailable,
				MaxSurge:       &maxSurge,
			},
		}
		deployment.Spec.Selector = &metav1.LabelSelector{MatchLabels: podLabels}
		deployment.Spec.Template = corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{Labels: podLabels},
			Spec: corev1.PodSpec{
				AutomountServiceAccountToken: &automountToken,
				SecurityContext: &corev1.PodSecurityContext{
					RunAsNonRoot:   &runAsNonRoot,
					SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
				},
				Containers: []corev1.Container{{
					Name:            "proxy",
					Image:           service.Image,
					ImagePullPolicy: pullPolicy,
					Command:         append([]string(nil), service.Command...),
					Args:            append([]string(nil), service.Args...),
					Ports:           ports,
					Resources:       *service.Resources.DeepCopy(),
					ReadinessProbe: &corev1.Probe{
						ProbeHandler:     corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: readinessPath, Port: intstr.FromString(service.Name)}},
						PeriodSeconds:    5,
						TimeoutSeconds:   2,
						FailureThreshold: 3,
					},
					LivenessProbe: &corev1.Probe{
						ProbeHandler:     corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: readinessPath, Port: intstr.FromString(service.Name)}},
						PeriodSeconds:    10,
						TimeoutSeconds:   2,
						FailureThreshold: 3,
					},
					SecurityContext: &corev1.SecurityContext{
						AllowPrivilegeEscalation: &allowPrivilegeEscalation,
						ReadOnlyRootFilesystem:   &readOnlyRootFilesystem,
						Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
					},
					VolumeMounts: mounts,
				}},
				Volumes: volumes,
			},
		}
		return controllerutil.SetControllerReference(proxy, deployment, r.Scheme)
	}); err != nil {
		return false, err
	}

	if deployment.Generation != deployment.Status.ObservedGeneration {
		return false, nil
	}
	return deployment.Status.AvailableReplicas >= replicas && deployment.Status.UpdatedReplicas >= replicas, nil
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
