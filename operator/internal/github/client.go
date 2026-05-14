// Package github wraps the GitHub REST API behind a narrow interface used by
// the operator's reconcilers. The real implementation uses go-github with a
// GitHub App installation transport.
package github

import (
	"context"
	"fmt"
	"net/http"
	"sort"

	"github.com/bradleyfalzon/ghinstallation/v2"
	gogithub "github.com/google/go-github/v86/github"
)

// Client is the operator's view of GitHub. Reconcilers depend on this interface
// (not on go-github directly).
type Client interface {
	// ListIssues returns OPEN issues for owner/repo. Pull requests are filtered
	// out (GitHub's REST endpoint conflates them).
	ListIssues(ctx context.Context, owner, repo string) ([]Issue, error)
	// GetPullRequest returns pull request state for owner/repo + number.
	GetPullRequest(ctx context.Context, owner, repo string, number int32) (PullRequest, error)
}

// NewClient builds a real GitHub client backed by go-github + ghinstallation.
func NewClient(creds AppCreds) (Client, error) {
	tr, err := ghinstallation.New(http.DefaultTransport, creds.AppID, creds.InstallationID, creds.PrivateKeyPEM)
	if err != nil {
		return nil, fmt.Errorf("github app transport: %w", err)
	}
	return &client{gh: gogithub.NewClient(&http.Client{Transport: tr})}, nil
}

type client struct {
	gh *gogithub.Client
}

func (c *client) ListIssues(ctx context.Context, owner, repo string) ([]Issue, error) {
	var out []Issue
	opt := &gogithub.IssueListByRepoOptions{
		State:       "open",
		ListOptions: gogithub.ListOptions{PerPage: 100},
	}
	for {
		issues, resp, err := c.gh.Issues.ListByRepo(ctx, owner, repo, opt)
		if err != nil {
			return nil, fmt.Errorf("list issues %s/%s: %w", owner, repo, err)
		}
		for _, i := range issues {
			if i.IsPullRequest() {
				continue
			}
			out = append(out, fromGoGitHub(i))
		}
		if resp.NextPage == 0 {
			break
		}
		opt.ListOptions.Page = resp.NextPage
	}
	return out, nil
}

func (c *client) GetPullRequest(ctx context.Context, owner, repo string, number int32) (PullRequest, error) {
	pr, _, err := c.gh.PullRequests.Get(ctx, owner, repo, int(number))
	if err != nil {
		return PullRequest{}, fmt.Errorf("get pull request %s/%s#%d: %w", owner, repo, number, err)
	}

	return PullRequest{
		Number: int32(pr.GetNumber()),
		State:  pr.GetState(),
		Merged: pr.GetMerged(),
	}, nil
}

func fromGoGitHub(i *gogithub.Issue) Issue {
	var labels []string
	for _, l := range i.Labels {
		if name := l.GetName(); name != "" {
			labels = append(labels, name)
		}
	}
	// Sort labels so reconcile's reflect.DeepEqual on IssueSpec is stable across
	// polls. GitHub's REST API does not guarantee label order, and an unstable
	// order would trigger spurious Issue CR updates every reconcile.
	sort.Strings(labels)

	state := "Open"
	if i.GetState() == "closed" {
		state = "Closed"
	}
	return Issue{
		Number: int32(i.GetNumber()),
		Title:  i.GetTitle(),
		Body:   i.GetBody(),
		Labels: labels,
		State:  state,
	}
}
