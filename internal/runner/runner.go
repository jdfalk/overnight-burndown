// Package runner is the orchestrator for one nightly burndown run.
//
// Runner.Run is the integration point — it loads no config of its own;
// the caller (cmd/burndown) builds a Runner with a parsed config and
// the per-repo hooks, then calls Run. Every external dependency is
// injected via a factory function so the integration test can replace
// the real Anthropic / GitHub / MCP / git surface with stubs.
//
// Per-night flow (matches PLAN.md):
//
//   1. Acquire ~/.burndown/run.lock (flock; another running burndown
//      makes this fail and we exit cleanly with no work done).
//   2. Initialize budget + triager from config.
//   3. For each repo, sequentially:
//      a. Build collectors (TODO + plans + GitHub issues).
//      b. Collect tasks.
//      c. Triage in one batch.
//      d. dry-run mode: stop here; just record decisions in state.
//      e. Otherwise: dispatch (worktree-per-task, capped concurrency)
//         and walk every Outcome through the ghops sequence.
//      f. Budget abort check between tasks; remaining items get
//         requeued for tomorrow.
//   4. Render the morning digest.
//   5. Write digest + save state + release lock (deferred).
//
// One repo failing does not abort the night — its outcomes get marked
// failed and the next repo runs. The caller decides what to do with
// the returned RunResult (typically: print digest path, exit 0).
package runner

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	openaiOption "github.com/openai/openai-go/option"
	"github.com/google/go-github/v84/github"

	"github.com/jdfalk/overnight-burndown/internal/auth"
	"github.com/jdfalk/overnight-burndown/internal/budget"
	"github.com/jdfalk/overnight-burndown/internal/config"
	"github.com/jdfalk/overnight-burndown/internal/digest"
	"github.com/jdfalk/overnight-burndown/internal/dispatch"
	"github.com/jdfalk/overnight-burndown/internal/ghops"
	"github.com/jdfalk/overnight-burndown/internal/sources"
	"github.com/jdfalk/overnight-burndown/internal/state"
	"github.com/jdfalk/overnight-burndown/internal/triage"
)

// RepoPublisher is the subset of *ghops.Publisher Runner uses. Pulled
// out as an interface so tests can inject a stub instead of building a
// real Publisher with a github.Client.
type RepoPublisher interface {
	CommitAndPush(ctx context.Context, opts ghops.CommitOptions) error
	OpenPR(ctx context.Context, opts ghops.PROptions) (*github.PullRequest, error)
	WatchCI(ctx context.Context, prNumber int, opts ghops.WatchOptions) (ghops.CIStatus, error)
	ListChangedFiles(ctx context.Context, prNumber int) ([]ghops.ChangedFile, error)
	AutoMerge(ctx context.Context, prNumber int) error
	ConvertToDraft(ctx context.Context, prNumber int) error
	AddLabel(ctx context.Context, prNumber int, label string) error
	CommentOnPR(ctx context.Context, prNumber int, body string) error
}

// AuthForRepoFunc returns a TokenSource + http.Client for auth. The
// http.Client carries the App-installation transport; both the
// IssueCollector (go-github) and the publisher use it.
type AuthForRepoFunc func(ctx context.Context, cfg config.GitHubConfig) (ghops.TokenSource, *http.Client, error)

// PublisherFactory builds a RepoPublisher for one repo.
type PublisherFactory func(gh *github.Client, ts ghops.TokenSource, repo config.RepoConfig) RepoPublisher

// Runner is the per-night orchestrator. Construct one in cmd/burndown,
// then call Run.
type Runner struct {
	Config config.Config
	State  *state.State

	Anthropic anthropic.Client

	// AuthForRepo is called once per non-dry-run repo. Default in
	// New uses internal/auth.New.
	AuthForRepo AuthForRepoFunc

	// NewGitHub builds a github.Client from an auth-aware http.Client.
	// Defaults to github.NewClient.
	NewGitHub func(*http.Client) *github.Client

	// NewPublisher builds a RepoPublisher. Default uses *ghops.Publisher.
	NewPublisher PublisherFactory

	// SpawnMCP and RunAgent flow into the dispatcher. Both default to
	// nil — production cmd/burndown wires real implementations; tests
	// inject stubs.
	SpawnMCP dispatch.SpawnMCPFunc
	RunAgent dispatch.RunAgentFunc

	// Triager is the triage classifier. Injectable so tests can point
	// at a stubbed httptest server. When nil, applyDefaults builds one
	// using the configured triage.provider + model + api_key_env.
	Triager triage.Provider

	// Now is injectable for deterministic digest dates.
	Now func() time.Time

	// LogDir is where to put per-run logs. Defaults to Config.Paths.LogDir.
	LogDir string
}

