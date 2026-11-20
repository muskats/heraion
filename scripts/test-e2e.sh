#!/usr/bin/env bash
# End-to-end: the heraion binary, run on the chart's rendered config, against
# a mock kube-apiserver, LLM and Gitea API and a real bare Git remote.
# Webhook -> Job and event Secret -> worker run -> pushed branch and PR.
#
# Env:
#   HERAION_BIN=path  test this binary instead of building one (e.g. a release build)
#   JUNIT=1           also write build/junit-e2e.xml (needs gotestsum on PATH)
#   GO_TEST_FLAGS     extra flags, e.g. "-run TestWebhookToPullRequest/gateway"
#   SKIP_DEPS=1       do not run `helm dependency build` (subcharts already in charts/)
# shellcheck source=lib.sh
source "$(dirname "$0")/lib.sh"
cd "$ROOT"
need go
need helm
need git
mkdir -p "$BUILD_DIR"

# The test renders the chart with `helm template`, which needs the subcharts.
[[ "${SKIP_DEPS:-}" == 1 ]] || helm dependency build "$CHART_DIR" >/dev/null

read -r -a extra <<<"${GO_TEST_FLAGS:-}"
args=(-tags e2e -count=1 -timeout 10m "${extra[@]}" ./test/e2e/)
log "e2e"
if [[ "${JUNIT:-}" == 1 ]] && optional gotestsum; then
	gotestsum --junitfile "$BUILD_DIR/junit-e2e.xml" --format testname -- "${args[@]}"
else
	go test -v "${args[@]}"
fi
ok "e2e"
