# Helm chart specification

The chart installs one Falcon instance in one Kubernetes namespace. It creates the controller, optional web UI and ZFS usage agent, Services, Gateway API routes, RBAC, and ConfigMap. Mirror and ProxyMirror objects are managed separately; the chart installs only their CRDs.

## Values

`controller.enabled` controls the controller and API. `webui.enabled` adds the static UI. `zfsAgent.enabled` adds the privileged OpenEBS ZFS LocalPV usage collector used by `/api/usage`. `metrics.enabled` and `serviceMonitor.enabled` expose metrics. `admin.enabled` requires `admin.host` and UI, while `catalog.enabled` publishes `/mirrorz.json` for `catalog.hosts`. `global.gatewayRef` is the default Gateway reference and can be overridden per route. Images default to `Chart.AppVersion`; digests override tags.

The chart does not create HPA, PDB, or NetworkPolicy. Controller and UI configuration is rendered into `config.yaml`; invalid values fail Helm rendering and a checksum rolls the controller when config changes.

## Resources

Controller metrics use port 8080 and web API port 80; UI uses port 80; the optional headless ZFS agent uses port 9474. Health endpoints are `/healthz` and `/readyz`. RBAC includes a namespaced Role plus optional cluster permissions for PV affinity and node statistics. With `rbac.create=false`, the Deployment uses the default ServiceAccount.

CRDs in `charts/falcon/crds` are install-only in Helm: upgrades do not update them and uninstall does not delete them. Apply changed CRDs manually before upgrading. CI publishes controller, UI, agent images and the chart from the same immutable `v*` tag.
