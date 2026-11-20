package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/muskats/heraion/internal/config"
	"github.com/muskats/heraion/internal/jira"
	"github.com/muskats/heraion/internal/llm"
	"github.com/muskats/heraion/internal/mcp"
	"github.com/muskats/heraion/internal/playbook"
	"github.com/muskats/heraion/internal/secret"
)

// cmdCheck validates the setup without running a playbook. Used by `helm test`:
//
//	$ heraion check --connect
//	ok    config     /etc/heraion/conf/config.yaml
//	ok    playbooks  alertmanager-triage (full), trivy-remediate (safe)
//	ok    secret     HERAION_LLM_API_KEY (file)
//	warn  secret     HERAION_GIT_TOKEN not set
//	ok    mcp        k8s: 18 tools
//	ok    llm        GET https://api.openai.com/v1/models: 200
//	ok    jira       create_issue through atlassian, in [OPS]
func cmdCheck(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("check", flag.ContinueOnError)
	cfgPath := configFlag(fs)
	role := fs.String("role", "worker", "worker or gateway: which secrets must be present")
	connect := fs.Bool("connect", false, "also connect to the MCP servers and the LLM endpoint")
	if err := fs.Parse(args); err != nil {
		return err
	}
	c := &checker{out: stdout}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		c.fail("config", err.Error())
		return c.result()
	}
	c.ok("config", *cfgPath)

	pbs, err := playbook.LoadDir(cfg.PlaybooksDir)
	if err != nil {
		c.fail("playbooks", err.Error())
	} else {
		names := make([]string, 0, len(pbs))
		for n, p := range pbs {
			if p.Output == playbook.OutputPR {
				n += " (" + string(p.EffectiveMode(cfg.Mode)) + ")"
			}
			names = append(names, n)
		}
		sort.Strings(names)
		c.ok("playbooks", strings.Join(names, ", "))
	}

	switch *role {
	case "gateway":
		for name, h := range cfg.Gateway.Hooks {
			if !h.Insecure {
				c.secret(h.Secret, true, "hook "+name)
			}
		}
	default:
		c.secret(cfg.LLM.APIKey, false, "llm")
		c.secret(cfg.Git.Token, false, "git")
		for name, m := range cfg.MCPServers {
			for h, src := range m.SecretHeaders {
				c.secret(src, true, "mcp "+name+" "+h)
			}
		}
	}

	if *connect && *role != "gateway" {
		ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
		defer cancel()
		for name, srv := range cfg.MCPServers {
			sess, tools, err := mcp.Dial(ctx, name, srv, version)
			switch {
			case err != nil && srv.Required:
				c.fail("mcp", err.Error())
			case err != nil:
				c.warn("mcp", err.Error()+" (optional: runs continue without it)")
			default:
				c.ok("mcp", fmt.Sprintf("%s: %d tools", name, len(tools)))
				if cfg.Jira != nil && name == cfg.Jira.Server {
					c.jiraTools(name, tools, cfg.Jira.Projects)
				}
				_ = sess.Close()
			}
		}
		c.llm(ctx, cfg)
	}
	return c.result()
}

type checker struct {
	out    io.Writer
	failed bool
}

func (c *checker) line(status, what, msg string) {
	fmt.Fprintf(c.out, "%-5s %-10s %s\n", status, what, msg)
}
func (c *checker) ok(what, msg string)   { c.line("ok", what, msg) }
func (c *checker) warn(what, msg string) { c.line("warn", what, msg) }
func (c *checker) fail(what, msg string) { c.line("FAIL", what, msg); c.failed = true }

func (c *checker) result() error {
	if c.failed {
		return errors.New("check failed")
	}
	return nil
}

// secret reports where a credential comes from, never its value.
func (c *checker) secret(src *secret.Source, required bool, label string) {
	if src == nil {
		return
	}
	_, err := src.Value()
	switch {
	case err == nil:
		c.ok("secret", fmt.Sprintf("%s (%s) for %s", src.Name, src.Origin(), label))
	case src.Set() || required:
		c.fail("secret", fmt.Sprintf("%s for %s: %v", src.Name, label, err))
	default:
		c.warn("secret", fmt.Sprintf("%s not set (%s)", src.Name, label))
	}
}

// llm calls GET /models, which most OpenAI-compatible servers implement.
// 401/403 and network errors fail; other statuses only warn.
func (c *checker) llm(ctx context.Context, cfg *config.Config) {
	url := cfg.LLM.BaseURL + "/models"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		c.fail("llm", err.Error())
		return
	}
	if cfg.LLM.APIKey.Set() {
		if key, err := cfg.LLM.APIKey.Value(); err == nil {
			req.Header.Set("Authorization", "Bearer "+key)
		}
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.fail("llm", err.Error())
		return
	}
	resp.Body.Close()
	msg := fmt.Sprintf("GET %s: %d", url, resp.StatusCode)
	switch {
	case resp.StatusCode/100 == 2:
		c.ok("llm", msg)
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		c.fail("llm", msg+" (check the API key)")
	default:
		c.warn("llm", msg+" (endpoint may not implement /models)")
	}
}

// jiraTools fails unless the server offers the tools and arguments
// create_issue calls, e.g. after an mcp-atlassian upgrade renamed one.
func (c *checker) jiraTools(server string, tools []llm.Tool, projects []string) {
	var missing []string
	for tool, args := range jira.RequiredArgs {
		var schema struct{ Properties map[string]json.RawMessage }
		found := false
		for _, t := range tools {
			if t.Name() == server+"."+tool {
				found = true
				_ = json.Unmarshal(t.Schema(), &schema)
			}
		}
		if !found {
			missing = append(missing, tool)
			continue
		}
		for _, a := range args {
			if _, ok := schema.Properties[a]; !ok {
				missing = append(missing, tool+"."+a)
			}
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		c.fail("jira", fmt.Sprintf("%s lacks %v; create_issue needs mcp-atlassian 0.24 or later with these tools enabled", server, missing))
		return
	}
	c.ok("jira", fmt.Sprintf("create_issue through %s, in %v", server, projects))
}
