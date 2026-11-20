package guardrails

import (
	"strings"
	"testing"

	"github.com/muskats/heraion/internal/secret"
)

func TestRedact(t *testing.T) {
	r := NewRedactor("my-llm-key-123", "abc", "")
	in := strings.Join([]string{
		"key=my-llm-key-123",
		"gh=ghp_abcdefghijklmnopqrstuvwxyz0123",
		"gl=glpat-abcdefghijklmnopqrstu",
		"Authorization: Bearer eyJhbGciOiJIUzI1NiJ9.payload.sig",
		"aws=AKIAABCDEFGHIJKLMNOP",
		"-----BEGIN RSA PRIVATE KEY-----\nMIIE\n-----END RSA PRIVATE KEY-----",
		"short abc stays",
	}, "\n")
	out := r.Redact(in)
	for _, leak := range []string{"my-llm-key-123", "ghp_", "glpat-", "eyJhbGci", "AKIA", "MIIE"} {
		if strings.Contains(out, leak) {
			t.Errorf("leaked %q in:\n%s", leak, out)
		}
	}
	if !strings.Contains(out, "short abc stays") {
		t.Error("short values must not be redacted")
	}
}

func TestGuard(t *testing.T) {
	g := New(Options{Repos: []string{"a/b"}, MaxPRs: 1, MaxFileBytes: 10})
	if g.CheckRepo("a/b") != nil || g.CheckRepo("a/c") == nil {
		t.Error("repo allowlist")
	}
	if g.CheckFileSize(10) != nil || g.CheckFileSize(11) == nil {
		t.Error("file size")
	}
	if g.ReservePR() != nil || g.ReservePR() == nil {
		t.Error("PR budget")
	}
	g.ReleasePR()
	if g.ReservePR() != nil {
		t.Error("release must free a slot")
	}
}

func TestRotatedSecretStaysMasked(t *testing.T) {
	env := map[string]string{"K": "first-token-value"}
	r := NewRedactor()
	r.AddSources(secret.FromMap("K", env))
	_ = r.Redact("warm up")
	env["K"] = "second-token-value"
	out := r.Redact("old=first-token-value new=second-token-value")
	if strings.Contains(out, "first-token") || strings.Contains(out, "second-token") {
		t.Fatalf("leak: %s", out)
	}
}

func TestCheckWritePath(t *testing.T) {
	g := New(Options{MaxPRs: 1})
	for _, p := range []string{
		".github/workflows/ci.yml", ".GITHUB/Workflows/ci.yml", ".github/workflows",
		"CODEOWNERS", "docs/CODEOWNERS", ".gitlab-ci.yml", "build/Jenkinsfile", ".gitmodules",
	} {
		if g.CheckWritePath(p) == nil {
			t.Errorf("%s must be refused", p)
		}
	}
	for _, p := range []string{"Dockerfile", "deploy/values.yaml", ".github/dependabot.yml", "workflows/ci.yml"} {
		if err := g.CheckWritePath(p); err != nil {
			t.Errorf("%s: %v", p, err)
		}
	}
	g = New(Options{AllowPaths: []string{"Dockerfile", "deploy/**"}, DenyPaths: []string{"deploy/secrets/**"}})
	if g.CheckWritePath("deploy/a/values.yaml") != nil || g.CheckWritePath("Dockerfile") != nil {
		t.Error("allowed paths refused")
	}
	if g.CheckWritePath("main.go") == nil || g.CheckWritePath("deploy/secrets/x") == nil {
		t.Error("allow/deny lists not applied")
	}
}

func TestMatchPath(t *testing.T) {
	cases := []struct {
		pat, name string
		want      bool
	}{
		{"**/values.yaml", "values.yaml", true},
		{"**/values.yaml", "a/b/values.yaml", true},
		{"deploy/*.yaml", "deploy/a.yaml", true},
		{"deploy/*.yaml", "deploy/x/a.yaml", false},
		{"a/**/b", "a/b", true},
		{"a/**/b", "a/x/y/b", true},
		{"deploy/**", "deploy", true},
		{"deploy/**", "deploy/a/values.yaml", true},
		{".GitHub/Workflows/**", ".github/workflows/ci.yml", true},
		{"[bad", "[bad", false},
	}
	for _, c := range cases {
		if got := MatchPath(c.pat, c.name); got != c.want {
			t.Errorf("MatchPath(%q, %q) = %v", c.pat, c.name, got)
		}
	}
}

func TestNewPatterns(t *testing.T) {
	r := NewRedactor()
	in := "jira=ATATT3xFfGF0abcdefghijklmnopqrstuvwxyz\n" +
		"sa=eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiJzeXN0ZW0.c2lnbmF0dXJlLXZhbHVl\n" +
		"db=postgres://app:hunter2@db.internal:5432/x"
	out := r.Redact(in)
	for _, leak := range []string{"ATATT3", "eyJhbGci", "hunter2"} {
		if strings.Contains(out, leak) {
			t.Errorf("leaked %q in %s", leak, out)
		}
	}
	if !strings.Contains(out, "postgres://[REDACTED]@db.internal") {
		t.Errorf("URL scheme and host must stay: %s", out)
	}
}

func TestFindSecretLines(t *testing.T) {
	r := NewRedactor("supersecret-token")
	patch := gitPatch(t, files("f", "context ghp_abcdefghijklmnopqrstuvwxyz0123\nold supersecret-token\n"),
		files("f", "context ghp_abcdefghijklmnopqrstuvwxyz0123\nimage: app:2\ntoken: supersecret-token\n"))
	got := r.FindSecretLines(patch)
	if len(got) != 1 || got[0] != "+token: [REDACTED]" {
		t.Fatalf("got %q", got)
	}
}

func TestFindSecretLinesPrivateKey(t *testing.T) {
	key := "-----BEGIN RSA PRIVATE KEY-----\nMIIEowIBAAKCAQEA\n-----END RSA PRIVATE KEY-----\n"
	patch := gitPatch(t, files("f", "a\n"), files("f", "a\n"+key))
	if got := NewRedactor().FindSecretLines(patch); len(got) != 1 || got[0] != "+[REDACTED]" {
		t.Fatalf("got %q", got)
	}
}

func TestIssueBudgetAndProjects(t *testing.T) {
	g := New(Options{DryRun: true, JiraProjects: []string{"OPS"}, MaxIssues: 1})
	if g.CheckProject("OPS") != nil || g.CheckProject("SEC") == nil {
		t.Error("project allowlist")
	}
	if g.ReserveIssue() != nil || g.ReserveIssue() == nil {
		t.Error("issue budget")
	}
	g.ReleaseIssue()
	g.ReleaseIssue() // never below zero
	if g.ReserveIssue() != nil || g.ReserveIssue() == nil {
		t.Error("release must free exactly one slot")
	}
	if !g.DryRun() || g.Redactor() == nil || len(g.Projects()) != 1 {
		t.Error("accessors")
	}
	if g.ReservePR() == nil {
		t.Error("maxPRs 0 allows no PR")
	}
}
