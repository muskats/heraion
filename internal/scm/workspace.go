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
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/go-git/go-git/v5"
	gitcfg "github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	"github.com/go-git/go-git/v5/storage/memory"

	"github.com/muskats/heraion/internal/config"
)

// Workspaces clones each repo on first use into dir (an emptyDir in Kubernetes).
type Workspaces struct {
	git  config.Git
	root string
	// resolve, if set, returns a repo's clone URL instead of CloneURL.
	resolve CloneURLFunc

	mu    sync.Mutex
	repos map[string]*Repo
}

// CloneURLFunc returns a repo's clone URL, e.g. NewCloneURLResolver.
type CloneURLFunc func(ctx context.Context, repo string) (string, error)

// NewWorkspaces creates a workspace manager rooted at dir. resolve, if not
// nil, provides clone URLs; otherwise they are <cloneBaseURL>/<repo>.git.
func NewWorkspaces(g config.Git, dir string, resolve CloneURLFunc) *Workspaces {
	return &Workspaces{git: g, root: dir, resolve: resolve, repos: map[string]*Repo{}}
}

// auth reads the token on every call, so a push long after the clone still
// uses a fresh token. No token set means anonymous access (e.g. public
// repos); a set token that cannot be read is an error, not anonymous access.
func (w *Workspaces) auth() (*githttp.BasicAuth, error) {
	if !w.git.Token.Set() {
		return nil, nil
	}
	tok, err := w.git.Token.Value()
	if err != nil {
		return nil, fmt.Errorf("git token: %w", err)
	}
	return &githttp.BasicAuth{Username: w.git.Username, Password: tok}, nil
}

// CloneURL returns the clone URL for owner/name.
func (w *Workspaces) CloneURL(repo string) string {
	return w.git.CloneBaseURL + "/" + repo + ".git"
}

// cloneURL returns resolve's URL when set, otherwise CloneURL.
func (w *Workspaces) cloneURL(ctx context.Context, repo string) (string, error) {
	if w.resolve == nil {
		return w.CloneURL(repo), nil
	}
	u, err := w.resolve(ctx, repo)
	if err != nil {
		return "", fmt.Errorf("look up clone URL: %w", err)
	}
	return u, nil
}

// Probe lists repo's refs like `git ls-remote`, without cloning, and
// returns the clone URL. heraion check --connect uses it to show that the
// Git server is reachable with the token, e.g. through a VPN or proxy.
func (w *Workspaces) Probe(ctx context.Context, repo string) (string, error) {
	u, err := w.cloneURL(ctx, repo)
	if err != nil {
		return "", err
	}
	auth, err := w.auth()
	if err != nil {
		return u, err
	}
	rem := git.NewRemote(memory.NewStorage(), &gitcfg.RemoteConfig{Name: "origin", URLs: []string{u}})
	if _, err := rem.ListContext(ctx, &git.ListOptions{Auth: auth}); err != nil {
		return u, fmt.Errorf("list refs: %w", err)
	}
	return u, nil
}

