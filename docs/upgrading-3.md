# Upgrading to 3.0.0

Breaking changes from 2.x, one row each. Check every row against your values and playbooks
before `helm upgrade`.

| 2.x | 3.0.0 | What to do |
|---|---|---|
| `mode: yolo` (global or playbook) pushes to the default branch | Removed: `mode: yolo` fails to load with `mode "yolo" was removed in 3.0.0` | Use `mode: full`; fixes arrive as PRs that need review |
| A full-mode playbook that opens PRs may write any file but CI files | It must list `guardrails.writePaths`; `run`, `serve` and `helm test` refuse it otherwise. `**`, `**/*`, `*` are refused | Add e.g. `writePaths: [Dockerfile, "deploy/**"]`, or `mode: safe` |
| Registry and package-manager configs (`.npmrc`, `pip.conf`, `settings.xml`, ...) can be written | Never written, like CI files | Change them by hand |
| `.trivyignore` and other scanner ignore files are written under any matching pattern | Only when a `writePaths` pattern names the file, e.g. `**/.trivyignore` | Add that pattern to playbooks that may mute findings |
| Chart default `trivy-remediate` runs in the global mode (default `full`) | `mode: safe` | Set `playbooks.trivy-remediate.mode: full` and `writePaths` for the old behavior |
| Chart default `alertmanager-triage` may write any file | `writePaths: ["deploy/**/values*.yaml", "**/kustomization.yaml"]` | Override `playbooks.alertmanager-triage.guardrails.writePaths` for other layouts |
| Safe mode accepts any lockfile change and new lines in scanner ignore files | Both are refused | Use full mode with `writePaths` for ecosystems that need lockfile updates; ignore files need full mode and a pattern naming them |
| Safe mode accepts `alpine:3.18` -> `3.20` (same first number) | Distro images (`alpine`, `debian`, `ubuntu`, `ubi9`, ...) get patch bumps only, e.g. `3.20.1` -> `3.20.3` | Distro releases need full mode and review |
| Safe mode refuses any change to an image digest | `@sha256:<64 hex>` may be replaced by another on an image reference | Nothing |
| Branch names come from the model, e.g. `heraion/trivy-remediate/bump-alpine-3-20` | `heraion/<playbook>/<first path>-<hash>`, e.g. `heraion/trivy-remediate/dockerfile-1b2c3d4e` | Close or merge the open HeraIon PRs from 2.x before upgrading; they are not recognized |
| `open_pr` takes a `branch` argument | No `branch` argument; the changed files name the branch | Remove "branch key" instructions from your playbook prompts |
| An open PR on the branch is skipped | It is force-pushed and its title and body refreshed when the change differs; left alone when its head commit is not HeraIon's | Nothing |
| `guardrails.reopenAfter` defaults to `168h` and blocks any new PR on the branch | Defaults to `0s` and blocks only the same change (marker `<!-- heraion:change=... -->`) | Set `reopenAfter: 168h` to allow the same change again after a week |
| A failed webhook run blocks every repeat of the event until its Job's TTL | The gateway deletes the failed Job and answers 503 with `Retry-After: 30` | Nothing; the gateway Role gets `delete` on Jobs |
| Webhook duplicates are byte-identical bodies | `gateway.hooks.<name>.identity`: `raw` (default) or `alertmanager` | Set `identity: alertmanager` on Alertmanager hooks |
| `create_issue` fingerprints come from the model's key, or the summary without one | Webhook runs: from the event (`HERAION_EVENT_ID`); scheduled runs: the key is required and must match `^[a-z0-9][a-z0-9-]{2,59}$` | Open issues from 2.x webhook runs are not found again; close them or let them be |
| Kubernetes 1.27+ | Kubernetes 1.30+ (native sidecars, ValidatingAdmissionPolicy) | Upgrade the cluster first |
| `mode: deploy` MCP servers run as a Deployment with a Service, ServiceAccount and NetworkPolicy | Native sidecars of each worker pod on `127.0.0.1:<port>`; the Deployments, Services, ServiceAccounts and NetworkPolicies are deleted on upgrade | Ports of deploy-mode servers must differ; worker pods request the sidecars' resources too |
| MCP RBAC is bound to the server's ServiceAccount | Bound to the worker ServiceAccount; only the sidecar mounts its token | Cloud identity annotations for an MCP server go on `worker.serviceAccount.annotations` |
| `mcpServers.<name>.deploy.replicas`, `readinessProbe`, `livenessProbe`, `serviceAccount`, `podLabels`, `podAnnotations`, scheduling keys | Rejected by the schema | Remove them; use `deploy.startupProbe`, and `worker.*` for scheduling |
| A custom deploy-mode server listening on all interfaces | Reached as `http://127.0.0.1:<port>` | Bind it to `127.0.0.1` if it supports that |
| No admission policy | `admissionPolicy.enabled: true` installs two ValidatingAdmissionPolicies and bindings | The installing user needs permission for cluster-scoped admission objects; set `false` to skip them |
| The gateway may `patch` and `delete` Secrets | `create` only; each event Secret is owned by its Job from creation | Nothing (`rbac.create: false`: update your Role) |
