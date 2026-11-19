package scm_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/muskats/heraion/internal/config"
	"github.com/muskats/heraion/internal/guardrails"
	"github.com/muskats/heraion/internal/llm"
	"github.com/muskats/heraion/internal/scm"
	"github.com/muskats/heraion/internal/scm/scmtest"
)

const repo = "acme/app"

func setup(t *testing.T, dryRun bool, maxPRs int) (*scm.Toolbox, map[string]llm.Tool, *scmtest.FakePRs, string) {
	t.Helper()
	base := t.TempDir()
	url := scmtest.NewRemote(t, base, repo, map[string]string{
		"Dockerfile":         "FROM alpine:3.18\n",
		"deploy/values.yaml": "image: app:1\n",
	})
	ws := scm.NewWorkspaces(config.Git{CloneBaseURL: url, AuthorName: "HeraIon", AuthorEmail: "h@x"}, t.TempDir())
	t.Cleanup(ws.Close)
	prs := &scmtest.FakePRs{}
	tb := &scm.Toolbox{
		Playbook:   "trivy-remediate",
		Workspaces: ws,
		PRs:        prs,
		Guard:      guardrails.New([]string{repo}, maxPRs, dryRun, 1024, guardrails.NewRedactor("supersecret-token")),
		Log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	tools := map[string]llm.Tool{}
	for _, tl := range tb.Tools() {
		tools[tl.Name()] = tl
	}
	return tb, tools, prs, base
}

func call(t *testing.T, tl llm.Tool, args any) (string, error) {
	t.Helper()
	b, _ := json.Marshal(args)
	return tl.Call(context.Background(), b)
}

func TestReadListWrite(t *testing.T) {
	_, tools, _, _ := setup(t, true, 1)

	out, err := call(t, tools["repo_list"], map[string]any{"repo": repo, "recursive": true})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Dockerfile", "deploy/", "deploy/values.yaml"} {
		if !strings.Contains(out, want) {
			t.Errorf("repo_list missing %q in %q", want, out)
		}
	}
	if strings.Contains(out, ".git") {
		t.Errorf("repo_list must hide .git: %q", out)
	}

	out, err = call(t, tools["repo_read_file"], map[string]any{"repo": repo, "path": "Dockerfile"})
	if err != nil || out != "FROM alpine:3.18\n" {
		t.Fatalf("read: %q %v", out, err)
	}

	if _, err := call(t, tools["repo_write_file"], map[string]any{"repo": repo, "path": "new/dir/f.txt", "content": "x"}); err != nil {
		t.Fatal(err)
	}
	if out, _ := call(t, tools["repo_read_file"], map[string]any{"repo": repo, "path": "new/dir/f.txt"}); out != "x" {
		t.Fatalf("read back: %q", out)
	}
}

func TestGuards(t *testing.T) {
	_, tools, _, _ := setup(t, true, 1)
	cases := []map[string]any{
		{"repo": "evil/other", "path": "Dockerfile"},
		{"repo": repo, "path": "../../etc/passwd"},
		{"repo": repo, "path": ".git/config"},
	}
	for _, c := range cases {
		if _, err := call(t, tools["repo_read_file"], c); err == nil {
			t.Errorf("expected error for %v", c)
		}
	}
	if _, err := call(t, tools["repo_write_file"], map[string]any{"repo": repo, "path": "big", "content": strings.Repeat("a", 2000)}); err == nil {
		t.Error("expected size limit error")
	}
	if _, err := call(t, tools["repo_write_file"], map[string]any{"repo": repo, "path": ".git/hooks/post-checkout", "content": "x"}); err == nil {
		t.Error("expected .git write to be refused")
	}
}

