package controller

import (
	"context"
	"fmt"
	"reflect"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	mirrorv1alpha1 "github.com/ZJUSCT/falcon/api/v1alpha1"
)

const (
	cacheUsageRefreshInterval = time.Hour
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

// reconcileProxyMode handles Cache Mirrors and Proxy Mirrors represented by a
// Mirror object. Proxy mode has no synchronization pipeline: it maintains an
// optional writable cache PVC, the HTTP proxy Deployment and Service, and the
// corresponding HTTPRoute.
func (r *MirrorReconciler) reconcileProxyMode(ctx context.Context, mirror *mirrorv1alpha1.Mirror) (result ctrl.Result, reconcileErr error) {
	cacheUsageRequeue := time.Duration(0)
	defer func() {
		if reconcileErr == nil && cacheUsageRequeue > 0 {
			result = requeueWithin(result, cacheUsageRequeue)
		}
	}()
	if err := r.cleanupProxyChildren(ctx, mirror); err != nil {
		return ctrl.Result{}, err
	}
	if errs := validateProxyMode(mirror); len(errs) > 0 {
		message := errs.ToAggregate().Error()
		return r.patchStatus(ctx, mirror, func() {
			mirror.Status.ObservedGeneration = mirror.Generation
			setCondition(mirror, conditionReady, conditionStatus(mirrorWasReady(mirror)), "InvalidSpec", message)
			setCondition(mirror, conditionProgressing, metav1.ConditionFalse, "InvalidSpec", message)
			setCondition(mirror, conditionDegraded, metav1.ConditionTrue, "InvalidSpec", message)
		})
	}
	if handled, err := r.enforceStorageClassFingerprint(ctx, mirror); err != nil || handled {
		return ctrl.Result{RequeueAfter: time.Second}, err
	}
	if err := r.ensureProxyCachePVC(ctx, mirror); err != nil {
		return ctrl.Result{}, err
	}
	var err error
	cacheUsageRequeue, err = r.refreshProxyCacheUsage(ctx, mirror)
	if err != nil {
		return ctrl.Result{}, err
	}

	redirecting := false
	if _, redirecting = mirror.Spec.Publish.RedirectActive(); redirecting {
		return r.reconcileMirrorProxyRedirect(ctx, mirror)
	}
	if mirror.Spec.Publish.HTTP == nil {
		drained, err := publishPodsDrained(ctx, r.Client, mirror)
		if err != nil {
			return ctrl.Result{}, err
		}
		result := ctrl.Result{}
		if !drained {
			result.RequeueAfter = 5 * time.Second
		}
		return r.patchStatusWithResult(ctx, mirror, result, func() {
			mirror.Status.ObservedGeneration = mirror.Generation
			applyMirrorConditions(mirror, publicationHealth{progressing: !drained, reason: conditionHTTPDisabled, message: "spec.publish.http is not configured"}, conditionHTTPDisabled, "no publish service is requested; waiting for any removed workloads to drain", nil)
		})
	}
	if !r.Config.PublishEnabled() {
		return r.patchStatus(ctx, mirror, func() {
			mirror.Status.ObservedGeneration = mirror.Generation
			setCondition(mirror, conditionReady, metav1.ConditionFalse, "HTTPRouteDisabled", "HTTP publishing is disabled by controller configuration")
			setCondition(mirror, conditionProgressing, metav1.ConditionTrue, "HTTPRouteDisabled", "")
			setCondition(mirror, conditionDegraded, metav1.ConditionTrue, "HTTPRouteDisabled", "HTTP publishing is requested but route generation is disabled")
		})
	}
	ready, err := r.ensureProxyPublish(ctx, mirror)
	if err != nil {
		return ctrl.Result{}, err
	}
	if err := ensurePublishedMirrorRoute(ctx, r, mirror); err != nil {
		return ctrl.Result{}, err
	}
	routeState, routeMessage, err := publishRouteHealth(ctx, r.Client, mirror)
	if err != nil {
		return ctrl.Result{}, err
	}
	failure, err := publishDeploymentFailure(ctx, r.Client, mirror, PublishProtocolHTTP)
	if err != nil {
		return ctrl.Result{}, err
	}
	deployment := &appsv1.Deployment{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: mirror.Namespace, Name: publishChildName(mirror.Name, PublishProtocolHTTP)}, deployment); err != nil {
		return ctrl.Result{}, err
	}
	available := deployment.Status.AvailableReplicas > 0
	drained, err := publishPodsDrained(ctx, r.Client, mirror)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !ready || !drained || routeState == publishRoutePending {
		message := routeMessage
		if !ready {
			message = "waiting for the proxy Deployment to become available"
		}
		return r.patchStatusWithResult(ctx, mirror, ctrl.Result{RequeueAfter: 5 * time.Second}, func() {
			mirror.Status.ObservedGeneration = mirror.Generation
			applyMirrorConditions(mirror, publicationHealth{ready: available && routeState == publishRouteReady, progressing: true, reason: "PublishProgressing", message: message, failure: failure}, "PublishProgressing", message, failure)
		})
	}
	return r.patchStatus(ctx, mirror, func() {
		mirror.Status.ObservedGeneration = mirror.Generation
		applyMirrorConditions(mirror, publicationHealth{ready: true, reason: "Published", message: "the proxy Deployment and HTTPRoute are available", failure: failure}, "Published", "the proxy Deployment and HTTPRoute are available", failure)
	})
}

