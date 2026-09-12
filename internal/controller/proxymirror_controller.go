package controller

import (
	"context"
	"fmt"
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
	if err := r.ensureCachePVC(ctx, proxy); err != nil {
		return ctrl.Result{}, err
	}
	if proxy.Spec.Publish.HTTP == nil {
		drained, err := publishPodsDrained(ctx, r.Client, proxy)
		if err != nil {
			return ctrl.Result{}, err
		}
		result := ctrl.Result{}
		if !drained {
			result.RequeueAfter = 5 * time.Second
		}
		return r.patchStatusWithResult(ctx, proxy, result, func() {
			proxy.Status.ObservedGeneration = proxy.Generation
			setProxyCondition(proxy, conditionReady, metav1.ConditionFalse, "HTTPDisabled", "spec.publish.http is not configured")
			setProxyCondition(proxy, conditionProgressing, conditionStatus(!drained), "HTTPDisabled", "no publish service is requested; waiting for any removed workloads to drain")
			setProxyCondition(proxy, conditionDegraded, metav1.ConditionFalse, "HTTPDisabled", "")
		})
	}
	if !r.Config.PublishEnabled() {
		return r.patchStatus(ctx, proxy, func() {
			proxy.Status.ObservedGeneration = proxy.Generation
			setProxyCondition(proxy, conditionReady, metav1.ConditionFalse, "HTTPRouteDisabled", "HTTP publishing is disabled by controller configuration")
			setProxyCondition(proxy, conditionProgressing, metav1.ConditionTrue, "HTTPRouteDisabled", "")
			setProxyCondition(proxy, conditionDegraded, metav1.ConditionTrue, "HTTPRouteDisabled", "HTTP publishing is requested but route generation is disabled")
		})
	}

	// A redirect-mode http key deploys no workload: the redirect route is the
	// entire endpoint. The optional cache PVC stays maintained above, so a
	// temporary redirect back to serving reuses the cached data.
	if hostname, redirecting := proxy.Spec.Publish.HTTP.RedirectActive(); redirecting {
		return r.reconcileProxyRedirect(ctx, proxy, hostname)
	}

	// A ProxyMirror has no paused concept: the reconciler always ensures the
	// declared HTTP service; removing services.http takes it offline.
	// A parked redirect next to a serving podTemplate is legal but inert:
	// say so, so a stale field cannot confuse an operator mid-incident.
	if http := proxy.Spec.Publish.HTTP; r.Recorder != nil && http.Serving() && http.Redirect != "" {
		r.Recorder.Event(proxy, corev1.EventTypeNormal, "RedirectIgnored",
			"publish.http.redirect is ignored while podTemplate serves")
	}
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
			setProxyCondition(proxy, conditionProgressing, metav1.ConditionTrue, "HTTPRouteRejected", routeMessage)
			setProxyCondition(proxy, conditionDegraded, metav1.ConditionTrue, "HTTPRouteRejected", routeMessage)
		})
	}
	failure, err := publishDeploymentFailure(ctx, r.Client, proxy, PublishProtocolHTTP)
	if err != nil {
		return ctrl.Result{}, err
	}
	deployment := &appsv1.Deployment{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: proxy.Namespace, Name: publishChildName(proxy.Name, PublishProtocolHTTP)}, deployment); err != nil {
		return ctrl.Result{}, err
	}
	available := deployment.Status.AvailableReplicas > 0
	drained, err := publishPodsDrained(ctx, r.Client, proxy)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !deploymentReady || !drained || routeState == publishRoutePending {
		message := routeMessage
		if !deploymentReady {
			message = "waiting for the proxy Deployment to become available"
		}
		return r.patchStatusWithResult(ctx, proxy, ctrl.Result{RequeueAfter: 5 * time.Second}, func() {
			proxy.Status.ObservedGeneration = proxy.Generation
			setProxyCondition(proxy, conditionReady, conditionStatus(available && routeState == publishRouteReady), "PublishProgressing", message)
			setProxyCondition(proxy, conditionProgressing, metav1.ConditionTrue, "PublishProgressing", message)
			if failure != nil {
				setProxyCondition(proxy, conditionDegraded, metav1.ConditionTrue, failure.reason, failure.message)
			} else {
				setProxyCondition(proxy, conditionDegraded, metav1.ConditionFalse, "PublishRollout", "")
			}
		})
	}
	return r.patchStatus(ctx, proxy, func() {
		proxy.Status.ObservedGeneration = proxy.Generation
		setProxyCondition(proxy, conditionReady, metav1.ConditionTrue, "Published", "the proxy Deployment and HTTPRoute are available")
		setProxyCondition(proxy, conditionProgressing, metav1.ConditionFalse, "Published", "the proxy Deployment and HTTPRoute are available")
		if failure != nil {
			setProxyCondition(proxy, conditionDegraded, metav1.ConditionTrue, failure.reason, failure.message)
		} else {
			setProxyCondition(proxy, conditionDegraded, metav1.ConditionFalse, "Publish", "")
		}
	})
}

func (r *ProxyMirrorReconciler) patchStatus(ctx context.Context, proxy *mirrorv1alpha1.ProxyMirror, mutate func()) (ctrl.Result, error) {
	return r.patchStatusWithResult(ctx, proxy, ctrl.Result{}, mutate)
}

