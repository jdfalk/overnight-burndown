// file: internal/ghops/issue_labels.go
// version: 1.0.0
// guid: 11ab1e15-0001-4000-a000-is5uel4bel55
//
// HubLabeler manages ao: lifecycle labels on burndown-tasks hub issues.
// It is separate from Publisher (which is scoped to the target repo) because
// issue labels live in the hub repo (e.g. falkcorp/burndown-tasks), not in
// the implementation target (e.g. falkcorp/audiobook-organizer).

package ghops

import (
	"context"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/google/go-github/v84/github"
)

// Label constants for the ao: lifecycle namespace.
const (
	LabelRunning      = "ao:running"
	LabelCodeComplete = "ao:code-complete"
	LabelFailed       = "ao:failed"
)

// HubLabeler applies lifecycle labels to issues in the task hub repo.
type HubLabeler struct {
	GitHub *github.Client
	Owner  string
	Name   string
}

// NewHubLabeler constructs a HubLabeler for the given hub repo (e.g. "falkcorp/burndown-tasks").
func NewHubLabeler(gh *github.Client, hubRepo string) (*HubLabeler, error) {
	owner, name, ok := strings.Cut(hubRepo, "/")
	if !ok {
		return nil, fmt.Errorf("ghops: invalid hub repo %q (expected owner/name)", hubRepo)
	}
	return &HubLabeler{GitHub: gh, Owner: owner, Name: name}, nil
}

// SetRunning adds ao:running and removes ao:code-complete / ao:failed.
func (h *HubLabeler) SetRunning(ctx context.Context, issueNum int) error {
	if err := h.addLabel(ctx, issueNum, LabelRunning); err != nil {
		return err
	}
	h.removeLabel(ctx, issueNum, LabelCodeComplete) //nolint:errcheck — best-effort
	h.removeLabel(ctx, issueNum, LabelFailed)       //nolint:errcheck
	return nil
}

// SetCodeComplete removes ao:running, adds ao:code-complete.
func (h *HubLabeler) SetCodeComplete(ctx context.Context, issueNum int) error {
	h.removeLabel(ctx, issueNum, LabelRunning) //nolint:errcheck — best-effort
	return h.addLabel(ctx, issueNum, LabelCodeComplete)
}

// SetFailed removes ao:running, adds ao:failed.
func (h *HubLabeler) SetFailed(ctx context.Context, issueNum int) error {
	h.removeLabel(ctx, issueNum, LabelRunning) //nolint:errcheck — best-effort
	return h.addLabel(ctx, issueNum, LabelFailed)
}

// HasLabel reports whether the issue currently carries the given label.
func (h *HubLabeler) HasLabel(ctx context.Context, issueNum int, label string) (bool, error) {
	labels, _, err := h.GitHub.Issues.ListLabelsByIssue(ctx, h.Owner, h.Name, issueNum, nil)
	if err != nil {
		return false, fmt.Errorf("ghops: list labels #%d: %w", issueNum, err)
	}
	return slices.ContainsFunc(labels, func(l *github.Label) bool {
		return l.GetName() == label
	}), nil
}

// EnsureLabelsExist creates any of the ao: labels that don't yet exist in the
// repo. Call once at the start of a run so AddLabelsToIssue never 422s on an
// unknown label name.
func (h *HubLabeler) EnsureLabelsExist(ctx context.Context) error {
	type labelDef struct {
		name  string
		color string
		desc  string
	}
	defs := []labelDef{
		{LabelRunning, "0075ca", "Agent is currently executing this task"},
		{LabelCodeComplete, "0e8a16", "Agent finished; PR open and awaiting review"},
		{LabelFailed, "d93f0b", "Agent failed; needs re-triage or decompose"},
	}
	existing, _, err := h.GitHub.Issues.ListLabels(ctx, h.Owner, h.Name, nil)
	if err != nil {
		return fmt.Errorf("ghops: list repo labels: %w", err)
	}
	have := make(map[string]bool, len(existing))
	for _, l := range existing {
		have[l.GetName()] = true
	}
	for _, d := range defs {
		if have[d.name] {
			continue
		}
		_, _, err := h.GitHub.Issues.CreateLabel(ctx, h.Owner, h.Name, &github.Label{
			Name:        github.Ptr(d.name),
			Color:       github.Ptr(d.color),
			Description: github.Ptr(d.desc),
		})
		if err != nil {
			return fmt.Errorf("ghops: create label %q: %w", d.name, err)
		}
	}
	return nil
}

// IssueNumberFromURL parses a GitHub issue URL and returns the issue number.
// Returns 0, nil when the URL is not a GitHub issue URL.
func IssueNumberFromURL(rawURL string) (int, error) {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host != "github.com" {
		return 0, nil
	}
	// path: /owner/repo/issues/42
	parts := strings.Split(strings.TrimPrefix(u.Path, "/"), "/")
	if len(parts) < 4 || parts[2] != "issues" {
		return 0, nil
	}
	n, err := strconv.Atoi(parts[3])
	if err != nil {
		return 0, nil
	}
	return n, nil
}

func (h *HubLabeler) addLabel(ctx context.Context, issueNum int, label string) error {
	_, _, err := h.GitHub.Issues.AddLabelsToIssue(ctx, h.Owner, h.Name, issueNum, []string{label})
	if err != nil {
		return fmt.Errorf("ghops: add label %q to #%d: %w", label, issueNum, err)
	}
	return nil
}

func (h *HubLabeler) removeLabel(ctx context.Context, issueNum int, label string) error {
	_, err := h.GitHub.Issues.RemoveLabelForIssue(ctx, h.Owner, h.Name, issueNum, label)
	if err != nil {
		// 404 = label not present; not an error.
		if strings.Contains(err.Error(), "404") {
			return nil
		}
		return fmt.Errorf("ghops: remove label %q from #%d: %w", label, issueNum, err)
	}
	return nil
}
