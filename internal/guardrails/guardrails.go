// Package guardrails limits what a run can do: allowed repos, PR budget,
// dry-run and secret masking. There is no code path that merges.
package guardrails

import (
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/muskats/heraion/internal/config"
	"github.com/muskats/heraion/internal/secret"
)

// Options configure a Guard. Zero values mean: no repos, no PRs, no issues,
// no file size limit, full mode.
type Options struct {
	Repos        []string
	MaxPRs       int
	DryRun       bool
	MaxFileBytes int       // 0: no limit
	Redactor     *Redactor // nil: a new one
	// AllowPaths limits writes, e.g. ["deploy/**"]. Empty: any path, or
	// SafePaths in safe mode. DenyPaths adds to DefaultDenyPaths.
	AllowPaths, DenyPaths []string
	Mode                  config.Mode // empty: full
	// JiraProjects create_issue may write to, e.g. [OPS], at most MaxIssues times.
	JiraProjects []string
	MaxIssues    int
}

// Guard enforces the limits of a single run. It is safe for concurrent use.
type Guard struct {
	o      Options
	prs    budget
	issues budget
}

// New creates a Guard, e.g. New(Options{Repos: []string{"acme/web"}, MaxPRs: 1}).
func New(o Options) *Guard {
	if o.Redactor == nil {
		o.Redactor = NewRedactor()
	}
	if o.Mode == "" {
		o.Mode = config.ModeFull
	}
	if o.Mode == config.ModeSafe && len(o.AllowPaths) == 0 {
		o.AllowPaths = SafePaths
	}
	return &Guard{
		o:      o,
		prs:    budget{max: o.MaxPRs, full: "PR budget exhausted: this playbook may open at most %d PR(s) per run"},
		issues: budget{max: o.MaxIssues, full: "issue budget exhausted: this playbook may create at most %d issue(s) per run"},
	}
}

// budget counts what a run creates, e.g. at most 1 PR.
type budget struct {
	mu   sync.Mutex
	used int
	max  int
	full string // error format, gets max
}

func (b *budget) reserve() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.used >= b.max {
		return fmt.Errorf(b.full, b.max)
	}
	b.used++
	return nil
}

func (b *budget) release() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.used > 0 {
		b.used--
	}
}

// ReservePR takes one PR from the budget. Call ReleasePR if no PR was opened.
func (g *Guard) ReservePR() error { return g.prs.reserve() }

// ReleasePR returns a slot taken by ReservePR.
func (g *Guard) ReleasePR() { g.prs.release() }

// ReserveIssue takes one issue from the budget. Call ReleaseIssue if none was created.
func (g *Guard) ReserveIssue() error { return g.issues.reserve() }

// ReleaseIssue returns a slot taken by ReserveIssue.
func (g *Guard) ReleaseIssue() { g.issues.release() }

// Projects returns the Jira projects issues may be created in.
func (g *Guard) Projects() []string { return slices.Clone(g.o.JiraProjects) }

// CheckProject fails unless project is allowlisted.
func (g *Guard) CheckProject(project string) error {
	if !slices.Contains(g.o.JiraProjects, project) {
		return fmt.Errorf("jira project %q is not allowed; allowed: %v", project, g.o.JiraProjects)
	}
	return nil
}

// DryRun reports whether changes must only be logged, not pushed.
func (g *Guard) DryRun() bool { return g.o.DryRun }

// Mode returns the run's mode.
func (g *Guard) Mode() config.Mode { return g.o.Mode }

// Repos returns the allowed repositories.
func (g *Guard) Repos() []string { return slices.Clone(g.o.Repos) }

// Redactor returns the run's redactor.
func (g *Guard) Redactor() *Redactor { return g.o.Redactor }

// CheckRepo fails unless repo is allowlisted.
func (g *Guard) CheckRepo(repo string) error {
	if !slices.Contains(g.o.Repos, repo) {
		return fmt.Errorf("repository %q is not in this playbook's allowlist %v", repo, g.o.Repos)
	}
	return nil
}

// CheckFileSize fails if a write exceeds the per-file limit.
func (g *Guard) CheckFileSize(n int) error {
	if g.o.MaxFileBytes > 0 && n > g.o.MaxFileBytes {
		return fmt.Errorf("file of %d bytes exceeds the %d byte limit", n, g.o.MaxFileBytes)
	}
	return nil
}

