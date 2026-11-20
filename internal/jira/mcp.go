// Package jira is the create_issue tool: a guard around the jira_search and
// jira_create_issue tools of an mcp-atlassian server
// (https://github.com/sooperset/mcp-atlassian). The server talks to Jira
// Cloud or Data Center and converts the Markdown description; this package
// adds the project allowlist, issue budget, de-duplication and dry-run.
package jira

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/muskats/heraion/internal/llm"
)

// The mcp-atlassian tools create_issue calls, and the arguments it passes.
// `heraion check --connect` fails if the server's schemas lack them.
const (
	SearchTool = "jira_search"       // jql, limit, fields
	CreateTool = "jira_create_issue" // project_key, summary, issue_type, description, additional_fields
)

// RequiredArgs lists the arguments each tool must accept.
var RequiredArgs = map[string][]string{
	SearchTool: {"jql", "limit", "fields"},
	CreateTool: {"project_key", "summary", "issue_type", "description", "additional_fields"},
}

// ErrNotConfigured is returned when create_issue has no server to call.
var ErrNotConfigured = errors.New("jira: no mcp-atlassian server configured")

// Issue is a created or found issue.
type Issue struct {
	Key string
	URL string // e.g. https://acme.atlassian.net/browse/OPS-123
}

// NewIssue is the input for Create.
type NewIssue struct {
	Project, Type, Summary, Description, Priority string
	Labels                                        []string
}

// MCP adapts an mcp-atlassian server's tools to API.
type MCP struct {
	Searcher, Creator llm.Tool // e.g. "atlassian.jira_search", "atlassian.jira_create_issue"
}

// FindOpen returns the newest issue jql finds, or nil, e.g. from
// {"issues":[{"key":"OPS-120","url":"https://..."}]}.
func (m MCP) FindOpen(ctx context.Context, jql string) (*Issue, error) {
	args, _ := json.Marshal(map[string]any{"jql": jql, "limit": 1, "fields": "summary"})
	out, err := m.Searcher.Call(ctx, args)
	if err != nil {
		return nil, err
	}
	var res struct{ Issues []Issue }
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		return nil, fmt.Errorf("unexpected %s result: %w", SearchTool, err)
	}
	if len(res.Issues) == 0 {
		return nil, nil
	}
	return &res.Issues[0], nil
}

// Create opens an issue, e.g. returning {"issue":{"key":"OPS-121","url":"..."}}.
func (m MCP) Create(ctx context.Context, in NewIssue) (*Issue, error) {
	extra := map[string]any{"labels": in.Labels}
	if in.Priority != "" {
		extra["priority"] = map[string]string{"name": in.Priority}
	}
	fields, _ := json.Marshal(extra)
	args, _ := json.Marshal(map[string]any{
		"project_key": in.Project, "summary": in.Summary, "issue_type": in.Type,
		"description": in.Description, "additional_fields": string(fields),
	})
	out, err := m.Creator.Call(ctx, args)
	if err != nil {
		return nil, err
	}
	var res struct{ Issue *Issue }
	if err := json.Unmarshal([]byte(out), &res); err != nil || res.Issue == nil || res.Issue.Key == "" {
		return nil, errors.New("unexpected " + CreateTool + " result: no issue key")
	}
	return res.Issue, nil
}
