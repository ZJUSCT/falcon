#!/usr/bin/env bash
# Default: install Falcon and verify basic reconciliation on a real API server.
# Pass full for the optional sync/snapshot/publish integration scenario.
set -euo pipefail

MODE=${1:-smoke}
case "$MODE" in
    smoke|full) ;;
    *) echo "Unknown e2e mode: $MODE" >&2; exit 2 ;;
esac

CLUSTER=falcon-e2e
NAMESPACE=mirror
SITE_HOST=mirrors.example.org
NODE_IMAGE=kindest/node:v1.36.4
IMAGE=falcon:e2e
KUBECONFIG=/tmp/kubeconfig
export KUBECONFIG

log() { printf '\n==> %s\n' "$*"; }

# dump prints the cluster state straight to stdout (the run's live output)
# on failure only. Fail fast per request: with the API server unreachable
# every call would otherwise hang for minutes in client-side retries.
dump() {
    k=(kubectl --request-timeout=10s)
    echo "==== e2e diagnostics ===="
    "${k[@]}" get pods -A || true
    "${k[@]}" get events -A --sort-by=.metadata.creationTimestamp || true
    "${k[@]}" -n "$NAMESPACE" get mirrors -o yaml || true
    "${k[@]}" -n "$NAMESPACE" logs deploy/falcon --tail=300 || true
    if [ "$MODE" = smoke ]; then return; fi
    "${k[@]}" get mirror,httproute,gateway,pvc,volumesnapshot -A || true
    "${k[@]}" -n "$NAMESPACE" get deployment,replicaset,pod \
        -l mirrors.zjusct.io/mirror=demo -o yaml || true
    "${k[@]}" -n "$NAMESPACE" describe deployment demo-publish-http || true
    "${k[@]}" describe gatewayclass envoy-gateway || true
    "${k[@]}" describe -n "$NAMESPACE" gateway mirror-gateway || true
    "${k[@]}" -n envoy-gateway-system logs deploy/envoy-gateway --tail=300 || true
    "${k[@]}" -n default logs statefulset/csi-hostpathplugin --all-containers --tail=60 || true
    "${k[@]}" get csidriver,csinode -o wide || true
    "${k[@]}" describe -n "$NAMESPACE" mirror demo || true
    "${k[@]}" -n reloader logs deploy/reloader-reloader --tail=300 || true
    jobs=$("${k[@]}" -n "$NAMESPACE" get jobs -o name 2>/dev/null || true)
    for job in $jobs; do
        "${k[@]}" -n "$NAMESPACE" logs "$job" --tail=100 || true
    done
}

cleanup() {
    rc=$?
    if [ "$rc" -ne 0 ]; then
        dump
    fi
    kind delete cluster --name "$CLUSTER" || rc=1
    exit "$rc"
}
# Never tear down a cluster belonging to another invocation.
if kind get clusters | grep -Fxq "$CLUSTER"; then
    echo "Cluster $CLUSTER already exists; refusing to replace it" >&2
    exit 1
fi
trap cleanup EXIT

log "creating kind cluster ($NODE_IMAGE)"
kind create cluster \
    --config scripts/e2e/kind-config.yaml \
    --image "$NODE_IMAGE" \
    --kubeconfig "$KUBECONFIG" \
    --wait 180s

# This container runs on the host network: the kubeconfig kind writes
# (127.0.0.1:<port> bound to the host loopback) is valid as-is, and the
# node addresses below are directly reachable.
kubectl wait --for=condition=Ready nodes --all --timeout=180s

log "loading $IMAGE into the cluster"
kind load docker-image "$IMAGE" --name "$CLUSTER"

