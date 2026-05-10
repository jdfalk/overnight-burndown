// file: internal/triagepoll/github.go
// version: 1.0.0
// guid: 4e5f6a7b-8c9d-0e1f-2a3b-4c5d6e7f8a9b
//
// GitHub interactions for the triage-poll state machine: querying untriaged
// issues, creating/closing the tracking issue, and writing triage results back.

package triagepoll

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/go-github/v84/github"
)

const (
	// LabelTriaged is applied to hub issues once triage is written back.
	LabelTriaged = "triaged"
	// LabelBatchPending marks the tracking issue while a batch is in-flight.
	LabelBatchPending = "burndown:batch-pending"

	trackingTitlePrefix = "burndown-triage-batch:"
)

// HubIssue is a minimal representation of a hub issue for the poll workflow.
type HubIssue struct {
	Number int
	Title  string
	Body   string
	URL    string
}

// FindUntriagedIssues returns open issues in the hub repo that have
// status:ready + the routing label but do NOT have the triaged label.
func FindUntriagedIssues(ctx context.Context, gh *github.Client, hubOwner, hubName, repoName, labelPrefix string) ([]HubIssue, error) {
	routingLabel := labelPrefix + repoName
	opts := &github.IssueListByRepoOptions{
		State:  "open",
		Labels: []string{routingLabel, "status:ready"},
		ListOptions: github.ListOptions{PerPage: 100},
	}
	var out []HubIssue
	for {
		issues, resp, err := gh.Issues.ListByRepo(ctx, hubOwner, hubName, opts)
		if err != nil {
			return nil, fmt.Errorf("triagepoll: list issues: %w", err)
		}
		for _, iss := range issues {
			if iss.PullRequestLinks != nil {
				continue
			}
			if hasLabel(iss, LabelTriaged) || hasLabel(iss, LabelBatchPending) {
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
			out = append(out, HubIssue{
				Number: int(iss.GetNumber()),
				Title:  title,
				Body:   body,
				URL:    url,
			})
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		opts.ListOptions.Page = resp.NextPage
	}
	return out, nil
}

// FindTrackingIssue looks for an open tracking issue in the hub repo
// for the given target repo. Returns nil if none is found.
func FindTrackingIssue(ctx context.Context, gh *github.Client, hubOwner, hubName, repoName string) (*github.Issue, error) {
	title := trackingTitle(repoName)
	opts := &github.IssueListByRepoOptions{
		State:  "open",
		Labels: []string{LabelBatchPending},
		ListOptions: github.ListOptions{PerPage: 20},
	}
	for {
		issues, resp, err := gh.Issues.ListByRepo(ctx, hubOwner, hubName, opts)
		if err != nil {
			return nil, fmt.Errorf("triagepoll: list tracking issues: %w", err)
		}
		for _, iss := range issues {
			if iss.Title != nil && *iss.Title == title {
				return iss, nil
			}
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		opts.ListOptions.Page = resp.NextPage
	}
	return nil, nil
}

// CreateTrackingIssue opens a new tracking issue in the hub repo and stores
// the batch_id in the body. Returns the created issue number.
func CreateTrackingIssue(ctx context.Context, gh *github.Client, hubOwner, hubName, repoName, batchID string, issueCount int) (int, error) {
	body := fmt.Sprintf("<!-- batch_id: %s -->\n\nBatch triage submitted for **%s** (%d issues).\n\nBatch ID: `%s`\n\nThis issue will be closed automatically when the batch completes.",
		batchID, repoName, issueCount, batchID)
	req := &github.IssueRequest{
		Title:  github.String(trackingTitle(repoName)),
		Body:   github.String(body),
		Labels: &[]string{LabelBatchPending},
	}
	iss, _, err := gh.Issues.Create(ctx, hubOwner, hubName, req)
	if err != nil {
		return 0, fmt.Errorf("triagepoll: create tracking issue: %w", err)
	}
	return int(iss.GetNumber()), nil
}

// CloseTrackingIssue closes the tracking issue once the batch is resolved.
func CloseTrackingIssue(ctx context.Context, gh *github.Client, hubOwner, hubName string, issueNumber int, comment string) error {
	if comment != "" {
		_, _, err := gh.Issues.CreateComment(ctx, hubOwner, hubName, issueNumber, &github.IssueComment{
			Body: github.String(comment),
		})
		if err != nil {
			return fmt.Errorf("triagepoll: comment tracking issue: %w", err)
		}
	}
	state := "closed"
	_, _, err := gh.Issues.Edit(ctx, hubOwner, hubName, issueNumber, &github.IssueRequest{State: &state})
	if err != nil {
		return fmt.Errorf("triagepoll: close tracking issue: %w", err)
	}
	return nil
}

// WriteTriageResult posts the triage decision as a comment on the issue
// and adds the triaged label.
func WriteTriageResult(ctx context.Context, gh *github.Client, hubOwner, hubName string, issueNumber int, d Decision) error {
	body := formatTriageComment(d)
	if _, _, err := gh.Issues.CreateComment(ctx, hubOwner, hubName, issueNumber, &github.IssueComment{
		Body: github.String(body),
	}); err != nil {
		return fmt.Errorf("triagepoll: comment #%d: %w", issueNumber, err)
	}
	if _, _, err := gh.Issues.AddLabelsToIssue(ctx, hubOwner, hubName, issueNumber, []string{LabelTriaged}); err != nil {
		return fmt.Errorf("triagepoll: label #%d: %w", issueNumber, err)
	}
	return nil
}

// EnsureLabelsExist creates the required labels in the hub repo if missing.
func EnsureLabelsExist(ctx context.Context, gh *github.Client, hubOwner, hubName string) error {
	needed := []struct {
		name  string
		color string
		desc  string
	}{
		{LabelTriaged, "0e8a16", "Triage decision written; ready for dispatch"},
		{LabelBatchPending, "e4e669", "OpenAI batch triage in flight"},
	}
	for _, l := range needed {
		_, resp, err := gh.Issues.GetLabel(ctx, hubOwner, hubName, l.name)
		if err == nil {
			continue // already exists
		}
		if resp != nil && resp.StatusCode == 404 {
			_, _, err = gh.Issues.CreateLabel(ctx, hubOwner, hubName, &github.Label{
				Name:        github.String(l.name),
				Color:       github.String(l.color),
				Description: github.String(l.desc),
			})
			if err != nil {
				return fmt.Errorf("triagepoll: create label %q: %w", l.name, err)
			}
			continue
		}
		return fmt.Errorf("triagepoll: get label %q: %w", l.name, err)
	}
	return nil
}

// ExtractBatchID parses the batch_id from a tracking issue body.
func ExtractBatchID(iss *github.Issue) string {
	if iss.Body == nil {
		return ""
	}
	body := *iss.Body
	prefix := "<!-- batch_id: "
	start := strings.Index(body, prefix)
	if start < 0 {
		return ""
	}
	start += len(prefix)
	end := strings.Index(body[start:], " -->")
	if end < 0 {
		return ""
	}
	return strings.TrimSpace(body[start : start+end])
}

func trackingTitle(repoName string) string {
	return trackingTitlePrefix + repoName
}

func hasLabel(iss *github.Issue, label string) bool {
	for _, l := range iss.Labels {
		if l.Name != nil && *l.Name == label {
			return true
		}
	}
	return false
}

func formatTriageComment(d Decision) string {
	classEmoji := map[string]string{
		"AUTO_MERGE_SAFE": "✅",
		"NEEDS_REVIEW":    "👀",
		"BLOCKED":         "🚫",
	}
	emoji := classEmoji[d.Classification]
	if emoji == "" {
		emoji = "•"
	}

	branch := d.SuggestedBranch
	if branch == "" {
		branch = "_(none — blocked)_"
	}

	return fmt.Sprintf(`<!-- burndown-triage: %s -->
<details><summary>%s Burndown triage decision</summary>

| Field | Value |
|---|---|
| **Classification** | %s |
| **Complexity** | %d / 5 |
| **Suggested branch** | %s |
| **Reason** | %s |

</details>`,
		d.Classification,
		emoji,
		d.Classification,
		d.EstComplexity,
		"`"+branch+"`",
		d.Reason,
	)
}
