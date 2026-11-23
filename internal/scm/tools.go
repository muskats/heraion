package scm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/go-git/go-git/v5/plumbing/object"

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
	// ReopenAfter is how long a PR closed without merging blocks the same
	// change again. 0: forever.
	ReopenAfter time.Duration
	// Opened collects the outcome of every open_pr call of the run.
	Opened []OpenedPR
}

// OpenedPR records the outcome of one open_pr call.
type OpenedPR struct {
	Repo, Branch, URL string
	Number            int // 0 when no PR was opened, e.g. dry-run
	Outcome           Outcome
}

// Outcome is what one open_pr call did.
type Outcome string

// Outcomes.
const (
	OutcomeOpened    Outcome = "opened"    // a new PR
	OutcomeUpdated   Outcome = "updated"   // the open PR on the branch got the new change
	OutcomeUnchanged Outcome = "unchanged" // the open PR already has this change
	OutcomeRejected  Outcome = "rejected"  // a PR with this change was closed without merging
	OutcomeSkipped   Outcome = "skipped"   // the open PR has commits not made by HeraIon
	OutcomeDryRun    Outcome = "dry-run"   // logged, not pushed
)

// Limits of repo_read_file (bytes) and repo_list (entries).
const (
	readLimit      = 256 << 10
	maxListEntries = 2000
)

// Arguments of the built-in tools; their JSON Schemas come from these.
type (
	listArgs struct {
		Repo      string `json:"repo" jsonschema:"Repository as owner/name"`
		Path      string `json:"path,omitempty" jsonschema:"Directory relative to the repo root; default '.'"`
		Recursive bool   `json:"recursive,omitempty" jsonschema:"Walk subdirectories (capped at 2000 entries)"`
	}
	readArgs struct {
		Repo string `json:"repo" jsonschema:"Repository as owner/name"`
		Path string `json:"path" jsonschema:"File path relative to the repo root"`
	}
	writeArgs struct {
		Repo    string `json:"repo" jsonschema:"Repository as owner/name"`
		Path    string `json:"path" jsonschema:"File path relative to the repo root"`
		Content string `json:"content" jsonschema:"Full new file content"`
	}
	openPRArgs struct {
		Repo  string `json:"repo" jsonschema:"Repository as owner/name"`
		Title string `json:"title" jsonschema:"Short PR title"`
		Body  string `json:"body" jsonschema:"Markdown description: problem, evidence, change, risk"`
	}
)

// Tools returns the built-in tools.
func (tb *Toolbox) Tools() []llm.Tool {
	repos := map[string][]string{"repo": tb.Guard.Repos()}
	return []llm.Tool{
		&llm.FuncTool{
			ToolName: "repo_list",
			Desc:     "List files and directories in a repository. Directories end in '/'.",
			Params:   llm.SchemaFor[listArgs](repos),
			Fn:       tb.list,
		},
		&llm.FuncTool{
			ToolName: "repo_read_file",
			Desc:     "Read a text file from a repository.",
			Params:   llm.SchemaFor[readArgs](repos),
			Fn:       tb.read,
		},
		&llm.FuncTool{
			ToolName: "repo_write_file",
			Desc:     "Create or overwrite a file in the local clone. Changes are only proposed through open_pr.",
			Params:   llm.SchemaFor[writeArgs](repos),
			Fn:       tb.write,
		},
		&llm.FuncTool{
			ToolName: "open_pr",
			Desc:     openPRDesc[tb.Guard.Mode()],
			Params:   llm.SchemaFor[openPRArgs](repos),
			Fn:       tb.openPR,
		},
	}
}

var openPRDesc = map[config.Mode]string{
	config.ModeFull: "Commit all files written to the repo since the last open_pr and open a pull request. " +
		"Never merges. The branch is named after the changed files, so a rerun updates the open PR for them.",
	config.ModeSafe: "Commit all files written to the repo since the last open_pr and open a pull request. " +
		"Safe mode: only versions and image digests may change in existing lines: an upgrade within the major " +
		"version (e.g. node:20.11 -> 20.18, v1.4.2 -> v1.5.0), only the patch level of a distro image " +
		"(alpine:3.20.1 -> 3.20.3), or another @sha256 digest. Lockfiles, scanner ignore files, and new, " +
		"deleted or renamed files are refused.",
}

func (tb *Toolbox) repo(ctx context.Context, name string) (*Repo, error) {
	if err := tb.Guard.CheckRepo(name); err != nil {
		return nil, err
	}
	return tb.Workspaces.Get(ctx, name)
}

