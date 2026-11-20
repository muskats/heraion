// Package playbook loads playbooks, e.g.
//
//	name: trivy-remediate
//	trigger: {schedule: "0 */6 * * *"}
//	tools: [k8s.*, repo_*, open_pr]
//	prompt: Fix fixable HIGH CVEs.
package playbook

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/robfig/cron/v3"

	"sigs.k8s.io/yaml"

	"github.com/muskats/heraion/internal/config"
	"github.com/muskats/heraion/internal/guardrails"
)

// Output kinds.
const (
	OutputPR     = "pr"     // the agent may open pull requests
	OutputReport = "report" // the agent only reports findings; open_pr is removed
)

// Playbook is one investigation definition.
type Playbook struct {
	Name        string   `json:"name"`
	Description string   `json:"description,omitempty"`
	Trigger     Trigger  `json:"trigger"`
	Tools       []string `json:"tools"`
	Repos       []string `json:"repos,omitempty"`
	// JiraProjects narrows jira.projects, e.g. [OPS]. Empty: all of them.
	JiraProjects []string `json:"jiraProjects,omitempty"`
	Prompt       string   `json:"prompt"`
	Output       string   `json:"output,omitempty"`
	// Mode is safe, full or yolo, see config.Mode. Empty: the global mode.
	// A riskier mode than the global one is narrowed to it.
	Mode       config.Mode `json:"mode,omitempty"`
	Guardrails Guardrails  `json:"guardrails,omitempty"`
}

// Trigger says how a playbook is started. Exactly one field must be set.
type Trigger struct {
	Schedule string `json:"schedule,omitempty"` // cron expression, rendered as a CronJob by the chart
	Webhook  string `json:"webhook,omitempty"`  // hook name, served at POST /hooks/{name}
	Manual   bool   `json:"manual,omitempty"`   // only started by `heraion run`
}

// Guardrails limit what a single run may do. Unset keys get the defaults of
// DefaultGuardrails; an explicit 0 is kept, e.g. maxIssues: 0 means none.
type Guardrails struct {
	MaxPRs       int   `json:"maxPRs"`
	MaxIssues    int   `json:"maxIssues"`
	DryRun       *bool `json:"dryRun,omitempty"`
	MaxFileBytes int   `json:"maxFileBytes"`
	// WritePaths limits repo_write_file to these globs, e.g. [Dockerfile,
	// "deploy/**"]. Empty: any path except the denied ones.
	WritePaths []string `json:"writePaths,omitempty"`
	// DenyPaths adds to guardrails.DefaultDenyPaths (CI files, CODEOWNERS).
	DenyPaths []string `json:"denyPaths,omitempty"`
	// ReopenAfter is how long a PR closed without merging stops a new PR on
	// the same branch. "0s": never reopen.
	ReopenAfter config.Duration `json:"reopenAfter"`
}

// DefaultGuardrails are used for keys a playbook does not set.
func DefaultGuardrails() Guardrails {
	return Guardrails{MaxPRs: 1, MaxIssues: 1, MaxFileBytes: 1 << 20,
		ReopenAfter: config.Duration{Duration: 7 * 24 * time.Hour}}
}

// Parse decodes and validates a single playbook.
func Parse(b []byte) (*Playbook, error) {
	// Decoding keeps fields the YAML does not mention, so prefilled defaults
	// apply only to unset keys.
	p := &Playbook{Guardrails: DefaultGuardrails()}
	if err := yaml.UnmarshalStrict(b, p); err != nil {
		return nil, err
	}
	if p.Output == "" {
		p.Output = OutputPR
	}
	return p, p.Validate()
}