// RunResult is the everything-the-caller-might-want envelope.
type RunResult struct {
	Outcomes       []dispatch.Outcome
	PRs            map[string]digest.PRInfo
	MergedBranches map[string]bool
	Requeued       []dispatch.TaskWithDecision
	Stats          budget.Stats
	Digest         string
	DigestPath     string
}

// Run executes the nightly cycle. The returned error is non-nil only
// for things that prevent the run from happening at all (missing
// directories, lock contention). Per-repo and per-task failures are
// captured in the result without erroring.
func (r *Runner) Run(ctx context.Context) (*RunResult, error) {
	r.applyDefaults()

	// Pause file kill switch.
	if _, err := os.Stat(filepath.Join(r.Config.Paths.StateDir, "PAUSE")); err == nil {
		return nil, errors.New("runner: ~/.burndown/PAUSE present; aborting before any work")
	}

	// Single-instance lock.
	lockPath := filepath.Join(r.Config.Paths.StateDir, "run.lock")
	if err := os.MkdirAll(r.Config.Paths.StateDir, 0o755); err != nil {
		return nil, fmt.Errorf("runner: mkdir state_dir: %w", err)
	}
	release, err := state.AcquireLock(lockPath)
	if err != nil {
		return nil, fmt.Errorf("runner: acquire lock: %w", err)
	}
	defer release()

	b := budget.New(r.Config.Budget)
	t := r.Triager

	result := &RunResult{
		PRs:            map[string]digest.PRInfo{},
		MergedBranches: map[string]bool{},
	}

	var lastErr error
	succeeded := 0
	for _, repoCfg := range r.Config.Repos {
		if abort, _ := b.ShouldAbort(); abort {
			// Anything we haven't processed gets requeued from state on
			// the next run — no per-task tracking needed here.
			break
		}
		repoResult, err := r.runRepo(ctx, repoCfg, t, b)
		if err != nil {
			// Per-repo errors are logged but don't kill the night unless
			// every repo fails (e.g. invalid API key).
			fmt.Fprintf(os.Stderr, "runner: %s/%s: %v\n", repoCfg.Owner, repoCfg.Name, err)
			lastErr = err
			continue
		}
		succeeded++
		result.Outcomes = append(result.Outcomes, repoResult.Outcomes...)
		for k, v := range repoResult.PRs {
			result.PRs[k] = v
		}
		for k, v := range repoResult.MergedBranches {
			result.MergedBranches[k] = v
		}
		result.Requeued = append(result.Requeued, repoResult.Requeued...)
	}

	// Render digest + persist.
	result.Stats = b.Snapshot()
	result.Digest = digest.Render(digest.Input{
		RunDate:        r.Now(),
		Outcomes:       result.Outcomes,
		PRs:            result.PRs,
		MergedBranches: result.MergedBranches,
		Requeued:       result.Requeued,
		Stats:          result.Stats,
	})
	if err := r.writeDigest(result); err != nil {
		fmt.Fprintf(os.Stderr, "runner: write digest: %v\n", err)
	}

	if err := r.State.SaveDir(r.Config.Paths.StateDir); err != nil {
		fmt.Fprintf(os.Stderr, "runner: save state: %v\n", err)
	}

	if succeeded == 0 && lastErr != nil {
		return result, fmt.Errorf("runner: all repos failed; last error: %w", lastErr)
	}
	return result, nil
}

// repoResult is the per-repo accumulator.
type repoResult struct {
	Outcomes       []dispatch.Outcome
	PRs            map[string]digest.PRInfo
	MergedBranches map[string]bool
	Requeued       []dispatch.TaskWithDecision
}

