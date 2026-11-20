// Package config loads the runtime configuration. Settings come from a YAML
// file, secrets only from env vars:
//
//	llm: {baseURL: https://api.openai.com/v1, model: gpt-4.1}
//	HERAION_LLM_API_KEY=sk-...
package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path"
	"regexp"
	"slices"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"

	"sigs.k8s.io/yaml"

	"github.com/muskats/heraion/internal/secret"
)

// Environment variables read by Load.
//
//nolint:gosec // G101: env var names, not credentials
const (
	EnvLLMAPIKey  = "HERAION_LLM_API_KEY"
	EnvLLMBaseURL = "HERAION_LLM_BASE_URL"
	EnvLLMModel   = "HERAION_LLM_MODEL"
	EnvGitToken   = "HERAION_GIT_TOKEN"
	EnvDryRun     = "HERAION_DRY_RUN"
)

// Config is the top-level runtime configuration.
type Config struct {
	LLM   LLM      `json:"llm"`
	Git   Git      `json:"git"`
	Repos []string `json:"repos,omitempty"`
	// Namespaces limits the Kubernetes tools to these namespaces, e.g.
	// [shop, payments]. Empty: all namespaces. See mcp.LimitNamespaces.
	Namespaces   []string `json:"namespaces,omitempty"`
	PlaybooksDir string   `json:"playbooksDir,omitempty"`
	WorkDir      string   `json:"workDir,omitempty"`
	DryRun       bool     `json:"dryRun,omitempty"`
	// Mode is the default and the riskiest mode a playbook may use, e.g.
	// "full" lets a playbook pick safe or full but not yolo. Default full.
	Mode Mode `json:"mode,omitempty"`
	// MCPServers maps a name to a server. Server "k8s" exposes tools such
	// as "k8s.resources_list".
	MCPServers map[string]*MCPServer `json:"mcpServers,omitempty"`
	// Jira enables create_issue through an mcp-atlassian server. Nil: none.
	Jira    *Jira   `json:"jira,omitempty"`
	Gateway Gateway `json:"gateway,omitempty"`
}

// MCPServer is one MCP server reached over HTTP, e.g.
//
//	k8s: {url: http://heraion-mcp-k8s:8080/mcp, allowTools: [resources_*]}
type MCPServer struct {
	URL string `json:"url"`
	// Transport is "streamable" (default) or "sse".
	Transport string `json:"transport,omitempty"`
	// Headers are plain headers, e.g. {"X-Org": "acme"}.
	Headers map[string]string `json:"headers,omitempty"`
	// HeadersFromEnv reads secret header values from env vars, e.g.
	// {"DD-API-KEY": "HERAION_MCP_DATADOG_API_KEY"}. Each also accepts
	// <ENV>_FILE and <ENV>_FALLBACK, see package secret.
	HeadersFromEnv map[string]string `json:"headersFromEnv,omitempty"`
	// HeaderPrefixes are put before a secret header value, e.g.
	// {"Authorization": "Bearer "} when the value is a raw token file.
	HeaderPrefixes map[string]string `json:"headerPrefixes,omitempty"`
	// AllowTools and DenyTools filter tools by name, e.g. ["pods_*"].
	// Deny wins. An empty allow list allows every tool.
	AllowTools []string `json:"allowTools,omitempty"`
	DenyTools  []string `json:"denyTools,omitempty"`
	// Timeout per tool call. Default 60s.
	Timeout Duration `json:"timeout,omitempty"`
	// Required makes a run fail if the server is down. Otherwise the server
	// is skipped with a warning.
	Required bool `json:"required,omitempty"`

	SecretHeaders map[string]*secret.Source `json:"-"` // from HeadersFromEnv
}

// Jira is where create_issue writes, through the jira_search and
// jira_create_issue tools of an mcp-atlassian server, e.g.
//
//	jira: {server: atlassian, projects: [OPS], issueType: Task, labels: [heraion]}
type Jira struct {
	// Server is the mcpServers entry running mcp-atlassian, e.g. "atlassian".
	Server string `json:"server"`
	// Projects issues may be created in, e.g. [OPS, SEC].
	Projects []string `json:"projects"`
	// IssueType is used when the model does not pick one. Default "Task".
	IssueType string `json:"issueType,omitempty"`
	// Labels are added to every issue, e.g. [heraion].
	Labels []string `json:"labels,omitempty"`
}

