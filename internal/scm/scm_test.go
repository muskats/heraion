package scm_test

import (
	"context"
	"encoding/json"
	"errors"
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
	"github.com/muskats/heraion/internal/secret"
)

var dockerfileBranch = scm.BranchForPaths("trivy-remediate", repo, []string{"Dockerfile"})

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
	pr := map[string]any{"repo": repo, "title": "Bump alpine", "body": "fixes CVE; token supersecret-token"}
	out, err := call(t, tools["open_pr"], pr)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "opened PR #1") {
		t.Fatalf("unexpected: %q", out)
	}
	branch := dockerfileBranch
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

	if scm.ChangeOf(in.Body) == "" {
		t.Errorf("no change marker in the body: %q", in.Body)
	}

	// Same change again, worded differently: same branch, nothing pushed, no budget used.
	_, _ = call(t, tools["repo_write_file"], map[string]any{"repo": repo, "path": "Dockerfile", "content": "FROM alpine:3.20\n"})
	out, err = call(t, tools["open_pr"], map[string]any{"repo": repo, "title": "Update alpine image", "body": "other words"})
	if err != nil || !strings.Contains(out, "already has exactly this change") {
		t.Fatalf("dedup: %q %v", out, err)
	}
	if len(prs.Created) != 1 || len(prs.Updated) != 0 || tb.Opened[1].Outcome != scm.OutcomeUnchanged {
		t.Fatalf("expected unchanged, created=%d updated=%d", len(prs.Created), len(prs.Updated))
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
	if _, err := scmtest.RemoteBranchFile(t, base, repo, dockerfileBranch, "Dockerfile"); err == nil {
		t.Fatal("dry-run must not push")
	}
}