// refreshProxyCacheUsage periodically records the kubelet-observed usage of a
// Cache Mirror's writable cache PVC. A cache has no immutable publication
// generation, so its size is refreshed independently of proxy readiness and
// the last successful measurement is retained when the kubelet has no usable
// report yet.
func (r *MirrorReconciler) refreshProxyCacheUsage(ctx context.Context, mirror *mirrorv1alpha1.Mirror) (time.Duration, error) {
	if !mirror.IsCacheMirror() || r.UsageReader == nil || mirror.Spec.Publish.HTTP == nil {
		return 0, nil
	}
	if _, redirecting := mirror.Spec.Publish.RedirectActive(); redirecting {
		return 0, nil
	}
	if r.Config != nil && !r.Config.PublishEnabled() {
		return 0, nil
	}
	now := r.now()
	if updated := mirror.Status.CacheSizeUpdatedAt; updated != nil {
		if remaining := updated.Time.Add(cacheUsageRefreshInterval).Sub(now); remaining > 0 {
			return remaining, nil
		}
	}
	pvcName := resourceName(childBase(mirror.Name), "cache")
	usage, ok := r.proxyCachePVCUsage(ctx, mirror, pvcName)
	if !ok {
		return activePVCUsageRetry, nil
	}
	if _, err := r.patchStatus(ctx, mirror, func() {
		mirror.Status.SizeBytes = usage
		mirror.Status.CacheSizeUpdatedAt = timePtr(now)
	}); err != nil {
		return 0, err
	}
	return cacheUsageRefreshInterval, nil
}

