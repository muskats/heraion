#!/usr/bin/env bash
# Helm chart checks: lint, helm-unittest, the chart <-> binary cross-check,
# kubeconform against several Kubernetes versions, and values the schema or
# templates must reject.
#
# Env:
#   SKIP_DEPS=1      do not run `helm dependency build` (subcharts already in charts/)
#   KUBE_VERSIONS    space-separated, default "1.27.0 1.33.0"
#   JUNIT=1          also write build/junit-chart.xml (helm-unittest results)
#   SKIP_UNITTEST=1  skip helm-unittest locally (refused when CI=true)
source "$(dirname "$0")/lib.sh"
cd "$ROOT"
need helm
need go
mkdir -p "$BUILD_DIR"

base=(--set llm.baseURL=http://llm.example/v1 --set llm.model=m)

if [[ "${SKIP_DEPS:-}" == 1 ]]; then
	warn "helm dependency build skipped (SKIP_DEPS=1)"
else
	log "helm dependency build"
	helm dependency build "$CHART_DIR" >/dev/null
	ok "dependencies"
fi

log "helm lint"
helm lint "$CHART_DIR" "${base[@]}" --strict >/dev/null
for f in "$CHART_DIR"/ci/*-values.yaml; do
	helm lint "$CHART_DIR" -f "$f" --strict >/dev/null || die "helm lint -f $f"
done
ok "helm lint (defaults and ci/*-values.yaml)"

if [[ "${SKIP_UNITTEST:-}" == 1 ]]; then
	[[ "$CI" == true ]] && die "SKIP_UNITTEST is not allowed in CI"
	warn "helm-unittest skipped (SKIP_UNITTEST=1)"
else
	log "helm-unittest $HELM_UNITTEST_VERSION"
	unittest_args=()
	if [[ "${JUNIT:-}" == 1 ]]; then
		unittest_args+=(--output-type JUnit --output-file "$BUILD_DIR/junit-chart.xml")
	fi
	helm_unittest "${unittest_args[@]}" "$CHART_DIR"
	ok "helm-unittest"
fi

log "chart <-> binary cross-check"
# Renders the chart and loads the output with the binary's own config,
# playbook and Job template loaders.
go test -count=1 ./charts/heraion/
ok "cross-check"

log "kubeconform $KUBECONFORM_VERSION (Kubernetes $KUBE_VERSIONS)"
kc=$(kubeconform)
render_dir=$(mktemp -d)
trap 'rm -rf "$render_dir"' EXIT
helm template t "$CHART_DIR" "${base[@]}" >"$render_dir/defaults.yaml"
helm template t "$CHART_DIR" "${base[@]}" --set playbooks.trivy-remediate.enabled=true \
	--set mcpServers.k8s.enabled=true >"$render_dir/minimal.yaml"
for f in "$CHART_DIR"/ci/*-values.yaml "$ROOT"/examples/helm/values-*.yaml; do
	helm template t "$CHART_DIR" "${base[@]}" -f "$f" >"$render_dir/$(basename "$f")" || die "helm template -f $f"
done
# CRDs kubeconform has no built-in schema for, used in examples/helm (extraObjects).
crds=SecretProviderClass
for v in $KUBE_VERSIONS; do
	"$kc" -strict -summary -kubernetes-version "$v" -schema-location default -skip "$crds" "$render_dir"/*.yaml ||
		die "kubeconform failed for Kubernetes $v"
done
ok "kubeconform"

log "values that must be rejected"
reject() {
	local why=$1
	shift
	if helm template t "$CHART_DIR" "${base[@]}" "$@" >/dev/null 2>&1; then
		die "accepted, but should be rejected: $why ($*)"
	fi
	ok "rejects: $why"
}
reject "no llm.model" --set llm.model=
reject "unknown top-level key" --set notAKey=1
reject "unknown guardrail key" --set playbooks.trivy-remediate.enabled=true --set playbooks.trivy-remediate.guardrails.maxPR=1
reject "reopenAfter is not a duration" --set playbooks.trivy-remediate.enabled=true --set playbooks.trivy-remediate.guardrails.reopenAfter=week
reject "unknown mode" --set mode=reckless
reject "invalid namespace name" --set 'namespaces={Shop_1}'
reject "unknown playbook mode" --set playbooks.trivy-remediate.enabled=true --set playbooks.trivy-remediate.mode=reckless
reject "maxBodyBytes over the Secret limit" --set gateway.maxBodyBytes=2000000
reject "webhook playbook without a hook" --set playbooks.alertmanager-triage.enabled=true
reject "csiKey without secrets.csi" --set llm.apiKey.csiKey=llm-api-key --set playbooks.trivy-remediate.enabled=true
