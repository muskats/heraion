// Package jira creates Jira issues through the REST API, for Jira Cloud and
// Data Center:
//
//	Cloud        POST /rest/api/3/issue        description in ADF
//	             POST /rest/api/3/search/jql   (the old /search returns 410)
//	             auth: Basic base64(email:api-token)
//	Data Center  POST /rest/api/2/issue        description in wiki markup
//	             POST /rest/api/2/search
//	             auth: Bearer <personal access token>
package jira

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/muskats/heraion/internal/atlassian"
	"github.com/muskats/heraion/internal/config"
)

// Client talks to one Jira site.
type Client struct {
	BaseURL string // e.g. https://acme.atlassian.net
	Cloud   bool
	HTTP    *atlassian.Client
}

// NewClient builds a Client from the config.
func NewClient(c *config.Jira) *Client {
	return &Client{BaseURL: strings.TrimRight(c.URL, "/"), Cloud: c.Deployment == config.DeploymentCloud,
		HTTP: atlassian.New(c.Email, c.Token)}
}

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

func (c *Client) api() string {
	if c.Cloud {
		return c.BaseURL + "/rest/api/3"
	}
	return c.BaseURL + "/rest/api/2"
}

// Create opens an issue and returns its key and browse URL.
func (c *Client) Create(ctx context.Context, in NewIssue) (*Issue, error) {
	fields := map[string]any{
		"project":   map[string]string{"key": in.Project},
		"issuetype": map[string]string{"name": in.Type},
		"summary":   in.Summary,
	}
	if in.Description != "" {
		if c.Cloud {
			fields["description"] = ToADF(in.Description)
		} else {
			fields["description"] = ToWiki(in.Description)
		}
	}
	if len(in.Labels) > 0 {
		fields["labels"] = in.Labels
	}
	if in.Priority != "" {
		fields["priority"] = map[string]string{"name": in.Priority}
	}
	var out struct{ Key string }
	if err := c.do(ctx, http.MethodPost, c.api()+"/issue", map[string]any{"fields": fields}, &out); err != nil {
		return nil, err
	}
	return &Issue{Key: out.Key, URL: c.BaseURL + "/browse/" + out.Key}, nil
}

// FindOpen returns the first unresolved issue matching jql, or nil.
func (c *Client) FindOpen(ctx context.Context, jql string) (*Issue, error) {
	path := "/search"
	if c.Cloud {
		path = "/search/jql"
	}
	body := map[string]any{"jql": jql, "fields": []string{"summary"}, "maxResults": 1}
	var out struct {
		Issues []struct{ Key string } `json:"issues"`
	}
	if err := c.do(ctx, http.MethodPost, c.api()+path, body, &out); err != nil {
		return nil, err
	}
	if len(out.Issues) == 0 || out.Issues[0].Key == "" {
		return nil, nil
	}
	k := out.Issues[0].Key
	return &Issue{Key: k, URL: c.BaseURL + "/browse/" + k}, nil
}

// Myself checks the credentials (GET /myself), for `heraion check`.
func (c *Client) Myself(ctx context.Context) error {
	return c.do(ctx, http.MethodGet, c.api()+"/myself", nil, nil)
}

func (c *Client) do(ctx context.Context, method, url string, in, out any) error {
	return c.HTTP.Do(ctx, "jira", method, url, in, out)
}

// ErrNotConfigured is returned when no Jira site is configured.
var ErrNotConfigured = errors.New("jira is not configured")
