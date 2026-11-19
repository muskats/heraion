# HeraIon

HeraIon looks into problems in a Kubernetes cluster with an LLM and proposes fixes as pull
requests, or, in [yolo mode](#modes), pushes them to the default branch. When a problem needs
a person instead, it can open a Jira issue. It never merges, and it cannot change the cluster.

Example: Trivy Operator reports a fixable CVE in `alpine:3.18`. The `trivy-remediate`
playbook reads the report, finds the Dockerfile in `acme/web`, and opens a PR on the branch
`heraion/trivy-remediate/bump-alpine-3-20`. On the next run the PR is still open, so no
second PR is opened. If an alert has no config fix, `alertmanager-triage` opens one Jira
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
| [docs/integrations.md](docs/integrations.md) | Kubernetes, Grafana, Datadog, Jira, Confluence and custom MCP servers; webhook senders; LLM providers |
| [examples/](examples/) | Runtime config, playbooks, Helm values |

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
| `heraion check [--role gateway] [--connect]` | Checks config, playbooks and where each secret comes from; with `--connect` also the MCP servers and the LLM |
| `heraion version` | Prints the version |

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

Each credential is read from the first source that has a value: a ServiceAccount token or
cloud secret manager file, then an existing Secret, then a literal value. Example:

```yaml
secrets:
  existingSecret: heraion        # default Secret: llm-api-key, git-token, hook-<name>, ...
llm:
  apiKey:
    csiKey: llm-api-key          # first: AWS/Azure/GCP secret manager via the CSI driver
    value: sk-local-dev          # last: only if nothing else is set
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
mode: safe                             # optional: safe, full or yolo, see Modes
guardrails:
  maxPRs: 3                            # default 1; 0 removes open_pr
  writePaths: [Dockerfile, "deploy/**"]  # optional; CI files are always refused
  reopenAfter: 168h                    # a rejected PR is not reopened for a week
```

Built-in tools:

| Tool | Does |
|---|---|
| `repo_list`, `repo_read_file`, `repo_write_file` | Read and edit a clone of an allowed repo |
| `open_pr` | Commit the edits and open a pull request (in yolo mode: push them) |
| `create_issue` | Create a Jira issue, see [Jira issues](#jira-issues) |
| `jira_search`, `jira_get_issue` | Read recent issues in chosen Jira spaces, see [Reading Jira and Confluence](#reading-jira-and-confluence) |
| `confluence_search`, `confluence_get_page` | Read recent pages in chosen Confluence spaces |

MCP tools are named `<server>.<tool>`, so `k8s.*` matches every tool of the `k8s` server.

More examples: [trivy-remediate.yaml](examples/playbooks/trivy-remediate.yaml),
[alertmanager-triage.yaml](examples/playbooks/alertmanager-triage.yaml).

## Modes

The mode says how `open_pr` delivers a fix:

| Mode | Does | Example |
|---|---|---|
| `safe` | PR on `heraion/<playbook>/<key>`, only for non-breaking changes | `alpine:3.18` -> `3.20` opens a PR; `node:20` -> `22` is refused |
| `full` (default) | PR on `heraion/<playbook>/<key>`, for any change | A memory limit raised from `512Mi` to `1Gi` |
| `yolo` | Commit pushed to the default branch, without review | `alpine:3.18` -> `3.20` lands on `main` |

```yaml
# config.yaml (chart value: mode)
mode: full    # the default, and the riskiest mode a playbook may use

# playbook
mode: safe    # optional; empty means the global mode
```

A playbook can only pick an equal or safer mode: with global `full`, a playbook with
`mode: yolo` runs as `full` and the run logs a warning. Yolo needs `mode: yolo` in the global
config too.

Safe mode checks the diff, not only the prompt. `open_pr` refuses the change unless each
changed file is one of:

| File | Allowed change |
|---|---|
| Lockfile (`go.sum`, `package-lock.json`, `yarn.lock`, `poetry.lock`, ...) | Any |
| Ignore file (`.trivyignore`, `.grype.yaml`, `.snyk`, `*.vex.json`, ...) | Added lines only, e.g. `CVE-2024-1234 # not reachable` |
| Any other file | Only versions change, each an upgrade within the same major version: `v1.4.2` -> `v1.5.0`, `3.1.4-r5` -> `3.1.4-r6`. A major bump, a downgrade, a new variant (`-alpine` -> `-bookworm`) or a quantity (`1.5Gi`) is refused. For `0.x` the minor version counts as major |

New, deleted and renamed files are refused, except new ignore files and lockfiles. Without
`writePaths`, safe mode writes only dependency manifests, image references, lockfiles and
ignore files (`Dockerfile`, `go.mod`, `package.json`, `values*.yaml`, ...). With `writePaths`,
those paths apply instead, and the diff check still runs.

Yolo mode pushes without force, so the push only works as a fast-forward. If it is rejected,
for example by branch protection or because the branch moved since the clone, the same commit
goes to `heraion/<playbook>/<key>` as a PR, with the usual duplicate and closed-PR checks. With
`--dry-run`, yolo logs the commit and its diff instead of pushing.

## Jira issues

`create_issue` opens an issue in Jira Cloud or Jira Data Center. Use it for findings that need
a person, e.g. an alert without a config fix, or a CVE with no fixed version yet.

```yaml
# config.yaml (the chart writes this from jira.* values)
jira:
  url: https://acme.atlassian.net   # or https://jira.example.com
  email: heraion-bot@acme.com       # Cloud API token: Basic auth. Omit for a Data Center PAT
  projects: [OPS]
  issueType: Task
  labels: [heraion]
```

```yaml
# playbook
tools: [k8s.*, create_issue]
jiraProjects: [OPS]                # optional, narrows jira.projects
guardrails: { maxIssues: 1 }       # default 1
```

The token is `HERAION_JIRA_TOKEN`: a Cloud API token with `email`, or a Data Center personal
access token without it.

| | Jira Cloud | Jira Data Center |
|---|---|---|
| Detected by | `url` on `*.atlassian.net`, or `deployment: cloud` | Anything else, or `deployment: datacenter` |
| Create | `POST /rest/api/3/issue`, description in ADF | `POST /rest/api/2/issue`, description in wiki markup |
| Search | `POST /rest/api/3/search/jql` | `POST /rest/api/2/search` |
| Auth | `Basic base64(email:api-token)` | `Bearer <personal access token>` |

How an issue is created:

1. The model calls `create_issue` with a project, a summary, a Markdown description and a
   `key` such as `oomkilled-payments-api`. HeraIon converts the Markdown to ADF or wiki markup.
2. HeraIon adds the labels `heraion`, `heraion-<playbook>` and a fingerprint such as
   `heraion-1b2c3d4e5f60`, computed from the playbook, project and key.
3. Before creating, it searches
   `project = "OPS" AND labels = "heraion-1b2c3d4e5f60" AND statusCategory != Done`. If that
   finds an issue, it is returned and nothing new is created. Once the issue is done, the next
   occurrence opens a new one.

In dry-run the issue is only logged. To search for related tickets, use the read tools in
[Reading Jira and Confluence](#reading-jira-and-confluence).

## Reading Jira and Confluence

Optional read-only tools give the agent context, e.g. "ingress-nginx was upgraded in CHG-42
yesterday" or the rollback steps from a runbook. Each one is limited to the spaces you choose.

```yaml
# config.yaml (the chart writes this from jira.* and confluence.* values)
jira:
  url: https://acme.atlassian.net
  email: heraion-bot@acme.com
  readSpaces: [CHG, OPS]           # jira_search, jira_get_issue
confluence:
  url: https://acme.atlassian.net/wiki
  email: heraion-bot@acme.com
  spaces: [RUNBOOKS]               # confluence_search, confluence_get_page
```

```yaml
# playbook
tools: [k8s.*, jira_search, jira_get_issue, confluence_search, confluence_get_page]
jiraReadSpaces: [CHG]              # optional, narrows jira.readSpaces
confluenceSpaces: [RUNBOOKS]       # optional, narrows confluence.spaces
```

| Tool | Arguments | Returns |
|---|---|---|
| `jira_search` | `space`, `days` (default 7, max 90), `text`, `limit` (max 50) | One line per issue: `CHG-42 \| Change \| Done \| High \| 2026-11-18T10:02 \| Upgrade ingress-nginx to 1.12` |
| `jira_get_issue` | `key`, e.g. `CHG-42` | Fields, description and the last 5 comments as text |
| `confluence_search` | `space`, `days` (default 30, max 365), `text`, `limit` (max 50) | One line per page: id, space, last change, title, excerpt |
| `confluence_get_page` | `id` from `confluence_search` | The page as plain text, cut at `maxPageBytes` (default 32 KiB) |

How the space limit is enforced:

- HeraIon writes the query itself, e.g.
  `project in ("CHG") AND updated >= -7d AND text ~ "ingress" ORDER BY updated DESC` or
  `space in ("RUNBOOKS") AND type = page AND lastmodified >= now("-30d")`. The model only
  supplies the search text, which is escaped.
- `jira_get_issue` and `confluence_get_page` check the space of the returned issue or page
  before giving anything to the model. An issue that moved to another space is refused.
- Content is marked as untrusted, and secrets in it are masked like any tool result.
- Comment authors are not sent to the model.

| API | Jira Cloud | Jira Data Center |
|---|---|---|
| Search | `POST /rest/api/3/search/jql` | `POST /rest/api/2/search` |
| Get issue | `GET /rest/api/3/issue/{key}` (ADF converted to text) | `GET /rest/api/2/issue/{key}` |

| API | Confluence Cloud | Confluence Data Center |
|---|---|---|
| Search | `GET /wiki/rest/api/search?cql=...` (v1; v2 has no CQL search) | `GET /rest/api/content/search?cql=...` |
| Get page | `GET /wiki/api/v2/pages/{id}?body-format=storage` | `GET /rest/api/content/{id}?expand=body.storage,space,version` |

The Confluence token is `HERAION_CONFLUENCE_TOKEN`. On Cloud, the same Atlassian API token as
Jira works.

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
| `202` with `"duplicate": true` | The same payload was already received and its Job still exists; no new Job |
| `401` | Wrong or missing secret |
| `413` | Body larger than `gateway.maxBodyBytes` (default 256 KiB) |
| `429` | `maxConcurrentJobs` reached; the sender retries |
| `500` | The Job could not be created, e.g. a ResourceQuota is full; the sender retries |

The Job name is a hash of the hook, the playbook and the payload, so a retried delivery maps
to the same Job. The payload is stored in a Secret owned by the Job (deleted with it) and
mounted into the worker, which reads it from `HERAION_EVENT_FILE`. The model sees the first
64 KiB, marked as untrusted.

## Configuration

Settings come from a YAML file ([examples/config.yaml](examples/config.yaml)) and secrets
come from env vars. A secret in the file is rejected.

| Env var | Example |
|---|---|
| `HERAION_LLM_API_KEY` | `sk-...`; also `_FILE` and `_FALLBACK`, see [Secrets](#secrets) |
| `HERAION_GIT_TOKEN` | A token that can push branches and open PRs; also `_FILE` and `_FALLBACK` |
| `HERAION_JIRA_TOKEN` | Jira Cloud API token or Data Center PAT; also `_FILE` and `_FALLBACK` |
| `HERAION_CONFLUENCE_TOKEN` | Confluence Cloud API token or Data Center PAT; also `_FILE` and `_FALLBACK` |
| `HERAION_LLM_BASE_URL`, `HERAION_LLM_MODEL` | Override the file, e.g. `http://vllm:8000/v1` |
| `HERAION_DRY_RUN` | `true` |
| `HERAION_CONFIG`, `HERAION_PLAYBOOK`, `HERAION_EVENT_FILE` | Defaults for `heraion run` flags; gateway Jobs set `HERAION_EVENT_FILE` to the mounted payload |
| `HERAION_MCP_<SERVER>_<HEADER>`, `HERAION_HOOK_<NAME>_SECRET` | e.g. `HERAION_MCP_DATADOG_DD_API_KEY` |

Git server:

```yaml
git:
  provider: gitea                        # github (default), gitlab, gitea, bitbucket, stash, gogs, azure
  endpoint: https://git.example.com      # API URL; empty for github.com and gitlab.com
  cloneBaseURL: https://git.example.com  # default: endpoint, github.com or gitlab.com
  username: x-access-token               # clone and push user; the token is the password
```

Clone URLs are `<cloneBaseURL>/<owner>/<repo>.git`. For `stash` (Bitbucket Server) and
`azure`, whose clone URLs have another shape, leave `cloneBaseURL` empty: HeraIon asks the API
for each repo's clone URL. The token needs to read the repos, push branches and open PRs.

## Limits on what a run can do

| Limit | Example |
|---|---|
| Repo allowlist | With `repos: [acme/web]`, a tool call for `acme/billing` fails |
| File access | `../../etc/passwd`, `.git/config` and symlinks out of the clone are refused |
| PR budget | With `maxPRs: 1`, a second `open_pr` call fails. `maxPRs: 0` removes `open_pr` |
| Write paths | CI and ownership files (`.github/workflows/**`, `.gitlab-ci.yml`, `Jenkinsfile`, `CODEOWNERS`, ...) are never written, also not through a symlink. `writePaths: [Dockerfile, "deploy/**"]` narrows further, `denyPaths` adds patterns |
| Rejected PRs | A PR closed without merging blocks a new PR on the same branch for `reopenAfter` (default `168h`; `0s`: forever) |
| Jira projects | With `jira.projects: [OPS]`, an issue for `SEC` is refused |
| Read spaces | With `jira.readSpaces: [CHG]` and `confluence.spaces: [RUNBOOKS]`, reading `HR-1` or a page in `HR` is refused |
| Issue budget | With `maxIssues: 1`, a second new issue is refused. `maxIssues: 0` removes `create_issue` |
| Branches | Only `heraion/<playbook>/...` branches are pushed, and in yolo mode the default branch, fast-forward only |
| Mode | A playbook's `mode` can only be equal to or safer than the global one; in `safe` mode `open_pr` refuses changes that are not version upgrades within the same major version, lockfile updates or new ignore-file entries |
| Duplicates | An open PR on the same branch of the same repo (a fork's branch does not count), or an open issue with the same fingerprint label, is reused |
| Masking | `ghp_...`, `glpat-...`, `sk-...`, `ATATT3...`, JWTs, `Bearer ...`, `user:password@` in URLs, PEM keys and configured secrets become `[REDACTED]` in prompts, PRs, issues and logs. `open_pr` refuses a change that adds such a value to a file |
| Run size | `llm.maxSteps`, `llm.maxTokens` (checked before each round of tool calls), `llm.timeout` |
| Webhook body | `gateway.maxBodyBytes`, default 256 KiB, at most 1032192 (it is stored in a Secret) |
| No merge | The code has no merge call; yolo mode pushes a commit instead |

## Development

Every check is a script in [scripts/](scripts/). CI runs the same scripts, and so can you:

```sh
scripts/ci.sh                 # lint, unit, chart, e2e, then a summary
scripts/ci.sh unit chart      # some stages
make build                    # bin/heraion
```

| Stage | Script | Checks |
|---|---|---|
| `lint` | `scripts/lint.sh` | gofmt, go vet, `go mod tidy` drift, golangci-lint ([.golangci.yml](.golangci.yml)), govulncheck, actionlint, shellcheck |
| `unit` | `scripts/test-unit.sh` | `go test -race`, coverage floor `COVERAGE_MIN` (default 80%), `build/coverage.html` |
| `chart` | `scripts/test-chart.sh` | helm lint, helm-unittest ([charts/heraion/tests](charts/heraion/tests)), chart/binary cross-check, kubeconform for Kubernetes 1.27 and 1.33, values that must be rejected |
| `e2e` | `scripts/test-e2e.sh` | [test/e2e](test/e2e): the binary on the rendered chart config, against a mock kube-apiserver, LLM and Gitea API and a real Git remote: webhook to Job and event Secret, worker run to pushed branch and PR, dedup, closed-PR cooldown, write-path and secret refusals |

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

Still to do: end-to-end tests on a `kind` cluster, and against real GitHub and Gitea servers.

## License

Apache-2.0
