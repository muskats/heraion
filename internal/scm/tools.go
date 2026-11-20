package scm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/muskats/heraion/internal/config"
	"github.com/muskats/heraion/internal/guardrails"
	"github.com/muskats/heraion/internal/llm"
	"github.com/muskats/heraion/internal/textutil"
)

// Toolbox holds what the built-in Git tools share during one run.
type Toolbox struct {
	Playbook   string
	Workspaces *Workspaces
	PRs        PRService // may be nil when dry-run
	Guard      *guardrails.Guard
	Log        *slog.Logger
	// ReopenAfter is how long a PR closed without merging blocks a new PR
	// on the same branch. 0: forever.
	ReopenAfter time.Duration
	// Opened collects PRs opened (or found as duplicates) during the run.
	Opened []OpenedPR
}

// OpenedPR records the outcome of one open_pr call.
type OpenedPR struct {
	Repo, Branch, URL string
	Outcome           Outcome
}

// Outcome is what one open_pr call did.
type Outcome string

// Outcomes.
const (
	OutcomeOpened    Outcome = "opened"    // a new PR
	OutcomeDuplicate Outcome = "duplicate" // an open PR for the branch exists
	OutcomeRejected  Outcome = "rejected"  // a PR for the branch was recently closed without merging
	OutcomeDryRun    Outcome = "dry-run"   // logged, not pushed
	OutcomePushed    Outcome = "pushed"    // yolo: pushed to the base branch, no PR
	OutcomeFellBack  Outcome = "fell-back" // yolo: the push was refused, a PR was opened instead
)

// Limits of repo_read_file (bytes) and repo_list (entries).
const (
	readLimit      = 256 << 10
	maxListEntries = 2000
)

// Tools returns the built-in tools.
func (tb *Toolbox) Tools() []llm.Tool {
	repos := tb.Guard.Repos()
	if repos == nil {
		repos = []string{} // "enum": null is not a valid schema
	}
	repoEnum, _ := json.Marshal(repos)
	repoProp := fmt.Sprintf(`"repo":{"type":"string","enum":%s,"description":"Repository as owner/name"}`, repoEnum)
	return []llm.Tool{
		&llm.FuncTool{
			ToolName: "repo_list",
			Desc:     "List files and directories in a repository. Directories end in '/'.",
			Params: `{"type":"object","properties":{` + repoProp + `,
				"path":{"type":"string","description":"Directory relative to the repo root; default '.'"},
				"recursive":{"type":"boolean","description":"Walk subdirectories (capped at ` + strconv.Itoa(maxListEntries) + ` entries)"}},
				"required":["repo"]}`,
			Fn: tb.list,
		},
		&llm.FuncTool{
			ToolName: "repo_read_file",
			Desc:     "Read a text file from a repository.",
			Params: `{"type":"object","properties":{` + repoProp + `,
				"path":{"type":"string","description":"File path relative to the repo root"}},
				"required":["repo","path"]}`,
			Fn: tb.read,
		},
		&llm.FuncTool{
			ToolName: "repo_write_file",
			Desc:     "Create or overwrite a file in the local clone. Changes are only proposed through open_pr.",
			Params: `{"type":"object","properties":{` + repoProp + `,
				"path":{"type":"string","description":"File path relative to the repo root"},
				"content":{"type":"string","description":"Full new file content"}},
				"required":["repo","path","content"]}`,
			Fn: tb.write,
		},
		&llm.FuncTool{
			ToolName: "open_pr",
			Desc:     openPRDesc[tb.Guard.Mode()],
			Params: `{"type":"object","properties":{` + repoProp + `,
				"title":{"type":"string","description":"Short PR title"},
				"body":{"type":"string","description":"Markdown description: problem, evidence, change, risk"},
				"branch":{"type":"string","description":"Stable short key for this change, e.g. 'bump-openssl-3.0.15'. Reusing a key de-duplicates."}},
				"required":["repo","title","body"]}`,
			Fn: tb.openPR,
		},
	}
}