if [ "$MODE" = smoke ]; then
    log "installing API dependencies (no gateway or storage workloads)"
    kubectl apply --server-side -k scripts/e2e/smoke-crds
    kubectl wait --for=condition=Established \
        crd/httproutes.gateway.networking.k8s.io \
        crd/volumesnapshots.snapshot.storage.k8s.io --timeout=120s

    log "installing Falcon chart"
    helm install falcon charts/falcon -n "$NAMESPACE" --create-namespace \
        -f scripts/e2e/smoke-values.yaml --wait --timeout 180s
    kubectl wait --for=condition=Established crd/mirrors.mirrors.zjusct.io --timeout=60s
    kubectl -n "$NAMESPACE" rollout status deploy/falcon --timeout=60s

    log "checking CR admission and controller status writes"
    kubectl -n "$NAMESPACE" apply -f tests/e2e/smoke-mirror.yaml
    kubectl -n "$NAMESPACE" wait --for=jsonpath='{.status.observedGeneration}'=1 \
        mirror/smoke --timeout=60s
    kubectl -n "$NAMESPACE" wait --for=condition=Degraded=false mirror/smoke --timeout=60s

    log "checking cleanup"
    kubectl -n "$NAMESPACE" delete mirror smoke --wait --timeout=60s
    helm uninstall falcon -n "$NAMESPACE" --wait --timeout 120s
    log "PASS (installation smoke test)"
    exit 0
fi

log "installing Envoy Gateway (v1.9.0)"
# Via the helm chart rather than the release install.yaml: the latter ships
# the Gateway API CRDs but no GatewayClass, which the chart provides. The
# chart also installs the CRDs the Gateway fixture below depends on.
helm upgrade --install eg oci://docker.io/envoyproxy/gateway-helm \
    --version v1.9.0 -n envoy-gateway-system --create-namespace \
    --wait --timeout 10m

log "installing cluster dependencies (kubectl apply -k scripts/e2e/cluster)"
# Server-side apply: the snapshot CRDs otherwise exceed the 256KB
# client-side last-applied-configuration annotation limit. Custom resources
# whose CRDs are created in the same batch fail to map until the CRDs reach
# Established, so retry once after waiting for them (idempotent; skipped
# when the first pass fully succeeds, e.g. on a warm cluster).
result=0
kubectl apply --server-side -k scripts/e2e/cluster || result=$?
kubectl wait --for=condition=Established \
    crd/volumesnapshotclasses.snapshot.storage.k8s.io --timeout=120s
if [ "$result" -ne 0 ]; then
    kubectl apply --server-side -k scripts/e2e/cluster
fi
kubectl -n kube-system rollout status deploy/snapshot-controller --timeout=300s
kubectl -n default rollout status statefulset/csi-hostpathplugin --timeout=300s

log "installing Reloader (2.2.17, annotations strategy)"
helm upgrade --install reloader oci://ghcr.io/stakater/charts/reloader \
    --version 2.2.17 -n reloader --create-namespace \
    --set reloader.reloadStrategy=annotations --wait --timeout 5m

log "installing falcon"
helm upgrade --install falcon charts/falcon \
    -n "$NAMESPACE" --create-namespace \
    -f scripts/e2e/values.yaml --wait --timeout 10m
kubectl -n "$NAMESPACE" rollout status deploy/falcon --timeout=180s
kubectl -n "$NAMESPACE" wait --for=condition=Programmed gateway/mirror-gateway --timeout=180s

log "reading the Envoy Gateway data-plane NodePort"
# The EnvoyProxy fixture makes EG create the data-plane Service as NodePort
# (kind has no load balancer); read the assigned port for the chainsaw
# assertions. Host-based routing still applies: requests carry the site
# Host.
envoy=$(kubectl get svc -A -l gateway.envoyproxy.io/owning-gateway-name=mirror-gateway \
    -o jsonpath='{.items[0].metadata.namespace}/{.items[0].metadata.name}')
node_port=$(kubectl -n "${envoy%%/*}" get "svc/${envoy#*/}" \
    -o jsonpath='{.spec.ports[0].nodePort}')

log "running the e2e test suite"
node_ip=$(kubectl get nodes -o jsonpath='{.items[0].status.addresses[?(@.type=="InternalIP")].address}')
export E2E_SITE_HOST="$SITE_HOST"
export E2E_ENVOY_ADDR="$node_ip:$node_port"
# Preserve failed resources for dump(); the cluster is always removed on exit.
chainsaw test --config scripts/e2e/chainsaw.yaml --skip-delete tests/e2e/demo-mirror
kubectl -n "$NAMESPACE" delete mirror demo proxy-demo --wait --timeout=180s

log "PASS"
