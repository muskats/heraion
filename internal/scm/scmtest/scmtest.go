// Package scmtest provides a local bare Git remote and an in-memory
// PRService for tests.
package scmtest

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/muskats/heraion/internal/scm"
)

// NewRemote creates a bare repository at <base>/<repo>.git containing files
// on branch main, and returns "file://<base>" for use as git.cloneBaseURL.
func NewRemote(t testing.TB, base, repo string, files map[string]string) string {
	t.Helper()
	src := t.TempDir()
	r, err := git.PlainInitWithOptions(src, &git.PlainInitOptions{
		InitOptions: git.InitOptions{DefaultBranch: "refs/heads/main"},
	})
	if err != nil {
		t.Fatal(err)
	}
	for p, c := range files {
		full := filepath.Join(src, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	wt, _ := r.Worktree()
	if err := wt.AddWithOptions(&git.AddOptions{All: true}); err != nil {
		t.Fatal(err)
	}
	sig := &object.Signature{Name: "test", Email: "test@example.com", When: time.Now()}
	if _, err := wt.Commit("initial", &git.CommitOptions{Author: sig}); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(base, filepath.FromSlash(repo)+".git")
	if _, err := git.PlainClone(dst, true, &git.CloneOptions{URL: src}); err != nil {
		t.Fatal(err)
	}
	return "file://" + base
}

// CommitToBranch commits files to branch of the bare remote as author
// email, e.g. a reviewer's fixup on HeraIon's PR branch.
func CommitToBranch(t testing.TB, base, repo, branch, email string, files map[string]string) {
	t.Helper()
	dir := t.TempDir()
	r, err := git.PlainClone(dir, false, &git.CloneOptions{
		URL: filepath.Join(base, filepath.FromSlash(repo)+".git"), ReferenceName: plumbing.NewBranchReferenceName(branch), SingleBranch: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	for p, c := range files {
		if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(p)), []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	wt, _ := r.Worktree()
	if err := wt.AddWithOptions(&git.AddOptions{All: true}); err != nil {
		t.Fatal(err)
	}
	sig := &object.Signature{Name: "someone", Email: email, When: time.Now()}
	if _, err := wt.Commit("fixup", &git.CommitOptions{Author: sig}); err != nil {
		t.Fatal(err)
	}
	if err := r.Push(&git.PushOptions{}); err != nil {
		t.Fatal(err)
	}
}

// RemoteBranchFile reads a file from a branch of the bare remote.
func RemoteBranchFile(t testing.TB, base, repo, branch, path string) (string, error) {
	t.Helper()
	r, err := git.PlainOpen(filepath.Join(base, filepath.FromSlash(repo)+".git"))
	if err != nil {
		return "", err
	}
	ref, err := r.Reference(plumbing.NewBranchReferenceName(branch), true)
	if err != nil {
		return "", err
	}
	c, err := r.CommitObject(ref.Hash())
	if err != nil {
		return "", err
	}
	f, err := c.File(path)
	if err != nil {
		return "", err
	}
	return f.Contents()
}

// FakePRs is an in-memory scm.PRService.
type FakePRs struct {
	mu      sync.Mutex
	Created []scm.PRInput
	Open    map[string]*scm.PR // key: repo + " " + branch
	// Closed holds PRs closed without merging (or merged), by the same key.
	Closed map[string][]scm.PR
	// Updated are the inputs of Update calls.
	Updated []scm.PRInput
	// Comments are the comments posted, e.g. {Repo: "acme/app", Number: 1, Body: "..."}.
	Comments []Comment
	// CommentErr, when set, is returned by Comment.
	CommentErr error
	// UpdateErr, when set, is returned by Update, which then changes nothing,
	// e.g. go-scm's ErrNotSupported on Gogs.
	UpdateErr error
}

// Comment is one comment posted through FakePRs.
type Comment struct {
	Repo   string
	Number int
	Body   string
}

// Update replaces the title and body of the open PR number, if any.
func (f *FakePRs) Update(_ context.Context, repo string, number int, in scm.PRInput) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.UpdateErr != nil {
		return f.UpdateErr
	}
	f.Updated = append(f.Updated, in)
	for _, pr := range f.Open {
		if pr.Number == number {
			pr.Body = in.Body
		}
	}
	return nil
}

func (f *FakePRs) Comment(_ context.Context, repo string, number int, body string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.CommentErr != nil {
		return f.CommentErr
	}
	f.Comments = append(f.Comments, Comment{Repo: repo, Number: number, Body: body})
	return nil
}

func (f *FakePRs) FindByBranch(_ context.Context, repo, branch string) ([]scm.PR, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []scm.PR
	if pr := f.Open[repo+" "+branch]; pr != nil {
		p := *pr
		p.State = scm.PRStateOpen
		out = append(out, p)
	}
	return append(out, f.Closed[repo+" "+branch]...), nil
}

func (f *FakePRs) Create(_ context.Context, repo string, in scm.PRInput) (*scm.PR, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Open == nil {
		f.Open = map[string]*scm.PR{}
	}
	f.Created = append(f.Created, in)
	pr := &scm.PR{Number: len(f.Created), URL: fmt.Sprintf("https://git.example/%s/pull/%d", repo, len(f.Created)), State: scm.PRStateOpen, Body: in.Body}
	f.Open[repo+" "+in.Head] = pr
	return pr, nil
}