var openPRDesc = map[config.Mode]string{
	config.ModeFull: "Commit all files written to the repo since the last open_pr and open a pull request. " +
		"Never merges. If an identical PR is already open it is reused.",
	config.ModeSafe: "Commit all files written to the repo since the last open_pr and open a pull request. " +
		"Only non-breaking changes are accepted: version upgrades within the same major version " +
		"(e.g. alpine:3.18 -> 3.20, v1.4.2 -> v1.5.0), lockfile updates, and new entries in scanner " +
		"ignore files such as .trivyignore for false positives. Any other change is refused.",
	config.ModeYolo: "Commit all files written to the repo since the last open_pr and push the commit directly " +
		"to the default branch, without review. If the push is rejected (branch protection, or the " +
		"branch moved), a pull request is opened instead.",
}

func (tb *Toolbox) repo(ctx context.Context, name string) (*Repo, error) {
	if err := tb.Guard.CheckRepo(name); err != nil {
		return nil, err
	}
	return tb.Workspaces.Get(ctx, name)
}

func (tb *Toolbox) list(ctx context.Context, raw json.RawMessage) (string, error) {
	var a struct {
		Repo, Path string
		Recursive  bool
	}
	if err := llm.DecodeArgs(raw, &a); err != nil {
		return "", err
	}
	r, err := tb.repo(ctx, a.Repo)
	if err != nil {
		return "", err
	}
	entries, truncated, err := r.List(a.Path, a.Recursive, maxListEntries)
	if err != nil {
		return "", err
	}
	out := strings.Join(entries, "\n")
	if truncated {
		out += fmt.Sprintf("\n...[truncated at %d entries; list a subdirectory]", maxListEntries)
	}
	if out == "" {
		out = "(empty)"
	}
	return out, nil
}

func (tb *Toolbox) read(ctx context.Context, raw json.RawMessage) (string, error) {
	var a struct{ Repo, Path string }
	if err := llm.DecodeArgs(raw, &a); err != nil {
		return "", err
	}
	r, err := tb.repo(ctx, a.Repo)
	if err != nil {
		return "", err
	}
	return r.ReadFile(a.Path, readLimit)
}

func (tb *Toolbox) write(ctx context.Context, raw json.RawMessage) (string, error) {
	var a struct{ Repo, Path, Content string }
	if err := llm.DecodeArgs(raw, &a); err != nil {
		return "", err
	}
	if err := tb.Guard.CheckFileSize(len(a.Content)); err != nil {
		return "", err
	}
	p, err := cleanPath(a.Path)
	if err != nil {
		return "", err
	}
	if err := tb.Guard.CheckWritePath(p); err != nil {
		return "", err
	}
	r, err := tb.repo(ctx, a.Repo)
	if err != nil {
		return "", err
	}
	if err := r.WriteFile(a.Path, []byte(a.Content)); err != nil {
		return "", err
	}
	return fmt.Sprintf("wrote %d bytes to %s", len(a.Content), a.Path), nil
}

// openPR returns the first open PR in prs, or nil.
func openPR(prs []PR) *PR {
	for i := range prs {
		if prs[i].State == PRStateOpen {
			return &prs[i]
		}
	}
	return nil
}

// lastClosed returns the most recently closed unmerged PR in prs, or nil.
func lastClosed(prs []PR) *PR {
	var last *PR
	for i := range prs {
		if prs[i].State == PRStateClosed && (last == nil || prs[i].Closed.After(last.Closed)) {
			last = &prs[i]
		}
	}
	return last
}

// footer is appended to the PR body and commit message, by mode.
func (tb *Toolbox) footer() string {
	switch tb.Guard.Mode() {
	case config.ModeYolo:
		return "\n\n---\n_Committed by HeraIon playbook `" + tb.Playbook + "` in yolo mode, without review._"
	case config.ModeSafe:
		return "\n\n---\n_Opened by HeraIon playbook `" + tb.Playbook + "`. Mode: safe (non-breaking changes only, checked by HeraIon). " +
			"Review carefully; HeraIon never merges._"
	}
	return "\n\n---\n_Opened by HeraIon playbook `" + tb.Playbook + "`. Review carefully; HeraIon never merges._"
}

