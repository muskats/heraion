// Package atlassian is the HTTP part shared by the Jira and Confluence
// clients: auth, JSON and error messages.
//
//	Cloud        Authorization: Basic base64(email:api-token)
//	Data Center  Authorization: Bearer <personal access token>
package atlassian

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/muskats/heraion/internal/secret"
	"github.com/muskats/heraion/internal/textutil"
)

// Client sends authenticated JSON requests to one Atlassian site.
type Client struct {
	Email      string         // set: Basic auth (Cloud API token); empty: Bearer
	Token      *secret.Source // read per request, so a rotated token file works
	HTTPClient *http.Client
}

// New returns a Client with a 30s timeout.
func New(email string, token *secret.Source) *Client {
	return &Client{Email: email, Token: token, HTTPClient: &http.Client{Timeout: 30 * time.Second}}
}

// APIError is a non-2xx response with the server's message, e.g.
// `jira returned 400: summary: You must specify a summary of the issue.`
type APIError struct {
	Service string // "jira" or "confluence"
	Status  int
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("%s returned %d: %s", e.Service, e.Status, e.Message)
}

// Do sends in as JSON (nil: no body) and decodes the response into out (nil: ignore).
func (c *Client) Do(ctx context.Context, service, method, url string, in, out any) error {
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
	tok, err := c.Token.Value()
	if err != nil {
		return fmt.Errorf("%s token: %w", service, err)
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
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode/100 != 2 {
		return &APIError{Service: service, Status: resp.StatusCode, Message: ErrorText(raw)}
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("decode %s response: %w", service, err)
	}
	return nil
}

// ErrorText flattens the error bodies Atlassian APIs return:
//
//	Jira:            {"errorMessages": ["..."], "errors": {"summary": "..."}}
//	Confluence v1:   {"statusCode": 404, "message": "..."}
//	Confluence v2:   {"errors": [{"title": "...", "detail": "..."}]}
func ErrorText(raw []byte) string {
	var e struct {
		ErrorMessages []string        `json:"errorMessages"`
		Errors        json.RawMessage `json:"errors"`
		Message       string          `json:"message"`
	}
	var parts []string
	if json.Unmarshal(raw, &e) == nil {
		parts = append(parts, e.ErrorMessages...)
		var byField map[string]string
		var list []struct{ Title, Detail string }
		if json.Unmarshal(e.Errors, &byField) == nil {
			for f, m := range byField {
				parts = append(parts, f+": "+m)
			}
		} else if json.Unmarshal(e.Errors, &list) == nil {
			for _, x := range list {
				parts = append(parts, strings.TrimSpace(x.Title+" "+x.Detail))
			}
		}
		if e.Message != "" {
			parts = append(parts, e.Message)
		}
	}
	if len(parts) > 0 {
		return strings.Join(parts, "; ")
	}
	s := strings.TrimSpace(string(raw))
	if len(s) > 500 {
		s = s[:500] + "..."
	}
	if s == "" {
		s = "no details"
	}
	return s
}

// QuoteText makes s safe inside a double-quoted JQL or CQL string, e.g.
// `say "hi"` -> `say \"hi\"`. Long input is cut to 200 characters.
func QuoteText(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	s = textutil.Truncate(s, 200)
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s)
}

// QuoteList renders keys as a JQL/CQL list, e.g. ["OPS","CHG"] -> `"OPS", "CHG"`.
// Keys must already be validated (letters, digits, underscore).
func QuoteList(keys []string) string {
	q := make([]string, len(keys))
	for i, k := range keys {
		q[i] = `"` + k + `"`
	}
	return strings.Join(q, ", ")
}

// Clamp returns v within [lo, hi], or def when v is 0, e.g. for the days and
// limit arguments of the search tools.
func Clamp(v, def, lo, hi int) int {
	if v == 0 {
		v = def
	}
	return max(lo, min(hi, v))
}
