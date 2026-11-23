# Integrations

Each integration is an existing [MCP](https://modelcontextprotocol.io) server under
`mcpServers.<name>`. With `mcpServers.grafana.enabled: true`, a playbook with
`tools: ["grafana.*"]` can call `grafana.query_prometheus`. All servers are off by default.

Full example: [examples/helm/values-integrations.yaml](../examples/helm/values-integrations.yaml).
Credentials (`secretHeaders`, `secretEnv`) accept the sources described in the chart README's
[Secrets](../charts/heraion/README.md#secrets) section.

## Connector modes

| Mode | The chart creates | Example |
|---|---|---|
| `deploy` | A native sidecar in each worker pod on `127.0.0.1:<port>`, the RBAC you list (bound to the worker ServiceAccount, token mounted in that sidecar only), a ConfigMap for `deploy.files` | kubernetes-mcp-server, mcp-atlassian |
| `dependency` | Nothing; a subchart runs the server and `url` points at it | grafana-mcp |
| `external` | Nothing; workers call `url` | Datadog, Atlassian Rovo |

Options:

```yaml
mcpServers:
  myserver:
    enabled: true
    mode: deploy | dependency | external
    url: ""              # dependency/external, e.g. "http://{{ .Release.Name }}-grafana-mcp:8000/mcp"
    path: /mcp           # deploy only
    transport: streamable  # or sse
    allowTools: []       # e.g. [pods_*, events_list]; empty = all
    denyTools: []        # e.g. [pods_exec]; wins over allowTools
    required: false      # true: fail the run if the server is down
    timeout: 60s         # per tool call
    headers: {}          # e.g. { X-Org: acme }
    secretHeaders:       # sent on every request
      Authorization: { existingSecret: my-secret, key: authorization }
```

Tools are filtered twice. With `allowTools: [pods_*]` on the server and `tools: [k8s.pods_log]`
in a playbook, that playbook gets only `k8s.pods_log`.

## Kubernetes and Trivy Operator (`k8s`)

[kubernetes-mcp-server](https://github.com/containers/kubernetes-mcp-server) with
`read_only = true`. It can read workloads, pod logs, events and Trivy reports
(`aquasecurity.github.io`). It cannot read Secrets, ConfigMaps or `exposedsecretreports`: its
RBAC does not grant them, and its `denied_resources` config refuses Secrets and ConfigMaps
even if you widen the RBAC. Literal `env` values in pod specs are readable; HeraIon masks known
credentials and token shapes in them before the model sees them.

```yaml
mcpServers:
  k8s: { enabled: true }
playbooks:
  trivy-remediate: { enabled: true }
```

To limit the server to some namespaces, set `namespaces: [shop, payments]` (empty: all). The
`clusterRules` are then bound with a RoleBinding in each of them, and HeraIon refuses k8s tool
calls for other namespaces, without a namespace, or with `all_namespaces`. Cluster-scoped
resources such as nodes are not readable. To change access, override
`mcpServers.k8s.deploy.rbac.clusterRules`.

## Grafana (`grafana`)

Installs the [grafana-mcp](https://github.com/grafana-community/helm-charts) subchart with
writes disabled. It needs a Grafana service-account token:

```yaml
mcpServers:
  grafana:
    enabled: true
    allowTools: [search_dashboards, get_dashboard_by_uid, query_prometheus, query_loki_logs]
grafana-mcp:
  grafana:
    url: http://grafana.monitoring.svc
    apiKeySecret: { name: grafana-sa-token, key: token }
```

## Datadog (`datadog`)

Datadog hosts the server. The URL depends on your site, see the
[Datadog MCP docs](https://docs.datadoghq.com/mcp_server/setup). Not available on GovCloud.

```yaml
mcpServers:
  datadog:
    enabled: true
    url: https://mcp.datadoghq.com/api/unstable/mcp-server/mcp
    secretHeaders:
      DD-API-KEY: { existingSecret: datadog, key: api-key }
      DD-APPLICATION-KEY: { existingSecret: datadog, key: app-key }
```

## Jira and Confluence (`atlassian`)

[mcp-atlassian](https://github.com/sooperset/mcp-atlassian), Cloud or Data Center. The
chart's `mcpServers.atlassian` preset enables only `jira_search`, `jira_get_issue`,
`jira_create_issue`, `confluence_search` and `confluence_get_page`. `JIRA_PROJECTS_FILTER`
and `CONFLUENCE_SPACES_FILTER` bound every tool, reads and writes.

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
        - { name: JIRA_USERNAME, value: heraion-bot@acme.com }      # Cloud only
        - { name: JIRA_PROJECTS_FILTER, value: "OPS,CHG" }
      secretEnv:
        JIRA_API_TOKEN: { existingSecret: atlassian, key: api-token }  # Data Center: JIRA_PERSONAL_TOKEN
jira:
  enabled: true
  server: atlassian
  projects: [OPS]
```

`env` is a list, so setting it replaces the preset's list: keep `STATELESS`, `HOME` and
`ENABLED_TOOLS`. Issues are created only through `create_issue`, which adds a project
allowlist, issue budget, de-duplication and dry-run, see the
[README](../README.md#jira-and-confluence). Atlassian's hosted server (Rovo) works for reads
as an `external` entry, but `create_issue` needs mcp-atlassian.

To start a run from a Jira webhook:

```yaml
gateway:
  hooks:
    jira:
      auth: hmac-sha256        # Jira webhook secret -> X-Hub-Signature: sha256=<hex>
      header: X-Hub-Signature
      secret: { existingSecret: heraion-hooks, key: jira }
playbooks:
  jira-bug-to-pr:
    trigger: { webhook: jira } # POST /hooks/jira
    tools: [atlassian.jira_get_issue, repo_*, open_pr]
    prompt: ...
```

## Webhook senders

| Sender | `auth` | `header` | Notes |
|---|---|---|---|
| Alertmanager | `token` | `Authorization` (default) | `http_config.authorization.credentials`; `send_resolved: false`, or each resolved alert starts a run |
| GitHub | `hmac-sha256` | `X-Hub-Signature-256` (default) | |
| Gitea / Forgejo | `hmac-sha256` | `X-Gitea-Signature` | Hex digest, no prefix |
| GitLab | `token` | `X-Gitlab-Token` | |
| Jira | `hmac-sha256` | `X-Hub-Signature` | |
| Grafana alerting | `token` | `Authorization` | Contact point with an authorization header |

A retried delivery is the same event, so it gets the same Job name and starts nothing new
while that run is running or done; a failed run is deleted and the retry runs again. For
Alertmanager, set `identity: alertmanager` on the hook, so a changing annotation value
(`{{ $value }}`) does not make every repeat a new event.

## Your own connector

Any MCP server with streamable HTTP or SSE works:

```yaml
mcpServers:
  pagerduty:
    enabled: true
    mode: deploy
    deploy:
      image: { repository: ghcr.io/example/pagerduty-mcp, tag: "1.2.3" }
      args: ["--http", "127.0.0.1:9100"]   # localhost: only the worker in the same pod connects
      port: 9100                           # unique among deploy-mode servers (k8s 8080, atlassian 9000)
      secretEnv:
        PAGERDUTY_TOKEN: { existingSecret: pagerduty, key: token }
```

A stdio-only server needs a stdio-to-HTTP adapter in its image.

Only connect servers you trust. A server's tool names and descriptions go to the model as
they are, so a hostile server can try to steer it ("tool poisoning"). Narrow each server with
`allowTools`, and remember that the guardrails (repo allowlist, write paths, budgets, no merge)
hold whatever the model is told. Secret headers are sent only to the host in `url`; redirects
to another host are refused.

## LLM providers

`llm.baseURL` is any OpenAI-compatible endpoint with tool calling:

| Provider | `llm.baseURL` |
|---|---|
| OpenAI | `https://api.openai.com/v1` |
| Azure OpenAI, Bedrock, Vertex | LiteLLM (`litellm.enabled: true`, needs PostgreSQL) |
| Anthropic | Its OpenAI-compatible endpoint, or LiteLLM |
| vLLM, Ollama | e.g. `http://vllm:8000/v1` |
| KubeAI | `http://kubeai/openai/v1` |
