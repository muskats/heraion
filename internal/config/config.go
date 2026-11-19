// Package config loads the runtime configuration. Settings come from a YAML
// file, secrets only from env vars:
//
//	llm: {baseURL: https://api.openai.com/v1, model: gpt-4.1}
//	HERAION_LLM_API_KEY=sk-...
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"sigs.k8s.io/yaml"
)

// Environment variables read by Load.
const (
	EnvLLMAPIKey  = "HERAION_LLM_API_KEY"
	EnvLLMBaseURL = "HERAION_LLM_BASE_URL"
	EnvLLMModel   = "HERAION_LLM_MODEL"
	EnvGitToken   = "HERAION_GIT_TOKEN"
	EnvDryRun     = "HERAION_DRY_RUN"
)

// Config is the top-level runtime configuration.
type Config struct {
	LLM          LLM      `json:"llm"`
	Git          Git      `json:"git"`
	Repos        []string `json:"repos,omitempty"`
	PlaybooksDir string   `json:"playbooksDir,omitempty"`
	WorkDir      string   `json:"workDir,omitempty"`
	DryRun       bool     `json:"dryRun,omitempty"`
	// MCPServers maps a name to a server. Server "k8s" exposes tools such
	// as "k8s.resources_list".
	MCPServers map[string]*MCPServer `json:"mcpServers,omitempty"`
	Gateway    Gateway               `json:"gateway,omitempty"`
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
	// {"DD-API-KEY": "HERAION_MCP_DATADOG_API_KEY"}.
	HeadersFromEnv map[string]string `json:"headersFromEnv,omitempty"`
	// AllowTools and DenyTools filter tools by name, e.g. ["pods_*"].
	// Deny wins. An empty allow list allows every tool.
	AllowTools []string `json:"allowTools,omitempty"`
	DenyTools  []string `json:"denyTools,omitempty"`
	// Timeout per tool call. Default 60s.
	Timeout Duration `json:"timeout,omitempty"`
	// Required makes a run fail if the server is down. Otherwise the server
	// is skipped with a warning.
	Required bool `json:"required,omitempty"`

	SecretHeaders map[string]string `json:"-"` // resolved from HeadersFromEnv
}

// Gateway configures `heraion serve`.
type Gateway struct {
	Listen string `json:"listen,omitempty"` // default :8080
	// Namespace for worker Jobs. Default: $POD_NAMESPACE, then the service
	// account's namespace.
	Namespace string `json:"namespace,omitempty"`
	// JobTemplate is a Job manifest file, e.g. /etc/heraion/job/job-template.yaml.
	JobTemplate string `json:"jobTemplate,omitempty"`
	// MaxConcurrentJobs caps unfinished worker Jobs (0 = no limit).
	MaxConcurrentJobs int `json:"maxConcurrentJobs,omitempty"`
	// MaxBodyBytes caps the webhook body (default 256 KiB). The body is
	// passed to the Job in an env var, so it has to stay small.
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

	Secret string `json:"-"` // resolved from SecretEnv
}

// LLM configures the OpenAI-compatible chat-completions endpoint.
type LLM struct {
	BaseURL     string   `json:"baseURL"`
	Model       string   `json:"model"`
	APIKey      string   `json:"-"` // from env only
	MaxSteps    int      `json:"maxSteps,omitempty"`
	MaxTokens   int      `json:"maxTokens,omitempty"` // total token budget per run
	Timeout     Duration `json:"timeout,omitempty"`
	Temperature *float64 `json:"temperature,omitempty"`
}

// Git configures the Git hosting provider.
type Git struct {
	// Provider is a go-scm driver: github, gitlab, gitea, bitbucket, stash,
	// gogs or azure.
	Provider string `json:"provider"`
	// Endpoint is the API URL, e.g. https://git.example.com. Empty means SaaS.
	Endpoint string `json:"endpoint,omitempty"`
	// CloneBaseURL builds clone URLs as <base>/<owner>/<repo>.git. Defaults
	// to Endpoint, or https://github.com for GitHub.
	CloneBaseURL string `json:"cloneBaseURL,omitempty"`
	// Username for clone and push; the token is the password.
	Username    string `json:"username,omitempty"`
	AuthorName  string `json:"authorName,omitempty"`
	AuthorEmail string `json:"authorEmail,omitempty"`
	Token       string `json:"-"` // from env only
}

// Duration is a time.Duration that unmarshals from strings like "15m".
type Duration struct{ time.Duration }

func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("duration must be a string like \"15m\": %w", err)
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	d.Duration = v
	return nil
}

