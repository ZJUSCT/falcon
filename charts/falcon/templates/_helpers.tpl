{{/* vim: set filetype=mustache: */}}

{{/*
Expand the name of the chart.
*/}}
{{- define "falcon.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
Create a default fully qualified app name (release name based).
We truncate at 63 chars because some Kubernetes name fields are limited to this.
*/}}
{{- define "falcon.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- $name := default .Chart.Name .Values.nameOverride -}}
{{- if contains $name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{/*
Chart name and version for the helm.sh/chart label.
*/}}
{{- define "falcon.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
Common labels.
*/}}
{{- define "falcon.labels" -}}
helm.sh/chart: {{ include "falcon.chart" . }}
{{ include "falcon.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{/*
Selector labels (shared by resources and their pod templates / service selectors).
*/}}
{{- define "falcon.selectorLabels" -}}
app.kubernetes.io/name: {{ include "falcon.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{/*
Name of the service account used by the controller. When rbac.create is
disabled the chart does not create any RBAC objects and the workload falls
back to the namespace's "default" service account.
*/}}
{{- define "falcon.serviceAccountName" -}}
{{- if .Values.controller.rbac.create -}}
{{ include "falcon.fullname" . }}
{{- else -}}
default
{{- end -}}
{{- end -}}

{{/*
Merge a section-local gatewayRef over global.gatewayRef and emit it as YAML.
Keys of the section that are present with an empty string value unset the
global value (so `namespace: ""` deliberately means "same namespace as the
release"); keys that are absent inherit the global value. The returned YAML
contains only non-empty values, which keeps the controller's config schema
(json omitempty) clean.
Usage: include "falcon.mergeGatewayRef" (dict "ctx" $ "section" .Values.foo.gatewayRef)
*/}}
{{- define "falcon.mergeGatewayRef" -}}
{{- $section := .section | default dict -}}
{{- $ref := deepCopy (.ctx.Values.global.gatewayRef | default dict) -}}
{{- range $key, $value := $section -}}
{{- if or (kindIs "invalid" $value) (eq (toString $value) "") -}}
{{- $_ := unset $ref $key -}}
{{- else -}}
{{- $_ := set $ref $key $value -}}
{{- end -}}
{{- end -}}
{{- $out := dict -}}
{{- range $key := list "name" "namespace" "sectionName" -}}
{{- if index $ref $key -}}{{- $_ := set $out $key (index $ref $key) -}}{{- end -}}
{{- end -}}
{{- toYaml $out -}}
{{- end -}}

{{/*
Render the `parentRefs` list of an HTTPRoute. When `.parentRefs` is non-empty
it is emitted verbatim (advanced escape hatch); otherwise a single parentRef
is derived from the section's gatewayRef merged over global.gatewayRef.
The namespace key is omitted from the derived parentRef when it is empty
(same-namespace reference in the Gateway API).
Usage: include "falcon.parentRefs" (dict "ctx" $ "section" .Values.admin.route.gatewayRef "parentRefs" .Values.admin.route.parentRefs)
*/}}
{{- define "falcon.parentRefs" -}}
{{- $custom := .parentRefs | default list -}}
{{- if gt (len $custom) 0 -}}
{{- toYaml $custom -}}
{{- else -}}
{{- $ref := fromYaml (include "falcon.mergeGatewayRef" (dict "ctx" .ctx "section" .section)) -}}
{{- if not $ref.name -}}
{{- fail (printf "gatewayRef.name is empty: set global.gatewayRef.name, the section gatewayRef, or route.parentRefs") -}}
{{- end -}}
- group: gateway.networking.k8s.io
  kind: Gateway
  name: {{ $ref.name | quote }}
  {{- with $ref.namespace }}
  namespace: {{ . | quote }}
  {{- end }}
  {{- with $ref.sectionName }}
  sectionName: {{ . | quote }}
  {{- end }}
{{- end -}}
{{- end -}}

{{/*
Build the container image reference: [registry/]repository[:tag|@digest].
An empty image.tag falls back to the given defaultTag (callers pass
Chart.AppVersion for the controller). image.digest, when set, wins over the tag.
Usage: include "falcon.image" (dict "image" .Values.controller.image "defaultTag" .Chart.AppVersion)
*/}}
{{- define "falcon.image" -}}
{{- $image := .image -}}
{{- $tag := $image.tag | default .defaultTag -}}
{{- $registry := $image.registry | default .ctx.Values.global.imageRegistry -}}
{{- with $registry -}}
{{- . }}/{{- end -}}
{{- $image.repository -}}
{{- if $image.digest -}}
@{{ $image.digest }}
{{- else -}}
:{{ $tag }}
{{- end -}}
{{- end -}}

{{/*
Render the controller config file (config.yaml) exactly in the schema of
internal/config (see internal/config/config.go). Rendered both into the
falcon-config ConfigMap and (hashed) into the controller pod's
checksum/config annotation, so any config change rolls the Deployment.

Mirrors internal/config Config.Validate():
  - site.url is required and must carry a scheme;
  - log.level must be one of debug|info|warn|error;
  - publish.hostnames entries must be bare, non-empty hostnames;
  - publish.gatewayRef.name is required when publish.hostnames is set.
*/}}
{{- define "falcon.config" -}}
{{- $cfg := .Values.controller.config -}}
{{- $logLevel := $cfg.log.level | default "info" -}}
{{- if not (has $logLevel (list "debug" "info" "warn" "error")) -}}
{{- fail (printf "controller.config.log.level %q is not one of debug, info, warn, error" $logLevel) -}}
{{- end -}}
{{- $siteURL := required "controller.config.site.url must not be empty" $cfg.site.url -}}
{{- if not (contains "://" $siteURL) -}}
{{- fail (printf "controller.config.site.url %q must carry a scheme (e.g. https://...)" $siteURL) -}}
{{- end -}}
{{- range $host := $cfg.publish.hostnames | default list -}}
{{- if eq (trim $host) "" -}}
{{- fail "controller.config.publish.hostnames must not contain empty entries" -}}
{{- end -}}
{{- if contains "/" $host -}}
{{- fail (printf "controller.config.publish.hostnames entry %q must be a bare hostname" $host) -}}
{{- end -}}
{{- end -}}
{{- $gw := fromYaml (include "falcon.mergeGatewayRef" (dict "ctx" $ "section" $cfg.publish.gatewayRef)) -}}
{{- if and (gt (len ($cfg.publish.hostnames | default list)) 0) (not $gw.name) -}}
{{- fail "controller.config.publish.gatewayRef.name is required when publish.hostnames is set" -}}
{{- end -}}
log:
  level: {{ $logLevel | quote }}
api:
  metricsBindAddress: {{ $cfg.api.metricsBindAddress | default ":8080" | quote }}
  healthProbeBindAddress: {{ $cfg.api.healthProbeBindAddress | default ":8081" | quote }}
  webapiBindAddress: {{ $cfg.api.webapiBindAddress | default ":8082" | quote }}
site:
  url: {{ $siteURL | quote }}
  {{- with $cfg.site.abbr }}
  abbr: {{ . | quote }}
  {{- end }}
  {{- with $cfg.site.name }}
  name: {{ . | quote }}
  {{- end }}
catalog:
  enabled: {{ $cfg.catalog.enabled }}
sync:
  maxConcurrent: {{ $cfg.sync.maxConcurrent | default 0 }}
admin:
  {{- $cfgAdmin := $cfg.admin | default dict }}
  {{- $adminHost := .Values.admin.host | default $cfgAdmin.host }}
  {{- with $adminHost }}
  host: {{ . | quote }}
  {{- end }}
auth:
  github:
    clientID: {{ $cfg.auth.github.clientID | default "" | quote }}
    clientSecret: {{ $cfg.auth.github.clientSecret | default "" | quote }}
    allowedUserIDs:
      {{- range ($cfg.auth.github.allowedUserIDs | default list) }}
      - {{ . }}
      {{- end }}
publish:
  {{- if $gw }}
  gatewayRef:
{{ toYaml $gw | indent 4 }}
  {{- end }}
  {{- with $cfg.publish.hostnames | default list }}
  hostnames:
{{ toYaml . | indent 4 }}
  {{- end }}
  labels: {{ toYaml ($cfg.publish.labels | default dict) | trim }}
  annotations: {{ toYaml ($cfg.publish.annotations | default dict) | trim }}
{{- end -}}
