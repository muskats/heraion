// Package scm clones repos (go-git), opens PRs (go-scm) and provides the
// built-in tools repo_list, repo_read_file, repo_write_file and open_pr.
package scm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/go-git/go-git/v5"
	gitcfg "github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"

	"github.com/muskats/heraion/internal/config"
)

// Workspaces clones each repo on first use into dir (an emptyDir in Kubernetes).
type Workspaces struct {
	git  config.Git
	root string

	mu    sync.Mutex
	repos map[string]*Repo
}

// NewWorkspaces creates a workspace manager rooted at dir.
func NewWorkspaces(g config.Git, dir string) *Workspaces {
	return &Workspaces{git: g, root: dir, repos: map[string]*Repo{}}
}

// auth reads the token on every call, so a push long after the clone still
// uses a fresh token. No token means anonymous access (e.g. public repos).
func (w *Workspaces) auth() *githttp.BasicAuth {
	tok := w.git.Token.String()
	if tok == "" {
		return nil
	}
	return &githttp.BasicAuth{Username: w.git.Username, Password: tok}
}

// CloneURL returns the clone URL for owner/name.
func (w *Workspaces) CloneURL(repo string) string {
	return w.git.CloneBaseURL + "/" + repo + ".git"
}

// Get returns the clone of repo, e.g. "acme/web", cloning it on first use.
// The caller checks the allowlist.
func (w *Workspaces) Get(ctx context.Context, repo string) (*Repo, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if r, ok := w.repos[repo]; ok {
		return r, nil
	}
	dir := filepath.Join(w.root, strings.ReplaceAll(repo, "/", "__"))
	if err := os.RemoveAll(dir); err != nil {
		return nil, err
	}
	g, err := git.PlainCloneContext(ctx, dir, false, &git.CloneOptions{
		URL:          w.CloneURL(repo),
		Auth:         w.auth(),
		Depth:        1,
		SingleBranch: true,
		Tags:         git.NoTags,
	})
	if err != nil {
		return nil, fmt.Errorf("clone %s: %w", repo, err)
	}
	head, err := g.Head()
	if err != nil {
		return nil, fmt.Errorf("clone %s: resolve HEAD: %w", repo, err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	r := &Repo{
		Name: repo, Dir: dir, BaseBranch: head.Name().Short(), baseHash: head.Hash(),
		g: g, root: root, auth: w.auth, author: w.git,
	}
	w.repos[repo] = r
	return r, nil
}

// Close releases file handles. The emptyDir removes the files.
func (w *Workspaces) Close() {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, r := range w.repos {
		r.root.Close()
	}
}

// Repo is a single cloned repository. Methods are safe for concurrent use.
type Repo struct {
	Name       string
	Dir        string
	BaseBranch string

	mu       sync.Mutex
	baseHash plumbing.Hash
	g        *git.Repository
	root     *os.Root // confines all file access to the clone, symlinks included
	auth     func() *githttp.BasicAuth
	author   config.Git
}

// cleanPath validates a repo-relative path supplied by the model.
func cleanPath(p string) (string, error) {
	p = strings.TrimPrefix(path.Clean("/"+strings.TrimSpace(p)), "/")
	if p == "" {
		p = "."
	}
	if !filepath.IsLocal(filepath.FromSlash(p)) && p != "." {
		return "", fmt.Errorf("path %q escapes the repository", p)
	}
	if p == ".git" || strings.HasPrefix(p, ".git/") {
		return "", errors.New("access to .git is not allowed")
	}
	return p, nil
}

// ReadFile returns a file's content, capped at limit bytes.
func (r *Repo) ReadFile(p string, limit int) (string, error) {
	p, err := cleanPath(p)
	if err != nil {
		return "", err
	}
	f, err := r.root.Open(filepath.FromSlash(p))
	if err != nil {
		return "", err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return "", err
	}
	if st.IsDir() {
		return "", fmt.Errorf("%s is a directory", p)
	}
	buf, err := io.ReadAll(io.LimitReader(f, int64(limit)))
	if err != nil {
		return "", err
	}
	s := string(buf)
	if st.Size() > int64(limit) {
		s += fmt.Sprintf("\n...[truncated at %d of %d bytes]", limit, st.Size())
	}
	return s, nil
}

// WriteFile creates or replaces a file, creating parent directories.
func (r *Repo) WriteFile(p string, data []byte) error {
	p, err := cleanPath(p)
	if err != nil {
		return err
	}
	if p == "." {
		return errors.New("path is required")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	native := filepath.FromSlash(p)
	if dir := filepath.Dir(native); dir != "." {
		if err := r.root.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	return r.root.WriteFile(native, data, 0o644)
}

// List returns entries under p, e.g. ["Dockerfile", "deploy/"]. With
// recursive it walks subdirectories (never .git), up to max entries.
func (r *Repo) List(p string, recursive bool, max int) ([]string, bool, error) {
	p, err := cleanPath(p)
	if err != nil {
		return nil, false, err
	}
	fsys := r.root.FS()
	var out []string
	truncated := false
	err = fs.WalkDir(fsys, filepath.ToSlash(p), func(fp string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if fp == p {
			return nil
		}
		if d.IsDir() && d.Name() == ".git" {
			return fs.SkipDir
		}
		if len(out) >= max {
			truncated = true
			return fs.SkipAll
		}
		name := fp
		if d.IsDir() {
			name += "/"
		}
		out = append(out, name)
		if d.IsDir() && !recursive {
			return fs.SkipDir
		}
		return nil
	})
	return out, truncated, err
}

// Commit commits all changes to a new branch and returns the diff. changed
// is false if there was nothing to commit.
func (r *Repo) Commit(branch, message string) (patch string, changed bool, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	wt, err := r.g.Worktree()
	if err != nil {
		return "", false, err
	}
	st, err := wt.Status()
	if err != nil {
		return "", false, err
	}
	if st.IsClean() {
		return "", false, nil
	}
	ref := plumbing.NewBranchReferenceName(branch)
	if err := wt.Checkout(&git.CheckoutOptions{Branch: ref, Create: true, Keep: true}); err != nil {
		return "", false, fmt.Errorf("create branch %s: %w", branch, err)
	}
	if err := wt.AddWithOptions(&git.AddOptions{All: true}); err != nil {
		return "", false, err
	}
	sig := &object.Signature{Name: r.author.AuthorName, Email: r.author.AuthorEmail, When: time.Now()}
	h, err := wt.Commit(message, &git.CommitOptions{Author: sig, Committer: sig})
	if err != nil {
		return "", false, err
	}
	head, err := r.g.CommitObject(h)
	if err != nil {
		return "", false, err
	}
	base, err := r.g.CommitObject(r.baseHash)
	if err != nil {
		return "", false, err
	}
	p, err := base.Patch(head)
	if err != nil {
		return "", false, err
	}
	return p.String(), true, nil
}

// Push force-pushes branch. Only heraion/... branches are allowed, so force
// never overwrites someone else's branch.
func (r *Repo) Push(ctx context.Context, branch string) error {
	if !strings.HasPrefix(branch, BranchPrefix) {
		return fmt.Errorf("refusing to push branch %q outside %s", branch, BranchPrefix)
	}
	ref := plumbing.NewBranchReferenceName(branch)
	err := r.g.PushContext(ctx, &git.PushOptions{
		RemoteName: "origin",
		RefSpecs:   []gitcfg.RefSpec{gitcfg.RefSpec("+" + ref + ":" + ref)},
		Auth:       r.auth(),
	})
	if errors.Is(err, git.NoErrAlreadyUpToDate) {
		return nil
	}
	return err
}

// Reset drops uncommitted changes and checks out the base branch, so the
// next PR starts clean.
func (r *Repo) Reset() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	wt, err := r.g.Worktree()
	if err != nil {
		return err
	}
	if err := wt.Checkout(&git.CheckoutOptions{Branch: plumbing.NewBranchReferenceName(r.BaseBranch), Force: true}); err != nil {
		return err
	}
	return wt.Clean(&git.CleanOptions{Dir: true})
}
