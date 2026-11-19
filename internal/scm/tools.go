package scm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

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
	Duplicate, DryRun bool
	// Rejected: a PR for this branch was recently closed without merging,
	// so none was opened.
	Rejected bool
}

const readLimit = 256 << 10

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
				"recursive":{"type":"boolean","description":"Walk subdirectories (capped at 2000 entries)"}},
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
			Desc: "Commit all files written to the repo since the last open_pr and open a pull request. " +
				"Never merges. If an identical PR is already open it is reused.",
			Params: `{"type":"object","properties":{` + repoProp + `,
				"title":{"type":"string","description":"Short PR title"},
				"body":{"type":"string","description":"Markdown description: problem, evidence, change, risk"},
				"branch":{"type":"string","description":"Stable short key for this change, e.g. 'bump-openssl-3.0.15'. Reusing a key de-duplicates."}},
				"required":["repo","title","body"]}`,
			Fn: tb.openPR,
		},
	}
}

func decode[T any](raw json.RawMessage) (T, error) {
	var v T
	if err := json.Unmarshal(raw, &v); err != nil {
		return v, fmt.Errorf("invalid arguments: %w", err)
	}
	return v, nil
}

func (tb *Toolbox) repo(ctx context.Context, name string) (*Repo, error) {
	if err := tb.Guard.CheckRepo(name); err != nil {
		return nil, err
	}
	return tb.Workspaces.Get(ctx, name)
}

func (tb *Toolbox) list(ctx context.Context, raw json.RawMessage) (string, error) {
	a, err := decode[struct {
		Repo, Path string
		Recursive  bool
	}](raw)
	if err != nil {
		return "", err
	}
	r, err := tb.repo(ctx, a.Repo)
	if err != nil {
		return "", err
	}
	entries, truncated, err := r.List(a.Path, a.Recursive, 2000)
	if err != nil {
		return "", err
	}
	out := strings.Join(entries, "\n")
	if truncated {
		out += "\n...[truncated at 2000 entries; list a subdirectory]"
	}
	if out == "" {
		out = "(empty)"
	}
	return out, nil
}

func (tb *Toolbox) read(ctx context.Context, raw json.RawMessage) (string, error) {
	a, err := decode[struct{ Repo, Path string }](raw)
	if err != nil {
		return "", err
	}
	r, err := tb.repo(ctx, a.Repo)
	if err != nil {
		return "", err
	}
	return r.ReadFile(a.Path, readLimit)
}

func (tb *Toolbox) write(ctx context.Context, raw json.RawMessage) (string, error) {
	a, err := decode[struct{ Repo, Path, Content string }](raw)
	if err != nil {
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

func (tb *Toolbox) openPR(ctx context.Context, raw json.RawMessage) (string, error) {
	a, err := decode[struct{ Repo, Title, Body, Branch string }](raw)
	if err != nil {
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
	branch := BranchName(tb.Playbook, key)
	body := a.Body + "\n\n---\n_Opened by HeraIon playbook `" + tb.Playbook + "`. Review carefully; HeraIon never merges._"
	// The PR text leaves the cluster, so mask it too.
	title, body := tb.Guard.Redactor().Redact(a.Title), tb.Guard.Redactor().Redact(body)
	log := tb.Log.With("repo", a.Repo, "branch", branch)

	// Re-check every changed path: a write through a symlink in the repo can
	// land somewhere repo_write_file would have refused.
	paths, err := r.Changed()
	if err != nil {
		return "", err
	}
	for _, p := range paths {
		if err := tb.Guard.CheckWritePath(p); err != nil {
			_ = r.Reset()
			return "", fmt.Errorf("not opening a PR: %w", err)
		}
	}

	// Look for an existing PR before using budget or pushing.
	if !tb.Guard.DryRun() && tb.PRs != nil {
		found, err := tb.PRs.FindByBranch(ctx, a.Repo, branch)
		if err != nil {
			return "", err
		}
		if open := openPR(found); open != nil {
			_ = r.Reset()
			tb.Opened = append(tb.Opened, OpenedPR{Repo: a.Repo, Branch: branch, URL: open.URL, Duplicate: true})
			log.Info("PR already open, skipping", "url", open.URL)
			return fmt.Sprintf("an open PR for this change already exists: %s (not opened again)", open.URL), nil
		}
		if c := lastClosed(found); c != nil && (tb.ReopenAfter == 0 || time.Since(c.Closed) < tb.ReopenAfter) {
			_ = r.Reset()
			tb.Opened = append(tb.Opened, OpenedPR{Repo: a.Repo, Branch: branch, URL: c.URL, Rejected: true})
			log.Info("PR for this change was closed without merging, not reopening", "url", c.URL, "closed", c.Closed)
			return fmt.Sprintf("a PR for this change was closed without merging on %s: %s. It is not reopened; "+
				"mention it in your summary instead.", c.Closed.Format(time.DateOnly), c.URL), nil
		}
	}
	if err := tb.Guard.ReservePR(); err != nil {
		return "", err
	}
	patch, changed, err := r.Commit(branch, title+"\n\n"+body)
	if err != nil || !changed {
		tb.Guard.ReleasePR()
		_ = r.Reset()
		if err != nil {
			return "", err
		}
		return "", errors.New("no changes to commit: write files with repo_write_file first")
	}
	defer func() { _ = r.Reset() }()

	// File contents are pushed as written, so refuse a change that adds a
	// credential, e.g. one the model copied from a tool result.
	if leaks := tb.Guard.Redactor().FindSecretLines(patch); len(leaks) > 0 {
		tb.Guard.ReleasePR()
		log.Warn("refusing PR with secret-like content", "lines", len(leaks))
		return "", fmt.Errorf("not opening a PR: the change adds %d line(s) that look like credentials, e.g. %q; remove them",
			len(leaks), textutil.Truncate(leaks[0], 200))
	}

	if tb.Guard.DryRun() {
		log.Info("dry-run: would open PR", "title", title, "base", r.BaseBranch,
			"body", body, "diff", tb.Guard.Redactor().Redact(patch))
		tb.Opened = append(tb.Opened, OpenedPR{Repo: a.Repo, Branch: branch, DryRun: true})
		return fmt.Sprintf("dry-run: PR %q on %s (%s -> %s) was logged, not opened", title, a.Repo, branch, r.BaseBranch), nil
	}
	if tb.PRs == nil {
		tb.Guard.ReleasePR()
		return "", errors.New("no Git provider configured")
	}
	if err := r.Push(ctx, branch); err != nil {
		tb.Guard.ReleasePR()
		return "", fmt.Errorf("push %s: %w", branch, err)
	}
	pr, err := tb.PRs.Create(ctx, a.Repo, PRInput{Title: title, Body: body, Head: branch, Base: r.BaseBranch})
	if err != nil {
		tb.Guard.ReleasePR()
		return "", err
	}
	tb.Opened = append(tb.Opened, OpenedPR{Repo: a.Repo, Branch: branch, URL: pr.URL})
	log.Info("opened PR", "url", pr.URL, "number", pr.Number)
	return fmt.Sprintf("opened PR #%d: %s", pr.Number, pr.URL), nil
}
