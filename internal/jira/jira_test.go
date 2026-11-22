package jira_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/muskats/heraion/internal/config"
	"github.com/muskats/heraion/internal/guardrails"
	"github.com/muskats/heraion/internal/jira"
	"github.com/muskats/heraion/internal/llm"
	"github.com/muskats/heraion/internal/mcp"
)

// Argument shapes of mcp-atlassian v0.24.7.
type searchArgs struct {
	JQL    string `json:"jql"`
	Limit  int    `json:"limit,omitempty"`
	Fields string `json:"fields,omitempty"`
}

type createArgs struct {
	ProjectKey       string `json:"project_key"`
	Summary          string `json:"summary"`
	IssueType        string `json:"issue_type"`
	Description      string `json:"description,omitempty"`
	AdditionalFields string `json:"additional_fields,omitempty"`
}

// fakeAtlassian answers like mcp-atlassian: open issues by label, and creates.
type fakeAtlassian struct {
	mu       sync.Mutex
	created  []createArgs
	searches []string
	byLabel  map[string]string // label -> issue key
}

var labelRE = regexp.MustCompile(`labels = "([^"]+)"`)

func text(v any) *sdk.CallToolResult {
	b, _ := json.Marshal(v)
	return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: string(b)}}}
}

func (f *fakeAtlassian) start(t *testing.T) []llm.Tool {
	t.Helper()
	s := sdk.NewServer(&sdk.Implementation{Name: "mcp-atlassian", Version: "0.24.7"}, nil)
	sdk.AddTool(s, &sdk.Tool{Name: jira.SearchTool},
		func(_ context.Context, _ *sdk.CallToolRequest, in searchArgs) (*sdk.CallToolResult, any, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.searches = append(f.searches, in.JQL)
			issues := []map[string]string{}
			if m := labelRE.FindStringSubmatch(in.JQL); m != nil && f.byLabel[m[1]] != "" {
				issues = append(issues, map[string]string{"key": f.byLabel[m[1]], "url": "https://jira/browse/" + f.byLabel[m[1]]})
			}
			return text(map[string]any{"total": len(issues), "issues": issues}), nil, nil
		})
	sdk.AddTool(s, &sdk.Tool{Name: jira.CreateTool},
		func(_ context.Context, _ *sdk.CallToolRequest, in createArgs) (*sdk.CallToolResult, any, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.created = append(f.created, in)
			key := fmt.Sprintf("%s-%d", in.ProjectKey, len(f.created))
			var extra struct{ Labels []string }
			_ = json.Unmarshal([]byte(in.AdditionalFields), &extra)
			for _, l := range extra.Labels {
				f.byLabel[l] = key
			}
			return text(map[string]any{"message": "Issue created successfully",
				"issue": map[string]string{"key": key, "url": "https://jira/browse/" + key}}), nil, nil
		})
	ts := httptest.NewServer(sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return s }, nil))
	t.Cleanup(ts.Close)
	servers := map[string]*config.MCPServer{"atlassian": {URL: ts.URL, Transport: "streamable",
		Timeout: config.Duration{Duration: 5 * time.Second}}}
	mgr, tools, err := mcp.Connect(context.Background(), servers, []string{"atlassian.*"}, "test", quiet())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mgr.Close)
	return tools
}

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func find(tools []llm.Tool, name string) llm.Tool {
	for _, t := range tools {
		if t.Name() == name {
			return t
		}
	}
	return nil
}

func newTool(t *testing.T, dryRun bool, max int) (*jira.Tool, *fakeAtlassian) {
	f := &fakeAtlassian{byLabel: map[string]string{}}
	tools := f.start(t)
	g := guardrails.New(guardrails.Options{DryRun: dryRun, Redactor: guardrails.NewRedactor("hunter2-secret"),
		JiraProjects: []string{"OPS"}, MaxIssues: max})
	api := jira.MCP{Searcher: find(tools, "atlassian.jira_search"), Creator: find(tools, "atlassian.jira_create_issue")}
	return &jira.Tool{API: api, Playbook: "triage", IssueType: "Task", Labels: []string{"heraion"}, Guard: g, Log: quiet()}, f
}

func call(t *testing.T, tl *jira.Tool, a map[string]any) (string, error) {
	b, _ := json.Marshal(a)
	return tl.Call(context.Background(), b)
}

