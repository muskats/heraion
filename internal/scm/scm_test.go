package scm_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/muskats/heraion/internal/config"
	"github.com/muskats/heraion/internal/guardrails"
	"github.com/muskats/heraion/internal/llm"
	"github.com/muskats/heraion/internal/scm"
	"github.com/muskats/heraion/internal/scm/scmtest"
)

const repo = "acme/app"

func setup(t *testing.T, dryRun bool, maxPRs int) (*scm.Toolbox, map[string]llm.Tool, *scmtest.FakePRs, string) {
	t.Helper()
	return setupMode(t, dryRun, maxPRs, config.ModeFull)
}

func setupMode(t *testing.T, dryRun bool, maxPRs int, mode config.Mode) (*scm.Toolbox, map[string]llm.Tool, *scmtest.FakePRs, string) {
	t.Helper()
	base := t.TempDir()
	url := scmtest.NewRemote(t, base, repo, map[string]string{
		"Dockerfile":         "FROM alpine:3.18\n",
		"deploy/values.yaml": "image: app:1\n",
	})
	ws := scm.NewWorkspaces(config.Git{CloneBaseURL: url, AuthorName: "HeraIon", AuthorEmail: "h@x"}, t.TempDir(), nil)
	t.Cleanup(ws.Close)
	prs := &scmtest.FakePRs{}
	tb := &scm.Toolbox{
		Playbook:   "trivy-remediate",
		Workspaces: ws,
		PRs:        prs,
		Guard: guardrails.New(guardrails.Options{Repos: []string{repo}, MaxPRs: maxPRs, DryRun: dryRun, MaxFileBytes: 1024,
			Redactor: guardrails.NewRedactor("supersecret-token"), Mode: mode}),
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
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
	if !strings.Contains(in.Body, "HeraIon never merges") || strings.Contains(in.Body, "Mode: safe") {
		t.Errorf("full mode footer: %q", in.Body)
	}
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
	if len(prs.Created) != 1 || tb.Opened[1].Outcome != scm.OutcomeDuplicate {
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
	if len(prs.Created) != 0 || tb.Opened[0].Outcome != scm.OutcomeDryRun {
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

func TestCIFilesAreNeverWritten(t *testing.T) {
	_, tools, _, _ := setup(t, true, 1)
	for _, p := range []string{".github/workflows/ci.yml", ".GITHUB/workflows/x.yml", "CODEOWNERS", ".gitlab-ci.yml", "sub/.git/config"} {
		if _, err := call(t, tools["repo_write_file"], map[string]any{"repo": repo, "path": p, "content": "x"}); err == nil {
			t.Errorf("write to %s must be refused", p)
		}
	}
}

func TestSymlinkIntoWorkflowsIsCaughtAtOpenPR(t *testing.T) {
	tb, tools, prs, _ := setup(t, false, 1)
	// Clone, then plant a symlink as if the repo contained one.
	if _, err := call(t, tools["repo_list"], map[string]any{"repo": repo}); err != nil {
		t.Fatal(err)
	}
	r, _ := tb.Workspaces.Get(context.Background(), repo)
	if err := os.MkdirAll(filepath.Join(r.Dir, ".github", "workflows"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(".github/workflows", filepath.Join(r.Dir, "link")); err != nil {
		t.Fatal(err)
	}
	if _, err := call(t, tools["repo_write_file"], map[string]any{"repo": repo, "path": "link/evil.yml", "content": "on: pull_request"}); err != nil {
		t.Fatal(err) // allowed by name; the file lands in .github/workflows
	}
	_, err := call(t, tools["open_pr"], map[string]any{"repo": repo, "title": "t", "body": "b"})
	if err == nil || !strings.Contains(err.Error(), ".github/workflows/evil.yml") {
		t.Fatalf("open_pr must refuse the workflow change, got %v", err)
	}
	if len(prs.Created) != 0 {
		t.Fatal("no PR may be created")
	}
}

func TestClosedPRCooldown(t *testing.T) {
	branch := "heraion/trivy-remediate/bump"
	for _, tc := range []struct {
		name        string
		state       string
		closedAgo   time.Duration
		reopenAfter time.Duration
		wantOpened  bool
	}{
		{"closed in window", scm.PRStateClosed, time.Hour, 7 * 24 * time.Hour, false},
		{"closed after window", scm.PRStateClosed, 8 * 24 * time.Hour, 7 * 24 * time.Hour, true},
		{"never reopen", scm.PRStateClosed, 365 * 24 * time.Hour, 0, false},
		{"merged does not block", scm.PRStateMerged, time.Hour, 7 * 24 * time.Hour, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tb, tools, prs, _ := setup(t, false, 1)
			tb.ReopenAfter = tc.reopenAfter
			prs.Closed = map[string][]scm.PR{repo + " " + branch: {{Number: 9, URL: "https://git.example/pull/9", State: tc.state, Closed: time.Now().Add(-tc.closedAgo)}}}
			_, _ = call(t, tools["repo_write_file"], map[string]any{"repo": repo, "path": "Dockerfile", "content": "FROM alpine:3.20\n"})
			out, err := call(t, tools["open_pr"], map[string]any{"repo": repo, "title": "t", "body": "b", "branch": "bump"})
			if err != nil {
				t.Fatal(err)
			}
			if opened := len(prs.Created) == 1; opened != tc.wantOpened {
				t.Fatalf("opened=%v want %v: %s", opened, tc.wantOpened, out)
			}
			if !tc.wantOpened && (tb.Opened[0].Outcome != scm.OutcomeRejected || !strings.Contains(out, "closed without merging")) {
				t.Fatalf("rejection not reported: %q %+v", out, tb.Opened)
			}
		})
	}
}

func TestOpenPRRefusesSecretInChange(t *testing.T) {
	tb, tools, prs, _ := setup(t, false, 1)
	_, _ = call(t, tools["repo_write_file"], map[string]any{"repo": repo, "path": "deploy/values.yaml", "content": "image: app:1\ntoken: supersecret-token\n"})
	_, err := call(t, tools["open_pr"], map[string]any{"repo": repo, "title": "t", "body": "b"})
	if err == nil || strings.Contains(err.Error(), "supersecret-token") {
		t.Fatalf("want a refusal that does not echo the secret, got %v", err)
	}
	if len(prs.Created) != 0 {
		t.Fatal("no PR may be created")
	}
	// The budget is returned.
	if err := tb.Guard.ReservePR(); err != nil {
		t.Fatalf("budget not released: %v", err)
	}
}

func TestCloneURLResolver(t *testing.T) {
	base := t.TempDir()
	url := scmtest.NewRemote(t, base, "PROJ/app", map[string]string{"a": "b"})
	ws := scm.NewWorkspaces(config.Git{CloneBaseURL: "https://wrong.example"}, t.TempDir(),
		func(_ context.Context, repo string) (string, error) {
			return url + "/" + repo + ".git", nil // e.g. https://bitbucket/scm/PROJ/app.git
		})
	t.Cleanup(ws.Close)
	r, err := ws.Get(context.Background(), "PROJ/app")
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := r.ReadFile("a", 10); got != "b" {
		t.Fatalf("got %q", got)
	}
}

func TestSameBranchKeyTwiceInOneRun(t *testing.T) {
	_, tools, _, _ := setup(t, true, 2)
	for i := range 2 {
		_, _ = call(t, tools["repo_write_file"], map[string]any{"repo": repo, "path": "Dockerfile", "content": fmt.Sprintf("FROM alpine:3.2%d\n", i)})
		if _, err := call(t, tools["open_pr"], map[string]any{"repo": repo, "title": "t", "body": "b", "branch": "bump"}); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
}

func TestYoloPushesToBase(t *testing.T) {
	tb, tools, prs, base := setupMode(t, false, 1, config.ModeYolo)
	if !strings.Contains(tools["open_pr"].Description(), "default branch") {
		t.Errorf("open_pr description: %q", tools["open_pr"].Description())
	}
	_, _ = call(t, tools["repo_write_file"], map[string]any{"repo": repo, "path": "Dockerfile", "content": "FROM alpine:3.20\n"})
	out, err := call(t, tools["open_pr"], map[string]any{"repo": repo, "title": "Bump alpine", "body": "b", "branch": "bump"})
	if err != nil || !strings.Contains(out, "pushed commit") {
		t.Fatalf("%q %v", out, err)
	}
	if got, err := scmtest.RemoteBranchFile(t, base, repo, "main", "Dockerfile"); err != nil || got != "FROM alpine:3.20\n" {
		t.Fatalf("main: %q %v", got, err)
	}
	if _, err := scmtest.RemoteBranchFile(t, base, repo, "heraion/trivy-remediate/bump", "Dockerfile"); err == nil {
		t.Error("yolo must not push a feature branch")
	}
	if len(prs.Created) != 0 || tb.Opened[0].Outcome != scm.OutcomePushed || tb.Opened[0].Branch != "main" {
		t.Fatalf("created=%d opened=%+v", len(prs.Created), tb.Opened)
	}
}

func TestYoloFallsBackToPR(t *testing.T) {
	tb, tools, prs, base := setupMode(t, false, 1, config.ModeYolo)
	_, _ = call(t, tools["repo_write_file"], map[string]any{"repo": repo, "path": "Dockerfile", "content": "FROM alpine:3.20\n"})
	// main moves after the clone, so the push is not a fast-forward.
	scmtest.AdvanceRemote(t, base, repo, map[string]string{"README.md": "hi\n"})
	out, err := call(t, tools["open_pr"], map[string]any{"repo": repo, "title": "Bump alpine", "body": "b", "branch": "bump"})
	if err != nil || !strings.Contains(out, "PR #1 was opened instead") {
		t.Fatalf("%q %v", out, err)
	}
	if got, _ := scmtest.RemoteBranchFile(t, base, repo, "main", "Dockerfile"); got != "FROM alpine:3.18\n" {
		t.Errorf("main must be unchanged: %q", got)
	}
	if len(prs.Created) != 1 || prs.Created[0].Head != "heraion/trivy-remediate/bump" || tb.Opened[0].Outcome != scm.OutcomeFellBack {
		t.Fatalf("created=%+v opened=%+v", prs.Created, tb.Opened)
	}
}

func TestYoloDryRun(t *testing.T) {
	tb, tools, _, base := setupMode(t, true, 1, config.ModeYolo)
	_, _ = call(t, tools["repo_write_file"], map[string]any{"repo": repo, "path": "Dockerfile", "content": "FROM alpine:3.20\n"})
	out, err := call(t, tools["open_pr"], map[string]any{"repo": repo, "title": "Bump", "body": "b"})
	if err != nil || !strings.Contains(out, "not pushed to main") {
		t.Fatalf("%q %v", out, err)
	}
	if got, _ := scmtest.RemoteBranchFile(t, base, repo, "main", "Dockerfile"); got != "FROM alpine:3.18\n" || tb.Opened[0].Outcome != scm.OutcomeDryRun {
		t.Fatalf("dry-run must not push: %q", got)
	}
}

func TestSafeModeRefusesBreakingChange(t *testing.T) {
	tb, tools, prs, _ := setupMode(t, false, 1, config.ModeSafe)
	if !strings.Contains(tools["open_pr"].Description(), "non-breaking") {
		t.Errorf("open_pr description: %q", tools["open_pr"].Description())
	}
	if _, err := call(t, tools["repo_write_file"], map[string]any{"repo": repo, "path": "main.go", "content": "package main\n"}); err == nil {
		t.Error("safe mode must refuse writing main.go")
	}
	_, _ = call(t, tools["repo_write_file"], map[string]any{"repo": repo, "path": "Dockerfile", "content": "FROM alpine:4.0\n"})
	if _, err := call(t, tools["open_pr"], map[string]any{"repo": repo, "title": "Major", "body": "b"}); err == nil ||
		!strings.Contains(err.Error(), "major version") {
		t.Fatalf("major bump: %v", err)
	}
	if out, _ := call(t, tools["repo_read_file"], map[string]any{"repo": repo, "path": "Dockerfile"}); out != "FROM alpine:3.18\n" {
		t.Errorf("workspace not reset: %q", out)
	}
	// The refused change used no budget.
	_, _ = call(t, tools["repo_write_file"], map[string]any{"repo": repo, "path": "Dockerfile", "content": "FROM alpine:3.20\n"})
	out, err := call(t, tools["open_pr"], map[string]any{"repo": repo, "title": "Minor", "body": "b"})
	if err != nil || !strings.Contains(out, "opened PR #1") {
		t.Fatalf("%q %v", out, err)
	}
	if len(prs.Created) != 1 || !strings.Contains(prs.Created[0].Body, "Mode: safe") || len(tb.Opened) != 1 {
		t.Fatalf("created=%+v", prs.Created)
	}
}

func TestYoloFallbackReusesOpenPR(t *testing.T) {
	tb, tools, prs, base := setupMode(t, false, 1, config.ModeYolo)
	branch := "heraion/trivy-remediate/bump"
	prs.Open = map[string]*scm.PR{repo + " " + branch: {Number: 7, URL: "https://git.example/pr/7"}}
	_, _ = call(t, tools["repo_write_file"], map[string]any{"repo": repo, "path": "Dockerfile", "content": "FROM alpine:3.20\n"})
	scmtest.AdvanceRemote(t, base, repo, map[string]string{"README.md": "hi\n"})
	out, err := call(t, tools["open_pr"], map[string]any{"repo": repo, "title": "Bump alpine", "body": "b", "branch": "bump"})
	if err != nil || !strings.Contains(out, "already exists") {
		t.Fatalf("%q %v", out, err)
	}
	if len(prs.Created) != 0 || len(tb.Opened) != 1 || tb.Opened[0].Outcome != scm.OutcomeDuplicate {
		t.Fatalf("created=%+v opened=%+v", prs.Created, tb.Opened)
	}
	if _, err := scmtest.RemoteBranchFile(t, base, repo, branch, "Dockerfile"); err == nil {
		t.Error("the branch of the open PR must not be pushed again")
	}
	if err := tb.Guard.ReservePR(); err != nil {
		t.Errorf("budget must be released: %v", err)
	}
}

func TestYoloCancelledPushDoesNotFallBack(t *testing.T) {
	tb, tools, prs, base := setupMode(t, false, 1, config.ModeYolo)
	_, _ = call(t, tools["repo_write_file"], map[string]any{"repo": repo, "path": "Dockerfile", "content": "FROM alpine:3.20\n"})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	b, _ := json.Marshal(map[string]any{"repo": repo, "title": "Bump alpine", "body": "b", "branch": "bump"})
	if _, err := tools["open_pr"].Call(ctx, b); err == nil {
		t.Fatal("a cancelled push must fail")
	}
	if len(prs.Created) != 0 || len(tb.Opened) != 0 {
		t.Fatalf("no PR on cancel: created=%+v opened=%+v", prs.Created, tb.Opened)
	}
	if got, _ := scmtest.RemoteBranchFile(t, base, repo, "main", "Dockerfile"); got != "FROM alpine:3.18\n" {
		t.Errorf("main must be unchanged: %q", got)
	}
	if err := tb.Guard.ReservePR(); err != nil {
		t.Errorf("budget must be released: %v", err)
	}
}

type failingPRs struct{ scmtest.FakePRs }

func (*failingPRs) FindByBranch(context.Context, string, string) ([]scm.PR, error) {
	return nil, fmt.Errorf("server unavailable")
}

func TestOpenPRLookupErrorStopsPR(t *testing.T) {
	tb, tools, _, base := setup(t, false, 1)
	prs := &failingPRs{}
	tb.PRs = prs
	_, _ = call(t, tools["repo_write_file"], map[string]any{"repo": repo, "path": "Dockerfile", "content": "FROM alpine:3.20\n"})
	if _, err := call(t, tools["open_pr"], map[string]any{"repo": repo, "title": "Bump", "body": "b", "branch": "bump"}); err == nil ||
		!strings.Contains(err.Error(), "server unavailable") {
		t.Fatalf("lookup error: %v", err)
	}
	if len(prs.Created) != 0 {
		t.Error("no PR without a successful lookup")
	}
	if _, err := scmtest.RemoteBranchFile(t, base, repo, "heraion/trivy-remediate/bump", "Dockerfile"); err == nil {
		t.Error("nothing may be pushed")
	}
	if out, _ := call(t, tools["repo_read_file"], map[string]any{"repo": repo, "path": "Dockerfile"}); out != "FROM alpine:3.18\n" {
		t.Errorf("workspace not reset: %q", out)
	}
}

func TestPushToBase(t *testing.T) {
	base := t.TempDir()
	url := scmtest.NewRemote(t, base, repo, map[string]string{"Dockerfile": "FROM alpine:3.18\n"})
	ws := scm.NewWorkspaces(config.Git{CloneBaseURL: url, AuthorName: "HeraIon", AuthorEmail: "h@x"}, t.TempDir(), nil)
	t.Cleanup(ws.Close)
	r, err := ws.Get(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.PushToBase(context.Background(), "main"); err == nil {
		t.Error("only heraion/ branches may be pushed")
	}
	if err := r.WriteFile("Dockerfile", []byte("FROM alpine:3.20\n")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.Commit("heraion/x/bump", "bump"); err != nil {
		t.Fatal(err)
	}
	for i := range 2 { // the second push has nothing new and succeeds
		if err := r.PushToBase(context.Background(), "heraion/x/bump"); err != nil {
			t.Fatalf("push %d: %v", i, err)
		}
	}
	if got, _ := scmtest.RemoteBranchFile(t, base, repo, "main", "Dockerfile"); got != "FROM alpine:3.20\n" {
		t.Errorf("main: %q", got)
	}
}
