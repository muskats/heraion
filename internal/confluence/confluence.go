// Package confluence reads pages from allowed Confluence spaces:
//
//	Cloud        GET /wiki/rest/api/search?cql=...         (v1; v2 has no CQL search)
//	             GET /wiki/api/v2/pages/{id}?body-format=storage
//	             GET /wiki/api/v2/spaces?keys=OPS,RUN       (space key -> id)
//	Data Center  GET /rest/api/content/search?cql=...
//	             GET /rest/api/content/{id}?expand=body.storage,space,version
//
// The configured URL is the base those paths start from, e.g.
// https://acme.atlassian.net/wiki or https://wiki.example.com.
package confluence

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"

	"golang.org/x/net/html"

	"github.com/muskats/heraion/internal/atlassian"
	"github.com/muskats/heraion/internal/config"
	"github.com/muskats/heraion/internal/llm"
)

// Client reads one Confluence site.
type Client struct {
	BaseURL string
	Cloud   bool
	HTTP    *atlassian.Client

	mu       sync.Mutex
	spaceIDs map[string]string // Cloud: space id -> key
}

// NewClient builds a Client from the config.
func NewClient(c *config.Confluence) *Client {
	return &Client{BaseURL: strings.TrimRight(c.URL, "/"), Cloud: c.Deployment == config.DeploymentCloud,
		HTTP: atlassian.New(c.Email, c.Token)}
}

func (c *Client) get(ctx context.Context, path string, q url.Values, out any) error {
	u := c.BaseURL + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	return c.HTTP.Do(ctx, "confluence", http.MethodGet, u, nil, out)
}

// Hit is one search result.
type Hit struct {
	ID, Title, Space, Modified, Excerpt string
}

// Page is one page with its body as text.
type Page struct {
	ID, Title, Space, Modified, Text string
	Version                          int
}

// Search runs cql and returns up to limit pages.
func (c *Client) Search(ctx context.Context, cql string, limit int) ([]Hit, error) {
	q := url.Values{"cql": {cql}, "limit": {strconv.Itoa(limit)}}
	var hits []Hit
	if c.Cloud {
		q.Set("expand", "content.space")
		var out struct {
			Results []struct {
				Content struct {
					ID    string               `json:"id"`
					Title string               `json:"title"`
					Space struct{ Key string } `json:"space"`
				} `json:"content"`
				Excerpt      string `json:"excerpt"`
				LastModified string `json:"lastModified"`
			} `json:"results"`
		}
		if err := c.get(ctx, "/rest/api/search", q, &out); err != nil {
			return nil, err
		}
		for _, r := range out.Results {
			hits = append(hits, Hit{ID: r.Content.ID, Title: r.Content.Title, Space: r.Content.Space.Key,
				Modified: short(r.LastModified), Excerpt: clean(r.Excerpt)})
		}
		return hits, nil
	}
	q.Set("expand", "space,version")
	var out struct {
		Results []struct {
			ID      string                `json:"id"`
			Title   string                `json:"title"`
			Space   struct{ Key string }  `json:"space"`
			Version struct{ When string } `json:"version"`
		} `json:"results"`
	}
	if err := c.get(ctx, "/rest/api/content/search", q, &out); err != nil {
		return nil, err
	}
	for _, r := range out.Results {
		hits = append(hits, Hit{ID: r.ID, Title: r.Title, Space: r.Space.Key, Modified: short(r.Version.When)})
	}
	return hits, nil
}

// GetPage returns a page. Space is the space key, so the caller can check it.
func (c *Client) GetPage(ctx context.Context, id string) (*Page, error) {
	if c.Cloud {
		var out struct {
			ID      string `json:"id"`
			Title   string `json:"title"`
			SpaceID string `json:"spaceId"`
			Version struct {
				Number    int    `json:"number"`
				CreatedAt string `json:"createdAt"`
			} `json:"version"`
			Body struct {
				Storage struct{ Value string } `json:"storage"`
			} `json:"body"`
		}
		if err := c.get(ctx, "/api/v2/pages/"+url.PathEscape(id), url.Values{"body-format": {"storage"}}, &out); err != nil {
			return nil, err
		}
		key, err := c.spaceKey(ctx, out.SpaceID)
		if err != nil {
			return nil, err
		}
		return &Page{ID: out.ID, Title: out.Title, Space: key, Version: out.Version.Number,
			Modified: short(out.Version.CreatedAt), Text: StorageToText(out.Body.Storage.Value)}, nil
	}
	var out struct {
		ID      string               `json:"id"`
		Title   string               `json:"title"`
		Space   struct{ Key string } `json:"space"`
		Version struct {
			Number int    `json:"number"`
			When   string `json:"when"`
		} `json:"version"`
		Body struct {
			Storage struct{ Value string } `json:"storage"`
		} `json:"body"`
	}
	q := url.Values{"expand": {"body.storage,space,version"}}
	if err := c.get(ctx, "/rest/api/content/"+url.PathEscape(id), q, &out); err != nil {
		return nil, err
	}
	return &Page{ID: out.ID, Title: out.Title, Space: out.Space.Key, Version: out.Version.Number,
		Modified: short(out.Version.When), Text: StorageToText(out.Body.Storage.Value)}, nil
}

