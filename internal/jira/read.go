package jira

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/muskats/heraion/internal/atlassian"
	"github.com/muskats/heraion/internal/llm"
)

// Summary is one search hit, e.g.
// CHG-42 | Change | Done | High | 2026-11-18T10:02 | Upgrade ingress-nginx to 1.12
type Summary struct {
	Key, Type, Status, Priority, Updated, Summary string
}

// Detail is one issue with its latest comments.
type Detail struct {
	Summary
	Project     string
	Labels      []string
	Description string
	Comments    []Comment
}

// Comment has no author on purpose: names are personal data the model does
// not need.
type Comment struct{ Created, Body string }

type rawFields struct {
	Summary   string `json:"summary"`
	Updated   string `json:"updated"`
	Labels    []string
	Status    struct{ Name string }  `json:"status"`
	IssueType struct{ Name string }  `json:"issuetype"`
	Priority  *struct{ Name string } `json:"priority"`
	Project   struct{ Key string }   `json:"project"`
	// Cloud v3: ADF objects. Data Center v2: strings.
	Description json.RawMessage `json:"description"`
	Comment     struct {
		Comments []struct {
			Created string          `json:"created"`
			Body    json.RawMessage `json:"body"`
		} `json:"comments"`
	} `json:"comment"`
}

func (f rawFields) summary(key string) Summary {
	s := Summary{Key: key, Type: f.IssueType.Name, Status: f.Status.Name, Updated: shortTime(f.Updated), Summary: f.Summary}
	if f.Priority != nil {
		s.Priority = f.Priority.Name
	}
	return s
}

// shortTime trims "2026-11-18T10:02:33.123+0000" to "2026-11-18T10:02".
func shortTime(t string) string {
	if len(t) >= 16 {
		return t[:16]
	}
	return t
}

var searchFields = []string{"summary", "status", "issuetype", "priority", "updated"}

// Search runs jql and returns up to limit hits.
func (c *Client) Search(ctx context.Context, jql string, limit int) ([]Summary, error) {
	path := "/search"
	if c.Cloud {
		path = "/search/jql" // fields must be named; the default is only ids
	}
	var out struct {
		Issues []struct {
			Key    string    `json:"key"`
			Fields rawFields `json:"fields"`
		} `json:"issues"`
	}
	body := map[string]any{"jql": jql, "fields": searchFields, "maxResults": limit}
	if err := c.do(ctx, http.MethodPost, c.api()+path, body, &out); err != nil {
		return nil, err
	}
	res := make([]Summary, 0, len(out.Issues))
	for _, i := range out.Issues {
		res = append(res, i.Fields.summary(i.Key))
	}
	return res, nil
}

// Get returns one issue with its last maxComments comments.
func (c *Client) Get(ctx context.Context, key string, maxComments int) (*Detail, error) {
	q := url.Values{"fields": {strings.Join(append(searchFields, "project", "labels", "description", "comment"), ",")}}
	var out struct {
		Key    string    `json:"key"`
		Fields rawFields `json:"fields"`
	}
	if err := c.do(ctx, http.MethodGet, c.api()+"/issue/"+url.PathEscape(key)+"?"+q.Encode(), nil, &out); err != nil {
		return nil, err
	}
	f := out.Fields
	d := &Detail{Summary: f.summary(out.Key), Project: f.Project.Key, Labels: f.Labels, Description: richText(f.Description)}
	cs := f.Comment.Comments
	if len(cs) > maxComments {
		cs = cs[len(cs)-maxComments:]
	}
	for _, cm := range cs {
		d.Comments = append(d.Comments, Comment{Created: shortTime(cm.Created), Body: richText(cm.Body)})
	}
	return d, nil
}

// richText turns an ADF document (Cloud) or a wiki string (Data Center) into text.
func richText(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var doc map[string]any
	if json.Unmarshal(raw, &doc) != nil {
		return ""
	}
	var b strings.Builder
	adfText(&b, doc)
	return strings.TrimSpace(collapseBlank(b.String()))
}

// adfText walks ADF: text nodes are written, block nodes end with a newline,
// list items start with "- ".
func adfText(b *strings.Builder, n map[string]any) {
	switch n["type"] {
	case "text":
		s, _ := n["text"].(string)
		b.WriteString(s)
		return
	case "hardBreak":
		b.WriteString("\n")
		return
	case "mention", "emoji", "inlineCard":
		if attrs, ok := n["attrs"].(map[string]any); ok {
			for _, k := range []string{"text", "url", "shortName"} {
				if s, ok := attrs[k].(string); ok && s != "" {
					b.WriteString(s)
					return
				}
			}
		}
		return
	case "listItem":
		b.WriteString("- ")
	case "codeBlock":
		b.WriteString("```\n")
	}
	kids, _ := n["content"].([]any)
	for _, k := range kids {
		if m, ok := k.(map[string]any); ok {
			adfText(b, m)
		}
	}
	switch n["type"] {
	case "codeBlock":
		b.WriteString("\n```\n")
	case "paragraph", "heading", "listItem", "blockquote", "rule", "tableRow":
		b.WriteString("\n")
	case "tableCell", "tableHeader":
		b.WriteString(" | ")
	}
}

