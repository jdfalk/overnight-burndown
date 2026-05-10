// file: internal/triagepoll/poll.go
// version: 1.0.0
// guid: 6c7d8e9f-0a1b-2c3d-4e5f-6a7b8c9d0e1f
//
// Triage-poll state machine. Each invocation is stateless — all durable state
// lives in GitHub (tracking issue) and OpenAI (batch job). The 30-min cron
// calls Poll() once; it reads the current state, takes one step, and returns.
//
// State transitions:
//
//	 [no tracking issue]
//	   │ FindUntriagedIssues → none    → exit (no work)
//	   │ FindUntriagedIssues → some    → SubmitBatch + CreateTrackingIssue
//	   ▼
//	 [tracking issue exists]
//	   │ PollBatch → in_progress       → exit (wait for next cron)
//	   │ PollBatch → completed         → WriteTriageResult × N + CloseTrackingIssue
//	   │ PollBatch → failed/expired    → CloseTrackingIssue(error comment)
//	   ▼
//	 [done]

package triagepoll

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/google/go-github/v84/github"

	"github.com/jdfalk/overnight-burndown/internal/auth"
	"github.com/jdfalk/overnight-burndown/internal/config"
)

// PollConfig carries everything Poll() needs in one struct so callers don't
// need to thread a dozen parameters.
type PollConfig struct {
	// GitHub configuration (App credentials, etc.)
	GitHub config.GitHubConfig
	// TaskHub identifies the hub repo that holds task issues.
	TaskHub config.TaskHubConfig
	// OpenAI API key for submitting/polling batches.
	OpenAIAPIKey string
	// TriageModel is the OpenAI model to use for batch triage (e.g. "o3").
	TriageModel string
	// RepoName is the target repo being triaged (e.g. "audiobook-organizer").
	RepoName string
	// DryRun — when true, log actions but don't write anything to GitHub or OpenAI.
	DryRun bool
}

// PollResult summarises what happened in this invocation.
type PollResult struct {
	// Action is a short label for logging / job summaries.
	Action string
	// Detail provides supplemental information.
	Detail string
}

// Poll runs one iteration of the triage-poll state machine.
// It is safe to call repeatedly from a cron job.
func Poll(ctx context.Context, cfg PollConfig) (*PollResult, error) {
	// Build GitHub auth.
	a, err := auth.New(ctx, cfg.GitHub)
	if err != nil {
		return nil, fmt.Errorf("triagepoll: github auth: %w", err)
	}
	gh := github.NewClient(a.HTTPClient())

	hubOwner, hubName, err := splitRepo(cfg.TaskHub.Repo)
	if err != nil {
		return nil, fmt.Errorf("triagepoll: bad hub repo %q: %w", cfg.TaskHub.Repo, err)
	}

	// Ensure required labels exist (idempotent, cheap).
	if !cfg.DryRun {
		if err := EnsureLabelsExist(ctx, gh, hubOwner, hubName); err != nil {
			return nil, fmt.Errorf("triagepoll: ensure labels: %w", err)
		}
	}

	// --- State: is there an in-flight batch? ---
	tracking, err := FindTrackingIssue(ctx, gh, hubOwner, hubName, cfg.RepoName)
	if err != nil {
		return nil, fmt.Errorf("triagepoll: find tracking issue: %w", err)
	}

	if tracking != nil {
		return handleInFlight(ctx, gh, cfg, hubOwner, hubName, tracking)
	}

	// --- State: no batch in flight. Look for untriaged issues. ---
	return handleIdle(ctx, gh, cfg, hubOwner, hubName)
}

