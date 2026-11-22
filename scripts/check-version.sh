#!/usr/bin/env bash
# Checks the versions that must agree:
#
#   - Chart.yaml version == appVersion, e.g. 2.0.0 (the release version)
#   - with a tag argument, also == the tag without "v"
#   - Dockerfile ARG GO_VERSION == the toolchain line in go.mod, e.g. 1.26.11
#
#   scripts/check-version.sh          versions only
#   scripts/check-version.sh v2.0.0   also the tag
# shellcheck source=lib.sh
source "$(dirname "$0")/lib.sh"

field() { awk -v k="$1" 'index($0, k ":") == 1 { gsub(/"/, "", $2); print $2 }' "$CHART_DIR/Chart.yaml"; }
version=$(field version)
app=$(field appVersion)

[[ -n "$version" && "$version" == "$app" ]] ||
	die "Chart.yaml: version ($version) and appVersion ($app) must be equal"
if [[ $# -gt 0 ]]; then
	[[ "${1#v}" == "$version" ]] ||
		die "tag $1 does not match Chart.yaml version $version: set both to ${1#v} and tag again"
fi
ok "version $version"

toolchain=$(awk '$1 == "toolchain" { sub(/^go/, "", $2); print $2 }' "$ROOT/go.mod")
docker_go=$(awk -F= '$1 == "ARG GO_VERSION" { print $2 }' "$ROOT/Dockerfile")
[[ -n "$toolchain" ]] || die "go.mod has no toolchain line, e.g. toolchain go1.26.11"
[[ "$toolchain" == "$docker_go" ]] ||
	die "Dockerfile GO_VERSION ($docker_go) must equal the go.mod toolchain ($toolchain)"
ok "go $toolchain"
