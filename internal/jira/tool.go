package jira

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"

	"github.com/muskats/heraion/internal/config"
	"github.com/muskats/heraion/internal/guardrails"
	"github.com/muskats/heraion/internal/llm"
	"github.com/muskats/heraion/internal/textutil"
)

// API is what the tool needs from Jira. MCP implements it.
type API interface {
	Create(ctx context.Context, in NewIssue) (*Issue, error)
	FindOpen(ctx context.Context, jql string) (*Issue, error)
}

// Tool is the built-in create_issue tool.
type Tool struct {
	API       API // may be nil in dry-run
	Playbook  string
	IssueType string   // default type, e.g. "Task"
	Labels    []string // added to every issue, e.g. [heraion]
	Guard     *guardrails.Guard
	Log       *slog.Logger
	// EventID identifies the webhook event of the run (HERAION_EVENT_ID).
	// When set, the fingerprint comes from it and the model's key is ignored.
	EventID string
	// Created records each call, for the run report.
	Created []Created
}

// Created is the outcome of one create_issue call.
type Created struct {
	Project, Key, URL, Summary string
	Fingerprint                string // e.g. heraion-1b2c3d4e5f60
	Outcome                    Outcome
}

// Outcome is what one create_issue call did.
type Outcome string

// Outcomes.
const (
	OutcomeCreated   Outcome = "created"
	OutcomeDuplicate Outcome = "duplicate" // an open issue with the same fingerprint exists
	OutcomeDryRun    Outcome = "dry-run"   // logged, not created
)

// Fingerprint is the label that identifies the same finding across runs, e.g.
// Fingerprint("trivy-remediate", "OPS", "CVE-2024-1234 in web") = "heraion-1b2c3d4e5f60".
func Fingerprint(playbook, project, key string) string {
	k := strings.Join(strings.Fields(strings.ToLower(key)), " ")
	sum := sha256.Sum256([]byte(playbook + "\x00" + project + "\x00" + k))
	return "heraion-" + hex.EncodeToString(sum[:6])
}

func (t *Tool) Name() string { return "create_issue" }

func (t *Tool) Description() string {
	return "Create a Jira issue for a finding that needs a human, e.g. a fix that cannot be done " +
		"as a pull request. If an open issue with the same key exists, it is returned instead."
}

type args struct {
	Project     string   `json:"project" jsonschema:"Jira project key"`
	Summary     string   `json:"summary" jsonschema:"One-line title, e.g. 'payments-api OOMKilled 14 times in 1h'"`
	Description string   `json:"description" jsonschema:"Markdown: what happened, evidence, impact, suggested next steps"`
	IssueType   string   `json:"issueType,omitempty" jsonschema:"e.g. Bug, Task, Incident. Default: the configured type"`
	Priority    string   `json:"priority,omitempty" jsonschema:"e.g. High. Leave empty for the project default"`
	Labels      []string `json:"labels,omitempty" jsonschema:"Extra labels, no spaces"`
	Key         string   `json:"key" jsonschema:"Stable key for this finding: <kind>-<namespace>-<workload>, lowercase, e.g. 'oomkilled-payments-api'. Reusing a key de-duplicates."`
}

func (t *Tool) Schema() json.RawMessage {
	return json.RawMessage(llm.SchemaFor[args](map[string][]string{"project": t.Guard.Projects()}))
}