// Gateway configures `heraion serve`.
type Gateway struct {
	Listen string `json:"listen,omitempty"` // default :8080
	// MetricsListen serves Prometheus metrics at /metrics, e.g. ":9090".
	// Empty: no metrics. A separate port keeps metrics off the webhook port.
	MetricsListen string `json:"metricsListen,omitempty"`
	// Namespace for worker Jobs. Default: $POD_NAMESPACE, then the service
	// account's namespace.
	Namespace string `json:"namespace,omitempty"`
	// JobTemplate is a Job manifest file, e.g. /etc/heraion/job/job-template.yaml.
	JobTemplate string `json:"jobTemplate,omitempty"`
	// MaxConcurrentJobs caps unfinished worker Jobs (0 = no limit).
	MaxConcurrentJobs int `json:"maxConcurrentJobs,omitempty"`
	// MaxBodyBytes caps the webhook body (default 256 KiB). The body is
	// stored in a Secret, which holds at most 1 MiB.
	MaxBodyBytes int64 `json:"maxBodyBytes,omitempty"`
	// Hooks maps a name to a webhook served at POST /hooks/<name>.
	Hooks map[string]*Hook `json:"hooks,omitempty"`
}

// Hook is one webhook endpoint.
type Hook struct {
	// SecretEnv is the env var with the secret, e.g. HERAION_HOOK_ALERTMANAGER_SECRET.
	SecretEnv string `json:"secretEnv,omitempty"`
	// Auth is "hmac-sha256" (default) or "token":
	//
	//	hmac-sha256  X-Hub-Signature-256: sha256=<hex HMAC of the body>  (GitHub, Gitea)
	//	token        Authorization: Bearer <secret>                      (Alertmanager, GitLab)
	Auth string `json:"auth,omitempty"`
	// Header overrides the header above, e.g. "X-Gitlab-Token".
	Header string `json:"header,omitempty"`
	// Insecure skips authentication. Local testing only.
	Insecure bool `json:"insecure,omitempty"`

	Secret *secret.Source `json:"-"` // from SecretEnv
}

// LLM configures the OpenAI-compatible chat-completions endpoint.
type LLM struct {
	BaseURL     string         `json:"baseURL"`
	Model       string         `json:"model"`
	APIKey      *secret.Source `json:"-"` // HERAION_LLM_API_KEY[_FILE|_FALLBACK]
	MaxSteps    int            `json:"maxSteps,omitempty"`
	MaxTokens   int            `json:"maxTokens,omitempty"` // total token budget per run
	Timeout     Duration       `json:"timeout,omitempty"`
	Temperature *float64       `json:"temperature,omitempty"`
}

// Git configures the Git hosting provider.
type Git struct {
	// Provider is a go-scm driver: github, gitlab, gitea, bitbucket, stash,
	// gogs or azure.
	Provider string `json:"provider"`
	// Endpoint is the API URL, e.g. https://git.example.com. Empty means SaaS.
	Endpoint string `json:"endpoint,omitempty"`
	// CloneBaseURL builds clone URLs as <base>/<owner>/<repo>.git. Defaults
	// to Endpoint, or https://github.com, https://gitlab.com or
	// https://bitbucket.org when Endpoint is empty. For stash,
	// bitbucketserver and azure, unset means: ask the API (see
	// ResolvesCloneURL).
	CloneBaseURL string `json:"cloneBaseURL,omitempty"`
	// Username for clone and push; the token is the password.
	Username    string         `json:"username,omitempty"`
	AuthorName  string         `json:"authorName,omitempty"`
	AuthorEmail string         `json:"authorEmail,omitempty"`
	Token       *secret.Source `json:"-"` // HERAION_GIT_TOKEN[_FILE|_FALLBACK]
}

// ResolvesCloneURL reports whether clone URLs come from the Git server's API
// (cloneBaseURL unset, and a provider whose URLs are not
// <base>/<owner>/<repo>.git: Bitbucket Server uses /scm/<project>/<repo>.git,
// Azure DevOps <org>/<project>/_git/<repo>).
func (g Git) ResolvesCloneURL() bool {
	if g.CloneBaseURL != "" {
		return false
	}
	switch g.Provider {
	case "stash", "bitbucketserver", "azure":
		return true
	}
	return false
}

// Duration is a duration written as a string, e.g. "15m".
type Duration = metav1.Duration