func TestOpenPRPushesAndDeduplicates(t *testing.T) {
	tb, tools, prs, base := setup(t, false, 2)

	if _, err := call(t, tools["open_pr"], map[string]any{"repo": repo, "title": "t", "body": "b"}); err == nil {
		t.Fatal("open_pr without changes must fail")
	}
	if _, err := call(t, tools["repo_write_file"], map[string]any{"repo": repo, "path": "Dockerfile", "content": "FROM alpine:3.20\n"}); err != nil {
		t.Fatal(err)
	}
	pr := map[string]any{"repo": repo, "title": "Bump alpine", "body": "fixes CVE; token supersecret-token", "branch": "bump-alpine-3.20"}
	out, err := call(t, tools["open_pr"], pr)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "opened PR #1") {
		t.Fatalf("unexpected: %q", out)
	}
	branch := "heraion/trivy-remediate/bump-alpine-3-20"
	if got, err := scmtest.RemoteBranchFile(t, base, repo, branch, "Dockerfile"); err != nil || got != "FROM alpine:3.20\n" {
		t.Fatalf("remote branch content %q, %v", got, err)
	}
	in := prs.Created[0]
	if in.Head != branch || in.Base != "main" {
		t.Errorf("head/base = %s/%s", in.Head, in.Base)
	}
	if strings.Contains(in.Body, "supersecret-token") {
		t.Error("PR body must be redacted")
	}

	// After open_pr the clone is back on main.
	if out, _ := call(t, tools["repo_read_file"], map[string]any{"repo": repo, "path": "Dockerfile"}); out != "FROM alpine:3.18\n" {
		t.Errorf("workspace not reset: %q", out)
	}

	// Same change again: the open PR is reused and no budget is used.
	_, _ = call(t, tools["repo_write_file"], map[string]any{"repo": repo, "path": "Dockerfile", "content": "FROM alpine:3.20\n"})
	out, err = call(t, tools["open_pr"], pr)
	if err != nil || !strings.Contains(out, "already exists") {
		t.Fatalf("dedup: %q %v", out, err)
	}
	if len(prs.Created) != 1 || !tb.Opened[1].Duplicate {
		t.Fatalf("expected dedup, created=%d", len(prs.Created))
	}

	// maxPRs is 2: one more PR works, the next is refused.
	for i, want := range []bool{true, false} {
		_, _ = call(t, tools["repo_write_file"], map[string]any{"repo": repo, "path": "f", "content": "v" + string(rune('0'+i))})
		_, err := call(t, tools["open_pr"], map[string]any{"repo": repo, "title": "change " + string(rune('a'+i)), "body": "b"})
		if (err == nil) != want {
			t.Fatalf("PR %d: err=%v want ok=%v", i, err, want)
		}
	}
}

func TestOpenPRDryRun(t *testing.T) {
	tb, tools, prs, base := setup(t, true, 1)
	_, _ = call(t, tools["repo_write_file"], map[string]any{"repo": repo, "path": "Dockerfile", "content": "FROM alpine:3.20\n"})
	out, err := call(t, tools["open_pr"], map[string]any{"repo": repo, "title": "Bump", "body": "b"})
	if err != nil || !strings.Contains(out, "dry-run") {
		t.Fatalf("%q %v", out, err)
	}
	if len(prs.Created) != 0 || !tb.Opened[0].DryRun {
		t.Fatal("dry-run must not create PRs")
	}
	if _, err := scmtest.RemoteBranchFile(t, base, repo, "heraion/trivy-remediate/bump", "Dockerfile"); err == nil {
		t.Fatal("dry-run must not push")
	}
}

func TestBranchName(t *testing.T) {
	cases := map[string]string{
		"Bump OpenSSL to 3.0.15":      "heraion/pb/bump-openssl-to-3-0-15",
		"heraion/pb/already-prefixed": "heraion/pb/already-prefixed",
		"../../refs/heads/main":       "heraion/pb/refs-heads-main",
		"!!!":                         "heraion/pb/change",
	}
	for in, want := range cases {
		if got := scm.BranchName("pb", in); got != want {
			t.Errorf("BranchName(%q) = %q, want %q", in, got, want)
		}
	}
}