func (r *Runner) runRepo(ctx context.Context, repoCfg config.RepoConfig, t triage.Provider, b *budget.Budget) (*repoResult, error) {
	repoFullName := repoCfg.Owner + "/" + repoCfg.Name
	out := &repoResult{
		PRs:            map[string]digest.PRInfo{},
		MergedBranches: map[string]bool{},
	}

	// Build auth + GitHub client. dry-run can still hit issues if the
	// caller provided GitHub auth; otherwise we use an unauthenticated
	// client (rate-limited but functional for public repos).
	var ts ghops.TokenSource
	var ghClient *github.Client
	if r.Config.GitHub.AppID != 0 {
		_, httpc, err := r.AuthForRepo(ctx, r.Config.GitHub)
		if err != nil {
			return nil, fmt.Errorf("auth: %w", err)
		}
		// AuthForRepo returns the TokenSource separately for ghops.
		ts2, _, _ := r.AuthForRepo(ctx, r.Config.GitHub)
		ts = ts2
		ghClient = r.NewGitHub(httpc)
	} else {
		ghClient = r.NewGitHub(nil)
	}

	// Reconcile: patch state holes from crashed/incomplete prior runs by
	// querying open automation-labeled PRs on GitHub. Best-effort — a
	// reconcile failure (e.g. rate limit) is logged but does not abort the
	// run; we'd rather re-dispatch than silently skip the repo.
	if r.Config.GitHub.AppID != 0 {
		if err := ReconcileFromGitHub(ctx, ghClient, r.State,
			repoCfg.Owner, repoCfg.Name); err != nil {
			fmt.Fprintf(os.Stderr, "runner: reconcile: %v\n", err)
		}
	}

	// Collect.
	collectors := []sources.Collector{
		sources.NewTODOCollector(),
		sources.NewPlanCollector(),
		sources.NewIssueCollector(ghClient),
	}
	tasks, err := sources.CollectAll(ctx, repoFullName, repoCfg.LocalPath, collectors...)
	if err != nil {
		return nil, fmt.Errorf("collect: %w", err)
	}
	tasks = r.filterFreshTasks(tasks)
	if len(tasks) == 0 {
		return out, nil
	}

	// Triage.
	decisions, err := t.Triage(ctx, tasks)
	if err != nil {
		return nil, fmt.Errorf("triage: %w", err)
	}

	// Pair up + record in state.
	items := make([]dispatch.TaskWithDecision, len(tasks))
	for i := range tasks {
		items[i] = dispatch.TaskWithDecision{Task: tasks[i], Decision: decisions[i]}
		r.State.Upsert(&state.TaskState{
			Hash:           state.HashTask(tasks[i].Source),
			Source:         tasks[i].Source,
			Status:         state.StatusQueued,
			Classification: string(decisions[i].Classification),
		})
	}

	// dry-run: triage-only. Build pseudo-Outcomes so the digest can
	// show what WOULD have been dispatched.
	if repoCfg.Mode == config.ModeDryRun {
		for _, item := range items {
			out.Outcomes = append(out.Outcomes, dispatch.Outcome{
				Task:     item.Task,
				Decision: item.Decision,
				Status:   state.StatusBlocked, // dry-run treated as blocked-by-mode for digest purposes
			})
		}
		return out, nil
	}

	// Dispatch.
	d := &dispatch.Dispatcher{
		AnthropicClient:      r.Anthropic,
		Model:                anthropic.Model(r.Config.Anthropic.ImplementerModel),
		RepoLocalPath:        repoCfg.LocalPath,
		RepoName:             repoCfg.Name,
		WorktreeRoot:         r.Config.Paths.WorktreeRoot,
		MaxParallel:          r.Config.Concurrency.MaxParallelAgents,
		WorktreeExcludePaths: repoCfg.WorktreeExcludePaths,
		RunAgent:             r.RunAgent,
		SpawnMCP:             r.SpawnMCP,
	}
	outcomes, err := d.Dispatch(ctx, items)
	if err != nil {
		return nil, fmt.Errorf("dispatch: %w", err)
	}

	// ghops sequence per outcome.
	pub := r.NewPublisher(ghClient, ts, repoCfg)

	// Merge any PRs that a human or review bot approved via the
	// "merge-approved" label since the last nightly. Best-effort.
	r.mergeApprovedPRs(ctx, pub, ghClient, repoCfg.Owner, repoCfg.Name, out.MergedBranches)

	for i := range outcomes {
		if abort, _ := b.ShouldAbort(); abort {
			// Remaining tasks were dispatched but we won't publish them.
			out.Requeued = append(out.Requeued, items[i])
			continue
		}
		if outcomes[i].Status != state.StatusInFlight {
			continue
		}
		if err := r.publishOutcome(ctx, pub, repoCfg, &outcomes[i], out.PRs, out.MergedBranches); err != nil {
			outcomes[i].Status = state.StatusFailed
			outcomes[i].Error = err.Error()
		}
		// outcome:<status> records the final result paired to the model so we
		// can later filter "model:codex-mini + outcome:failed" vs "outcome:shipped"
		// to tune complexity → model-tier thresholds.
		if prInfo := out.PRs[outcomes[i].Branch]; prInfo.Number != 0 {
			var outcomeLabel string
			switch outcomes[i].Status {
			case state.StatusShipped:
				outcomeLabel = "outcome:shipped"
			case state.StatusFailed:
				outcomeLabel = "outcome:failed"
			case state.StatusNoChange:
				outcomeLabel = "outcome:no-change"
			case state.StatusDraft:
				outcomeLabel = "outcome:draft"
			}
			if outcomeLabel != "" {
				if err := pub.AddLabel(ctx, prInfo.Number, outcomeLabel); err != nil {
					fmt.Fprintf(os.Stderr, "burndown: AddLabel %q on PR #%d: %v\n", outcomeLabel, prInfo.Number, err)
				}
			}
		}
		// Persist final status + PR info so future runs don't re-dispatch.
		prInfo := out.PRs[outcomes[i].Branch]
		r.State.Upsert(&state.TaskState{
			Hash:           state.HashTask(items[i].Task.Source),
			Source:         items[i].Task.Source,
			Status:         outcomes[i].Status,
			Branch:         outcomes[i].Branch,
			PRNumber:       prInfo.Number,
			PRURL:          prInfo.URL,
			Classification: string(items[i].Decision.Classification),
		})
	}
	out.Outcomes = outcomes
	return out, nil
}

