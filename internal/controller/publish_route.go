package controller

import (
	"context"
	"maps"

	corev1 "k8s.io/api/core/v1"
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

// Publish protocol identifiers allowed in spec.services[].name. Only the
// "http" entry gets a publish HTTPRoute; "rsync"/"git" are exposed through
// their Services only (no route, no TCPRoute).
const (
	PublishProtocolHTTP  = "http"
	PublishProtocolRsync = "rsync"
	PublishProtocolGit   = "git"

	// publishServicePort is the port every publish Service exposes on its
	// Service (the container port is reached through the named target port,
	// which is named after the service protocol), and the port publish
	// HTTPRoute backendRefs point at.
	publishServicePort = 80

	// publishRolePrefix is the shared prefix of every service entry's role
	// label value ("publish-http", "publish-rsync", "publish-git") — publish
	// pods are told apart from other children by this prefix.
	publishRolePrefix = "publish-"
)

// publishChildName is the deterministic name of one service entry's publish
// Deployment and Service: <base>-publish-<protocol> (e.g. <base>-publish-http).
func publishChildName(base, protocol string) (string, error) {
	return resourceName(base, "publish-"+protocol)
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

// publishHTTPEnabled reports whether any declared service entry is the "http"
// service — the only entry that receives a publish HTTPRoute.
func publishHTTPEnabled(mirror *mirrorv1alpha1.Mirror) bool {
	for i := range mirror.Spec.Services {
		if mirror.Spec.Services[i].Name == PublishProtocolHTTP {
			return true
		}
	}
	return false
}

// validateServices validates the spec.services[] entries shared by Mirror and
// ProxyMirror. The CRD additionally enforces these rules (enum, name-uniqueness
// CEL, ports MinItems) at admission; the controller-side check keeps the
// InvalidSpec path complete for specs that bypassed it. An absent or empty
// list is legal (a sync-only mirror publishes nothing).
func validateServices(services []mirrorv1alpha1.MirrorServingService, path *field.Path) field.ErrorList {
	var errs field.ErrorList
	for i := range services {
		service := &services[i]
		servicePath := path.Index(i)
		switch service.Name {
		case PublishProtocolHTTP, PublishProtocolRsync, PublishProtocolGit:
		default:
			errs = append(errs, field.NotSupported(servicePath.Child("name"), service.Name,
				[]string{PublishProtocolHTTP, PublishProtocolRsync, PublishProtocolGit}))
		}
		if service.Image == "" {
			errs = append(errs, field.Required(servicePath.Child("image"), "must not be empty"))
		}
		if len(service.Ports) == 0 {
			errs = append(errs, field.Required(servicePath.Child("ports"),
				"must declare at least one container port (the first port is the Service target)"))
		}
	}
	return errs
}

// ensurePublishRouteFor idempotently maintains the publish HTTPRoute
// (<base>-publish) of one published Mirror or ProxyMirror in the owner's
// namespace:
//
//   - ownerReference -> the CR (controller=true), so deleting the CR
//     garbage-collects the route; no finalizer involved;
//   - labels: the usual child labels plus config serving.labels;
//   - annotations: config serving.annotations;
//   - parentRefs: [config serving.gatewayRef] (namespace omitted when it
//     equals the CR namespace);
//   - hostnames: config serving.hostnames;
//   - one rule: PathPrefix /<cr name> -> Service <base>-publish-http port 80.
//
// It is the caller's responsibility to only invoke this for CRs in a
// published state, and only when config.ServingEnabled() is true
// (with serving disabled existing routes are deliberately left alone).
func ensurePublishRouteFor(ctx context.Context, c client.Client, recorder record.EventRecorder, scheme *runtime.Scheme, cfg *config.Config, owner client.Object) error {
	base, err := childBase(owner.GetName())
	if err != nil {
		return err
	}
	routeName, err := resourceName(base, "publish")
	if err != nil {
		return err
	}
	routeKey := types.NamespacedName{Namespace: owner.GetNamespace(), Name: routeName}
	route := &gatewayv1.HTTPRoute{ObjectMeta: metav1.ObjectMeta{Namespace: routeKey.Namespace, Name: routeKey.Name}}
	// The route always targets the http service entry: rsync/git services are
	// Service-only and never routed.
	httpServiceName, err := publishChildName(base, PublishProtocolHTTP)
	if err != nil {
		return err
	}

	labels := objectLabels(base, "publish")
	maps.Copy(labels, cfg.Serving.Labels)

	op, err := controllerutil.CreateOrUpdate(ctx, c, route, func() error {
		route.Labels = labels
		route.Annotations = maps.Clone(cfg.Serving.Annotations)
		route.Spec = gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{publishGatewayParentRef(cfg, owner.GetNamespace())},
			},
			Hostnames: hostnamesAsGatewayHostnames(cfg.Serving.Hostnames),
			Rules: []gatewayv1.HTTPRouteRule{{
				Matches: []gatewayv1.HTTPRouteMatch{{
					Path: &gatewayv1.HTTPPathMatch{
						Type:  ptr.To(gatewayv1.PathMatchPathPrefix),
						Value: ptr.To("/" + owner.GetName()),
					},
				}},
				BackendRefs: []gatewayv1.HTTPBackendRef{{
					BackendRef: gatewayv1.BackendRef{
						BackendObjectReference: gatewayv1.BackendObjectReference{
							Group: ptr.To(gatewayv1.Group("")),
							Kind:  ptr.To(gatewayv1.Kind("Service")),
							Name:  gatewayv1.ObjectName(httpServiceName),
							Port:  ptr.To(gatewayv1.PortNumber(publishServicePort)),
						},
					},
				}},
			}},
		}
		return controllerutil.SetControllerReference(owner, route, scheme)
	})
	if err != nil {
		return err
	}
	if recorder != nil {
		switch op {
		case controllerutil.OperationResultCreated:
			recorder.Eventf(owner, corev1.EventTypeNormal, "ServingRouteCreated",
				"Created publish HTTPRoute %s/%s (PathPrefix /%s)", routeKey.Namespace, routeKey.Name, owner.GetName())
		case controllerutil.OperationResultUpdated:
			recorder.Eventf(owner, corev1.EventTypeNormal, "ServingRouteUpdated",
				"Updated publish HTTPRoute %s/%s", routeKey.Namespace, routeKey.Name)
		}
	}
	return nil
}

