package ghops

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/google/go-github/v84/github"
)

// AutoMerge requests that GitHub merge the PR as soon as branch protection
// allows (CI green, required reviews satisfied). Uses the rebase strategy,
// matching the repo's configured "rebase only" merge setting.
//
// The repo must have auto-merge enabled (we set this earlier on every
// burndown-managed repo via the gh CLI). If auto-merge is not enabled,
// GitHub returns 422 and this returns an error — the caller should fall
// back to a synchronous merge, but for v1 we just surface the error and
// treat the task as failed.
//
// We use the GraphQL `enablePullRequestAutoMerge` mutation rather than
// the REST `PUT /pulls/{n}/merge` because the latter merges immediately
// (synchronously) and doesn't honor branch protection in the same way.
// For unattended overnight ops, "wait for CI then merge" is the right
// semantics.
func (p *Publisher) AutoMerge(ctx context.Context, prNumber int) error {
	pr, _, err := p.GitHub.PullRequests.Get(ctx, p.Owner, p.Name, prNumber)
	if err != nil {
		return fmt.Errorf("ghops: get PR %d: %w", prNumber, err)
	}
	if pr.NodeID == nil {
		return fmt.Errorf("ghops: PR %d missing node_id", prNumber)
	}
	const mutation = `
mutation EnableAutoMerge($id: ID!) {
  enablePullRequestAutoMerge(input: {pullRequestId: $id, mergeMethod: REBASE}) {
    pullRequest { id, autoMergeRequest { enabledAt } }
  }
}`
	return p.runGraphQL(ctx, mutation, map[string]any{"id": *pr.NodeID})
}

// ConvertToDraft toggles the PR back to draft state. Used when an
// AUTO_MERGE_SAFE PR fails CI or otherwise gets demoted — the draft
// state signals "human action needed" to anyone scanning the PR list.
//
// REST has no public endpoint for this transition (only ready→draft is
// reachable via the dashboard or the GraphQL mutation), so we use
// GraphQL.
func (p *Publisher) ConvertToDraft(ctx context.Context, prNumber int) error {
	pr, _, err := p.GitHub.PullRequests.Get(ctx, p.Owner, p.Name, prNumber)
	if err != nil {
		return fmt.Errorf("ghops: get PR %d: %w", prNumber, err)
	}
	if pr.NodeID == nil {
		return fmt.Errorf("ghops: PR %d missing node_id", prNumber)
	}
	const mutation = `
mutation ConvertToDraft($id: ID!) {
  convertPullRequestToDraft(input: {pullRequestId: $id}) {
    pullRequest { id, isDraft }
  }
}`
	return p.runGraphQL(ctx, mutation, map[string]any{"id": *pr.NodeID})
}

// AddLabel applies a single label to the PR. The label is created on
// the repo if it doesn't already exist (via the labels endpoint with
// `add` semantics — GitHub returns 422 only on auth/perm issues, not
// missing labels).
func (p *Publisher) AddLabel(ctx context.Context, prNumber int, label string) error {
	if label == "" {
		return errors.New("ghops: empty label")
	}
	_, _, err := p.GitHub.Issues.AddLabelsToIssue(ctx, p.Owner, p.Name, prNumber, []string{label})
	if err != nil {
		return fmt.Errorf("ghops: add label %q to #%d: %w", label, prNumber, err)
	}
	return nil
}

// CommentOnPR posts a markdown comment on the PR. Used to record why a
// PR was demoted to draft, what gates failed, etc., so the human reviewer
// has everything they need without digging through logs.
func (p *Publisher) CommentOnPR(ctx context.Context, prNumber int, body string) error {
	if body == "" {
		return errors.New("ghops: empty comment body")
	}
	_, _, err := p.GitHub.Issues.CreateComment(ctx, p.Owner, p.Name, prNumber, &github.IssueComment{
		Body: github.Ptr(body),
	})
	if err != nil {
		return fmt.Errorf("ghops: comment on #%d: %w", prNumber, err)
	}
	return nil
}

// runGraphQL POSTs a GraphQL request through the github.Client's transport
// (which carries the App-installation auth). go-github does not expose
// a typed GraphQL helper — the project recommends shurcooL/graphql for
// that — but for two one-off mutations a tiny direct call is much less
// code and one fewer dependency.
//
// We build the request via client.NewRequest (not http.NewRequest) so
// the relative path "graphql" resolves correctly against the client's
// BaseURL, which is how tests inject their httptest server URL.
func (p *Publisher) runGraphQL(ctx context.Context, query string, variables map[string]any) error {
	req, err := p.GitHub.NewRequest("POST", "graphql", map[string]any{
		"query":     query,
		"variables": variables,
	})
	if err != nil {
		return fmt.Errorf("ghops: build graphql request: %w", err)
	}
	var raw struct {
		Errors []struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"errors"`
	}
	resp, err := p.GitHub.Do(ctx, req, &raw)
	if err != nil {
		return fmt.Errorf("ghops: graphql call: %w", err)
	}
	if resp != nil && resp.Body != nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}
	if len(raw.Errors) > 0 {
		return fmt.Errorf("ghops: graphql error: %s (%s)", raw.Errors[0].Message, raw.Errors[0].Type)
	}
	return nil
}
