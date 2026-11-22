//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// gitea serves the part of the Gitea v1 API go-scm uses for PRs and their
// comments. Like Gitea, listing without ?state= returns only open PRs.
type gitea struct {
	*httptest.Server
	mu         sync.Mutex
	prs        map[string][]map[string]any // "owner/repo" -> PRs
	comments   map[string][]string         // "owner/repo#1" -> comment bodies
	listStates []string
	unauthed   int // API calls without Authorization
}

func newGitea(t *testing.T) *gitea {
	g := &gitea{prs: map[string][]map[string]any{}, comments: map[string][]string{}}
	g.Server = httptest.NewServer(http.HandlerFunc(g.handle))
	t.Cleanup(g.Close)
	return g
}

func ts(t time.Time) string { return t.UTC().Format(time.RFC3339) }

func repoJSON(full string) map[string]any {
	owner, name, _ := strings.Cut(full, "/")
	return map[string]any{"id": 1, "name": name, "full_name": full, "owner": map[string]any{"login": owner}}
}

// add stores a PR as the server would, e.g. one opened from a fork.
func (g *gitea) add(repo, title, head, headRepo string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.addLocked(repo, title, "", head, "main", headRepo)
}

func (g *gitea) addLocked(repo, title, body, head, base, headRepo string) map[string]any {
	n := len(g.prs[repo]) + 1
	now := ts(time.Now())
	pr := map[string]any{
		"id": n, "number": n, "title": title, "body": body, "state": "open", "merged": false,
		"html_url": fmt.Sprintf("%s/%s/pulls/%d", g.URL, repo, n),
		"head":     map[string]any{"label": head, "ref": head, "sha": strings.Repeat("0", 40), "repo": repoJSON(headRepo)},
		"base":     map[string]any{"label": base, "ref": base, "sha": strings.Repeat("0", 40), "repo": repoJSON(repo)},
		"user":     map[string]any{"login": "heraion"}, "created_at": now, "updated_at": now,
	}
	g.prs[repo] = append(g.prs[repo], pr)
	return pr
}

// close closes PR n without merging, as a reviewer rejecting it would.
func (g *gitea) close(repo string, n int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	pr := g.prs[repo][n-1]
	pr["state"], pr["updated_at"], pr["closed_at"] = "closed", ts(time.Now()), ts(time.Now())
}

func (g *gitea) commentsOn(repo string, n int) []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.comments[fmt.Sprintf("%s#%d", repo, n)]...)
}

func (g *gitea) list(repo string) []map[string]any {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]map[string]any(nil), g.prs[repo]...)
}

func (g *gitea) handle(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/api/v1/version" {
		send(w, http.StatusOK, map[string]string{"version": "1.22.3"})
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if r.Header.Get("Authorization") == "" {
		g.unauthed++
	}
	p := strings.Split(r.URL.Path, "/") // "", api, v1, repos, owner, repo, pulls
	// PR comments are issue comments: POST /api/v1/repos/owner/repo/issues/1/comments
	if len(p) == 9 && p[3] == "repos" && p[6] == "issues" && p[8] == "comments" && r.Method == http.MethodPost {
		var in struct{ Body string }
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			send(w, http.StatusUnprocessableEntity, map[string]string{"message": err.Error()})
			return
		}
		key := p[4] + "/" + p[5] + "#" + p[7]
		g.comments[key] = append(g.comments[key], in.Body)
		send(w, http.StatusCreated, map[string]any{"id": len(g.comments[key]), "body": in.Body,
			"user": map[string]any{"login": "heraion"}, "created_at": ts(time.Now()), "updated_at": ts(time.Now())})
		return
	}
	if len(p) != 7 || p[3] != "repos" || p[6] != "pulls" {
		send(w, http.StatusNotFound, map[string]string{"message": "not found"})
		return
	}
	repo := p[4] + "/" + p[5]
	switch r.Method {
	case http.MethodGet:
		state := r.URL.Query().Get("state")
		g.listStates = append(g.listStates, state)
		if state == "" {
			state = "open"
		}
		var out []map[string]any
		// Newest first and at most 50 per page, as Gitea's default
		// MAX_RESPONSE_ITEMS does.
		prs := g.prs[repo]
		for i := len(prs) - 1; i >= 0; i-- {
			pr := prs[i]
			if state == "all" || pr["state"] == state {
				out = append(out, pr)
			}
		}
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		if page < 1 {
			page = 1
		}
		if limit < 1 {
			limit = 30
		}
		limit = min(limit, 50)
		lo, hi := min((page-1)*limit, len(out)), min(page*limit, len(out))
		send(w, http.StatusOK, append([]map[string]any{}, out[lo:hi]...))
	case http.MethodPost:
		var in struct{ Title, Body, Head, Base string }
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			send(w, http.StatusUnprocessableEntity, map[string]string{"message": err.Error()})
			return
		}
		send(w, http.StatusCreated, g.addLocked(repo, in.Title, in.Body, in.Head, in.Base, repo))
	}
}
