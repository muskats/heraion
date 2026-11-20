#!/usr/bin/env bash
# Downloads the chart's subcharts into charts/heraion/charts/. Adds the HTTP
# repositories from Chart.yaml first, since a fresh machine (e.g. a CI runner)
# has none and `helm dependency build` refuses unknown ones:
#
#   repository: https://grafana-community.github.io/helm-charts
#   -> helm repo add heraion-dep-1 https://grafana-community.github.io/helm-charts
#
# OCI repositories (oci://...) need no repo entry.
# shellcheck source=lib.sh
source "$(dirname "$0")/lib.sh"
cd "$ROOT"
need helm

log "helm dependency build"
n=0
while read -r url; do
	n=$((n + 1))
	helm repo add --force-update "heraion-dep-$n" "$url" >/dev/null
done < <(sed -nE 's/^[[:space:]]*repository:[[:space:]]*"?(https?:\/\/[^"[:space:]]+)"?.*/\1/p' "$CHART_DIR/Chart.yaml" | sort -u)
helm dependency build "$CHART_DIR" >/dev/null
ok "dependencies"
