// Package mcp is the MCP client. Each server's tools are exposed as
// "<server>.<tool>", e.g. "grafana.query_prometheus".
package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strings"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/muskats/heraion/internal/config"
	"github.com/muskats/heraion/internal/llm"
	"github.com/muskats/heraion/internal/secret"
)

// Manager holds open MCP sessions for one run.
type Manager struct {
	sessions map[string]*sdk.ClientSession
}

// Close closes every session.
func (m *Manager) Close() {
	for _, s := range m.sessions {
		_ = s.Close()
	}
}

// NeededBy reports whether patterns can match a tool of server, e.g.
// [k8s.*, repo_*] needs "k8s" but not "grafana".
func NeededBy(patterns []string, server string) bool {
	for _, p := range patterns {
		if prefix, _, dotted := strings.Cut(p, "."); dotted {
			if ok, _ := path.Match(prefix, server); ok {
				return true
			}
		} else if ok, _ := path.Match(p, server+".x"); ok {
			return true
		}
	}
	return false
}

// Connect opens a session to each server the tool patterns can use, e.g.
// [k8s.*] connects only "k8s" (see NeededBy). A server that fails is skipped
// with a warning, unless it is Required.
func Connect(ctx context.Context, servers map[string]*config.MCPServer, patterns []string, version string, log *slog.Logger) (*Manager, []llm.Tool, error) {
	m := &Manager{sessions: map[string]*sdk.ClientSession{}}
	names := make([]string, 0, len(servers))
	for n := range servers {
		names = append(names, n)
	}
	sort.Strings(names)

	var tools []llm.Tool
	for _, name := range names {
		srv := servers[name]
		if !NeededBy(patterns, name) {
			continue
		}
		sess, ts, err := dial(ctx, name, srv, version)
		if err != nil {
			if srv.Required {
				m.Close()
				return nil, nil, fmt.Errorf("mcp server %s: %w", name, err)
			}
			log.Warn("mcp server unavailable, continuing without it", "server", name, "err", err)
			continue
		}
		log.Info("mcp server connected", "server", name, "tools", len(ts))
		m.sessions[name] = sess
		tools = append(tools, ts...)
	}
	return m, tools, nil
}

// Dial connects to one server and returns its allowed tools, e.g. for
// `heraion check`. Close the returned session when done.
func Dial(ctx context.Context, name string, srv *config.MCPServer, version string) (io.Closer, []llm.Tool, error) {
	sess, tools, err := dial(ctx, name, srv, version)
	if err != nil {
		return nil, nil, err
	}
	return sess, tools, nil
}

func dial(ctx context.Context, name string, srv *config.MCPServer, version string) (*sdk.ClientSession, []llm.Tool, error) {
	for h, src := range srv.SecretHeaders {
		if _, err := src.Value(); err != nil {
			return nil, nil, fmt.Errorf("header %s: %w", h, err)
		}
	}
	// Time out in the transport, not with a context: SSE ties its stream to
	// the Connect context, so cancelling it would close the session.
	base := http.DefaultTransport.(*http.Transport).Clone()
	base.ResponseHeaderTimeout = 30 * time.Second
	u, err := url.Parse(srv.URL)
	if err != nil {
		return nil, nil, fmt.Errorf("url: %w", err)
	}
	hc := &http.Client{
		Transport: &headerTransport{
			host: u.Host, base: base, static: srv.Headers, secrets: srv.SecretHeaders, prefixes: srv.HeaderPrefixes,
		},
		// The transport adds headers per request, so a redirect elsewhere
		// would carry the API keys along. Stay on the configured host.
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if !strings.EqualFold(req.URL.Host, u.Host) {
				return fmt.Errorf("refusing redirect from %s to another host %s", u.Host, req.URL.Host)
			}
			if len(via) >= 10 {
				return errors.New("stopped after 10 redirects")
			}
			return nil
		},
	}
	var t sdk.Transport
	switch srv.Transport {
	case "sse":
		t = &sdk.SSEClientTransport{Endpoint: srv.URL, HTTPClient: hc}
	default:
		// Workers only call tools, so no server-to-client stream is needed.
		t = &sdk.StreamableClientTransport{Endpoint: srv.URL, HTTPClient: hc, DisableStandaloneSSE: true}
	}
	client := sdk.NewClient(&sdk.Implementation{Name: "heraion", Version: version}, nil)

	sess, err := client.Connect(ctx, t, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("connect %s: %w", srv.URL, err)
	}
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var out []llm.Tool
	for tl, err := range sess.Tools(cctx, nil) {
		if err != nil {
			_ = sess.Close()
			return nil, nil, fmt.Errorf("list tools: %w", err)
		}
		if !filter(tl.Name, srv.AllowTools, srv.DenyTools) {
			continue
		}
		schema, err := json.Marshal(tl.InputSchema)
		if err != nil || string(schema) == "null" {
			schema = []byte(`{"type":"object","properties":{}}`)
		}
		desc := tl.Description
		if desc == "" {
			desc = tl.Title
		}
		out = append(out, &tool{
			server: name, name: tl.Name, desc: "[" + name + "] " + desc,
			schema: schema, sess: sess, timeout: srv.Timeout.Duration,
		})
	}
	return sess, out, nil
}

