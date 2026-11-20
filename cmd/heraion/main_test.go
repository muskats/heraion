package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/muskats/heraion/internal/guardrails"
	"github.com/muskats/heraion/internal/llm/llmtest"
)

// capture swaps stdout and stderr for buffers until the test ends.
func capture(t *testing.T) (out, errOut *bytes.Buffer) {
	t.Helper()
	out, errOut = &bytes.Buffer{}, &bytes.Buffer{}
	oldOut, oldErr := stdout, stderr
	stdout, stderr = out, errOut
	t.Cleanup(func() { stdout, stderr = oldOut, oldErr })
	return out, errOut
}

// setup writes a config and two manual playbooks, triage (pr) and digest
// (report), and returns the config path.
func setup(t *testing.T, llmURL, extra string) string {
	t.Helper()
	dir := t.TempDir()
	pbs := filepath.Join(dir, "playbooks")
	if err := os.Mkdir(pbs, 0o755); err != nil {
		t.Fatal(err)
	}
	pb := "name: triage\ntrigger: {manual: true}\ntools: ['*']\nprompt: Triage the event.\n"
	if err := os.WriteFile(filepath.Join(pbs, "triage.yaml"), []byte(pb), 0o644); err != nil {
		t.Fatal(err)
	}
	// A report playbook opens no PRs, so check shows no mode for it.
	rp := "name: digest\ntrigger: {manual: true}\ntools: ['*']\noutput: report\nprompt: Summarize.\n"
	if err := os.WriteFile(filepath.Join(pbs, "digest.yaml"), []byte(rp), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := "llm: {baseURL: " + llmURL + ", model: m}\nplaybooksDir: " + pbs + "\nworkDir: " + dir + "\n" + extra
	p := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(p, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestEnvOr(t *testing.T) {
	t.Setenv("HERAION_TEST_X", "set")
	if envOr("HERAION_TEST_X", "def") != "set" || envOr("HERAION_TEST_UNSET", "def") != "def" {
		t.Fatal("envOr")
	}
}

func TestLoggerMasksSecrets(t *testing.T) {
	_, errOut := capture(t)
	log := newLogger("debug", guardrails.NewRedactor("configured-secret-1"))
	log.Info("x", "msg", "token ghp_abcdefghijklmnopqrstuvwxyz0123", "err", errors.New("auth configured-secret-1 failed"))
	got := errOut.String()
	if strings.Contains(got, "ghp_") || strings.Contains(got, "configured-secret-1") || strings.Count(got, "[REDACTED]") != 2 {
		t.Fatalf("not masked: %s", got)
	}
	// An unknown level falls back to info.
	errOut.Reset()
	newLogger("loud", guardrails.NewRedactor()).Debug("hidden")
	if errOut.Len() != 0 {
		t.Fatalf("debug logged at the default level: %s", errOut)
	}
}

func TestNamespace(t *testing.T) {
	if ns, _ := namespace("configured"); ns != "configured" {
		t.Errorf("configured namespace: %s", ns)
	}
	t.Setenv("POD_NAMESPACE", "from-env")
	if ns, _ := namespace(""); ns != "from-env" {
		t.Errorf("POD_NAMESPACE: %s", ns)
	}
}

func TestCheckConnectsToLLM(t *testing.T) {
	for _, tc := range []struct {
		status   int
		wantErr  bool
		wantLine string
	}{
		{200, false, "ok    llm"},
		{401, true, "FAIL  llm"},
		{404, false, "warn  llm"},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/v1/models" || r.Header.Get("Authorization") != "Bearer sk-check" {
				t.Errorf("request %s auth %q", r.URL.Path, r.Header.Get("Authorization"))
			}
			w.WriteHeader(tc.status)
		}))
		t.Setenv("HERAION_LLM_API_KEY", "sk-check")
		out, _ := capture(t)
		err := cmdCheck(context.Background(), []string{"--config", setup(t, srv.URL+"/v1", ""), "--connect"})
		srv.Close()
		if (err != nil) != tc.wantErr || !strings.Contains(out.String(), tc.wantLine) {
			t.Errorf("status %d: err=%v\n%s", tc.status, err, out)
		}
		if !strings.Contains(out.String(), "ok    playbooks  digest, triage (full)") {
			t.Errorf("playbooks line missing:\n%s", out)
		}
		if strings.Contains(out.String(), "sk-check") {
			t.Error("check must never print secret values")
		}
	}
}

