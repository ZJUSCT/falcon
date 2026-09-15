package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	mirrorv1alpha1 "github.com/ZJUSCT/falcon/api/v1alpha1"
)

const derivedResourceInvalid = "DerivedResourceInvalid"

// derivedNameConflictError reports a derived publish workload whose name is
// already taken by the same-named child of another Mirror/ProxyMirror. Names
// are derived deterministically from the CR name (dots mapped to '-' for the
// DNS-1035 publish pair), so `crates.io-index` and `crates-io-index` collide;
// Falcon refuses to adopt or overwrite the foreign child and projects the
// conflict onto its parent as Degraded.
type derivedNameConflictError struct {
	kind  string
	name  string
	owner metav1.OwnerReference
}

func (e *derivedNameConflictError) Error() string {
	return fmt.Sprintf("derived %s %q is already controlled by %s %q; refusing to adopt it. Publish workload names are the CR name with dots replaced by '-', so this is a name collision — rename one of the colliding mirrors",
		e.kind, e.name, e.owner.Kind, e.owner.Name)
}

// wrapDerivedConflict converts an ownership conflict on a derived child into
// a typed error the reconcilers project as Degraded; every other error is
// returned unchanged.
func wrapDerivedConflict(err error, kind, name string) error {
	var owned *controllerutil.AlreadyOwnedError
	if errors.As(err, &owned) {
		return &derivedNameConflictError{kind: kind, name: name, owner: owned.Owner}
	}
	return err
}

// derivedResourceInvalidMessage recognizes failures of a derived child object
// that should surface on the parent CR: an Invalid response from the API
// server (the authority for derived-name validation) and a transformed-name
// collision with another mirror's child. Falcon only projects those onto the
// parent.
func derivedResourceInvalidMessage(err error, parentKind, parentName string) (string, bool) {
	var conflict *derivedNameConflictError
	if errors.As(err, &conflict) {
		return conflict.Error(), true
	}
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
		setCondition(mirror, conditionProgressing, conditionStatus(mirror.Status.Publication != nil), derivedResourceInvalid, message)
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
