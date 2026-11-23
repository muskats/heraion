{{/* ---------- names and labels ---------- */}}
{{- define "heraion.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "heraion.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- $name := default .Chart.Name .Values.nameOverride }}
{{- if contains $name .Release.Name }}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}
{{- end }}

{{- define "heraion.namespace" -}}{{ .Values.namespaceOverride | default .Release.Namespace }}{{- end }}

{{- define "heraion.selectorLabels" -}}
app.kubernetes.io/name: {{ include "heraion.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/* dict "root" $ "component" "worker" */}}
{{- define "heraion.componentSelector" -}}
{{ include "heraion.selectorLabels" .root }}
app.kubernetes.io/component: {{ .component }}
{{- end }}

{{- define "heraion.labels" -}}
{{ include "heraion.componentSelector" . }}
helm.sh/chart: {{ printf "%s-%s" .root.Chart.Name .root.Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
app.kubernetes.io/version: {{ .root.Values.image.tag | default .root.Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .root.Release.Service }}
app.kubernetes.io/part-of: heraion
{{- with .root.Values.commonLabels }}
{{ toYaml . }}
{{- end }}
{{- end }}

{{/* dict "root" $ "component" "gateway" "name" "x-heraion-gateway" ["namespace" "shop"] ["labels" (dict "a" "b")] */}}
{{- define "heraion.metadata" -}}
name: {{ .name }}
namespace: {{ .namespace | default (include "heraion.namespace" .root) }}
labels:
  {{- include "heraion.labels" . | nindent 2 }}
  {{- with .labels }}
  {{- toYaml . | nindent 2 }}
  {{- end }}
{{- with .root.Values.commonAnnotations }}
annotations:
  {{- toYaml . | nindent 2 }}
{{- end }}
{{- end }}

{{/*
image.registry + image.repository, then @digest or :tag. global.imageRegistry
replaces image.registry (e.g. a mirror); images without a registry field are
used as given.
*/}}
{{- define "heraion.image" -}}
{{- $repo := .image.repository -}}
{{- with .image.registry }}{{ $repo = printf "%s/%s" ($.root.Values.global.imageRegistry | default .) $repo }}{{ end -}}
{{- if .image.digest -}}
{{ $repo }}@{{ .image.digest }}
{{- else -}}
{{ $repo }}:{{ .image.tag | default .root.Chart.AppVersion }}
{{- end }}
{{- end }}

{{- define "heraion.secretName" -}}{{ include "heraion.fullname" . }}-secrets{{- end }}
{{- define "heraion.configName" -}}{{ include "heraion.fullname" . }}-config{{- end }}
{{- define "heraion.playbooksName" -}}{{ include "heraion.fullname" . }}-playbooks{{- end }}
{{- define "heraion.jobTemplateName" -}}{{ include "heraion.fullname" . }}-job-template{{- end }}

{{- define "heraion.workerSA" -}}
{{- if .Values.worker.serviceAccount.create }}{{ default (printf "%s-worker" (include "heraion.fullname" .)) .Values.worker.serviceAccount.name }}
{{- else }}{{ default "default" .Values.worker.serviceAccount.name }}{{- end }}
{{- end }}

{{- define "heraion.gatewaySA" -}}
{{- if .Values.gateway.serviceAccount.create }}{{ default (printf "%s-gateway" (include "heraion.fullname" .)) .Values.gateway.serviceAccount.name }}
{{- else }}{{ default "default" .Values.gateway.serviceAccount.name }}{{- end }}
{{- end }}

{{- define "heraion.mcpName" -}}{{ printf "%s-mcp-%s" (include "heraion.fullname" .root) .name | trunc 63 | trimSuffix "-" }}{{- end }}

{{/* ---------- credentials ----------
Each credential is resolved by the binary at runtime: 1 when set (a broken
file is an error, not a fallthrough), otherwise the first of 2 and 3 with a value:
  1. <ENV>_FILE      serviceAccountToken (projected token) or csiKey (Secrets Store CSI file)
  2. <ENV>           existingSecret / key, default secrets.existingSecret; optional key
  3. <ENV>_FALLBACK  value, stored in the chart's Secret
*/}}

{{/* datadog + DD-API-KEY -> dd-api-key */}}
{{- define "heraion.keySuffix" -}}{{ regexReplaceAll "[^a-z0-9.-]" (lower .) "-" }}{{- end }}
{{/* datadog + DD-API-KEY -> HERAION_MCP_DATADOG_DD_API_KEY */}}
{{- define "heraion.mcpHeaderEnv" -}}
{{- regexReplaceAll "[^A-Z0-9_]" (printf "HERAION_MCP_%s_%s" .server .header | upper) "_" }}
{{- end }}
{{- define "heraion.hookEnv" -}}{{ printf "HERAION_HOOK_%s_SECRET" (. | upper | replace "-" "_") }}{{- end }}

{{/*
Credentials of a role as {items: [{env, key, ref}]}. key is the default key
in secrets.existingSecret and in the chart Secret, e.g. llm-api-key.
dict "root" $ "role" "worker"|"gateway"
*/}}
{{- define "heraion.credentials" -}}
{{- $v := .root.Values -}}
{{- $out := list -}}
{{- if eq .role "worker" -}}
{{- $out = append $out (dict "env" "HERAION_LLM_API_KEY" "key" "llm-api-key" "ref" $v.llm.apiKey) -}}
{{- $out = append $out (dict "env" "HERAION_GIT_TOKEN" "key" "git-token" "ref" $v.git.token) -}}
{{- range $n, $s := $v.mcpServers }}{{ if $s.enabled }}{{ range $h, $ref := $s.secretHeaders -}}
{{- $out = append $out (dict "env" (include "heraion.mcpHeaderEnv" (dict "server" $n "header" $h)) "key" (printf "mcp-%s-%s" $n (include "heraion.keySuffix" $h)) "ref" $ref) -}}
{{- end }}{{ end }}{{ end -}}
{{- else -}}
{{- range $n, $h := $v.gateway.hooks -}}
{{- $out = append $out (dict "env" (include "heraion.hookEnv" $n) "key" (printf "hook-%s" $n) "ref" ($h.secret | default dict)) -}}
{{- end -}}
{{- end -}}
{{- toYaml (dict "items" $out) }}
{{- end }}

{{/* "true" if a ref has any source. dict "root" $ "ref" ref */}}
{{- define "heraion.credConfigured" -}}
{{- $r := .ref | default dict -}}
{{- if or ($r.serviceAccountToken | default dict).audience $r.csiKey $r.existingSecret .root.Values.secrets.existingSecret $r.value }}true{{ end -}}
{{- end }}

{{/* Env entries for a credential list. dict "root" $ "items" list */}}
{{- define "heraion.credEnv" -}}
{{- $root := .root -}}
{{- $csi := $root.Values.secrets.csi -}}
{{- range .items }}
{{- $env := .env }}{{ $key := .key }}{{ $ref := .ref | default dict }}
{{- $sa := $ref.serviceAccountToken | default dict }}
{{- if $sa.audience }}
- name: {{ $env }}_FILE
  value: /var/run/secrets/heraion/sa/{{ $key }}
{{- else if $ref.csiKey }}
{{- if not $csi.enabled }}{{ fail (printf "%s uses csiKey %q but secrets.csi.enabled is false" $env $ref.csiKey) }}{{ end }}
- name: {{ $env }}_FILE
  value: {{ $csi.mountPath }}/{{ $ref.csiKey }}
{{- end }}
{{- $name := $ref.existingSecret | default $root.Values.secrets.existingSecret }}
{{- if $name }}
- name: {{ $env }}
  valueFrom:
    secretKeyRef: { name: {{ tpl $name $root }}, key: {{ $ref.key | default $key }}, optional: true }
{{- end }}
{{- if $ref.value }}
- name: {{ $env }}_FALLBACK
  valueFrom:
    secretKeyRef: { name: {{ include "heraion.secretName" $root }}, key: {{ $key }} }
{{- end }}
{{- end }}
{{- end }}

{{/* Which file sources a credential list uses, e.g. {sa: true, csi: false}. dict "root" $ "items" list */}}
{{- define "heraion.credSources" -}}
{{- $sa := false -}}{{- $csi := false -}}
{{- range .items }}{{ $r := .ref | default dict -}}
{{- if ($r.serviceAccountToken | default dict).audience }}{{ $sa = true }}{{ end -}}
{{- if $r.csiKey }}{{ $csi = true }}{{ end -}}
{{- end -}}
{{- toYaml (dict "sa" $sa "csi" (and $csi .root.Values.secrets.csi.enabled)) }}
{{- end }}

{{/* Volumes for a credential list: projected SA tokens and the CSI mount. */}}
{{- define "heraion.credVolumes" -}}
{{- $sources := list -}}
{{- range .items }}{{ $key := .key }}{{ with (.ref | default dict).serviceAccountToken }}{{ if .audience -}}
{{- $sources = append $sources (dict "serviceAccountToken" (dict "audience" .audience "expirationSeconds" (.expirationSeconds | default 3600 | int) "path" $key)) -}}
{{- end }}{{ end }}{{ end -}}
{{- if $sources }}
- name: sa-tokens
  projected:
    sources:
      {{- toYaml $sources | nindent 6 }}
{{- end }}
{{- $use := include "heraion.credSources" . | fromYaml -}}
{{- with .root.Values.secrets.csi }}{{ if $use.csi }}
- name: csi-secrets
  csi:
    driver: secrets-store.csi.k8s.io
    readOnly: true
    volumeAttributes:
      secretProviderClass: {{ required "secrets.csi.secretProviderClass is required" .secretProviderClass }}
{{- end }}{{ end }}
{{- end }}

{{/* Mounts for the volumes of heraion.credVolumes. */}}
{{- define "heraion.credMounts" -}}
{{- $use := include "heraion.credSources" . | fromYaml -}}
{{- if $use.sa }}
- { name: sa-tokens, mountPath: /var/run/secrets/heraion/sa, readOnly: true }
{{- end }}
{{- if $use.csi }}
- { name: csi-secrets, mountPath: {{ .root.Values.secrets.csi.mountPath }}, readOnly: true }
{{- end }}
{{- end }}

{{/* Inline values by chart Secret key, e.g. {llm-api-key: sk-..., mcp-custom-env-api-token: ...}. */}}
{{- define "heraion.inlineSecrets" -}}
{{- $out := dict -}}
{{- range $role := list "worker" "gateway" }}
{{- range (include "heraion.credentials" (dict "root" $ "role" $role) | fromYaml).items }}
{{- $key := .key }}{{ with (.ref | default dict).value }}{{ $_ := set $out $key . }}{{ end }}
{{- end }}
{{- end }}
{{- range $n, $s := .Values.mcpServers }}{{ if and $s.enabled (eq $s.mode "deploy") }}
{{- range $e, $ref := ($s.deploy.secretEnv | default dict) }}{{ with $ref.value }}{{ $_ := set $out (printf "mcp-%s-env-%s" $n (include "heraion.keySuffix" $e)) . }}{{ end }}{{ end }}
{{- end }}{{ end }}
{{- toYaml $out }}
{{- end }}

{{/* ---------- playbooks ---------- */}}

{{/* Enabled playbooks as YAML, without the chart-only keys enabled and suspend. */}}
{{- define "heraion.playbooks" -}}
{{- $out := dict -}}
{{- range $n, $p := .Values.playbooks }}
{{- if and $p (ne (toString $p.enabled) "false") }}
{{- $_ := set $out $n (merge (dict "name" $n) (omit $p "enabled" "suspend")) }}
{{- end }}
{{- end }}
{{- toYaml $out }}
{{- end }}

{{/* Fails unless each webhook playbook has a gateway.hooks entry with a secret. */}}
{{- define "heraion.checkHooks" -}}
{{- range $n, $p := (include "heraion.playbooks" . | fromYaml) }}
{{- with ($p.trigger | default dict).webhook }}
{{- $h := index $.Values.gateway.hooks . }}
{{- if not $h }}
{{- fail (printf "playbook %s uses webhook %q: add gateway.hooks.%s" $n . .) }}
{{- end }}
{{- if not (include "heraion.credConfigured" (dict "root" $ "ref" $h.secret)) }}
{{- fail (printf "gateway.hooks.%s.secret: set serviceAccountToken, csiKey, existingSecret or value (or secrets.existingSecret)" .) }}
{{- end }}
{{- end }}
{{- end }}
{{- end }}

{{- define "heraion.gatewayEnabled" -}}
{{- $any := false -}}
{{- range $n, $p := (include "heraion.playbooks" . | fromYaml) }}{{ if ($p.trigger | default dict).webhook }}{{ $any = true }}{{ end }}{{ end -}}
{{- if $any }}true{{ end -}}
{{- end }}

{{/* ---------- runtime config ---------- */}}
{{- define "heraion.llmBaseURL" -}}
{{- if .Values.llm.baseURL }}{{ tpl .Values.llm.baseURL . }}
{{- else if .Values.litellm.enabled }}http://{{ .Release.Name }}-litellm-gateway:4000/v1
{{- else }}{{ fail "llm.baseURL is required (or enable litellm)" }}
{{- end }}
{{- end }}

{{- define "heraion.config" -}}
{{- $v := .Values -}}
{{- $llm := dict "baseURL" (include "heraion.llmBaseURL" .) "model" (required "llm.model is required" $v.llm.model) "maxSteps" $v.llm.maxSteps "maxTokens" $v.llm.maxTokens "timeout" $v.llm.timeout "maxToolResultBytes" $v.llm.maxToolResultBytes "maxContextBytes" $v.llm.maxContextBytes -}}
{{- if hasKey $v.llm "temperature" }}{{ $_ := set $llm "temperature" $v.llm.temperature }}{{ end -}}
{{- $git := dict "provider" $v.git.provider "authorName" $v.git.authorName "authorEmail" $v.git.authorEmail -}}
{{- range $k := list "endpoint" "cloneBaseURL" "username" }}{{ with index $v.git $k }}{{ $_ := set $git $k . }}{{ end }}{{ end -}}
{{- $mcp := dict -}}
{{- range $n, $s := $v.mcpServers }}{{ if $s.enabled -}}
{{- $url := "" -}}
{{- if eq $s.mode "deploy" -}}
{{- $url = printf "http://127.0.0.1:%v%s" $s.deploy.port ($s.path | default "/mcp") -}}
{{- else -}}
{{- $url = tpl (required (printf "mcpServers.%s.url is required for mode %s" $n $s.mode) $s.url) $ -}}
{{- end -}}
{{- $e := dict "url" $url "transport" ($s.transport | default "streamable") -}}
{{- range $k := list "allowTools" "denyTools" "headers" "timeout" "required" }}{{ with index $s $k }}{{ $_ := set $e $k . }}{{ end }}{{ end -}}
{{- $hdrs := dict -}}{{- $prefixes := dict -}}
{{- range $h, $ref := $s.secretHeaders }}{{ if include "heraion.credConfigured" (dict "root" $ "ref" $ref) -}}
{{- $_ := set $hdrs $h (include "heraion.mcpHeaderEnv" (dict "server" $n "header" $h)) -}}
{{- $prefix := $ref.prefix -}}
{{- if and (not (hasKey $ref "prefix")) ($ref.serviceAccountToken | default dict).audience (eq (lower $h) "authorization") }}{{ $prefix = "Bearer " }}{{ end -}}
{{- with $prefix }}{{ $_ := set $prefixes $h . }}{{ end -}}
{{- end }}{{ end -}}
{{- if $hdrs }}{{ $_ := set $e "headersFromEnv" $hdrs }}{{ end -}}
{{- if $prefixes }}{{ $_ := set $e "headerPrefixes" $prefixes }}{{ end -}}
{{- $_ := set $mcp $n $e -}}
{{- end }}{{ end -}}
{{- $hooks := dict -}}
{{- range $n, $h := $v.gateway.hooks -}}
{{- $e := dict "auth" ($h.auth | default "hmac-sha256") "secretEnv" (include "heraion.hookEnv" $n) -}}
{{- with $h.header }}{{ $_ := set $e "header" . }}{{ end -}}
{{- with $h.identity }}{{ $_ := set $e "identity" . }}{{ end -}}
{{- $_ := set $hooks $n $e -}}
{{- end -}}
{{- $cfg := dict "llm" $llm "git" $git "repos" ($v.repos | default list) "dryRun" $v.dryRun
      "mode" ($v.mode | default "full") "playbooksDir" "/etc/heraion/playbooks" "workDir" "/workspace" -}}
{{- with $v.namespaces }}{{ $_ := set $cfg "namespaces" . }}{{ end -}}
{{- if $mcp }}{{ $_ := set $cfg "mcpServers" $mcp }}{{ end -}}
{{- with $v.jira }}{{ if .enabled -}}
{{- $srv := index ($v.mcpServers | default dict) .server -}}
{{- if not (and $srv $srv.enabled) }}{{ fail (printf "jira.server %q must be an enabled mcpServers entry running mcp-atlassian" .server) }}{{ end -}}
{{- $_ := set $cfg "jira" (dict "server" .server "projects" (required "jira.projects is required when jira.enabled" .projects) "issueType" .issueType "labels" .labels) -}}
{{- end }}{{ end -}}
{{- $gw := dict "listen" ":8080" "jobTemplate" "/etc/heraion/job/job-template.yaml"
      "maxConcurrentJobs" $v.gateway.maxConcurrentJobs "maxBodyBytes" $v.gateway.maxBodyBytes "hooks" $hooks -}}
{{- if $v.gateway.metrics.enabled }}{{ $_ := set $gw "metricsListen" (printf ":%v" $v.gateway.metrics.port) }}{{ end -}}
{{- $_ := set $cfg "gateway" $gw -}}
{{- toYaml $cfg }}
{{- end }}

{{/* Restart pods when config, playbooks or chart-managed secrets change. */}}
{{- define "heraion.checksums" -}}
checksum/config: {{ include "heraion.config" . | sha256sum }}
checksum/playbooks: {{ include "heraion.playbooks" . | sha256sum }}
checksum/secret: {{ include "heraion.inlineSecrets" . | sha256sum }}
{{- end }}

{{/* ---------- scheduling (role value, else global) ---------- */}}
{{/*
dict "root" $ "role" .Values.gateway "component" "gateway"
A topologySpreadConstraint without labelSelector gets the component's labels,
so values can stay generic.
*/}}
{{- define "heraion.podScheduling" -}}
{{- $g := .root.Values.global -}}
{{- with (.role.nodeSelector | default $g.nodeSelector) }}
nodeSelector:
  {{- toYaml . | nindent 2 }}
{{- end }}
{{- with (.role.tolerations | default $g.tolerations) }}
tolerations:
  {{- toYaml . | nindent 2 }}
{{- end }}
{{- with (.role.affinity | default $g.affinity) }}
affinity:
  {{- toYaml . | nindent 2 }}
{{- end }}
{{- with (.role.topologySpreadConstraints | default $g.topologySpreadConstraints) }}
{{- $sel := include "heraion.componentSelector" (dict "root" $.root "component" ($.component | default "worker")) | fromYaml }}
topologySpreadConstraints:
  {{- range . }}
  - {{- toYaml (merge (deepCopy .) (dict "labelSelector" (dict "matchLabels" $sel))) | nindent 4 }}
  {{- end }}
{{- end }}
{{- with (.role.priorityClassName | default $g.priorityClassName) }}
priorityClassName: {{ . }}
{{- end }}
{{- with $g.imagePullSecrets }}
imagePullSecrets:
  {{- toYaml . | nindent 2 }}
{{- end }}
securityContext:
  {{- toYaml (.role.podSecurityContext | default $g.podSecurityContext) | nindent 2 }}
{{- end }}

{{- define "heraion.containerSecurityContext" -}}
{{- toYaml (.role.securityContext | default .root.Values.global.securityContext) }}
{{- end }}

{{/* ---------- worker ---------- */}}

{{/* Worker pod spec. dict "root" $ "args" (list "run" "--playbook" "x") */}}
{{- define "heraion.workerPodSpec" -}}
{{- $root := .root -}}
{{- $w := $root.Values.worker -}}
{{- $creds := include "heraion.credentials" (dict "root" $root "role" "worker") | fromYaml -}}
restartPolicy: Never
serviceAccountName: {{ include "heraion.workerSA" $root }}
automountServiceAccountToken: {{ $w.automountServiceAccountToken }}
{{- include "heraion.podScheduling" (dict "root" $root "role" $w "component" "worker") }}
{{- $sidecars := include "heraion.mcpSidecars" $root }}
{{- if or $sidecars $w.initContainers }}
initContainers:
  {{- with $sidecars }}
  {{- . | nindent 2 }}
  {{- end }}
  {{- with $w.initContainers }}
  {{- toYaml . | nindent 2 }}
  {{- end }}
{{- end }}
containers:
  - name: worker
    image: {{ include "heraion.image" (dict "image" $root.Values.image "root" $root) }}
    imagePullPolicy: {{ $root.Values.image.pullPolicy }}
    args: {{ toJson .args }}
    env:
      - name: HERAION_CONFIG
        value: /etc/heraion/conf/config.yaml
      {{- include "heraion.credEnv" (dict "root" $root "items" $creds.items) | nindent 6 }}
      {{- with $w.extraEnv }}
      {{- toYaml . | nindent 6 }}
      {{- end }}
    {{- with $w.extraEnvFrom }}
    envFrom:
      {{- toYaml . | nindent 6 }}
    {{- end }}
    resources:
      {{- toYaml $w.resources | nindent 6 }}
    securityContext:
      {{- include "heraion.containerSecurityContext" (dict "root" $root "role" $w) | nindent 6 }}
    volumeMounts:
      - { name: config, mountPath: /etc/heraion/conf, readOnly: true }
      - { name: playbooks, mountPath: /etc/heraion/playbooks, readOnly: true }
      - { name: workspace, mountPath: /workspace }
      - { name: tmp, mountPath: /tmp }
      {{- include "heraion.credMounts" (dict "root" $root "items" $creds.items) | nindent 6 }}
      {{- with $w.extraVolumeMounts }}
      {{- toYaml . | nindent 6 }}
      {{- end }}
  {{- with $w.extraContainers }}
  {{- toYaml . | nindent 2 }}
  {{- end }}
volumes:
  - name: config
    configMap: { name: {{ include "heraion.configName" $root }} }
  - name: playbooks
    configMap: { name: {{ include "heraion.playbooksName" $root }} }
  - name: workspace
    emptyDir: { sizeLimit: {{ $w.workspaceSizeLimit }} }
  - name: tmp
    emptyDir: { sizeLimit: 256Mi }
  {{- include "heraion.credVolumes" (dict "root" $root "items" $creds.items) | nindent 2 }}
  {{- include "heraion.mcpVolumes" $root | nindent 2 }}
  {{- with $w.extraVolumes }}
  {{- toYaml . | nindent 2 }}
  {{- end }}
{{- end }}

{{/* ---------- MCP sidecars ---------- */}}

{{/* "true" when the server's sidecar gets a Kubernetes token: RBAC rules are set. */}}
{{- define "heraion.mcpNeedsToken" -}}
{{- $rbac := .server.deploy.rbac | default dict -}}
{{- if and .root.Values.rbac.create (or $rbac.clusterRules $rbac.rules) }}true{{ end -}}
{{- end }}

{{/*
Native sidecars (restartPolicy: Always) for mode: deploy servers, e.g. container
mcp-k8s on 127.0.0.1:8080. They share the pod's network, so ports must differ.
No default startupProbe: the kubelet probes the pod IP and cannot reach a server
on 127.0.0.1. The worker retries a refused connection instead (internal/mcp).
*/}}
{{- define "heraion.mcpSidecars" -}}
{{- $root := . -}}
{{- $ports := dict -}}
{{- range $n, $s := $root.Values.mcpServers }}{{ if and $s.enabled (eq $s.mode "deploy") }}
{{- $d := $s.deploy }}
{{- $p := toString $d.port }}
{{- if hasKey $ports $p }}{{ fail (printf "mcpServers.%s.deploy.port %s is also used by mcpServers.%s: sidecars share the worker pod's network" $n $p (index $ports $p)) }}{{ end }}
{{- $_ := set $ports $p $n }}
- name: mcp-{{ $n }}
  image: {{ include "heraion.image" (dict "image" $d.image "root" $root) }}
  imagePullPolicy: {{ $d.image.pullPolicy | default "IfNotPresent" }}
  restartPolicy: Always
  {{- with $d.command }}
  command:
    {{- toYaml . | nindent 4 }}
  {{- end }}
  {{- with $d.args }}
  args:
    {{- toYaml . | nindent 4 }}
  {{- end }}
  ports:
    - { containerPort: {{ $d.port }}, protocol: TCP }
  {{- if or $d.env $d.secretEnv }}
  env:
    {{- with $d.env }}
    {{- toYaml . | nindent 4 }}
    {{- end }}
    {{- /* Third-party servers read plain env vars, so the source is picked here:
           own existingSecret, then value, then secrets.existingSecret. */}}
    {{- range $e, $ref := ($d.secretEnv | default dict) }}
    {{- $key := printf "mcp-%s-env-%s" $n (include "heraion.keySuffix" $e) }}
    {{- if $ref.existingSecret }}
    - name: {{ $e }}
      valueFrom: { secretKeyRef: { name: {{ $ref.existingSecret }}, key: {{ $ref.key | default $key }} } }
    {{- else if $ref.value }}
    - name: {{ $e }}
      valueFrom: { secretKeyRef: { name: {{ include "heraion.secretName" $root }}, key: {{ $key }} } }
    {{- else if $root.Values.secrets.existingSecret }}
    - name: {{ $e }}
      valueFrom: { secretKeyRef: { name: {{ $root.Values.secrets.existingSecret }}, key: {{ $ref.key | default $key }} } }
    {{- end }}
    {{- end }}
  {{- end }}
  {{- with $d.envFrom }}
  envFrom:
    {{- toYaml . | nindent 4 }}
  {{- end }}
  {{- with $d.startupProbe }}
  startupProbe:
    {{- toYaml . | nindent 4 }}
  {{- end }}
  resources:
    {{- toYaml ($d.resources | default (dict "requests" (dict "cpu" "50m" "memory" "64Mi") "limits" (dict "memory" "256Mi"))) | nindent 4 }}
  securityContext:
    {{- include "heraion.containerSecurityContext" (dict "root" $root "role" $d) | nindent 4 }}
  volumeMounts:
    - { name: mcp-{{ $n }}-tmp, mountPath: /tmp }
    {{- if $d.files }}
    - { name: mcp-{{ $n }}-files, mountPath: {{ $d.filesMountPath | default "/etc/mcp" }}, readOnly: true }
    {{- end }}
    {{- if include "heraion.mcpNeedsToken" (dict "root" $root "server" $s) }}
    # Only this container gets the worker ServiceAccount's token.
    - { name: mcp-{{ $n }}-token, mountPath: /var/run/secrets/kubernetes.io/serviceaccount, readOnly: true }
    {{- end }}
    {{- with $d.extraVolumeMounts }}
    {{- toYaml . | nindent 4 }}
    {{- end }}
{{- end }}{{ end }}
{{- end }}

{{/* Volumes of the MCP sidecars: /tmp, the files ConfigMap, the token. */}}
{{- define "heraion.mcpVolumes" -}}
{{- $root := . -}}
{{- range $n, $s := $root.Values.mcpServers }}{{ if and $s.enabled (eq $s.mode "deploy") }}
{{- $d := $s.deploy }}
- name: mcp-{{ $n }}-tmp
  emptyDir: { sizeLimit: 256Mi }
{{- if $d.files }}
- name: mcp-{{ $n }}-files
  configMap: { name: {{ include "heraion.mcpName" (dict "root" $root "name" $n) }} }
{{- end }}
{{- if include "heraion.mcpNeedsToken" (dict "root" $root "server" $s) }}
- name: mcp-{{ $n }}-token
  projected:
    sources:
      - serviceAccountToken: { path: token, expirationSeconds: 3600 }
      - configMap: { name: kube-root-ca.crt, items: [{ key: ca.crt, path: ca.crt }] }
      - downwardAPI: { items: [{ path: namespace, fieldRef: { fieldPath: metadata.namespace } }] }
{{- end }}
{{- with $d.extraVolumes }}
{{- toYaml . | nindent 0 }}
{{- end }}
{{- end }}{{ end }}
{{- end }}

{{- define "heraion.workerPodMetadata" -}}
labels:
  {{- include "heraion.componentSelector" (dict "root" .root "component" "worker") | nindent 2 }}
  {{- with .playbook }}
  heraion.io/playbook: {{ . }}
  {{- end }}
  {{- with .root.Values.worker.podLabels }}
  {{- toYaml . | nindent 2 }}
  {{- end }}
annotations:
  {{- include "heraion.checksums" .root | nindent 2 }}
  {{- with .root.Values.worker.podAnnotations }}
  {{- toYaml . | nindent 2 }}
  {{- end }}
{{- end }}

{{/* Worker Job spec. dict "root" $ "playbook" name ("" for the gateway template) */}}
{{- define "heraion.workerJobSpec" -}}
{{- $w := .root.Values.worker -}}
{{- $args := list "run" -}}
{{- with .playbook }}{{ $args = list "run" "--playbook" . }}{{ end -}}
backoffLimit: {{ $w.backoffLimit }}
activeDeadlineSeconds: {{ $w.activeDeadlineSeconds }}
ttlSecondsAfterFinished: {{ $w.ttlSecondsAfterFinished }}
template:
  metadata:
    {{- include "heraion.workerPodMetadata" . | nindent 4 }}
  spec:
    {{- include "heraion.workerPodSpec" (dict "root" .root "args" $args) | nindent 4 }}
{{- end }}
