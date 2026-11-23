#!/usr/bin/env bash
# Smoke test on a kind cluster: the chart with the k8s MCP sidecar, an LLM
# stub (test/stubs/llm) and a report-only webhook playbook in dry-run.
#
#   1. helm install and helm test (check --connect: sidecar, LLM)
#   2. a webhook run finishes with status ok after a k8s.* tool call
#   3. the gateway ServiceAccount cannot create a Job with another image or
#      ServiceAccount (ValidatingAdmissionPolicy)
#   4. the worker container has no Kubernetes token
#
# Env:
#   KIND_K8S=1.30    one cluster from KIND_CLUSTERS in lib.sh (default: all)
#   KEEP_CLUSTER=1   leave the cluster running
#   SKIP_DEPS=1      do not run `helm dependency build`
# Without docker it is skipped, except with CI=true.
# shellcheck source=lib.sh
source "$(dirname "$0")/lib.sh"
cd "$ROOT"
if ! command -v docker >/dev/null 2>&1; then
	[[ "$CI" == true ]] && die "docker is required in CI"
	warn "docker not installed, skipping the kind smoke test"
	exit 0
fi
need go
need helm
need kubectl
need jq
need curl
mkdir -p "$BUILD_DIR"

CLUSTER=heraion-kind
NS=heraion
export KUBECONFIG="$BUILD_DIR/kind-kubeconfig"
KIND=""
cleanup() {
	[[ -n "${PF:-}" ]] && kill "$PF" 2>/dev/null
	if [[ -n "$KIND" && "${KEEP_CLUSTER:-}" != 1 ]]; then
		"$KIND" delete cluster --name "$CLUSTER" >/dev/null 2>&1 || true
	fi
}
trap cleanup EXIT

[[ "${SKIP_DEPS:-}" == 1 ]] || helm dependency build "$CHART_DIR" >/dev/null

log "images"
docker build -q -t heraion:kind --build-arg VERSION=kind . >/dev/null
docker build -q -t heraion-llm-stub:kind -f test/stubs/llm/Dockerfile . >/dev/null
ok "heraion:kind, heraion-llm-stub:kind"

