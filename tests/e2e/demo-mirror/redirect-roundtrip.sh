#!/usr/bin/env bash
set -euo pipefail

# Exercise redirect transitions against real admission, garbage collection,
# CSI snapshots and the gateway, including a sync while serving is suppressed.
mirror() { kubectl -n mirror get mirror demo -o json; }
wait_for() {
    for ((attempt = 0; attempt < 90; attempt++)); do
        if "$@"; then
            return 0
        fi
        sleep 2
    done
    echo "Timed out: $*" >&2
    return 1
}
redirect_ready() {
    mirror | jq -e '
        .metadata.generation as $generation |
        any(.status.conditions[]; .type == "Ready" and .status == "True"
            and .reason == "RedirectActive" and .observedGeneration == $generation)
    ' >/dev/null
}
catalog_empty() {
    curl -fsS -H "Host: $E2E_SITE_HOST" "http://$E2E_ENVOY_ADDR/mirrorz.json" |
        jq -e '[.mirrors[] | select(.cname == "demo" or .cname == "demo-subset")] | length == 0' >/dev/null
}
new_snapshot_ready() {
    mirror | jq -e --arg old "$original_snapshot" '
        .status.lastSnapshot.name != $old and .status.lastSnapshot.name != null
        and .status.currentSync == null
        and .metadata.annotations["mirrors.zjusct.io/sync-request"] == null
    ' >/dev/null
}
publication_ready() {
    mirror | jq -e --arg snapshot "$latest_snapshot" '
        .metadata.generation as $generation |
        .status.activeSnapshot == $snapshot and .status.publication == null
        and .status.currentSync == null
        and any(.status.conditions[]; .type == "Ready" and .status == "True"
            and .observedGeneration == $generation)
    ' >/dev/null
}
catalog_restored() {
    curl -fsS -H "Host: $E2E_SITE_HOST" "http://$E2E_ENVOY_ADDR/mirrorz.json" |
        jq -e '[.mirrors[] | select(.cname == "demo" or .cname == "demo-subset")] | length == 2' >/dev/null
}

original_snapshot=$(mirror | jq -er '.status.activeSnapshot')
original_template=$(mirror | jq -cS '.spec.publish.http')
kubectl -n mirror annotate mirror demo mirrors.zjusct.io/sync-paused=true
kubectl -n mirror patch mirror demo --type merge -p '{"spec":{"publish":{"redirect":"other.example.org"}}}'
wait_for redirect_ready
kubectl -n mirror wait --for=delete deployment/demo-publish-http --timeout=60s
test -z "$(kubectl -n mirror get service demo-publish-http --ignore-not-found -o name)"
test "$(mirror | jq -cS '.spec.publish.http')" = "$original_template"
wait_for catalog_empty

for path in /demo/index.html /DEMO/index.html /demo-subset/index.html; do
    response=$(curl -fsS -o /dev/null -H "Host: $E2E_SITE_HOST" \
        -w '%{http_code} %{redirect_url}' "http://$E2E_ENVOY_ADDR$path?check=1")
    test "$response" = "302 http://other.example.org$path?check=1"
done

# Manual sync remains available while paused and redirected. It advances
# lastSnapshot without starting a publication or replacing activeSnapshot.
kubectl -n mirror annotate mirror demo mirrors.zjusct.io/sync-request=true
wait_for new_snapshot_ready
latest_snapshot=$(mirror | jq -er '.status.lastSnapshot.name')
last_sync=$(mirror | jq -er '.status.lastSync.jobName')
test "$(mirror | jq -r '.status.activeSnapshot')" = "$original_snapshot"
test -z "$(kubectl -n mirror get deployment demo-publish-http --ignore-not-found -o name)"

kubectl -n mirror patch mirror demo --type json -p '[{"op":"remove","path":"/spec/publish/redirect"}]'
wait_for publication_ready
test "$(mirror | jq -r '.status.lastSync.jobName')" = "$last_sync"
test "$(mirror | jq -cS '.spec.publish.http')" = "$original_template"
test "$(kubectl -n mirror get deployment demo-publish-http -o json |
    jq -r '.spec.template.spec.volumes[] | select(.name == "mirror-data") | .persistentVolumeClaim.claimName')" = "$latest_snapshot"
wait_for catalog_restored
page=$(curl -fsSL --connect-to "$E2E_SITE_HOST:80:$E2E_ENVOY_ADDR" \
    "http://$E2E_SITE_HOST/demo-subset/index.html")
[[ "$page" == *UTC* ]]
