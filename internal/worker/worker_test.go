package worker_test

import (
	"context"
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
	c := llm.NewClient(url, secret.Static("sk-live-abcdef"))
	c.RetryBase = time.Millisecond
	return c
}

func TestRunOpensPR(t *testing.T) {
	base := t.TempDir()
	cloneURL := scmtest.NewRemote(t, base, repo, map[string]string{"Dockerfile": "FROM alpine:3.18\n"})
	srv := llmtest.New(t,
		llmtest.Turn{Calls: []llmtest.Call{{Name: "repo_read_file", Args: map[string]string{"repo": repo, "path": "Dockerfile"}}}},
		llmtest.Turn{Calls: []llmtest.Call{{Name: "repo_write_file", Args: map[string]string{"repo": repo, "path": "Dockerfile", "content": "FROM alpine:3.20\n"}}}},
		llmtest.Turn{Calls: []llmtest.Call{{Name: "open_pr", Args: map[string]string{"repo": repo, "title": "Bump alpine to 3.20", "body": "CVE-2024-0001", "branch": "bump-alpine"}}}},
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
	if got, err := scmtest.RemoteBranchFile(t, base, repo, "heraion/trivy-remediate/bump-alpine", "Dockerfile"); err != nil || got != "FROM alpine:3.20\n" {
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
