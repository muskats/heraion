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

	"github.com/muskats/heraion/internal/secret"
)

// Guard enforces the limits of a single run. It is safe for concurrent use.
type Guard struct {
	repos        []string
	maxPRs       int
	dryRun       bool
	maxFileBytes int
	redactor     *Redactor

	projects  []string
	maxIssues int

	mu     sync.Mutex
	prs    int
	issues int
}

// New creates a Guard.
func New(repos []string, maxPRs int, dryRun bool, maxFileBytes int, r *Redactor) *Guard {
	if r == nil {
		r = NewRedactor()
	}
	return &Guard{repos: repos, maxPRs: maxPRs, dryRun: dryRun, maxFileBytes: maxFileBytes, redactor: r}
}

// WithIssues allows creating up to max issues in the given Jira projects,
// e.g. WithIssues([]string{"OPS"}, 1).
func (g *Guard) WithIssues(projects []string, max int) *Guard {
	g.projects, g.maxIssues = projects, max
	return g
}

// Projects returns the Jira projects issues may be created in.
func (g *Guard) Projects() []string { return slices.Clone(g.projects) }

// CheckProject fails unless project is allowlisted.
func (g *Guard) CheckProject(project string) error {
	if !slices.Contains(g.projects, project) {
		return fmt.Errorf("jira project %q is not allowed; allowed: %v", project, g.projects)
	}
	return nil
}

// ReserveIssue takes one issue from the budget. Call ReleaseIssue if none was created.
func (g *Guard) ReserveIssue() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.issues >= g.maxIssues {
		return fmt.Errorf("issue budget exhausted: this playbook may create at most %d issue(s) per run", g.maxIssues)
	}
	g.issues++
	return nil
}

// ReleaseIssue returns a slot taken by ReserveIssue.
func (g *Guard) ReleaseIssue() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.issues > 0 {
		g.issues--
	}
}

// DryRun reports whether changes must only be logged, not pushed.
func (g *Guard) DryRun() bool { return g.dryRun }

// Repos returns the allowed repositories.
func (g *Guard) Repos() []string { return slices.Clone(g.repos) }

// Redactor returns the run's redactor.
func (g *Guard) Redactor() *Redactor { return g.redactor }

// CheckRepo fails unless repo is allowlisted.
func (g *Guard) CheckRepo(repo string) error {
	if !slices.Contains(g.repos, repo) {
		return fmt.Errorf("repository %q is not in this playbook's allowlist %v", repo, g.repos)
	}
	return nil
}

// CheckFileSize fails if a write exceeds the per-file limit.
func (g *Guard) CheckFileSize(n int) error {
	if g.maxFileBytes > 0 && n > g.maxFileBytes {
		return fmt.Errorf("file of %d bytes exceeds the %d byte limit", n, g.maxFileBytes)
	}
	return nil
}

// ReservePR takes one PR from the budget. Call ReleasePR if no PR was opened.
func (g *Guard) ReservePR() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.prs >= g.maxPRs {
		return fmt.Errorf("PR budget exhausted: this playbook may open at most %d PR(s) per run", g.maxPRs)
	}
	g.prs++
	return nil
}

// ReleasePR returns a slot taken by ReservePR.
func (g *Guard) ReleasePR() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.prs > 0 {
		g.prs--
	}
}

const mask = "[REDACTED]"

// Token shapes masked even when the value is unknown, e.g. "ghp_...",
// "glpat-...", "Bearer ...".
var patterns = []*regexp.Regexp{
	regexp.MustCompile(`\b(gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,})\b`),
	regexp.MustCompile(`\bglpat-[A-Za-z0-9_\-]{20,}\b`),
	regexp.MustCompile(`\bsk-(ant-|proj-)?[A-Za-z0-9_\-]{20,}\b`),
	regexp.MustCompile(`\bxox[abprs]-[A-Za-z0-9\-]{10,}\b`),
	regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`),
	regexp.MustCompile(`(?i)\b(bearer|basic)\s+[A-Za-z0-9._~+/\-]{16,}=*`),
	regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]*?-----END [A-Z ]*PRIVATE KEY-----`),
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
	for _, s := range secrets {
		s = strings.TrimSpace(s)
		if len(s) >= 6 && !slices.Contains(r.values, s) {
			r.values = append(r.values, s)
		}
	}
	// Longest first: with "abc123" and "abc123xyz", the longer one is masked whole.
	sort.Slice(r.values, func(i, j int) bool { return len(r.values[i]) > len(r.values[j]) })
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
		s = p.ReplaceAllString(s, mask)
	}
	return s
}