// Load reads path (which may be empty), applies env overrides and defaults,
// and validates the result.
func Load(path string) (*Config, error) {
	cfg := &Config{}
	if path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read config: %w", err)
		}
		if err := yaml.UnmarshalStrict(b, cfg); err != nil {
			return nil, fmt.Errorf("parse config %s: %w", path, err)
		}
	}
	cfg.applyEnv(os.Getenv)
	cfg.applyDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func (c *Config) applyEnv(getenv func(string) string) {
	c.LLM.APIKey = secret.New(EnvLLMAPIKey, getenv)
	c.Git.Token = secret.New(EnvGitToken, getenv)
	if v := getenv(EnvLLMBaseURL); v != "" {
		c.LLM.BaseURL = v
	}
	if v := getenv(EnvLLMModel); v != "" {
		c.LLM.Model = v
	}
	for _, m := range c.MCPServers {
		if m == nil {
			continue
		}
		m.SecretHeaders = map[string]*secret.Source{}
		for h, env := range m.HeadersFromEnv {
			m.SecretHeaders[h] = secret.New(env, getenv)
		}
	}
	for _, h := range c.Gateway.Hooks {
		if h != nil && h.SecretEnv != "" {
			h.Secret = secret.New(h.SecretEnv, getenv)
		}
	}
	switch strings.ToLower(getenv(EnvDryRun)) {
	case "1", "true", "yes":
		c.DryRun = true
	}
}

func (c *Config) applyDefaults() {
	if c.LLM.MaxSteps == 0 {
		c.LLM.MaxSteps = 30
	}
	if c.LLM.MaxTokens == 0 {
		c.LLM.MaxTokens = 500_000
	}
	if c.LLM.Timeout.Duration == 0 {
		c.LLM.Timeout.Duration = 20 * time.Minute
	}
	c.LLM.BaseURL = strings.TrimRight(c.LLM.BaseURL, "/")
	if c.Git.Provider == "" {
		c.Git.Provider = "github"
	}
	if c.Git.CloneBaseURL == "" && !c.Git.ResolvesCloneURL() {
		if c.Git.Endpoint == "" && c.Git.Provider == "github" {
			c.Git.CloneBaseURL = "https://github.com"
		} else if c.Git.Endpoint == "" && c.Git.Provider == "gitlab" {
			c.Git.CloneBaseURL = "https://gitlab.com"
		} else if c.Git.Endpoint == "" && (c.Git.Provider == "bitbucket" || c.Git.Provider == "bitbucketcloud") {
			c.Git.CloneBaseURL = "https://bitbucket.org"
		} else {
			c.Git.CloneBaseURL = c.Git.Endpoint
		}
	}
	c.Git.CloneBaseURL = strings.TrimRight(c.Git.CloneBaseURL, "/")
	if c.Git.Username == "" {
		c.Git.Username = "x-access-token"
	}
	if c.Git.AuthorName == "" {
		c.Git.AuthorName = "HeraIon"
	}
	if c.Git.AuthorEmail == "" {
		c.Git.AuthorEmail = "heraion@users.noreply.local"
	}
	if c.Mode == "" {
		c.Mode = ModeFull
	}
	if c.PlaybooksDir == "" {
		c.PlaybooksDir = "/etc/heraion/playbooks"
	}
	if c.WorkDir == "" {
		c.WorkDir = os.TempDir()
	}
	for _, m := range c.MCPServers {
		if m == nil {
			continue
		}
		if m.Transport == "" {
			m.Transport = "streamable"
		}
		if m.Timeout.Duration == 0 {
			m.Timeout.Duration = 60 * time.Second
		}
	}
	if j := c.Jira; j != nil && j.IssueType == "" {
		j.IssueType = "Task"
	}
	if c.Gateway.Listen == "" {
		c.Gateway.Listen = ":8080"
	}
	if c.Gateway.MaxBodyBytes == 0 {
		c.Gateway.MaxBodyBytes = 256 << 10
	}
	for _, h := range c.Gateway.Hooks {
		if h == nil {
			continue
		}
		if h.Auth == "" {
			h.Auth = HookAuthHMAC
		}
		if h.Header == "" {
			if h.Auth == HookAuthToken {
				h.Header = "Authorization"
			} else {
				h.Header = "X-Hub-Signature-256"
			}
		}
	}
}

// MaxBodyBytesLimit is the largest gateway.maxBodyBytes: a Secret holds at
// most 1 MiB, minus room for its metadata.
const MaxBodyBytesLimit = 1<<20 - 16<<10

// Hook auth modes.
const (
	HookAuthHMAC  = "hmac-sha256"
	HookAuthToken = "token"
)

