package jira

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

// readServer answers search and get like Jira Cloud (v3, ADF) or Data Center (v2, strings).
func readServer(t *testing.T, cloud bool, project string) (*Client, *[]string) {
	t.Helper()
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if jql, ok := body["jql"].(string); ok {
			seen = append(seen, r.URL.Path+" "+jql)
		} else {
			seen = append(seen, r.URL.Path+"?"+r.URL.RawQuery)
		}
		desc := `"Rolled out 1.12 at 10:00."`
		comment := `"Rolled back at 10:20."`
		if cloud {
			desc = `{"type":"doc","version":1,"content":[{"type":"paragraph","content":[{"type":"text","text":"Rolled out 1.12 at 10:00."}]},{"type":"bulletList","content":[{"type":"listItem","content":[{"type":"paragraph","content":[{"type":"text","text":"prod only"}]}]}]}]}`
			comment = `{"type":"doc","version":1,"content":[{"type":"paragraph","content":[{"type":"text","text":"Rolled back at 10:20."}]}]}`
		}
		fields := `"summary":"Upgrade ingress-nginx to 1.12","status":{"name":"Done"},"issuetype":{"name":"Change"},"priority":{"name":"High"},"updated":"2026-11-18T10:02:33.000+0000"`
		switch {
		case strings.Contains(r.URL.Path, "/search"):
			_, _ = io.WriteString(w, `{"issues":[{"key":"CHG-42","fields":{`+fields+`}}],"isLast":true}`)
		case strings.Contains(r.URL.Path, "/issue/"):
			_, _ = io.WriteString(w, `{"key":"CHG-42","fields":{`+fields+`,"project":{"key":"`+project+`"},"labels":["ingress"],"description":`+desc+`,"comment":{"comments":[{"created":"2026-11-18T10:20:00.000+0000","body":`+comment+`,"author":{"displayName":"Jane Doe"}}]}}}`)
		}
	}))
	t.Cleanup(srv.Close)
	return &Client{BaseURL: srv.URL, Cloud: cloud, HTTP: atlassian.New("", secret.Static("tok"))}, &seen
}

func callTool(t *testing.T, c *Client, name string, args map[string]any, spaces ...string) (string, error) {
	t.Helper()
	for _, tl := range ReadTools(c, c.BaseURL, spaces) {
		if tl.Name() == name {
			b, _ := json.Marshal(args)
			return tl.Call(context.Background(), b)
		}
	}
	t.Fatalf("no tool %s", name)
	return "", nil
}

func TestJiraSearchBuildsJQL(t *testing.T) {
	for _, cloud := range []bool{true, false} {
		c, seen := readServer(t, cloud, "CHG")
		out, err := callTool(t, c, "jira_search", map[string]any{"days": 3, "text": `ingress" OR project = SECRET`}, "CHG", "OPS")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out, "CHG-42 | Change | Done | High | 2026-11-18T10:02 | Upgrade ingress-nginx to 1.12") {
			t.Errorf("output %q", out)
		}
		wantPath := "/rest/api/2/search "
		if cloud {
			wantPath = "/rest/api/3/search/jql "
		}
		want := wantPath + `project in ("CHG", "OPS") AND updated >= -3d AND text ~ "ingress\" OR project = SECRET" ORDER BY updated DESC`
		if (*seen)[0] != want {
			t.Errorf("cloud=%v\n got %s\nwant %s", cloud, (*seen)[0], want)
		}
	}
}

func TestJiraSearchSpaceGuard(t *testing.T) {
	c, seen := readServer(t, true, "CHG")
	if _, err := callTool(t, c, "jira_search", map[string]any{"space": "HR"}, "CHG"); err == nil {
		t.Fatal("space outside the allowlist must be refused")
	}
	if len(*seen) != 0 {
		t.Fatal("nothing may reach Jira")
	}
}

func TestJiraGetIssue(t *testing.T) {
	c, seen := readServer(t, true, "CHG")
	out, err := callTool(t, c, "jira_get_issue", map[string]any{"key": "chg-42"}, "CHG")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"CHG-42: Upgrade ingress-nginx to 1.12", "Rolled out 1.12 at 10:00.", "- prod only", "Rolled back at 10:20.", "/browse/CHG-42"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
	if strings.Contains(out, "Jane Doe") {
		t.Error("comment authors must not be sent to the model")
	}
	if !strings.HasPrefix((*seen)[0], "/rest/api/3/issue/CHG-42?fields=") {
		t.Errorf("request %s", (*seen)[0])
	}
	if _, err := callTool(t, c, "jira_get_issue", map[string]any{"key": "HR-1"}, "CHG"); err == nil {
		t.Error("issue outside the allowlist must be refused")
	}
}

func TestJiraGetIssueMovedSpace(t *testing.T) {
	c, _ := readServer(t, false, "HR") // CHG-42 now lives in HR
	if _, err := callTool(t, c, "jira_get_issue", map[string]any{"key": "CHG-42"}, "CHG"); err == nil || !strings.Contains(err.Error(), "now in space HR") {
		t.Fatalf("moved issue must be refused, got %v", err)
	}
}