func TestCreateDeduplicatesAndBudgets(t *testing.T) {
	tl, f := newTool(t, false, 2)
	a := map[string]any{"project": "OPS", "summary": "payments-api OOMKilled", "description": "leaked hunter2-secret",
		"key": "oomkilled-payments-api", "priority": "High"}

	out, err := call(t, tl, a)
	if err != nil || !strings.Contains(out, "created OPS-1: https://jira/browse/OPS-1") {
		t.Fatalf("%q %v", out, err)
	}
	c := f.created[0]
	fp := jira.Fingerprint("triage", "OPS", "oomkilled-payments-api")
	if c.ProjectKey != "OPS" || c.IssueType != "Task" ||
		c.AdditionalFields != `{"labels":["heraion","`+fp+`","heraion-triage"],"priority":{"name":"High"}}` {
		t.Errorf("create args %+v", c)
	}
	if strings.Contains(c.Description, "hunter2-secret") || !strings.Contains(c.Description, "Created by HeraIon playbook triage.") {
		t.Errorf("description %q", c.Description)
	}
	if want := `project = "OPS" AND labels = "` + fp + `" AND statusCategory != Done`; !strings.HasPrefix(f.searches[0], want) {
		t.Errorf("search %q", f.searches[0])
	}

	// Same key again in this run: answered from the run's own record, as
	// Jira's search may not index the new issue yet.
	out, err = call(t, tl, a)
	if err != nil || !strings.Contains(out, "already handled in this run") || len(f.searches) != 1 {
		t.Fatalf("%q %v", out, err)
	}
	// Same key in a later run: found by the fingerprint search, nothing created, budget kept.
	tl.Created = nil
	out, err = call(t, tl, a)
	if err != nil || !strings.Contains(out, "already exists: OPS-1") || len(f.created) != 1 {
		t.Fatalf("%q %v", out, err)
	}
	if tl.Created[0].Outcome != jira.OutcomeDuplicate {
		t.Error("duplicate not recorded")
	}
	// Budget is 2: one more works, then refused.
	for i, ok := range []bool{true, false} {
		_, err := call(t, tl, map[string]any{"project": "OPS", "summary": fmt.Sprintf("other %d", i), "description": "d", "key": fmt.Sprint(i)})
		if (err == nil) != ok {
			t.Fatalf("issue %d: %v", i, err)
		}
	}
}

func TestCreateGuards(t *testing.T) {
	tl, f := newTool(t, false, 5)
	for _, a := range []map[string]any{
		{"project": "SEC", "summary": "s", "description": "d"},
		{"project": "OPS", "summary": " ", "description": "d"},
		{"project": "OPS", "summary": "s", "description": "d", "labels": []string{"has space"}},
	} {
		if _, err := call(t, tl, a); err == nil {
			t.Errorf("expected error for %v", a)
		}
	}
	if len(f.created)+len(f.searches) != 0 {
		t.Fatalf("no call may reach the server, got %d creates, %d searches", len(f.created), len(f.searches))
	}
}

func TestCreateDryRunCallsNothing(t *testing.T) {
	tl, f := newTool(t, true, 1)
	out, err := call(t, tl, map[string]any{"project": "OPS", "summary": "s", "description": "d", "key": "k"})
	if err != nil || !strings.Contains(out, "dry-run") || tl.Created[0].Outcome != jira.OutcomeDryRun {
		t.Fatalf("%q %v", out, err)
	}
	if len(f.created)+len(f.searches) != 0 {
		t.Fatal("dry-run must not call the server")
	}
}

// maxIssues: 1 allows one dry-run issue, as in a real run.
func TestCreateDryRunUsesBudget(t *testing.T) {
	tl, _ := newTool(t, true, 1)
	if _, err := call(t, tl, map[string]any{"project": "OPS", "summary": "a", "description": "d", "key": "a"}); err != nil {
		t.Fatal(err)
	}
	_, err := call(t, tl, map[string]any{"project": "OPS", "summary": "b", "description": "d", "key": "b"})
	if err == nil || !strings.Contains(err.Error(), "budget") || len(tl.Created) != 1 {
		t.Fatalf("second dry-run issue: %v, created %d", err, len(tl.Created))
	}
}

func TestSchemaListsAllowedProjects(t *testing.T) {
	tl, _ := newTool(t, true, 1)
	var s struct {
		Properties map[string]struct{ Enum []string }
		Required   []string
	}
	if err := json.Unmarshal(tl.Schema(), &s); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(s.Properties["project"].Enum) != "[OPS]" || !strings.Contains(fmt.Sprint(s.Required), "key") {
		t.Errorf("%+v", s)
	}
}

func TestFingerprint(t *testing.T) {
	a := jira.Fingerprint("p", "OPS", "OOMKilled  payments-api")
	if a != jira.Fingerprint("p", "OPS", "oomkilled payments-api") {
		t.Error("case and spacing must not matter")
	}
	if a == jira.Fingerprint("p", "SEC", "oomkilled payments-api") || len(a) != len("heraion-")+12 {
		t.Error("project must be part of the fingerprint")
	}
}
