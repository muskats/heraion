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
app.kubernetes.io/version: {{ .root.Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .root.Release.Service }}
app.kubernetes.io/part-of: heraion
{{- with .root.Values.commonLabels }}
{{ toYaml . }}
{{- end }}
{{- end }}

{{/* Metadata block: dict "root" $ "component" c "name" n */}}
{{- define "heraion.metadata" -}}
name: {{ .name }}
namespace: {{ .root.Release.Namespace }}
labels:
  {{- include "heraion.labels" . | nindent 2 }}
{{- with .root.Values.commonAnnotations }}
annotations:
  {{- toYaml . | nindent 2 }}
{{- end }}
{{- end }}

{{/* dict "image" .image "root" $ */}}
{{- define "heraion.image" -}}
{{- if .image.digest -}}
{{ .image.repository }}@{{ .image.digest }}
{{- else -}}
{{ .image.repository }}:{{ .image.tag | default .root.Chart.AppVersion }}
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

{{/* ---------- secrets ---------- */}}

{{/* datadog + DD-API-KEY -> HERAION_MCP_DATADOG_DD_API_KEY */}}
{{- define "heraion.mcpHeaderEnv" -}}
{{- regexReplaceAll "[^A-Z0-9_]" (printf "HERAION_MCP_%s_%s" .server .header | upper) "_" }}
{{- end }}
{{- define "heraion.secretKeySuffix" -}}{{ regexReplaceAll "[^a-z0-9.-]" (lower .) "-" }}{{- end }}

{{/* Inline secret values by Secret key, e.g. {llm-api-key: sk-..., hook-alertmanager: ...}. */}}
{{- define "heraion.inlineSecrets" -}}
{{- $v := .Values -}}
{{- $out := dict -}}
{{- with $v.llm.apiKey.value }}{{ $_ := set $out "llm-api-key" . }}{{ end -}}
{{- with $v.git.token.value }}{{ $_ := set $out "git-token" . }}{{ end -}}
{{- range $n, $h := $v.gateway.hooks }}{{ with ($h.secret | default dict).value }}{{ $_ := set $out (printf "hook-%s" $n) . }}{{ end }}{{ end -}}
{{- range $n, $s := $v.mcpServers }}{{ if $s.enabled -}}
{{- range $h, $ref := $s.secretHeaders }}{{ with $ref.value }}{{ $_ := set $out (printf "mcp-%s-h-%s" $n (include "heraion.secretKeySuffix" $h)) . }}{{ end }}{{ end -}}
{{- if eq $s.mode "deploy" }}{{ range $e, $ref := ($s.deploy.secretEnv | default dict) }}{{ with $ref.value }}{{ $_ := set $out (printf "mcp-%s-env-%s" $n (include "heraion.secretKeySuffix" $e)) . }}{{ end }}{{ end }}{{ end -}}
{{- end }}{{ end -}}
{{- toYaml $out }}
{{- end }}

{{/*
Env entry for a secret ref; nothing if the ref is empty. Example:
  include "heraion.secretEnv" (dict "root" $ "env" "HERAION_GIT_TOKEN" "ref" .Values.git.token "key" "git-token")
*/}}
{{- define "heraion.secretEnv" -}}
{{- $ref := .ref | default dict -}}
{{- if $ref.existingSecret }}
- name: {{ .env }}
  valueFrom:
    secretKeyRef:
      name: {{ $ref.existingSecret }}
      key: {{ required (printf "%s: key is required with existingSecret" .env) $ref.key }}
{{- else if $ref.value }}
- name: {{ .env }}
  valueFrom:
    secretKeyRef:
      name: {{ include "heraion.secretName" .root }}
      key: {{ .key }}
{{- end }}
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
{{- $llm := dict "baseURL" (include "heraion.llmBaseURL" .) "model" (required "llm.model is required" $v.llm.model) "maxSteps" $v.llm.maxSteps "maxTokens" $v.llm.maxTokens "timeout" $v.llm.timeout -}}
{{- if hasKey $v.llm "temperature" }}{{ $_ := set $llm "temperature" $v.llm.temperature }}{{ end -}}
{{- $git := dict "provider" $v.git.provider "authorName" $v.git.authorName "authorEmail" $v.git.authorEmail -}}
{{- range $k := list "endpoint" "cloneBaseURL" "username" }}{{ with index $v.git $k }}{{ $_ := set $git $k . }}{{ end }}{{ end -}}
{{- $mcp := dict -}}
{{- range $n, $s := $v.mcpServers }}{{ if $s.enabled -}}
{{- $url := "" -}}
{{- if eq $s.mode "deploy" -}}
{{- $url = printf "http://%s.%s.svc:%v%s" (include "heraion.mcpName" (dict "root" $ "name" $n)) $.Release.Namespace $s.deploy.port ($s.path | default "/mcp") -}}
{{- else -}}
{{- $url = tpl (required (printf "mcpServers.%s.url is required for mode %s" $n $s.mode) $s.url) $ -}}
{{- end -}}
{{- $e := dict "url" $url "transport" ($s.transport | default "streamable") -}}
{{- range $k := list "allowTools" "denyTools" "headers" "timeout" "required" }}{{ with index $s $k }}{{ $_ := set $e $k . }}{{ end }}{{ end -}}
{{- $hdrs := dict -}}
{{- range $h, $ref := $s.secretHeaders }}{{ if or $ref.existingSecret $ref.value }}{{ $_ := set $hdrs $h (include "heraion.mcpHeaderEnv" (dict "server" $n "header" $h)) }}{{ end }}{{ end -}}
{{- if $hdrs }}{{ $_ := set $e "headersFromEnv" $hdrs }}{{ end -}}
{{- $_ := set $mcp $n $e -}}
{{- end }}{{ end -}}
{{- $hooks := dict -}}
{{- range $n, $h := $v.gateway.hooks -}}
{{- $e := dict "auth" ($h.auth | default "hmac-sha256") "secretEnv" (printf "HERAION_HOOK_%s_SECRET" ($n | upper | replace "-" "_")) -}}
{{- with $h.header }}{{ $_ := set $e "header" . }}{{ end -}}
{{- $_ := set $hooks $n $e -}}
{{- end -}}
{{- $cfg := dict "llm" $llm "git" $git "repos" ($v.repos | default list) "dryRun" $v.dryRun
      "playbooksDir" "/etc/heraion/playbooks" "workDir" "/workspace" -}}
{{- if $mcp }}{{ $_ := set $cfg "mcpServers" $mcp }}{{ end -}}
{{- $_ := set $cfg "gateway" (dict "listen" ":8080" "jobTemplate" "/etc/heraion/job/job-template.yaml"
      "maxConcurrentJobs" $v.gateway.maxConcurrentJobs "maxBodyBytes" $v.gateway.maxBodyBytes "hooks" $hooks) -}}
{{- toYaml $cfg }}
{{- end }}