const mask = "[REDACTED]"

// Token shapes masked even when the value is unknown, e.g. "ghp_...",
// "glpat-...", "Bearer ...". repl is the replacement ($1 keeps a group).
var patterns = []struct {
	re   *regexp.Regexp
	repl string
}{
	{regexp.MustCompile(`\b(gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,})\b`), mask},
	{regexp.MustCompile(`\bglpat-[A-Za-z0-9_\-]{20,}\b`), mask},
	{regexp.MustCompile(`\bsk-(ant-|proj-)?[A-Za-z0-9_\-]{20,}\b`), mask},
	{regexp.MustCompile(`\bxox[abprs]-[A-Za-z0-9\-]{10,}\b`), mask},
	{regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`), mask},
	// Atlassian API tokens, as used for Jira and Confluence.
	{regexp.MustCompile(`\bATATT3[A-Za-z0-9_\-=]{20,}`), mask},
	// JWTs without "Bearer", e.g. a ServiceAccount token in a log line.
	{regexp.MustCompile(`\beyJ[A-Za-z0-9_\-]{10,}\.[A-Za-z0-9_\-]{10,}\.[A-Za-z0-9_\-]{10,}`), mask},
	{regexp.MustCompile(`(?i)\b(bearer|basic)\s+[A-Za-z0-9._~+/\-]{16,}=*`), mask},
	// user:password in a URL, e.g. postgres://app:hunter2@db -> postgres://[REDACTED]@db.
	{regexp.MustCompile(`(?i)\b([a-z][a-z0-9+.\-]*://)[^/\s:@]+:[^/\s@]+@`), "${1}" + mask + "@"},
	{regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]*?-----END [A-Z ]*PRIVATE KEY-----`), mask},
}

// Redactor masks known secret values and common credential patterns.
type Redactor struct {
	mu      sync.RWMutex
	values  []string
	sources []*secret.Source
}

// NewRedactor masks the given values. Values under 6 characters are ignored,
// so a password like "admin" does not mask every "admin" in a log.
func NewRedactor(secrets ...string) *Redactor {
	r := &Redactor{}
	r.Add(secrets...)
	return r
}

// AddSources masks the current value of each source. Values seen once stay
// masked, so a token that was just rotated out is still hidden.
func (r *Redactor) AddSources(srcs ...*secret.Source) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, s := range srcs {
		if s != nil {
			r.sources = append(r.sources, s)
		}
	}
}

// Add registers more secret values.
func (r *Redactor) Add(secrets ...string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	added := false
	for _, s := range secrets {
		s = strings.TrimSpace(s)
		if len(s) >= 6 && !slices.Contains(r.values, s) {
			r.values = append(r.values, s)
			added = true
		}
	}
	if added {
		// Longest first: with "abc123" and "abc123xyz", the longer one is masked whole.
		sort.Slice(r.values, func(i, j int) bool { return len(r.values[i]) > len(r.values[j]) })
	}
}

// Redact returns s with secrets masked.
func (r *Redactor) Redact(s string) string {
	r.mu.RLock()
	srcs := r.sources
	r.mu.RUnlock()
	for _, src := range srcs {
		if v := src.String(); v != "" {
			r.Add(v) // no-op when already known
		}
	}
	r.mu.RLock()
	for _, v := range r.values {
		s = strings.ReplaceAll(s, v, mask)
	}
	r.mu.RUnlock()
	for _, p := range patterns {
		s = p.re.ReplaceAllString(s, p.repl)
	}
	return s
}

// FindSecretLines returns the lines added by a unified diff that hold a
// secret, e.g. "+token: ghp_...", masked. Context and removed lines are
// ignored: they are already in the repository.
func (r *Redactor) FindSecretLines(patch string) []string {
	var out []string
	for _, l := range strings.Split(patch, "\n") {
		if !strings.HasPrefix(l, "+") || strings.HasPrefix(l, "+++ ") {
			continue
		}
		if m := r.Redact(l); m != l {
			out = append(out, m)
		}
	}
	return out
}