// Validate reports every problem in the playbook at once.
func (p *Playbook) Validate() error {
	var errs []error
	if !config.DNSLabel(p.Name, 52) {
		errs = append(errs, fmt.Errorf("name %q must be a lowercase DNS label (max 52 chars)", p.Name))
	}
	if strings.TrimSpace(p.Prompt) == "" {
		errs = append(errs, errors.New("prompt is required"))
	}
	n := 0
	if p.Trigger.Schedule != "" {
		n++
		// The same parser the kube-apiserver uses for CronJob schedules.
		if _, err := cron.ParseStandard(p.Trigger.Schedule); err != nil {
			errs = append(errs, fmt.Errorf("trigger.schedule %q: %w", p.Trigger.Schedule, err))
		}
	}
	if p.Trigger.Webhook != "" {
		n++
		if !config.DNSLabel(p.Trigger.Webhook, 52) {
			errs = append(errs, fmt.Errorf("trigger.webhook %q must be a lowercase DNS label", p.Trigger.Webhook))
		}
	}
	if p.Trigger.Manual {
		n++
	}
	if n != 1 {
		errs = append(errs, errors.New("trigger must set exactly one of schedule, webhook or manual"))
	}
	if p.Output != OutputPR && p.Output != OutputReport {
		errs = append(errs, fmt.Errorf("output %q must be %q or %q", p.Output, OutputPR, OutputReport))
	}
	if p.Mode != "" {
		if err := config.ValidateMode("mode", p.Mode); err != nil {
			errs = append(errs, err)
		}
	}
	if len(p.Tools) == 0 {
		errs = append(errs, errors.New("tools allowlist must not be empty"))
	}
	for _, t := range p.Tools {
		if _, err := path.Match(t, ""); err != nil {
			errs = append(errs, fmt.Errorf("tools pattern %q: %w", t, err))
		}
	}
	for _, r := range p.Repos {
		if err := config.ValidateRepoName(r); err != nil {
			errs = append(errs, fmt.Errorf("repos: %w", err))
		}
	}
	if p.Guardrails.MaxPRs < 0 {
		errs = append(errs, errors.New("guardrails.maxPRs must be >= 0"))
	}
	if p.Guardrails.MaxIssues < 0 {
		errs = append(errs, errors.New("guardrails.maxIssues must be >= 0"))
	}
	if p.Guardrails.MaxFileBytes < 1 {
		errs = append(errs, errors.New("guardrails.maxFileBytes must be >= 1"))
	}
	if p.Guardrails.ReopenAfter.Duration < 0 {
		errs = append(errs, errors.New("guardrails.reopenAfter must not be negative"))
	}
	for _, pat := range append(slices.Clone(p.Guardrails.WritePaths), p.Guardrails.DenyPaths...) {
		if err := guardrails.ValidatePathPattern(pat); err != nil {
			errs = append(errs, fmt.Errorf("guardrails: %w", err))
		}
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("playbook %q: %w", p.Name, err)
	}
	return nil
}

// AllowsTool matches name against the tools list (path.Match syntax), e.g.
// "k8s.*" allows "k8s.pods_log". With output "report", open_pr is never allowed.
func (p *Playbook) AllowsTool(name string) bool {
	if p.Output == OutputReport && name == "open_pr" {
		return false
	}
	for _, pat := range p.Tools {
		if ok, _ := path.Match(pat, name); ok {
			return true
		}
	}
	return false
}

// EffectiveRepos intersects the playbook repos with the global list. No
// playbook repos means all global repos; no global repos means none.
func (p *Playbook) EffectiveRepos(global []string) []string {
	return intersect(p.Repos, global)
}

// EffectiveJiraProjects works like EffectiveRepos, for jira.projects.
func (p *Playbook) EffectiveJiraProjects(global []string) []string {
	return intersect(p.JiraProjects, global)
}

func intersect(local, global []string) []string {
	if len(local) == 0 {
		return slices.Clone(global)
	}
	var out []string
	for _, x := range local {
		if slices.Contains(global, x) {
			out = append(out, x)
		}
	}
	return out
}

// EffectiveMode is the playbook mode narrowed to the global one, e.g.
// playbook yolo with global full gives full. No playbook mode: global.
// An empty global mode (a Config not built by config.Load) counts as full.
func (p *Playbook) EffectiveMode(global config.Mode) config.Mode {
	if global == "" {
		global = config.ModeFull
	}
	if p.Mode == "" {
		return global
	}
	return config.Safer(p.Mode, global)
}

// DryRun is true if the global flag or the playbook asks for it.
func (p *Playbook) DryRun(global bool) bool {
	if global {
		return true
	}
	return p.Guardrails.DryRun != nil && *p.Guardrails.DryRun
}

// LoadDir loads every *.yaml / *.yml file in dir. Names must be unique.
func LoadDir(dir string) (map[string]*Playbook, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read playbooks dir: %w", err)
	}
	out := map[string]*Playbook{}
	var errs []error
	for _, e := range entries {
		ext := filepath.Ext(e.Name())
		// Skip hidden entries such as the ..data symlink in ConfigMap mounts.
		if e.IsDir() || strings.HasPrefix(e.Name(), ".") || (ext != ".yaml" && ext != ".yml") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			errs = append(errs, err)
			continue
		}
		p, err := Parse(b)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", e.Name(), err))
			continue
		}
		if _, dup := out[p.Name]; dup {
			errs = append(errs, fmt.Errorf("%s: duplicate playbook name %q", e.Name(), p.Name))
			continue
		}
		out[p.Name] = p
	}
	return out, errors.Join(errs...)
}