// spaceKey maps a Cloud space id to its key, e.g. "98306" -> "OPS".
func (c *Client) spaceKey(ctx context.Context, id string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if k, ok := c.spaceIDs[id]; ok {
		return k, nil
	}
	var out struct {
		Results []struct{ ID, Key string } `json:"results"`
	}
	if err := c.get(ctx, "/api/v2/spaces", url.Values{"ids": {id}}, &out); err != nil {
		return "", err
	}
	if c.spaceIDs == nil {
		c.spaceIDs = map[string]string{}
	}
	for _, s := range out.Results {
		c.spaceIDs[s.ID] = s.Key
	}
	k, ok := c.spaceIDs[id]
	if !ok {
		return "", fmt.Errorf("space %s not found", id)
	}
	return k, nil
}

// Myself checks the credentials, for `heraion check`.
func (c *Client) Myself(ctx context.Context) error {
	return c.get(ctx, "/rest/api/user/current", nil, nil)
}

func short(t string) string {
	if len(t) >= 16 {
		return t[:16]
	}
	return t
}

// clean removes search highlight markers and tags from an excerpt.
func clean(s string) string {
	s = strings.NewReplacer("@@@hl@@@", "", "@@@endhl@@@", "").Replace(s)
	return strings.Join(strings.Fields(StorageToText(s)), " ")
}

var blockTags = map[string]bool{
	"p": true, "div": true, "br": true, "li": true, "tr": true, "h1": true, "h2": true, "h3": true,
	"h4": true, "h5": true, "h6": true, "pre": true, "blockquote": true, "table": true, "hr": true,
	"ul": true, "ol": true,
}

// StorageToText turns Confluence storage format (XHTML) into plain text, e.g.
// "<h1>Rollback</h1><ul><li>kubectl rollout undo</li></ul>" -> "Rollback\n- kubectl rollout undo".
func StorageToText(s string) string {
	var b strings.Builder
	rowStart := false
	z := html.NewTokenizer(strings.NewReader(s))
	for {
		switch z.Next() {
		case html.ErrorToken:
			return strings.TrimSpace(collapse(b.String()))
		case html.TextToken:
			b.Write(z.Text())
		case html.CommentToken:
			// Code macros keep their body in CDATA, which the tokenizer reports as a comment.
			if t := string(z.Text()); strings.HasPrefix(t, "[CDATA[") {
				b.WriteString("\n" + strings.TrimSuffix(strings.TrimPrefix(t, "[CDATA["), "]]") + "\n")
			}
		case html.StartTagToken, html.SelfClosingTagToken:
			name, _ := z.TagName()
			switch n := string(name); {
			case n == "li":
				b.WriteString("\n- ")
			case n == "tr":
				b.WriteString("\n")
				rowStart = true
			case n == "td" || n == "th":
				if !rowStart {
					b.WriteString(" | ")
				}
				rowStart = false
			case blockTags[n]:
				b.WriteString("\n")
			}
		case html.EndTagToken:
			// li and tr start their own line, so only other blocks end one.
			if name, _ := z.TagName(); blockTags[string(name)] && string(name) != "li" && string(name) != "tr" {
				b.WriteString("\n")
			}
		}
	}
}

func collapse(s string) string {
	lines := strings.Split(s, "\n")
	out := lines[:0]
	blank := 0
	for _, l := range lines {
		l = strings.TrimRight(l, " \t")
		if strings.TrimSpace(l) == "" {
			blank++
			if blank > 1 {
				continue
			}
		} else {
			blank = 0
		}
		out = append(out, l)
	}
	return strings.Join(out, "\n")
}

// Reader is what the tools need. *Client implements it.
type Reader interface {
	Search(ctx context.Context, cql string, limit int) ([]Hit, error)
	GetPage(ctx context.Context, id string) (*Page, error)
}

