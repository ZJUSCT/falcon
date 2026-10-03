package controller

import (
	"context"
	"fmt"
	"maps"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	mirrorv1alpha1 "github.com/ZJUSCT/falcon/api/v1alpha1"
	"github.com/ZJUSCT/falcon/internal/config"
)

// Publish service keys allowed under spec.publish. Only an enabled "http"
// service gets a publish HTTPRoute; "rsync" is exposed through its Service
// only (no route, no TCPRoute). "git" is not a key on purpose: git publishing
// uses HTTP (a fastcgi-style container) expressed through the "http" key.
const (
	PublishProtocolHTTP  = "http"
	PublishProtocolRsync = "rsync"

	// publishServicePort is the port every publish Service exposes on its
	// Service (the container port is reached through the named target port,
	// which is named after the service key), and the port publish HTTPRoute
	// backendRefs point at.
	publishServicePort = 80

	// publishRolePrefix is the shared prefix of every service key's role
	// label value ("publish-http", "publish-rsync") — publish pods are told
	// apart from other children by this prefix.
	publishRolePrefix = "publish-"

	// reloaderStampAnnotation is Reloader's pod-template annotation under
	// the annotations reload strategy (--reload-strategy=annotations). It is
	// the one foreign mutation preserved when the controller replaces the
	// publish pod template: wiping it would revert (and re-roll) every
	// reloader-triggered rollout on the next reconcile. See the README,
	// "发布工作负载的配置热更新".
	reloaderStampAnnotation = "reloader.stakater.com/last-reloaded-from"
)

// publishChildName is the deterministic name of one service entry's publish
// Deployment and Service: <base>-publish-<protocol> (e.g. <base>-publish-http).
// Service names are DNS-1035 labels and cannot contain dots, so a dotted CR
// name has its dots mapped to '-' for both workloads (e.g. `crates.io-index`
// -> `crates-io-index-publish-http`); the Deployment shares the name so every
// reference (backendRef, teardown, health, drain) stays in lockstep.
func publishChildName(base, protocol string) string {
	return resourceName(dns1035Base(base), "publish-"+protocol)
}

// publishRole is the role label value shared by one service entry's
// Deployment, Service and pods ("publish-http", "publish-rsync",
// "publish-git") so each Service selects only its own pods.
func publishRole(protocol string) string {
	return publishRolePrefix + protocol
}

// publishAppProtocol is the appProtocol of the publish Service port: "http"
// for the routed http service, unset for the others.
func publishAppProtocol(protocol string) *string {
	if protocol == PublishProtocolHTTP {
		return stringPtr("http")
	}
	return nil
}

// publishHTTPEnabled reports whether the "http" publish service is enabled —
// the only service that receives a publish HTTPRoute.
func publishHTTPEnabled(mirror *mirrorv1alpha1.Mirror) bool {
	return mirror.Spec.Publish.HTTP != nil
}

// publishRouteEnabled reports whether a publish HTTPRoute is desired at all:
// a declared http key, or an active publish-level redirect (its route is the
// entire endpoint, with or without any protocol key).
func publishRouteEnabled(mirror *mirrorv1alpha1.Mirror) bool {
	if _, redirecting := mirror.Spec.Publish.RedirectActive(); redirecting {
		return true
	}
	return mirror.Spec.Publish.HTTP != nil
}