values="$BUILD_DIR/kind-values.yaml"
cat >"$values" <<'EOF'
image: { registry: "", repository: heraion, tag: kind, pullPolicy: Never }
llm: { baseURL: http://llm-stub.heraion.svc:8080/v1, model: stub }
dryRun: true
mcpServers:
  k8s: { enabled: true, required: true }
gateway:
  hooks:
    smoke: { auth: token, secret: { value: smoke-secret } }
playbooks:
  smoke:
    trigger: { webhook: smoke }
    tools: ["k8s.*"]
    output: report
    prompt: List the pods in namespace heraion.
EOF

# One LLM answer per step: a k8s tool call, then the report.
script='[{"calls":[{"name":"k8s__pods_list_in_namespace","args":{"namespace":"heraion"}}]},{"final":"smoke ok"}]'

# The Job the gateway ServiceAccount tries: another image and ServiceAccount.
rogue='apiVersion: batch/v1
kind: Job
metadata: { name: rogue, labels: { heraion.io/playbook: smoke } }
spec:
  template:
    spec:
      restartPolicy: Never
      serviceAccountName: default
      containers: [{ name: worker, image: "busybox:1.36", command: [sh, -c, "cat /etc/hostname"] }]'

smoke() {
	local k8s=$1 kind_version node
	read -r kind_version node <<<"${KIND_CLUSTERS[$k8s]:?unknown KIND_K8S $k8s}"
	KIND=$(kind_bin "$kind_version")
	log "kind $kind_version, Kubernetes $k8s"
	"$KIND" delete cluster --name "$CLUSTER" >/dev/null 2>&1 || true
	"$KIND" create cluster --name "$CLUSTER" --image "$node" --wait 120s
	"$KIND" load docker-image heraion:kind heraion-llm-stub:kind --name "$CLUSTER"

	kubectl create namespace "$NS"
	kubectl -n "$NS" create configmap llm-stub --from-literal=script.json="$script"
	kubectl -n "$NS" apply -f - <<'EOF'
apiVersion: apps/v1
kind: Deployment
metadata: { name: llm-stub }
spec:
  selector: { matchLabels: { app: llm-stub } }
  template:
    metadata: { labels: { app: llm-stub } }
    spec:
      containers:
        - name: stub
          image: heraion-llm-stub:kind
          imagePullPolicy: Never
          env: [{ name: LLM_STUB_SCRIPT, value: /etc/stub/script.json }]
          ports: [{ containerPort: 8080 }]
          volumeMounts: [{ name: script, mountPath: /etc/stub }]
      volumes: [{ name: script, configMap: { name: llm-stub } }]
---
apiVersion: v1
kind: Service
metadata: { name: llm-stub }
spec:
  selector: { app: llm-stub }
  ports: [{ port: 8080, targetPort: 8080 }]
EOF
	kubectl -n "$NS" rollout status deploy/llm-stub --timeout 120s

	log "helm install and helm test"
	helm install heraion "$CHART_DIR" -n "$NS" -f "$values" --wait --timeout 5m
	helm test heraion -n "$NS" --timeout 5m --logs
	ok "helm test"

	log "webhook run"
	kubectl -n "$NS" port-forward svc/heraion-gateway 18080:8080 >/dev/null 2>&1 &
	PF=$!
	for _ in $(seq 30); do curl -sf http://127.0.0.1:18080/readyz >/dev/null && break; sleep 1; done
	local resp job logs
	resp=$(curl -sS -X POST -H "Authorization: Bearer smoke-secret" -d '{"alerts":[]}' http://127.0.0.1:18080/hooks/smoke)
	kill "$PF"
	PF=""
	job=$(jq -r '.jobs[0].job // empty' <<<"$resp")
	[[ -n "$job" ]] || die "no Job in the webhook response: $resp"
	if ! kubectl -n "$NS" wait --for=condition=complete "job/$job" --timeout 5m; then
		kubectl -n "$NS" logs "job/$job" --all-containers >&2 || true
		die "Job $job did not complete"
	fi
	logs=$(kubectl -n "$NS" logs "job/$job" -c worker)
	[[ -n $(jq -c 'select(.msg == "run finished" and .status == "ok")' <<<"$logs") ]] ||
		die "no successful run finished line:"$'\n'"$logs"
	[[ -n $(jq -c 'select(.msg == "tool call" and (.tool | startswith("k8s.")) and .error == false)' <<<"$logs") ]] ||
		die "no successful k8s tool call:"$'\n'"$logs"
	ok "run $job: status ok, k8s tool call"

	log "admission policy"
	local out
	if out=$(kubectl -n "$NS" --as="system:serviceaccount:$NS:heraion-gateway" create -f - <<<"$rogue" 2>&1); then
		die "the gateway ServiceAccount created a Job with another image and ServiceAccount"
	fi
	grep -q "ValidatingAdmissionPolicy" <<<"$out" || die "rogue Job refused, but not by the policy: $out"
	ok "rogue Job denied by the policy"

	log "token isolation"
	kubectl -n "$NS" get pod -l "job-name=$job" -o json | jq -e '.items[0].spec |
		.automountServiceAccountToken == false and
		([.containers[] | select(.name == "worker") | .volumeMounts[]?.mountPath |
			select(startswith("/var/run/secrets/kubernetes.io"))] | length == 0)' >/dev/null ||
		die "the worker container can read a Kubernetes token"
	ok "no token in the worker container"

	if [[ "${KEEP_CLUSTER:-}" != 1 ]]; then
		"$KIND" delete cluster --name "$CLUSTER"
	fi
}

if [[ -n "${KIND_K8S:-}" ]]; then
	smoke "$KIND_K8S"
else
	for k8s in "${!KIND_CLUSTERS[@]}"; do
		smoke "$k8s"
	done
fi
ok "kind"
