package sources

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/go-github/v84/github"

	"github.com/jdfalk/overnight-burndown/internal/state"
)

// IssueCollector pulls open issues from a GitHub repository via the REST API.
//
// Default mode: reads issues labelled `auto-ok` from the target repo itself.
//
// Hub mode (HubRepo set): reads from a central hub repository instead,
// filtering by a routing label of the form "{HubLabelPrefix}{repo-name}"
// (e.g. "repo:audiobook-organizer"). This lets a single private hub like
// jdfalk/burndown-tasks serve as the task source for multiple target repos.
//
// HasAutoOK is always true — every issue returned already passed the label
// gate, so the triage agent can treat it as pre-approved.
type IssueCollector struct {
	Client *github.Client
	Label  string // defaults to "auto-ok" if empty; ignored in hub mode

	// Hub mode: when HubRepo is non-empty, issues are read from HubRepo
	// and filtered by HubLabelPrefix+<target-repo-name>.
	HubRepo        string // "owner/name" of the hub, e.g. "jdfalk/burndown-tasks"
	HubLabelPrefix string // routing label prefix, e.g. "repo:"
}

// NewIssueCollector returns an IssueCollector with the conventional `auto-ok`
// label. Pass the App-installation client from internal/auth.
func NewIssueCollector(client *github.Client) *IssueCollector {
	return &IssueCollector{Client: client, Label: "auto-ok"}
}

// NewHubIssueCollector returns an IssueCollector that reads from a central
// hub repo and routes by label. hubRepo is "owner/name"; labelPrefix is
// prepended to the target repo name for the routing label.
func NewHubIssueCollector(client *github.Client, hubRepo, labelPrefix string) *IssueCollector {
	return &IssueCollector{
		Client:         client,
		HubRepo:        hubRepo,
		HubLabelPrefix: labelPrefix,
	}
}

// Collect implements Collector. The `repo` argument is "owner/name".
// In hub mode the routing label is derived from the leaf repo name.
func (c *IssueCollector) Collect(ctx context.Context, repo string, _ string) ([]Task, error) {
	if c.Client == nil {
		return nil, errors.New("sources/issues: nil github client")
	}

	if c.HubRepo != "" {
		return c.collectFromHub(ctx, repo)
	}
	return c.collectFromRepo(ctx, repo)
}

// collectFromRepo reads auto-ok issues from the target repo itself.
func (c *IssueCollector) collectFromRepo(ctx context.Context, repo string) ([]Task, error) {
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || owner == "" || name == "" {
		return nil, fmt.Errorf("sources/issues: repo %q must be owner/name", repo)
	}
	label := c.Label
	if label == "" {
		label = "auto-ok"
	}
	return c.listIssues(ctx, owner, name, []string{label}, repo)
}

// collectFromHub reads issues from the hub repo filtered by the routing label.
func (c *IssueCollector) collectFromHub(ctx context.Context, targetRepo string) ([]Task, error) {
	hubOwner, hubName, ok := strings.Cut(c.HubRepo, "/")
	if !ok || hubOwner == "" || hubName == "" {
		return nil, fmt.Errorf("sources/issues: hub_repo %q must be owner/name", c.HubRepo)
	}
	_, repoName, ok := strings.Cut(targetRepo, "/")
	if !ok || repoName == "" {
		return nil, fmt.Errorf("sources/issues: target repo %q must be owner/name", targetRepo)
	}
	prefix := c.HubLabelPrefix
	if prefix == "" {
		prefix = "repo:"
	}
	routingLabel := prefix + repoName
	return c.listIssues(ctx, hubOwner, hubName, []string{routingLabel, "status:ready"}, targetRepo)
}

// listIssues paginates through open issues in owner/name that match all labels,
// attributing them to targetRepo in the returned Task sources.
func (c *IssueCollector) listIssues(ctx context.Context, owner, name string, labels []string, targetRepo string) ([]Task, error) {
	opts := &github.IssueListByRepoOptions{
		State:  "open",
		Labels: labels,
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
					Repo:        targetRepo,
					URL:         url,
					ContentHash: state.HashContent(content),
					Title:       title,
				},
				Body:      content,
				HasAutoOK: true,
			})
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		opts.ListOptions.Page = resp.NextPage
	}
	return out, nil
}