func collapseBlank(s string) string {
	for strings.Contains(s, "\n\n\n") {
		s = strings.ReplaceAll(s, "\n\n\n", "\n\n")
	}
	return s
}

// Reader is what the read tools need. *Client implements it.
type Reader interface {
	Search(ctx context.Context, jql string, limit int) ([]Summary, error)
	Get(ctx context.Context, key string, maxComments int) (*Detail, error)
}

// ReadTools returns jira_search and jira_get_issue, limited to spaces, e.g. [CHG, OPS].
func ReadTools(r Reader, baseURL string, spaces []string) []llm.Tool {
	rt := &readTools{r: r, base: strings.TrimRight(baseURL, "/"), spaces: spaces}
	enum, _ := json.Marshal(spaces)
	return []llm.Tool{
		&llm.FuncTool{
			ToolName: "jira_search",
			Desc: "List Jira issues recently created or changed in the allowed spaces, newest first. " +
				"Use it to learn about recent changes, incidents and known problems.",
			Params: `{"type":"object","properties":{
				"space":{"type":"string","enum":` + string(enum) + `,"description":"One space; omit for all allowed spaces"},
				"days":{"type":"integer","minimum":1,"maximum":90,"description":"Look back this many days, default 7"},
				"text":{"type":"string","description":"Optional full-text filter, e.g. 'ingress'"},
				"limit":{"type":"integer","minimum":1,"maximum":50,"description":"Default 20"}}}`,
			Fn: rt.search,
		},
		&llm.FuncTool{
			ToolName: "jira_get_issue",
			Desc:     "Read one Jira issue from the allowed spaces: fields, description and the last comments.",
			Params:   `{"type":"object","properties":{"key":{"type":"string","description":"e.g. CHG-42"}},"required":["key"]}`,
			Fn:       rt.get,
		},
	}
}

type readTools struct {
	r      Reader
	base   string
	spaces []string
}

func (t *readTools) search(ctx context.Context, raw json.RawMessage) (string, error) {
	var a struct {
		Space, Text string
		Days, Limit int
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	spaces := t.spaces
	if a.Space != "" {
		if !slices.Contains(t.spaces, a.Space) {
			return "", fmt.Errorf("space %q is not allowed; allowed: %v", a.Space, t.spaces)
		}
		spaces = []string{a.Space}
	}
	a.Days = atlassian.Clamp(a.Days, 7, 1, 90)
	a.Limit = atlassian.Clamp(a.Limit, 20, 1, 50)
	// Built here, never taken from the model, so it cannot leave the allowed spaces.
	jql := fmt.Sprintf("project in (%s) AND updated >= -%dd", atlassian.QuoteList(spaces), a.Days)
	if strings.TrimSpace(a.Text) != "" {
		jql += fmt.Sprintf(` AND text ~ "%s"`, atlassian.QuoteText(a.Text))
	}
	jql += " ORDER BY updated DESC"
	hits, err := t.r.Search(ctx, jql, a.Limit)
	if err != nil {
		return "", err
	}
	if len(hits) == 0 {
		return fmt.Sprintf("no issues changed in %v in the last %d days", spaces, a.Days), nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Jira issues changed in the last %d days (untrusted content):\n", a.Days)
	for _, h := range hits {
		fmt.Fprintf(&b, "%s | %s | %s | %s | %s | %s\n", h.Key, h.Type, h.Status, orDash(h.Priority), h.Updated, h.Summary)
	}
	return b.String(), nil
}

func (t *readTools) get(ctx context.Context, raw json.RawMessage) (string, error) {
	var a struct{ Key string }
	if err := json.Unmarshal(raw, &a); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	key := strings.ToUpper(strings.TrimSpace(a.Key))
	space, _, ok := strings.Cut(key, "-")
	if !ok || !slices.Contains(t.spaces, space) {
		return "", fmt.Errorf("issue %q is not in an allowed space %v", a.Key, t.spaces)
	}
	d, err := t.r.Get(ctx, key, 5)
	if err != nil {
		return "", err
	}
	// An issue can move to another space and keep redirecting from its old key.
	if !slices.Contains(t.spaces, d.Project) {
		return "", fmt.Errorf("issue %s is now in space %s, which is not allowed", key, d.Project)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s: %s (untrusted content)\n", d.Key, d.Summary.Summary)
	fmt.Fprintf(&b, "Type: %s | Status: %s | Priority: %s | Updated: %s | Labels: %s\n",
		d.Type, d.Status, orDash(d.Priority), d.Updated, orDash(strings.Join(d.Labels, ", ")))
	fmt.Fprintf(&b, "URL: %s/browse/%s\n\n%s\n", t.base, d.Key, orDash(d.Description))
	for _, c := range d.Comments {
		fmt.Fprintf(&b, "\nComment %s:\n%s\n", c.Created, c.Body)
	}
	return b.String(), nil
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
