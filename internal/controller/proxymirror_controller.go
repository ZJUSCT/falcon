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
// HTTPRoute once the proxy is Ready, and reports readiness through conditions.
// Cleanup of children relies purely on owner-reference GC, so no finalizer is
// needed.
type ProxyMirrorReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
	Now      func() time.Time
	// Config is the loaded controller configuration (required). The serving
	// section (config serving.*) gates publish HTTPRoute generation.
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

func (r *ProxyMirrorReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	proxy := &mirrorv1alpha1.ProxyMirror{}
	if err := r.Get(ctx, req.NamespacedName, proxy); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Children are garbage-collected through owner references; nothing to do.
	if !proxy.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	if errs := validateProxyMirror(proxy); len(errs) > 0 {
		message := errs.ToAggregate().Error()
		logger.Info("ProxyMirror specification is invalid", "errors", message)
		return r.patchStatus(ctx, proxy, func() {
			proxy.Status.ObservedGeneration = proxy.Generation
			proxy.Status.Phase = mirrorv1alpha1.PhaseDegraded
			setProxyCondition(proxy, conditionReady, metav1.ConditionFalse, "InvalidSpec", message)
			setProxyCondition(proxy, conditionProgressing, metav1.ConditionFalse, "InvalidSpec", message)
			setProxyCondition(proxy, conditionDegraded, metav1.ConditionTrue, "InvalidSpec", message)
		})
	}

	cachePVC, err := r.ensureCachePVC(ctx, proxy)
	if err != nil {
		return ctrl.Result{}, err
	}

	// A ProxyMirror has no paused concept: the reconciler always ensures the
	// declared publish services; to take a proxy mirror offline, delete the
	// CR.
	ready, serviceName, err := r.ensureProxyPublish(ctx, proxy)
	if err != nil {
		return ctrl.Result{}, err
	}
	// A Ready proxy is published: ensure its publish HTTPRoute, but only when
	// the http service is enabled and the config enables route generation —
	// see ServingEnabled.
	if ready && serviceName != "" {
		if err := ensureReadyProxyRoute(ctx, r, proxy); err != nil {
			return ctrl.Result{}, err
		}
	}
	persistChildNames := func() {
		proxy.Status.CachePVC = cachePVC
		proxy.Status.PublishedServiceName = serviceName
	}
	if !ready {
		return r.patchStatusWithResult(ctx, proxy, ctrl.Result{RequeueAfter: 5 * time.Second}, func() {
			persistChildNames()
			proxy.Status.ObservedGeneration = proxy.Generation
			proxy.Status.Phase = mirrorv1alpha1.PhasePending
			setProxyCondition(proxy, conditionReady, metav1.ConditionFalse, "ServingRollout", "waiting for the proxy Deployment to become available")
			setProxyCondition(proxy, conditionProgressing, metav1.ConditionTrue, "ServingRollout", "updating the proxy Deployment")
			setProxyCondition(proxy, conditionDegraded, metav1.ConditionFalse, "ServingRollout", "")
		})
	}

	return r.patchStatus(ctx, proxy, func() {
		persistChildNames()
		proxy.Status.ObservedGeneration = proxy.Generation
		proxy.Status.Phase = mirrorv1alpha1.PhaseReady
		setProxyCondition(proxy, conditionReady, metav1.ConditionTrue, "Serving", "the proxy Deployment is available")
		setProxyCondition(proxy, conditionProgressing, metav1.ConditionFalse, "Serving", "the proxy Deployment is available")
		setProxyCondition(proxy, conditionDegraded, metav1.ConditionFalse, "Serving", "")
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
	// publish-http is the longest suffix of any ProxyMirror child name
	// (<base>-publish-http; the proxy has no rsync key).
	if err := validateDerivedName(proxy.Name, "publish-http"); err != nil {
		errs = append(errs, err)
	}
	// Only an ENABLED http service is validated (a disabled key may park
	// anything).
	if http := proxy.Spec.Services.HTTP; http.Enable {
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
