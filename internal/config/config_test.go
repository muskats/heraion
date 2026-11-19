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

func TestJiraDefaultsAndValidation(t *testing.T) {
	t.Setenv(EnvJiraToken, "tok")
	c, err := Load(write(t, "llm: {baseURL: http://x, model: m}\njira: {url: https://acme.atlassian.net/, projects: [OPS]}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c.Jira.Deployment != JiraCloud || c.Jira.IssueType != "Task" || c.Jira.URL != "https://acme.atlassian.net" || c.Jira.Token.String() != "tok" {
		t.Errorf("%+v", c.Jira)
	}
	c, err = Load(write(t, "llm: {baseURL: http://x, model: m}\njira: {url: https://jira.example.internal, projects: [OPS]}\n"))
	if err != nil || c.Jira.Deployment != JiraDataCenter {
		t.Errorf("self-hosted must default to datacenter: %v %v", err, c.Jira)
	}
	_, err = Load(write(t, "llm: {baseURL: http://x, model: m}\njira: {url: ftp://x, deployment: server, projects: [ops], labels: [a b]}\n"))
	for _, want := range []string{"jira.url", "jira.deployment", `"ops" is not a space (project) key`, `"a b"`} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("error %v does not mention %s", err, want)
		}
	}
}

func TestConfluenceDefaults(t *testing.T) {
	c, err := Load(write(t, "llm: {baseURL: http://x, model: m}\nconfluence: {url: https://acme.atlassian.net, spaces: [RUN, ~5b10ac8d82e05b22cc7d4ef5]}\njira: {url: https://acme.atlassian.net, readSpaces: [CHG]}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c.Confluence.URL != "https://acme.atlassian.net/wiki" || c.Confluence.Deployment != DeploymentCloud || c.Confluence.MaxPageBytes != 32<<10 {
		t.Errorf("%+v", c.Confluence)
	}
	if len(c.Jira.Projects) != 0 || c.Jira.ReadSpaces[0] != "CHG" {
		t.Errorf("read-only jira %+v", c.Jira)
	}
	c, err = Load(write(t, "llm: {baseURL: http://x, model: m}\nconfluence: {url: https://wiki.example.com, spaces: [RUN]}\n"))
	if err != nil || c.Confluence.URL != "https://wiki.example.com" || c.Confluence.Deployment != DeploymentDataCenter {
		t.Errorf("data center: %v %+v", err, c.Confluence)
	}
	if _, err := Load(write(t, "llm: {baseURL: http://x, model: m}\nconfluence: {url: https://wiki.example.com, spaces: [\"bad key\"]}\n")); err == nil {
		t.Error("bad space key must fail")
	}
	if _, err := Load(write(t, "llm: {baseURL: http://x, model: m}\njira: {url: https://x.example.com}\n")); err == nil {
		t.Error("jira without projects and readSpaces must fail")
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
