// file: internal/runner/reconcile.go
// version: 1.0.0
// guid: c4d5e6f7-a8b9-0c1d-2e3f-4a5b6c7d8e9f

package runner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"

	"github.com/google/go-github/v84/github"

	"github.com/jdfalk/overnight-burndown/internal/state"
)

// ReconcileFromGitHub queries the target repo for all open PRs bearing
// the "automation" label and upserts a state row for each one. This
// patches holes left by crash-before-state-save and artifact-upload
// failures so that filterFreshTasks never re-dispatches a task whose PR
// is still open on GitHub.
//
// Each reconciled row uses branchHash(branch) as its key (distinct from
// the source-based HashTask key normal dispatch uses). filterFreshTasks
// deduplicates by source hash, so reconciled rows are not matched by the
// standard path — but the InFlight() index and the 7-day TTL on StatusDraft
// together ensure the rows expire cleanly once a PR is merged or closed.
//
// A row is skipped when local state already has an entry under that key;
// existing rows are authoritative (they carry more-specific status like
// StatusShipped).
//
// Best-effort: the caller logs and continues on error.
func ReconcileFromGitHub(
	ctx context.Context,
	gh *github.Client,
	s *state.State,
	owner, repo string,
) error {
	opts := &github.PullRequestListOptions{
		State:       "open",
		ListOptions: github.ListOptions{PerPage: 100},
	}
	patched := 0
	for page := 1; page <= 10; page++ { // max-pages guard: 1,000 PRs is already a bug signal
		opts.Page = page
		prs, resp, err := gh.PullRequests.List(ctx, owner, repo, opts)
		if err != nil {
			return fmt.Errorf("reconcile: list PRs (page %d): %w", page, err)
		}
		for _, pr := range prs {
			if !prHasBurndownLabel(pr) {
				continue
			}
			branch := pr.GetHead().GetRef()
			if branch == "" {
				continue
			}
			hash := BranchHash(branch)
			if _, exists := s.Get(hash); exists {
				continue // local state is authoritative
			}
			s.Upsert(&state.TaskState{
				Hash:     hash,
				Branch:   branch,
				PRNumber: pr.GetNumber(),
				PRURL:    pr.GetHTMLURL(),
				Status:   state.StatusDraft,
				Source: state.Source{
					Type: state.SourceUnknown,
					URL:  pr.GetHTMLURL(),
				},
			})
			fmt.Fprintf(os.Stderr,
				"runner: reconciled PR #%d branch %q from GitHub\n",
				pr.GetNumber(), branch)
			patched++
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
	}
	if patched > 0 {
		fmt.Fprintf(os.Stderr, "runner: reconcile: patched %d missing state rows\n", patched)
	}
	return nil
}

// mergeApprovedPRs scans open PRs carrying the "merge-approved" label and
// calls AutoMerge on each. This lets a human reviewer (or a review bot like
// Claude) approve a PR for landing by adding the label, without waiting for
// the next full dispatch cycle to produce a new PR.
//
// On successful AutoMerge the state row is updated to StatusShipped (when
// a matching source-hash row exists) and the branch is recorded in merged.
// All errors are best-effort: a single PR failure does not abort the scan.
func (r *Runner) mergeApprovedPRs(
	ctx context.Context,
	pub RepoPublisher,
	gh *github.Client,
	owner, repo string,
	merged map[string]bool,
) {
	opts := &github.PullRequestListOptions{
		State:       "open",
		ListOptions: github.ListOptions{PerPage: 100},
	}
	for page := 1; page <= 10; page++ {
		opts.Page = page
		prs, resp, err := gh.PullRequests.List(ctx, owner, repo, opts)
		if err != nil {
			fmt.Fprintf(os.Stderr, "runner: merge-approved: list PRs (page %d): %v\n", page, err)
			return
		}
		for _, pr := range prs {
			if !hasLabel(pr, "merge-approved") {
				continue
			}
			prNum := pr.GetNumber()
			branch := pr.GetHead().GetRef()
			if err := pub.AutoMerge(ctx, prNum); err != nil {
				fmt.Fprintf(os.Stderr, "runner: merge-approved: AutoMerge #%d: %v\n", prNum, err)
				continue
			}
			merged[branch] = true
			// Mark state row shipped if we have one under the branch hash.
			if ts, ok := r.State.Get(BranchHash(branch)); ok {
				ts2 := *ts
				ts2.Status = state.StatusShipped
				r.State.Upsert(&ts2)
			}
			fmt.Fprintf(os.Stderr, "runner: merge-approved: merged PR #%d (%s)\n", prNum, branch)
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
	}
}

// BranchHash produces a stable state-row key from a branch name alone.
// Exported so tests and the reconcile function share the same derivation.
func BranchHash(branch string) string {
	sum := sha256.Sum256([]byte("branch\x00" + branch))
	return hex.EncodeToString(sum[:])
}

// prHasBurndownLabel returns true when the PR carries the "automation"
// label that applyBurndownLabels applies to every burndown-managed PR.
func prHasBurndownLabel(pr *github.PullRequest) bool {
	return hasLabel(pr, "automation")
}

// hasLabel reports whether a PR carries a given label.
func hasLabel(pr *github.PullRequest, label string) bool {
	for _, l := range pr.Labels {
		if l.GetName() == label {
			return true
		}
	}
	return false
}
