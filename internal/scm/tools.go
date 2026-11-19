package scm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/muskats/heraion/internal/guardrails"
	"github.com/muskats/heraion/internal/llm"
)

// Toolbox holds what the built-in Git tools share during one run.
type Toolbox struct {
	Playbook   string
	Workspaces *Workspaces
	PRs        PRService // may be nil when dry-run
	Guard      *guardrails.Guard
	Log        *slog.Logger
	// Opened collects PRs opened (or found as duplicates) during the run.
	Opened []OpenedPR
}

// OpenedPR records the outcome of one open_pr call.
type OpenedPR struct {
	Repo, Branch, URL string
	Duplicate, DryRun bool
}

const readLimit = 256 << 10

// Tools returns the built-in tools.
func (tb *Toolbox) Tools() []llm.Tool {
	repoEnum, _ := json.Marshal(tb.Guard.Repos())
	repoProp := fmt.Sprintf(`"repo":{"type":"string","enum":%s,"description":"Repository as owner/name"}`, repoEnum)
	return []llm.Tool{
		&tool{
			name: "repo_list",
			desc: "List files and directories in a repository. Directories end in '/'.",
			schema: `{"type":"object","properties":{` + repoProp + `,
				"path":{"type":"string","description":"Directory relative to the repo root; default '.'"},
				"recursive":{"type":"boolean","description":"Walk subdirectories (capped at 2000 entries)"}},
				"required":["repo"]}`,
			call: tb.list,
		},
		&tool{
			name: "repo_read_file",
			desc: "Read a text file from a repository.",
			schema: `{"type":"object","properties":{` + repoProp + `,
				"path":{"type":"string","description":"File path relative to the repo root"}},
				"required":["repo","path"]}`,
			call: tb.read,
		},
		&tool{
			name: "repo_write_file",
			desc: "Create or overwrite a file in the local clone. Changes are only proposed through open_pr.",
			schema: `{"type":"object","properties":{` + repoProp + `,
				"path":{"type":"string","description":"File path relative to the repo root"},
				"content":{"type":"string","description":"Full new file content"}},
				"required":["repo","path","content"]}`,
			call: tb.write,
		},
		&tool{
			name: "open_pr",
			desc: "Commit all files written to the repo since the last open_pr and open a pull request. " +
				"Never merges. If an identical PR is already open it is reused.",
			schema: `{"type":"object","properties":{` + repoProp + `,
				"title":{"type":"string","description":"Short PR title"},
				"body":{"type":"string","description":"Markdown description: problem, evidence, change, risk"},
				"branch":{"type":"string","description":"Stable short key for this change, e.g. 'bump-openssl-3.0.15'. Reusing a key de-duplicates."}},
				"required":["repo","title","body"]}`,
			call: tb.openPR,
		},
	}
}

type tool struct {
	name, desc, schema string
	call               func(context.Context, json.RawMessage) (string, error)
}

func (t *tool) Name() string                                                { return t.name }
func (t *tool) Description() string                                         { return t.desc }
func (t *tool) Schema() json.RawMessage                                     { return json.RawMessage(t.schema) }
func (t *tool) Call(ctx context.Context, a json.RawMessage) (string, error) { return t.call(ctx, a) }

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
	r, err := tb.repo(ctx, a.Repo)
	if err != nil {
		return "", err
	}
	if err := r.WriteFile(a.Path, []byte(a.Content)); err != nil {
		return "", err
	}
	return fmt.Sprintf("wrote %d bytes to %s", len(a.Content), a.Path), nil
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

	// Look for an open PR before using budget or pushing.
	if !tb.Guard.DryRun() && tb.PRs != nil {
		existing, err := tb.PRs.FindOpen(ctx, a.Repo, branch)
		if err != nil {
			return "", err
		}
		if existing != nil {
			_ = r.Reset()
			tb.Opened = append(tb.Opened, OpenedPR{Repo: a.Repo, Branch: branch, URL: existing.URL, Duplicate: true})
			log.Info("PR already open, skipping", "url", existing.URL)
			return fmt.Sprintf("an open PR for this change already exists: %s (not opened again)", existing.URL), nil
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