func (t *Tool) Call(ctx context.Context, raw json.RawMessage) (string, error) {
	var a args
	if err := llm.DecodeArgs(raw, &a); err != nil {
		return "", err
	}
	red := t.Guard.Redactor()
	// Redact before the length cut, so a secret at the cut is masked whole.
	a.Summary = red.Redact(strings.TrimSpace(strings.ReplaceAll(a.Summary, "\n", " ")))
	if a.Summary == "" {
		return "", errors.New("summary is required")
	}
	if len(a.Summary) > 255 { // Jira's summary limit
		a.Summary = textutil.Truncate(a.Summary, 252) + "..."
	}
	if err := t.Guard.CheckProject(a.Project); err != nil {
		return "", err
	}
	for _, l := range a.Labels {
		if !config.ValidLabel(l) {
			return "", fmt.Errorf("label %q must not contain spaces", l)
		}
	}
	// A webhook run is about one event, so the event is the finding. A
	// scheduled run needs the model's key, in a shape that stays stable.
	key := "event:" + t.EventID
	if t.EventID == "" {
		if !keyRE.MatchString(a.Key) {
			return "", fmt.Errorf("key %q must match %s, e.g. 'oomkilled-payments-api' (<kind>-<namespace>-<workload>)", a.Key, keyRE)
		}
		key = a.Key
	}
	fp := Fingerprint(t.Playbook, a.Project, key)
	in := NewIssue{
		Project:     a.Project,
		Type:        firstNonEmpty(a.IssueType, t.IssueType, "Task"),
		Summary:     a.Summary,
		Description: red.Redact(a.Description) + "\n\nCreated by HeraIon playbook " + t.Playbook + ".",
		Priority:    a.Priority,
		Labels:      dedupe(append(append(append([]string{}, t.Labels...), a.Labels...), fp, "heraion-"+t.Playbook)),
	}
	log := t.Log.With("project", a.Project, "fingerprint", fp)

	// Jira's search index lags, so an issue created earlier in this run may
	// not be found by the search below yet.
	for _, c := range t.Created {
		if c.Fingerprint == fp {
			return fmt.Sprintf("this finding was already handled in this run (%s %s); not created again", c.Outcome, c.Key), nil
		}
	}
	if t.Guard.DryRun() {
		// The same budget as a real run, so a dry run logs what it would create.
		if err := t.Guard.ReserveIssue(); err != nil {
			return "", err
		}
		log.Info("dry-run: would create issue", "summary", in.Summary, "type", in.Type, "labels", in.Labels, "description", in.Description)
		t.Created = append(t.Created, Created{Project: a.Project, Summary: in.Summary, Fingerprint: fp, Outcome: OutcomeDryRun})
		return fmt.Sprintf("dry-run: issue %q in %s was logged, not created", in.Summary, a.Project), nil
	}
	if t.API == nil {
		return "", ErrNotConfigured
	}
	// Labels are exact-match in JQL, so this finds the same finding only.
	jql := fmt.Sprintf(`project = "%s" AND labels = "%s" AND statusCategory != Done ORDER BY created DESC`, a.Project, fp)
	existing, err := t.API.FindOpen(ctx, jql)
	if err != nil {
		return "", fmt.Errorf("search for an existing issue: %w", err)
	}
	if existing != nil {
		log.Info("issue already open, skipping", "key", existing.Key)
		t.Created = append(t.Created, Created{Project: a.Project, Key: existing.Key, URL: existing.URL, Summary: in.Summary, Fingerprint: fp, Outcome: OutcomeDuplicate})
		return fmt.Sprintf("an open issue for this finding already exists: %s %s (not created again)", existing.Key, existing.URL), nil
	}
	if err := t.Guard.ReserveIssue(); err != nil {
		return "", err
	}
	issue, err := t.API.Create(ctx, in)
	if err != nil {
		t.Guard.ReleaseIssue()
		return "", err
	}
	log.Info("created issue", "key", issue.Key, "url", issue.URL)
	t.Created = append(t.Created, Created{Project: a.Project, Key: issue.Key, URL: issue.URL, Summary: in.Summary, Fingerprint: fp, Outcome: OutcomeCreated})
	return fmt.Sprintf("created %s: %s", issue.Key, issue.URL), nil
}

// keyRE is the shape of a scheduled run's key, e.g. "oomkilled-shop-payments-api".
var keyRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{2,59}$`)

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// Ensure the tool satisfies llm.Tool.
var _ llm.Tool = (*Tool)(nil)