func (r *MirrorReconciler) reconcileMirrorProxyRedirect(ctx context.Context, mirror *mirrorv1alpha1.Mirror) (ctrl.Result, error) {
	if err := ensurePublishedMirrorRoute(ctx, r, mirror); err != nil {
		return ctrl.Result{}, err
	}
	routeState, routeMessage, err := publishRouteHealth(ctx, r.Client, mirror)
	if err != nil {
		return ctrl.Result{}, err
	}
	drained, err := publishPodsDrained(ctx, r.Client, mirror)
	if err != nil {
		return ctrl.Result{}, err
	}
	if routeState == publishRouteRejected {
		return r.patchStatusWithResult(ctx, mirror, ctrl.Result{RequeueAfter: time.Minute}, func() {
			mirror.Status.ObservedGeneration = mirror.Generation
			setCondition(mirror, conditionReady, metav1.ConditionFalse, "HTTPRouteRejected", routeMessage)
			setCondition(mirror, conditionProgressing, metav1.ConditionTrue, "HTTPRouteRejected", routeMessage)
			setCondition(mirror, conditionDegraded, metav1.ConditionTrue, "HTTPRouteRejected", routeMessage)
		})
	}
	if routeState == publishRoutePending || !drained {
		message := routeMessage
		if routeState == publishRouteReady {
			message = "waiting for removed proxy workloads to drain"
		}
		return r.patchStatusWithResult(ctx, mirror, ctrl.Result{RequeueAfter: 5 * time.Second}, func() {
			mirror.Status.ObservedGeneration = mirror.Generation
			applyMirrorConditions(mirror, publicationHealth{ready: false, progressing: true, reason: "RedirectProgressing", message: message}, "RedirectProgressing", message, nil)
		})
	}
	hostname, _ := mirror.Spec.Publish.RedirectActive()
	message := fmt.Sprintf("every public path redirects to %s (302)", hostname)
	return r.patchStatus(ctx, mirror, func() {
		mirror.Status.ObservedGeneration = mirror.Generation
		applyMirrorConditions(mirror, publicationHealth{ready: true, reason: "RedirectActive", message: message}, "RedirectActive", message, nil)
	})
}

func validateProxyMode(mirror *mirrorv1alpha1.Mirror) field.ErrorList {
	path := field.NewPath("spec")
	var errs field.ErrorList
	if mirror.Spec.Sync != nil {
		errs = append(errs, field.Forbidden(path.Child("sync"), "must be omitted for Cache Mirror and Proxy Mirror modes"))
	}
	storage := mirror.Spec.Storage
	if storage != nil {
		if storage.SyncStorageClassName != "" {
			errs = append(errs, field.Forbidden(path.Child("storage", "syncStorageClassName"), "must be empty in proxy mode"))
		}
		if storage.PublishStorageClassName != "" || storage.VolumeSnapshotClassName != "" {
			errs = append(errs, field.Forbidden(path.Child("storage"), "publishStorageClassName and volumeSnapshotClassName are only valid for synchronized Mirrors"))
		}
		if storage.CacheStorageClassName == "" {
			if !reflect.DeepEqual(storage.PVCSpec, corev1.PersistentVolumeClaimSpec{}) {
				errs = append(errs, field.Forbidden(path.Child("storage", "pvcTemplate"), "requires cacheStorageClassName"))
			}
		} else {
			errs = append(errs, validateCachePVC(storage, path.Child("storage", "pvcTemplate"))...)
		}
	}
	publish := mirror.Spec.Publish
	publishPath := path.Child("publish")
	if publish.Rsync != nil {
		errs = append(errs, field.Forbidden(publishPath.Child("rsync"), "proxy modes support only the http publish service"))
	}
	if publish.Redirect != "" {
		errs = append(errs, validateRedirectHostname(publish.Redirect, publishPath.Child("redirect"))...)
	}
	errs = append(errs, validateAliases(publish.Aliases, mirror.Name, mirror.Spec.Info.CName, publishPath.Child("aliases"))...)
	if publish.HTTP != nil {
		errs = append(errs, validatePublishPodTemplate(&publish.HTTP.PodTemplate, publishPath.Child("http", "podTemplate"), ProxyCacheVolumeName)...)
	}
	return errs
}

func validateCachePVC(storage *mirrorv1alpha1.MirrorStorageSpec, path *field.Path) field.ErrorList {
	var errs field.ErrorList
	if len(storage.PVCSpec.AccessModes) == 0 {
		errs = append(errs, field.Required(path.Child("accessModes"), "must declare at least one access mode when cache is enabled"))
	}
	requestedStorage := storage.PVCSpec.Resources.Requests[corev1.ResourceStorage]
	if requestedStorage.IsZero() || requestedStorage.Sign() < 0 {
		errs = append(errs, field.Required(path.Child("resources", "requests", string(corev1.ResourceStorage)), "must be greater than zero when cache is enabled"))
	}
	if storage.PVCSpec.StorageClassName != nil || storage.PVCSpec.DataSource != nil || storage.PVCSpec.DataSourceRef != nil || storage.PVCSpec.VolumeName != "" || storage.PVCSpec.Selector != nil {
		errs = append(errs, field.Invalid(path, storage.PVCSpec, "storageClassName, dataSource, dataSourceRef, volumeName, and selector are unsupported for the cache PVC"))
	}
	return errs
}

