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

	for _, repoCfg := range r.Config.Repos {
		if abort, _ := b.ShouldAbort(); abort {
			// Anything we haven't processed gets requeued from state on
			// the next run — no per-task tracking needed here.
			break
		}
		repoResult, err := r.runRepo(ctx, repoCfg, t, b)
		if err != nil {
			// Per-repo errors are logged but don't kill the night.
			fmt.Fprintf(os.Stderr, "runner: %s/%s: %v\n", repoCfg.Owner, repoCfg.Name, err)
			continue
		}
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

	statePath := filepath.Join(r.Config.Paths.StateDir, "state.json")
	if err := r.State.Save(statePath); err != nil {
		fmt.Fprintf(os.Stderr, "runner: save state: %v\n", err)
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
			Hash:           tasks[i].Source.ContentHash,
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
		AnthropicClient: r.Anthropic,
		Model:           anthropic.Model(r.Config.Anthropic.ImplementerModel),
		RepoLocalPath:   repoCfg.LocalPath,
		RepoName:        repoCfg.Name,
		WorktreeRoot:    r.Config.Paths.WorktreeRoot,
		MaxParallel:     r.Config.Concurrency.MaxParallelAgents,
		RunAgent:        r.RunAgent,
		SpawnMCP:        r.SpawnMCP,
	}
	outcomes, err := d.Dispatch(ctx, items)
	if err != nil {
		return nil, fmt.Errorf("dispatch: %w", err)
	}

	// ghops sequence per outcome.
	pub := r.NewPublisher(ghClient, ts, repoCfg)

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
	// Commit + push.
	err := pub.CommitAndPush(ctx, ghops.CommitOptions{
		WorktreePath: oc.WorktreePath,
		Branch:       oc.Branch,
		Message:      buildCommitMessage(oc),
	})
	if errors.Is(err, ghops.ErrNoChanges) {
		// No diff is a successful no-op — task done, nothing to ship.
		oc.Status = state.StatusShipped
		return nil
	}
	if err != nil {
		return fmt.Errorf("commit/push: %w", err)
	}

	// Open PR. Draft if classification is not SAFE OR repo is in draft-only mode.
	isDraft := oc.Decision.Classification != triage.ClassAutoMergeSafe || repoCfg.Mode == config.ModeDraftOnly
	pr, err := pub.OpenPR(ctx, ghops.PROptions{
		Branch:     oc.Branch,
		Title:      buildPRTitle(oc),
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

	// draft-only mode stops at PR creation.
	if repoCfg.Mode == config.ModeDraftOnly {
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

// filterFreshTasks drops tasks that the state already records as
// terminal-shipped within the past 24h. Avoids re-attempting yesterday's
// successes; doesn't filter draft/blocked because those still need
// re-triage on each run.
func (r *Runner) filterFreshTasks(tasks []sources.Task) []sources.Task {
	cutoff := r.Now().Add(-24 * time.Hour)
	var out []sources.Task
	for _, t := range tasks {
		hash := state.HashTask(t.Source)
		existing, ok := r.State.Get(hash)
		if !ok {
			out = append(out, t)
			continue
		}
		if existing.Status == state.StatusShipped && existing.LastUpdated.After(cutoff) {
			continue
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

func buildPRTitle(oc *dispatch.Outcome) string {
	title := strings.TrimSpace(oc.Task.Source.Title)
	if title == "" {
		title = "burndown task"
	}
	if oc.Decision.Classification == triage.ClassNeedsReview {
		title = "WIP: " + title
	}
	return title
}

func buildPRBody(oc *dispatch.Outcome) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## Source\n\n%s\n\n", oc.Task.Source.URL)
	fmt.Fprintf(&b, "## Triage\n\nClassification: **%s**\n\nReason: %s\n\n",
		oc.Decision.Classification, oc.Decision.Reason)
	if oc.AgentResult != nil && oc.AgentResult.Summary != "" {
		fmt.Fprintf(&b, "## Agent summary\n\n%s\n\n", oc.AgentResult.Summary)
	}
	fmt.Fprint(&b, "---\n_Opened by overnight-burndown._\n")
	return b.String()
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
