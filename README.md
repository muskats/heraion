# HeraIon

HeraIon looks into problems in a Kubernetes cluster with an LLM and proposes fixes as pull
requests. When a problem needs a person instead, it can open a Jira issue. It never merges,
and it cannot change the cluster.

Example: Trivy Operator reports a fixable CVE in `alpine:3.18`. The `trivy-remediate`
playbook reads the report, finds the Dockerfile in `acme/web`, and opens a PR on the branch
`heraion/trivy-remediate/bump-alpine-3-20`. On the next run the PR is still open, so no
second PR is opened. If an alert has no config fix, `alertmanager-triage` opens one Jira
issue with the evidence, and reruns find that issue instead of creating another.

It works with any OpenAI-compatible endpoint (OpenAI, vLLM, Ollama, KubeAI, LiteLLM) and any
Git server go-scm supports (GitHub, GitLab, Gitea, Bitbucket, including self-hosted).
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
guardrails: { maxPRs: 3 }
```

Built-in tools:

| Tool | Does |
|---|---|
| `repo_list`, `repo_read_file`, `repo_write_file` | Read and edit a clone of an allowed repo |
| `open_pr` | Commit the edits and open a pull request |
| `create_issue` | Create a Jira issue, see [Jira issues](#jira-issues) |
| `jira_search`, `jira_get_issue` | Read recent issues in chosen Jira spaces, see [Reading Jira and Confluence](#reading-jira-and-confluence) |
| `confluence_search`, `confluence_get_page` | Read recent pages in chosen Confluence spaces |

MCP tools are named `<server>.<tool>`, so `k8s.*` matches every tool of the `k8s` server.

More examples: [trivy-remediate.yaml](examples/playbooks/trivy-remediate.yaml),
[alertmanager-triage.yaml](examples/playbooks/alertmanager-triage.yaml).

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

In dry-run the issue is only logged. Reading Jira, e.g. searching for related tickets, works
through a Jira MCP server; see [docs/integrations.md](docs/integrations.md#jira).

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
| `HERAION_JIRA_TOKEN` | Jira Cloud API token or Data Center PAT; also `_FILE` and `_FALLBACK` |
| `HERAION_CONFLUENCE_TOKEN` | Confluence Cloud API token or Data Center PAT; also `_FILE` and `_FALLBACK` |
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
| Jira projects | With `jira.projects: [OPS]`, an issue for `SEC` is refused |
| Read spaces | With `jira.readSpaces: [CHG]` and `confluence.spaces: [RUNBOOKS]`, reading `HR-1` or a page in `HR` is refused |
| Issue budget | With `maxIssues: 1`, a second new issue is refused |
| Branches | Only `heraion/<playbook>/...` branches are pushed |
| Duplicates | An open PR on the same branch, or an open issue with the same fingerprint label, is reused |
| Masking | `ghp_...`, `glpat-...`, `sk-...`, `Bearer ...`, PEM keys and configured secrets become `[REDACTED]` in prompts, PRs, issues and logs |
| Run size | `llm.maxSteps`, `llm.maxTokens`, `llm.timeout` |
| No merge | The code has no merge call |

## Development

```sh
./bin/heraion check --config examples/config.yaml
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
