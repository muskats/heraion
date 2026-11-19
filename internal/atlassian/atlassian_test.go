package atlassian

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/muskats/heraion/internal/secret"
)

func TestDoAuthModes(t *testing.T) {
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.Header.Get("Authorization"))
		if r.Header.Get("Accept") != "application/json" {
			t.Errorf("Accept = %q", r.Header.Get("Accept"))
		}
		_, _ = w.Write([]byte(`{"key":"OPS-1"}`))
	}))
	defer srv.Close()

	var out struct{ Key string }
	cloud := New("bot@acme.com", secret.Static("api-token"))
	if err := cloud.Do(context.Background(), "jira", http.MethodGet, srv.URL, nil, &out); err != nil || out.Key != "OPS-1" {
		t.Fatalf("%v %+v", err, out)
	}
	dc := New("", secret.Static("pat-token"))
	if err := dc.Do(context.Background(), "jira", http.MethodGet, srv.URL, nil, nil); err != nil {
		t.Fatal(err)
	}
	wantBasic := "Basic " + base64.StdEncoding.EncodeToString([]byte("bot@acme.com:api-token"))
	if got[0] != wantBasic || got[1] != "Bearer pat-token" {
		t.Fatalf("auth headers %q", got)
	}
}

func TestDoSendsJSONBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Content-Type") != "application/json" || r.Method != http.MethodPost {
			t.Errorf("method %s content-type %q", r.Method, r.Header.Get("Content-Type"))
		}
		b, _ := io.ReadAll(r.Body)
		if string(b) != `{"jql":"project = OPS"}` {
			t.Errorf("body %q", b)
		}
	}))
	defer srv.Close()
	err := New("", secret.Static("t")).Do(context.Background(), "jira", http.MethodPost, srv.URL,
		map[string]string{"jql": "project = OPS"}, nil)
	if err != nil {
		t.Fatal(err)
	}
}

func TestDoErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"errorMessages":["bad jql"]}`))
	}))
	defer srv.Close()
	err := New("", secret.Static("t")).Do(context.Background(), "jira", http.MethodGet, srv.URL, nil, nil)
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != 400 || apiErr.Error() != "jira returned 400: bad jql" {
		t.Fatalf("got %v", err)
	}

	// No token: fail before any request is sent.
	err = New("", secret.FromMap("HERAION_JIRA_TOKEN", nil)).Do(context.Background(), "jira", http.MethodGet, srv.URL, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "jira token") {
		t.Fatalf("missing token: %v", err)
	}

	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("<html>")) }))
	defer bad.Close()
	var out map[string]any
	err = New("", secret.Static("t")).Do(context.Background(), "confluence", http.MethodGet, bad.URL, nil, &out)
	if err == nil || !strings.Contains(err.Error(), "decode confluence response") {
		t.Fatalf("bad JSON: %v", err)
	}
}

func TestErrorText(t *testing.T) {
	cases := map[string]string{
		`{"errorMessages":["a"],"errors":{"summary":"required"}}`:    "a; summary: required",
		`{"statusCode":404,"message":"No content found"}`:            "No content found",
		`{"errors":[{"title":"Bad","detail":"id must be numeric"}]}`: "Bad id must be numeric",
		``:                     "no details",
		`plain text failure  `: "plain text failure",
	}
	for in, want := range cases {
		if got := ErrorText([]byte(in)); got != want {
			t.Errorf("ErrorText(%q) = %q, want %q", in, got, want)
		}
	}
	if got := ErrorText([]byte(strings.Repeat("x", 600))); len(got) != 503 || !strings.HasSuffix(got, "...") {
		t.Errorf("long body not cut: %d bytes", len(got))
	}
}

func TestQuoting(t *testing.T) {
	if got := QuoteText(`say "hi" \ now`); got != `say \"hi\" \\ now` {
		t.Errorf("QuoteText escapes: %q", got)
	}
	if got := QuoteText("a\n\tb"); got != "a b" {
		t.Errorf("QuoteText whitespace: %q", got)
	}
	if got := QuoteText(strings.Repeat("é", 150)); len(got) > 200 || !strings.HasPrefix(got, "é") || strings.ContainsRune(got, '�') {
		t.Errorf("QuoteText must cut at 200 bytes on a rune boundary: %d bytes", len(got))
	}
	if got := QuoteList([]string{"OPS", "CHG"}); got != `"OPS", "CHG"` {
		t.Errorf("QuoteList: %q", got)
	}
}

func TestClamp(t *testing.T) {
	for _, c := range [][5]int{{0, 7, 1, 90, 7}, {500, 7, 1, 90, 90}, {-3, 7, 1, 90, 1}, {30, 7, 1, 90, 30}} {
		if got := Clamp(c[0], c[1], c[2], c[3]); got != c[4] {
			t.Errorf("Clamp%v = %d", c[:4], got)
		}
	}
}
