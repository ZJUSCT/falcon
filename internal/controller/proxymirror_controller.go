package controller

import (
	"context"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	mirrorv1alpha1 "github.com/ZJUSCT/falcon/api/v1alpha1"
	"github.com/ZJUSCT/falcon/internal/config"
)

// ProxyMirrorReconciler drives publish-only proxy mirrors. Unlike Mirror it
// has no sync Job, no sync PVC and no snapshot lifecycle: it ensures a cache
// PVC (optional), the http publish Service and Deployment, the publish
// HTTPRoute, and reports their combined readiness through conditions.
// Cleanup of children relies purely on owner-reference GC, so no finalizer is
// needed.
type ProxyMirrorReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
	Now      func() time.Time
	// Config is the loaded controller configuration (required). The publish
	// section (config publish.*) gates publish HTTPRoute generation.
	Config *config.Config
}

func (r *ProxyMirrorReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&mirrorv1alpha1.ProxyMirror{}).
		Owns(&corev1.PersistentVolumeClaim{}).
		Owns(&corev1.Service{}).
		Owns(&appsv1.Deployment{}).
		Owns(&gatewayv1.HTTPRoute{}).
		Named("proxymirror").
		Complete(r)
}

func (r *ProxyMirrorReconciler) Reconcile(ctx context.Context, req ctrl.Request) (result ctrl.Result, reconcileErr error) {
	logger := log.FromContext(ctx)
	proxy := &mirrorv1alpha1.ProxyMirror{}
	if err := r.Get(ctx, req.NamespacedName, proxy); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Children are garbage-collected through owner references; nothing to do.
	if !proxy.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}
	defer func() {
		result, reconcileErr = r.handleDerivedResourceInvalid(ctx, proxy, result, reconcileErr)
	}()

	// Disabling HTTP or cache must take effect even if another field in the
	// updated spec is invalid.
	if err := r.cleanupDisabledProxyChildren(ctx, proxy); err != nil {
		return ctrl.Result{}, err
	}

	if errs := validateProxyMirror(proxy); len(errs) > 0 {
		message := errs.ToAggregate().Error()
		logger.Info("ProxyMirror specification is invalid", "errors", message)
		return r.patchStatus(ctx, proxy, func() {
			proxy.Status.ObservedGeneration = proxy.Generation
			setProxyCondition(proxy, conditionReady, conditionStatus(proxy.Spec.Publish.HTTP != nil && proxyWasReady(proxy)), "InvalidSpec", message)
			setProxyCondition(proxy, conditionProgressing, metav1.ConditionFalse, "InvalidSpec", message)
			setProxyCondition(proxy, conditionDegraded, metav1.ConditionTrue, "InvalidSpec", message)
		})
	}
	if proxy.Spec.Publish.HTTP == nil {
		return r.patchStatus(ctx, proxy, func() {
			proxy.Status.ObservedGeneration = proxy.Generation
			setProxyCondition(proxy, conditionReady, metav1.ConditionFalse, "HTTPDisabled", "spec.publish.http is not configured")
			setProxyCondition(proxy, conditionProgressing, metav1.ConditionFalse, "HTTPDisabled", "no publish service is requested")
			setProxyCondition(proxy, conditionDegraded, metav1.ConditionFalse, "HTTPDisabled", "")
		})
	}
	if err := r.ensureCachePVC(ctx, proxy); err != nil {
		return ctrl.Result{}, err
	}
	if !r.Config.PublishEnabled() {
		return r.patchStatus(ctx, proxy, func() {
			proxy.Status.ObservedGeneration = proxy.Generation
			setProxyCondition(proxy, conditionReady, metav1.ConditionFalse, "HTTPRouteDisabled", "HTTP publishing is disabled by controller configuration")
			setProxyCondition(proxy, conditionProgressing, metav1.ConditionFalse, "HTTPRouteDisabled", "")
			setProxyCondition(proxy, conditionDegraded, metav1.ConditionTrue, "HTTPRouteDisabled", "HTTP publishing is requested but route generation is disabled")
		})
	}

	// A ProxyMirror has no paused concept: the reconciler always ensures the
	// declared HTTP service; removing services.http takes it offline.
	deploymentReady, err := r.ensureProxyPublish(ctx, proxy)
	if err != nil {
		return ctrl.Result{}, err
	}
	if err := ensureReadyProxyRoute(ctx, r, proxy); err != nil {
		return ctrl.Result{}, err
	}
	routeState, routeMessage, err := publishRouteHealth(ctx, r.Client, proxy)
	if err != nil {
		return ctrl.Result{}, err
	}
	if routeState == publishRouteRejected {
		if r.Recorder != nil {
			r.Recorder.Event(proxy, corev1.EventTypeWarning, "HTTPRouteRejected", routeMessage)
		}
		return r.patchStatusWithResult(ctx, proxy, ctrl.Result{RequeueAfter: time.Minute}, func() {
			proxy.Status.ObservedGeneration = proxy.Generation
			setProxyCondition(proxy, conditionReady, metav1.ConditionFalse, "HTTPRouteRejected", routeMessage)
			setProxyCondition(proxy, conditionProgressing, metav1.ConditionFalse, "HTTPRouteRejected", routeMessage)
			setProxyCondition(proxy, conditionDegraded, metav1.ConditionTrue, "HTTPRouteRejected", routeMessage)
		})
	}
	if !deploymentReady || routeState == publishRoutePending {
		message := routeMessage
		if !deploymentReady {
			message = "waiting for the proxy Deployment to become available"
		}
		return r.patchStatusWithResult(ctx, proxy, ctrl.Result{RequeueAfter: 5 * time.Second}, func() {
			proxy.Status.ObservedGeneration = proxy.Generation
			setProxyCondition(proxy, conditionReady, metav1.ConditionFalse, "PublishProgressing", message)
			setProxyCondition(proxy, conditionProgressing, metav1.ConditionTrue, "PublishProgressing", message)
			setProxyCondition(proxy, conditionDegraded, metav1.ConditionFalse, "PublishRollout", "")
		})
	}
	return r.patchStatus(ctx, proxy, func() {
		proxy.Status.ObservedGeneration = proxy.Generation
		setProxyCondition(proxy, conditionReady, metav1.ConditionTrue, "Published", "the proxy Deployment and HTTPRoute are available")
		setProxyCondition(proxy, conditionProgressing, metav1.ConditionFalse, "Published", "the proxy Deployment and HTTPRoute are available")
		setProxyCondition(proxy, conditionDegraded, metav1.ConditionFalse, "Publish", "")
	})
}

