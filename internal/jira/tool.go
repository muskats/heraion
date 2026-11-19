package jira

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/muskats/heraion/internal/guardrails"
	"github.com/muskats/heraion/internal/llm"
	"github.com/muskats/heraion/internal/textutil"
)

// API is what the tool needs from a Jira client. *Client implements it.
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
	// Created records each call, for the run report.
	Created []Created
}

// Created is the outcome of one create_issue call.
type Created struct {
	Project, Key, URL, Summary string
	Duplicate, DryRun          bool
}

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

func (t *Tool) Schema() json.RawMessage {
	projects, _ := json.Marshal(t.Guard.Projects())
	return json.RawMessage(`{"type":"object","properties":{
		"project":{"type":"string","enum":` + string(projects) + `,"description":"Jira project key"},
		"summary":{"type":"string","description":"One-line title, e.g. 'payments-api OOMKilled 14 times in 1h'"},
		"description":{"type":"string","description":"Markdown: what happened, evidence, impact, suggested next steps"},
		"issueType":{"type":"string","description":"e.g. Bug, Task, Incident. Default: ` + t.IssueType + `"},
		"priority":{"type":"string","description":"e.g. High. Leave empty for the project default"},
		"labels":{"type":"array","items":{"type":"string"},"description":"Extra labels, no spaces"},
		"key":{"type":"string","description":"Stable key for this finding, e.g. 'oomkilled-payments-api'. Reusing a key de-duplicates."}},
		"required":["project","summary","description","key"]}`)
}

type args struct {
	Project, Summary, Description, IssueType, Priority, Key string
	Labels                                                  []string
}

func (t *Tool) Call(ctx context.Context, raw json.RawMessage) (string, error) {
	var a args
	if err := json.Unmarshal(raw, &a); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
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
		if l == "" || strings.ContainsAny(l, " \t\n") {
			return "", fmt.Errorf("label %q must not contain spaces", l)
		}
	}
	key := a.Key
	if key == "" {
		// The summary usually changes between runs, so reruns may not find
		// this issue again.
		t.Log.Warn("create_issue without key: de-duplicating on the summary", "summary", a.Summary)
		key = a.Summary
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

	if t.Guard.DryRun() {
		log.Info("dry-run: would create issue", "summary", in.Summary, "type", in.Type, "labels", in.Labels, "description", in.Description)
		t.Created = append(t.Created, Created{Project: a.Project, Summary: in.Summary, DryRun: true})
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
		t.Created = append(t.Created, Created{Project: a.Project, Key: existing.Key, URL: existing.URL, Summary: in.Summary, Duplicate: true})
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
	t.Created = append(t.Created, Created{Project: a.Project, Key: issue.Key, URL: issue.URL, Summary: in.Summary})
	return fmt.Sprintf("created %s: %s", issue.Key, issue.URL), nil
}

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
