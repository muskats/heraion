package worker_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/muskats/heraion/internal/config"
	"github.com/muskats/heraion/internal/guardrails"
	"github.com/muskats/heraion/internal/llm"
	"github.com/muskats/heraion/internal/llm/llmtest"
	"github.com/muskats/heraion/internal/playbook"
	"github.com/muskats/heraion/internal/scm/scmtest"
	"github.com/muskats/heraion/internal/secret"
	"github.com/muskats/heraion/internal/worker"
)

const repo = "acme/app"

func cfg(t *testing.T, llmURL, cloneURL string) *config.Config {
	t.Helper()
	return &config.Config{
		LLM: config.LLM{BaseURL: llmURL, Model: "m", APIKey: secret.Static("sk-live-abcdef"), MaxSteps: 10,
			Timeout: config.Duration{Duration: time.Minute}},
		Git:     config.Git{Provider: "gitea", CloneBaseURL: cloneURL, AuthorName: "HeraIon", AuthorEmail: "h@x", Token: secret.Static("git-token-123456")},
		Repos:   []string{repo},
		WorkDir: t.TempDir(),
	}
}

func pb(t *testing.T, src string) *playbook.Playbook {
	t.Helper()
	p, err := playbook.Parse([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func fastClient(url string) llm.Chatter {
	return llm.NewClient(url, secret.Static("sk-live-abcdef"))
}

func TestRunOpensPR(t *testing.T) {
	base := t.TempDir()
	cloneURL := scmtest.NewRemote(t, base, repo, map[string]string{"Dockerfile": "FROM alpine:3.18\n"})
	srv := llmtest.New(t,
		llmtest.Turn{Calls: []llmtest.Call{{Name: "repo_read_file", Args: map[string]string{"repo": repo, "path": "Dockerfile"}}}},
		llmtest.Turn{Calls: []llmtest.Call{{Name: "repo_write_file", Args: map[string]string{"repo": repo, "path": "Dockerfile", "content": "FROM alpine:3.20\n"}}}},
		llmtest.Turn{Calls: []llmtest.Call{{Name: "open_pr", Args: map[string]string{"repo": repo, "title": "Bump alpine to 3.20", "body": "CVE-2024-0001"}}}},
		llmtest.Turn{Final: "Opened one PR."},
	)
	prs := &scmtest.FakePRs{}
	c := cfg(t, srv.BaseURL(), cloneURL)
	rep, err := worker.Run(context.Background(), worker.Options{
		Config: c,
		Playbook: pb(t, `
name: trivy-remediate
trigger: {schedule: "0 */6 * * *"}
tools: [repo_*, open_pr]
prompt: Fix fixable HIGH CVEs.
`),
		Event:    []byte(`{"alert":"token git-token-123456"}`),
		Redactor: redactor(c),
		LLM:      fastClient(srv.BaseURL()),
		PRs:      prs,
		Log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Final != "Opened one PR." || len(rep.PRs) != 1 || rep.PRs[0].URL == "" || rep.DryRun {
		t.Fatalf("%+v", rep)
	}
	if !strings.HasPrefix(rep.PRs[0].Branch, "heraion/trivy-remediate/dockerfile-") {
		t.Errorf("branch %q is not named after the changed file", rep.PRs[0].Branch)
	}
	if got, err := scmtest.RemoteBranchFile(t, base, repo, rep.PRs[0].Branch, "Dockerfile"); err != nil || got != "FROM alpine:3.20\n" {
		t.Fatalf("pushed content %q %v", got, err)
	}

	first := srv.Requests[0]
	sys, user := first.Messages[0].Text(), first.Messages[1].Text()
	if !strings.Contains(sys, repo) || !strings.Contains(user, "<event>") {
		t.Errorf("prompts missing repo/event:\n%s\n%s", sys, user)
	}
	if strings.Contains(user, "git-token-123456") {
		t.Error("event payload was not redacted before reaching the LLM")
	}
	var names []string
	for _, tl := range first.Tools {
		names = append(names, tl.Function.Name)
	}
	if !slices.Equal(names, []string{"open_pr", "repo_list", "repo_read_file", "repo_write_file"}) {
		t.Errorf("tools = %v", names)
	}
}

// runFix runs a playbook that edits the Dockerfile and opens one PR, then
// answers final, and returns the report and the log.
func runFix(t *testing.T, prs *scmtest.FakePRs, dryRun bool, final string) (*worker.Report, string, error) {
	t.Helper()
	base := t.TempDir()
	cloneURL := scmtest.NewRemote(t, base, repo, map[string]string{"Dockerfile": "FROM alpine:3.18\n"})
	srv := llmtest.New(t,
		llmtest.Turn{Calls: []llmtest.Call{{Name: "repo_write_file", Args: map[string]string{"repo": repo, "path": "Dockerfile", "content": "FROM alpine:3.20\n"}}}},
		llmtest.Turn{Calls: []llmtest.Call{{Name: "open_pr", Args: map[string]string{"repo": repo, "title": "Bump alpine", "body": "b"}}}},
		llmtest.Turn{Final: final},
	)
	c := cfg(t, srv.BaseURL(), cloneURL)
	c.DryRun = dryRun
	var logs strings.Builder
	o := worker.Options{
		Config:   c,
		Playbook: pb(t, "name: fix\ntrigger: {manual: true}\ntools: [repo_*, open_pr]\nprompt: Fix.\n"),
		Redactor: redactor(c),
		LLM:      fastClient(srv.BaseURL()),
		Log:      slog.New(slog.NewJSONHandler(&logs, nil)),
	}
	if prs != nil {
		o.PRs = prs
	}
	rep, err := worker.Run(context.Background(), o)
	return rep, logs.String(), err
}

func TestReportIsPostedOnOpenedPR(t *testing.T) {
	prs := &scmtest.FakePRs{}
	rep, _, err := runFix(t, prs, false, "Bumped alpine. Token git-token-123456 was not used.")
	if err != nil {
		t.Fatal(err)
	}
	if len(prs.Comments) != 1 || prs.Comments[0].Repo != repo || prs.Comments[0].Number != rep.PRs[0].Number {
		t.Fatalf("comments %+v, PRs %+v", prs.Comments, rep.PRs)
	}
	body := prs.Comments[0].Body
	if !strings.Contains(body, "Bumped alpine.") || !strings.Contains(body, "`fix`") {
		t.Errorf("body %q", body)
	}
	if strings.Contains(body, "git-token-123456") {
		t.Error("report was not redacted")
	}
}

// A rerun with the same change leaves the PR alone and posts no second report.
func TestReportNotPostedOnUnchangedPR(t *testing.T) {
	prs := &scmtest.FakePRs{}
	if _, _, err := runFix(t, prs, false, "Bumped alpine."); err != nil {
		t.Fatal(err)
	}
	rep, _, err := runFix(t, prs, false, "Bumped alpine again.")
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.PRs) != 1 || rep.PRs[0].Outcome != scm.OutcomeUnchanged || len(prs.Comments) != 1 {
		t.Fatalf("PRs %+v, comments %d", rep.PRs, len(prs.Comments))
	}
}

func TestReportNotPostedInDryRun(t *testing.T) {
	prs := &scmtest.FakePRs{}
	_, logs, err := runFix(t, prs, true, "Bumped alpine.")
	if err != nil {
		t.Fatal(err)
	}
	if len(prs.Comments) != 0 || len(prs.Created) != 0 {
		t.Fatalf("dry-run posted: %+v", prs)
	}
	if !strings.Contains(logs, "dry-run: would post report") {
		t.Errorf("no dry-run line in:\n%s", logs)
	}
}

func TestReportCommentFailureIsAWarning(t *testing.T) {
	prs := &scmtest.FakePRs{CommentErr: errors.New("not supported")}
	rep, logs, err := runFix(t, prs, false, "Bumped alpine.")
	if err != nil || len(rep.PRs) != 1 {
		t.Fatalf("a failed comment must not fail the run: %v %+v", err, rep)
	}
	if !strings.Contains(logs, `"level":"WARN","msg":"could not post the report on the PR"`) {
		t.Errorf("no warning in:\n%s", logs)
	}
}

func TestBrokenTokenFileFailsTheRun(t *testing.T) {
	srv := llmtest.New(t, llmtest.Turn{Final: "never"})
	c := cfg(t, srv.BaseURL(), "file:///nonexistent")
	c.Git.Token = secret.FromMap("HERAION_GIT_TOKEN", map[string]string{
		"HERAION_GIT_TOKEN_FILE": "/nonexistent/token", "HERAION_GIT_TOKEN_FALLBACK": "lit",
	})
	_, err := worker.Run(context.Background(), worker.Options{
		Config:   c,
		Playbook: pb(t, "name: x\ntrigger: {manual: true}\ntools: ['*']\nprompt: p\n"),
		LLM:      fastClient(srv.BaseURL()),
		PRs:      &scmtest.FakePRs{},
		Log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err == nil || !strings.Contains(err.Error(), "HERAION_GIT_TOKEN_FILE") {
		t.Fatalf("got %v", err)
	}
	if len(srv.Requests) != 0 {
		t.Errorf("the run must fail before the first LLM request, got %d", len(srv.Requests))
	}
}

func TestReportPlaybookHasNoOpenPR(t *testing.T) {
	base := t.TempDir()
	cloneURL := scmtest.NewRemote(t, base, repo, map[string]string{"a": "b"})
	srv := llmtest.New(t, llmtest.Turn{Final: "report"})
	rep, err := worker.Run(context.Background(), worker.Options{
		Config:   cfg(t, srv.BaseURL(), cloneURL),
		Playbook: pb(t, "name: triage\ntrigger: {webhook: alertmanager}\ntools: ['*']\noutput: report\nprompt: Triage.\n"),
		LLM:      fastClient(srv.BaseURL()),
		Log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil || rep.Final != "report" {
		t.Fatalf("%v %v", rep, err)
	}
	for _, tl := range srv.Requests[0].Tools {
		if tl.Function.Name == "open_pr" {
			t.Fatal("open_pr must not be offered for report playbooks")
		}
	}
}

func TestNoReposHidesGitTools(t *testing.T) {
	srv := llmtest.New(t, llmtest.Turn{Final: "nothing to do"})
	c := cfg(t, srv.BaseURL(), "file:///nonexistent")
	c.Repos = nil
	_, err := worker.Run(context.Background(), worker.Options{
		Config:   c,
		Playbook: pb(t, "name: x\ntrigger: {manual: true}\ntools: ['*']\nprompt: p\nguardrails: {dryRun: true}\n"),
		LLM:      fastClient(srv.BaseURL()),
		Log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if n := len(srv.Requests[0].Tools); n != 0 {
		t.Fatalf("expected no tools, got %d", n)
	}
}

func TestRunWithMCPServer(t *testing.T) {
	srv := sdk.NewServer(&sdk.Implementation{Name: "fake-k8s", Version: "1"}, nil)
	type args struct {
		APIVersion string `json:"apiVersion"`
		Kind       string `json:"kind"`
	}
	sdk.AddTool(srv, &sdk.Tool{Name: "resources_list"}, func(_ context.Context, _ *sdk.CallToolRequest, a args) (*sdk.CallToolResult, any, error) {
		return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: a.Kind + ": CVE-2024-1 openssl 3.0.1 -> 3.0.15 (token git-token-123456)"}}}, nil, nil
	})
	sdk.AddTool(srv, &sdk.Tool{Name: "pods_exec"}, func(context.Context, *sdk.CallToolRequest, args) (*sdk.CallToolResult, any, error) {
		panic("not allowed by playbook")
	})
	ts := httptest.NewServer(sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return srv }, nil))
	t.Cleanup(ts.Close)

	llmSrv := llmtest.New(t,
		llmtest.Turn{Calls: []llmtest.Call{{Name: "k8s__resources_list", Args: map[string]string{"apiVersion": "aquasecurity.github.io/v1alpha1", "kind": "VulnerabilityReport"}}}},
		llmtest.Turn{Final: "1 fixable CVE"},
	)
	c := cfg(t, llmSrv.BaseURL(), "file:///unused")
	c.Repos = nil
	c.MCPServers = map[string]*config.MCPServer{
		"k8s":     {URL: ts.URL, Transport: "streamable", Timeout: config.Duration{Duration: 5 * time.Second}},
		"grafana": {URL: "http://127.0.0.1:1/mcp", Transport: "streamable", Required: true}, // not needed by playbook: never dialled
	}
	rep, err := worker.Run(context.Background(), worker.Options{
		Config:   c,
		Playbook: pb(t, "name: scan\ntrigger: {manual: true}\ntools: [k8s.resources_*]\noutput: report\nprompt: p\n"),
		Redactor: redactor(c),
		LLM:      fastClient(llmSrv.BaseURL()),
		Log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil || rep.Final != "1 fixable CVE" {
		t.Fatalf("%v %v", rep, err)
	}
	if tools := llmSrv.Requests[0].Tools; len(tools) != 1 || tools[0].Function.Name != "k8s__resources_list" {
		t.Fatalf("tools offered: %+v", tools)
	}
	result := llmSrv.Requests[1].Messages[3].Text()
	if !strings.Contains(result, "VulnerabilityReport: CVE-2024-1") || strings.Contains(result, "git-token-123456") {
		t.Fatalf("tool result %q", result)
	}
}

func redactor(c *config.Config) *guardrails.Redactor {
	r := guardrails.NewRedactor()
	r.AddSources(c.Secrets()...)
	return r
}

// fakeAtlassian is an mcp-atlassian server with the tools create_issue uses.
func fakeAtlassian(t *testing.T, created *[]string) string {
	srv := sdk.NewServer(&sdk.Implementation{Name: "mcp-atlassian", Version: "0.24.7"}, nil)
	type searchArgs struct { // as in mcp-atlassian v0.24.7
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
	type getArgs struct {
		IssueKey string `json:"issue_key"`
	}
	sdk.AddTool(srv, &sdk.Tool{Name: "jira_search"}, func(context.Context, *sdk.CallToolRequest, searchArgs) (*sdk.CallToolResult, any, error) {
		return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: `{"issues":[]}`}}}, nil, nil
	})
	sdk.AddTool(srv, &sdk.Tool{Name: "jira_create_issue"}, func(_ context.Context, _ *sdk.CallToolRequest, a createArgs) (*sdk.CallToolResult, any, error) {
		*created = append(*created, a.ProjectKey+": "+a.Summary)
		return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: `{"issue":{"key":"OPS-7","url":"https://acme.atlassian.net/browse/OPS-7"}}`}}}, nil, nil
	})
	sdk.AddTool(srv, &sdk.Tool{Name: "jira_get_issue"}, func(context.Context, *sdk.CallToolRequest, getArgs) (*sdk.CallToolResult, any, error) {
		return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: `{"key":"CHG-42"}`}}}, nil, nil
	})
	ts := httptest.NewServer(sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return srv }, nil))
	t.Cleanup(ts.Close)
	return ts.URL
}

