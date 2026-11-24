# HeraIon

HeraIon looks into problems in a Kubernetes cluster with an LLM and proposes fixes as pull
requests. When a problem needs a person instead, it can open a Jira issue. It never merges,
never pushes to the default branch, and it cannot change the cluster.

Example: Trivy Operator reports a CVE in `alpine:3.20.1` that `3.20.3` fixes. The
`trivy-remediate` playbook reads the report, finds the Dockerfile in `acme/web`, and opens a
PR on the branch `heraion/trivy-remediate/dockerfile-1b2c3d4e`, named after the changed file.
On the next run the PR is still open with the same change, so nothing is pushed; a newer fix
for the same file updates that PR, and a change a reviewer closed is never proposed again. If an alert has no config fix, `alertmanager-triage` opens one Jira
issue with the evidence, and reruns find that issue instead of creating another.

It works with any OpenAI-compatible endpoint (OpenAI, vLLM, Ollama, KubeAI, LiteLLM) and any
Git server go-scm supports: GitHub, GitLab, Gitea, Bitbucket Cloud and Server, Azure
DevOps and Gogs, SaaS or self-hosted.
Integrations such as Kubernetes, Grafana, Datadog and Jira are MCP servers you turn on one by
one.

## Documentation

| Document | Contents |
|---|---|
| This README | What HeraIon does, playbooks, webhooks, configuration, limits |
| [charts/heraion/README.md](charts/heraion/README.md) | Helm install, secrets, every chart value, testing, upgrading |
| [Monitoring](#monitoring) | Prometheus metrics of the gateway, Loki queries for worker runs |
| [docs/integrations.md](docs/integrations.md) | Kubernetes, Grafana, Datadog, Jira, Confluence and custom MCP servers; webhook senders; LLM providers |
| [docs/private-networks.md](docs/private-networks.md) | Git servers behind a VPN, a proxy or Tailscale; private CAs |
| [examples/](examples/) | Runtime config, playbooks, Helm values |
| [docs/upgrading-3.md](docs/upgrading-3.md) | Every breaking change from 2.x to 3.0.0, with what to do |
| [SECURITY.md](SECURITY.md) | Supported versions and how to report a vulnerability privately |

## Try it locally

```sh
make build
export HERAION_LLM_API_KEY=sk-...
export HERAION_GIT_TOKEN=...
./bin/heraion run --config examples/config.yaml --playbook trivy-remediate --dry-run
```

| Command | Does |
|---|---|
| `heraion run --playbook NAME [--dry-run] [--event FILE]` | Runs one playbook, then exits (what a worker Job runs) |
| `heraion serve` | Webhook gateway: creates a worker Job per request |
| `heraion check [--role gateway] [--connect]` | Checks config, playbooks and where each secret comes from; with `--connect` also the MCP servers, the LLM and each repo |
| `heraion version` | Prints the version |

With `--dry-run` the agent clones the repos and edits files, but each PR is only logged with
its diff:

```json
{"level":"INFO","msg":"dry-run: would open PR","repo":"platform/web","branch":"heraion/trivy-remediate/dockerfile-1b2c3d4e","diff":"--- a/Dockerfile\n+++ b/Dockerfile\n-FROM alpine:3.20.1\n+FROM alpine:3.20.3\n"}
```

## Install with Helm

```sh
kubectl create namespace heraion
kubectl -n heraion create secret generic heraion \
  --from-literal=llm-api-key=sk-... --from-literal=git-token=ghp_...
helm install heraion oci://ghcr.io/muskats/charts/heraion -n heraion \
  -f examples/helm/values-minimal.yaml
helm test heraion -n heraion
```

The **[chart README](charts/heraion/README.md)** covers the chart in detail:

| Topic | Section |
|---|---|
| Requirements and install from a checkout | [Install](charts/heraion/README.md#install) |
| Which objects are created, and when | [What gets deployed](charts/heraion/README.md#what-gets-deployed) |
| ServiceAccount tokens, CSI, existing Secrets, literal fallbacks | [Secrets](charts/heraion/README.md#secrets) |
| Enabling, scheduling and running playbooks | [Playbooks](charts/heraion/README.md#playbooks) |
| Hooks, Ingress, Alertmanager receiver, load limits | [Webhooks](charts/heraion/README.md#webhooks) |
| Every value with its default | [Values reference](charts/heraion/README.md#values-reference) |
| `helm test`, breaking changes | [Testing](charts/heraion/README.md#testing), [Upgrading](charts/heraion/README.md#upgrading) |

Example values: [minimal](examples/helm/values-minimal.yaml),
[self-hosted](examples/helm/values-selfhosted.yaml),
[integrations](examples/helm/values-integrations.yaml),
[identity-based secrets](examples/helm/values-identity.yaml).

## Secrets

Each credential comes from a file when one is configured (a ServiceAccount token or a cloud
secret manager file), otherwise from an existing Secret, then a literal value. A configured
file that is missing or empty fails the run and `heraion check`; it never falls back. Example:

```yaml
secrets:
  existingSecret: heraion        # default Secret: llm-api-key, git-token, hook-<name>, ...
llm:
  apiKey:
    csiKey: llm-api-key          # AWS/Azure/GCP secret manager via the CSI driver
git:
  token:
    value: ghp-local-dev         # used only if heraion has no git-token key
```

Outside Kubernetes the same order applies to env vars, e.g. `HERAION_LLM_API_KEY_FILE`, then
`HERAION_LLM_API_KEY`, then `HERAION_LLM_API_KEY_FALLBACK`. See the chart README's
[Secrets](charts/heraion/README.md#secrets) section for every option.

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
mode: safe                             # optional: safe or full, see Modes
guardrails:
  maxPRs: 3                            # default 1; 0 removes open_pr
  writePaths: [Dockerfile, "deploy/**"]  # required in full mode when the playbook opens PRs
  reopenAfter: 0s                      # default: a change closed without merging is never proposed again
```

Built-in tools:

| Tool | Does |
|---|---|
| `repo_list`, `repo_read_file`, `repo_write_file` | Read and edit a clone of an allowed repo |
| `open_pr` | Commit the edits and open a pull request. Each call ends with a clean clone, so after a refusal the model writes the files again |
| `create_issue` | Create a Jira issue through mcp-atlassian, see [Jira and Confluence](#jira-and-confluence) |

MCP tools are named `<server>.<tool>`, so `k8s.*` matches every tool of the `k8s` server.

A full-mode playbook that can open PRs must list `writePaths`; `run`, `serve` and
`heraion check` refuse it otherwise. Catch-alls such as `**`, `**/*`, `*` or `**/*.*` are
refused. Safe mode and playbooks without `open_pr` (report output, `maxPRs: 0`, no repos)
need none.

More examples: [trivy-remediate.yaml](examples/playbooks/trivy-remediate.yaml),
[alertmanager-triage.yaml](examples/playbooks/alertmanager-triage.yaml).

## Modes

The mode says how `open_pr` delivers a fix:

| Mode | Does | Example |
|---|---|---|
| `safe` | PR on `heraion/<playbook>/<key>`; only versions and image digests may change | `node:20.11` -> `20.18` or `alpine:3.20.1` -> `3.20.3` opens a PR; `node:20` -> `22` and `alpine:3.18` -> `3.20` are refused |
| `full` (default) | PR on `heraion/<playbook>/<key>`, for any change in `writePaths` | A memory limit raised from `512Mi` to `1Gi` |

```yaml
# config.yaml (chart value: mode)
mode: full    # the default, and the riskiest mode a playbook may use

# playbook
mode: safe    # optional; empty means the global mode
```

A playbook can only pick an equal or safer mode: with global `safe`, a playbook with
`mode: full` runs as `safe` and the run logs a warning.

Safe mode checks the diff, not only the prompt. `open_pr` refuses the change unless every
changed line differs from the old one only in these tokens:

| Token | Allowed change | Refused |
|---|---|---|
| Version | An upgrade within the major version: `v1.4.2` -> `v1.5.0`, `3.1.4-r5` -> `3.1.4-r6`, `node:20.11-alpine3.19` -> `20.18-alpine3.19`. For `0.x` the minor version counts as major | A major bump, a downgrade, a new variant (`-alpine` -> `-bookworm`), a quantity (`1.5Gi`), an IP address (`10.0.0.1`) |
| Distro image tag (`alpine`, `debian`, `ubuntu`, `fedora`, `ubi9`, ...) | Only the patch level: `alpine:3.20.1` -> `3.20.3` | `alpine:3.18` -> `3.20`, `ubuntu:22.04` -> `24.04`: distro releases do not follow semver |
| Image digest | `@sha256:<64 hex>` for another, with or without a tag change | A malformed digest, a digest added or removed, a changed registry or name |

Any other dotted number counts as a version, e.g. `ratio: 1.5` -> `1.9` passes; narrow with
`writePaths`. A distro tag is only recognized right after the image name, e.g.
`FROM alpine:3.20.1` or `image: docker.io/library/alpine:3.20.1`, not in a separate `tag:` key.

Lockfiles (`go.sum`, `package-lock.json`, `Chart.lock`, ...) and scanner ignore files
(`.trivyignore`, `.grype.yaml`, ...) are refused: the worker has no package manager, so a
lockfile it writes is stale or invented, and one ignore entry can mute a whole package type.
Ecosystems that need a lockfile update with each bump (npm, Go modules, Poetry, ...) are not
supported in safe mode; use full mode with `writePaths` and review.

New, deleted and renamed files are refused. Without `writePaths`, safe mode writes only
dependency manifests and image references (`Dockerfile`, `go.mod`, `package.json`,
`values*.yaml`, ...). With `writePaths`, those paths apply instead, and the diff check still runs.

## Jira and Confluence

Jira and Confluence, Cloud or Data Center, are reached through
[mcp-atlassian](https://github.com/sooperset/mcp-atlassian) (chart value
`mcpServers.atlassian`). Its filters bound every tool, reads and writes:

```yaml
# chart values
mcpServers:
  atlassian:
    enabled: true
    deploy:
      env:
        # env replaces the chart's list, so keep these three: ENABLED_TOOLS keeps
        # write tools such as jira_update_issue away from the model.
        - { name: STATELESS, value: "true" }
        - { name: HOME, value: /tmp }
        - { name: ENABLED_TOOLS, value: "jira_search,jira_get_issue,jira_create_issue,confluence_search,confluence_get_page" }
        - { name: JIRA_URL, value: https://jira.example.com }
        - { name: JIRA_PROJECTS_FILTER, value: "OPS,CHG" }
        - { name: CONFLUENCE_URL, value: https://wiki.example.com }
        - { name: CONFLUENCE_SPACES_FILTER, value: RUNBOOKS }
      secretEnv:
        JIRA_PERSONAL_TOKEN: { existingSecret: jira, key: pat }   # Cloud: JIRA_USERNAME + JIRA_API_TOKEN
jira:
  enabled: true
  server: atlassian
  projects: [OPS]            # create_issue
```

```yaml
# playbook
tools: [k8s.*, atlassian.jira_search, atlassian.confluence_search, create_issue]
jiraProjects: [OPS]          # optional, narrows jira.projects
guardrails: { maxIssues: 1 } # default 1
```

Reading uses the server's tools directly, e.g. `atlassian.jira_get_issue`. Creating goes
through HeraIon's `create_issue`, which wraps the server's `jira_search` and
`jira_create_issue`; the model never sees `jira_create_issue` itself.

1. The model calls `create_issue` with a project, a summary, a Markdown description and a
   `key` such as `oomkilled-shop-payments-api` (`<kind>-<namespace>-<workload>`, lowercase,
   3 to 60 characters; other keys are refused).
2. HeraIon checks the project against `jira.projects`, masks secrets, and adds the labels
   from `jira.labels` (chart default `heraion`), `heraion-<playbook>` and a fingerprint such
   as `heraion-1b2c3d4e5f60`: a hash of the playbook, the project and the key. In a webhook
   run the event identity (`HERAION_EVENT_ID`) replaces the key, so the model's wording has
   no influence and one event gets one issue. A label from the model that starts with
   `heraion-` is refused, so it cannot carry another finding's fingerprint.
3. A fingerprint already used in the same run returns that earlier result. Otherwise HeraIon searches
   `project = "OPS" AND labels = "heraion-1b2c3d4e5f60" AND statusCategory != Done`.
   A match is returned instead of a new issue. Once the issue is done, the next occurrence
   opens a new one.
4. Otherwise it spends one of `maxIssues` and calls `jira_create_issue`. In dry-run only the
   log line is written.

`heraion check --connect` fails if the server cannot be reached or lacks these tools or their
arguments, e.g. after an mcp-atlassian upgrade.

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
| `202` with `"duplicate": true` | The same event was already received and its run is running or done; no new Job |
| `401` | Wrong or missing secret |
| `413` | Body larger than `gateway.maxBodyBytes` (default 256 KiB) |
| `429` | `maxConcurrentJobs` reached; the sender retries |
| `503` with `Retry-After: 30` | The run for this event failed and its Job is being deleted; the sender's retry starts a new run |
| `500` | The Job could not be created, e.g. a ResourceQuota is full; the sender retries |

The Job name is a hash of the hook, the playbook and the event identity, so a retried delivery
maps to the same Job. The identity is set per hook:

| `gateway.hooks.<name>.identity` | Same event when |
|---|---|
| `raw` (default) | The body is byte-for-byte the same |
| `alertmanager` | `groupKey`, `status` and each alert's `fingerprint` and `status` are the same; a changed annotation such as `{{ $value }}` is the same event, a new or resolved alert is a new one |

The payload is stored in a Secret owned by the Job (deleted with it) and
mounted into the worker, which reads it from `HERAION_EVENT_FILE`. The model sees the first
64 KiB, marked as untrusted.

## Monitoring

The gateway serves Prometheus metrics at `:9090/metrics` (`gateway.metricsListen`), a separate
port from the webhooks. With the chart, set `gateway.metrics.serviceMonitor.enabled=true` for
the prometheus-operator.

| Metric | Labels | Meaning |
|---|---|---|
| `heraion_webhook_requests_total` | `hook`, `code` | Webhook requests by HTTP status; unknown hook names count as `hook="unknown"` |
| `heraion_dispatches_total` | `hook`, `playbook`, `result` | `created`, `duplicate`, `retry_failed` (503), `at_capacity` (429) or `error` |
| `go_*`, `process_*` | | Go runtime and process |

Job state comes from [kube-state-metrics](https://github.com/kubernetes/kube-state-metrics).
For the playbook label, run it with
`--metric-labels-allowlist=jobs=[heraion.io/playbook]`:

```promql
# no successful scheduled trivy-remediate run for 13 hours (survives Job cleanup)
time() - kube_cronjob_status_last_successful_time{cronjob=~".*-heraion-trivy-remediate"} > 13 * 3600
# failed worker Jobs per playbook, webhook and scheduled runs alike
sum by (label_heraion_io_playbook) (kube_job_status_failed * on (namespace, job_name) group_left(label_heraion_io_playbook) kube_job_labels{label_heraion_io_playbook!=""})
```

Workers are short-lived Jobs, so per-run details are in their JSON logs, one `run finished`
line per run, e.g. for Loki:

```logql
# failed runs per playbook
sum by (playbook) (count_over_time({app_kubernetes_io_component="worker"} | json | msg="run finished" | status="failed" [1h]))
# tokens used per playbook
sum by (playbook) (sum_over_time({app_kubernetes_io_component="worker"} | json | msg="run finished" | unwrap tokens [1d]))
```

`run finished` carries `status` (`ok` or `failed`), `error`, `durationSeconds`, `steps`,
`tokens`, `mode`, `dryRun`, `prs` and `issues`.

Each tool call is one `tool call` line, so a run can be reconstructed without file contents
or secrets in the log:

```json
{"msg":"tool call","playbook":"trivy-remediate","tool":"repo_write_file","args":"{\"content\":\"<412 bytes>\",\"path\":\"Dockerfile\",\"repo\":\"acme/web\"}","resultBytes":31,"durationMs":2,"error":false}
```

| Field | Content |
|---|---|
| `args` | JSON arguments, masked; strings over 256 bytes and `repo_write_file` content as `"<N bytes>"`; at most 1 KiB |
| `resultBytes`, `durationMs` | Result size before the `llm.maxToolResultBytes` cut, call duration |
| `error`, `err` | `true` and the message when the call failed |

`context trimmed` is logged once per run when older tool results are replaced to stay under
`llm.maxContextBytes`. The final report is also posted as a comment on each PR the run opened
(not in dry-run; a failed comment is a warning; Azure DevOps and Gogs do not support it).

## Configuration

Settings come from a YAML file ([examples/config.yaml](examples/config.yaml)) and secrets
come from env vars. A secret in the file is rejected.

| Env var | Example |
|---|---|
| `HERAION_LLM_API_KEY` | `sk-...`; also `_FILE` and `_FALLBACK`, see [Secrets](#secrets) |
| `HERAION_GIT_TOKEN` | A token that can push branches and open PRs; also `_FILE` and `_FALLBACK` |
| `HERAION_LLM_BASE_URL`, `HERAION_LLM_MODEL` | Override the file, e.g. `http://vllm:8000/v1` |
| `HERAION_DRY_RUN` | `true` |
| `HERAION_CONFIG`, `HERAION_PLAYBOOK`, `HERAION_EVENT_FILE` | Defaults for `heraion run` flags; gateway Jobs set `HERAION_EVENT_FILE` to the mounted payload |
| `HERAION_MCP_<SERVER>_<HEADER>`, `HERAION_HOOK_<NAME>_SECRET` | e.g. `HERAION_MCP_DATADOG_DD_API_KEY` |

Git server:

```yaml
git:
  provider: gitea                        # github (default), gitlab, gitea, bitbucket, stash, gogs, azure
  endpoint: https://git.example.com      # API URL; empty for github.com, gitlab.com and bitbucket.org
  cloneBaseURL: https://git.example.com  # default: endpoint, github.com, gitlab.com or bitbucket.org
  username: x-access-token               # clone and push user; the token is the password
```

Clone URLs are `<cloneBaseURL>/<owner>/<repo>.git`. For `stash` (Bitbucket Server) and
`azure`, whose clone URLs have another shape, leave `cloneBaseURL` empty: HeraIon asks the API
for each repo's clone URL. The token needs to read the repos, push branches and open PRs.

## Limits on what a run can do

| Limit | Example |
|---|---|
| Repo allowlist | With `repos: [acme/web]`, a tool call for `acme/billing` fails |
| Namespaces | With `namespaces: [shop]`, `k8s.resources_list` in `kube-system`, without a namespace or with `all_namespaces` fails; the chart binds the k8s server's RBAC to `shop` only. Empty: all namespaces |
| File access | `../../etc/passwd` and `.git/config` are refused, and nothing is read or written through a symlink: reading `notes.txt -> .git/config` returns "notes.txt is a symlink to .git/config" |
| PR budget | With `maxPRs: 1`, a second `open_pr` call fails. `maxPRs: 0` removes `open_pr` |
| Write paths | Never written, whatever `writePaths` say, also not through a symlink: CI, hook and ownership files (`.github/workflows/**`, `.gitlab-ci.yml`, `Jenkinsfile`, `CODEOWNERS`, `.pre-commit-config.yaml`, ...) and package-manager and registry configs (`.npmrc`, `.yarnrc.yml`, `pip.conf`, `.pypirc`, `settings.xml`, `.mvn/**`, `nuget.config`, `.cargo/config.toml`, `gradle.properties`, ...). Full mode needs `writePaths: [Dockerfile, "deploy/**"]`; `denyPaths` adds patterns. Scanner ignore files (`.trivyignore`, `.grype.yaml`, ...) are written only when a pattern names them, e.g. `**/.trivyignore`; `**` or `deploy/**` do not |
| Rejected PRs | A PR closed without merging blocks exactly that change (the `heraion:change` marker in its body) for `reopenAfter`; default `0s`, forever. Another change on the same files may open a new PR |
| Jira projects | With `jira.projects: [OPS]`, `create_issue` for `SEC` is refused |
| Atlassian spaces | With `JIRA_PROJECTS_FILTER=CHG,OPS` on mcp-atlassian, reading `HR-1` or creating in `HR` is refused by the server |
| Issue budget | With `maxIssues: 1`, a second new issue is refused. `maxIssues: 0` removes `create_issue` |
| Branches | Only `heraion/<playbook>/...` branches are pushed, never the default branch |
| Mode | A playbook's `mode` can only be equal to or safer than the global one; in `safe` mode `open_pr` refuses changes other than versions within the major version (distro images: patch only) and image digests, and every lockfile or ignore-file change |
| Duplicates | The branch is `heraion/<playbook>/<first path>-<hash of repo and paths>`, so one PR per playbook, repo and set of changed files (a fork's branch does not count). Its open PR is left alone for the same change (the `Heraion-Change: <hash>` line that ends each HeraIon commit), force-pushed and refreshed for a new one, and never touched when its head commit is not HeraIon's. An open issue with the same fingerprint label is reused |
| Masking | `ghp_...`, `glpat-...`, `sk-...`, `ATATT3...`, JWTs, `Bearer ...`, `user:password@` in URLs, PEM keys and configured secrets become `[REDACTED]` in prompts, PRs, issues and logs. `open_pr` refuses a change that adds such a value to a file |
| Run size | `llm.maxSteps`, `llm.maxTokens` (checked before each round of tool calls), `llm.timeout`; each tool result is cut at `llm.maxToolResultBytes` (16 KiB), and above `llm.maxContextBytes` (200000) the oldest results become a stub. The defaults need a model context of about 64k tokens |
| Webhook body | `gateway.maxBodyBytes`, default 256 KiB, at most 1032192 (it is stored in a Secret) |
| No merge | The code has no merge call and never pushes to the default branch |

## CI secrets and bot branches

HeraIon pushes `heraion/*` branches to the repository itself, and CI usually runs same-repo
branches with the repository's secrets. A prompt-injected change in a build script, test or
Dockerfile `RUN` line could reach those secrets. `writePaths` narrows what can change; keep
the secrets away from these branches too:

| CI | Do |
|---|---|
| GitHub Actions | Put secret-using jobs behind an environment with required reviewers, or keep them out of `pull_request` workflows. Do not use `pull_request_target` with a checkout of the PR head |
| GitLab | Mark variables as protected and do not protect `heraion/*` |
| Any | Require CODEOWNERS review for the paths in `writePaths` |

## Development

Every check is a script in [scripts/](scripts/). CI runs the same scripts, and so can you:

```sh
scripts/ci.sh                 # lint, unit, chart, e2e, kind, then a summary
scripts/ci.sh unit chart      # some stages
make build                    # bin/heraion
```

| Stage | Script | Checks |
|---|---|---|
| `lint` | `scripts/lint.sh` | gofmt, `Chart.yaml` version equals appVersion, Dockerfile Go version equals the go.mod toolchain, go vet, `go mod tidy` drift, golangci-lint ([.golangci.yml](.golangci.yml)), govulncheck, actionlint, shellcheck |
| `unit` | `scripts/test-unit.sh` | `go test -race`, coverage floor `COVERAGE_MIN` (default 80%), `build/coverage.html` |
| `chart` | `scripts/test-chart.sh` | helm lint, helm-unittest ([charts/heraion/tests](charts/heraion/tests)), chart/binary cross-check, kubeconform for Kubernetes 1.30 and 1.35, values that must be rejected |
| `e2e` | `scripts/test-e2e.sh` | [test/e2e](test/e2e): the binary on the rendered chart config, against a mock kube-apiserver, LLM and Gitea API and a real Git remote: webhook to Job and event Secret, worker run to pushed branch, PR and report comment, dedup, closed-PR cooldown, write-path and secret refusals |
| `kind` | `scripts/test-kind.sh` | The chart on kind clusters, Kubernetes 1.30 and 1.37 (`KIND_K8S=1.30` for one): `helm test` with the k8s sidecar and an LLM stub ([test/stubs/llm](test/stubs/llm)), a webhook run with a k8s tool call, the admission policy refusing a rogue Job from the gateway ServiceAccount, no token in the worker container. Needs docker, kubectl and jq; skipped without docker outside CI |

You need Go, helm and git. The scripts build the other tools (golangci-lint, govulncheck,
kubeconform, helm-unittest, actionlint) at pinned versions into `.tools/`; the versions are in
[scripts/lib.sh](scripts/lib.sh). With `CI=true` (set by most CI systems), optional tools such
as shellcheck become required.

| Env | Effect |
|---|---|
| `JUNIT=1` | Also write JUnit XML to `build/` (unit and e2e need `gotestsum`) |
| `COVERAGE_MIN=85` | Raise the coverage floor; `0` turns it off |
| `GO_TEST_FLAGS="-run TestOpenPR"` | Extra `go test` flags for `unit` and `e2e` |
| `HERAION_BIN=dist/heraion` | Run the e2e suite against a given binary, e.g. a release build |
| `SKIP_VULN=1`, `SKIP_DEPS=1` | Skip govulncheck or `helm dependency build`, e.g. offline |
| `SKIP_UNITTEST=1` | Skip helm-unittest locally (refused when `CI=true`) |
| `KEEP_GOING=1` | `ci.sh` runs every stage even after a failure |

The Go version is in two places, the `toolchain` line of [go.mod](go.mod) and
`ARG GO_VERSION` in the [Dockerfile](Dockerfile); the `lint` stage fails when they differ.
Base images and the default MCP server images are pinned by digest. CI builds the image and
scans it with Trivy (`HIGH` and `CRITICAL` with a fix fail the build).

Releasing: set `version` and `appVersion` in [Chart.yaml](charts/heraion/Chart.yaml) to the
same value, e.g. `2.1.0`, merge to `main`, then push the tag `v2.1.0`. The release workflow
refuses a tag that does not match `Chart.yaml` or is not on `main`, runs the whole CI workflow
on the tagged commit, and only then publishes the image `docker.io/muskats/heraion:2.1.0`
(also `v2.1.0`, with SBOM and provenance) and the chart `oci://ghcr.io/muskats/charts/heraion`
version `2.1.0`. With the repo variable `HERAION_EVALS=true`, the eval suite
([test/evals](test/evals)) runs against a real model first and a failed safety scenario stops
the release; it then needs the variables `HERAION_EVAL_BASE_URL` and `HERAION_EVAL_MODEL` and
the secret `HERAION_EVAL_API_KEY`. Without the switch the evals are skipped. The image and the chart
are signed with cosign keyless signing (GitHub OIDC):

```sh
cosign verify docker.io/muskats/heraion:2.1.0 \
  --certificate-identity https://github.com/muskats/heraion/.github/workflows/release.yaml@refs/tags/v2.1.0 \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
cosign verify ghcr.io/muskats/charts/heraion:2.1.0 \
  --certificate-identity https://github.com/muskats/heraion/.github/workflows/release.yaml@refs/tags/v2.1.0 \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

## License

Apache-2.0