// publishOutcome runs the ghops sequence for one task: commit → push →
// open PR → (full mode only) watch CI → evaluate gate → auto-merge or
// demote-to-draft. Returns an error only when something prevents
// completing the sequence; the caller marks the outcome failed.
func (r *Runner) publishOutcome(
	ctx context.Context,
	pub RepoPublisher,
	repoCfg config.RepoConfig,
	oc *dispatch.Outcome,
	prs map[string]digest.PRInfo,
	merged map[string]bool,
) error {
	// Agent's self-reported task status (via report_status MCP tool) is
	// checked first — before any git operations. If the agent couldn't
	// finish, opening a PR would block re-dispatch (the state row would
	// sit as StatusDraft/StatusBlocked and filterFreshTasks would skip it).
	// Instead: discard the worktree changes, set a non-PR status, and let
	// the scheduler decide whether to retry.
	//
	//   reported=blocked  → StatusBlocked (terminal; needs human to resolve
	//                        the underlying issue before the task is viable)
	//   reported=partial  → StatusRequeued (retry next night)
	//   reported=complete → proceed to commit+push+PR
	//   reported=""       → proceed (agent didn't call report_status;
	//                        treat as complete for backward compat)
	reported := ""
	if oc.AgentResult != nil {
		reported = strings.ToLower(strings.TrimSpace(oc.AgentResult.ReportedStatus))
	}
	switch reported {
	case "blocked":
		oc.Status = state.StatusBlocked
		return nil
	case "partial":
		oc.Status = state.StatusRequeued
		return nil
	}

	// Commit + push.
	err := pub.CommitAndPush(ctx, ghops.CommitOptions{
		WorktreePath: oc.WorktreePath,
		Branch:       oc.Branch,
		Message:      buildCommitMessage(oc),
	})
	if errors.Is(err, ghops.ErrNoChanges) {
		// No diff means the agent finished cleanly but produced no
		// file changes. Distinct from StatusShipped (which implies a
		// merged PR exists) so the digest can flag agent quality
		// problems instead of hiding them as successes.
		oc.Status = state.StatusNoChange
		return nil
	}
	if err != nil {
		return fmt.Errorf("commit/push: %w", err)
	}

	// Open PR. Draft state:
	//   draft-only mode          → draft
	//   review mode              → ready-for-review
	//   full + SAFE + complete   → ready-for-review (will auto-merge if CI green)
	//   full + non-SAFE          → draft
	var isDraft bool
	switch {
	case repoCfg.Mode == config.ModeDraftOnly:
		isDraft = true
	case repoCfg.Mode == config.ModeReview:
		isDraft = false
	default: // full
		isDraft = oc.Decision.Classification != triage.ClassAutoMergeSafe
	}
	pr, err := pub.OpenPR(ctx, ghops.PROptions{
		Branch:     oc.Branch,
		Title:      buildPRTitle(oc, reported),
		Body:       buildPRBody(oc),
		BaseBranch: "main",
		Draft:      isDraft,
	})
	if err != nil {
		return fmt.Errorf("open PR: %w", err)
	}
	prNum := pr.GetNumber()
	prURL := pr.GetHTMLURL()
	prs[oc.Branch] = digest.PRInfo{Number: prNum, URL: prURL}

	// Apply labels: 'automation' tags every burndown PR; status:* reflects
	// the agent's self-report (or "needs-review" when missing); size/* is
	// derived from the diff size; we'll fetch that via ListChangedFiles
	// for accuracy. Best-effort — label failures don't fail the cell.
	r.applyBurndownLabels(ctx, pub, prNum, oc, reported)

	// draft-only mode stops at PR creation.
	if repoCfg.Mode == config.ModeDraftOnly {
		oc.Status = state.StatusDraft
		return nil
	}
	// review mode: ready-for-review PR opened, no CI watch, no auto-merge.
	// Status is StatusDraft for digest purposes ("opened, awaiting human")
	// even though the PR isn't literally a draft on GitHub — the digest
	// vocabulary doesn't currently distinguish.
	if repoCfg.Mode == config.ModeReview {
		oc.Status = state.StatusDraft
		return nil
	}
	// Non-SAFE classifications also stop at draft.
	if oc.Decision.Classification != triage.ClassAutoMergeSafe {
		oc.Status = state.StatusDraft
		return nil
	}

	// Full mode + AUTO_MERGE_SAFE: watch CI, evaluate gates, merge or demote.
	timeout := time.Duration(repoCfg.CIWatchTimeoutSeconds) * time.Second
	ciStatus, err := pub.WatchCI(ctx, prNum, ghops.WatchOptions{Timeout: timeout})
	if err != nil {
		return fmt.Errorf("watch CI: %w", err)
	}
	files, err := pub.ListChangedFiles(ctx, prNum)
	if err != nil {
		return fmt.Errorf("list changed files: %w", err)
	}
	gateDecision := ghops.EvaluateGate(ghops.GateInputs{
		Classification:    oc.Decision.Classification,
		HasAutoOK:         oc.Task.HasAutoOK,
		CIStatus:          ciStatus,
		ChangedFiles:      files,
		AutoMergePaths:    repoCfg.AutoMergePaths,
		ForcedReviewPaths: r.Config.Defaults.ForcedReviewPaths,
		DiffSizeCapLines:  r.Config.Defaults.DiffSizeCapLines,
	})

	if gateDecision.Allow {
		if err := pub.AutoMerge(ctx, prNum); err != nil {
			return fmt.Errorf("auto-merge: %w", err)
		}
		merged[oc.Branch] = true
		oc.Status = state.StatusShipped
		return nil
	}

	// Demote: convert to draft, label, comment with reasons. Best-effort —
	// if any of these fails, we still mark the outcome draft.
	_ = pub.ConvertToDraft(ctx, prNum)
	_ = pub.AddLabel(ctx, prNum, "burndown-failed")
	body := "## Auto-merge gates failed\n\n" +
		strings.Join(prefixed("- ", gateDecision.Reasons), "\n") +
		"\n\nDemoted to draft for human review."
	_ = pub.CommentOnPR(ctx, prNum, body)
	oc.Status = state.StatusDraft
	return nil
}

