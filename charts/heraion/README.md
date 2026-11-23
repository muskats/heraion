# HeraIon Helm chart

Installs [HeraIon](../../README.md): scheduled and webhook-triggered investigations that
propose fixes as pull requests.

- [Requirements](#requirements)
- [Install](#install)
- [What gets deployed](#what-gets-deployed)
- [Secrets](#secrets)
- [Playbooks](#playbooks)
- [Webhooks](#webhooks)
- [Jira and Confluence](#jira-and-confluence)
- [MCP servers](#mcp-servers)
- [LLM endpoint](#llm-endpoint)
- [Security defaults](#security-defaults)
- [Images and scheduling](#images-and-scheduling)
- [Values reference](#values-reference)
- [Testing](#testing)
- [Upgrading](#upgrading)
- [Uninstall](#uninstall)

## Requirements

| Needed for | Requirement |
|---|---|
| Always | Kubernetes 1.30+ (native sidecars, ValidatingAdmissionPolicy), Helm 3.18+ or Helm 4 |
| `trivy-remediate` playbook | [Trivy Operator](https://github.com/aquasecurity/trivy-operator) |
| `csiKey` secrets | [Secrets Store CSI driver](https://secrets-store-csi-driver.sigs.k8s.io/) and a provider (AWS, Azure, GCP, Vault) |
| `litellm.enabled` | PostgreSQL for LiteLLM |
| `networkPolicy.enabled` (default) | A CNI that enforces NetworkPolicy, e.g. Calico or Cilium |

## Install

```sh
kubectl create namespace heraion
kubectl -n heraion create secret generic heraion \
  --from-literal=llm-api-key=sk-... --from-literal=git-token=ghp_...

helm install heraion oci://ghcr.io/muskats/charts/heraion -n heraion \
  --set secrets.existingSecret=heraion \
  --set llm.baseURL=https://api.openai.com/v1 --set llm.model=gpt-4.1 \
  --set repos='{acme/web}' --set dryRun=true \
  --set playbooks.trivy-remediate.enabled=true --set mcpServers.k8s.enabled=true

helm test heraion -n heraion
```

Start with `dryRun=true`: each PR the agent would open is written to the Job log with its
diff. Set `dryRun=false` once the diffs look right.

To install from a checkout instead:

```sh
helm dependency build charts/heraion
helm install heraion charts/heraion -n heraion -f examples/helm/values-minimal.yaml
```

Example values files:

| File | Shows |
|---|---|
| [values-minimal.yaml](../../examples/helm/values-minimal.yaml) | One scheduled playbook, a shared Secret, OpenAI, GitHub |
| [values-selfhosted.yaml](../../examples/helm/values-selfhosted.yaml) | Gitea, LiteLLM, Alertmanager webhooks, HPA and PDB |
| [values-integrations.yaml](../../examples/helm/values-integrations.yaml) | Jira/Confluence (mcp-atlassian), Grafana and Datadog MCP servers |
| [values-identity.yaml](../../examples/helm/values-identity.yaml) | No static secrets: CSI driver with IRSA, projected ServiceAccount tokens |

## What gets deployed

Only what the values need. With the install command above you get a CronJob, its
ConfigMaps, a ServiceAccount, NetworkPolicies and the k8s MCP server.

| Object | Created when | Name |
|---|---|---|
| CronJob | A playbook with `trigger.schedule` is enabled | `<release>-heraion-<playbook>` |
| Job | A schedule fires, or a webhook arrives | CronJob: `<release>-heraion-<playbook>-<timestamp>`; gateway: `heraion-<playbook>-<hash of payload>` |
| Gateway Deployment, Service, Role | A playbook with `trigger.webhook` is enabled | `<release>-heraion-gateway` |
| Ingress, HPA, PDB | `gateway.ingress`, `.autoscaling`, `.podDisruptionBudget` enabled | `<release>-heraion-gateway` |
| MCP server RBAC, files ConfigMap | `mcpServers.<name>.enabled` and `mode: deploy`; the server runs as sidecar `mcp-<name>` in each worker pod | `<release>-heraion-mcp-<name>` |
| ValidatingAdmissionPolicy and binding, for Jobs and for event Secrets | `admissionPolicy.enabled` (default), `rbac.create` and a webhook playbook | `<namespace>-<release>-heraion-jobs`, `-event-secrets` |
| Secret | Any credential has a `value` | `<release>-heraion-secrets` |
| Secret per webhook Job | A webhook with a body arrives; deleted with its Job | `heraion-<playbook>-<hash>-event` |
| ResourceQuota | `resourceQuota.enabled` | `<release>-heraion` |
| Test Pod | `helm test` (`tests.enabled`) | `<release>-heraion-check` |

A worker Job runs one playbook and exits. It has no Kubernetes API token. It reads the
cluster only through MCP servers, and it changes things only by pushing a branch and opening
a PR.

## Secrets

### Sources and order

These credentials can each use any of the sources below:

| Credential | Env var in the pod | Default key |
|---|---|---|
| `llm.apiKey` | `HERAION_LLM_API_KEY` | `llm-api-key` |
| `git.token` | `HERAION_GIT_TOKEN` | `git-token` |
| `gateway.hooks.<name>.secret` | `HERAION_HOOK_<NAME>_SECRET` | `hook-<name>` |
| `mcpServers.<name>.secretHeaders.<header>` | `HERAION_MCP_<NAME>_<HEADER>` | `mcp-<name>-<header>` |

For example, `mcpServers.datadog.secretHeaders.DD-API-KEY` becomes the env var
`HERAION_MCP_DATADOG_DD_API_KEY` with the key `mcp-datadog-dd-api-key`.

The chart renders every source you configure. When the value is needed, the binary uses
level 1 if it is configured, otherwise the first of levels 2 and 3 that has a value:

| Order | Values | Pod env | Typical use |
|---|---|---|---|
| 1 | `serviceAccountToken: { audience: vault }` | `<ENV>_FILE` = a projected ServiceAccount token | A service that trusts the cluster's OIDC issuer |
| 1 | `csiKey: llm-api-key` | `<ENV>_FILE` = a file from the CSI driver | AWS Secrets Manager, Azure Key Vault, Google Secret Manager, Vault |
| 2 | `existingSecret: llm`, `key: api-key` | `<ENV>` from a `secretKeyRef` with `optional: true` | Secrets from External Secrets, Sealed Secrets, or kubectl |
| 3 | `value: sk-...` | `<ENV>_FALLBACK` from the chart's Secret | Local tests |

Rules:

- Level 2 defaults to `secrets.existingSecret` with the default key from the first table. A
  credential's own `existingSecret` and `key` override that.
- A configured level 1 file that is missing or empty fails the run and `helm test`, e.g.
  `HERAION_LLM_API_KEY_FILE: open /var/run/secrets/heraion/csi/llm-api-key: no such file or
  directory (not falling back to HERAION_LLM_API_KEY_FALLBACK ...)`. Levels 2 and 3 are not
  used, so a broken CSI mount never runs on a literal value.
- A missing level 2 key is skipped. `optional: true` lets the pod start without the key, and
  level 3 is used.
- If no level has a value, the run fails. `helm test` reports it first.
- The check happens at runtime, not render time, because Helm cannot see what an existing
  Secret contains without `lookup`. That would break `helm template`, Argo CD and Flux.
- Values never go into a ConfigMap. A chart test enforces this.

### Shared Secret

One Secret for everything, with the default keys:

```sh
kubectl -n heraion create secret generic heraion \
  --from-literal=llm-api-key=sk-... \
  --from-literal=git-token=ghp_... \
  --from-literal=hook-alertmanager=$(openssl rand -hex 32) \
  --from-literal=mcp-atlassian-env-jira-personal-token=...
```

```yaml
secrets:
  existingSecret: heraion
```

### Per-credential Secrets

```yaml
llm:
  apiKey: { existingSecret: openai, key: api-key }
git:
  token: { existingSecret: github-bot, key: token }
```

### Cloud secret manager (CSI driver)

The CSI driver authenticates as the pod's ServiceAccount, so the cloud identity goes on the
ServiceAccount. AWS example:

```yaml
secrets:
  csi:
    enabled: true
    secretProviderClass: heraion-aws
llm:
  apiKey: { csiKey: llm-api-key }    # file /var/run/secrets/heraion/csi/llm-api-key
git:
  token: { csiKey: git-token }
worker:
  serviceAccount:
    annotations:
      eks.amazonaws.com/role-arn: arn:aws:iam::123456789012:role/heraion-worker
extraObjects:
  - apiVersion: secrets-store.csi.x-k8s.io/v1
    kind: SecretProviderClass
    metadata: { name: heraion-aws }
    spec:
      provider: aws
      parameters:
        objects: |
          - objectName: "heraion/llm-api-key"
            objectType: "secretsmanager"
            objectAlias: "llm-api-key"
          - objectName: "heraion/git-token"
            objectType: "secretsmanager"
            objectAlias: "git-token"
```

`objectAlias` is the file name, and `csiKey` must match it. For Azure, use
`azure.workload.identity/client-id`; for GKE, use `iam.gke.io/gcp-service-account`.

The CSI volume is mounted only into pods that have a credential with `csiKey`. With the
example above, that means workers but not the gateway. If a webhook secret also uses
`csiKey`, annotate `gateway.serviceAccount` too.

### ServiceAccount token

The kubelet writes a token for the pod's ServiceAccount, limited to `audience`, and rotates
it. This works only with services that validate Kubernetes tokens, such as Vault, an LLM
gateway with JWT auth, or an in-cluster MCP server. OpenAI and GitHub do not accept them.

```yaml
mcpServers:
  vault:
    enabled: true
    mode: external
    url: https://vault-mcp.example.internal/mcp
    secretHeaders:
      Authorization:
        serviceAccountToken: { audience: vault, expirationSeconds: 3600 }
```

On an `Authorization` header the token is sent as `Bearer <token>`. Set `prefix` to change
this, e.g. `prefix: ""`.

The token's audience is not the Kubernetes API, so it cannot be used against the cluster.

### Mixing levels

```yaml
secrets:
  existingSecret: heraion      # git-token and hook-alertmanager live here
llm:
  apiKey:
    csiKey: llm-api-key        # 1. from the cloud
    value: sk-local-dev        # 3. only if the CSI file is missing
```

### Rotation and restarts

- Files (levels 1) are re-read about every 30 seconds. Rotated tokens are used without a
  restart, and the old value stays masked in logs.
- Kubernetes does not update env vars in a running pod. A worker Job is short-lived and gets
  current values when it starts. The gateway needs `kubectl rollout restart` after you change
  an existing Secret.
- A change to a chart-managed `value` changes the `checksum/secret` annotation and rolls the
  pods on `helm upgrade`.

### Servers the chart deploys

`mcpServers.<name>.deploy.secretEnv` is passed to a third-party container as a plain env var,
so the source is chosen at render time. The order is: the entry's own `existingSecret`, then
`value`, then `secrets.existingSecret` with the key `mcp-<name>-env-<var>`.

```yaml
mcpServers:
  atlassian:
    deploy:
      secretEnv:
        JIRA_PERSONAL_TOKEN: { existingSecret: jira, key: pat }
```

## Playbooks

Playbooks are defined under `playbooks.<name>`. The chart ships two, both disabled:

| Playbook | Trigger | Needs |
|---|---|---|
| `trivy-remediate` | Every 6 hours | `mcpServers.k8s`, Trivy Operator, `repos` |
| `alertmanager-triage` | Webhook `alertmanager` | `gateway.hooks.alertmanager`, `mcpServers.k8s`; optionally `mcpServers.atlassian` and `jira` |

Enable one, change its schedule, or add your own:

```yaml
playbooks:
  trivy-remediate:
    enabled: true
    trigger: { schedule: "0 3 * * *" }
    mode: safe               # only versions and digests change, see the main README's Modes section
  nightly-digest:
    trigger: { schedule: "0 7 * * 1-5" }
    tools: ["grafana.*"]
    output: report
    prompt: Summarize yesterday's alerts.
  noisy-one:
    suspend: true            # keep the CronJob, but stop scheduling it
```

Chart-only keys: `enabled` (default true) and `suspend`. Everything else is the playbook
format described in the [main README](../../README.md#playbooks).

To run a scheduled playbook now:

```sh
kubectl -n heraion create job --from=cronjob/heraion-trivy-remediate trivy-now
kubectl -n heraion logs -f job/trivy-now
```

## Webhooks

The gateway is created as soon as one playbook has `trigger.webhook`. Each hook needs a
secret from any level above:

```yaml
playbooks:
  alertmanager-triage: { enabled: true }
gateway:
  hooks:
    alertmanager:
      auth: token            # Authorization: Bearer <secret>
      secret: {}             # key hook-alertmanager in secrets.existingSecret
    github:
      auth: hmac-sha256      # X-Hub-Signature-256: sha256=<hmac of body>
      secret: { existingSecret: github-webhook, key: secret }
  ingress:
    enabled: true
    className: nginx
    hosts:
      - host: heraion.example.com
        paths: [{ path: /hooks, pathType: Prefix }]
```

Alertmanager receiver, from inside the cluster:

```yaml
receivers:
  - name: heraion
    webhook_configs:
      - url: http://heraion-gateway.heraion.svc:8080/hooks/alertmanager
        send_resolved: false   # each notification is one run; resolved alerts need none
        http_config:
          authorization: { credentials_file: /etc/alertmanager/secrets/heraion/hook-alertmanager }
```

Header settings for other senders (GitLab, Gitea, Jira, Grafana) are in
[docs/integrations.md](../../docs/integrations.md#webhook-senders).

Load control:

- `gateway.maxConcurrentJobs` (default 5): above it the gateway answers 429 and the sender
  retries. A redelivery of an event whose run exists is answered as a duplicate, even then.
- `resourceQuota.enabled` caps Jobs in the namespace (`count/jobs.batch`) as a hard limit.
- The same event delivered twice gets the same Job name, so it runs only once while that
  Job exists (`worker.ttlSecondsAfterFinished`, default one day). A failed run is deleted
  and answered with 503, so the retry runs again. `gateway.hooks.<name>.identity:
  alertmanager` makes the event identity ignore annotation values.
- `gateway.maxBodyBytes` (default 256 KiB) caps the body. Each body is stored in a Secret
  `heraion-<playbook>-<hash>-event`, owned by its Job, and mounted into the worker at
  `/var/run/heraion/event/payload`.

## Jira and Confluence

Turn on the `mcpServers.atlassian` preset
([mcp-atlassian](https://github.com/sooperset/mcp-atlassian) 0.24.7) and `jira` for
`create_issue`. The URLs, tokens and project/space filters are the server's env:

```yaml
mcpServers:
  atlassian:
    enabled: true
    deploy:
      env:
        - { name: STATELESS, value: "true" }
        - { name: HOME, value: /tmp }
        - { name: ENABLED_TOOLS, value: "jira_search,jira_get_issue,jira_create_issue,confluence_search,confluence_get_page" }
        - { name: JIRA_URL, value: https://acme.atlassian.net }
        - { name: JIRA_USERNAME, value: heraion-bot@acme.com }
        - { name: JIRA_PROJECTS_FILTER, value: "OPS,CHG" }
      secretEnv:
        JIRA_API_TOKEN: { existingSecret: atlassian, key: api-token }
jira:
  enabled: true
  server: atlassian
  projects: [OPS]
```

| Deployment | Credentials |
|---|---|
| Jira Cloud | `JIRA_USERNAME` (env) and `JIRA_API_TOKEN` from https://id.atlassian.com/manage-profile/security/api-tokens |
| Jira Data Center | `JIRA_PERSONAL_TOKEN` |
| Confluence | Same with `CONFLUENCE_*`; on Cloud the Jira API token works |

Setting `env` replaces the preset's list, so keep `STATELESS`, `HOME` and `ENABLED_TOOLS`.
How `create_issue` de-duplicates is in the [main README](../../README.md#jira-and-confluence).

## MCP servers

| Mode | The chart creates | Example |
|---|---|---|
| `deploy` | A native sidecar `mcp-<name>` in each worker pod on `127.0.0.1:<port>` (ports must differ); your RBAC rules bound to the worker ServiceAccount, its token mounted in that sidecar only | `k8s` (kubernetes-mcp-server) |
| `dependency` | Nothing; a subchart runs it | `grafana` (grafana-mcp subchart) |
| `external` | Nothing; workers call `url` | `datadog` |

The `k8s` server is read-only: `read_only = true`, and a ClusterRole with `get`, `list` and
`watch` on workloads, pods and their logs, events and Trivy reports. It cannot read Secrets or
ConfigMaps: RBAC refuses them, and `denied_resources` in its config refuses them again if you
widen the rules. Pod, Deployment and Job specs are readable, so a secret written as a literal
`env` value there reaches the model unless the redactor recognises it (a configured credential
or a known token shape). Use `valueFrom.secretKeyRef` in your workloads. To limit it to one
namespace:

```yaml
mcpServers:
  k8s:
    enabled: true
    deploy:
      rbac:
        clusterRules: []
        rules:               # a Role in the release namespace
          - apiGroups: [""]
            resources: [pods, pods/log, events]
            verbs: [get, list, watch]
```

Grafana, Datadog, Jira and custom servers are covered in
[docs/integrations.md](../../docs/integrations.md).

## LLM endpoint

```yaml
llm:
  baseURL: https://api.openai.com/v1   # or http://vllm:8000/v1, http://kubeai/openai/v1
  model: gpt-4.1
  maxSteps: 30                         # model turns per run
  maxTokens: 500000                    # tokens per run
  timeout: 20m
  maxToolResultBytes: 16384            # each tool result is cut here
  maxContextBytes: 200000              # history per request, see below
```

Every request resends the history. Above `maxContextBytes` the oldest tool results, except
the last 4, become `[earlier k8s__pods_log result of 16384 bytes removed ...]`, so the model
calls the tool again if it needs it. The defaults need a model context of about 64k tokens;
for a 32k model, use e.g. `maxContextBytes: 80000` and `maxToolResultBytes: 8192`.

With `litellm.enabled: true` and `llm.baseURL` left empty, workers use
`http://<release>-litellm-gateway:4000/v1`. Configure LiteLLM under `litellm:` as in its
chart. It needs PostgreSQL:

```yaml
litellm:
  enabled: true
  database:
    writer:
      host: postgres.db.svc
      dbname: litellm
      passwordSecret: { name: litellm-db, usernameKey: username, passwordKey: password }
```

## Security defaults

| Area | Default |
|---|---|
| Pods | Non-root (65532), read-only root filesystem, all capabilities dropped, seccomp `RuntimeDefault` |
| Workers | No ServiceAccount token in the worker container (only an MCP sidecar with RBAC rules gets one), `emptyDir` workspace, deny all ingress |
| Gateway RBAC | `get`, `create` and `delete` (a failed run, so the retry runs again) Jobs, plus `list` when `maxConcurrentJobs` > 0; `create` Secrets (one per webhook Job, holding its payload, owned by the Job from creation); all in its own namespace. The [admission policy](#admission-policy) narrows which Jobs and Secrets it may create |
| MCP servers | Sidecars of the worker pod, listening on `127.0.0.1`; no Service, nothing reachable from other pods |
| Secrets | Never in ConfigMaps; masked in prompts, PR text and logs |
| Git | Pushes only `heraion/<playbook>/...` branches, never the default branch; never CI or CODEOWNERS files; no merge |

### Admission policy

The gateway's Role allows creating Jobs and Secrets in its namespace. The shipped
ValidatingAdmissionPolicies (`admissionPolicy.enabled: true`) narrow that for requests by the
gateway ServiceAccount, using the worker pod spec the chart renders:

| Object | Admitted only when |
|---|---|
| Job | Label `heraion.io/playbook`; the worker ServiceAccount; every container one the chart renders, with its image and command; Secrets in `env` only those the chart uses; every volume one the chart renders, with its source; `heraion-event` is `<job>-event`; no privileged container, host network, PID or IPC, ephemeral container |
| Secret | Named `<job>-event`, label `heraion.io/playbook`, immutable, `Opaque`, one key `payload`, owned by that Job |

Installing the policies needs permission to create cluster-scoped admission objects. Install
HeraIon in a namespace of its own.

Turn off parts that conflict with your platform with `rbac.create: false` (bring your own
RBAC), `networkPolicy.enabled: false`, or role-level `podSecurityContext` and
`securityContext`.

## Images and scheduling

```yaml
image:
  registry: docker.io
  repository: muskats/heraion
  tag: ""            # default: appVersion
  digest: ""         # sha256:...; wins over tag
global:
  imageRegistry: registry.example.com   # mirror for every image that has a registry field
  imagePullSecrets: [{ name: regcred }]
```

`global.imageRegistry` turns `docker.io/muskats/heraion` into
`registry.example.com/muskats/heraion`, and `quay.io/containers/kubernetes_mcp_server` into
`registry.example.com/containers/kubernetes_mcp_server`. Images given as one full
`repository` with no `registry` field are left as they are.

The scheduling keys `nodeSelector`, `tolerations`, `affinity`, `topologySpreadConstraints`,
`priorityClassName`, `podSecurityContext` and `securityContext` exist under `global` and
under each role (`worker`, `gateway`). A role's value replaces the global one.
`mcpServers.<name>.deploy` has `securityContext` only: sidecars run in the worker pod.

A `topologySpreadConstraints` entry without a `labelSelector` gets the role's pod labels.
This is how the gateway spreads its replicas across nodes by default:

```yaml
gateway:
  topologySpreadConstraints:
    - { maxSkew: 1, topologyKey: topology.kubernetes.io/zone, whenUnsatisfiable: ScheduleAnyway }
```

## Values reference

The full list with comments is in [values.yaml](values.yaml). Unknown keys and wrong types are
rejected by [values.schema.json](values.schema.json).

### General

| Key | Default | Description |
|---|---|---|
| `nameOverride`, `fullnameOverride` | `""` | Change the `heraion` / `<release>-heraion` names |
| `namespaceOverride` | `""` | Namespace for all objects instead of the release namespace |
| `commonLabels`, `commonAnnotations` | `{}` | Added to every object |
| `image.registry` / `.repository` / `.tag` / `.digest` | `docker.io` / `muskats/heraion` / appVersion / `""` | HeraIon image |
| `global.imageRegistry` | `""` | Registry mirror, see above |
| `global.imagePullSecrets` | `[]` | Pull secrets for every pod |
| `global.podSecurityContext`, `global.securityContext` | hardened, see above | Defaults for every pod and container |
| `rbac.create` | `true` | Create Roles and ClusterRoles |
| `extraObjects` | `[]` | Extra manifests; strings are templated |

### Secrets

| Key | Default | Description |
|---|---|---|
| `secrets.existingSecret` | `""` | Default Secret for every credential, with the default keys |
| `secrets.csi.enabled` | `false` | Mount the Secrets Store CSI volume |
| `secrets.csi.secretProviderClass` | `""` | Required when enabled |
| `secrets.csi.mountPath` | `/var/run/secrets/heraion/csi` | `csiKey` files are read from here |
| `<credential>.serviceAccountToken.audience` | `""` | Level 1: projected token with this audience |
| `<credential>.serviceAccountToken.expirationSeconds` | `3600` | At least 600 |
| `<credential>.csiKey` | `""` | Level 1: file name in the CSI mount |
| `<credential>.existingSecret`, `.key` | `""` | Level 2: overrides the shared Secret and default key |
| `<credential>.value` | `""` | Level 3: literal, stored in the chart's Secret |
| `mcpServers.<name>.secretHeaders.<header>.prefix` | `"Bearer "` for an `Authorization` header with a ServiceAccount token, otherwise `""` | Put before the header value |

### LLM and Git

| Key | Default | Description |
|---|---|---|
| `llm.baseURL` | `""` | OpenAI-compatible endpoint; required unless `litellm.enabled` |
| `llm.model` | `""` | Required, e.g. `gpt-4.1` |
| `llm.apiKey` | `{}` | Credential, see [Secrets](#secrets) |
| `llm.maxSteps`, `llm.maxTokens`, `llm.timeout` | `30`, `500000`, `20m` | Limits per run |
| `llm.maxToolResultBytes` | `16384` | Each tool result is cut here; 1024 to 262144 |
| `llm.maxContextBytes` | `200000` | History per request; above it the oldest tool results (not the last 4) become a stub |
| `llm.temperature` | unset | Sent only when set |
| `git.provider` | `github` | `github`, `gitlab`, `gitea`, `bitbucket`, `stash`, `gogs`, `azure` |
| `git.endpoint` | `""` | API URL of a self-hosted server |
| `git.cloneBaseURL` | endpoint, or `https://github.com` / `https://gitlab.com` | Clone URLs are `<base>/<owner>/<repo>.git`. For `stash` and `azure`, leave empty to look each repo's clone URL up through the API |
| `git.username` | `x-access-token` | Basic-auth user for clone and push |
| `git.authorName`, `git.authorEmail` | `HeraIon`, `heraion@users.noreply.local` | Commit author |
| `git.token` | `{}` | Credential |
| `repos` | `[]` | Repos HeraIon may change; empty means none |
| `namespaces` | `[]` | Kubernetes namespaces the k8s tools may read, e.g. `[shop]`; empty means all. Deployed MCP servers then get a RoleBinding per namespace instead of a ClusterRoleBinding |
| `dryRun` | `false` | Log PRs instead of pushing them |
| `mode` | `full` | `safe` or `full`: how `open_pr` delivers a fix, and the riskiest mode a playbook may use, see [Modes](../../README.md#modes) |
| `playbooks.<name>.mode` | `""` | Mode for this playbook; empty: `mode`. A riskier mode is narrowed to `mode` |
| `playbooks.<name>` | two disabled examples | See [Playbooks](#playbooks) |
| `playbooks.<name>.guardrails.maxPRs` | `1` | PRs per run; `0` removes `open_pr` |
| `playbooks.<name>.guardrails.maxFileBytes` | `1048576` | Largest file `repo_write_file` may write |
| `playbooks.<name>.guardrails.writePaths` | `[]` | Globs `repo_write_file` may write, e.g. `[Dockerfile, "deploy/**"]`. Required when a full-mode playbook may open PRs; `**`, `*` and other catch-alls are refused. Scanner ignore files need a pattern that names them, e.g. `**/.trivyignore`. Empty (safe mode or no PRs): safe mode's defaults, or all but denied |
| `playbooks.<name>.guardrails.denyPaths` | `[]` | Added to the built-in denylist of CI and CODEOWNERS files |
| `playbooks.<name>.guardrails.reopenAfter` | `0s` | A PR closed without merging blocks the same change this long; `0s`: forever |
| `playbooks.<name>.guardrails.dryRun` | `false` | Dry-run for this playbook only |

### Jira

| Key | Default | Description |
|---|---|---|
| `jira.enabled` | `false` | Turn on `create_issue` |
| `jira.server` | `atlassian` | Enabled `mcpServers` entry running mcp-atlassian |
| `jira.projects` | `[]` | Projects issues may be created in; required when enabled |
| `jira.issueType` | `Task` | Used when the model does not choose one |
| `jira.labels` | `[heraion]` | Added to every issue |
| `playbooks.<name>.jiraProjects` | `[]` | Narrows `jira.projects` for one playbook |
| `playbooks.<name>.guardrails.maxIssues` | `1` | New issues per run; `0` removes `create_issue` |

### Worker

| Key | Default | Description |
|---|---|---|
| `worker.serviceAccount.create`, `.name`, `.annotations` | `true`, `""`, `{}` | Put cloud identity annotations here |
| `worker.automountServiceAccountToken` | `false` | Workers do not need the Kubernetes API |
| `worker.backoffLimit` | `0` | Retries of a failed run |
| `worker.activeDeadlineSeconds` | `1800` | Kill a run after this; keep above `llm.timeout`, or the report is lost |
| `worker.ttlSecondsAfterFinished` | `86400` | Delete finished Jobs after this |
| `worker.workspaceSizeLimit` | `2Gi` | `emptyDir` for clones |
| `worker.cronJob.concurrencyPolicy` | `Forbid` | Skip a schedule while the last run is active |
| `worker.cronJob.successfulJobsHistoryLimit`, `failedJobsHistoryLimit` | `3`, `3` | |
| `worker.cronJob.startingDeadlineSeconds` | `600` | |
| `worker.cronJob.timeZone` | `""` | e.g. `Europe/Berlin` |
| `worker.cronJob.suspend` | `false` | Suspend every CronJob |
| `worker.resources` | 100m / 128Mi requests, 1Gi limit | |
| `worker.extraEnv`, `extraEnvFrom`, `extraVolumes`, `extraVolumeMounts`, `extraContainers`, `initContainers` | `[]` | e.g. `OTEL_EXPORTER_OTLP_ENDPOINT` |
| `worker.podAnnotations`, `podLabels` and the scheduling keys | empty | |

### Gateway

| Key | Default | Description |
|---|---|---|
| `gateway.hooks.<name>.auth` | `hmac-sha256` | Or `token` |
| `gateway.hooks.<name>.header` | `X-Hub-Signature-256`, or `Authorization` for `token` | Header to check |
| `gateway.hooks.<name>.secret` | `{}` | Credential |
| `gateway.hooks.<name>.identity` | `raw` | Which deliveries are the same event: `raw` (the body) or `alertmanager` (`groupKey`, `status`, alert fingerprints) |
| `gateway.replicas` | `1` | Ignored with autoscaling |
| `gateway.maxConcurrentJobs` | `5` | 0 = no limit |
| `gateway.maxBodyBytes` | `262144` | Larger bodies get 413; at most 1032192, since the body is stored in a Secret |
| `gateway.metrics.enabled` | `true` | Prometheus metrics on the gateway Service, see [Monitoring](../../README.md#monitoring) |
| `gateway.metrics.port` | `9090` | Metrics port, separate from the webhook port |
| `gateway.metrics.serviceMonitor.enabled` | `false` | Render a prometheus-operator ServiceMonitor (needs its CRDs) |
| `gateway.metrics.serviceMonitor.interval` | `30s` | Scrape interval |
| `gateway.metrics.serviceMonitor.labels` | `{}` | Extra labels, e.g. `{release: kube-prometheus-stack}` to match the Prometheus selector |
| `gateway.strategy` | RollingUpdate, `maxUnavailable: 0` | |
| `gateway.revisionHistoryLimit` | `5` | |
| `gateway.terminationGracePeriodSeconds` | `30` | |
| `gateway.service.type`, `.port` | `ClusterIP`, `8080` | |
| `gateway.ingress.*` | disabled | `className`, `annotations`, `hosts`, `tls` |
| `gateway.autoscaling.*` | disabled, 2 to 5 replicas, 80% CPU | |
| `gateway.podDisruptionBudget.*` | disabled, `minAvailable: 1` | Or `maxUnavailable` |
| `gateway.livenessProbe`, `readinessProbe`, `startupProbe` | `/healthz`, `/readyz`, none | |
| `gateway.topologySpreadConstraints` | spread over nodes | `labelSelector` is added automatically |
| `gateway.resources` | 50m / 64Mi requests, 256Mi limit | |
| `gateway.serviceAccount.*`, `extra*`, scheduling keys | as for the worker | |

### MCP servers

| Key | Default | Description |
|---|---|---|
| `mcpServers.<name>.enabled` | `false` | |
| `mcpServers.<name>.mode` | | `deploy`, `dependency` or `external` |
| `mcpServers.<name>.url` | | Required for `dependency` and `external`; templated |
| `mcpServers.<name>.path` | `/mcp` | `deploy` only |
| `mcpServers.<name>.transport` | `streamable` | Or `sse` |
| `mcpServers.<name>.allowTools`, `denyTools` | `[]` | e.g. `[pods_*]` |
| `mcpServers.<name>.required` | `false` | Fail the run if the server is down |
| `mcpServers.<name>.timeout` | `60s` | Per tool call |
| `mcpServers.<name>.headers` | `{}` | Plain headers |
| `mcpServers.<name>.secretHeaders.<header>` | | Credential |
| `mcpServers.<name>.deploy.image` | | `registry`, `repository`, `tag`, `digest`, `pullPolicy` |
| `mcpServers.{k8s,atlassian}.deploy.image.digest` | `sha256:...` of the default tag | Wins over `tag`: set `""` when you change `tag` |
| `mcpServers.<name>.deploy.port`, `args`, `command`, `env`, `envFrom` | | Sidecar settings; ports must differ between deploy-mode servers |
| `mcpServers.<name>.deploy.files` | `{}` | Files mounted at `filesMountPath` (default `/etc/mcp`) |
| `mcpServers.<name>.deploy.secretEnv.<VAR>` | | `existingSecret` + `key`, or `value` |
| `mcpServers.<name>.deploy.rbac.clusterRules`, `rules` | read-only for `k8s` | ClusterRole and Role rules |
| `mcpServers.<name>.deploy.resources`, `startupProbe`, `securityContext`, `extraVolumes`, `extraVolumeMounts` | 50m / 64Mi requests, 256Mi limit; no startupProbe | The worker retries a refused connection for 30s while the sidecar starts. A `startupProbe` only works for a server listening on all interfaces: the kubelet probes the pod IP |
| `grafana-mcp.*` | `disableWrite: true`, streamable HTTP | Values for the grafana-mcp subchart |
| `litellm.*` | `enabled: false` | Values for the LiteLLM subchart |

### Network, quota, tests

| Key | Default | Description |
|---|---|---|
| `networkPolicy.enabled` | `true` | Worker ingress denied |
| `admissionPolicy.enabled` | `true` | Admission policies for the gateway's Jobs and Secrets, see [Admission policy](#admission-policy) |
| `networkPolicy.gatewayIngressFrom` | `[]` | Who may call the gateway; empty = anyone |
| `networkPolicy.metricsFrom` | `[]` | Who may scrape `gateway.metrics.port`; empty = anyone |
| `networkPolicy.workerEgress` | `[]` | Egress rules for workers; empty = unrestricted |
| `resourceQuota.enabled`, `.jobs` | `false`, `20` | Hard cap on Jobs |
| `tests.enabled` | `true` | Render the `helm test` pod |
| `tests.args` | `["--connect"]` | Arguments for `heraion check` |

## Testing

`helm test` starts a pod with the worker's pod spec (same env, volumes, ServiceAccount and
labels) and runs `heraion check --connect`:

```
$ helm test heraion -n heraion --logs
ok    config     /etc/heraion/conf/config.yaml
ok    playbooks  trivy-remediate
ok    secret     HERAION_LLM_API_KEY (file) for llm
ok    secret     HERAION_GIT_TOKEN (env) for git
ok    mcp        k8s: 18 tools
ok    llm        GET https://api.openai.com/v1/models: 200
ok    mcp        atlassian: 5 tools
ok    jira       create_issue through atlassian, in [OPS]
```

The check fails on a missing or unreadable required secret, a required MCP server that is
down, a 401 or 403 from the LLM endpoint, or a `jira.server` that lacks the tools and
arguments `create_issue` calls.
Secret values are never printed, only their source: `file`, `env` or `fallback`.

Chart development:

```sh
scripts/test-chart.sh   # or: make chart-test
```

| Check | What it covers |
|---|---|
| `helm lint --strict` | Defaults and every [ci/](ci/) values file |
| helm-unittest | The suites in [tests/](tests/), including values that must be rejected ([reject_test.yaml](tests/reject_test.yaml)) |
| [chart_test.go](chart_test.go) | Renders the chart and loads the result with the binary's own config, playbook and Job-template loaders, so a key the binary does not know fails |
| kubeconform | Rendered output of the defaults, [ci/](ci/) and [examples/helm](../../examples/helm) against Kubernetes 1.30 and 1.35 |

## Upgrading

```sh
helm upgrade heraion oci://ghcr.io/muskats/charts/heraion -n heraion -f my-values.yaml
```

- A change to the config, the playbooks or a chart-managed `value` changes the checksum
  annotations, so the gateway rolls and the next worker Jobs use the new settings. Running Jobs
  finish with the settings they started with.
- A changed existing Secret is not seen by the gateway until `kubectl rollout restart`; see
  [Rotation and restarts](#rotation-and-restarts).
- With `rbac.create: false`, keep your RBAC in line with the chart's: see
  [Security defaults](#security-defaults).
- Breaking changes are listed in the release notes of each chart version. From 2.x to 3.0.0:
  [docs/upgrading-3.md](../../docs/upgrading-3.md), one row per change with what to do.

Behavior changes in 3.0.1 (only the first two rows can need a values change):

| 3.0.0 | 3.0.1 |
|---|---|
| `writePaths` such as `**/**`, `**/*.*` or `{**,x}` load | They are refused as catch-alls, like `**`; list what the playbook may change, e.g. `[Dockerfile, "deploy/**"]` |
| `trigger.webhook` names have up to 52 characters | Up to 32, the limit of the `gateway.hooks` name they must match |
| `repo_read_file` follows symlinks inside the clone | It refuses them and names the target, e.g. "notes.txt is a symlink to .git/config" |
| `create_issue` accepts any label from the model | Labels starting with `heraion-` are refused |
| A HeraIon commit message is the PR title and body | It also ends with `Heraion-Change: <hash>`, which reruns compare |
| `heraion run --event -` cuts stdin at 1 MiB | An event over 1 MiB fails the run, from a file or stdin |

Behavior changes in 2.1.1 (no values need to change):

| 2.1.0 | 2.1.1 |
|---|---|
| An MCP tool call fails after 30s, whatever `mcpServers.<name>.timeout` says | It runs up to that timeout, e.g. 60s |
| Dry-run `create_issue` logs any number of issues | It stops at `guardrails.maxIssues`, like a real run |
| A slow Kubernetes API keeps the webhook request open | The gateway answers 500 after 15s; the sender retries |

Behavior changes in 2.1.0 (no values need to change):

| 2.0.0 | 2.1.0 |
|---|---|
| Tool results cut at 64 KiB | Cut at `llm.maxToolResultBytes`, default 16 KiB; set `65536` for the old size |
| Full history sent on every request | Above `llm.maxContextBytes` (default 200000) the oldest tool results become a stub |
| `tool call` log line has only `tool` | Also `args` (shortened, masked), `resultBytes`, `durationMs`, `error` |
| The final report is only in the `run finished` log line | It is also posted as a comment on each PR the run opened |
| A missing or empty `serviceAccountToken` or `csiKey` file falls back to `existingSecret`, then `value` | It fails the run and `helm test` with the file's env var and path |

Breaking changes in 2.0.0:

| 1.0.0 | 2.0.0 |
|---|---|
| `jira.url`, `email`, `deployment`, `token` | `mcpServers.atlassian.deploy.env` (`JIRA_URL`, `JIRA_USERNAME`) and `secretEnv` (`JIRA_API_TOKEN` or `JIRA_PERSONAL_TOKEN`) |
| `jira.readSpaces`, `confluence.spaces` | `JIRA_PROJECTS_FILTER`, `CONFLUENCE_SPACES_FILTER` on the server |
| `confluence.*` | `CONFLUENCE_*` env on the server |
| Tools `jira_search`, `jira_get_issue`, `confluence_*` | `atlassian.jira_search`, ... from the server |
| Playbook `jiraReadSpaces`, `confluenceSpaces` | Removed; the server filter is the boundary. Use a second mcp-atlassian entry for another scope |
| `heraion_jobs`, `heraion_job_last_success_timestamp_seconds` | kube-state-metrics, see the [main README](../../README.md#monitoring) |

## Uninstall

```sh
helm uninstall heraion -n heraion
```

Finished Jobs are deleted by their TTL. PRs and branches that HeraIon opened stay on the Git
server.
