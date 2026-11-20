#!/usr/bin/env bash
# Go unit tests with the race detector and coverage.
#
#   scripts/test-unit.sh                 all packages
#   scripts/test-unit.sh ./internal/scm  some packages
#
# Env:
#   COVERAGE_MIN=80   fail when total statement coverage is lower (0 = off)
#   JUNIT=1           also write build/junit-unit.xml (needs gotestsum on PATH)
#   GO_TEST_FLAGS     extra flags, e.g. "-count=1 -run TestOpenPR"
#
# Writes build/coverage.out and build/coverage.txt (per-function summary);
# open build/coverage.html for a line view.
# shellcheck source=lib.sh
source "$(dirname "$0")/lib.sh"
cd "$ROOT"
need go
mkdir -p "$BUILD_DIR"

COVERAGE_MIN="${COVERAGE_MIN:-80}"
read -r -a pkgs <<<"${*:-$(go_packages | tr '\n' ' ')}"
read -r -a extra <<<"${GO_TEST_FLAGS:-}"
# Coverage of every package, including code only exercised by other packages' tests.
args=(-race -covermode=atomic "-coverpkg=./internal/...,./cmd/..." -coverprofile="$BUILD_DIR/coverage.out" "${extra[@]}" "${pkgs[@]}")

log "go test ${pkgs[*]}"
if [[ "${JUNIT:-}" == 1 ]] && optional gotestsum; then
	gotestsum --junitfile "$BUILD_DIR/junit-unit.xml" --format testname -- "${args[@]}"
else
	go test "${args[@]}"
fi

go tool cover -func="$BUILD_DIR/coverage.out" >"$BUILD_DIR/coverage.txt"
go tool cover -html="$BUILD_DIR/coverage.out" -o "$BUILD_DIR/coverage.html"
total=$(awk '/^total:/ {sub("%", "", $3); print $3}' "$BUILD_DIR/coverage.txt")
if [[ "$COVERAGE_MIN" != 0 ]] && awk -v t="$total" -v m="$COVERAGE_MIN" 'BEGIN { exit !(t < m) }'; then
	die "coverage ${total}% is below COVERAGE_MIN=${COVERAGE_MIN}% (see build/coverage.html)"
fi
ok "unit tests, coverage ${total}% (min ${COVERAGE_MIN}%)"
