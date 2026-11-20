package guardrails

import (
	"fmt"
	"strings"

	"github.com/bmatcuk/doublestar/v4"
)

// DefaultDenyPaths are files a run may never write: CI definitions and code
// ownership. A PR from a branch in the same repository runs CI with the
// repository's secrets, so a prompt-injected edit there could leak them.
var DefaultDenyPaths = []string{
	".github/workflows/**", ".github/actions/**", "**/CODEOWNERS",
	".gitea/workflows/**", ".gitea/actions/**", ".forgejo/workflows/**",
	".gitlab-ci.yml", ".gitlab/**",
	"**/Jenkinsfile", ".circleci/**", ".travis.yml", ".drone.yml",
	".woodpecker.yml", ".woodpecker/**", ".buildkite/**",
	"azure-pipelines.yml", ".azure-pipelines/**", ".azuredevops/**",
	"bitbucket-pipelines.yml", ".gitmodules",
}

// CheckWritePath fails unless the repo-relative path p may be written:
// Options.AllowPaths (empty: everything) minus DefaultDenyPaths and
// Options.DenyPaths. Patterns are slash-separated globs where "**" matches
// any number of directories, e.g. "deploy/**" or "**/values.yaml".
// Matching ignores case, since some checkouts are on case-insensitive disks
// and some servers (e.g. GitHub's CODEOWNERS lookup) are lenient.
func (g *Guard) CheckWritePath(p string) error {
	for _, pat := range DefaultDenyPaths {
		if MatchPath(pat, p) {
			return fmt.Errorf("writing %s is not allowed (matches %q): CI and ownership files are never changed", p, pat)
		}
	}
	for _, pat := range g.o.DenyPaths {
		if MatchPath(pat, p) {
			return fmt.Errorf("writing %s is not allowed: this playbook denies %q", p, pat)
		}
	}
	if len(g.o.AllowPaths) == 0 {
		return nil
	}
	for _, pat := range g.o.AllowPaths {
		if MatchPath(pat, p) {
			return nil
		}
	}
	return fmt.Errorf("writing %s is not allowed; this playbook may only write %v", p, g.o.AllowPaths)
}

// ValidatePathPattern reports a malformed pattern, e.g. "deploy/[".
func ValidatePathPattern(pat string) error {
	if strings.TrimSpace(pat) == "" {
		return fmt.Errorf("empty path pattern")
	}
	if !doublestar.ValidatePattern(pat) {
		return fmt.Errorf("path pattern %q is malformed", pat)
	}
	return nil
}

// MatchPath matches a slash-separated path against a doublestar pattern,
// ignoring case, e.g. "deploy/**" matches "deploy/a/values.yaml". A
// malformed pattern never matches.
func MatchPath(pattern, name string) bool {
	ok, err := doublestar.Match(strings.ToLower(pattern), strings.ToLower(name))
	return err == nil && ok
}
