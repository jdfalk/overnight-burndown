package sources

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/go-github/v84/github"

	"github.com/jdfalk/overnight-burndown/internal/state"
)

// IssueCollector pulls open issues with the configured Label from a single
// repo via the GitHub REST API. The driver wires this collector with the
// installation-token-aware client built by the auth package.
//
// Auto-OK is determined by Label presence — every issue this collector
// returns already has the label, so HasAutoOK is always true.
type IssueCollector struct {
	Client *github.Client
	Label  string // defaults to "auto-ok" if empty
}

// NewIssueCollector returns an IssueCollector with the conventional `auto-ok`
// label. Pass the App-installation client from internal/auth.
func NewIssueCollector(client *github.Client) *IssueCollector {
	return &IssueCollector{Client: client, Label: "auto-ok"}
}

// Collect implements Collector. The `repo` argument is "owner/name"; this
// collector splits it into the two components GitHub's REST API expects.
func (c *IssueCollector) Collect(ctx context.Context, repo string, _ string) ([]Task, error) {
	if c.Client == nil {
		return nil, errors.New("sources/issues: nil github client")
	}
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || owner == "" || name == "" {
		return nil, fmt.Errorf("sources/issues: repo %q must be owner/name", repo)
	}
	label := c.Label
	if label == "" {
		label = "auto-ok"
	}

	opts := &github.IssueListByRepoOptions{
		State:  "open",
		Labels: []string{label},
		ListOptions: github.ListOptions{
			PerPage: 100,
		},
	}

	var out []Task
	for {
		issues, resp, err := c.Client.Issues.ListByRepo(ctx, owner, name, opts)
		if err != nil {
			return nil, fmt.Errorf("sources/issues: list %s/%s: %w", owner, name, err)
		}
		for _, iss := range issues {
			// PRs are returned by the issues endpoint too; skip them.
			if iss.PullRequestLinks != nil {
				continue
			}
			body := ""
			if iss.Body != nil {
				body = *iss.Body
			}
			title := ""
			if iss.Title != nil {
				title = *iss.Title
			}
			url := ""
			if iss.HTMLURL != nil {
				url = *iss.HTMLURL
			}
			content := title + "\n\n" + body
			out = append(out, Task{
				Source: state.Source{
					Type:        state.SourceIssue,
					Repo:        repo,
					URL:         url,
					ContentHash: state.HashContent(content),
					Title:       title,
				},
				Body:      content,
				HasAutoOK: true, // label-gated by the query
			})
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		opts.ListOptions.Page = resp.NextPage
	}
	return out, nil
}