func (r *MirrorReconciler) ensureProxyCachePVC(ctx context.Context, mirror *mirrorv1alpha1.Mirror) error {
	if !mirror.IsCacheMirror() {
		return nil
	}
	base := childBase(mirror.Name)
	name := resourceName(base, "cache")
	claim := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Namespace: mirror.Namespace, Name: name}}
	if err := r.Get(ctx, client.ObjectKeyFromObject(claim), claim); err == nil {
		if !claim.DeletionTimestamp.IsZero() {
			return nil
		}
		current := claim.Spec.Resources.Requests[corev1.ResourceStorage]
		desired := mirror.Spec.Storage.PVCSpec.Resources.Requests[corev1.ResourceStorage]
		if current.Cmp(desired) < 0 {
			before := claim.DeepCopy()
			claim.Spec.Resources.Requests[corev1.ResourceStorage] = desired.DeepCopy()
			return r.Patch(ctx, claim, client.MergeFrom(before))
		}
		return nil
	} else if !apierrors.IsNotFound(err) {
		return err
	}
	claim = &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
		Namespace: mirror.Namespace,
		Name:      name,
		Labels:    objectLabels(base, ProxyCacheRoleLabel),
	}, Spec: *mirror.Spec.Storage.PVCSpec.DeepCopy()}
	claim.Spec.StorageClassName = stringPtr(mirror.Spec.Storage.CacheStorageClassName)
	if err := controllerutil.SetControllerReference(mirror, claim, r.Scheme); err != nil {
		return err
	}
	return r.Create(ctx, claim)
}

func (r *MirrorReconciler) cleanupProxyChildren(ctx context.Context, mirror *mirrorv1alpha1.Mirror) error {
	_, redirecting := mirror.Spec.Publish.RedirectActive()
	if redirecting || !mirror.Spec.Publish.HTTP.Serving() {
		if err := deletePublishEntry(ctx, r.Client, mirror, PublishProtocolHTTP); err != nil {
			return err
		}
	}
	if (!redirecting && mirror.Spec.Publish.HTTP == nil) || !r.Config.PublishEnabled() {
		if err := deletePublishRouteFor(ctx, r.Client, mirror); err != nil {
			return err
		}
	}
	return nil
}

func (r *MirrorReconciler) ensureProxyPublish(ctx context.Context, mirror *mirrorv1alpha1.Mirror) (bool, error) {
	service := mirror.Spec.Publish.HTTP
	if service == nil {
		return true, nil
	}
	base := childBase(mirror.Name)
	template := service.PodTemplate.DeepCopy()
	if template.Labels == nil {
		template.Labels = map[string]string{}
	}
	for label, value := range map[string]string{MirrorLabel: base, ComponentLabel: publishRole(PublishProtocolHTTP)} {
		template.Labels[label] = value
	}
	if mirror.IsCacheMirror() {
		template.Spec.Volumes = append(template.Spec.Volumes, corev1.Volume{
			Name: ProxyCacheVolumeName,
			VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
				ClaimName: resourceName(base, "cache"),
			}},
		})
	}
	return ensurePublishServiceAndDeployment(ctx, r.Client, r.Scheme, mirror, base, PublishProtocolHTTP, replicasOrDefault(service.Replicas), *template)
}

func objectLabels(base, role string) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":       "falcon",
		"app.kubernetes.io/managed-by": "falcon-controller",
		MirrorLabel:                    base,
		ComponentLabel:                 role,
	}
}