// reconcileProxyRedirect drives a redirect-mode ProxyMirror: no Deployment or
// Service exists, so the redirect publish HTTPRoute is the entire endpoint.
// Its gateway acceptance (plus draining of any removed serving workloads)
// becomes Ready; rejections become Degraded, like the serving path.
func (r *ProxyMirrorReconciler) reconcileProxyRedirect(ctx context.Context, proxy *mirrorv1alpha1.ProxyMirror, hostname string) (ctrl.Result, error) {
	if err := ensureReadyProxyRoute(ctx, r, proxy); err != nil {
		return ctrl.Result{}, err
	}
	routeState, routeMessage, err := publishRouteHealth(ctx, r.Client, proxy)
	if err != nil {
		return ctrl.Result{}, err
	}
	drained, err := publishPodsDrained(ctx, r.Client, proxy)
	if err != nil {
		return ctrl.Result{}, err
	}
	switch {
	case routeState == publishRouteRejected:
		if r.Recorder != nil {
			r.Recorder.Event(proxy, corev1.EventTypeWarning, "HTTPRouteRejected", routeMessage)
		}
		return r.patchStatusWithResult(ctx, proxy, ctrl.Result{RequeueAfter: time.Minute}, func() {
			proxy.Status.ObservedGeneration = proxy.Generation
			setProxyCondition(proxy, conditionReady, metav1.ConditionFalse, "HTTPRouteRejected", routeMessage)
			setProxyCondition(proxy, conditionProgressing, metav1.ConditionTrue, "HTTPRouteRejected", routeMessage)
			setProxyCondition(proxy, conditionDegraded, metav1.ConditionTrue, "HTTPRouteRejected", routeMessage)
		})
	case routeState == publishRoutePending || !drained:
		message := routeMessage
		if routeState == publishRouteReady {
			message = "waiting for removed serving workloads to drain"
		}
		return r.patchStatusWithResult(ctx, proxy, ctrl.Result{RequeueAfter: 5 * time.Second}, func() {
			proxy.Status.ObservedGeneration = proxy.Generation
			setProxyCondition(proxy, conditionReady, metav1.ConditionFalse, "RedirectProgressing", message)
			setProxyCondition(proxy, conditionProgressing, metav1.ConditionTrue, "RedirectProgressing", message)
			setProxyCondition(proxy, conditionDegraded, metav1.ConditionFalse, "Redirect", "")
		})
	default:
		message := fmt.Sprintf("every public path redirects to %s (302)", hostname)
		return r.patchStatus(ctx, proxy, func() {
			proxy.Status.ObservedGeneration = proxy.Generation
			setProxyCondition(proxy, conditionReady, metav1.ConditionTrue, "RedirectActive", message)
			setProxyCondition(proxy, conditionProgressing, metav1.ConditionFalse, "RedirectActive", message)
			setProxyCondition(proxy, conditionDegraded, metav1.ConditionFalse, "Redirect", "")
		})
	}
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
	// Only a DECLARED http service is validated (an absent key may park
	// nothing — absent = disabled). It either serves or redirects; the CRD
	// enforces the either-or at admission (CEL on the shared
	// MirrorHTTPServiceSpec), these checks keep the InvalidSpec path complete
	// for specs that bypassed it.
	if http := proxy.Spec.Publish.HTTP; http != nil {
		httpPath := path.Child("publish", "http")
		switch {
		case http.Serving():
			errs = append(errs, validatePublishPodTemplate(&http.PodTemplate,
				httpPath.Child("podTemplate"), ProxyCacheVolumeName)...)
		case http.Redirect == "":
			errs = append(errs, field.Required(httpPath, "must declare a serving podTemplate (spec.containers) or a redirect"))
		default:
			errs = append(errs, validateRedirectHostname(http.Redirect, httpPath.Child("redirect"))...)
		}
		errs = append(errs, validateHTTPAliases(http, proxy.Name, httpPath.Child("aliases"))...)
	}
	if proxyCacheEnabled(proxy) {
		cachePath := path.Child("cache", "pvcTemplate")
		spec := proxy.Spec.Cache.PVCSpec
		if len(spec.AccessModes) == 0 {
			errs = append(errs, field.Required(cachePath.Child("accessModes"), "must declare at least one access mode when cache is enabled"))
		}
		requestedStorage := spec.Resources.Requests[corev1.ResourceStorage]
		if requestedStorage.IsZero() || requestedStorage.Sign() < 0 {
			errs = append(errs, field.Required(cachePath.Child("resources", "requests", string(corev1.ResourceStorage)), "must be greater than zero when cache is enabled"))
		}
		if spec.DataSource != nil || spec.DataSourceRef != nil || spec.VolumeName != "" || spec.Selector != nil {
			errs = append(errs, field.Invalid(cachePath, spec, "dataSource, dataSourceRef, volumeName, and selector are unsupported for the cache PVC"))
		}
	}
	return errs
}

func proxyCacheEnabled(proxy *mirrorv1alpha1.ProxyMirror) bool {
	return proxy.Spec.Cache != nil
}
