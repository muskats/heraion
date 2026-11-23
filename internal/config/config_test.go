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

func TestBitbucketCloudDefaults(t *testing.T) {
	c, err := Load(write(t, "llm: {baseURL: https://api.openai.com/v1, model: m}\ngit: {provider: bitbucket}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c.Git.CloneBaseURL != "https://bitbucket.org" {
		t.Errorf("got %s", c.Git.CloneBaseURL)
	}
}

func TestNegativeLimitsRejected(t *testing.T) {
	_, err := Load(write(t, `
llm: {baseURL: https://api.openai.com/v1, model: m, maxTokens: -1, timeout: -1m}
gateway: {maxConcurrentJobs: -1}
`))
	for _, want := range []string{"llm.maxTokens", "llm.timeout", "gateway.maxConcurrentJobs"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("error %v does not mention %s", err, want)
		}
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

func TestJiraDefaultsAndValidation(t *testing.T) {
	base := "llm: {baseURL: http://x, model: m}\nmcpServers: {atlassian: {url: http://mcp:9000/mcp}}\n"
	c, err := Load(write(t, base+"jira: {server: atlassian, projects: [OPS]}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c.Jira.IssueType != "Task" || c.Jira.Server != "atlassian" {
		t.Errorf("%+v", c.Jira)
	}
	_, err = Load(write(t, base+"jira: {server: nope, projects: [ops], labels: [a b]}\n"))
	for _, want := range []string{`jira.server "nope"`, `"ops" is not a project key`, `"a b"`} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("want %q in %v", want, err)
		}
	}
	if _, err := Load(write(t, base+"jira: {server: atlassian}\n")); err == nil {
		t.Error("jira without projects must fail")
	}
	// The removed keys are rejected, not ignored.
	if _, err := Load(write(t, base+"jira: {server: atlassian, projects: [OPS], url: https://x}\n")); err == nil {
		t.Error("jira.url must be rejected")
	}
}

func TestNilHookAndBadToolPatternRejected(t *testing.T) {
	_, err := Load(write(t, `
llm: {baseURL: http://x, model: m}
mcpServers:
  k8s: {url: http://k8s/mcp, denyTools: ["[pods"]}
gateway:
  hooks:
    empty:
`))
	if err == nil {
		t.Fatal("expected errors")
	}
	for _, want := range []string{"gateway.hooks.empty: empty", `tool pattern "[pods"`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %s", err, want)
		}
	}
}

