package ghops

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/go-github/v84/github"
)

// CIStatus is the aggregate of every check / status run on a PR's head commit.
type CIStatus string

const (
	// CIPending means at least one check is still queued or in progress.
	CIPending CIStatus = "pending"
	// CISuccess means every check has concluded with success or neutral.
	CISuccess CIStatus = "success"
	// CIFailure means at least one check concluded with failure / cancelled / timed_out.
	CIFailure CIStatus = "failure"
)

// WatchOptions controls the polling behavior of WatchCI.
type WatchOptions struct {
	// Timeout caps the total wait. Default 30 minutes per the locked design (D1).
	Timeout time.Duration
	// PollInterval is how often we re-fetch status. Default 30 seconds.
	PollInterval time.Duration
	// Now is injectable for testing time-dependent logic. nil → time.Now.
	Now func() time.Time
}

// WatchCI polls the PR's head-commit checks and statuses until they reach
// a terminal state or the timeout expires. Returns CIPending when the
// timeout fires before anything terminal happens — caller treats that as
// "leave the PR alone, mark in digest".
//
// We aggregate two separate APIs because GitHub still has both:
//
//   - `/repos/{owner}/{repo}/commits/{sha}/check-runs` (modern, used by
//     GitHub Actions and Apps)
//   - `/repos/{owner}/{repo}/commits/{sha}/status` (legacy, used by
//     external CIs like CircleCI, Travis-CI)
//
// A PR is green only when both APIs are green; either red is a failure.
func (p *Publisher) WatchCI(ctx context.Context, prNumber int, opts WatchOptions) (CIStatus, error) {
	if opts.Timeout <= 0 {
		opts.Timeout = 30 * time.Minute
	}
	if opts.PollInterval <= 0 {
		opts.PollInterval = 30 * time.Second
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}

	pr, _, err := p.GitHub.PullRequests.Get(ctx, p.Owner, p.Name, prNumber)
	if err != nil {
		return "", fmt.Errorf("ghops: get PR %d: %w", prNumber, err)
	}
	if pr.Head == nil || pr.Head.SHA == nil {
		return "", fmt.Errorf("ghops: PR %d missing head SHA", prNumber)
	}
	sha := *pr.Head.SHA

	deadline := now().Add(opts.Timeout)
	for {
		status, err := p.aggregatedCIStatus(ctx, sha)
		if err != nil {
			return "", err
		}
		if status != CIPending {
			return status, nil
		}
		if !now().Before(deadline) {
			return CIPending, nil
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(opts.PollInterval):
		}
	}
}

// aggregatedCIStatus fetches both check-runs and the legacy status API
// and merges them into a single CIStatus. Pending wins over success;
// failure wins over pending.
func (p *Publisher) aggregatedCIStatus(ctx context.Context, sha string) (CIStatus, error) {
	checkRuns, _, err := p.GitHub.Checks.ListCheckRunsForRef(ctx, p.Owner, p.Name, sha, &github.ListCheckRunsOptions{})
	if err != nil {
		return "", fmt.Errorf("ghops: list check-runs: %w", err)
	}
	combined, _, err := p.GitHub.Repositories.GetCombinedStatus(ctx, p.Owner, p.Name, sha, nil)
	if err != nil {
		return "", fmt.Errorf("ghops: get combined status: %w", err)
	}

	pending := false
	for _, cr := range checkRuns.CheckRuns {
		switch cr.GetStatus() {
		case "queued", "in_progress", "waiting", "pending":
			pending = true
		case "completed":
			switch cr.GetConclusion() {
			case "success", "neutral", "skipped":
				// fine
			case "":
				pending = true // completed but conclusion missing — treat as pending
			default:
				return CIFailure, nil // failure / cancelled / timed_out / action_required
			}
		default:
			pending = true
		}
	}

	switch combined.GetState() {
	case "success":
		// no-op
	case "pending":
		pending = true
	case "failure", "error":
		return CIFailure, nil
	}

	if pending {
		return CIPending, nil
	}
	return CISuccess, nil
}

// ChangedFile is the subset of github.CommitFile this package needs:
// path + line counts. Used by the merge gate to enforce path allowlists
// and the diff-size cap.
type ChangedFile struct {
	Path      string
	Additions int
	Deletions int
}

// ListChangedFiles returns the files changed in the PR via the
// `/pulls/{n}/files` API. Paginated transparently.
func (p *Publisher) ListChangedFiles(ctx context.Context, prNumber int) ([]ChangedFile, error) {
	if prNumber == 0 {
		return nil, errors.New("ghops: zero PR number")
	}
	opts := &github.ListOptions{PerPage: 100}
	var out []ChangedFile
	for {
		batch, resp, err := p.GitHub.PullRequests.ListFiles(ctx, p.Owner, p.Name, prNumber, opts)
		if err != nil {
			return nil, fmt.Errorf("ghops: list PR files: %w", err)
		}
		for _, f := range batch {
			out = append(out, ChangedFile{
				Path:      f.GetFilename(),
				Additions: f.GetAdditions(),
				Deletions: f.GetDeletions(),
			})
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	return out, nil
}

// TotalLinesChanged sums additions + deletions across files. Used by the
// merge gate to enforce the diff-size cap (default 200 lines per PLAN.md).
func TotalLinesChanged(files []ChangedFile) int {
	n := 0
	for _, f := range files {
		n += f.Additions + f.Deletions
	}
	return n
}