// handleInFlight polls the OpenAI batch referenced by the tracking issue.
func handleInFlight(ctx context.Context, gh *github.Client, cfg PollConfig, hubOwner, hubName string, tracking *github.Issue) (*PollResult, error) {
	batchID := ExtractBatchID(tracking)
	if batchID == "" {
		slog.WarnContext(ctx, "triagepoll: tracking issue has no batch_id, closing it",
			"issue", tracking.GetNumber())
		if !cfg.DryRun {
			if err := CloseTrackingIssue(ctx, gh, hubOwner, hubName,
				tracking.GetNumber(),
				"⚠️ Tracking issue had no `batch_id` — closing and resetting."); err != nil {
				return nil, err
			}
		}
		return &PollResult{Action: "reset", Detail: "tracking issue lacked batch_id"}, nil
	}

	slog.InfoContext(ctx, "triagepoll: polling OpenAI batch", "batch_id", batchID)
	if cfg.DryRun {
		return &PollResult{Action: "dry-run-poll", Detail: batchID}, nil
	}

	status, result, err := PollBatch(ctx, cfg.OpenAIAPIKey, batchID)
	if err != nil {
		return nil, fmt.Errorf("triagepoll: poll batch: %w", err)
	}

	switch status {
	case BatchStatusCompleted:
		return applyResults(ctx, gh, cfg, hubOwner, hubName, tracking.GetNumber(), batchID, result)

	case BatchStatusFailed, BatchStatusExpired, BatchStatusCancelled:
		comment := fmt.Sprintf("❌ OpenAI batch `%s` ended with status **%s** — no triage written. Re-triage will happen on the next cron run.", batchID, status)
		if err := CloseTrackingIssue(ctx, gh, hubOwner, hubName, tracking.GetNumber(), comment); err != nil {
			return nil, err
		}
		return &PollResult{Action: "batch-failed", Detail: string(status)}, nil

	default:
		// Still running (in_progress, finalizing, validating).
		slog.InfoContext(ctx, "triagepoll: batch still running", "status", status, "batch_id", batchID)
		return &PollResult{Action: "waiting", Detail: string(status)}, nil
	}
}

// applyResults writes triage decisions to hub issues and closes the tracking issue.
func applyResults(ctx context.Context, gh *github.Client, cfg PollConfig, hubOwner, hubName string, trackingNum int, batchID string, result *BatchResult) (*PollResult, error) {
	slog.InfoContext(ctx, "triagepoll: batch complete, applying results",
		"decisions", len(result.Decisions),
		"failed_requests", result.FailedCount)

	var written, skipped int
	for _, d := range result.Decisions {
		if err := WriteTriageResult(ctx, gh, hubOwner, hubName, d.IssueNumber, d); err != nil {
			slog.WarnContext(ctx, "triagepoll: write triage failed, skipping",
				"issue", d.IssueNumber, "err", err)
			skipped++
			continue
		}
		written++
	}

	comment := fmt.Sprintf(
		"✅ Batch `%s` complete — triage written for **%d** issue(s) (%d skipped, %d batch failures).",
		batchID, written, skipped, result.FailedCount)
	if err := CloseTrackingIssue(ctx, gh, hubOwner, hubName, trackingNum, comment); err != nil {
		return nil, err
	}

	return &PollResult{
		Action: "completed",
		Detail: fmt.Sprintf("wrote %d, skipped %d, batch_failed %d", written, skipped, result.FailedCount),
	}, nil
}

// handleIdle looks for untriaged issues and submits a new batch if any are found.
func handleIdle(ctx context.Context, gh *github.Client, cfg PollConfig, hubOwner, hubName string) (*PollResult, error) {
	issues, err := FindUntriagedIssues(ctx, gh, hubOwner, hubName, cfg.RepoName, cfg.TaskHub.LabelPrefix)
	if err != nil {
		return nil, fmt.Errorf("triagepoll: find untriaged: %w", err)
	}

	if len(issues) == 0 {
		slog.InfoContext(ctx, "triagepoll: no untriaged issues", "repo", cfg.RepoName)
		return &PollResult{Action: "idle", Detail: "no untriaged issues"}, nil
	}

	slog.InfoContext(ctx, "triagepoll: found untriaged issues, submitting batch",
		"count", len(issues), "repo", cfg.RepoName)

	if cfg.DryRun {
		return &PollResult{Action: "dry-run-submit", Detail: fmt.Sprintf("%d issues", len(issues))}, nil
	}

	batchID, err := SubmitBatch(ctx, cfg.OpenAIAPIKey, cfg.TriageModel, issues)
	if err != nil {
		return nil, fmt.Errorf("triagepoll: submit batch: %w", err)
	}

	trackingNum, err := CreateTrackingIssue(ctx, gh, hubOwner, hubName, cfg.RepoName, batchID, len(issues))
	if err != nil {
		return nil, fmt.Errorf("triagepoll: create tracking issue: %w", err)
	}

	slog.InfoContext(ctx, "triagepoll: batch submitted",
		"batch_id", batchID, "tracking_issue", trackingNum, "issue_count", len(issues))

	return &PollResult{
		Action: "submitted",
		Detail: fmt.Sprintf("batch %s, %d issues, tracking #%d", batchID, len(issues), trackingNum),
	}, nil
}

func splitRepo(repo string) (owner, name string, err error) {
	parts := strings.SplitN(repo, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("expected owner/name, got %q", repo)
	}
	return parts[0], parts[1], nil
}