// validatePublishPodTemplate checks the user-written pod template of an
// enabled publish service: at least one container, whose first declared
// container port becomes the Service target (targeted by number), and no
// user-declared volume clashing with a volume name the
// controller injects itself. The CRD additionally enforces the
// declaration-time podTemplate.spec presence rule at admission; the
// controller-side check keeps the InvalidSpec path complete for specs that
// bypassed it.
func validatePublishPodTemplate(template *corev1.PodTemplateSpec, path *field.Path, reservedVolumeNames ...string) field.ErrorList {
	var errs field.ErrorList
	containers := template.Spec.Containers
	if len(containers) == 0 {
		errs = append(errs, field.Required(path.Child("spec", "containers"), "must declare at least one container"))
		return errs
	}
	if len(containers[0].Ports) == 0 {
		errs = append(errs, field.Required(path.Child("spec", "containers").Index(0).Child("ports"),
			"must declare at least one container port on the first container (the first port is the Service target)"))
	}
	reserved := make(map[string]bool, len(reservedVolumeNames))
	for _, name := range reservedVolumeNames {
		reserved[name] = true
	}
	for i := range template.Spec.Volumes {
		if reserved[template.Spec.Volumes[i].Name] {
			errs = append(errs, field.Invalid(path.Child("spec", "volumes").Index(i).Child("name"),
				template.Spec.Volumes[i].Name, "this volume name is reserved: the controller injects it itself"))
		}
	}
	return errs
}

// pathPrefixMatches builds one PathPrefix match per public path prefix
// (canonical first, then aliases in declaration order — matches within a rule
// are OR).
func pathPrefixMatches(pathPrefixes []string) []gatewayv1.HTTPRouteMatch {
	matches := make([]gatewayv1.HTTPRouteMatch, 0, len(pathPrefixes))
	for _, prefix := range pathPrefixes {
		matches = append(matches, gatewayv1.HTTPRouteMatch{
			Path: &gatewayv1.HTTPPathMatch{
				Type:  ptr.To(gatewayv1.PathMatchPathPrefix),
				Value: ptr.To(prefix),
			},
		})
	}
	return matches
}

// ensureRouteWithRules idempotently maintains the publish HTTPRoute
// (<base>-publish) of one published Mirror in the owner's
// namespace:
//
//   - ownerReference -> the CR (controller=true), so deleting the CR
//     garbage-collects the route; no finalizer involved;
//   - labels: the usual child labels plus config publish.labels;
//   - annotations: config publish.annotations;
//   - parentRefs: [config publish.gatewayRef] (namespace omitted when it
//     equals the CR namespace);
//   - hostnames: config publish.hostnames;
//   - the caller's rules (serving or redirect mode of the same object —
//     switching modes rewrites the rules in place and never leaves two
//     routes competing for the same paths).
//
// It is the caller's responsibility to invoke this only when an HTTP endpoint
// is desired and config.PublishEnabled() is true.
func ensureRouteWithRules(ctx context.Context, c client.Client, recorder record.EventRecorder, scheme *runtime.Scheme, cfg *config.Config, owner client.Object, rules []gatewayv1.HTTPRouteRule, detail string) error {
	base := childBase(owner.GetName())
	routeName := resourceName(base, "publish")
	routeKey := types.NamespacedName{Namespace: owner.GetNamespace(), Name: routeName}
	route := &gatewayv1.HTTPRoute{ObjectMeta: metav1.ObjectMeta{Namespace: routeKey.Namespace, Name: routeKey.Name}}

	labels := objectLabels(base, publishRole(PublishProtocolHTTP))
	maps.Copy(labels, cfg.Publish.HTTP.Labels)

	op, err := controllerutil.CreateOrUpdate(ctx, c, route, func() error {
		route.Labels = labels
		// Keep an absent annotations field absent. Assigning a non-nil empty map
		// makes the API server persist `{}`, after which every reconcile compares
		// it with the desired empty map and writes the HTTPRoute again.
		if len(cfg.Publish.HTTP.Annotations) == 0 {
			route.Annotations = nil
		} else {
			route.Annotations = maps.Clone(cfg.Publish.HTTP.Annotations)
		}
		route.Spec = gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{publishGatewayParentRef(cfg, owner.GetNamespace())},
			},
			Hostnames: hostnamesAsGatewayHostnames(cfg.Publish.HTTP.Hostnames),
			Rules:     rules,
		}
		return controllerutil.SetControllerReference(owner, route, scheme)
	})
	if err != nil {
		return err
	}
	if recorder != nil {
		switch op {
		case controllerutil.OperationResultCreated:
			recorder.Eventf(owner, corev1.EventTypeNormal, "PublishRouteCreated",
				"Created publish HTTPRoute %s/%s (%s)", routeKey.Namespace, routeKey.Name, detail)
		case controllerutil.OperationResultUpdated:
			recorder.Eventf(owner, corev1.EventTypeNormal, "PublishRouteUpdated",
				"Updated publish HTTPRoute %s/%s", routeKey.Namespace, routeKey.Name)
		}
	}
	return nil
}