func filter(name string, allow, deny []string) bool {
	for _, p := range deny {
		// A malformed deny pattern denies everything rather than nothing.
		if ok, err := path.Match(p, name); ok || err != nil {
			return false
		}
	}
	if len(allow) == 0 {
		return true
	}
	for _, p := range allow {
		if ok, _ := path.Match(p, name); ok {
			return true
		}
	}
	return false
}

// headerTransport sets headers on every request to host. Secret values are
// read per request, so a rotated ServiceAccount token is used as soon as it
// changes. Requests to other hosts, e.g. an SSE endpoint the server points
// elsewhere, get no headers.
type headerTransport struct {
	host     string
	base     http.RoundTripper
	static   map[string]string
	secrets  map[string]*secret.Source
	prefixes map[string]string
}

func (h *headerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if len(h.static)+len(h.secrets) == 0 || !strings.EqualFold(r.URL.Host, h.host) {
		return h.base.RoundTrip(r)
	}
	r = r.Clone(r.Context())
	for k, v := range h.static {
		r.Header.Set(k, v)
	}
	for k, src := range h.secrets {
		v, err := src.Value()
		if err != nil {
			return nil, fmt.Errorf("header %s: %w", k, err)
		}
		r.Header.Set(k, h.prefixes[k]+v)
	}
	return h.base.RoundTrip(r)
}

type tool struct {
	server, name, desc string
	schema             json.RawMessage
	sess               *sdk.ClientSession
	timeout            time.Duration
}

func (t *tool) Name() string            { return t.server + "." + t.name }
func (t *tool) Description() string     { return t.desc }
func (t *tool) Schema() json.RawMessage { return t.schema }

func (t *tool) Call(ctx context.Context, args json.RawMessage) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, t.timeout)
	defer cancel()
	res, err := t.sess.CallTool(ctx, &sdk.CallToolParams{Name: t.name, Arguments: args})
	if err != nil {
		return "", err
	}
	text := render(res)
	if res.IsError {
		if text == "" {
			text = "tool reported an error"
		}
		return "", errors.New(text)
	}
	return text, nil
}

// render flattens a tool result to text for the model.
func render(res *sdk.CallToolResult) string {
	var parts []string
	for _, c := range res.Content {
		switch c := c.(type) {
		case *sdk.TextContent:
			parts = append(parts, c.Text)
		case *sdk.EmbeddedResource:
			if c.Resource != nil && c.Resource.Text != "" {
				parts = append(parts, c.Resource.Text)
			} else if c.Resource != nil {
				parts = append(parts, fmt.Sprintf("[binary resource %s omitted]", c.Resource.URI))
			}
		case *sdk.ResourceLink:
			parts = append(parts, "[resource link: "+c.URI+"]")
		case *sdk.ImageContent:
			parts = append(parts, "[image omitted]")
		case *sdk.AudioContent:
			parts = append(parts, "[audio omitted]")
		}
	}
	if len(parts) == 0 && res.StructuredContent != nil {
		if b, err := json.Marshal(res.StructuredContent); err == nil {
			return string(b)
		}
	}
	return strings.Join(parts, "\n")
}
