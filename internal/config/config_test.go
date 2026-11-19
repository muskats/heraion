package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func write(t *testing.T, s string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadWithEnvAndDefaults(t *testing.T) {
	t.Setenv(EnvLLMAPIKey, "sk-test-key")
	t.Setenv(EnvGitToken, "git-token-value")
	t.Setenv(EnvDryRun, "true")
	p := write(t, `
llm:
  baseURL: http://litellm:4000/v1/
  model: gpt-4.1
  timeout: 5m
git:
  provider: gitea
  endpoint: https://git.internal.example
repos: [acme/app, group/sub/svc]
`)
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.LLM.APIKey.String() != "sk-test-key" || c.Git.Token.String() != "git-token-value" {
		t.Error("secrets not read from env")
	}
	if !c.DryRun {
		t.Error("HERAION_DRY_RUN not applied")
	}
	if c.LLM.BaseURL != "http://litellm:4000/v1" {
		t.Errorf("trailing slash not trimmed: %s", c.LLM.BaseURL)
	}
	if c.LLM.Timeout.Duration != 5*time.Minute || c.LLM.MaxSteps != 30 {
		t.Errorf("timeout/maxSteps: %v %d", c.LLM.Timeout, c.LLM.MaxSteps)
	}
	if c.Git.CloneBaseURL != "https://git.internal.example" {
		t.Errorf("cloneBaseURL default: %s", c.Git.CloneBaseURL)
	}
}

func TestGitHubDefaults(t *testing.T) {
	c, err := Load(write(t, "llm: {baseURL: https://api.openai.com/v1, model: m}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c.Git.Provider != "github" || c.Git.CloneBaseURL != "https://github.com" {
		t.Errorf("got %s %s", c.Git.Provider, c.Git.CloneBaseURL)
	}
}

func TestValidationErrors(t *testing.T) {
	_, err := Load(write(t, `
llm: {baseURL: "ftp://x", model: ""}
git: {provider: svn}
repos: [noslash, "a/../b"]
`))
	if err == nil {
		t.Fatal("expected errors")
	}
	for _, want := range []string{"llm.baseURL", "llm.model", "git.provider", `"noslash"`, `"a/../b"`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %s", err, want)
		}
	}
}

func TestUnknownFieldRejected(t *testing.T) {
	if _, err := Load(write(t, "llm: {baseURL: http://x, model: m, apiKey: oops}\n")); err == nil {
		t.Fatal("apiKey in file must be rejected (secrets come from env only)")
	}
}

func TestEnvOverridesFile(t *testing.T) {
	t.Setenv(EnvLLMModel, "other-model")
	c, err := Load(write(t, "llm: {baseURL: http://x, model: m}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c.LLM.Model != "other-model" {
		t.Errorf("model = %s", c.LLM.Model)
	}
}

func TestExampleConfigIsValid(t *testing.T) {
	if _, err := Load("../../examples/config.yaml"); err != nil {
		t.Fatal(err)
	}
}

func TestSecretHierarchy(t *testing.T) {
	tok := filepath.Join(t.TempDir(), "token")
	_ = os.WriteFile(tok, []byte("from-sa-token"), 0o600)
	t.Setenv(EnvLLMAPIKey+"_FILE", tok)
	t.Setenv(EnvLLMAPIKey, "from-secret")
	t.Setenv(EnvGitToken+"_FALLBACK", "literal-token")
	c, err := Load(write(t, "llm: {baseURL: http://x, model: m}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c.LLM.APIKey.String() != "from-sa-token" || c.LLM.APIKey.Origin() != "file" {
		t.Errorf("llm key %q from %s", c.LLM.APIKey.String(), c.LLM.APIKey.Origin())
	}
	if c.Git.Token.String() != "literal-token" || c.Git.Token.Origin() != "fallback" {
		t.Errorf("git token %q from %s", c.Git.Token.String(), c.Git.Token.Origin())
	}
}