// Jira project keys, e.g. OPS, SEC2, MY_PROJ.
var projectKeyRE = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,254}$`)

var knownProviders = map[string]bool{
	"github": true, "gitlab": true, "gitea": true, "bitbucket": true,
	"bitbucketcloud": true, "stash": true, "bitbucketserver": true,
	"gogs": true, "azure": true,
}

// Validate returns every problem at once.
func (c *Config) Validate() error {
	var errs []error
	errs = append(errs, c.LLM.validate()...)
	errs = append(errs, c.Git.validate()...)
	for _, r := range c.Repos {
		if err := ValidateRepoName(r); err != nil {
			errs = append(errs, fmt.Errorf("repos: %w", err))
		}
	}
	for _, ns := range c.Namespaces {
		if !DNSLabel(ns, 63) {
			errs = append(errs, fmt.Errorf("namespaces: %q is not a Kubernetes namespace name", ns))
		}
	}
	if err := ValidateMode("mode", c.Mode); err != nil {
		errs = append(errs, err)
	}
	if c.Jira != nil {
		errs = append(errs, c.Jira.validate(c.MCPServers)...)
	}
	errs = append(errs, validateMCP(c.MCPServers)...)
	errs = append(errs, c.Gateway.validate()...)
	return errors.Join(errs...)
}

func (l *LLM) validate() []error {
	var errs []error
	if l.BaseURL == "" {
		errs = append(errs, errors.New("llm.baseURL is required"))
	} else if err := checkURL(l.BaseURL); err != nil {
		errs = append(errs, fmt.Errorf("llm.baseURL: %w", err))
	}
	if l.Model == "" {
		errs = append(errs, errors.New("llm.model is required"))
	}
	if l.MaxSteps < 1 {
		errs = append(errs, errors.New("llm.maxSteps must be >= 1"))
	}
	if l.MaxTokens < 1 {
		errs = append(errs, errors.New("llm.maxTokens must be >= 1"))
	}
	if l.Timeout.Duration <= 0 {
		errs = append(errs, errors.New("llm.timeout must be positive"))
	}
	return errs
}

func (g *Git) validate() []error {
	var errs []error
	if !knownProviders[g.Provider] {
		errs = append(errs, fmt.Errorf("git.provider %q is not supported", g.Provider))
	}
	if g.Endpoint != "" {
		if err := checkURL(g.Endpoint); err != nil {
			errs = append(errs, fmt.Errorf("git.endpoint: %w", err))
		}
	}
	// An empty cloneBaseURL is fine when the API provides clone URLs.
	switch {
	case g.CloneBaseURL != "":
		if err := checkURLSchemes(g.CloneBaseURL, true); err != nil {
			errs = append(errs, fmt.Errorf("git.cloneBaseURL: %w", err))
		}
	case !g.ResolvesCloneURL():
		errs = append(errs, errors.New("git.cloneBaseURL (or git.endpoint) is required for this provider"))
	}
	return errs
}

func (j *Jira) validate(servers map[string]*MCPServer) []error {
	var errs []error
	if servers[j.Server] == nil {
		errs = append(errs, fmt.Errorf("jira.server %q must name an mcpServers entry running mcp-atlassian", j.Server))
	}
	if len(j.Projects) == 0 {
		errs = append(errs, errors.New("jira.projects must list at least one project key, e.g. [OPS]"))
	}
	for _, p := range j.Projects {
		if !projectKeyRE.MatchString(p) {
			errs = append(errs, fmt.Errorf("jira.projects: %q is not a project key", p))
		}
	}
	for _, l := range j.Labels {
		if !ValidLabel(l) {
			errs = append(errs, fmt.Errorf("jira.labels: %q must not contain spaces", l))
		}
	}
	return errs
}

// validateMCP checks each server. Empty secret env vars are checked by the
// pod that uses them: the gateway has no MCP secrets.
func validateMCP(servers map[string]*MCPServer) []error {
	var errs []error
	for name, m := range servers {
		if !DNSLabel(name, 32) {
			errs = append(errs, fmt.Errorf("mcpServers: name %q must be a lowercase DNS label (max 32 chars)", name))
		}
		if m == nil {
			errs = append(errs, fmt.Errorf("mcpServers.%s: empty", name))
			continue
		}
		if err := checkURL(m.URL); err != nil || m.URL == "" {
			errs = append(errs, fmt.Errorf("mcpServers.%s.url: invalid %q", name, m.URL))
		}
		if m.Transport != "streamable" && m.Transport != "sse" {
			errs = append(errs, fmt.Errorf("mcpServers.%s.transport %q must be streamable or sse", name, m.Transport))
		}
		for _, p := range append(slices.Clone(m.AllowTools), m.DenyTools...) {
			if _, err := path.Match(p, ""); err != nil {
				errs = append(errs, fmt.Errorf("mcpServers.%s: tool pattern %q: %w", name, p, err))
			}
		}
	}
	return errs
}

// validate checks the hooks. Empty secret env vars are checked when the
// gateway starts: workers have no hook secrets.
func (gw *Gateway) validate() []error {
	var errs []error
	if gw.MetricsListen != "" && gw.MetricsListen == gw.Listen {
		errs = append(errs, fmt.Errorf("gateway.metricsListen %q must differ from gateway.listen", gw.MetricsListen))
	}
	// The body is stored in a Secret, whose data is capped at 1 MiB.
	if gw.MaxConcurrentJobs < 0 {
		errs = append(errs, errors.New("gateway.maxConcurrentJobs must be >= 0 (0: no limit)"))
	}
	if gw.MaxBodyBytes < 0 || gw.MaxBodyBytes > MaxBodyBytesLimit {
		errs = append(errs, fmt.Errorf("gateway.maxBodyBytes must be between 1 and %d", MaxBodyBytesLimit))
	}
	for name, h := range gw.Hooks {
		if !DNSLabel(name, 32) {
			errs = append(errs, fmt.Errorf("gateway.hooks: name %q must be a lowercase DNS label", name))
		}
		if h == nil {
			errs = append(errs, fmt.Errorf("gateway.hooks.%s: empty", name))
			continue
		}
		if h.Auth != HookAuthHMAC && h.Auth != HookAuthToken {
			errs = append(errs, fmt.Errorf("gateway.hooks.%s.auth %q must be %s or %s", name, h.Auth, HookAuthHMAC, HookAuthToken))
		}
		if !h.Insecure && h.SecretEnv == "" {
			errs = append(errs, fmt.Errorf("gateway.hooks.%s: secretEnv is required (or set insecure: true)", name))
		}
	}
	return errs
}

// ValidLabel reports whether l is a usable Jira label: not empty, no
// whitespace, e.g. "heraion" ok, "needs triage" not.
func ValidLabel(l string) bool { return l != "" && !strings.ContainsAny(l, " \t\n") }

// ValidateRepoName accepts "owner/name" and nested groups like "group/sub/name".
func ValidateRepoName(r string) error {
	parts := strings.Split(r, "/")
	if len(parts) < 2 {
		return fmt.Errorf("repo %q must be owner/name", r)
	}
	for _, p := range parts {
		if p == "" || p == "." || p == ".." || strings.ContainsAny(p, " \\:?#") {
			return fmt.Errorf("repo %q is not a valid repository name", r)
		}
	}
	return nil
}

// checkURL accepts http and https URLs.
func checkURL(s string) error { return checkURLSchemes(s, false) }

// checkURLSchemes also accepts file:// URLs when allowFile is set (local
// bare repositories as git.cloneBaseURL, e.g. in tests).
func checkURLSchemes(s string, allowFile bool) error {
	u, err := url.Parse(s)
	if err != nil {
		return err
	}
	if u.Scheme != "http" && u.Scheme != "https" && !(allowFile && u.Scheme == "file") {
		return fmt.Errorf("unsupported scheme %q", u.Scheme)
	}
	if u.Scheme != "file" && u.Host == "" {
		return errors.New("missing host")
	}
	return nil
}

// Secrets returns the credentials to mask in prompts and logs.
func (c *Config) Secrets() []*secret.Source {
	out := []*secret.Source{c.LLM.APIKey, c.Git.Token}
	for _, m := range c.MCPServers {
		if m == nil {
			continue
		}
		for _, v := range m.SecretHeaders {
			out = append(out, v)
		}
	}
	for _, h := range c.Gateway.Hooks {
		if h != nil && h.Secret != nil {
			out = append(out, h.Secret)
		}
	}
	return out
}

// DNSLabel reports whether s is a DNS-1123 label of at most max characters,
// e.g. DNSLabel("trivy-remediate", 52) is true, DNSLabel("Shop_1", 63) false.
func DNSLabel(s string, max int) bool {
	return len(s) <= max && len(validation.IsDNS1123Label(s)) == 0
}