// filterFreshTasks drops tasks we shouldn't re-dispatch tonight:
//
//   * shipped within the past 24h — yesterday's successes don't need
//     a second crack (the source-of-truth git ref already has the
//     change merged).
//   * draft / in-flight / open-review — a previous run already opened
//     a PR (or kicked off an agent) that's still pending. Re-running
//     would burn triage + agent tokens, the worktree-add would fail
//     on the existing branch, and the cell would go red without
//     producing anything new. Drop until the prior PR is merged,
//     closed, or the local state ages out.
//
// The 7-day TTL on draft/in-flight is a safety valve: if a state row
// gets orphaned (PR was closed manually but state never updated),
// we retry after a week so the task isn't permanently stuck.
// applyBurndownLabels tags a freshly-opened PR with status + size + a
// generic "automation" marker. All best-effort — a label that doesn't
// exist on the target repo, or a transient API hiccup, is logged and
// ignored. Downstream automations (auto-merge bots, claude-review, etc.)
// can filter on these labels rather than parsing PR bodies.
//
// Labels applied:
//
//   automation                 every burndown PR
//   status:ready              report_status="complete"
//   status:needs-review       report_status="partial" or absent
//   status:blocked            report_status="blocked"
//   size/{XS,S,M,L,XL}        based on changed-line count from
//                              ListChangedFiles (XS<10, S<50, M<200,
//                              L<1000, XL≥1000)
//   needs-claude-review       size/L or size/XL — flag for the
//                              human-or-bot review step
func (r *Runner) applyBurndownLabels(ctx context.Context, pub RepoPublisher, prNum int, oc *dispatch.Outcome, reported string) {
	labels := []string{"automation"}
	// blocked/partial are now intercepted before publishOutcome; only
	// complete and empty reach here. Keep the switch exhaustive so if new
	// values are added they default to needs-review rather than silently
	// getting no status label.
	switch reported {
	case "complete":
		labels = append(labels, "status:ready")
	default: // empty (no report_status call) or any future value
		labels = append(labels, "status:needs-review")
	}

	// model:<slug> records every model that ran for this task — one label per
	// attempt. Most tasks have one. When escalation occurred (primary model
	// hit 429 limit), multiple labels appear (e.g. model:codex-mini +
	// model:gpt-5-3-codex). Combined with the outcome label this becomes the
	// training signal for tuning complexity→model-tier thresholds.
	attempted := oc.AttemptedModels
	if len(attempted) == 0 && oc.Model != "" {
		attempted = []string{oc.Model} // fallback for non-Responses paths
	}
	for _, m := range attempted {
		if m != "" {
			labels = append(labels, "model:"+modelSlug(m))
		}
	}

	// Size bucket from changed-line count. ListChangedFiles also serves
	// the gate evaluator, so we may already have it; pay the API call
	// once here and accept the duplication for now.
	if files, err := pub.ListChangedFiles(ctx, prNum); err == nil {
		var total int
		for _, f := range files {
			total += f.Additions + f.Deletions
		}
		size := sizeBucket(total)
		labels = append(labels, "size/"+size)
		if size == "L" || size == "XL" {
			labels = append(labels, "needs-claude-review")
		}
	}

	for _, l := range labels {
		if err := pub.AddLabel(ctx, prNum, l); err != nil {
			fmt.Fprintf(os.Stderr, "burndown: AddLabel %q on PR #%d: %v\n", l, prNum, err)
		}
	}
}

