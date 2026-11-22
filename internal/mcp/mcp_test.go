package mcp_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/muskats/heraion/internal/config"
	"github.com/muskats/heraion/internal/llm"
	"github.com/muskats/heraion/internal/mcp"
	"github.com/muskats/heraion/internal/secret"
)

type listArgs struct {
	Kind string `json:"kind" jsonschema:"resource kind"`
}

func newServer() *sdk.Server {
	s := sdk.NewServer(&sdk.Implementation{Name: "fake-k8s", Version: "1"}, nil)
	sdk.AddTool(s, &sdk.Tool{Name: "resources_list", Description: "List resources"},
		func(_ context.Context, _ *sdk.CallToolRequest, in listArgs) (*sdk.CallToolResult, any, error) {
			if in.Kind == "Secret" {
				return &sdk.CallToolResult{IsError: true, Content: []sdk.Content{&sdk.TextContent{Text: "forbidden"}}}, nil, nil
			}
			return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: "listed " + in.Kind}}}, nil, nil
		})
	sdk.AddTool(s, &sdk.Tool{Name: "resources_delete", Description: "Delete"},
		func(context.Context, *sdk.CallToolRequest, listArgs) (*sdk.CallToolResult, any, error) {
			panic("must be filtered out")
		})
	sdk.AddTool(s, &sdk.Tool{Name: "pods_log", Description: "Logs"},
		func(context.Context, *sdk.CallToolRequest, listArgs) (*sdk.CallToolResult, any, error) {
			return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: "log"}}}, nil, nil
		})
	return s
}

type headerRecorder struct {
	mu   sync.Mutex
	seen []string
	next http.Handler
}

func (h *headerRecorder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	h.seen = append(h.seen, r.Header.Get("DD-API-KEY")+"|"+r.Header.Get("X-Static"))
	h.mu.Unlock()
	h.next.ServeHTTP(w, r)
}

func start(t *testing.T, sse bool) (string, *headerRecorder) {
	t.Helper()
	srv := newServer()
	var h http.Handler = sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return srv }, nil)
	if sse {
		h = sdk.NewSSEHandler(func(*http.Request) *sdk.Server { return srv }, nil)
	}
	rec := &headerRecorder{next: h}
	ts := httptest.NewServer(rec)
	t.Cleanup(ts.Close)
	return ts.URL, rec
}

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

func byName(tools []llm.Tool) map[string]llm.Tool {
	m := map[string]llm.Tool{}
	for _, t := range tools {
		m[t.Name()] = t
	}
	return m
}

func TestConnectFilterAndCall(t *testing.T) {
	for _, transport := range []string{"streamable", "sse"} {
		t.Run(transport, func(t *testing.T) {
			url, rec := start(t, transport == "sse")
			servers := map[string]*config.MCPServer{"k8s": {
				URL: url, Transport: transport,
				Headers:        map[string]string{"X-Static": "s"},
				HeadersFromEnv: map[string]string{"DD-API-KEY": "ENV"},
				SecretHeaders:  map[string]*secret.Source{"DD-API-KEY": secret.Static("secret-key")},
				DenyTools:      []string{"*_delete"},
				Timeout:        config.Duration{Duration: 5 * time.Second},
			}}
			m, tools, err := mcp.Connect(context.Background(), servers, []string{"*"}, "test", quiet)
			if err != nil {
				t.Fatal(err)
			}
			defer m.Close()

			got := byName(tools)
			var names []string
			for n := range got {
				names = append(names, n)
			}
			sort.Strings(names)
			if strings.Join(names, ",") != "k8s.pods_log,k8s.resources_list" {
				t.Fatalf("tools = %v", names)
			}
			list := got["k8s.resources_list"]
			var schema map[string]any
			if err := json.Unmarshal(list.Schema(), &schema); err != nil || schema["properties"] == nil {
				t.Errorf("schema not propagated: %s", list.Schema())
			}
			if !strings.HasPrefix(list.Description(), "[k8s] ") {
				t.Errorf("description %q", list.Description())
			}

			out, err := list.Call(context.Background(), json.RawMessage(`{"kind":"VulnerabilityReport"}`))
			if err != nil || out != "listed VulnerabilityReport" {
				t.Fatalf("call: %q %v", out, err)
			}
			if _, err := list.Call(context.Background(), json.RawMessage(`{"kind":"Secret"}`)); err == nil || !strings.Contains(err.Error(), "forbidden") {
				t.Fatalf("IsError must surface as error, got %v", err)
			}
			rec.mu.Lock()
			defer rec.mu.Unlock()
			for _, s := range rec.seen {
				if s != "secret-key|s" {
					t.Fatalf("headers not sent on every request: %q", s)
				}
			}
		})
	}
}

func TestAllowList(t *testing.T) {
	url, _ := start(t, false)
	servers := map[string]*config.MCPServer{"k8s": {URL: url, Transport: "streamable", AllowTools: []string{"pods_*"},
		Timeout: config.Duration{Duration: time.Second}}}
	m, tools, err := mcp.Connect(context.Background(), servers, []string{"*"}, "test", quiet)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if len(tools) != 1 || tools[0].Name() != "k8s.pods_log" {
		t.Fatalf("got %v", tools)
	}
}

func TestMalformedDenyPatternDeniesAll(t *testing.T) {
	url, _ := start(t, false)
	servers := map[string]*config.MCPServer{"k8s": {URL: url, Transport: "streamable", DenyTools: []string{"[pods"},
		Timeout: config.Duration{Duration: time.Second}}}
	m, tools, err := mcp.Connect(context.Background(), servers, []string{"*"}, "test", quiet)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if len(tools) != 0 {
		t.Fatalf("a malformed deny pattern must fail closed, got %d tools", len(tools))
	}
}

