# HeraIon

HeraIon looks into problems in a Kubernetes cluster with an LLM and proposes fixes as pull
requests. It never merges, and it cannot change the cluster.

Example: Trivy Operator reports a fixable CVE in `alpine:3.18`. The `trivy-remediate`
playbook reads the report, finds the Dockerfile in `acme/web`, and opens a PR on the branch
`heraion/trivy-remediate/bump-alpine-3-20`. On the next run the PR is still open, so no
second PR is opened.

It works with any OpenAI-compatible endpoint (OpenAI, vLLM, Ollama, KubeAI, LiteLLM) and any
Git server go-scm supports (GitHub, GitLab, Gitea, Bitbucket, including self-hosted).
Integrations such as Kubernetes, Grafana, Datadog and Jira are MCP servers you turn on one by
one.

## Try it locally

```sh
make build
export HERAION_LLM_API_KEY=sk-...
export HERAION_GIT_TOKEN=...
./bin/heraion run --config examples/config.yaml --playbook trivy-remediate --dry-run
```

With `--dry-run` the agent clones the repos and edits files, but each PR is only logged with
its diff:

```json
{"level":"INFO","msg":"dry-run: would open PR","repo":"platform/web","branch":"heraion/trivy-remediate/bump-alpine-3-20","diff":"--- a/Dockerfile\n+++ b/Dockerfile\n-FROM alpine:3.18\n+FROM alpine:3.20\n"}
```

## Install with Helm

```sh
kubectl create namespace heraion
kubectl -n heraion create secret generic heraion \
  --from-literal=llm-api-key=sk-... --from-literal=git-token=ghp_...
helm install heraion oci://ghcr.io/muskats/charts/heraion -n heraion \
  -f examples/helm/values-minimal.yaml
helm test heraion -n heraion   # runs `heraion check --connect` with the worker's pod spec
```

| Values file | Installs |
|---|---|
| [values-minimal.yaml](examples/helm/values-minimal.yaml) | A CronJob for `trivy-remediate` and the read-only k8s MCP server |
| [values-selfhosted.yaml](examples/helm/values-selfhosted.yaml) | Gitea, LiteLLM, and a webhook gateway for Alertmanager |
| [values-integrations.yaml](examples/helm/values-integrations.yaml) | Jira, Grafana and Datadog servers, plus report and webhook playbooks |
| [values-identity.yaml](examples/helm/values-identity.yaml) | No static secrets: AWS Secrets Manager via CSI and IRSA, and a projected token for an MCP server |

What runs in the cluster:

| Component | Created when | Notes |
|---|---|---|
| Worker Job | A CronJob fires or a webhook arrives | One Job per run, no ServiceAccount token, read-only root filesystem |
| Gateway Deployment | A webhook playbook is enabled | RBAC: `create` and `list` on Jobs, nothing else |
| MCP server | `mcpServers.<name>.enabled: true` | Its own ServiceAccount and RBAC; only worker pods can connect |

## Secrets

Every credential (`llm.apiKey`, `git.token`, `gateway.hooks.<name>.secret`,
`mcpServers.<name>.secretHeaders.<header>`) takes up to three sources. The first one that
yields a value at runtime wins:

| Order | Values | Pod gets | Example |
|---|---|---|---|
| 1 | `serviceAccountToken: { audience: vault }` | `<ENV>_FILE`, a projected ServiceAccount token, rotated | An MCP server or LLM gateway that trusts the cluster's OIDC issuer |
| 1 | `csiKey: llm-api-key` | `<ENV>_FILE` from the Secrets Store CSI driver | AWS Secrets Manager, Azure Key Vault or Google Secret Manager, via IRSA or Workload Identity |
| 2 | `existingSecret: llm`, `key: api-key` | `<ENV>` from an optional `secretKeyRef` | A Secret from External Secrets or Sealed Secrets |
| 3 | `value: sk-...` | `<ENV>_FALLBACK` from the chart's own Secret | Quick tests |

`secrets.existingSecret` is the default Secret for level 2, with keys `llm-api-key`,
`git-token`, `hook-<name>` and `mcp-<server>-<header>`. A key missing from it is skipped:

```yaml
secrets:
  existingSecret: heraion        # holds git-token and hook-alertmanager
llm:
  apiKey:
    csiKey: llm-api-key          # 1: from the cloud secret manager
    value: sk-local-dev          # 3: used only when the CSI file is missing
```

Token files are re-read, so rotation needs no restart. Changing a chart-managed value rolls
the pods (`checksum/secret`). `heraion check` lists the source of each secret, never the value:

```
ok    secret     HERAION_LLM_API_KEY (file) for llm
ok    secret     HERAION_GIT_TOKEN (env) for git
```

Servers deployed by the chart (`mcpServers.<name>.deploy.secretEnv`) read plain env vars, so
the source is picked when the chart is rendered: their own `existingSecret`, then `value`,
then `secrets.existingSecret`.

## Playbooks

A playbook says when to run, which tools to use and what to do:

```yaml
name: trivy-remediate
trigger: { schedule: "0 */6 * * *" }   # or { webhook: alertmanager }, or { manual: true }
tools: [k8s.resources_list, k8s.resources_get, repo_*, open_pr]
repos: [acme/web]                      # optional, narrows the global repos list
prompt: |
  List VulnerabilityReports with fixable HIGH or CRITICAL CVEs and open one PR per repo
  with the minimal version bump.
output: pr                             # "report" removes open_pr
guardrails: { maxPRs: 3 }
```

Built-in tools: `repo_list`, `repo_read_file`, `repo_write_file` and `open_pr`. MCP tools are
named `<server>.<tool>`, so `k8s.*` matches every tool of the `k8s` server.

More examples: [trivy-remediate.yaml](examples/playbooks/trivy-remediate.yaml),
[alertmanager-triage.yaml](examples/playbooks/alertmanager-triage.yaml).

## Webhooks

`heraion serve` accepts `POST /hooks/<name>` and starts every playbook with
`trigger: { webhook: <name> }`. An Alertmanager receiver looks like this:

```yaml
receivers:
  - name: heraion
    webhook_configs:
      - url: http://heraion-gateway.heraion.svc:8080/hooks/alertmanager
        http_config:
          authorization: { credentials: <secret> }
```

| Response | Meaning |
|---|---|
| `202` | Jobs created, e.g. `{"jobs":[{"playbook":"alertmanager-triage","job":"heraion-alertmanager-triage-3f9a1c2b7d"}]}` |
| `202` with `"duplicate": true` | The same payload was already received; no new Job |
| `401` | Wrong or missing secret |
| `429` | `maxConcurrentJobs` reached; the sender retries |

## Configuration

Settings come from a YAML file ([examples/config.yaml](examples/config.yaml)) and secrets
come from env vars. A secret in the file is rejected.

| Env var | Example |
|---|---|
| `HERAION_LLM_API_KEY` | `sk-...`; also `_FILE` and `_FALLBACK`, see [Secrets](#secrets) |
| `HERAION_GIT_TOKEN` | A token that can push branches and open PRs; also `_FILE` and `_FALLBACK` |
| `HERAION_LLM_BASE_URL`, `HERAION_LLM_MODEL` | Override the file, e.g. `http://vllm:8000/v1` |
| `HERAION_DRY_RUN` | `true` |
| `HERAION_CONFIG`, `HERAION_PLAYBOOK`, `HERAION_EVENT_FILE` | Defaults for `heraion run` flags |
| `HERAION_EVENT` | Webhook payload, set by the gateway |
| `HERAION_MCP_<SERVER>_<HEADER>`, `HERAION_HOOK_<NAME>_SECRET` | e.g. `HERAION_MCP_DATADOG_DD_API_KEY` |

## Limits on what a run can do

| Limit | Example |
|---|---|
| Repo allowlist | With `repos: [acme/web]`, a tool call for `acme/billing` fails |
| File access | `../../etc/passwd`, `.git/config` and symlinks out of the clone are refused |
| PR budget | With `maxPRs: 1`, a second `open_pr` call fails |
| Branches | Only `heraion/<playbook>/...` branches are pushed |
| Duplicates | If an open PR already uses the branch, it is reused |
| Masking | `ghp_...`, `glpat-...`, `sk-...`, `Bearer ...`, PEM keys and configured secrets become `[REDACTED]` in prompts, PRs and logs |
| Run size | `llm.maxSteps`, `llm.maxTokens`, `llm.timeout` |
| No merge | The code has no merge call |

## Development

```sh
heraion check --config examples/config.yaml   # config, playbooks and secret sources
make test         # Go tests with the race detector
make chart-test   # helm lint, chart cross-check, helm-unittest
make lint
make image
```

`charts/heraion/chart_test.go` renders the chart and loads the output with the binary's own
loaders, so a key the binary does not know makes the test fail.

Still to do: the end-to-end tests on a `kind` cluster, and against real GitHub and Gitea
servers.

## License

Apache-2.0