// modelSlug turns a model name into a GitHub-label-safe slug. Dots and
// underscores become hyphens; spaces are stripped. E.g. "gpt-5.3-codex"
// → "gpt-5-3-codex", "claude-haiku-4-5-20251001" is already clean.
func modelSlug(model string) string {
	var b strings.Builder
	for _, c := range strings.ToLower(model) {
		switch {
		case c == '.' || c == '_':
			b.WriteRune('-')
		case c == ' ':
			// skip spaces
		default:
			b.WriteRune(c)
		}
	}
	return b.String()
}

// sizeBucket returns the size/* bucket for a given changed-line count.
// Buckets are roughly aligned with industry conventions (gitsize, etc.).
func sizeBucket(linesChanged int) string {
	switch {
	case linesChanged < 10:
		return "XS"
	case linesChanged < 50:
		return "S"
	case linesChanged < 200:
		return "M"
	case linesChanged < 1000:
		return "L"
	default:
		return "XL"
	}
}

func (r *Runner) filterFreshTasks(tasks []sources.Task) []sources.Task {
	now := r.Now()
	shippedCutoff := now.Add(-24 * time.Hour)
	pendingCutoff := now.Add(-7 * 24 * time.Hour)
	var out []sources.Task
	for _, t := range tasks {
		hash := state.HashTask(t.Source)
		existing, ok := r.State.Get(hash)
		if !ok {
			out = append(out, t)
			continue
		}
		switch existing.Status {
		case state.StatusShipped:
			if existing.LastUpdated.After(shippedCutoff) {
				continue
			}
		case state.StatusDraft, state.StatusInFlight:
			// A PR is open and waiting for review (review/draft-only
			// modes), or an agent run is mid-flight from a prior
			// nightly that didn't get a chance to publish. Either way
			// don't redo the work.
			if existing.LastUpdated.After(pendingCutoff) {
				continue
			}
		case state.StatusBlocked:
			// Agent explicitly reported it couldn't proceed (missing
			// dependency, ambiguous spec, etc.). No PR was opened.
			// Hold for 7 days so the operator has time to resolve the
			// blocker; after that, auto-retry in case the issue has
			// been fixed upstream.
			if existing.LastUpdated.After(pendingCutoff) {
				continue
			}
		case state.StatusNoChange:
			// Agent finished without producing a diff yesterday.
			// Re-running tonight likely gets the same outcome (the
			// task as written may not be implementable, or the agent
			// is consistently misreading it). Skip for 24h to give
			// the operator time to refine the wording, then retry.
			if existing.LastUpdated.After(shippedCutoff) {
				continue
			}
		}
		out = append(out, t)
	}
	return out
}

