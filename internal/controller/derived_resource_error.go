package controller

import (
	"context"
	"errors"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"

	mirrorv1alpha1 "github.com/ZJUSCT/falcon/api/v1alpha1"
)

const derivedResourceInvalid = "DerivedResourceInvalid"

// derivedResourceInvalidMessage recognizes an Invalid response for an object
// other than the parent CR. The API server is the authority for validation of
// generated resources; Falcon only projects that rejection onto the parent.
func derivedResourceInvalidMessage(err error, parentKind, parentName string) (string, bool) {
	if !apierrors.IsInvalid(err) {
		return "", false
	}
	var status apierrors.APIStatus
	if errors.As(err, &status) {
		details := status.Status().Details
		if details != nil &&
			details.Group == mirrorv1alpha1.GroupVersion.Group &&
			strings.EqualFold(details.Kind, parentKind) &&
			details.Name == parentName {
			return "", false
		}
	}
	return "Kubernetes API server rejected a derived resource: " + err.Error(), true
}

func (r *MirrorReconciler) handleDerivedResourceInvalid(ctx context.Context, mirror *mirrorv1alpha1.Mirror, result ctrl.Result, err error) (ctrl.Result, error) {
	message, ok := derivedResourceInvalidMessage(err, "Mirror", mirror.Name)
	if !ok {
		return result, err
	}
	if r.Recorder != nil {
		r.Recorder.Event(mirror, corev1.EventTypeWarning, derivedResourceInvalid, message)
	}
	return r.patchStatus(ctx, mirror, func() {
		mirror.Status.ObservedGeneration = mirror.Generation
		setCondition(mirror, conditionReady, conditionStatus(mirrorWasReady(mirror)), derivedResourceInvalid, message)
		setCondition(mirror, conditionProgressing, metav1.ConditionFalse, derivedResourceInvalid, message)
		setCondition(mirror, conditionDegraded, metav1.ConditionTrue, derivedResourceInvalid, message)
	})
}

func (r *ProxyMirrorReconciler) handleDerivedResourceInvalid(ctx context.Context, proxy *mirrorv1alpha1.ProxyMirror, result ctrl.Result, err error) (ctrl.Result, error) {
	message, ok := derivedResourceInvalidMessage(err, "ProxyMirror", proxy.Name)
	if !ok {
		return result, err
	}
	if r.Recorder != nil {
		r.Recorder.Event(proxy, corev1.EventTypeWarning, derivedResourceInvalid, message)
	}
	return r.patchStatus(ctx, proxy, func() {
		proxy.Status.ObservedGeneration = proxy.Generation
		setProxyCondition(proxy, conditionReady, conditionStatus(proxyWasReady(proxy)), derivedResourceInvalid, message)
		setProxyCondition(proxy, conditionProgressing, metav1.ConditionFalse, derivedResourceInvalid, message)
		setProxyCondition(proxy, conditionDegraded, metav1.ConditionTrue, derivedResourceInvalid, message)
	})
}
