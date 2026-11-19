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
	g := New([]string{"a/b"}, 1, false, 10, nil)
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
