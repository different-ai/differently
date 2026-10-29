{{/* vim: set filetype=mustache: */}}

{{- define "differently.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "differently.fullname" -}}
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

{{- define "differently.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/* Selector labels shared by every component. */}}
{{- define "differently.selectorLabels" -}}
app.kubernetes.io/name: {{ include "differently.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{/* Common labels. */}}
{{- define "differently.labels" -}}
helm.sh/chart: {{ include "differently.chart" . }}
{{ include "differently.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: differently
{{- with .Values.commonLabels }}
{{ toYaml . }}
{{- end }}
{{- end -}}

{{/* Component selector labels. Call with (dict "ctx" $ "component" "server"). */}}
{{- define "differently.componentSelectorLabels" -}}
{{ include "differently.selectorLabels" .ctx }}
app.kubernetes.io/component: {{ .component }}
{{- end -}}

{{- define "differently.componentLabels" -}}
{{ include "differently.labels" .ctx }}
app.kubernetes.io/component: {{ .component }}
{{- end -}}

{{/* Resource names. */}}
{{- define "differently.serverName" -}}
{{- printf "%s-server" (include "differently.fullname" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "differently.configName" -}}
{{- printf "%s-config" (include "differently.fullname" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "differently.postgresqlName" -}}
{{- printf "%s-postgresql" (include "differently.fullname" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "differently.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "differently.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{- define "differently.secretsKeySecretName" -}}
{{- if .Values.secrets.existingSecret -}}
{{- .Values.secrets.existingSecret -}}
{{- else -}}
{{- printf "%s-secrets-key" (include "differently.fullname" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{- define "differently.tokensSecretName" -}}
{{- if .Values.auth.existingSecret -}}
{{- .Values.auth.existingSecret -}}
{{- else -}}
{{- printf "%s-tokens" (include "differently.fullname" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{/*
Image reference. Call with (dict "image" .Values.server.image "ctx" $).
global.imageRegistry replaces the registry; the tag defaults to appVersion;
a digest, when set, pins the image.
*/}}
{{- define "differently.image" -}}
{{- $registry := .image.registry -}}
{{- if .ctx.Values.global.imageRegistry -}}
{{- $registry = .ctx.Values.global.imageRegistry -}}
{{- end -}}
{{- $repo := .image.repository -}}
{{- if $registry -}}
{{- $repo = printf "%s/%s" $registry .image.repository -}}
{{- end -}}
{{- if .image.digest -}}
{{- printf "%s@%s" $repo .image.digest -}}
{{- else -}}
{{- printf "%s:%s" $repo (toString (default .ctx.Chart.AppVersion .image.tag)) -}}
{{- end -}}
{{- end -}}

{{/* Store driver. */}}
{{- define "differently.storeDriver" -}}
{{- $store := .Values.config.store | default dict -}}
{{- default "memory" $store.driver -}}
{{- end -}}

{{- define "differently.usesPostgres" -}}
{{- if eq (include "differently.storeDriver" .) "postgres" -}}true{{- end -}}
{{- end -}}

{{/* Secret and key holding the database DSN. */}}
{{- define "differently.databaseSecretName" -}}
{{- if .Values.postgresql.embedded.enabled -}}
{{- include "differently.postgresqlName" . -}}
{{- else if .Values.postgresql.external.existingSecret -}}
{{- .Values.postgresql.external.existingSecret -}}
{{- else -}}
{{- printf "%s-database" (include "differently.fullname" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{- define "differently.databaseSecretKey" -}}
{{- if and (not .Values.postgresql.embedded.enabled) .Values.postgresql.external.existingSecret -}}
{{- .Values.postgresql.external.existingSecretKey -}}
{{- else -}}
database-url
{{- end -}}
{{- end -}}

{{/* Resolved migration mode: hook | initContainer | "" (none). */}}
{{- define "differently.migrationMode" -}}
{{- if and .Values.migrations.enabled (include "differently.usesPostgres" .) -}}
{{- if eq .Values.migrations.mode "auto" -}}
{{- if .Values.postgresql.embedded.enabled -}}initContainer{{- else -}}hook{{- end -}}
{{- else -}}
{{- .Values.migrations.mode -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{/* In-cluster control-plane URL. */}}
{{- define "differently.internalServerURL" -}}
{{- printf "http://%s.%s.svc.%s:%v" (include "differently.serverName" .) .Release.Namespace .Values.clusterDomain .Values.server.service.port -}}
{{- end -}}

{{/* Enabled providers as a list of dicts with a "kind" key. */}}
{{- define "differently.enabledProviders" -}}
{{- $out := list -}}
{{- range $kind := list "daytona" "freestyle" -}}
{{- $p := index $.Values.providers $kind -}}
{{- if $p.enabled -}}
{{- $out = append $out (merge (dict "kind" $kind) $p) -}}
{{- end -}}
{{- end -}}
{{- toJson $out -}}
{{- end -}}

{{- define "differently.providerSecretName" -}}
{{- if .provider.existingSecret -}}
{{- .provider.existingSecret -}}
{{- else -}}
{{- printf "%s-%s" (include "differently.fullname" .ctx) .provider.kind | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{- define "differently.providerSecretKey" -}}
{{- if .provider.existingSecret -}}
{{- .provider.existingSecretKey -}}
{{- else -}}
api-key
{{- end -}}
{{- end -}}

{{/*
Secret and key holding the OpenSandbox API key: existingSecret when set,
otherwise <fullname>-opensandbox (created by the chart from apiKey, or by the
operator before installing).
*/}}
{{- define "differently.opensandboxSecretName" -}}
{{- if .Values.opensandbox.existingSecret -}}
{{- .Values.opensandbox.existingSecret -}}
{{- else -}}
{{- printf "%s-opensandbox" (include "differently.fullname" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{- define "differently.opensandboxSecretKey" -}}
{{- if .Values.opensandbox.existingSecret -}}
{{- .Values.opensandbox.existingSecretKey -}}
{{- else -}}
api-key
{{- end -}}
{{- end -}}

{{/* The OpenSandbox pool declared by .Values.opensandbox, as a dict. */}}
{{- define "differently.opensandboxPool" -}}
{{- $o := .Values.opensandbox -}}
{{- $c := dict "apiURL" $o.apiURL "isolation" $o.isolation -}}
{{- if not (kindIs "invalid" $o.useServerProxy) }}{{ $_ := set $c "useServerProxy" (toString $o.useServerProxy) }}{{ end -}}
{{- with $o.timeoutSeconds }}{{ $_ := set $c "timeoutSeconds" (toString .) }}{{ end -}}
{{- with $o.image }}{{ $_ := set $c "image" . }}{{ end -}}
{{- with $o.templateId }}{{ $_ := set $c "templateId" . }}{{ end -}}
{{- with $o.lineEndings }}{{ $_ := set $c "lineEndings" . }}{{ end -}}
{{- with $o.runtime }}{{ $_ := set $c "runtime" . }}{{ end -}}
{{- with $o.ports }}{{ $_ := set $c "ports" . }}{{ end -}}
{{- with $o.egress }}{{ $_ := set $c "egress" . }}{{ end -}}
{{- with $o.egressAllow }}{{ $_ := set $c "egressAllow" (join "," .) }}{{ end -}}
{{- with $o.portScheme }}{{ $_ := set $c "portScheme" . }}{{ end -}}
{{- with $o.portURLSeconds }}{{ $_ := set $c "portURLSeconds" (toString .) }}{{ end -}}
{{- with $o.features }}{{ $_ := set $c "features" (join "," .) }}{{ end -}}
{{- $pool := dict "name" $o.pool.name "adapter" "opensandbox" "trust" $o.trust "config" $c "credentials" (dict "env" $o.env) -}}
{{- if $o.pool.region }}{{ $_ := set $pool "region" $o.pool.region }}{{ end -}}
{{- if $o.pool.labels }}{{ $_ := set $pool "labels" $o.pool.labels }}{{ end -}}
{{- if $o.pool.maxConcurrency }}{{ $_ := set $pool "maxConcurrency" $o.pool.maxConcurrency }}{{ end -}}
{{- if $o.pool.costModel }}{{ $_ := set $pool "costModel" $o.pool.costModel }}{{ end -}}
{{- toJson $pool -}}
{{- end -}}

{{/* publicURL: explicit value, else the first ingress host. */}}
{{- define "differently.publicURL" -}}
{{- if .Values.config.publicURL -}}
{{- .Values.config.publicURL -}}
{{- else if and .Values.ingress.enabled .Values.ingress.hosts -}}
{{- printf "%s://%s" (ternary "https" "http" .Values.ingress.tls.enabled) (first .Values.ingress.hosts) -}}
{{- end -}}
{{- end -}}

{{/*
The server configuration file. User keys from .Values.config plus the
chart-managed keys (listen, dataDir, store.driver), the OpenSandbox pool
(first) and provider pools (last).
*/}}
{{- define "differently.config" -}}
{{- $cfg := deepCopy (.Values.config | default dict) -}}
{{- $_ := set $cfg "listen" (printf ":%v" .Values.server.containerPort) -}}
{{- $_ := set $cfg "dataDir" "/var/lib/differently" -}}
{{- if .Values.metrics.port -}}
{{- $_ := set $cfg "metrics" (dict "listen" (printf ":%v" .Values.metrics.port)) -}}
{{- end -}}
{{- $store := deepCopy ($cfg.store | default dict) -}}
{{- $_ := set $store "driver" (include "differently.storeDriver" .) -}}
{{- $_ := set $cfg "store" $store -}}
{{- $public := include "differently.publicURL" . -}}
{{- if $public -}}
{{- $_ := set $cfg "publicURL" $public -}}
{{- else -}}
{{- $_ := unset $cfg "publicURL" -}}
{{- end -}}
{{- $pools := list -}}
{{- if .Values.opensandbox.enabled -}}
{{- $pools = append $pools (include "differently.opensandboxPool" . | fromJson) -}}
{{- end -}}
{{- range ($cfg.pools | default list) -}}
{{- $pools = append $pools . -}}
{{- end -}}
{{- range $p := (include "differently.enabledProviders" . | fromJsonArray) -}}
{{- $pool := dict "name" $p.pool.name "adapter" $p.kind "credentials" (dict "env" $p.env) -}}
{{- if $p.pool.region }}{{ $_ := set $pool "region" $p.pool.region }}{{ end -}}
{{- if $p.pool.labels }}{{ $_ := set $pool "labels" $p.pool.labels }}{{ end -}}
{{- if $p.pool.maxConcurrency }}{{ $_ := set $pool "maxConcurrency" $p.pool.maxConcurrency }}{{ end -}}
{{- if $p.pool.costModel }}{{ $_ := set $pool "costModel" $p.pool.costModel }}{{ end -}}
{{- if $p.pool.config }}{{ $_ := set $pool "config" $p.pool.config }}{{ end -}}
{{- $pools = append $pools $pool -}}
{{- end -}}
{{- $_ := set $cfg "pools" $pools -}}
{{- if not $cfg.templates -}}
{{- $_ := unset $cfg "templates" -}}
{{- end -}}
{{- toYaml $cfg -}}
{{- end -}}

{{/* Environment shared by the server, preflight and migrate containers. */}}
{{- define "differently.serverEnv" -}}
- name: DIFFERENTLY_BOOTSTRAP_TOKEN
  valueFrom:
    secretKeyRef:
      name: {{ include "differently.tokensSecretName" . }}
      key: {{ .Values.auth.bootstrapTokenKey }}
{{- if .Values.secrets.enabled }}
- name: DIFFERENTLY_SECRETS_KEY
  valueFrom:
    secretKeyRef:
      name: {{ include "differently.secretsKeySecretName" . }}
      key: {{ .Values.secrets.existingSecretKey }}
{{- end }}
{{- if include "differently.usesPostgres" . }}
- name: DIFFERENTLY_DATABASE_URL
  valueFrom:
    secretKeyRef:
      name: {{ include "differently.databaseSecretName" . }}
      key: {{ include "differently.databaseSecretKey" . }}
{{- end }}
{{- if .Values.opensandbox.enabled }}
- name: {{ .Values.opensandbox.env }}
  valueFrom:
    secretKeyRef:
      name: {{ include "differently.opensandboxSecretName" . }}
      key: {{ include "differently.opensandboxSecretKey" . }}
{{- end }}
{{- range $p := (include "differently.enabledProviders" . | fromJsonArray) }}
- name: {{ $p.env }}
  valueFrom:
    secretKeyRef:
      name: {{ include "differently.providerSecretName" (dict "provider" $p "ctx" $) }}
      key: {{ include "differently.providerSecretKey" (dict "provider" $p "ctx" $) }}
{{- end }}
{{- with .Values.server.extraEnv }}
{{ toYaml . }}
{{- end }}
{{- end -}}

{{/*
Validation. Fails the render with an actionable message. The server's
`preflight` subcommand repeats the semantic checks at runtime.
*/}}
{{- define "differently.validate" -}}
{{- $driver := include "differently.storeDriver" . -}}
{{- $pg := .Values.postgresql -}}
{{- if .Values.config.dev -}}
{{- fail "config.dev is not supported by this chart: dev mode enables the devhost adapter, which has no isolation." -}}
{{- end -}}
{{- if and (eq $driver "memory") (gt (int .Values.server.replicas) 1) -}}
{{- fail "config.store.driver=memory keeps state in each replica's memory; set server.replicas=1 or use config.store.driver=postgres." -}}
{{- end -}}
{{- if and $pg.embedded.enabled (ne $driver "postgres") -}}
{{- fail "postgresql.embedded.enabled=true needs config.store.driver=postgres." -}}
{{- end -}}
{{- if and $pg.embedded.enabled (or $pg.external.existingSecret $pg.external.url) -}}
{{- fail "Choose one database: postgresql.embedded.enabled or postgresql.external, not both." -}}
{{- end -}}
{{- if and (eq $driver "postgres") (not $pg.embedded.enabled) (not $pg.external.existingSecret) (not $pg.external.url) -}}
{{- fail "config.store.driver=postgres needs a database: set postgresql.external.existingSecret (recommended), postgresql.external.url, or postgresql.embedded.enabled=true (evaluation only)." -}}
{{- end -}}
{{- if and $pg.external.existingSecret $pg.external.url -}}
{{- fail "Set only one of postgresql.external.existingSecret and postgresql.external.url." -}}
{{- end -}}
{{- if and (eq (include "differently.migrationMode" .) "hook") $pg.embedded.enabled -}}
{{- fail "migrations.mode=hook cannot reach postgresql.embedded (it does not exist yet when pre-install hooks run); use migrations.mode=auto or initContainer." -}}
{{- end -}}
{{- if and .Values.auth.existingSecret .Values.auth.bootstrapToken -}}
{{- fail "auth.existingSecret is set, so auth.bootstrapToken would be ignored; remove it." -}}
{{- end -}}
{{- $o := .Values.opensandbox -}}
{{- if $o.enabled -}}
{{- if not $o.apiURL -}}
{{- fail "opensandbox.enabled=true needs opensandbox.apiURL, the OpenSandbox lifecycle API ending in /v1 (e.g. http://opensandbox-server.opensandbox-system.svc/v1). Install OpenSandbox separately, or set opensandbox.enabled=false." -}}
{{- end -}}
{{- if not (regexMatch "^https?://" $o.apiURL) -}}
{{- fail (printf "opensandbox.apiURL=%q must be an http:// or https:// URL." $o.apiURL) -}}
{{- end -}}
{{- if not (has $o.trust (list "private" "external")) -}}
{{- fail (printf "opensandbox.trust=%q must be private (you operate the OpenSandbox server) or external (someone else does)." (toString $o.trust)) -}}
{{- end -}}
{{- if not (has $o.isolation (list "container" "microvm")) -}}
{{- fail (printf "opensandbox.isolation=%q must be container (Docker or Kubernetes runtime) or microvm (Fast Sandbox)." (toString $o.isolation)) -}}
{{- end -}}
{{- if eq (toString $o.runtime) "fastsandbox" -}}
{{- if not $o.templateId -}}
{{- fail "opensandbox.runtime=fastsandbox needs opensandbox.templateId: Fast Sandbox creates microVMs from a template's golden image, not from an image." -}}
{{- end -}}
{{- if ne $o.isolation "microvm" -}}
{{- fail "opensandbox.runtime=fastsandbox runs Firecracker microVMs; set opensandbox.isolation=microvm." -}}
{{- end -}}
{{- if $o.ports -}}
{{- fail "opensandbox.ports=signed needs secure access, which Fast Sandbox templates reject; leave opensandbox.ports empty." -}}
{{- end -}}
{{- end -}}
{{- if and $o.existingSecret $o.apiKey -}}
{{- fail "Set only one of opensandbox.existingSecret and opensandbox.apiKey." -}}
{{- end -}}
{{- end -}}
{{- if and .Values.auth.bootstrapToken (not (regexMatch "^dly_[0-9a-z]+_[0-9a-z]{24,}$" .Values.auth.bootstrapToken)) -}}
{{- fail "auth.bootstrapToken must look like dly_<id>_<secret> with a secret of at least 24 characters from [0-9a-z]." -}}
{{- end -}}
{{- $cfg := include "differently.config" . | fromYaml -}}
{{- $names := dict -}}
{{- range $cfg.pools -}}
{{- if hasKey $names .name -}}
{{- fail (printf "Duplicate pool %q (check config.pools, opensandbox.pool.name and providers.*.pool.name)." .name) -}}
{{- end -}}
{{- $_ := set $names .name .adapter -}}
{{- if and (has .adapter (list "daytona" "freestyle")) (not .credentials) -}}
{{- fail (printf "Pool %q uses the external %s adapter but has no credentials reference; use providers.%s or set credentials: {env: NAME} and server.extraEnv." .name .adapter .adapter) -}}
{{- end -}}
{{- if eq .adapter "devhost" -}}
{{- fail (printf "Pool %q: the devhost adapter has no isolation and is not supported by this chart." .name) -}}
{{- end -}}
{{- if eq .adapter "opensandbox" -}}
{{- if not (has (.trust | default "") (list "private" "external")) -}}
{{- fail (printf "Pool %q uses the opensandbox adapter: set trust to private (you operate the server) or external (someone else does)." .name) -}}
{{- end -}}
{{- if not (.config | default dict).apiURL -}}
{{- fail (printf "Pool %q uses the opensandbox adapter and needs config.apiURL." .name) -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- range (($cfg.policy | default dict).externalPools | default list) -}}
{{- if not (hasKey $names .) -}}
{{- fail (printf "config.policy.externalPools names unknown pool %q." .) -}}
{{- end -}}
{{- end -}}
{{- range $p := (include "differently.enabledProviders" . | fromJsonArray) -}}
{{- if and (not $p.existingSecret) (not $p.apiKey) -}}
{{- fail (printf "providers.%s.enabled=true needs providers.%s.existingSecret (recommended) or providers.%s.apiKey." $p.kind $p.kind $p.kind) -}}
{{- end -}}
{{- if and $p.existingSecret $p.apiKey -}}
{{- fail (printf "Set only one of providers.%s.existingSecret and providers.%s.apiKey." $p.kind $p.kind) -}}
{{- end -}}
{{- end -}}
{{- if and .Values.ingress.enabled (not .Values.ingress.hosts) -}}
{{- fail "ingress.enabled=true needs at least one entry in ingress.hosts." -}}
{{- end -}}
{{- if and .Values.ingress.tls.certManager.clusterIssuer .Values.ingress.tls.certManager.issuer -}}
{{- fail "Set only one of ingress.tls.certManager.clusterIssuer and ingress.tls.certManager.issuer." -}}
{{- end -}}
{{- end -}}