func (tb *Toolbox) list(ctx context.Context, raw json.RawMessage) (string, error) {
	var a listArgs
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
	var a readArgs
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
	var a writeArgs
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

// closedWith returns the most recently closed unmerged PR in prs that
// carries change, or nil. Merged PRs never block.
func closedWith(prs []PR, change string) *PR {
	var last *PR
	for i := range prs {
		if prs[i].State == PRStateClosed && ChangeOf(prs[i].Body) == change && (last == nil || prs[i].Closed.After(last.Closed)) {
			last = &prs[i]
		}
	}
	return last
}

// footer is appended to the PR body and commit message, by mode.
func (tb *Toolbox) footer() string {
	if tb.Guard.Mode() == config.ModeSafe {
		return "\n\n---\n_Opened by HeraIon playbook `" + tb.Playbook + "`. Mode: safe (HeraIon checked that only versions and image digests changed). " +
			"Review carefully; HeraIon never merges._"
	}
	return "\n\n---\n_Opened by HeraIon playbook `" + tb.Playbook + "`. Review carefully; HeraIon never merges._"
}

// existingPR decides what open_pr does with change on branch:
//
//	open PR, same change            unchanged, nothing pushed (the head commit's trailer, else the body's marker)
//	open PR, head not by HeraIon    skipped: a human's commits are not overwritten
//	open PR, other change           update returns that PR
//	closed PR with the same change  rejected, within ReopenAfter (0: forever)
//	otherwise                       a new PR
//
// stop is true when nothing may be pushed; msg is then the tool result.
func (tb *Toolbox) existingPR(ctx context.Context, r *Repo, branch, change string, log *slog.Logger) (update *PR, msg string, stop bool, err error) {
	if tb.Guard.DryRun() || tb.PRs == nil {
		return nil, "", false, nil
	}
	found, err := tb.PRs.FindByBranch(ctx, r.Name, branch)
	if err != nil {
		return nil, "", false, err
	}
	record := func(o Outcome, pr *PR) {
		tb.Opened = append(tb.Opened, OpenedPR{Repo: r.Name, Branch: branch, URL: pr.URL, Number: pr.Number, Outcome: o})
	}
	if open := openPR(found); open != nil {
		ours, onBranch, err := r.RemoteHead(ctx, branch)
		if err != nil {
			return nil, "", false, err
		}
		// The head commit's trailer says what the branch holds, also when the
		// body is old because its update failed, e.g. on Bitbucket Cloud or
		// Gogs. Without a trailer (a commit made before HeraIon wrote one, or
		// a person's commit) the body's marker counts.
		if onBranch == "" {
			onBranch = ChangeOf(open.Body)
		}
		if onBranch == change {
			record(OutcomeUnchanged, open)
			log.Info("open PR already has this change", "url", open.URL)
			return nil, fmt.Sprintf("the open PR %s already has exactly this change; nothing was pushed", open.URL), true, nil
		}
		if !ours {
			record(OutcomeSkipped, open)
			log.Warn("open PR has commits not made by HeraIon, leaving it alone", "url", open.URL)
			return nil, fmt.Sprintf("the open PR %s has commits by someone else, so it is not updated; "+
				"mention the change in your summary instead", open.URL), true, nil
		}
		return open, "", false, nil
	}
	if c := closedWith(found, change); c != nil && (tb.ReopenAfter == 0 || time.Since(c.Closed) < tb.ReopenAfter) {
		record(OutcomeRejected, c)
		log.Info("this change was closed without merging, not proposing it again", "url", c.URL, "closed", c.Closed)
		return nil, fmt.Sprintf("a PR with exactly this change was closed without merging on %s: %s. It is not proposed "+
			"again; mention it in your summary instead.", c.Closed.Format(time.DateOnly), c.URL), true, nil
	}
	return nil, "", false, nil
}

func (tb *Toolbox) openPR(ctx context.Context, raw json.RawMessage) (string, error) {
	var a openPRArgs
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
	// Whatever happens, the next open_pr starts from a clean base branch.
	defer func() {
		if err := r.Reset(); err != nil {
			tb.Log.Warn("could not reset the clone", "repo", a.Repo, "err", err)
		}
	}()
	paths, err := tb.checkChangedPaths(r)
	if err != nil {
		return "", err
	}
	if len(paths) == 0 {
		return "", errors.New("no changes to commit: write files with repo_write_file first")
	}
	// The branch comes from what changed, not from the model's words.
	branch := BranchForPaths(tb.Playbook, a.Repo, paths)
	// The PR text leaves the cluster, so mask it too.
	red := tb.Guard.Redactor()
	title, body := red.Redact(a.Title), red.Redact(a.Body+tb.footer())
	log := tb.Log.With("repo", a.Repo, "branch", branch, "mode", tb.Guard.Mode())

	patch, change, err := tb.commit(r, branch, title+"\n\n"+body, log)
	if err != nil {
		return "", err
	}
	body += "\n\n" + changeMarker(change)
	if tb.Guard.DryRun() {
		if err := tb.Guard.ReservePR(); err != nil {
			return "", err
		}
		return tb.logDryRun(r, branch, title, body, patch.String(), log), nil
	}
	update, msg, stop, err := tb.existingPR(ctx, r, branch, change, log)
	if err != nil || stop {
		return msg, err
	}
	if err := tb.Guard.ReservePR(); err != nil {
		return "", err
	}
	msg, used, err := tb.deliver(ctx, r, branch, title, body, update, log)
	if !used {
		tb.Guard.ReleasePR()
	}
	return msg, err
}

// discarded ends every open_pr refusal: the clone is reset after each call,
// so the model must write the files it still wants again.
const discarded = "All edits in this repo were discarded: write the files you still want again, then call open_pr."

// checkChangedPaths returns the changed paths after re-checking each: a
// write through a symlink in the repo can land somewhere repo_write_file
// would have refused.
func (tb *Toolbox) checkChangedPaths(r *Repo) ([]string, error) {
	paths, err := r.Changed()
	if err != nil {
		return nil, err
	}
	for _, p := range paths {
		if err := tb.Guard.CheckWritePath(p); err != nil {
			return nil, fmt.Errorf("not opening a PR: %w. %s", err, discarded)
		}
	}
	return paths, nil
}

// commit commits the clone's changes to branch and returns the diff and its
// change ID. It refuses a diff that adds a credential, or in safe mode one
// that changes more than versions and image digests (guardrails.CheckSafeDiff).
func (tb *Toolbox) commit(r *Repo, branch, message string, log *slog.Logger) (*object.Patch, string, error) {
	patch, change, err := r.Commit(branch, message)
	if err != nil {
		return nil, "", err
	}
	if patch == nil {
		return nil, "", errors.New("no changes to commit: write files with repo_write_file first")
	}
	// File contents are pushed as written, so refuse a change that adds a
	// credential, e.g. one the model copied from a tool result.
	if leaks := tb.Guard.Redactor().FindSecretLines(patch); len(leaks) > 0 {
		log.Warn("refusing PR with secret-like content", "lines", len(leaks))
		return nil, "", fmt.Errorf("not opening a PR: the change adds %d line(s) that look like credentials, e.g. %q. %s",
			len(leaks), textutil.Truncate(leaks[0], 200), discarded)
	}
	if tb.Guard.Mode() == config.ModeSafe {
		if err := guardrails.CheckSafeDiff(patch); err != nil {
			log.Info("refusing PR in safe mode", "reason", err)
			return nil, "", fmt.Errorf("not opening a PR: %w. Leave this change out, or mention it in your summary. %s", err, discarded)
		}
	}
	return patch, change, nil
}

// logDryRun logs the change instead of pushing it.
func (tb *Toolbox) logDryRun(r *Repo, branch, title, body, patch string, log *slog.Logger) string {
	diff := tb.Guard.Redactor().Redact(patch)
	tb.Opened = append(tb.Opened, OpenedPR{Repo: r.Name, Branch: branch, Outcome: OutcomeDryRun})
	log.Info("dry-run: would open PR", "title", title, "base", r.BaseBranch, "body", body, "diff", diff)
	return fmt.Sprintf("dry-run: PR %q on %s (%s -> %s) was logged, not opened", title, r.Name, branch, r.BaseBranch)
}

// deliver force-pushes branch, then opens a PR for it, or refreshes the
// title and body of update. used is false when nothing was pushed, so the
// caller returns the budget.
func (tb *Toolbox) deliver(ctx context.Context, r *Repo, branch, title, body string, update *PR, log *slog.Logger) (msg string, used bool, err error) {
	if tb.PRs == nil {
		return "", false, errors.New("no Git provider configured")
	}
	if err := r.Push(ctx, branch); err != nil {
		return "", false, fmt.Errorf("push %s: %w", branch, err)
	}
	in := PRInput{Title: title, Body: body, Head: branch, Base: r.BaseBranch}
	if update != nil {
		tb.Opened = append(tb.Opened, OpenedPR{Repo: r.Name, Branch: branch, URL: update.URL, Number: update.Number, Outcome: OutcomeUpdated})
		if err := tb.PRs.Update(ctx, r.Name, update.Number, in); err != nil {
			// The PR shows the pushed commit anyway; only its text is old.
			log.Warn("pushed the new change, but could not refresh the PR title and body", "url", update.URL, "err", err)
			return fmt.Sprintf("updated PR #%d with the new change (its description could not be refreshed): %s", update.Number, update.URL), true, nil
		}
		log.Info("updated PR", "url", update.URL, "number", update.Number)
		return fmt.Sprintf("updated PR #%d with the new change: %s", update.Number, update.URL), true, nil
	}
	pr, err := tb.PRs.Create(ctx, r.Name, in)
	if err != nil {
		return "", false, err
	}
	tb.Opened = append(tb.Opened, OpenedPR{Repo: r.Name, Branch: branch, URL: pr.URL, Number: pr.Number, Outcome: OutcomeOpened})
	log.Info("opened PR", "url", pr.URL, "number", pr.Number)
	return fmt.Sprintf("opened PR #%d: %s", pr.Number, pr.URL), true, nil
}
