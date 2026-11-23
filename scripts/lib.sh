#!/usr/bin/env bash
# Shared helpers for the scripts in this directory. Source it, do not run it.
#
# Tools are pinned here and built with `go install` into $TOOLS_DIR (default
# .tools/), one directory per version, so a CI cache of .tools/ stays valid
# until a version changes. Override a tool with its env var, e.g.
# KUBECONFORM=/usr/local/bin/kubeconform. helm-unittest is a Helm plugin, see
# helm_unittest.

# Variables set here are used by the scripts that source this file.
# shellcheck disable=SC2034
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TOOLS_DIR="${TOOLS_DIR:-$ROOT/.tools}"
BUILD_DIR="${BUILD_DIR:-$ROOT/build}"
CHART_DIR="$ROOT/charts/heraion"
# CI=true (set by GitHub Actions, GitLab CI and most others) makes optional
# tools such as shellcheck required.
CI="${CI:-false}"

GOLANGCI_LINT_VERSION=v2.15.0
GOVULNCHECK_VERSION=v1.9.0
KUBECONFORM_VERSION=v0.8.0
HELM_UNITTEST_VERSION=v1.2.1
ACTIONLINT_VERSION=v1.7.12
# Kubernetes versions kubeconform checks the rendered chart against: the
# oldest supported (Chart.yaml kubeVersion) and a current one.
KUBE_VERSIONS="${KUBE_VERSIONS:-1.30.0 1.35.0}"
# kind clusters of scripts/test-kind.sh, by Kubernetes minor: the oldest
# supported (Chart.yaml kubeVersion) and the newest kind ships. Each kind
# release builds its own node images, so both are pinned together.
declare -A KIND_CLUSTERS=(
	[1.30]="v0.29.0 kindest/node:v1.30.13@sha256:397209b3d947d154f6641f2d0ce8d473732bd91c87d9575ade99049aa33cd648"
	[1.37]="v0.33.0 kindest/node:v1.37.0@sha256:a1ed56cfb0e7b93589bdf97c8cd566405a265939e3620fc4f5de89adff580ae5"
)

if [[ -t 1 ]]; then
	_bold=$'\e[1m' _red=$'\e[31m' _green=$'\e[32m' _yellow=$'\e[33m' _reset=$'\e[0m'
else
	_bold='' _red='' _green='' _yellow='' _reset=''
fi

log() { printf '%s==> %s%s\n' "$_bold" "$*" "$_reset"; }
ok() { printf '%sok%s   %s\n' "$_green" "$_reset" "$*"; }
warn() { printf '%swarn%s %s\n' "$_yellow" "$_reset" "$*" >&2; }
die() {
	printf '%sFAIL%s %s\n' "$_red" "$_reset" "$*" >&2
	exit 1
}

need() {
	command -v "$1" >/dev/null 2>&1 || die "$1 is required but not installed"
}

# go_tool NAME MODULE/PATH VERSION prints the path of the tool, building it
# first if needed. $<NAME in upper case, '-' as '_'> overrides it.
go_tool() {
	local name=$1 pkg=$2 version=$3
	local override_var
	override_var=$(tr 'a-z-' 'A-Z_' <<<"$name")
	if [[ -n "${!override_var:-}" ]]; then
		printf '%s\n' "${!override_var}"
		return
	fi
	local dir="$TOOLS_DIR/$name@$version"
	if [[ ! -x "$dir/$name" ]]; then
		need go
		printf 'installing %s %s\n' "$name" "$version" >&2
		GOBIN="$dir" GOFLAGS=-mod=mod go install "$pkg@$version" >&2
	fi
	printf '%s\n' "$dir/$name"
}

golangci_lint() { go_tool golangci-lint github.com/golangci/golangci-lint/v2/cmd/golangci-lint "$GOLANGCI_LINT_VERSION"; }
govulncheck() { go_tool govulncheck golang.org/x/vuln/cmd/govulncheck "$GOVULNCHECK_VERSION"; }
kubeconform() { go_tool kubeconform github.com/yannh/kubeconform/cmd/kubeconform "$KUBECONFORM_VERSION"; }
# helm_unittest ARGS... runs `helm unittest`. The plugin is installed into
# $TOOLS_DIR, not ~/.local (its go.mod has replace directives, so `go install`
# cannot build it). $HELM_UNITTEST=/path/to/helm-unittest overrides it.
helm_unittest() {
	if [[ -n "${HELM_UNITTEST:-}" ]]; then
		"$HELM_UNITTEST" "$@"
		return
	fi
	local dir="$TOOLS_DIR/helm-plugins@$HELM_UNITTEST_VERSION"
	if ! HELM_PLUGINS="$dir" helm plugin list | grep -q '^unittest'; then
		printf 'installing helm-unittest %s\n' "$HELM_UNITTEST_VERSION" >&2
		mkdir -p "$dir"
		# --verify=false: the plugin publishes no provenance files.
		HELM_PLUGINS="$dir" helm plugin install https://github.com/helm-unittest/helm-unittest \
			--version "$HELM_UNITTEST_VERSION" --verify=false >&2
	fi
	HELM_PLUGINS="$dir" helm unittest "$@"
}
# kind VERSION prints the path of kind at that version, e.g. kind v0.33.0.
kind_bin() { go_tool kind sigs.k8s.io/kind "$1"; }
actionlint() { go_tool actionlint github.com/rhysd/actionlint/cmd/actionlint "$ACTIONLINT_VERSION"; }

# optional NAME: true if the tool exists. Missing tools fail in CI and are
# skipped with a warning elsewhere.
optional() {
	if command -v "$1" >/dev/null 2>&1; then
		return 0
	fi
	[[ "$CI" == true ]] && die "$1 is required in CI"
	warn "$1 not installed, skipping its checks"
	return 1
}

# Go packages with tests, without the chart cross-check (test-chart.sh runs it).
go_packages() { go list ./... | awk '!/\/charts\//'; }
