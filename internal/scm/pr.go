package scm

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

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
	State  string    // PRStateOpen, PRStateClosed (not merged) or PRStateMerged
	Closed time.Time // last update of a closed or merged PR, i.e. about when it was closed
}

// PR states.
const (
	PRStateOpen   = "open"
	PRStateClosed = "closed"
	PRStateMerged = "merged"
)

// PRInput describes a PR to open.
type PRInput struct {
	Title, Body, Head, Base string
}

// PRService is the Git hosting API HeraIon uses. It has no merge method.
type PRService interface {
	// FindByBranch returns the PRs whose head is branch in repo itself (a
	// fork's branch of the same name does not count): open ones first.
	FindByBranch(ctx context.Context, repo, branch string) ([]PR, error)
	Create(ctx context.Context, repo string, in PRInput) (*PR, error)
}

// NewPRService uses go-scm, so self-hosted servers work, e.g. provider
// "gitea" with endpoint https://git.example.com.
func NewPRService(g config.Git) (PRService, error) {
	c, err := newGoSCMClient(g)
	if err != nil {
		return nil, err
	}
	return &goscmService{c: c}, nil
}

// newGoSCMClient resolves the token per request, so a rotating token file works.
func newGoSCMClient(g config.Git) (*goscm.Client, error) {
	c, err := factory.NewClientWithTokenSource(g.Provider, g.Endpoint, tokenSource{g.Token})
	if err != nil {
		return nil, fmt.Errorf("git provider %s: %w", g.Provider, err)
	}
	return c, nil
}

// NewCloneURLResolver asks the Git server for a repo's HTTPS clone URL. It
// only reads, so it is used in dry-run too.
func NewCloneURLResolver(g config.Git) (CloneURLFunc, error) {
	c, err := newGoSCMClient(g)
	if err != nil {
		return nil, err
	}
	return func(ctx context.Context, repo string) (string, error) {
		r, _, err := c.Repositories.Find(ctx, repo)
		if err != nil {
			return "", err
		}
		if r.Clone == "" {
			return "", fmt.Errorf("the %s API returned no HTTP clone URL for %s; set git.cloneBaseURL", g.Provider, repo)
		}
		return r.Clone, nil
	}, nil
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

func (s *goscmService) FindByBranch(ctx context.Context, repo, branch string) ([]PR, error) {
	// Two passes: some drivers (e.g. Gitea) ignore Open and Closed together
	// and list only open PRs.
	var out []PR
	for _, opts := range []goscm.PullRequestListOptions{{Open: true}, {Closed: true}} {
		found, err := s.list(ctx, repo, branch, opts)
		if err != nil {
			return nil, err
		}
		out = append(out, found...)
	}
	return out, nil
}

func (s *goscmService) list(ctx context.Context, repo, branch string, opts goscm.PullRequestListOptions) ([]PR, error) {
	const size, maxPages = 100, 20
	var out []PR
	for page := 1; page <= maxPages; page++ {
		opts.Page, opts.Size = page, size
		prs, _, err := s.c.PullRequests.List(ctx, repo, &opts)
		if err != nil {
			return nil, fmt.Errorf("list PRs on %s: %w", repo, err)
		}
		for _, pr := range prs {
			if pr == nil {
				continue // go-scm's Gitea driver returns nil for a PR it cannot convert
			}
			if pr.Head.Ref != branch && pr.Source != branch {
				continue
			}
			if head := pr.Head.Repo.FullName; head != "" && !strings.EqualFold(head, repo) {
				continue // a fork's branch with the same name
			}
			out = append(out, toPR(pr))
		}
		// Stop on an empty page, not a short one: servers cap the page size,
		// e.g. Gitea returns at most 50 even when asked for 100.
		if len(prs) == 0 {
			break
		}
	}
	return out, nil
}

func toPR(pr *goscm.PullRequest) PR {
	p := PR{Number: pr.Number, URL: pr.Link, State: PRStateOpen}
	switch {
	case pr.Merged:
		p.State, p.Closed = PRStateMerged, pr.Updated
	case pr.Closed:
		p.State, p.Closed = PRStateClosed, pr.Updated
	}
	return p
}

func (s *goscmService) Create(ctx context.Context, repo string, in PRInput) (*PR, error) {
	pr, _, err := s.c.PullRequests.Create(ctx, repo, &goscm.PullRequestInput{
		Title: in.Title, Body: in.Body, Head: in.Head, Base: in.Base,
	})
	if err != nil {
		return nil, fmt.Errorf("create PR on %s: %w", repo, err)
	}
	return &PR{Number: pr.Number, URL: pr.Link, State: PRStateOpen}, nil
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