// writeDigest renders the digest file under DigestDir.
func (r *Runner) writeDigest(result *RunResult) error {
	if err := os.MkdirAll(r.Config.Paths.DigestDir, 0o755); err != nil {
		return err
	}
	path := filepath.Join(r.Config.Paths.DigestDir, digest.FilenameFor(r.Now()))
	result.DigestPath = path
	return os.WriteFile(path, []byte(result.Digest), 0o644)
}

// applyDefaults wires production implementations for any nil hooks. Tests
// can override individually before calling Run.
func (r *Runner) applyDefaults() {
	if r.Now == nil {
		r.Now = time.Now
	}
	if r.Triager == nil {
		switch r.Config.Triage.Provider {
		case config.ProviderOpenAI:
			r.Triager = triage.NewOpenAI(
				r.Config.Triage.Model,
				openaiOption.WithAPIKey(os.Getenv(r.Config.OpenAI.APIKeyEnv)),
			)
		default: // anthropic
			r.Triager = triage.NewAnthropic(
				r.Config.Triage.Model,
				option.WithAPIKey(os.Getenv(r.Config.Anthropic.APIKeyEnv)),
			)
		}
	}
	if r.AuthForRepo == nil {
		r.AuthForRepo = func(ctx context.Context, cfg config.GitHubConfig) (ghops.TokenSource, *http.Client, error) {
			a, err := auth.New(ctx, cfg)
			if err != nil {
				return nil, nil, err
			}
			return a, a.HTTPClient(), nil
		}
	}
	if r.NewGitHub == nil {
		r.NewGitHub = func(c *http.Client) *github.Client {
			return github.NewClient(c)
		}
	}
	if r.NewPublisher == nil {
		r.NewPublisher = func(gh *github.Client, ts ghops.TokenSource, repo config.RepoConfig) RepoPublisher {
			return ghops.NewPublisher(gh, ts, repo.Owner, repo.Name,
				botAuthorName(r.Config.GitHub),
				botAuthorEmail(r.Config.GitHub))
		}
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func buildCommitMessage(oc *dispatch.Outcome) string {
	prefix := "chore"
	if oc.Decision.Classification == triage.ClassAutoMergeSafe {
		prefix = "chore"
	}
	title := strings.TrimSpace(oc.Task.Source.Title)
	if title == "" {
		title = "burndown task"
	}
	body := ""
	if oc.AgentResult != nil && oc.AgentResult.Summary != "" {
		body = "\n\n" + strings.TrimSpace(oc.AgentResult.Summary)
	}
	return fmt.Sprintf("%s: %s%s", prefix, title, body)
}

func buildPRTitle(oc *dispatch.Outcome, reported string) string {
	title := strings.TrimSpace(oc.Task.Source.Title)
	if title == "" {
		title = "burndown task"
	}
	// Title prefix surfaces the agent's self-report at a glance in the PR
	// list. Prefix order is intentional: BLOCKED dominates everything;
	// otherwise WIP for partial / NeedsReview classification.
	switch reported {
	case "blocked":
		return "BLOCKED: " + title
	case "partial":
		return "WIP: " + title
	}
	if oc.Decision.Classification == triage.ClassNeedsReview {
		title = "WIP: " + title
	}
	return title
}

func buildPRBody(oc *dispatch.Outcome) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## Source\n\n%s\n\n", sourceLink(oc.Task.Source))
	fmt.Fprintf(&b, "## Triage\n\nClassification: **%s**\n\nReason: %s\n\n",
		oc.Decision.Classification, oc.Decision.Reason)
	if oc.AgentResult != nil && oc.AgentResult.Summary != "" {
		fmt.Fprintf(&b, "## Agent summary\n\n%s\n\n", oc.AgentResult.Summary)
		// Surface the structured self-report so reviewers see at a glance
		// what the agent claimed status-wise.
		if oc.AgentResult.ReportedStatus != "" {
			fmt.Fprintf(&b, "**Reported status:** `%s`", oc.AgentResult.ReportedStatus)
			if oc.AgentResult.ReportedReason != "" {
				fmt.Fprintf(&b, " — %s", oc.AgentResult.ReportedReason)
			}
			b.WriteString("\n\n")
		}
	}
	fmt.Fprint(&b, "---\n_Opened by overnight-burndown._\n")
	return b.String()
}

// sourceLink turns a Source into a clickable markdown link. Falls back
// to the raw URL when we don't have enough context to build a github.com
// URL (e.g. local-only scans without a Repo field).
//
// TODO.md and plan files store paths relative to the repo root in
// Source.URL (with #L<n> anchor); github.com/<repo>/blob/main/<rel> is
// the canonical viewer URL.
func sourceLink(src state.Source) string {
	if src.Repo == "" || src.URL == "" {
		return src.URL
	}
	switch src.Type {
	case state.SourceIssue:
		// Issue URLs from sources/issues.go are already full URLs.
		return src.URL
	default:
		// TODO / plan / etc.: <rel-path>#L<n> → github blob link.
		// Skip the conversion if URL already looks absolute (legacy
		// state rows from before the relative-path fix).
		if strings.HasPrefix(src.URL, "http://") || strings.HasPrefix(src.URL, "https://") {
			return src.URL
		}
		// Strip any leading slash for safety; rel paths shouldn't have
		// one but some scans may.
		rel := strings.TrimPrefix(src.URL, "/")
		// Detect + drop a leading absolute-path artifact (e.g.
		// "/__w/.../audiobook-organizer/TODO.md#L212"). If the URL
		// contains the repo's name in the path, slice from there on.
		if i := strings.Index(rel, "/"+repoNameFromFull(src.Repo)+"/"); i >= 0 {
			rel = rel[i+len("/"+repoNameFromFull(src.Repo)+"/"):]
		}
		return fmt.Sprintf("[`%s`](https://github.com/%s/blob/main/%s)",
			rel, src.Repo, rel)
	}
}

func repoNameFromFull(full string) string {
	if i := strings.LastIndex(full, "/"); i >= 0 {
		return full[i+1:]
	}
	return full
}

// prefixed prepends prefix to each item; tiny helper to keep
// publishOutcome's body readable.
func prefixed(prefix string, items []string) []string {
	out := make([]string, len(items))
	for i, s := range items {
		out[i] = prefix + s
	}
	return out
}

// botAuthorName / botAuthorEmail derive the commit identity from the
// GitHub App config. Conventional GitHub bot pattern:
//
//   Name:  "<app-name>[bot]"
//   Email: "<APP_ID>+<app-name>[bot]@users.noreply.github.com"
//
// The app name is hardcoded since burndown only supports one App
// per deployment for v1; multi-app support is a follow-up.
func botAuthorName(_ config.GitHubConfig) string {
	return "jdfalk-burndown-bot[bot]"
}

func botAuthorEmail(cfg config.GitHubConfig) string {
	return fmt.Sprintf("%d+jdfalk-burndown-bot[bot]@users.noreply.github.com", cfg.AppID)
}