// existingPR reports an open PR on branch, or one closed without merging
// within ReopenAfter. It returns the tool result and true if no new PR may
// be opened.
func (tb *Toolbox) existingPR(ctx context.Context, repo, branch string, log *slog.Logger) (string, bool, error) {
	if tb.Guard.DryRun() || tb.PRs == nil {
		return "", false, nil
	}
	found, err := tb.PRs.FindByBranch(ctx, repo, branch)
	if err != nil {
		return "", false, err
	}
	if open := openPR(found); open != nil {
		tb.Opened = append(tb.Opened, OpenedPR{Repo: repo, Branch: branch, URL: open.URL, Outcome: OutcomeDuplicate})
		log.Info("PR already open, skipping", "url", open.URL)
		return fmt.Sprintf("an open PR for this change already exists: %s (not opened again)", open.URL), true, nil
	}
	if c := lastClosed(found); c != nil && (tb.ReopenAfter == 0 || time.Since(c.Closed) < tb.ReopenAfter) {
		tb.Opened = append(tb.Opened, OpenedPR{Repo: repo, Branch: branch, URL: c.URL, Outcome: OutcomeRejected})
		log.Info("PR for this change was closed without merging, not reopening", "url", c.URL, "closed", c.Closed)
		return fmt.Sprintf("a PR for this change was closed without merging on %s: %s. It is not reopened; "+
			"mention it in your summary instead.", c.Closed.Format(time.DateOnly), c.URL), true, nil
	}
	return "", false, nil
}

func (tb *Toolbox) openPR(ctx context.Context, raw json.RawMessage) (string, error) {
	var a struct{ Repo, Title, Body, Branch string }
	if err := llm.DecodeArgs(raw, &a); err != nil {
		return "", err
	}
	if strings.TrimSpace(a.Title) == "" {
		return "", errors.New("title is required")
	}
	r, err := tb.repo(ctx, a.Repo)
	if err != nil {
		return "", err
	}
	key := a.Branch
	if key == "" {
		key = a.Title
	}
	mode := tb.Guard.Mode()
	branch := BranchName(tb.Playbook, key)
	// The PR text leaves the cluster, so mask it too.
	red := tb.Guard.Redactor()
	title, body := red.Redact(a.Title), red.Redact(a.Body+tb.footer())
	log := tb.Log.With("repo", a.Repo, "branch", branch, "mode", mode)

	// Whatever happens, the next open_pr starts from a clean base branch.
	defer func() {
		if err := r.Reset(); err != nil {
			log.Warn("could not reset the clone", "err", err)
		}
	}()
	if err := tb.checkChangedPaths(r); err != nil {
		return "", err
	}
	// Look for an existing PR before using budget or pushing. Yolo looks
	// only if it has to fall back to a PR.
	if mode != config.ModeYolo {
		if msg, stop, err := tb.existingPR(ctx, a.Repo, branch, log); err != nil || stop {
			return msg, err
		}
	}
	if err := tb.Guard.ReservePR(); err != nil {
		return "", err
	}
	used := false
	defer func() {
		if !used {
			tb.Guard.ReleasePR()
		}
	}()
	patch, err := tb.commit(r, branch, title+"\n\n"+body, log)
	if err != nil {
		return "", err
	}
	if tb.Guard.DryRun() {
		used = true
		return tb.logDryRun(r, branch, title, body, patch, log), nil
	}
	msg, used, err := tb.deliver(ctx, r, branch, title, body, log)
	return msg, err
}

// checkChangedPaths re-checks every changed path: a write through a symlink
// in the repo can land somewhere repo_write_file would have refused.
func (tb *Toolbox) checkChangedPaths(r *Repo) error {
	paths, err := r.Changed()
	if err != nil {
		return err
	}
	for _, p := range paths {
		if err := tb.Guard.CheckWritePath(p); err != nil {
			return fmt.Errorf("not opening a PR: %w", err)
		}
	}
	return nil
}

