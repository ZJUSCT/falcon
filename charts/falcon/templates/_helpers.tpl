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
Extract the numeric port of a ":<port>" listen address. Probes and Services
reach the listener on every interface, so an address bound to a specific IP
is rejected here.
Usage: include "falcon.port" ":8080"
*/}}
{{- define "falcon.port" -}}
{{- $addr := . | default "" -}}
{{- if not (regexMatch "^:[0-9]+$" $addr) -}}
{{- fail (printf "listen address %q must be a bare \":<port>\" (all-interfaces) address" $addr) -}}
{{- end -}}
{{- trimPrefix ":" $addr -}}
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
  - log.level must be one of debug|info|warn|error;
  - mirrorz.site.url is required (with a scheme) while mirrorz.enabled;
  - publish.http.hostnames entries must be bare, non-empty hostnames;
  - publish.http.gatewayRef.name is required when publish.http.hostnames is set.

The OAuth clientSecret never appears here: when configured it is injected
through the FALCON_ADMIN_OAUTH_CLIENT_SECRET environment variable and
referenced via ${...} expansion.
*/}}
{{- define "falcon.config" -}}
{{- $cfg := .Values.controller.config -}}
{{- $http := $cfg.publish.http | default dict -}}
{{- $logLevel := $cfg.log.level | default "info" -}}
{{- if not (has $logLevel (list "debug" "info" "warn" "error")) -}}
{{- fail (printf "controller.config.log.level %q is not one of debug, info, warn, error" $logLevel) -}}
{{- end -}}
{{- $site := $cfg.mirrorz.site | default dict -}}
{{- $mirrorzEnabled := $cfg.mirrorz.enabled | default false -}}
{{- $siteURL := $site.url | default "" -}}
{{- if $mirrorzEnabled -}}
{{- if eq (trim $siteURL) "" -}}
{{- fail "controller.config.mirrorz.site.url must not be empty while mirrorz.enabled is set" -}}
{{- end -}}
{{- if not (contains "://" $siteURL) -}}
{{- fail (printf "controller.config.mirrorz.site.url %q must carry a scheme (e.g. https://...)" $siteURL) -}}
{{- end -}}
{{- end -}}
{{- $hostnames := $http.hostnames | default list -}}
{{- range $host := $hostnames -}}
{{- if eq (trim $host) "" -}}
{{- fail "controller.config.publish.http.hostnames must not contain empty entries" -}}
{{- end -}}
{{- if contains "/" $host -}}
{{- fail (printf "controller.config.publish.http.hostnames entry %q must be a bare hostname" $host) -}}
{{- end -}}
{{- end -}}
{{- $gw := $http.gatewayRef | default dict -}}
{{- if and (gt (len $hostnames) 0) (not (index $gw "name" | default "")) -}}
{{- fail "controller.config.publish.http.gatewayRef.name is required when publish.http.hostnames is set" -}}
{{- end -}}
{{- $secretConfigured := or .Values.ui.oauth.clientSecret .Values.ui.oauth.existingSecret -}}
{{- if and .Values.ui.oauth.clientSecret .Values.ui.oauth.existingSecret -}}
{{- fail "ui.oauth.clientSecret and ui.oauth.existingSecret are mutually exclusive" -}}
{{- end -}}
{{- if and .Values.ui.enabled (not $secretConfigured) -}}
{{- fail "ui.enabled requires ui.oauth.clientSecret or ui.oauth.existingSecret" -}}
{{- end -}}
{{- if and $secretConfigured (not .Values.ui.oauth.clientID) -}}
{{- fail "ui.oauth.clientSecret/existingSecret require ui.oauth.clientID" -}}
{{- end -}}
{{- $uiHostnames := .Values.ui.route.hostnames | default list -}}
{{- if and .Values.ui.enabled (eq (len $uiHostnames) 0) -}}
{{- fail "ui.enabled requires ui.route.hostnames" -}}
{{- end -}}
{{- if and .Values.ui.enabled (eq (len (.Values.ui.route.parentRefs | default list)) 0) -}}
{{- fail "ui.enabled requires ui.route.parentRefs" -}}
{{- end -}}
log:
  level: {{ $logLevel | quote }}
api:
  metricsBindAddress: ":8080"
  healthProbeBindAddress: ":8081"
  mirrorzBindAddress: ":8082"
  adminBindAddress: ":8083"
mirrorz:
  enabled: {{ $mirrorzEnabled }}
  site:
    url: {{ $siteURL | quote }}
    {{- with $site.abbr }}
    abbr: {{ . | quote }}
    {{- end }}
    {{- with $site.name }}
    name: {{ . | quote }}
    {{- end }}
    {{- range $k := list "logo" "logo_darkmode" "homepage" "issue" "request" "email" "group" "disk" "note" "big" }}
    {{- with index $site $k }}
    {{ $k }}: {{ . | quote }}
    {{- end }}
    {{- end }}
    disable: {{ $site.disable | default false }}
sync:
  maxConcurrent: {{ $cfg.sync.maxConcurrent | default 0 }}
admin:
  enabled: {{ .Values.ui.enabled }}
  {{- with .Values.ui.oauth }}
  {{- if .clientID }}
  host: {{ index $uiHostnames 0 | quote }}
  oauth:
    clientID: {{ .clientID | quote }}
    clientSecret: {{ if $secretConfigured }}"${FALCON_ADMIN_OAUTH_CLIENT_SECRET}"{{ else }}""{{ end }}
    allowedUserIDs:
      {{- range (.allowedUserIDs | default list) }}
      - {{ . }}
      {{- end }}
  {{- end }}
  {{- end }}
publish:
  http:
    {{- with $gw }}
    gatewayRef:
{{ toYaml . | indent 6 }}
    {{- end }}
    {{- with $hostnames }}
    hostnames:
{{ toYaml . | indent 6 }}
    {{- end }}
    labels: {{ toYaml ($http.labels | default dict) | trim }}
    annotations: {{ toYaml ($http.annotations | default dict) | trim }}
{{- end -}}