// aliasRedirectTarget returns the canonical-tree prefix an alias path
// redirects into: the canonical /<owner name> prefix itself, or — for a
// subset alias — that prefix plus the subset subpath.
func aliasRedirectTarget(canonicalPath string, alias mirrorv1alpha1.MirrorAlias) string {
	if alias.Subset != nil {
		return canonicalPath + "/" + alias.Subset.SubPath
	}
	return canonicalPath
}

// ensurePublishRouteFor maintains the SERVING-mode publish HTTPRoute. The
// canonical /<owner name> prefix points at Service <base>-publish-http port 80.
// Every publish-level alias gets its own rule that permanently redirects the
// matched prefix into the canonical tree (a subset alias into its
// subdirectory) while preserving the unmatched suffix. Backends therefore
// only need to serve the name-derived canonical path. The route always
// targets the http service: the rsync service is Service-only and never
// routed.
func ensurePublishRouteFor(ctx context.Context, c client.Client, recorder record.EventRecorder, scheme *runtime.Scheme, cfg *config.Config, owner client.Object, aliases []mirrorv1alpha1.MirrorAlias) error {
	httpServiceName := publishChildName(childBase(owner.GetName()), PublishProtocolHTTP)
	canonicalPath := "/" + owner.GetName()
	rules := []gatewayv1.HTTPRouteRule{{
		Matches: pathPrefixMatches([]string{canonicalPath}),
		BackendRefs: []gatewayv1.HTTPBackendRef{{
			BackendRef: gatewayv1.BackendRef{
				BackendObjectReference: gatewayv1.BackendObjectReference{
					Group: ptr.To(gatewayv1.Group("")),
					Kind:  ptr.To(gatewayv1.Kind("Service")),
					Name:  gatewayv1.ObjectName(httpServiceName),
					Port:  ptr.To(gatewayv1.PortNumber(publishServicePort)),
				},
				Weight: ptr.To(int32(1)), // Gateway API defaults this to 1; set it explicitly for idempotence.
			},
		}},
	}}
	for _, alias := range aliases {
		target := aliasRedirectTarget(canonicalPath, alias)
		rules = append(rules, gatewayv1.HTTPRouteRule{
			Matches: pathPrefixMatches([]string{alias.Path}),
			Filters: []gatewayv1.HTTPRouteFilter{{
				Type: gatewayv1.HTTPRouteFilterRequestRedirect,
				RequestRedirect: &gatewayv1.HTTPRequestRedirectFilter{
					Path: &gatewayv1.HTTPPathModifier{
						Type:               gatewayv1.PrefixMatchHTTPPathModifier,
						ReplacePrefixMatch: ptr.To(target),
					},
					StatusCode: ptr.To(301),
				},
			}},
		})
	}
	return ensureRouteWithRules(ctx, c, recorder, scheme, cfg, owner, rules, fmt.Sprintf("PathPrefix %s", canonicalPath))
}