{{/* ---------- scheduling (role value, else global) ---------- */}}
{{/* dict "root" $ "role" .Values.worker */}}
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
topologySpreadConstraints:
  {{- toYaml . | nindent 2 }}
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
{{/* Shared env for workers: config path + every credential they need. */}}
{{- define "heraion.workerEnv" -}}
- name: HERAION_CONFIG
  value: /etc/heraion/conf/config.yaml
{{- include "heraion.secretEnv" (dict "root" . "env" "HERAION_LLM_API_KEY" "ref" .Values.llm.apiKey "key" "llm-api-key") }}
{{- include "heraion.secretEnv" (dict "root" . "env" "HERAION_GIT_TOKEN" "ref" .Values.git.token "key" "git-token") }}
{{- range $n, $s := .Values.mcpServers }}{{ if $s.enabled }}
{{- range $h, $ref := $s.secretHeaders }}
{{- include "heraion.secretEnv" (dict "root" $ "env" (include "heraion.mcpHeaderEnv" (dict "server" $n "header" $h)) "ref" $ref "key" (printf "mcp-%s-h-%s" $n (include "heraion.secretKeySuffix" $h))) }}
{{- end }}
{{- end }}{{ end }}
{{- with .Values.worker.extraEnv }}
{{ toYaml . }}
{{- end }}
{{- end }}

{{/* Worker Job spec. dict "root" $ "playbook" name ("" for the gateway template) */}}
{{- define "heraion.workerJobSpec" -}}
{{- $root := .root -}}
{{- $w := $root.Values.worker -}}
backoffLimit: {{ $w.backoffLimit }}
activeDeadlineSeconds: {{ $w.activeDeadlineSeconds }}
ttlSecondsAfterFinished: {{ $w.ttlSecondsAfterFinished }}
template:
  metadata:
    labels:
      {{- include "heraion.componentSelector" (dict "root" $root "component" "worker") | nindent 6 }}
      {{- with .playbook }}
      heraion.io/playbook: {{ . }}
      {{- end }}
      {{- with $w.podLabels }}
      {{- toYaml . | nindent 6 }}
      {{- end }}
    annotations:
      checksum/config: {{ include "heraion.config" $root | sha256sum }}
      checksum/playbooks: {{ include "heraion.playbooks" $root | sha256sum }}
      {{- with $w.podAnnotations }}
      {{- toYaml . | nindent 6 }}
      {{- end }}
  spec:
    restartPolicy: Never
    serviceAccountName: {{ include "heraion.workerSA" $root }}
    automountServiceAccountToken: {{ $w.automountServiceAccountToken }}
    {{- include "heraion.podScheduling" (dict "root" $root "role" $w) | nindent 4 }}
    {{- with $w.initContainers }}
    initContainers:
      {{- toYaml . | nindent 6 }}
    {{- end }}
    containers:
      - name: worker
        image: {{ include "heraion.image" (dict "image" $root.Values.image "root" $root) }}
        imagePullPolicy: {{ $root.Values.image.pullPolicy }}
        args: ["run"{{ with .playbook }}, "--playbook", {{ . | quote }}{{ end }}]
        env:
          {{- include "heraion.workerEnv" $root | nindent 10 }}
        {{- with $w.extraEnvFrom }}
        envFrom:
          {{- toYaml . | nindent 10 }}
        {{- end }}
        resources:
          {{- toYaml $w.resources | nindent 10 }}
        securityContext:
          {{- include "heraion.containerSecurityContext" (dict "root" $root "role" $w) | nindent 10 }}
        volumeMounts:
          - { name: config, mountPath: /etc/heraion/conf, readOnly: true }
          - { name: playbooks, mountPath: /etc/heraion/playbooks, readOnly: true }
          - { name: workspace, mountPath: /workspace }
          - { name: tmp, mountPath: /tmp }
          {{- with $w.extraVolumeMounts }}
          {{- toYaml . | nindent 10 }}
          {{- end }}
      {{- with $w.extraContainers }}
      {{- toYaml . | nindent 6 }}
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
      {{- with $w.extraVolumes }}
      {{- toYaml . | nindent 6 }}
      {{- end }}
{{- end }}
