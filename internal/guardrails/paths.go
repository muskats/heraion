package guardrails

import (
	"fmt"
	"path"
	"strings"

	"github.com/bmatcuk/doublestar/v4"
)

// DefaultDenyPaths are files a run may never write, whatever writePaths say:
//
//   - CI definitions, hooks and code ownership: a PR from a branch in the same
//     repository runs CI with the repository's secrets, e.g. .github/workflows/**
//   - package-manager and registry configs: they redirect dependency
//     resolution to another server, e.g. .npmrc, pip.conf, settings.xml
var DefaultDenyPaths = []string{
	".github/workflows/**", ".github/actions/**", "**/CODEOWNERS",
	".gitea/workflows/**", ".gitea/actions/**", ".forgejo/workflows/**",
	".gitlab-ci.yml", ".gitlab/**",
	"**/Jenkinsfile", ".circleci/**", ".travis.yml", ".drone.yml",
	".woodpecker.yml", ".woodpecker/**", ".buildkite/**",
	"azure-pipelines.yml", ".azure-pipelines/**", ".azuredevops/**",
	"bitbucket-pipelines.yml", ".gitmodules", "**/.pre-commit-config.yaml",
	"**/.npmrc", "**/.yarnrc", "**/.yarnrc.yml", "**/.pnpmfile.cjs",
	"**/pip.conf", "**/pip.ini", "**/.pypirc", "**/.mvn/**", "**/settings.xml",
	"**/nuget.config", "**/.cargo/config", "**/.cargo/config.toml",
	"**/.gemrc", "**/.bundle/config", "**/gradle.properties",
}

// OptInPaths are scanner ignore files: writing one can mute the scanner, so
// a playbook must name it in writePaths, e.g. ".trivyignore" or
// "**/.trivyignore". "**" or "deploy/**" never unlock them.
var OptInPaths = IgnoreFiles

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
	if matchAny(OptInPaths, p) {
		for _, pat := range g.o.AllowPaths {
			if explicitFor(pat, p) {
				return nil
			}
		}
		return fmt.Errorf("writing %s is not allowed: scanner ignore files are written only when writePaths names them, e.g. %q", p, "**/"+path.Base(p))
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

// explicitFor reports whether pattern matches p and names the file itself:
// its last segment has no wildcard, e.g. "**/.trivyignore" is explicit for
// "app/.trivyignore", "deploy/**" and "**/.trivy*" are not.
func explicitFor(pattern, p string) bool {
	last := pattern[strings.LastIndex(pattern, "/")+1:]
	return !strings.ContainsAny(last, "*?[{") && MatchPath(pattern, p)
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