// ensureRedirectRouteFor maintains the REDIRECT-mode publish HTTPRoute: the
// same object as the serving variant (name, labels, annotations, parentRef,
// hostnames), but the single rule answers every matched path with a
// RequestRedirect filter instead of backendRefs. Scheme and path stay unset
// in the filter: the gateway then preserves the request scheme (http stays
// http, https stays https) and reuses the request path as-is, so every public
// path — canonical and aliases alike, matches within a rule being OR — maps
// to the same path on the redirect hostname over the incoming scheme. The
// redirect is temporary ops tooling (e.g. migrating data across nodes), hence
// the explicit 302: clients must not cache it.
func ensureRedirectRouteFor(ctx context.Context, c client.Client, recorder record.EventRecorder, scheme *runtime.Scheme, cfg *config.Config, owner client.Object, pathPrefixes []string, hostname string) error {
	rules := []gatewayv1.HTTPRouteRule{{
		Matches: pathPrefixMatches(pathPrefixes),
		Filters: []gatewayv1.HTTPRouteFilter{{
			Type: gatewayv1.HTTPRouteFilterRequestRedirect,
			RequestRedirect: &gatewayv1.HTTPRequestRedirectFilter{
				Hostname: ptr.To(gatewayv1.PreciseHostname(hostname)),
				// Gateway API defaults this to 302; set it explicitly for idempotence.
				StatusCode: ptr.To(302),
			},
		}},
	}}
	return ensureRouteWithRules(ctx, c, recorder, scheme, cfg, owner, rules, fmt.Sprintf("302 /%s -> %s", owner.GetName(), hostname))
}

// publishGatewayParentRef builds the single parentRef pointing at the
// configured publish Gateway. The parentRef namespace is omitted when it
// equals the CR namespace (same-namespace default of the Gateway API).
func publishGatewayParentRef(cfg *config.Config, ownerNamespace string) gatewayv1.ParentReference {
	ref := gatewayv1.ParentReference{
		Group: ptr.To(gatewayv1.Group("gateway.networking.k8s.io")),
		Kind:  ptr.To(gatewayv1.Kind("Gateway")),
		Name:  gatewayv1.ObjectName(cfg.Publish.HTTP.GatewayRef.Name),
	}
	if ns := cfg.Publish.HTTP.GatewayRef.Namespace; ns != "" && ns != ownerNamespace {
		ref.Namespace = ptr.To(gatewayv1.Namespace(ns))
	}
	if section := cfg.Publish.HTTP.GatewayRef.SectionName; section != "" {
		ref.SectionName = ptr.To(gatewayv1.SectionName(section))
	}
	return ref
}

func hostnamesAsGatewayHostnames(hostnames []string) []gatewayv1.Hostname {
	out := make([]gatewayv1.Hostname, 0, len(hostnames))
	for _, host := range hostnames {
		out = append(out, gatewayv1.Hostname(host))
	}
	return out
}

// ensurePublishedMirrorRoute guards the Mirror-specific invocation of the
// publish route, in whichever mode applies. An active publish-level redirect
// wins over everything: the route is the redirect variant and no serving
// workload is expected (cleanupDisabledPublishChildren tears any down).
// Otherwise a published Mirror (status.activePVC non-empty) gets the serving
// route: the canonical /<mirror name> path forwards to the publish Service,
// and every publish-level alias redirects into the canonical tree.
func ensurePublishedMirrorRoute(ctx context.Context, r *MirrorReconciler, mirror *mirrorv1alpha1.Mirror) error {
	if !r.Config.PublishEnabled() {
		return nil
	}
	if hostname, ok := mirror.Spec.Publish.RedirectActive(); ok {
		return ensureRedirectRouteFor(ctx, r.Client, r.Recorder, r.Scheme, r.Config, mirror, mirrorRoutePaths(mirror), hostname)
	}
	return ensurePublishRouteFor(ctx, r.Client, r.Recorder, r.Scheme, r.Config, mirror, mirror.Spec.Publish.Aliases)
}