func TestUnreachable(t *testing.T) {
	dead := map[string]*config.MCPServer{"grafana": {URL: "http://127.0.0.1:1/mcp", Transport: "streamable"}}
	m, tools, err := mcp.Connect(context.Background(), dead, []string{"*"}, "test", quiet)
	if err != nil || len(tools) != 0 {
		t.Fatalf("optional server must be skipped: %v", err)
	}
	m.Close()
	dead["grafana"].Required = true
	if _, _, err := mcp.Connect(context.Background(), dead, []string{"*"}, "test", quiet); err == nil {
		t.Fatal("required server must fail the run")
	}
}

func TestMissingSecretHeader(t *testing.T) {
	url, _ := start(t, false)
	servers := map[string]*config.MCPServer{"dd": {URL: url, Transport: "streamable", Required: true,
		HeadersFromEnv: map[string]string{"DD-API-KEY": "HERAION_DD"}, SecretHeaders: map[string]*secret.Source{"DD-API-KEY": secret.FromMap("HERAION_DD", nil)}}}
	if _, _, err := mcp.Connect(context.Background(), servers, []string{"*"}, "test", quiet); err == nil || !strings.Contains(err.Error(), "HERAION_DD") {
		t.Fatalf("expected empty-env error, got %v", err)
	}
}

func TestNeededBy(t *testing.T) {
	cases := []struct {
		patterns []string
		server   string
		want     bool
	}{
		{[]string{"k8s.*", "repo_*"}, "k8s", true},
		{[]string{"k8s.*", "repo_*"}, "grafana", false},
		{[]string{"*"}, "grafana", true},
		{[]string{"k8s.resources_*"}, "k8s", true},
		{[]string{"repo_*", "open_pr"}, "jira", false},
		{[]string{"*.search"}, "jira", true},
	}
	for _, c := range cases {
		if got := mcp.NeededBy(c.patterns, c.server); got != c.want {
			t.Errorf("NeededBy(%v, %s) = %v", c.patterns, c.server, got)
		}
	}
}

func TestBearerPrefixFromTokenFile(t *testing.T) {
	srv := newServer()
	var mu sync.Mutex
	var seen []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Header.Get("Authorization"))
		mu.Unlock()
		sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return srv }, nil).ServeHTTP(w, r)
	}))
	t.Cleanup(ts.Close)

	tok := filepath.Join(t.TempDir(), "token")
	_ = os.WriteFile(tok, []byte("sa-token-1"), 0o600)
	servers := map[string]*config.MCPServer{"k8s": {
		URL: ts.URL, Transport: "streamable", Timeout: config.Duration{Duration: 5 * time.Second},
		SecretHeaders:  map[string]*secret.Source{"Authorization": secret.FromMap("T", map[string]string{"T_FILE": tok})},
		HeaderPrefixes: map[string]string{"Authorization": "Bearer "},
	}}
	m, _, err := mcp.Connect(context.Background(), servers, []string{"*"}, "test", quiet)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	mu.Lock()
	first := seen[0]
	mu.Unlock()
	if first != "Bearer sa-token-1" {
		t.Fatalf("Authorization = %q", first)
	}
}

func TestSecretHeaderNotSentAcrossRedirect(t *testing.T) {
	var mu sync.Mutex
	var leaked []string
	evil := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		leaked = append(leaked, r.Header.Get("DD-API-KEY"))
		mu.Unlock()
		http.NotFound(w, r)
	}))
	t.Cleanup(evil.Close)
	// 127.0.0.1 vs localhost: a different host with the same listener.
	evilURL := strings.Replace(evil.URL, "127.0.0.1", "localhost", 1)
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, evilURL+"/mcp", http.StatusTemporaryRedirect)
	}))
	t.Cleanup(redirector.Close)

	servers := map[string]*config.MCPServer{"datadog": {
		URL: redirector.URL + "/mcp", Transport: "streamable", Required: true,
		Timeout:       config.Duration{Duration: time.Second},
		SecretHeaders: map[string]*secret.Source{"DD-API-KEY": secret.Static("dd-secret-key")},
	}}
	if _, _, err := mcp.Connect(context.Background(), servers, []string{"*"}, "test", quiet); err == nil {
		t.Fatal("expected the cross-host redirect to fail")
	}
	mu.Lock()
	defer mu.Unlock()
	for _, v := range leaked {
		if v != "" {
			t.Fatalf("secret header sent to the redirect target: %q", v)
		}
	}
}

// A server that sends the headers of a tool call only with its result must
// get the tool timeout, not the connect timeout, e.g. 300ms of work with a
// 100ms connect timeout and timeout: 5s.
func TestSlowToolGetsToolTimeout(t *testing.T) {
	defer mcp.SetConnectTimeout(100 * time.Millisecond)()
	s := sdk.NewServer(&sdk.Implementation{Name: "slow", Version: "1"}, nil)
	sdk.AddTool(s, &sdk.Tool{Name: "slow"}, func(context.Context, *sdk.CallToolRequest, struct{}) (*sdk.CallToolResult, any, error) {
		time.Sleep(300 * time.Millisecond)
		return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: "done"}}}, nil, nil
	})
	ts := httptest.NewServer(sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return s },
		&sdk.StreamableHTTPOptions{JSONResponse: true}))
	defer ts.Close()

	srv := &config.MCPServer{URL: ts.URL, Transport: "streamable", Timeout: config.Duration{Duration: 5 * time.Second}}
	sess, tools, err := mcp.Dial(context.Background(), "s", srv, "test")
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	if out, err := tools[0].Call(context.Background(), json.RawMessage(`{}`)); err != nil || out != "done" {
		t.Fatalf("got %q, %v", out, err)
	}
}
