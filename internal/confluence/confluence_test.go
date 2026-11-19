package confluence

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/muskats/heraion/internal/atlassian"
	"github.com/muskats/heraion/internal/secret"
)

const storage = `<h1>Rollback</h1><p>If the canary fails:</p><ul><li>kubectl rollout undo deploy/payments</li><li>post in #ops</li></ul>` +
	`<ac:structured-macro ac:name="code"><ac:plain-text-body><![CDATA[helm rollback payments 41]]></ac:plain-text-body></ac:structured-macro>` +
	`<table><tr><th>Env</th><th>Owner</th></tr><tr><td>prod</td><td>sre</td></tr></table>`

func server(t *testing.T, cloud bool, pageSpace string) (*Client, *[]string) {
	t.Helper()
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.URL.Path+"?"+r.URL.Query().Encode())
		body, _ := json.Marshal(storage)
		switch r.URL.Path {
		case "/wiki/rest/api/search":
			_, _ = io.WriteString(w, `{"results":[{"content":{"id":"98765","title":"Payments runbook","space":{"key":"RUN"}},"excerpt":"@@@hl@@@rollback@@@endhl@@@ steps","lastModified":"2026-11-17T08:00:00.000Z"}]}`)
		case "/wiki/api/v2/pages/98765":
			_, _ = io.WriteString(w, `{"id":"98765","title":"Payments runbook","spaceId":"111","version":{"number":7,"createdAt":"2026-11-17T08:00:00.000Z"},"body":{"storage":{"value":`+string(body)+`,"representation":"storage"}}}`)
		case "/wiki/api/v2/spaces":
			_, _ = io.WriteString(w, `{"results":[{"id":"111","key":"`+pageSpace+`"}]}`)
		case "/rest/api/content/search":
			_, _ = io.WriteString(w, `{"results":[{"id":"98765","title":"Payments runbook","space":{"key":"RUN"},"version":{"when":"2026-11-17T08:00:00.000Z"}}]}`)
		case "/rest/api/content/98765":
			_, _ = io.WriteString(w, `{"id":"98765","title":"Payments runbook","space":{"key":"`+pageSpace+`"},"version":{"number":7,"when":"2026-11-17T08:00:00.000Z"},"body":{"storage":{"value":`+string(body)+`}}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	base := srv.URL
	if cloud {
		base += "/wiki"
	}
	return &Client{BaseURL: base, Cloud: cloud, HTTP: atlassian.New("", secret.Static("tok"))}, &seen
}

func call(t *testing.T, c *Client, name string, args map[string]any) (string, error) {
	t.Helper()
	for _, tl := range Tools(c, c.BaseURL, []string{"RUN", "OPS"}, 0) {
		if tl.Name() == name {
			b, _ := json.Marshal(args)
			return tl.Call(context.Background(), b)
		}
	}
	t.Fatalf("no tool %s", name)
	return "", nil
}

func TestSearch(t *testing.T) {
	for _, cloud := range []bool{true, false} {
		c, seen := server(t, cloud, "RUN")
		out, err := call(t, c, "confluence_search", map[string]any{"space": "RUN", "days": 14, "text": "rollback"})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out, "98765 | RUN | 2026-11-17T08:00 | Payments runbook") {
			t.Errorf("cloud=%v output %q", cloud, out)
		}
		if cloud && !strings.Contains(out, "rollback steps") {
			t.Errorf("excerpt highlight markers not removed: %q", out)
		}
		path := "/rest/api/content/search?"
		if cloud {
			path = "/wiki/rest/api/search?"
		}
		want := `space in ("RUN") AND type = page AND lastmodified >= now("-14d") AND text ~ "rollback" ORDER BY lastmodified DESC`
		if !strings.HasPrefix((*seen)[0], path) || !strings.Contains((*seen)[0], "cql="+urlEnc(want)) {
			t.Errorf("cloud=%v request %s", cloud, (*seen)[0])
		}
	}
}

func TestGetPage(t *testing.T) {
	for _, cloud := range []bool{true, false} {
		c, _ := server(t, cloud, "RUN")
		out, err := call(t, c, "confluence_get_page", map[string]any{"id": "98765"})
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{"Payments runbook (space RUN, version 7", "Rollback", "- kubectl rollout undo deploy/payments", "helm rollback payments 41", "prod", "pageId=98765"} {
			if !strings.Contains(out, want) {
				t.Errorf("cloud=%v missing %q in\n%s", cloud, want, out)
			}
		}
	}
}

func TestSpaceGuards(t *testing.T) {
	for _, cloud := range []bool{true, false} {
		c, seen := server(t, cloud, "HR") // page 98765 lives in HR
		if _, err := call(t, c, "confluence_get_page", map[string]any{"id": "98765"}); err == nil || !strings.Contains(err.Error(), "space HR") {
			t.Errorf("cloud=%v: page outside the allowlist must be refused, got %v", cloud, err)
		}
		n := len(*seen)
		if _, err := call(t, c, "confluence_search", map[string]any{"space": "HR"}); err == nil {
			t.Error("search in a disallowed space must be refused")
		}
		if _, err := call(t, c, "confluence_get_page", map[string]any{"id": "../../admin"}); err == nil {
			t.Error("non-numeric id must be refused")
		}
		if len(*seen) != n {
			t.Error("refused calls must not reach Confluence")
		}
	}
}

func TestStorageToText(t *testing.T) {
	got := StorageToText(storage)
	want := "Rollback\n\nIf the canary fails:\n\n- kubectl rollout undo deploy/payments\n- post in #ops\n\nhelm rollback payments 41\n\nEnv | Owner\nprod | sre"
	if got != want {
		t.Errorf("got\n%q\nwant\n%q", got, want)
	}
}

func urlEnc(s string) string {
	r := strings.NewReplacer(" ", "+", `"`, "%22", "(", "%28", ")", "%29", "=", "%3D", ">", "%3E", "~", "~", ",", "%2C")
	return r.Replace(s)
}
