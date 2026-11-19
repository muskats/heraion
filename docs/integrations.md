# Integrations

Each integration is an existing [MCP](https://modelcontextprotocol.io) server under
`mcpServers.<name>`. With `mcpServers.grafana.enabled: true`, a playbook with
`tools: ["grafana.*"]` can call `grafana.query_prometheus`. All servers are off by default.

Full example: [examples/helm/values-integrations.yaml](../examples/helm/values-integrations.yaml).

## Connector modes

| Mode | The chart creates | Example |
|---|---|---|
| `deploy` | Deployment, Service, ServiceAccount, the RBAC you list, a ConfigMap for `deploy.files`, a NetworkPolicy that only admits workers | kubernetes-mcp-server, mcp-atlassian |
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
(`aquasecurity.github.io`). It cannot read Secrets, ConfigMaps or `exposedsecretreports`.

```yaml
mcpServers:
  k8s: { enabled: true }
playbooks:
  trivy-remediate: { enabled: true }
```

To change access, override `mcpServers.k8s.deploy.rbac.clusterRules`. For one namespace only,
set `clusterRules: []` and put the rules in `rbac.rules`.

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

## Jira

Server or Data Center: run [mcp-atlassian](https://github.com/sooperset/mcp-atlassian) with a
personal access token and `READ_ONLY_MODE=true`, as in
[values-integrations.yaml](../examples/helm/values-integrations.yaml).

Cloud: use Atlassian's hosted server as an `external` entry with an `Authorization` secret
header. Check Atlassian's docs for the current URL.

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
    tools: ["jira.*", repo_*, open_pr]
    prompt: ...
```

## Webhook senders

| Sender | `auth` | `header` | Notes |
|---|---|---|---|
| Alertmanager | `token` | `Authorization` (default) | `http_config.authorization.credentials` |
| GitHub | `hmac-sha256` | `X-Hub-Signature-256` (default) | |
| Gitea / Forgejo | `hmac-sha256` | `X-Gitea-Signature` | Hex digest, no prefix |
| GitLab | `token` | `X-Gitlab-Token` | |
| Jira | `hmac-sha256` | `X-Hub-Signature` | |
| Grafana alerting | `token` | `Authorization` | Contact point with an authorization header |

A retried delivery has the same payload, so it gets the same Job name and starts nothing new.

## Your own connector

Any MCP server with streamable HTTP or SSE works:

```yaml
mcpServers:
  pagerduty:
    enabled: true
    mode: deploy
    deploy:
      image: { repository: ghcr.io/example/pagerduty-mcp, tag: "1.2.3" }
      args: ["--http", ":9000"]
      port: 9000
      secretEnv:
        PAGERDUTY_TOKEN: { existingSecret: pagerduty, key: token }
```

A stdio-only server needs a stdio-to-HTTP adapter in its image.

## LLM providers

`llm.baseURL` is any OpenAI-compatible endpoint with tool calling:

| Provider | `llm.baseURL` |
|---|---|
| OpenAI | `https://api.openai.com/v1` |
| Azure OpenAI, Bedrock, Vertex | LiteLLM (`litellm.enabled: true`, needs PostgreSQL) |
| Anthropic | Its OpenAI-compatible endpoint, or LiteLLM |
| vLLM, Ollama | e.g. `http://vllm:8000/v1` |
| KubeAI | `http://kubeai/openai/v1` |
