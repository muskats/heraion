# Security

HeraIon holds repository write tokens and reads untrusted input (pod logs, alerts, CVE text),
so reports about how that can be abused are welcome.

## Supported versions

| Version | Security fixes |
|---|---|
| 3.0.x | Yes |
| 2.1.x | Until 2027-05-31 |
| 2.0.x and older | No |

## Reporting a vulnerability

Report privately through GitHub: **Security** tab of this repository, then **Report a
vulnerability**. Do not open a public issue.

Include the version (chart and image), the configuration or playbook involved, and the steps
or the input that triggers it. You get an answer within 7 days. A fix is released before the
report is published, and you are credited unless you ask not to be.

## In scope

| Area | Example |
|---|---|
| Guardrail bypass | A model or input that makes `open_pr` write a CI file, a registry config or a path outside `writePaths`, push to the default branch, or pass safe mode with a change it should refuse |
| Secret leakage | A credential reaching a prompt, a PR, an issue, a comment or a log line unmasked |
| Privilege escalation through the chart | The gateway or a worker getting more than the chart grants, e.g. a Job or Secret the admission policy should refuse, or a Kubernetes token in the worker container |
| Webhook authentication | A request accepted without the hook's secret |

## Out of scope

- Wrong or poor fixes proposed by the model, when every guardrail held: review the PR.
- Settings an administrator chose, e.g. `insecure: true` on a hook or a broad `writePaths`.
- Vulnerabilities in a third-party MCP server or LLM provider: report them upstream.
