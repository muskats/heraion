package scm

import (
	"context"
	"testing"
	"time"

	goscm "github.com/jenkins-x/go-scm/scm"
	"github.com/jenkins-x/go-scm/scm/driver/fake"
)

func TestFindByBranchIgnoresForksAndSeesClosed(t *testing.T) {
	c, data := fake.NewDefault()
	base := goscm.Repository{Namespace: "acme", Name: "app", FullName: "acme/app"}
	fork := goscm.Repository{Namespace: "mallory", Name: "app", FullName: "mallory/app"}
	branch := "heraion/pb/bump"
	closedAt := time.Now().Add(-time.Hour)
	data.PullRequests = map[int]*goscm.PullRequest{
		1: {Number: 1, Link: "u1", Base: goscm.PullRequestBranch{Repo: base}, Head: goscm.PullRequestBranch{Ref: branch, Repo: fork}},
		2: {Number: 2, Link: "u2", Closed: true, Updated: closedAt, Base: goscm.PullRequestBranch{Repo: base}, Head: goscm.PullRequestBranch{Ref: branch, Repo: base}},
		3: {Number: 3, Link: "u3", Base: goscm.PullRequestBranch{Repo: base}, Head: goscm.PullRequestBranch{Ref: "other", Repo: base}},
	}
	prs, err := (&goscmService{c: c}).FindByBranch(context.Background(), "acme/app", branch)
	if err != nil {
		t.Fatal(err)
	}
	if len(prs) != 1 || prs[0].Number != 2 || prs[0].State != PRStateClosed || !prs[0].Closed.Equal(closedAt) {
		t.Fatalf("want only the closed same-repo PR #2, got %+v", prs)
	}
}