// commit commits the clone's changes to branch and returns the diff. It
// refuses a diff that adds a credential, or in safe mode one that is not
// non-breaking.
func (tb *Toolbox) commit(r *Repo, branch, message string, log *slog.Logger) (string, error) {
	patch, changed, err := r.Commit(branch, message)
	if err != nil {
		return "", err
	}
	if !changed {
		return "", errors.New("no changes to commit: write files with repo_write_file first")
	}
	// File contents are pushed as written, so refuse a change that adds a
	// credential, e.g. one the model copied from a tool result.
	if leaks := tb.Guard.Redactor().FindSecretLines(patch); len(leaks) > 0 {
		log.Warn("refusing PR with secret-like content", "lines", len(leaks))
		return "", fmt.Errorf("not opening a PR: the change adds %d line(s) that look like credentials, e.g. %q; remove them",
			len(leaks), textutil.Truncate(leaks[0], 200))
	}
	if tb.Guard.Mode() == config.ModeSafe {
		if err := guardrails.CheckNonBreaking(patch); err != nil {
			log.Info("refusing PR in safe mode", "reason", err)
			return "", fmt.Errorf("not opening a PR: %w. Leave this change out, or mention it in your summary", err)
		}
	}
	return patch, nil
}

// logDryRun logs the change instead of pushing it.
func (tb *Toolbox) logDryRun(r *Repo, branch, title, body, patch string, log *slog.Logger) string {
	diff := tb.Guard.Redactor().Redact(patch)
	tb.Opened = append(tb.Opened, OpenedPR{Repo: r.Name, Branch: branch, Outcome: OutcomeDryRun})
	if tb.Guard.Mode() == config.ModeYolo {
		log.Info("dry-run: would push to "+r.BaseBranch, "title", title, "body", body, "diff", diff)
		return fmt.Sprintf("dry-run: commit %q on %s was logged, not pushed to %s", title, r.Name, r.BaseBranch)
	}
	log.Info("dry-run: would open PR", "title", title, "base", r.BaseBranch, "body", body, "diff", diff)
	return fmt.Sprintf("dry-run: PR %q on %s (%s -> %s) was logged, not opened", title, r.Name, branch, r.BaseBranch)
}

// deliver pushes the committed change: in yolo mode to the base branch,
// otherwise, or when that push is refused, as a PR. used is false when
// nothing was pushed or opened, so the caller returns the budget.
func (tb *Toolbox) deliver(ctx context.Context, r *Repo, branch, title, body string, log *slog.Logger) (msg string, used bool, err error) {
	outcome := OutcomeOpened
	if tb.Guard.Mode() == config.ModeYolo {
		err := r.PushToBase(ctx, branch)
		if err == nil {
			tb.Opened = append(tb.Opened, OpenedPR{Repo: r.Name, Branch: r.BaseBranch, Outcome: OutcomePushed})
			log.Info("pushed to base branch", "base", r.BaseBranch)
			return fmt.Sprintf("pushed commit %q to %s on %s", title, r.BaseBranch, r.Name), true, nil
		}
		if ctx.Err() != nil {
			return "", false, fmt.Errorf("push to %s: %w", r.BaseBranch, err)
		}
		log.Warn("push to base branch rejected, opening a PR instead", "base", r.BaseBranch, "err", err)
		if msg, stop, err := tb.existingPR(ctx, r.Name, branch, log); err != nil || stop {
			return msg, false, err
		}
		outcome = OutcomeFellBack
	}
	if tb.PRs == nil {
		return "", false, errors.New("no Git provider configured")
	}
	if err := r.Push(ctx, branch); err != nil {
		return "", false, fmt.Errorf("push %s: %w", branch, err)
	}
	pr, err := tb.PRs.Create(ctx, r.Name, PRInput{Title: title, Body: body, Head: branch, Base: r.BaseBranch})
	if err != nil {
		return "", false, err
	}
	tb.Opened = append(tb.Opened, OpenedPR{Repo: r.Name, Branch: branch, URL: pr.URL, Outcome: outcome})
	log.Info("opened PR", "url", pr.URL, "number", pr.Number)
	if outcome == OutcomeFellBack {
		return fmt.Sprintf("the push to %s was rejected, so PR #%d was opened instead: %s", r.BaseBranch, pr.Number, pr.URL), true, nil
	}
	return fmt.Sprintf("opened PR #%d: %s", pr.Number, pr.URL), true, nil
}