func TestStashResolvesCloneURL(t *testing.T) {
	c, err := Load(write(t, "llm: {baseURL: http://x, model: m}\ngit: {provider: stash, endpoint: https://bitbucket.example}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c.Git.CloneBaseURL != "" || !c.Git.ResolvesCloneURL() {
		t.Fatalf("stash must look clone URLs up, got base %q", c.Git.CloneBaseURL)
	}
	c, err = Load(write(t, "llm: {baseURL: http://x, model: m}\ngit: {provider: stash, endpoint: https://b.example, cloneBaseURL: https://b.example/scm}\n"))
	if err != nil || c.Git.ResolvesCloneURL() {
		t.Fatalf("an explicit cloneBaseURL wins: %v", err)
	}
}

func TestFileURLOnlyForCloneBase(t *testing.T) {
	if _, err := Load(write(t, "llm: {baseURL: file:///etc, model: m}\n")); err == nil {
		t.Error("file:// must be refused for llm.baseURL")
	}
	if _, err := Load(write(t, "llm: {baseURL: http://x, model: m}\ngit: {provider: gitea, cloneBaseURL: file:///repos}\n")); err != nil {
		t.Errorf("file:// cloneBaseURL: %v", err)
	}
}

func TestMode(t *testing.T) {
	c, err := Load(write(t, "llm: {baseURL: https://api.openai.com/v1, model: m}\n"))
	if err != nil || c.Mode != ModeFull {
		t.Fatalf("default mode: %q %v", c.Mode, err)
	}
	if _, err := Load(write(t, "llm: {baseURL: https://api.openai.com/v1, model: m}\nmode: reckless\n")); err == nil ||
		!strings.Contains(err.Error(), "mode") {
		t.Errorf("invalid mode: %v", err)
	}
	_, err = Load(write(t, "llm: {baseURL: https://api.openai.com/v1, model: m}\nmode: yolo\n"))
	if err == nil || !strings.Contains(err.Error(), ErrYoloRemoved) {
		t.Errorf("mode yolo: %v", err)
	}
	for _, c := range []struct{ a, b, want Mode }{
		{ModeFull, ModeSafe, ModeSafe}, {ModeSafe, ModeFull, ModeSafe}, {ModeFull, ModeFull, ModeFull},
	} {
		if got := Safer(c.a, c.b); got != c.want {
			t.Errorf("Safer(%q, %q) = %q, want %q", c.a, c.b, got, c.want)
		}
	}
}

func TestNamespaces(t *testing.T) {
	c, err := Load(write(t, "llm: {baseURL: http://x, model: m}\nnamespaces: [shop, kube-system]\n"))
	if err != nil || len(c.Namespaces) != 2 {
		t.Fatalf("%v %v", c, err)
	}
	if _, err := Load(write(t, "llm: {baseURL: http://x, model: m}\nnamespaces: [Shop_1]\n")); err == nil ||
		!strings.Contains(err.Error(), "namespaces") {
		t.Errorf("invalid namespace: %v", err)
	}
}

func TestMetricsListen(t *testing.T) {
	if _, err := Load(write(t, "llm: {baseURL: http://x, model: m}\ngateway: {listen: ':8080', metricsListen: ':8080'}\n")); err == nil {
		t.Error("metrics on the webhook port must be refused")
	}
	if c, err := Load(write(t, "llm: {baseURL: http://x, model: m}\ngateway: {metricsListen: ':9090'}\n")); err != nil || c.Gateway.MetricsListen != ":9090" {
		t.Errorf("%v %v", c, err)
	}
}

func TestResultAndContextLimits(t *testing.T) {
	c, err := Load(write(t, "llm: {baseURL: https://api.openai.com/v1, model: m}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c.LLM.MaxToolResultBytes != 16384 || c.LLM.MaxContextBytes != 200000 {
		t.Errorf("defaults: %d %d", c.LLM.MaxToolResultBytes, c.LLM.MaxContextBytes)
	}
	for _, bad := range []string{
		"maxToolResultBytes: 512",
		"maxToolResultBytes: 300000",
		"{maxToolResultBytes: 65536, maxContextBytes: 4096}",
		"maxContextBytes: -1",
	} {
		y := "llm: {baseURL: https://api.openai.com/v1, model: m, " + strings.Trim(bad, "{}") + "}\n"
		if _, err := Load(write(t, y)); err == nil {
			t.Errorf("%s: want an error", bad)
		}
	}
	if _, err := Load(write(t, "llm: {baseURL: https://api.openai.com/v1, model: m, maxToolResultBytes: 1024, maxContextBytes: 1024}\n")); err != nil {
		t.Errorf("bounds must be accepted: %v", err)
	}
}

func TestCheckCredentials(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope")
	c := &Config{}
	c.applyEnv(func(k string) string {
		return map[string]string{EnvGitToken + "_FILE": missing, EnvGitToken + "_FALLBACK": "lit"}[k]
	})
	err := c.CheckCredentials()
	if err == nil || !strings.Contains(err.Error(), "git token: HERAION_GIT_TOKEN_FILE") {
		t.Fatalf("got %v", err)
	}
	c.applyEnv(func(string) string { return "" })
	if err := c.CheckCredentials(); err != nil {
		t.Fatalf("unset credentials are fine: %v", err)
	}
}

func TestHookIdentity(t *testing.T) {
	c, err := Load(write(t, "llm: {baseURL: http://x, model: m}\ngateway: {hooks: {am: {insecure: true}}}\n"))
	if err != nil || c.Gateway.Hooks["am"].Identity != IdentityRaw {
		t.Fatalf("default identity: %v %v", c, err)
	}
	if _, err := Load(write(t, "llm: {baseURL: http://x, model: m}\ngateway: {hooks: {am: {insecure: true, identity: alertmanager}}}\n")); err != nil {
		t.Errorf("alertmanager identity: %v", err)
	}
	if _, err := Load(write(t, "llm: {baseURL: http://x, model: m}\ngateway: {hooks: {am: {insecure: true, identity: body}}}\n")); err == nil ||
		!strings.Contains(err.Error(), "identity") {
		t.Errorf("unknown identity: %v", err)
	}
}
