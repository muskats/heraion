package scm

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	goscm "github.com/jenkins-x/go-scm/scm"
	"github.com/jenkins-x/go-scm/scm/factory"

	"github.com/muskats/heraion/internal/config"
	"github.com/muskats/heraion/internal/secret"
)

// BranchPrefix starts every branch HeraIon pushes, e.g.
// "heraion/trivy-remediate/bump-alpine-3-20".
const BranchPrefix = "heraion/"

// PR is a pull (or merge) request on the Git server.
type PR struct {
	Number int
	URL    string
}

// PRInput describes a PR to open.
type PRInput struct {
	Title, Body, Head, Base string
}

// PRService is the Git hosting API HeraIon uses. It has no merge method.
type PRService interface {
	// FindOpen returns the open PR whose head is branch, or nil.
	FindOpen(ctx context.Context, repo, branch string) (*PR, error)
	Create(ctx context.Context, repo string, in PRInput) (*PR, error)
}

// NewPRService uses go-scm, so self-hosted servers work, e.g. provider
// "gitea" with endpoint https://git.example.com.
func NewPRService(g config.Git) (PRService, error) {
	// The token is resolved per request, so a rotating token file works.
	c, err := factory.NewClientWithTokenSource(g.Provider, g.Endpoint, tokenSource{g.Token})
	if err != nil {
		return nil, fmt.Errorf("git provider %s: %w", g.Provider, err)
	}
	return &goscmService{c: c}, nil
}

type tokenSource struct{ src *secret.Source }

func (t tokenSource) Token(context.Context) (*goscm.Token, error) {
	v, err := t.src.Value()
	if err != nil {
		return nil, err
	}
	return &goscm.Token{Token: v}, nil
}

type goscmService struct{ c *goscm.Client }

func (s *goscmService) FindOpen(ctx context.Context, repo, branch string) (*PR, error) {
	const size, maxPages = 100, 20
	for page := 1; page <= maxPages; page++ {
		prs, _, err := s.c.PullRequests.List(ctx, repo, &goscm.PullRequestListOptions{Page: page, Size: size, Open: true})
		if err != nil {
			return nil, fmt.Errorf("list open PRs on %s: %w", repo, err)
		}
		for _, pr := range prs {
			if pr.Closed || pr.Merged {
				continue
			}
			if pr.Head.Ref == branch || pr.Source == branch {
				return &PR{Number: pr.Number, URL: pr.Link}, nil
			}
		}
		if len(prs) < size {
			break
		}
	}
	return nil, nil
}

func (s *goscmService) Create(ctx context.Context, repo string, in PRInput) (*PR, error) {
	pr, _, err := s.c.PullRequests.Create(ctx, repo, &goscm.PullRequestInput{
		Title: in.Title, Body: in.Body, Head: in.Head, Base: in.Base,
	})
	if err != nil {
		return nil, fmt.Errorf("create PR on %s: %w", repo, err)
	}
	return &PR{Number: pr.Number, URL: pr.Link}, nil
}

var nonSlug = regexp.MustCompile(`[^a-z0-9]+`)

func slug(s string, max int) string {
	s = strings.Trim(nonSlug.ReplaceAllString(strings.ToLower(s), "-"), "-")
	if len(s) > max {
		s = strings.TrimRight(s[:max], "-")
	}
	return s
}

// BranchName is stable per change, so reruns find the existing PR:
//
//	BranchName("trivy-remediate", "Bump alpine to 3.20")
//	  = "heraion/trivy-remediate/bump-alpine-to-3-20"
func BranchName(playbook, key string) string {
	k := slug(strings.TrimPrefix(strings.TrimPrefix(key, BranchPrefix), playbook+"/"), 60)
	if k == "" {
		k = "change"
	}
	return BranchPrefix + playbook + "/" + k
}