func TestRunCreatesJiraIssueThroughMCP(t *testing.T) {
	srv := llmtest.New(t,
		llmtest.Turn{Calls: []llmtest.Call{{Name: "create_issue", Args: map[string]string{
			"project": "OPS", "summary": "payments-api OOMKilled", "description": "14 restarts", "key": "oom-payments"}}}},
		llmtest.Turn{Final: "Opened OPS-7."},
	)
	var created []string
	c := cfg(t, srv.BaseURL(), "file:///unused")
	c.Repos = nil
	c.MCPServers = map[string]*config.MCPServer{"atlassian": {URL: fakeAtlassian(t, &created), Transport: "streamable",
		Timeout: config.Duration{Duration: 5 * time.Second}}}
	c.Jira = &config.Jira{Server: "atlassian", Projects: []string{"OPS", "SEC"}, IssueType: "Task"}
	rep, err := worker.Run(context.Background(), worker.Options{
		Config: c,
		Playbook: pb(t, "name: triage\ntrigger: {webhook: alertmanager}\n"+
			"tools: [create_issue, atlassian.*]\njiraProjects: [OPS]\noutput: report\nprompt: p\n"),
		LLM: fastClient(srv.BaseURL()),
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(created) != 1 || len(rep.Issues) != 1 || rep.Issues[0].Key != "OPS-7" {
		t.Fatalf("created=%v report=%+v", created, rep.Issues)
	}
	// The raw jira_create_issue is hidden: issues go through the guard only.
	var names []string
	var schema string
	for _, tl := range srv.Requests[0].Tools {
		names = append(names, tl.Function.Name)
		if tl.Function.Name == "create_issue" {
			schema = string(tl.Function.Parameters)
		}
	}
	if strings.Join(names, ",") != "atlassian__jira_get_issue,atlassian__jira_search,create_issue" {
		t.Errorf("tools %v", names)
	}
	// jiraProjects narrows the enum offered to the model to OPS.
	if !strings.Contains(schema, `"enum":["OPS"]`) {
		t.Errorf("project enum not narrowed: %s", schema)
	}
	if sys := srv.Requests[0].Messages[0].Text(); !strings.Contains(sys, "create_issue, in: OPS") {
		t.Errorf("system prompt does not mention Jira: %s", sys)
	}
}

func TestCreateIssueNeedsNoAtlassianPattern(t *testing.T) {
	// tools: [create_issue] alone still connects the jira server.
	srv := llmtest.New(t, llmtest.Turn{Final: "ok"})
	var created []string
	c := cfg(t, srv.BaseURL(), "file:///unused")
	c.Repos = nil
	c.MCPServers = map[string]*config.MCPServer{"atlassian": {URL: fakeAtlassian(t, &created), Transport: "streamable",
		Timeout: config.Duration{Duration: 5 * time.Second}}}
	c.Jira = &config.Jira{Server: "atlassian", Projects: []string{"OPS"}, IssueType: "Task"}
	if _, err := worker.Run(context.Background(), worker.Options{
		Config: c, Playbook: pb(t, "name: x\ntrigger: {manual: true}\ntools: [create_issue]\noutput: report\nprompt: p\n"),
		LLM: fastClient(srv.BaseURL()), Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}); err != nil {
		t.Fatal(err)
	}
	if tools := srv.Requests[0].Tools; len(tools) != 1 || tools[0].Function.Name != "create_issue" {
		t.Fatalf("tools %+v", tools)
	}
}

func TestNoJiraToolWithoutConfig(t *testing.T) {
	srv := llmtest.New(t, llmtest.Turn{Final: "ok"})
	c := cfg(t, srv.BaseURL(), "file:///unused")
	c.Repos = nil
	_, err := worker.Run(context.Background(), worker.Options{
		Config: c, Playbook: pb(t, "name: x\ntrigger: {manual: true}\ntools: ['*']\nprompt: p\nguardrails: {dryRun: true}\n"),
		LLM: fastClient(srv.BaseURL()), Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if tools := srv.Requests[0].Tools; len(tools) != 0 {
		t.Fatalf("create_issue offered without jira config: %v", tools)
	}
}

func TestZeroBudgetHidesTools(t *testing.T) {
	base := t.TempDir()
	cloneURL := scmtest.NewRemote(t, base, repo, map[string]string{"a": "b"})
	srv := llmtest.New(t, llmtest.Turn{Final: "report"})
	_, err := worker.Run(context.Background(), worker.Options{
		Config:   cfg(t, srv.BaseURL(), cloneURL),
		Playbook: pb(t, "name: x\ntrigger: {manual: true}\ntools: ['*']\nprompt: p\nguardrails: {maxPRs: 0}\n"),
		PRs:      &scmtest.FakePRs{},
		LLM:      fastClient(srv.BaseURL()),
		Log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, tl := range srv.Requests[0].Tools {
		if tl.Function.Name == "open_pr" {
			t.Fatal("maxPRs: 0 must hide open_pr")
		}
	}
}

func TestModeIsNarrowedToGlobal(t *testing.T) {
	base := t.TempDir()
	cloneURL := scmtest.NewRemote(t, base, repo, map[string]string{"Dockerfile": "FROM alpine:3.18\n"})
	srv := llmtest.New(t,
		llmtest.Turn{Calls: []llmtest.Call{{Name: "repo_write_file", Args: map[string]string{"repo": repo, "path": "Dockerfile", "content": "FROM alpine:3.18\nUSER root\n"}}}},
		llmtest.Turn{Calls: []llmtest.Call{{Name: "open_pr", Args: map[string]string{"repo": repo, "title": "Run as root", "body": "b", "branch": "root"}}}},
		llmtest.Turn{Final: "done"},
	)
	c := cfg(t, srv.BaseURL(), cloneURL)
	c.Mode = config.ModeSafe
	prs := &scmtest.FakePRs{}
	var logs strings.Builder
	rep, err := worker.Run(context.Background(), worker.Options{
		Config:   c,
		Playbook: pb(t, "name: fix\ntrigger: {manual: true}\ntools: [repo_*, open_pr]\nmode: full\nprompt: Fix.\n"),
		LLM:      fastClient(srv.BaseURL()),
		PRs:      prs,
		Log:      slog.New(slog.NewTextHandler(&logs, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	// Safe mode refuses a change that is not a version bump.
	if rep.Mode != config.ModeSafe || len(rep.PRs) != 0 || len(prs.Created) != 0 {
		t.Fatalf("want safe mode and no PR: %+v", rep)
	}
	if !strings.Contains(logs.String(), "narrowed") {
		t.Errorf("no warning:\n%s", logs.String())
	}
	if !strings.Contains(srv.Requests[0].Messages[0].Text(), "Safe mode") {
		t.Error("system prompt must describe safe mode")
	}
}

func TestSafeModePrompt(t *testing.T) {
	srv := llmtest.New(t, llmtest.Turn{Final: "done"})
	c := cfg(t, srv.BaseURL(), "file:///nonexistent")
	c.Mode = config.ModeFull
	rep, err := worker.Run(context.Background(), worker.Options{
		Config:   c,
		Playbook: pb(t, "name: fix\ntrigger: {manual: true}\ntools: [repo_*, open_pr]\nmode: safe\nprompt: Fix.\n"),
		LLM:      fastClient(srv.BaseURL()),
		PRs:      &scmtest.FakePRs{},
		Log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil || rep.Mode != config.ModeSafe {
		t.Fatalf("%+v %v", rep, err)
	}
	if !strings.Contains(srv.Requests[0].Messages[0].Text(), "Safe mode") {
		t.Error("system prompt must describe safe mode")
	}
}

func TestNamespacesPrompt(t *testing.T) {
	srv := llmtest.New(t, llmtest.Turn{Final: "done"}, llmtest.Turn{Final: "done"})
	c := cfg(t, srv.BaseURL(), "file:///nonexistent")
	run := func() string {
		_, err := worker.Run(context.Background(), worker.Options{
			Config: c, Playbook: pb(t, "name: x\ntrigger: {manual: true}\ntools: [k8s.*]\noutput: report\nprompt: p\n"),
			LLM: fastClient(srv.BaseURL()), Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		})
		if err != nil {
			t.Fatal(err)
		}
		return srv.Requests[len(srv.Requests)-1].Messages[0].Text()
	}
	if strings.Contains(run(), "namespaces you may read") {
		t.Error("no namespaces configured: no namespace line")
	}
	c.Namespaces = []string{"shop", "payments"}
	if !strings.Contains(run(), "Kubernetes namespaces you may read: shop, payments.") {
		t.Error("system prompt must name the namespaces")
	}
}