// Get returns the clone of repo, e.g. "acme/web", cloning it on first use.
// The caller checks the allowlist.
func (w *Workspaces) Get(ctx context.Context, repo string) (*Repo, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if r, ok := w.repos[repo]; ok {
		return r, nil
	}
	// The index keeps names unique: "a/b__c" and "a__b/c" would collide.
	dir := filepath.Join(w.root, fmt.Sprintf("%d-%s", len(w.repos), strings.ReplaceAll(repo, "/", "__")))
	if err := os.RemoveAll(dir); err != nil {
		return nil, err
	}
	cloneURL, err := w.cloneURL(ctx, repo)
	if err != nil {
		return nil, fmt.Errorf("clone %s: %w", repo, err)
	}
	auth, err := w.auth()
	if err != nil {
		return nil, fmt.Errorf("clone %s: %w", repo, err)
	}
	g, err := git.PlainCloneContext(ctx, dir, false, &git.CloneOptions{
		URL:          cloneURL,
		Auth:         auth,
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
	auth     func() (*githttp.BasicAuth, error)
	author   config.Git
}

// cleanPath validates a repo-relative path from the model, e.g.
// "./deploy//values.yaml" -> "deploy/values.yaml".
func cleanPath(p string) (string, error) {
	p = strings.TrimPrefix(path.Clean("/"+strings.TrimSpace(p)), "/")
	if p == "" {
		p = "."
	}
	if !filepath.IsLocal(filepath.FromSlash(p)) && p != "." {
		return "", fmt.Errorf("path %q escapes the repository", p)
	}
	for _, seg := range strings.Split(p, "/") {
		if strings.EqualFold(seg, ".git") { // also submodules' .git
			return "", errors.New("access to .git is not allowed")
		}
	}
	return p, nil
}

// Changed lists the paths that differ from the base commit, including
// deletions and files that arrived through a symlink.
func (r *Repo) Changed() ([]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	wt, err := r.g.Worktree()
	if err != nil {
		return nil, err
	}
	st, err := wt.Status()
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(st))
	for p, fs := range st {
		if fs.Worktree != git.Unmodified || fs.Staging != git.Unmodified {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out, nil
}

// ReadFile returns a file's content, capped at limit bytes.
func (r *Repo) ReadFile(p string, limit int) (string, error) {
	p, err := cleanPath(p)
	if err != nil {
		return "", err
	}
	// A link can point into .git, e.g. "notes.txt -> .git/config", which
	// cleanPath refuses by name. The model reads the target path instead.
	if err := r.noSymlink(p); err != nil {
		return "", fmt.Errorf("reading %s is not allowed: %w; read the path it points to instead", p, err)
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
	if err := r.noSymlink(p); err != nil {
		return fmt.Errorf("writing %s is not allowed: %w", p, err)
	}
	native := filepath.FromSlash(p)
	if dir := filepath.Dir(native); dir != "." {
		if err := r.root.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	return r.root.WriteFile(native, data, 0o644)
}

// noSymlink fails if p or one of its parent directories is a symlink, e.g.
// "notes.txt is a symlink to .git/config". The clone confines symlinks to
// itself, but .git is inside it too: such a link would let a write change
// the push URL.
func (r *Repo) noSymlink(p string) error {
	cur := ""
	for _, seg := range strings.Split(p, "/") {
		cur = path.Join(cur, seg)
		st, err := r.root.Lstat(filepath.FromSlash(cur))
		if errors.Is(err, fs.ErrNotExist) {
			return nil // the rest is created by the write
		}
		if err != nil {
			return err
		}
		if st.Mode()&fs.ModeSymlink != 0 {
			target, _ := r.root.Readlink(filepath.FromSlash(cur))
			return fmt.Errorf("%s is a symlink to %s", cur, filepath.ToSlash(target))
		}
	}
	return nil
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

// Commit commits all changes to a new branch and returns the diff from the
// base commit and its ChangeID. The message ends with the trailer
// "Heraion-Change: <change>", which RemoteHead reads back. The patch is nil
// if there was nothing to commit.
func (r *Repo) Commit(branch, message string) (*object.Patch, string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	wt, err := r.g.Worktree()
	if err != nil {
		return nil, "", err
	}
	st, err := wt.Status()
	if err != nil {
		return nil, "", err
	}
	if st.IsClean() {
		return nil, "", nil
	}
	ref := plumbing.NewBranchReferenceName(branch)
	// An earlier open_pr in this run may have left the local branch; we are
	// on the base branch again (Reset), so start it over.
	if err := r.g.Storer.RemoveReference(ref); err != nil {
		return nil, "", fmt.Errorf("reset branch %s: %w", branch, err)
	}
	if err := wt.Checkout(&git.CheckoutOptions{Branch: ref, Create: true, Keep: true}); err != nil {
		return nil, "", fmt.Errorf("create branch %s: %w", branch, err)
	}
	if err := wt.AddWithOptions(&git.AddOptions{All: true}); err != nil {
		return nil, "", err
	}
	sig := &object.Signature{Name: r.author.AuthorName, Email: r.author.AuthorEmail, When: time.Now()}
	h, err := wt.Commit(message, &git.CommitOptions{Author: sig, Committer: sig})
	if err != nil {
		return nil, "", err
	}
	head, err := r.g.CommitObject(h)
	if err != nil {
		return nil, "", err
	}
	base, err := r.g.CommitObject(r.baseHash)
	if err != nil {
		return nil, "", err
	}
	patch, err := base.Patch(head)
	if err != nil {
		return nil, "", err
	}
	// The change ID comes from the diff, so it is known only after the
	// commit; amending keeps the tree and replaces the message.
	change := ChangeID(patch)
	if _, err := wt.Commit(message+"\n\n"+changeTrailer(change), &git.CommitOptions{Amend: true, Author: sig, Committer: sig}); err != nil {
		return nil, "", fmt.Errorf("add change trailer: %w", err)
	}
	return patch, change, nil
}

// Push force-pushes branch. Only heraion/... branches are allowed, so force
// never overwrites someone else's branch.
func (r *Repo) Push(ctx context.Context, branch string) error {
	if !strings.HasPrefix(branch, BranchPrefix) {
		return fmt.Errorf("refusing to push branch %q outside %s", branch, BranchPrefix)
	}
	ref := plumbing.NewBranchReferenceName(branch)
	return r.push(ctx, gitcfg.RefSpec("+"+ref+":"+ref))
}

// RemoteHead describes the head commit of branch on the remote: ours is
// true when HeraIon made it (author git.authorEmail), so a force-push never
// overwrites a person's commits on an open PR, and change is its
// "Heraion-Change" trailer, or "". A missing branch counts as HeraIon's,
// with no change.
func (r *Repo) RemoteHead(ctx context.Context, branch string) (ours bool, change string, err error) {
	auth, err := r.auth()
	if err != nil {
		return false, "", err
	}
	rem, err := r.g.Remote("origin")
	if err != nil {
		return false, "", err
	}
	refs, err := rem.ListContext(ctx, &git.ListOptions{Auth: auth})
	if err != nil {
		return false, "", fmt.Errorf("list remote branches: %w", err)
	}
	ref := plumbing.NewBranchReferenceName(branch)
	var head plumbing.Hash
	for _, x := range refs {
		if x.Name() == ref {
			head = x.Hash()
		}
	}
	if head.IsZero() {
		return true, "", nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	spec := gitcfg.RefSpec("+" + ref.String() + ":refs/remotes/origin/" + branch)
	err = r.g.FetchContext(ctx, &git.FetchOptions{RemoteName: "origin", RefSpecs: []gitcfg.RefSpec{spec}, Depth: 1, Auth: auth, Tags: git.NoTags})
	if err != nil && !errors.Is(err, git.NoErrAlreadyUpToDate) {
		return false, "", fmt.Errorf("fetch %s: %w", branch, err)
	}
	c, err := r.g.CommitObject(head)
	if err != nil {
		return false, "", err
	}
	return strings.EqualFold(c.Author.Email, r.author.AuthorEmail), changeOfCommit(c.Message), nil
}

// push pushes one refspec to origin, e.g. "+refs/heads/a:refs/heads/a".
func (r *Repo) push(ctx context.Context, spec gitcfg.RefSpec) error {
	auth, err := r.auth()
	if err != nil {
		return err
	}
	err = r.g.PushContext(ctx, &git.PushOptions{RemoteName: "origin", RefSpecs: []gitcfg.RefSpec{spec}, Auth: auth})
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