// publishGatewayParentRef builds the single parentRef pointing at the
// configured serving Gateway. The parentRef namespace is omitted when it
// equals the CR namespace (same-namespace default of the Gateway API).
func publishGatewayParentRef(cfg *config.Config, ownerNamespace string) gatewayv1.ParentReference {
	ref := gatewayv1.ParentReference{
		Group: ptr.To(gatewayv1.Group("gateway.networking.k8s.io")),
		Kind:  ptr.To(gatewayv1.Kind("Gateway")),
		Name:  gatewayv1.ObjectName(cfg.Serving.GatewayRef.Name),
	}
	if ns := cfg.Serving.GatewayRef.Namespace; ns != "" && ns != ownerNamespace {
		ref.Namespace = ptr.To(gatewayv1.Namespace(ns))
	}
	if section := cfg.Serving.GatewayRef.SectionName; section != "" {
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

// ensurePublishedMirrorRoute guards the Mirror-specific invocation: only
// published Mirrors (status.activePVC non-empty) get a publish route.
func ensurePublishedMirrorRoute(ctx context.Context, r *MirrorReconciler, mirror *mirrorv1alpha1.Mirror) error {
	if !r.Config.ServingEnabled() {
		return nil
	}
	return ensurePublishRouteFor(ctx, r.Client, r.Recorder, r.Scheme, r.Config, mirror)
}

// ensureReadyProxyRoute guards the ProxyMirror-specific invocation: only
// Ready proxies (deployment available) get a publish route.
func ensureReadyProxyRoute(ctx context.Context, r *ProxyMirrorReconciler, proxy *mirrorv1alpha1.ProxyMirror) error {
	if !r.Config.ServingEnabled() {
		return nil
	}
	return ensurePublishRouteFor(ctx, r.Client, r.Recorder, r.Scheme, r.Config, proxy)
}
