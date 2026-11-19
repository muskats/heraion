package jira

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/muskats/heraion/internal/atlassian"
	"github.com/muskats/heraion/internal/guardrails"
	"github.com/muskats/heraion/internal/secret"
)

// fakeJira records requests and answers like Jira does.
type fakeJira struct {
	mu       sync.Mutex
	requests []recorded
	open     map[string]string // fingerprint label -> issue key
	next     int
	fail     int // status to return on create
}

type recorded struct {
	Method, Path, Auth string
	Body               map[string]any
}

func (f *fakeJira) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	f.requests = append(f.requests, recorded{r.Method, r.URL.Path, r.Header.Get("Authorization"), body})
	w.Header().Set("Content-Type", "application/json")
	switch {
	case strings.HasSuffix(r.URL.Path, "/issue") && r.Method == http.MethodPost:
		if f.fail != 0 {
			w.WriteHeader(f.fail)
			_, _ = io.WriteString(w, `{"errorMessages":[],"errors":{"summary":"You must specify a summary of the issue."}}`)
			return
		}
		f.next++
		key := "OPS-" + string(rune('0'+f.next))
		fields := body["fields"].(map[string]any)
		labels, _ := fields["labels"].([]any)
		for _, l := range labels {
			if strings.HasPrefix(l.(string), "heraion-") && len(l.(string)) == len("heraion-")+12 {
				f.open[l.(string)] = key
			}
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]string{"id": "10001", "key": key, "self": "x"})
	case strings.HasSuffix(r.URL.Path, "/search/jql") || strings.HasSuffix(r.URL.Path, "/search"):
		jql, _ := body["jql"].(string)
		var issues []map[string]string
		for label, key := range f.open {
			if strings.Contains(jql, `labels = "`+label+`"`) {
				issues = append(issues, map[string]string{"id": "1", "key": key})
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"issues": issues, "isLast": true})
	case strings.HasSuffix(r.URL.Path, "/myself"):
		_, _ = io.WriteString(w, `{"accountId":"abc"}`)
	default:
		http.NotFound(w, r)
	}
}

func setup(t *testing.T, cloud bool) (*Client, *fakeJira) {
	t.Helper()
	f := &fakeJira{open: map[string]string{}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	email := ""
	if cloud {
		email = "bot@acme.com"
	}
	c := &Client{BaseURL: srv.URL, Cloud: cloud, HTTP: atlassian.New(email, secret.Static("jira-token-123"))}
	return c, f
}

func TestCloudCreate(t *testing.T) {
	c, f := setup(t, true)
	issue, err := c.Create(context.Background(), NewIssue{Project: "OPS", Type: "Bug", Summary: "s",
		Description: "Pod restarted.\n\n- 14 restarts\n- exit 137", Labels: []string{"heraion"}, Priority: "High"})
	if err != nil {
		t.Fatal(err)
	}
	if issue.Key != "OPS-1" || !strings.HasSuffix(issue.URL, "/browse/OPS-1") {
		t.Fatalf("%+v", issue)
	}
	r := f.requests[0]
	if r.Path != "/rest/api/3/issue" {
		t.Errorf("path %s", r.Path)
	}
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("bot@acme.com:jira-token-123"))
	if r.Auth != want {
		t.Errorf("auth %q", r.Auth)
	}
	fields := r.Body["fields"].(map[string]any)
	desc := fields["description"].(map[string]any)
	if desc["type"] != "doc" || desc["version"].(float64) != 1 {
		t.Errorf("description is not ADF: %v", desc)
	}
	content := desc["content"].([]any)
	if content[1].(map[string]any)["type"] != "bulletList" {
		t.Errorf("bullets not converted: %v", content)
	}
	if fields["priority"].(map[string]any)["name"] != "High" || fields["issuetype"].(map[string]any)["name"] != "Bug" {
		t.Errorf("fields %v", fields)
	}
}

func TestDataCenterCreateAndSearch(t *testing.T) {
	c, f := setup(t, false)
	if _, err := c.Create(context.Background(), NewIssue{Project: "OPS", Type: "Task", Summary: "s", Description: "# Cause\n```\nOOMKilled\n```"}); err != nil {
		t.Fatal(err)
	}
	r := f.requests[0]
	if r.Path != "/rest/api/2/issue" || r.Auth != "Bearer jira-token-123" {
		t.Errorf("path %s auth %q", r.Path, r.Auth)
	}
	if d := r.Body["fields"].(map[string]any)["description"]; d != "h1. Cause\n\n{code}\nOOMKilled\n{code}" {
		t.Errorf("wiki description %q", d)
	}
	if _, err := c.FindOpen(context.Background(), `project = "OPS"`); err != nil {
		t.Fatal(err)
	}
	if p := f.requests[1].Path; p != "/rest/api/2/search" {
		t.Errorf("DC search path %s", p)
	}
}