func (r *ProxyMirrorReconciler) patchStatus(ctx context.Context, proxy *mirrorv1alpha1.ProxyMirror, mutate func()) (ctrl.Result, error) {
	return r.patchStatusWithResult(ctx, proxy, ctrl.Result{}, mutate)
}

func (r *ProxyMirrorReconciler) patchStatusWithResult(ctx context.Context, proxy *mirrorv1alpha1.ProxyMirror, result ctrl.Result, mutate func()) (ctrl.Result, error) {
	before := proxy.DeepCopy()
	mutate()
	if err := r.Status().Patch(ctx, proxy, client.MergeFrom(before)); err != nil {
		return ctrl.Result{}, err
	}
	return result, nil
}

func setProxyCondition(proxy *mirrorv1alpha1.ProxyMirror, conditionType string, status metav1.ConditionStatus, reason, message string) {
	meta.SetStatusCondition(&proxy.Status.Conditions, metav1.Condition{
		Type:               conditionType,
		Status:             status,
		ObservedGeneration: proxy.Generation,
		Reason:             reason,
		Message:            message,
	})
}

func validateProxyMirror(proxy *mirrorv1alpha1.ProxyMirror) field.ErrorList {
	path := field.NewPath("spec")
	var errs field.ErrorList
	// Only an ENABLED http service is validated (an absent key may park
	// nothing — absent = disabled).
	if http := proxy.Spec.Publish.HTTP; http != nil {
		errs = append(errs, validatePublishPodTemplate(&http.PodTemplate,
			path.Child("services", "http", "podTemplate"), ProxyCacheVolumeName)...)
	}
	if proxyCacheEnabled(proxy) {
		if proxy.Spec.Proxy.Cache.StorageClassName == "" {
			errs = append(errs, field.Required(path.Child("proxy", "cache", "storageClassName"), "must not be empty when cache is enabled"))
		}
		if proxy.Spec.Proxy.Cache.Size.IsZero() || proxy.Spec.Proxy.Cache.Size.Sign() < 0 {
			errs = append(errs, field.Invalid(path.Child("proxy", "cache", "size"), proxy.Spec.Proxy.Cache.Size.String(), "must be greater than zero when cache is enabled"))
		}
	}
	return errs
}

func proxyCacheEnabled(proxy *mirrorv1alpha1.ProxyMirror) bool {
	return proxy.Spec.Proxy.Cache.Enabled != nil && *proxy.Spec.Proxy.Cache.Enabled
}