func TestBranchForPaths(t *testing.T) {
	a := scm.BranchForPaths("pb", "acme/web", []string{"deploy/values.yaml", "Dockerfile"})
	if a != scm.BranchForPaths("pb", "acme/web", []string{"Dockerfile", "deploy/values.yaml"}) {
		t.Error("the order of paths must not matter")
	}
	if !strings.HasPrefix(a, "heraion/pb/dockerfile-") || len(a) != len("heraion/pb/dockerfile-")+8 {
		t.Errorf("branch %q", a)
	}
	for _, other := range []string{
		scm.BranchForPaths("pb", "acme/api", []string{"Dockerfile", "deploy/values.yaml"}),
		scm.BranchForPaths("pb", "acme/web", []string{"Dockerfile"}),
	} {
		if other == a {
			t.Errorf("%q: another repo or path set must give another branch", other)
		}
	}
	long := scm.BranchForPaths("pb", "acme/web", []string{"../../" + strings.Repeat("x", 80) + "/values.yaml"})
	if strings.Contains(long, "..") || len(long) > len("heraion/pb/")+30+1+8 {
		t.Errorf("long path: %q", long)
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

func TestSymlinksAreRefused(t *testing.T) {
	tb, tools, _, _ := setup(t, false, 1)
	// Clone, then plant symlinks as if the repo contained them.
	if _, err := call(t, tools["repo_list"], map[string]any{"repo": repo}); err != nil {
		t.Fatal(err)
	}
	r, _ := tb.Workspaces.Get(context.Background(), repo)
	if err := os.MkdirAll(filepath.Join(r.Dir, ".github", "workflows"), 0o755); err != nil {
		t.Fatal(err)
	}
	for link, target := range map[string]string{"link": ".github/workflows", "notes.txt": ".git/config"} {
		if err := os.Symlink(target, filepath.Join(r.Dir, link)); err != nil {
			t.Fatal(err)
		}
	}
	for _, p := range []string{"link/evil.yml", "notes.txt"} {
		_, err := call(t, tools["repo_write_file"], map[string]any{"repo": repo, "path": p, "content": "x"})
		if err == nil || !strings.Contains(err.Error(), "is a symlink") {
			t.Errorf("write to %s: want a symlink refusal, got %v", p, err)
		}
	}
	if cfg, _ := os.ReadFile(filepath.Join(r.Dir, ".git", "config")); string(cfg) == "x" {
		t.Fatal(".git/config was overwritten")
	}
	// Reads refuse links too, and name the target.
	_, err := call(t, tools["repo_read_file"], map[string]any{"repo": repo, "path": "notes.txt"})
	if err == nil || !strings.Contains(err.Error(), "notes.txt is a symlink to .git/config") {
		t.Errorf("read through a symlink: got %v", err)
	}
}

// A denied file that changed in the clone without repo_write_file, e.g.
// through a symlink, stops the PR.
func TestOpenPRRechecksChangedPaths(t *testing.T) {
	tb, tools, prs, _ := setup(t, false, 1)
	_, _ = call(t, tools["repo_write_file"], map[string]any{"repo": repo, "path": "Dockerfile", "content": "FROM alpine:3.20\n"})
	r, _ := tb.Workspaces.Get(context.Background(), repo)
	if err := os.MkdirAll(filepath.Join(r.Dir, "web"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(r.Dir, "web", ".npmrc"), []byte("registry=https://evil.example/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := call(t, tools["open_pr"], map[string]any{"repo": repo, "title": "Bump", "body": "b"})
	if err == nil || !strings.Contains(err.Error(), "writing web/.npmrc is not allowed") || len(prs.Created) != 0 {
		t.Fatalf("got %v, created %d", err, len(prs.Created))
	}
}

// closeOpenPR moves the open PR of branch to the closed (or merged) list,
// as a reviewer would.
func closeOpenPR(prs *scmtest.FakePRs, branch, state string, ago time.Duration) {
	key := repo + " " + branch
	pr := *prs.Open[key]
	delete(prs.Open, key)
	pr.State, pr.Closed = state, time.Now().Add(-ago)
	if prs.Closed == nil {
		prs.Closed = map[string][]scm.PR{}
	}
	prs.Closed[key] = append(prs.Closed[key], pr)
}

func TestClosedPRCooldown(t *testing.T) {
	for _, tc := range []struct {
		name        string
		state       string
		closedAgo   time.Duration
		reopenAfter time.Duration
		again       string // the Dockerfile of the second proposal
		wantOpened  bool
	}{
		{"same change, never again by default", scm.PRStateClosed, 365 * 24 * time.Hour, 0, "FROM alpine:3.20\n", false},
		{"same change in window", scm.PRStateClosed, time.Hour, 7 * 24 * time.Hour, "FROM alpine:3.20\n", false},
		{"same change after window", scm.PRStateClosed, 8 * 24 * time.Hour, 7 * 24 * time.Hour, "FROM alpine:3.20\n", true},
		{"other change on the same file", scm.PRStateClosed, time.Hour, 0, "FROM alpine:3.21\n", true},
		{"merged does not block", scm.PRStateMerged, time.Hour, 0, "FROM alpine:3.20\n", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tb, tools, prs, _ := setup(t, false, 2)
			tb.ReopenAfter = tc.reopenAfter
			_, _ = call(t, tools["repo_write_file"], map[string]any{"repo": repo, "path": "Dockerfile", "content": "FROM alpine:3.20\n"})
			if _, err := call(t, tools["open_pr"], map[string]any{"repo": repo, "title": "t", "body": "b"}); err != nil {
				t.Fatal(err)
			}
			closeOpenPR(prs, dockerfileBranch, tc.state, tc.closedAgo)
			_, _ = call(t, tools["repo_write_file"], map[string]any{"repo": repo, "path": "Dockerfile", "content": tc.again})
			out, err := call(t, tools["open_pr"], map[string]any{"repo": repo, "title": "other title", "body": "b"})
			if err != nil {
				t.Fatal(err)
			}
			if opened := len(prs.Created) == 2; opened != tc.wantOpened {
				t.Fatalf("opened=%v want %v: %s", opened, tc.wantOpened, out)
			}
			if !tc.wantOpened && (tb.Opened[1].Outcome != scm.OutcomeRejected || !strings.Contains(out, "closed without merging")) {
				t.Fatalf("rejection not reported: %q %+v", out, tb.Opened)
			}
		})
	}
}

// A new change on the same files updates the open PR: force-push, new body.
func TestOpenPRUpdatesOpenPR(t *testing.T) {
	tb, tools, prs, base := setup(t, false, 2)
	for _, v := range []string{"3.20", "3.21"} {
		_, _ = call(t, tools["repo_write_file"], map[string]any{"repo": repo, "path": "Dockerfile", "content": "FROM alpine:" + v + "\n"})
		if _, err := call(t, tools["open_pr"], map[string]any{"repo": repo, "title": "Bump alpine to " + v, "body": "b"}); err != nil {
			t.Fatal(err)
		}
	}
	if len(prs.Created) != 1 || len(prs.Updated) != 1 || tb.Opened[1].Outcome != scm.OutcomeUpdated || tb.Opened[1].Number != 1 {
		t.Fatalf("created=%d updated=%d opened=%+v", len(prs.Created), len(prs.Updated), tb.Opened)
	}
	if got, _ := scmtest.RemoteBranchFile(t, base, repo, dockerfileBranch, "Dockerfile"); got != "FROM alpine:3.21\n" {
		t.Errorf("branch not updated: %q", got)
	}
	if u := prs.Updated[0]; u.Title != "Bump alpine to 3.21" || scm.ChangeOf(u.Body) == scm.ChangeOf(prs.Created[0].Body) {
		t.Errorf("update %+v", u)
	}
}

// When the PR body cannot be updated (e.g. Gogs), the branch's commit
// trailer still names the change, so a rerun with the same change pushes
// nothing.
func TestOpenPRUnchangedWhenBodyUpdateFailed(t *testing.T) {
	tb, tools, prs, base := setup(t, false, 3)
	open := func(v string) string {
		t.Helper()
		_, _ = call(t, tools["repo_write_file"], map[string]any{"repo": repo, "path": "Dockerfile", "content": "FROM alpine:" + v + "\n"})
		out, err := call(t, tools["open_pr"], map[string]any{"repo": repo, "title": "Bump alpine to " + v, "body": "b"})
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	open("3.20")
	prs.UpdateErr = errors.New("not supported")
	if out := open("3.21"); !strings.Contains(out, "could not be refreshed") {
		t.Fatalf("second call: %q", out)
	}
	if out := open("3.21"); !strings.Contains(out, "already has exactly this change") {
		t.Fatalf("third call: %q", out)
	}
	if got := tb.Opened[2].Outcome; got != scm.OutcomeUnchanged {
		t.Errorf("outcome %s, want unchanged", got)
	}
	// The body still names 3.20, but the branch holds 3.21: going back to
	// 3.20 is a new change and is pushed.
	if out := open("3.20"); !strings.Contains(out, "updated PR") {
		t.Fatalf("fourth call: %q", out)
	}
	if got, _ := scmtest.RemoteBranchFile(t, base, repo, dockerfileBranch, "Dockerfile"); got != "FROM alpine:3.20\n" {
		t.Errorf("branch: %q", got)
	}
}

// A commit by someone else on the PR branch is never force-pushed over.
func TestOpenPRLeavesHumanCommitsAlone(t *testing.T) {
	tb, tools, prs, base := setup(t, false, 2)
	_, _ = call(t, tools["repo_write_file"], map[string]any{"repo": repo, "path": "Dockerfile", "content": "FROM alpine:3.20\n"})
	if _, err := call(t, tools["open_pr"], map[string]any{"repo": repo, "title": "t", "body": "b"}); err != nil {
		t.Fatal(err)
	}
	scmtest.CommitToBranch(t, base, repo, dockerfileBranch, "reviewer@example.com", map[string]string{"Dockerfile": "FROM alpine:3.20\nUSER app\n"})
	_, _ = call(t, tools["repo_write_file"], map[string]any{"repo": repo, "path": "Dockerfile", "content": "FROM alpine:3.21\n"})
	out, err := call(t, tools["open_pr"], map[string]any{"repo": repo, "title": "t", "body": "b"})
	if err != nil || !strings.Contains(out, "commits by someone else") {
		t.Fatalf("%q %v", out, err)
	}
	if len(prs.Updated) != 0 || tb.Opened[1].Outcome != scm.OutcomeSkipped {
		t.Fatalf("updated=%d opened=%+v", len(prs.Updated), tb.Opened)
	}
	if got, _ := scmtest.RemoteBranchFile(t, base, repo, dockerfileBranch, "Dockerfile"); got != "FROM alpine:3.20\nUSER app\n" {
		t.Errorf("the reviewer's commit was overwritten: %q", got)
	}
	if err := tb.Guard.ReservePR(); err != nil {
		t.Errorf("a skipped PR must not use budget: %v", err)
	}
}

func TestOpenPRRefusesSecretInChange(t *testing.T) {
	tb, tools, prs, _ := setup(t, false, 1)
	_, _ = call(t, tools["repo_write_file"], map[string]any{"repo": repo, "path": "deploy/values.yaml", "content": "image: app:1\ntoken: supersecret-token\n"})
	_, err := call(t, tools["open_pr"], map[string]any{"repo": repo, "title": "t", "body": "b"})
	if err == nil || strings.Contains(err.Error(), "supersecret-token") {
		t.Fatalf("want a refusal that does not echo the secret, got %v", err)
	}
	if !strings.Contains(err.Error(), "discarded") {
		t.Errorf("the refusal must say the edits were discarded: %v", err)
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

func TestSamePathsTwiceInOneRun(t *testing.T) {
	_, tools, _, _ := setup(t, true, 2)
	for i := range 2 {
		_, _ = call(t, tools["repo_write_file"], map[string]any{"repo": repo, "path": "Dockerfile", "content": fmt.Sprintf("FROM alpine:3.2%d\n", i)})
		if _, err := call(t, tools["open_pr"], map[string]any{"repo": repo, "title": "t", "body": "b"}); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
}

func TestSafeModeRefusesBreakingChange(t *testing.T) {
	tb, tools, prs, _ := setupMode(t, false, 1, config.ModeSafe)
	if d := tools["open_pr"].Description(); !strings.Contains(d, "only versions and image digests") || strings.Contains(d, "non-breaking") {
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
	_, _ = call(t, tools["repo_write_file"], map[string]any{"repo": repo, "path": "Dockerfile", "content": "FROM alpine:3.18.4\n"})
	out, err := call(t, tools["open_pr"], map[string]any{"repo": repo, "title": "Patch", "body": "b"})
	if err != nil || !strings.Contains(out, "opened PR #1") {
		t.Fatalf("%q %v", out, err)
	}
	if len(prs.Created) != 1 || !strings.Contains(prs.Created[0].Body, "Mode: safe") || len(tb.Opened) != 1 {
		t.Fatalf("created=%+v", prs.Created)
	}
}

// A push that fails opens no PR, records nothing and returns the budget.
func TestFailedPushReleasesBudget(t *testing.T) {
	tb, tools, prs, base := setup(t, false, 1)
	_, _ = call(t, tools["repo_write_file"], map[string]any{"repo": repo, "path": "Dockerfile", "content": "FROM alpine:3.20\n"})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	b, _ := json.Marshal(map[string]any{"repo": repo, "title": "Bump alpine", "body": "b"})
	if _, err := tools["open_pr"].Call(ctx, b); err == nil {
		t.Fatal("a cancelled push must fail")
	}
	if len(prs.Created) != 0 || len(tb.Opened) != 0 {
		t.Fatalf("created=%+v opened=%+v", prs.Created, tb.Opened)
	}
	if _, err := scmtest.RemoteBranchFile(t, base, repo, dockerfileBranch, "Dockerfile"); err == nil {
		t.Error("nothing may be pushed")
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
	if _, err := call(t, tools["open_pr"], map[string]any{"repo": repo, "title": "Bump", "body": "b"}); err == nil ||
		!strings.Contains(err.Error(), "server unavailable") {
		t.Fatalf("lookup error: %v", err)
	}
	if len(prs.Created) != 0 {
		t.Error("no PR without a successful lookup")
	}
	if _, err := scmtest.RemoteBranchFile(t, base, repo, dockerfileBranch, "Dockerfile"); err == nil {
		t.Error("nothing may be pushed")
	}
	if out, _ := call(t, tools["repo_read_file"], map[string]any{"repo": repo, "path": "Dockerfile"}); out != "FROM alpine:3.18\n" {
		t.Errorf("workspace not reset: %q", out)
	}
}

func TestBrokenTokenFileIsNotAnonymous(t *testing.T) {
	base := t.TempDir()
	url := scmtest.NewRemote(t, base, repo, map[string]string{"a": "b"})
	tok := secret.FromMap("HERAION_GIT_TOKEN", map[string]string{"HERAION_GIT_TOKEN_FILE": "/nonexistent/token"})
	ws := scm.NewWorkspaces(config.Git{CloneBaseURL: url, Token: tok}, t.TempDir(), nil)
	t.Cleanup(ws.Close)
	if _, err := ws.Get(context.Background(), repo); err == nil || !strings.Contains(err.Error(), "git token: HERAION_GIT_TOKEN_FILE") {
		t.Fatalf("a set but unreadable token must fail the clone, got %v", err)
	}
}

func TestPushOnlyHeraionBranches(t *testing.T) {
	base := t.TempDir()
	url := scmtest.NewRemote(t, base, repo, map[string]string{"Dockerfile": "FROM alpine:3.18\n"})
	ws := scm.NewWorkspaces(config.Git{CloneBaseURL: url, AuthorName: "HeraIon", AuthorEmail: "h@x"}, t.TempDir(), nil)
	t.Cleanup(ws.Close)
	r, err := ws.Get(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Push(context.Background(), "main"); err == nil {
		t.Error("only heraion/ branches may be pushed")
	}
	if err := r.WriteFile("Dockerfile", []byte("FROM alpine:3.20\n")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.Commit("heraion/x/bump", "bump"); err != nil {
		t.Fatal(err)
	}
	for i := range 2 { // the second push has nothing new and succeeds
		if err := r.Push(context.Background(), "heraion/x/bump"); err != nil {
			t.Fatalf("push %d: %v", i, err)
		}
	}
	if got, _ := scmtest.RemoteBranchFile(t, base, repo, "heraion/x/bump", "Dockerfile"); got != "FROM alpine:3.20\n" {
		t.Errorf("pushed branch: %q", got)
	}
	if got, _ := scmtest.RemoteBranchFile(t, base, repo, "main", "Dockerfile"); got != "FROM alpine:3.18\n" {
		t.Errorf("main must be unchanged: %q", got)
	}
}
