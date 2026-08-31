# falcon Helm chart

Deploys the Falcon stack into a single namespace (one full stack per
namespace: controller, admin web UI, metrics, admin/catalog HTTPRoutes).
The Mirror/ProxyMirror CR instances themselves are not managed by this chart —
keep them in your GitOps repository.

## Install

```sh
helm install falcon oci://ghcr.io/<owner>/charts/falcon \
  --namespace mirror-production --create-namespace \
  -f my-values.yaml
```

The release namespace holds every chart resource; the controller only
watches/manages that namespace (`POD_NAMESPACE` is wired to
`metadata.namespace` automatically).

## CRDs: `helm install` does not upgrade them

The Mirror/ProxyMirror CRDs live in `crds/` following the Helm CRD convention:

- `helm install` applies them (skipping if they already exist);
- **`helm upgrade` never touches them**;
- they are not deleted on `helm uninstall` (orphaned on purpose, CRs would
  otherwise be garbage-collected).

Therefore, whenever the generated CRDs change (see the README dev section for
the controller-gen invocation), apply them manually before upgrading the
release:

```sh
kubectl apply -f charts/falcon/crds/
```

## Controller configuration

All runtime configuration is rendered into the `falcon-config` ConfigMap
(key `config.yaml`) from `controller.config` in values, matching the schema in
`internal/config/config.go`. The Deployment carries only
`--config=/etc/falcon/config.yaml`; a `checksum/config` pod annotation makes
any config change roll the pods. Note that `serving.gatewayRef` falls back to
`global.gatewayRef`, and that the probe/Service ports assume the default API
listen addresses (`:8080/:8081/:8082`) — the template refuses to render
anything else.

## Publishing (OCI)

- CI (on tag `falcon-chart-v*`): pushes to
  `oci://ghcr.io/<owner>/charts/falcon` — see
  `.github/workflows/release-chart.yml`.
- Testing against the ZJUSCT harbor (reuses the docker login):

  ```sh
  helm lint charts/falcon
  helm package charts/falcon
  helm push falcon-<version>.tgz oci://harbor.s.zjusct.io/library/charts
  helm show chart oci://harbor.s.zjusct.io/library/charts/falcon
  ```