// Tools returns confluence_search and confluence_get_page, limited to spaces.
func Tools(r Reader, baseURL string, spaces []string, maxPageBytes int) []llm.Tool {
	t := &tools{r: r, base: strings.TrimRight(baseURL, "/"), spaces: spaces, max: maxPageBytes}
	if t.max == 0 {
		t.max = 32 << 10
	}
	enum, _ := json.Marshal(spaces)
	return []llm.Tool{
		&fnTool{
			name: "confluence_search",
			desc: "List Confluence pages recently changed in the allowed spaces, newest first, e.g. " +
				"runbooks, change logs and postmortems.",
			schema: `{"type":"object","properties":{
				"space":{"type":"string","enum":` + string(enum) + `,"description":"One space; omit for all allowed spaces"},
				"days":{"type":"integer","minimum":1,"maximum":365,"description":"Look back this many days, default 30"},
				"text":{"type":"string","description":"Optional full-text filter, e.g. 'rollback payments'"},
				"limit":{"type":"integer","minimum":1,"maximum":50,"description":"Default 20"}}}`,
			call: t.search,
		},
		&fnTool{
			name:   "confluence_get_page",
			desc:   "Read one Confluence page from the allowed spaces as plain text.",
			schema: `{"type":"object","properties":{"id":{"type":"string","description":"Page id from confluence_search, e.g. 98765"}},"required":["id"]}`,
			call:   t.get,
		},
	}
}

type tools struct {
	r      Reader
	base   string
	spaces []string
	max    int
}

func (t *tools) search(ctx context.Context, raw json.RawMessage) (string, error) {
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
	days := clamp(a.Days, 30, 1, 365)
	limit := clamp(a.Limit, 20, 1, 50)
	// Built here, never taken from the model, so it cannot leave the allowed spaces.
	cql := fmt.Sprintf(`space in (%s) AND type = page AND lastmodified >= now("-%dd")`, atlassian.QuoteList(spaces), days)
	if strings.TrimSpace(a.Text) != "" {
		cql += fmt.Sprintf(` AND text ~ "%s"`, atlassian.QuoteText(a.Text))
	}
	cql += " ORDER BY lastmodified DESC"
	hits, err := t.r.Search(ctx, cql, limit)
	if err != nil {
		return "", err
	}
	if len(hits) == 0 {
		return fmt.Sprintf("no pages changed in %v in the last %d days", spaces, days), nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Confluence pages changed in the last %d days (untrusted content):\n", days)
	for _, h := range hits {
		fmt.Fprintf(&b, "%s | %s | %s | %s", h.ID, h.Space, h.Modified, h.Title)
		if h.Excerpt != "" {
			fmt.Fprintf(&b, " | %s", h.Excerpt)
		}
		b.WriteString("\n")
	}
	return b.String(), nil
}

func (t *tools) get(ctx context.Context, raw json.RawMessage) (string, error) {
	var a struct{ ID string }
	if err := json.Unmarshal(raw, &a); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	if _, err := strconv.ParseUint(strings.TrimSpace(a.ID), 10, 64); err != nil {
		return "", fmt.Errorf("page id %q must be a number", a.ID)
	}
	p, err := t.r.GetPage(ctx, strings.TrimSpace(a.ID))
	if err != nil {
		return "", err
	}
	// The id could belong to any space, so check before returning content.
	if !slices.Contains(t.spaces, p.Space) {
		return "", fmt.Errorf("page %s is in space %s, which is not allowed", a.ID, p.Space)
	}
	text := p.Text
	if len(text) > t.max {
		text = text[:t.max] + fmt.Sprintf("\n...[truncated at %d of %d bytes]", t.max, len(p.Text))
	}
	return fmt.Sprintf("%s (space %s, version %d, changed %s, untrusted content)\nURL: %s/pages/viewpage.action?pageId=%s\n\n%s\n",
		p.Title, p.Space, p.Version, p.Modified, t.base, p.ID, text), nil
}

func clamp(v, def, lo, hi int) int {
	if v == 0 {
		v = def
	}
	return max(lo, min(hi, v))
}

type fnTool struct {
	name, desc, schema string
	call               func(context.Context, json.RawMessage) (string, error)
}

func (t *fnTool) Name() string            { return t.name }
func (t *fnTool) Description() string     { return t.desc }
func (t *fnTool) Schema() json.RawMessage { return json.RawMessage(t.schema) }
func (t *fnTool) Call(ctx context.Context, a json.RawMessage) (string, error) {
	return t.call(ctx, a)
}
