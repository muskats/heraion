#!/usr/bin/env bash
# Checks that Chart.yaml's version and appVersion are equal, and with a tag
# that they match it. Chart.yaml is the single source of the release version.
#
#   scripts/check-version.sh          version == appVersion, e.g. 2.0.0
#   scripts/check-version.sh v2.0.0   also == the tag without "v"
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
