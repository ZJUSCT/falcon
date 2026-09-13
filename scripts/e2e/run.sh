#!/usr/bin/env bash
# End-to-end scenario bootstrap: a single-node kind cluster on the host
# Docker daemon (this container drives it through the mounted socket), the
# cluster dependencies from the scripts/e2e/cluster kustomization, the falcon
# chart, and the demo Mirror. The assertions run with chainsaw (tests/e2e).
set -euo pipefail

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
    "${k[@]}" get events -A --sort-by=.lastTransitionTime || true
    "${k[@]}" get mirror,httproute,gateway,pvc,volumesnapshot -A || true
    "${k[@]}" describe gatewayclass envoy-gateway || true
    "${k[@]}" describe -n "$NAMESPACE" gateway mirror-gateway || true
    "${k[@]}" -n envoy-gateway-system logs deploy/envoy-gateway --tail=300 || true
    "${k[@]}" -n default logs statefulset/csi-hostpathplugin --all-containers --tail=60 || true
    "${k[@]}" get csidriver,csinode -o wide || true
    "${k[@]}" describe -n "$NAMESPACE" mirror demo || true
    "${k[@]}" -n "$NAMESPACE" logs deploy/falcon --tail=300 || true
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
    kind delete cluster --name "$CLUSTER" >/dev/null 2>&1 || true
    exit "$rc"
}
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
chainsaw test --config scripts/e2e/chainsaw.yaml tests/e2e

log "PASS"