func (d Duration) MarshalJSON() ([]byte, error) { return json.Marshal(d.String()) }

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
	c.LLM.APIKey = getenv(EnvLLMAPIKey)
	c.Git.Token = getenv(EnvGitToken)
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
		m.SecretHeaders = map[string]string{}
		for h, env := range m.HeadersFromEnv {
			m.SecretHeaders[h] = getenv(env)
		}
	}
	for _, h := range c.Gateway.Hooks {
		if h != nil && h.SecretEnv != "" {
			h.Secret = getenv(h.SecretEnv)
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
	if c.Git.CloneBaseURL == "" {
		if c.Git.Endpoint == "" && c.Git.Provider == "github" {
			c.Git.CloneBaseURL = "https://github.com"
		} else if c.Git.Endpoint == "" && c.Git.Provider == "gitlab" {
			c.Git.CloneBaseURL = "https://gitlab.com"
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

// Hook auth modes.
const (
	HookAuthHMAC  = "hmac-sha256"
	HookAuthToken = "token"
)

var serverNameRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,30}[a-z0-9])?$`)

var knownProviders = map[string]bool{
	"github": true, "gitlab": true, "gitea": true, "bitbucket": true,
	"bitbucketcloud": true, "stash": true, "bitbucketserver": true,
	"gogs": true, "azure": true, "fake": true,
}

// Validate returns every problem at once.
func (c *Config) Validate() error {
	var errs []error
	if c.LLM.BaseURL == "" {
		errs = append(errs, errors.New("llm.baseURL is required"))
	} else if err := checkURL(c.LLM.BaseURL); err != nil {
		errs = append(errs, fmt.Errorf("llm.baseURL: %w", err))
	}
	if c.LLM.Model == "" {
		errs = append(errs, errors.New("llm.model is required"))
	}
	if c.LLM.MaxSteps < 1 {
		errs = append(errs, errors.New("llm.maxSteps must be >= 1"))
	}
	if !knownProviders[c.Git.Provider] {
		errs = append(errs, fmt.Errorf("git.provider %q is not supported", c.Git.Provider))
	}
	if c.Git.Endpoint != "" {
		if err := checkURL(c.Git.Endpoint); err != nil {
			errs = append(errs, fmt.Errorf("git.endpoint: %w", err))
		}
	}
	if c.Git.CloneBaseURL == "" {
		errs = append(errs, errors.New("git.cloneBaseURL (or git.endpoint) is required for this provider"))
	} else if err := checkURL(c.Git.CloneBaseURL); err != nil {
		errs = append(errs, fmt.Errorf("git.cloneBaseURL: %w", err))
	}
	for _, r := range c.Repos {
		if err := ValidateRepoName(r); err != nil {
			errs = append(errs, fmt.Errorf("repos: %w", err))
		}
	}
	for name, m := range c.MCPServers {
		if !serverNameRE.MatchString(name) {
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
		// Empty secret env vars are checked by the pod that uses them: the
		// gateway has no MCP secrets and workers have no hook secrets.
	}
	for name, h := range c.Gateway.Hooks {
		if !serverNameRE.MatchString(name) {
			errs = append(errs, fmt.Errorf("gateway.hooks: name %q must be a lowercase DNS label", name))
		}
		if h == nil {
			continue
		}
		if h.Auth != HookAuthHMAC && h.Auth != HookAuthToken {
			errs = append(errs, fmt.Errorf("gateway.hooks.%s.auth %q must be %s or %s", name, h.Auth, HookAuthHMAC, HookAuthToken))
		}
		if !h.Insecure && h.SecretEnv == "" {
			errs = append(errs, fmt.Errorf("gateway.hooks.%s: secretEnv is required (or set insecure: true)", name))
		}
	}
	return errors.Join(errs...)
}

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

func checkURL(s string) error {
	u, err := url.Parse(s)
	if err != nil {
		return err
	}
	if u.Scheme != "http" && u.Scheme != "https" && u.Scheme != "file" {
		return fmt.Errorf("unsupported scheme %q", u.Scheme)
	}
	if u.Scheme != "file" && u.Host == "" {
		return errors.New("missing host")
	}
	return nil
}

// Secrets returns the values to mask in prompts and logs.
func (c *Config) Secrets() []string {
	out := []string{c.LLM.APIKey, c.Git.Token}
	for _, m := range c.MCPServers {
		if m == nil {
			continue
		}
		for _, v := range m.SecretHeaders {
			out = append(out, v, strings.TrimPrefix(v, "Bearer "))
		}
	}
	for _, h := range c.Gateway.Hooks {
		if h != nil {
			out = append(out, h.Secret)
		}
	}
	return out
}
