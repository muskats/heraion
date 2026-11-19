#!/usr/bin/env bash
# Runs the CI stages and prints a summary. Each stage is also a script:
#
#   scripts/ci.sh                  lint unit chart e2e
#   scripts/ci.sh unit chart       some stages
#   KEEP_GOING=1 scripts/ci.sh     run every stage even after a failure
#
#   lint   scripts/lint.sh        gofmt, go vet, go mod tidy, golangci-lint, govulncheck, actionlint, shellcheck
#   unit   scripts/test-unit.sh   go test -race with a coverage floor
#   chart  scripts/test-chart.sh  helm lint, helm-unittest, cross-check, kubeconform, rejected values
#   e2e    scripts/test-e2e.sh    binary + mock cluster, LLM and Gitea
source "$(dirname "$0")/lib.sh"

stages=("$@")
[[ ${#stages[@]} -gt 0 ]] || stages=(lint unit chart e2e)
declare -A script=([lint]=lint.sh [unit]=test-unit.sh [chart]=test-chart.sh [e2e]=test-e2e.sh)

results=() failed=0
for s in "${stages[@]}"; do
	[[ -n "${script[$s]:-}" ]] || die "unknown stage $s (want: ${!script[*]})"
	start=$SECONDS
	if bash "$ROOT/scripts/${script[$s]}"; then
		results+=("$(printf '%sok%s   %-6s %3ds' "$_green" "$_reset" "$s" $((SECONDS - start)))")
	else
		results+=("$(printf '%sFAIL%s %-6s %3ds' "$_red" "$_reset" "$s" $((SECONDS - start)))")
		failed=1
		[[ "${KEEP_GOING:-}" == 1 ]] || break
	fi
done

printf '\n%sSummary%s\n' "$_bold" "$_reset"
printf '  %s\n' "${results[@]}"
exit "$failed"