// deletePublishRouteFor removes the deterministic route when HTTP publishing
// is no longer desired. NotFound is the steady state.
func deletePublishRouteFor(ctx context.Context, c client.Client, owner client.Object) error {
	route := &gatewayv1.HTTPRoute{ObjectMeta: metav1.ObjectMeta{
		Namespace: owner.GetNamespace(),
		Name:      resourceName(childBase(owner.GetName()), "publish"),
	}}
	if err := c.Get(ctx, client.ObjectKeyFromObject(route), route); apierrors.IsNotFound(err) {
		return nil
	} else if err != nil {
		return err
	}
	if metav1.IsControlledBy(route, owner) {
		if err := c.Delete(ctx, route); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

// mirrorRoutePaths returns the public path prefixes of a Mirror: the canonical
// /<mirror name> first, then the publish-level aliases in declaration order.
// The rsync service has no path representation.
func mirrorRoutePaths(mirror *mirrorv1alpha1.Mirror) []string {
	paths := []string{"/" + mirror.Name}
	for _, alias := range mirror.Spec.Publish.Aliases {
		paths = append(paths, alias.Path)
	}
	return paths
}

type publishRouteState int

const (
	publishRoutePending publishRouteState = iota
	publishRouteReady
	publishRouteRejected
)

// publishRouteHealth requires current-generation Accepted=True and
// ResolvedRefs=True. Explicit False means the desired endpoint is degraded;
// Unknown, stale, or missing conditions mean the gateway is still converging.
func publishRouteHealth(ctx context.Context, c client.Client, owner client.Object) (publishRouteState, string, error) {
	routeName := resourceName(childBase(owner.GetName()), "publish")
	route := &gatewayv1.HTTPRoute{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: owner.GetNamespace(), Name: routeName}, route); err != nil {
		if apierrors.IsNotFound(err) {
			return publishRoutePending, "waiting for HTTPRoute creation", nil
		}
		return publishRoutePending, "", err
	}
	if len(route.Spec.ParentRefs) == 0 {
		return publishRoutePending, "waiting for HTTPRoute parent reference", nil
	}
	desiredParent := route.Spec.ParentRefs[0]
	for _, parent := range route.Status.Parents {
		if !sameRouteParent(desiredParent, parent.ParentRef, route.Namespace) {
			continue
		}
		accepted := false
		resolved := false
		for _, condition := range parent.Conditions {
			if condition.ObservedGeneration != route.Generation {
				continue
			}
			isAccepted := condition.Type == string(gatewayv1.RouteConditionAccepted)
			isResolved := condition.Type == string(gatewayv1.RouteConditionResolvedRefs)
			if !isAccepted && !isResolved {
				continue
			}
			if condition.Status == metav1.ConditionFalse {
				reason := condition.Reason
				if reason == "" {
					reason = "Rejected"
				}
				message := fmt.Sprintf("HTTPRoute %s %s=False (%s): %s", routeName, condition.Type, reason, condition.Message)
				return publishRouteRejected, message, nil
			}
			if condition.Status == metav1.ConditionTrue {
				accepted = accepted || isAccepted
				resolved = resolved || isResolved
			}
		}
		if accepted && resolved {
			return publishRouteReady, "HTTPRoute is accepted and all references are resolved", nil
		}
	}
	return publishRoutePending, "waiting for HTTPRoute Accepted=True and ResolvedRefs=True", nil
}

func sameRouteParent(desired, observed gatewayv1.ParentReference, routeNamespace string) bool {
	group := func(ref gatewayv1.ParentReference) gatewayv1.Group {
		if ref.Group == nil {
			return gatewayv1.Group(gatewayv1.GroupName)
		}
		return *ref.Group
	}
	kind := func(ref gatewayv1.ParentReference) gatewayv1.Kind {
		if ref.Kind == nil {
			return "Gateway"
		}
		return *ref.Kind
	}
	namespace := func(ref gatewayv1.ParentReference) gatewayv1.Namespace {
		if ref.Namespace == nil {
			return gatewayv1.Namespace(routeNamespace)
		}
		return *ref.Namespace
	}
	return group(desired) == group(observed) &&
		kind(desired) == kind(observed) &&
		namespace(desired) == namespace(observed) &&
		desired.Name == observed.Name &&
		ptr.Deref(desired.SectionName, "") == ptr.Deref(observed.SectionName, "") &&
		ptr.Deref(desired.Port, 0) == ptr.Deref(observed.Port, 0)
}