func TestCloudSearchUsesEnhancedEndpoint(t *testing.T) {
	c, f := setup(t, true)
	if _, err := c.FindOpen(context.Background(), `project = "OPS"`); err != nil {
		t.Fatal(err)
	}
	r := f.requests[0]
	if r.Path != "/rest/api/3/search/jql" {
		t.Fatalf("path %s (the old /rest/api/3/search returns 410)", r.Path)
	}
	// /search/jql returns only ids unless fields are requested.
	if fields, _ := r.Body["fields"].([]any); len(fields) == 0 {
		t.Error("fields must be requested explicitly")
	}
}

func TestErrorMessages(t *testing.T) {
	c, f := setup(t, true)
	f.fail = http.StatusBadRequest
	_, err := c.Create(context.Background(), NewIssue{Project: "OPS", Type: "Task"})
	var apiErr *atlassian.APIError
	if !errors.As(err, &apiErr) || apiErr.Status != 400 || !strings.Contains(err.Error(), "summary: You must specify a summary") {
		t.Fatalf("got %v", err)
	}
}

func newTool(t *testing.T, api API, dryRun bool, max int) *Tool {
	g := guardrails.New(nil, 0, dryRun, 0, guardrails.NewRedactor("hunter2-secret")).WithIssues([]string{"OPS"}, max)
	return &Tool{API: api, Playbook: "triage", IssueType: "Task", Labels: []string{"heraion"}, Guard: g,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

func call(t *testing.T, tl *Tool, a map[string]any) (string, error) {
	b, _ := json.Marshal(a)
	return tl.Call(context.Background(), b)
}

func TestToolDeduplicatesAndBudgets(t *testing.T) {
	c, f := setup(t, true)
	tl := newTool(t, c, false, 2)
	a := map[string]any{"project": "OPS", "summary": "payments-api OOMKilled", "description": "leaked hunter2-secret", "key": "oomkilled-payments-api"}

	out, err := call(t, tl, a)
	if err != nil || !strings.Contains(out, "created OPS-1") {
		t.Fatalf("%q %v", out, err)
	}
	create := f.requests[1].Body["fields"].(map[string]any)
	labels := create["labels"].([]any)
	if labels[0] != "heraion" || labels[1] != Fingerprint("triage", "OPS", "oomkilled-payments-api") || labels[2] != "heraion-triage" {
		t.Errorf("labels %v", labels)
	}
	if b, _ := json.Marshal(create["description"]); strings.Contains(string(b), "hunter2-secret") {
		t.Error("description must be redacted")
	}

	// Same key again: found by the fingerprint search, nothing created, budget kept.
	out, err = call(t, tl, a)
	if err != nil || !strings.Contains(out, "already exists: OPS-1") {
		t.Fatalf("%q %v", out, err)
	}
	if !tl.Created[1].Duplicate {
		t.Error("duplicate not recorded")
	}
	// Budget is 2: one more works, then refused.
	for i, ok := range []bool{true, false} {
		_, err := call(t, tl, map[string]any{"project": "OPS", "summary": "other " + string(rune('a'+i)), "description": "d"})
		if (err == nil) != ok {
			t.Fatalf("issue %d: %v", i, err)
		}
	}
}

func TestToolGuards(t *testing.T) {
	c, f := setup(t, true)
	tl := newTool(t, c, false, 5)
	for _, a := range []map[string]any{
		{"project": "SEC", "summary": "s", "description": "d"},
		{"project": "OPS", "summary": " ", "description": "d"},
		{"project": "OPS", "summary": "s", "description": "d", "labels": []string{"has space"}},
	} {
		if _, err := call(t, tl, a); err == nil {
			t.Errorf("expected error for %v", a)
		}
	}
	if len(f.requests) != 0 {
		t.Fatalf("no request must reach Jira, got %d", len(f.requests))
	}
}

func TestToolDryRun(t *testing.T) {
	tl := newTool(t, nil, true, 1)
	out, err := call(t, tl, map[string]any{"project": "OPS", "summary": "s", "description": "d"})
	if err != nil || !strings.Contains(out, "dry-run") || !tl.Created[0].DryRun {
		t.Fatalf("%q %v", out, err)
	}
}

func TestFingerprint(t *testing.T) {
	a := Fingerprint("p", "OPS", "OOMKilled  payments-api")
	if a != Fingerprint("p", "OPS", "oomkilled payments-api") {
		t.Error("case and spacing must not matter")
	}
	if a == Fingerprint("p", "SEC", "oomkilled payments-api") || len(a) != len("heraion-")+12 {
		t.Error("project must be part of the fingerprint")
	}
}

func TestToADF(t *testing.T) {
	doc := ToADF("# Title\nline one\nline two\n\n```\ncode\n```")
	b, _ := json.Marshal(doc)
	s := string(b)
	for _, want := range []string{`"type":"heading"`, `"level":1`, `"type":"hardBreak"`, `"type":"codeBlock"`} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %s in %s", want, s)
		}
	}
	if strings.Contains(s, "null") {
		t.Errorf("ADF must not contain null: %s", s)
	}
}
