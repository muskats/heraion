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
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/muskats/heraion/internal/config"
	"github.com/muskats/heraion/internal/secret"
)

// Client talks to one Jira site.
type Client struct {
	BaseURL    string // e.g. https://acme.atlassian.net
	Cloud      bool
	Email      string         // Cloud: Basic auth user. Empty: Bearer token.
	Token      *secret.Source // API token (Cloud) or PAT (Data Center)
	HTTPClient *http.Client
}

// NewClient builds a Client from the config.
func NewClient(c *config.Jira) *Client {
	return &Client{
		BaseURL: strings.TrimRight(c.URL, "/"), Cloud: c.Deployment == config.JiraCloud,
		Email: c.Email, Token: c.Token, HTTPClient: &http.Client{Timeout: 30 * time.Second},
	}
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

// APIError is a non-2xx response, with Jira's error text, e.g.
// `400: summary: You must specify a summary of the issue.`
type APIError struct {
	Status  int
	Message string
}

func (e *APIError) Error() string { return fmt.Sprintf("jira returned %d: %s", e.Status, e.Message) }

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
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	tok, err := c.Token.Value() // per request, so a rotated token file is picked up
	if err != nil {
		return fmt.Errorf("jira token: %w", err)
	}
	if c.Email != "" {
		req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(c.Email+":"+tok)))
	} else {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode/100 != 2 {
		return &APIError{Status: resp.StatusCode, Message: errorText(raw)}
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("decode jira response: %w", err)
	}
	return nil
}

// errorText flattens Jira's {"errorMessages": [...], "errors": {field: msg}}.
func errorText(raw []byte) string {
	var e struct {
		ErrorMessages []string          `json:"errorMessages"`
		Errors        map[string]string `json:"errors"`
	}
	if json.Unmarshal(raw, &e) != nil || (len(e.ErrorMessages) == 0 && len(e.Errors) == 0) {
		s := strings.TrimSpace(string(raw))
		if len(s) > 500 {
			s = s[:500] + "..."
		}
		if s == "" {
			s = "no details"
		}
		return s
	}
	parts := append([]string{}, e.ErrorMessages...)
	for f, m := range e.Errors {
		parts = append(parts, f+": "+m)
	}
	return strings.Join(parts, "; ")
}

// ErrNotConfigured is returned when no Jira site is configured.
var ErrNotConfigured = errors.New("jira is not configured")