func TestCheckRoles(t *testing.T) {
	hooks := "gateway:\n  hooks:\n    am: {auth: token, secretEnv: HERAION_HOOK_AM_SECRET}\n"
	out, _ := capture(t)
	if err := cmdCheck(context.Background(), []string{"--config", setup(t, "http://x", hooks), "--role", "gateway"}); err == nil {
		t.Fatalf("a missing hook secret must fail the gateway check:\n%s", out)
	}
	if !strings.Contains(out.String(), "FAIL  secret     HERAION_HOOK_AM_SECRET for hook am") {
		t.Errorf("hook secret line:\n%s", out)
	}

	out.Reset()
	t.Setenv("HERAION_HOOK_AM_SECRET", "s3cret-value")
	if err := cmdCheck(context.Background(), []string{"--config", setup(t, "http://x", hooks), "--role", "gateway"}); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}

	// Worker role: an unset optional token only warns.
	out.Reset()
	if err := cmdCheck(context.Background(), []string{"--config", setup(t, "http://x", "")}); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !strings.Contains(out.String(), "warn  secret     HERAION_GIT_TOKEN not set") {
		t.Errorf("git token warning:\n%s", out)
	}
}

func TestCheckReportsBadConfig(t *testing.T) {
	out, _ := capture(t)
	// An empty hook used to crash `check --role gateway`.
	bad := setup(t, "http://x", "gateway:\n  hooks:\n    empty:\n")
	if err := cmdCheck(context.Background(), []string{"--config", bad, "--role", "gateway"}); err == nil {
		t.Fatal("expected failure")
	}
	if !strings.Contains(out.String(), "FAIL  config") || !strings.Contains(out.String(), "gateway.hooks.empty: empty") {
		t.Errorf("output:\n%s", out)
	}
}

func TestRunDryRunWithEventFile(t *testing.T) {
	srv := llmtest.New(t, llmtest.Turn{Final: "triaged"})
	cfg := setup(t, srv.BaseURL(), "")
	event := filepath.Join(t.TempDir(), "event.json")
	_ = os.WriteFile(event, []byte(`{"alert":"OOMKilled","token":"ghp_abcdefghijklmnopqrstuvwxyz0123"}`), 0o600)
	_, errOut := capture(t)
	if err := cmdRun(context.Background(), []string{"--config", cfg, "--playbook", "triage", "--event", event, "--dry-run"}); err != nil {
		t.Fatalf("%v\n%s", err, errOut)
	}
	user := srv.Requests[0].Messages[1].Text()
	if !strings.Contains(user, "OOMKilled") || strings.Contains(user, "ghp_") {
		t.Errorf("event not passed or not masked: %s", user)
	}
	for _, want := range []string{`"msg":"run finished"`, `"playbook":"triage"`, `"status":"ok"`, `"durationSeconds":`, `"dryRun":true`} {
		if !strings.Contains(errOut.String(), want) {
			t.Errorf("run finished line lacks %s:\n%s", want, errOut)
		}
	}
}

func TestRunErrors(t *testing.T) {
	capture(t)
	cfg := setup(t, "http://x", "")
	for name, args := range map[string][]string{
		"no playbook":      {"--config", cfg},
		"unknown playbook": {"--config", cfg, "--playbook", "nope"},
		"missing event":    {"--config", cfg, "--playbook", "triage", "--event", "/nonexistent"},
		"missing config":   {"--config", "/nonexistent", "--playbook", "triage"},
	} {
		if err := cmdRun(context.Background(), args); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
	if err := cmdRun(context.Background(), []string{"-h"}); !errors.Is(err, flag.ErrHelp) {
		t.Errorf("-h: %v", err)
	}
}

func TestServeNeedsJobTemplate(t *testing.T) {
	capture(t)
	err := cmdServe(context.Background(), []string{"--config", setup(t, "http://x", "")})
	if err == nil || !strings.Contains(err.Error(), "gateway.jobTemplate is required") {
		t.Fatalf("got %v", err)
	}
}
