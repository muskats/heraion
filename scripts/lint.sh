#!/usr/bin/env bash
# Static checks: formatting, go vet, module tidiness, golangci-lint,
# govulncheck, GitHub workflow lint and shellcheck.
#
#   scripts/lint.sh            all checks
#   SKIP_VULN=1 scripts/lint.sh  without govulncheck (it needs network access)
source "$(dirname "$0")/lib.sh"
cd "$ROOT"
need go

log "gofmt"
unformatted=$(gofmt -l $(git ls-files '*.go' 2>/dev/null || find . -name '*.go' -not -path './.tools/*'))
[[ -z "$unformatted" ]] || die "not gofmt'ed (run gofmt -w):"$'\n'"$unformatted"
ok "gofmt"

log "go vet"
go vet ./...
ok "go vet"

log "go mod tidy"
# Compare a tidied copy, so the check never changes the working tree.
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
cp go.mod go.sum "$tmp/"
go mod tidy -diff >"$tmp/tidy.diff" 2>&1 || die "go.mod/go.sum are not tidy (run go mod tidy):"$'\n'"$(cat "$tmp/tidy.diff")"
ok "go mod tidy"

log "golangci-lint $GOLANGCI_LINT_VERSION"
"$(golangci_lint)" run ./...
ok "golangci-lint"

if [[ "${SKIP_VULN:-}" == 1 ]]; then
	warn "govulncheck skipped (SKIP_VULN=1)"
else
	log "govulncheck $GOVULNCHECK_VERSION"
	"$(govulncheck)" ./...
	ok "govulncheck"
fi

log "actionlint $ACTIONLINT_VERSION"
# actionlint also runs shellcheck on run: blocks when shellcheck is installed.
"$(actionlint)" -no-color
ok "actionlint"

log "shellcheck"
if optional shellcheck; then
	shellcheck -x scripts/*.sh
	ok "shellcheck"
fi
