package guardrails

import (
	"fmt"
	"path"
	"strings"
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

// WithPaths limits writes: allow (empty: everything) minus DefaultDenyPaths
// and deny. Patterns are slash-separated globs where "**" matches any number
// of directories, e.g. "deploy/**" or "**/values.yaml".
func (g *Guard) WithPaths(allow, deny []string) *Guard {
	g.allowPaths, g.denyPaths = allow, deny
	return g
}

// CheckWritePath fails unless the repo-relative path p may be written.
// Matching ignores case, since some checkouts are on case-insensitive disks
// and some servers (e.g. GitHub's CODEOWNERS lookup) are lenient.
func (g *Guard) CheckWritePath(p string) error {
	for _, pat := range append(DefaultDenyPaths[:len(DefaultDenyPaths):len(DefaultDenyPaths)], g.denyPaths...) {
		if MatchPath(pat, p) {
			return fmt.Errorf("writing %s is not allowed (matches %q): CI and ownership files are never changed", p, pat)
		}
	}
	if len(g.allowPaths) == 0 {
		return nil
	}
	for _, pat := range g.allowPaths {
		if MatchPath(pat, p) {
			return nil
		}
	}
	return fmt.Errorf("writing %s is not allowed; this playbook may only write %v", p, g.allowPaths)
}

// ValidatePathPattern reports a malformed pattern, e.g. "deploy/[".
func ValidatePathPattern(pat string) error {
	if strings.TrimSpace(pat) == "" {
		return fmt.Errorf("empty path pattern")
	}
	for _, seg := range strings.Split(pat, "/") {
		if _, err := path.Match(seg, ""); err != nil {
			return fmt.Errorf("path pattern %q: %w", pat, err)
		}
	}
	return nil
}

// MatchPath matches a slash-separated path against pattern, ignoring case.
// "**" matches zero or more segments; other segments use path.Match. A
// malformed segment never matches.
func MatchPath(pattern, name string) bool {
	return matchSegs(strings.Split(strings.ToLower(pattern), "/"), strings.Split(strings.ToLower(name), "/"))
}

func matchSegs(pat, name []string) bool {
	for len(pat) > 0 {
		if pat[0] == "**" {
			for i := 0; i <= len(name); i++ {
				if matchSegs(pat[1:], name[i:]) {
					return true
				}
			}
			return false
		}
		if len(name) == 0 {
			return false
		}
		if ok, err := path.Match(pat[0], name[0]); err != nil || !ok {
			return false
		}
		pat, name = pat[1:], name[1:]
	}
	return len(name) == 0
}
