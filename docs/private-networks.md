# Private networks

HeraIon reaches a Git server (and an LLM or MCP server) behind a VPN, a proxy or Tailscale.
Only worker pods and the `helm test` pod connect to Git; the gateway never does. Clones and
API calls use Go's HTTP client, so they follow `HTTPS_PROXY`, `NO_PROXY` and `SSL_CERT_DIR`.

| Setup | Use | HeraIon values |
|---|---|---|
| Cluster network routes to the VPN (site-to-site VPN, cloud VPN, subnet router on the nodes) | [VPN](#vpn) | `git.endpoint`, maybe `worker.caBundle` |
| Tailscale Kubernetes operator in the cluster | [Tailscale egress Service](#tailscale-egress-service) | `extraObjects`, maybe `worker.hostAliases` |
| Tailscale, no operator | [Tailscale sidecar](#tailscale-sidecar) | `worker.initContainers`, `worker.proxy` |
| HTTP proxy, e.g. Squid | [Proxy](#proxy) | `worker.proxy` |

Check the result with `helm test`, see [Check](#check).

## Settings

| Value | Does | Example |
|---|---|---|
| `worker.caBundle.pem` | Trusts these CAs besides the system CAs | The CA that signed `git.corp.example` |
| `worker.caBundle.configMap`, `.key` | Same, from an existing ConfigMap | A trust-manager `Bundle` target |
| `worker.proxy.url` | Sets `HTTP_PROXY` and `HTTPS_PROXY` | `http://127.0.0.1:1055` |
| `worker.proxy.noProxy` | Added to `localhost,127.0.0.1,::1,.svc,.cluster.local` | `[vllm, 10.0.0.0/8]` |
| `worker.hostAliases` | `/etc/hosts` entries | `git.corp.example` -> `10.96.12.34` |
| `worker.dnsPolicy`, `worker.dnsConfig` | Pod DNS | `{ searches: [corp.example] }` |
| `networkPolicy.workerEgress` | Egress rules of worker pods | Only DNS and `10.20.0.0/16:443` |

The [admission policy](../charts/heraion/README.md#admission-policy) admits only the
`hostAliases`, `dnsPolicy`, DNS nameservers and searches the chart renders, so the gateway
cannot point the Git server's name at another address.

## VPN

The nodes reach `git.corp.example` already, so pods do too:

```yaml
git:
  provider: gitlab
  endpoint: https://git.corp.example
worker:
  caBundle:
    pem: |
      -----BEGIN CERTIFICATE-----
      ...
      -----END CERTIFICATE-----
  dnsConfig: { searches: [corp.example] }   # only if cluster DNS does not resolve the name
networkPolicy:
  workerEgress:
    - ports: [{ port: 53, protocol: UDP }, { port: 53, protocol: TCP }]
    - to: [{ ipBlock: { cidr: 10.20.0.0/16 } }]          # the VPN range
      ports: [{ port: 443, protocol: TCP }]
    - to: [{ ipBlock: { cidr: 0.0.0.0/0, except: [10.0.0.0/8] } }]
      ports: [{ port: 443, protocol: TCP }]              # the LLM, e.g. api.openai.com
```

## Tailscale egress Service

The [Tailscale Kubernetes operator](https://tailscale.com/kb/1438/kubernetes-operator-cluster-egress)
turns an `ExternalName` Service into a proxy to a tailnet device. The workers need no
Tailscale of their own.

```yaml
extraObjects:
  - apiVersion: v1
    kind: Service
    metadata:
      name: git-tailnet
      annotations:
        tailscale.com/tailnet-fqdn: gitlab.tail1234.ts.net   # or tailscale.com/tailnet-ip: 10.20.0.5
        tailscale.com/proxy-group: egress                   # optional: a ProxyGroup of type egress (HA)
    spec:
      type: ExternalName
      externalName: placeholder                             # the operator sets it
      ports: [{ name: https, port: 443, protocol: TCP }]
```

TLS checks the name in the URL, so pick the URL by the server's certificate:

| Certificate is for | `git.endpoint` | Also needed |
|---|---|---|
| `gitlab.tail1234.ts.net` (`tailscale cert`, `tailscale serve`) | `https://gitlab.tail1234.ts.net` | A [DNSConfig](https://tailscale.com/docs/kubernetes-operator/egress/enable-magicdns-resolution) and a `ts.net` stub zone in CoreDNS |
| `git.corp.example`, behind a subnet router | `https://git.corp.example` | `worker.hostAliases` to the egress Service's ClusterIP, and `worker.caBundle` for an internal CA |
| Nothing checked (plain HTTP) | `http://git-tailnet.heraion.svc` | Nothing; the token crosses the cluster network unencrypted |

A server behind a subnet router needs a ProxyGroup whose ProxyClass sets
`tailscale.acceptRoutes: true`, and `tailscale.com/tailnet-ip`. With a ProxyGroup the
`ExternalName` points at a ClusterIP Service in the operator's namespace:

```sh
target=$(kubectl -n heraion get svc git-tailnet -o jsonpath='{.spec.externalName}')
kubectl -n tailscale get svc "${target%%.*}" -o jsonpath='{.spec.clusterIP}'   # e.g. 10.96.12.34
```

```yaml
worker:
  hostAliases: [{ ip: 10.96.12.34, hostnames: [git.corp.example] }]
```

The ClusterIP stays while the Service exists; recreating the egress Service changes it.

## Tailscale sidecar

Each worker pod joins the tailnet as an ephemeral device that is removed after the run.
Tailscale runs in userspace mode, without `NET_ADMIN` or `/dev/net/tun`, and offers an HTTP
proxy on `127.0.0.1:1055`. Addresses outside the tailnet go out directly.

```sh
kubectl -n heraion create secret generic tailscale-auth \
  --from-literal=authkey=tskey-client-...    # OAuth client secret (auth_keys scope), or an ephemeral auth key
```

```yaml
git:
  endpoint: https://gitlab.tail1234.ts.net   # MagicDNS name, resolved by Tailscale
worker:
  proxy: { url: "http://127.0.0.1:1055" }
  initContainers:
    - name: tailscale
      image: ghcr.io/tailscale/tailscale:v1.104.4
      restartPolicy: Always                  # native sidecar: stops when the run ends
      env:
        - { name: TS_AUTHKEY, valueFrom: { secretKeyRef: { name: tailscale-auth, key: authkey } } }
        - { name: TS_EXTRA_ARGS, value: "--advertise-tags=tag:heraion" }
        - { name: TS_HOSTNAME, value: heraion-worker }
        - { name: TS_KUBE_SECRET, value: "" } # state in /tmp; workers have no API token
        - { name: TS_STATE_DIR, value: /tmp }
        - { name: TS_USERSPACE, value: "true" }
        - { name: TS_OUTBOUND_HTTP_PROXY_LISTEN, value: "127.0.0.1:1055" }
        - { name: TS_ENABLE_HEALTH_CHECK, value: "true" }   # /healthz on :9002
      startupProbe: { httpGet: { path: /healthz, port: 9002 }, periodSeconds: 2, failureThreshold: 60 }
      securityContext: { allowPrivilegeEscalation: false, readOnlyRootFilesystem: true, capabilities: { drop: [ALL] } }
      resources: { requests: { cpu: 20m, memory: 64Mi }, limits: { memory: 256Mi } }
      volumeMounts: [{ name: tailscale-tmp, mountPath: /tmp }]
  extraVolumes:
    - { name: tailscale-tmp, emptyDir: { sizeLimit: 64Mi } }
```

The worker starts after `/healthz` answers 200, i.e. once the device has a tailnet IP.
Limit the tag in the tailnet policy, e.g.
`{"src": ["tag:heraion"], "dst": ["tag:git"], "ip": ["tcp:443"]}`.

With `networkPolicy.workerEgress` set, allow Tailscale out:

```yaml
networkPolicy:
  workerEgress:
    - ports: [{ port: 53, protocol: UDP }, { port: 53, protocol: TCP }]
    - ports: [{ port: 443, protocol: TCP }, { port: 3478, protocol: UDP }]   # control plane, DERP, STUN
    - ports: [{ port: 41641, protocol: UDP }]                                # direct connections; without it DERP relays
```

## Proxy

```yaml
worker:
  proxy:
    url: http://proxy.corp.example:3128
    noProxy: [vllm, git.corp.example]   # short Service names and hosts reached directly
```

Every outbound call of the worker uses the proxy: Git, the LLM, and MCP servers with a URL.
Sidecars on `127.0.0.1` never do. The MCP sidecar containers do not get these variables;
set them in `mcpServers.<name>.deploy.env` if they need the proxy.

## Check

`helm test` runs `heraion check --connect` with the worker's pod spec, so it uses the same
proxy, CAs, DNS and sidecars. Each repo is listed like `git ls-remote`, without cloning:

```
$ helm test heraion -n heraion --logs
ok    git        acme/web: API https://git.corp.example, clone https://git.corp.example/acme/web.git
FAIL  git        acme/api: https://git.corp.example/acme/api.git: list refs: Get "https://git.corp.example/acme/api.git/info/refs?service=git-upload-pack": tls: failed to verify certificate: x509: certificate signed by unknown authority
```

| Error | Cause |
|---|---|
| `x509: certificate signed by unknown authority` | Set `worker.caBundle` |
| `x509: certificate is valid for git.corp.example, not git-tailnet.heraion.svc` | Use the certificate's name in the URL, with `worker.hostAliases` |
| `dial tcp: lookup git.corp.example: no such host` | DNS: `worker.dnsConfig`, `worker.hostAliases` or a CoreDNS stub zone |
| `i/o timeout` | No route, or `networkPolicy.workerEgress` blocks it |
| `proxyconnect tcp: ... connection refused` | The proxy or the Tailscale sidecar is not listening |

In dry-run the API is only checked when it looks clone URLs up (`stash`, `azure`).
