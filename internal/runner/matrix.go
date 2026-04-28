// file: internal/runner/matrix.go
// version: 1.0.0
// guid: 7a8b9c0d-1e2f-4a3b-8c4d-5e6f7a8b9c0d
//
// Matrix-mode entry points for fan-out across one runner per task.
//
// The legacy Runner.Run flow does triage + dispatch + ghops + digest in
// one process. For GitHub Actions matrix execution we split it into
// three phases that each correspond to a single workflow job:
//
//   1. Triage — load config, collect tasks across all repos, run the
//      triage agent in one batch, emit a JSON file the matrix consumes.
//   2. DispatchOne — given a single TaskWithDecision (and the surrounding
//      repoCfg), create the worktree, run the implementer, run the ghops
//      sequence (commit/push/PR/CI/merge), emit a single Outcome JSON.
//   3. Aggregate — given a directory of Outcome JSON files, render the
//      digest the same way Run does.
//
// The split is deliberately minimal: we extract only the entry points
// the workflow needs, and re-use every internal helper (publishOutcome,
// dispatch.Dispatcher, etc.) without copies. The legacy Run flow stays
// intact so we can A/B against the matrix workflow.

package runner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/google/go-github/v84/github"

	"github.com/jdfalk/overnight-burndown/internal/budget"
	"github.com/jdfalk/overnight-burndown/internal/config"
	"github.com/jdfalk/overnight-burndown/internal/digest"
	"github.com/jdfalk/overnight-burndown/internal/dispatch"
	"github.com/jdfalk/overnight-burndown/internal/ghops"
	"github.com/jdfalk/overnight-burndown/internal/sources"
	"github.com/jdfalk/overnight-burndown/internal/state"
)

// MatrixTask is one row in the triage→dispatch handoff. It carries
// everything dispatch-one needs to run a single task without re-doing
// triage: the task itself, the triage decision, and the index of the
// repo it came from in Config.Repos.
type MatrixTask struct {
	RepoIndex int                       `json:"repo_index"`
	Item      dispatch.TaskWithDecision `json:"item"`
}

// TriageResult is what the triage subcommand emits — one JSON file per
// run. The matrix workflow reads Tasks and fans out one cell per entry.
type TriageResult struct {
	GeneratedAt time.Time    `json:"generated_at"`
	Tasks       []MatrixTask `json:"tasks"`
	// Decisions per repo for repos in dry-run mode — these never
	// dispatch, but we still record them in the digest. Stored as
	// pre-built Outcomes to keep the aggregator simple.
	DryRunOutcomes []dispatch.Outcome `json:"dry_run_outcomes"`
}

// Triage runs phases 1+2+3a of the legacy flow (collect + triage +
// dry-run-pseudo-outcome) and returns the result for the matrix
// dispatcher to consume. It does NOT acquire the run lock — the matrix
// workflow guarantees a single triage job per run via concurrency:.
func (r *Runner) Triage(ctx context.Context) (*TriageResult, error) {
	r.applyDefaults()

	if _, err := os.Stat(filepath.Join(r.Config.Paths.StateDir, "PAUSE")); err == nil {
		return nil, errors.New("runner: ~/.burndown/PAUSE present; aborting before any work")
	}
	if err := os.MkdirAll(r.Config.Paths.StateDir, 0o755); err != nil {
		return nil, fmt.Errorf("runner: mkdir state_dir: %w", err)
	}

	t := r.Triager
	out := &TriageResult{GeneratedAt: r.Now()}

	for repoIdx, repoCfg := range r.Config.Repos {
		ghClient, _, err := r.buildGitHubClient(ctx)
		if err != nil {
			return nil, fmt.Errorf("triage: %s/%s: %w", repoCfg.Owner, repoCfg.Name, err)
		}
		repoFullName := repoCfg.Owner + "/" + repoCfg.Name

		collectors := []sources.Collector{
			sources.NewTODOCollector(),
			sources.NewPlanCollector(),
			sources.NewIssueCollector(ghClient),
		}
		tasks, err := sources.CollectAll(ctx, repoFullName, repoCfg.LocalPath, collectors...)
		if err != nil {
			return nil, fmt.Errorf("triage: collect %s: %w", repoFullName, err)
		}
		tasks = r.filterFreshTasks(tasks)
		if len(tasks) == 0 {
			continue
		}

		decisions, err := t.Triage(ctx, tasks)
		if err != nil {
			return nil, fmt.Errorf("triage: %s: %w", repoFullName, err)
		}

		for i := range tasks {
			item := dispatch.TaskWithDecision{Task: tasks[i], Decision: decisions[i]}
			r.State.Upsert(&state.TaskState{
				Hash:           tasks[i].Source.ContentHash,
				Source:         tasks[i].Source,
				Status:         state.StatusQueued,
				Classification: string(decisions[i].Classification),
			})

			if repoCfg.Mode == config.ModeDryRun {
				out.DryRunOutcomes = append(out.DryRunOutcomes, dispatch.Outcome{
					Task:     item.Task,
					Decision: item.Decision,
					Status:   state.StatusBlocked,
				})
				continue
			}
			out.Tasks = append(out.Tasks, MatrixTask{RepoIndex: repoIdx, Item: item})
		}
	}

	statePath := filepath.Join(r.Config.Paths.StateDir, "state.json")
	if err := r.State.Save(statePath); err != nil {
		fmt.Fprintf(os.Stderr, "triage: save state: %v\n", err)
	}
	return out, nil
}

// DispatchOne runs the worktree-create + agent-run + ghops sequence for
// a single task. Returns one Outcome — the matrix workflow uploads it
// as an artifact and the aggregator concatenates them. Budget enforcement
// is per-cell rather than per-run; the workflow timeout-minutes provides
// the global cap.
func (r *Runner) DispatchOne(ctx context.Context, mt MatrixTask) (*dispatch.Outcome, error) {
	r.applyDefaults()
	if mt.RepoIndex < 0 || mt.RepoIndex >= len(r.Config.Repos) {
		return nil, fmt.Errorf("dispatch-one: repo_index %d out of range", mt.RepoIndex)
	}
	repoCfg := r.Config.Repos[mt.RepoIndex]

	ghClient, ts, err := r.buildGitHubClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("dispatch-one: auth: %w", err)
	}

	d := &dispatch.Dispatcher{
		AnthropicClient:      r.Anthropic,
		Model:                anthropic.Model(r.Config.Anthropic.ImplementerModel),
		RepoLocalPath:        repoCfg.LocalPath,
		RepoName:             repoCfg.Name,
		WorktreeRoot:         r.Config.Paths.WorktreeRoot,
		MaxParallel:          1, // single-task; concurrency irrelevant
		WorktreeExcludePaths: repoCfg.WorktreeExcludePaths,
		RunAgent:             r.RunAgent,
		SpawnMCP:             r.SpawnMCP,
	}
	outcomes, err := d.Dispatch(ctx, []dispatch.TaskWithDecision{mt.Item})
	if err != nil {
		return nil, fmt.Errorf("dispatch-one: %w", err)
	}
	if len(outcomes) != 1 {
		return nil, fmt.Errorf("dispatch-one: expected 1 outcome, got %d", len(outcomes))
	}
	oc := &outcomes[0]

	// In-flight = agent finished; run ghops next. Anything else means
	// the agent failed and there's nothing to publish.
	if oc.Status != state.StatusInFlight {
		return oc, nil
	}

	pub := r.NewPublisher(ghClient, ts, repoCfg)
	prs := map[string]digest.PRInfo{}
	merged := map[string]bool{}
	if err := r.publishOutcome(ctx, pub, repoCfg, oc, prs, merged); err != nil {
		oc.Status = state.StatusFailed
		oc.Error = err.Error()
	}
	return oc, nil
}

// AggregateInputs is what the aggregate subcommand consumes — the JSON
// shape produced by writing TriageResult + each DispatchOne Outcome to
// disk and reading them back in.
type AggregateInputs struct {
	GeneratedAt    time.Time          `json:"generated_at"`
	Outcomes       []dispatch.Outcome `json:"outcomes"`
	DryRunOutcomes []dispatch.Outcome `json:"dry_run_outcomes"`
	// PRInfo and merged-branch maps are reconstructed from outcomes —
	// each successful publish writes a PR number/URL into the outcome's
	// AgentResult metadata. The aggregator extracts and merges them.
}

// AggregateDigest renders the same digest format Run produces, but from
// per-task Outcomes collected by the matrix. It writes the digest file
// and returns its path. Stats / Requeued are best-effort placeholders
// — the per-task workflow doesn't track aggregate budget.
func (r *Runner) AggregateDigest(ai AggregateInputs) (string, error) {
	r.applyDefaults()

	all := append([]dispatch.Outcome{}, ai.Outcomes...)
	all = append(all, ai.DryRunOutcomes...)

	prs := map[string]digest.PRInfo{}
	mergedBranches := map[string]bool{}
	for _, oc := range all {
		// Outcome doesn't carry PR info post-publishOutcome in its public
		// fields; if we want richer data, the dispatch-one subcommand
		// needs to populate Outcome.AgentResult.Metadata. For now, the
		// digest renders without per-PR links from the matrix path —
		// the legacy Run flow remains the canonical PR-rich digest.
		_ = oc
	}

	rr := digest.Render(digest.Input{
		RunDate:        ai.GeneratedAt,
		Outcomes:       all,
		PRs:            prs,
		MergedBranches: mergedBranches,
		Stats:          budget.Stats{},
	})

	if err := os.MkdirAll(r.Config.Paths.DigestDir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(r.Config.Paths.DigestDir, digest.FilenameFor(ai.GeneratedAt))
	if err := os.WriteFile(path, []byte(rr), 0o644); err != nil {
		return "", err
	}
	return path, nil
}

// buildGitHubClient is a small helper extracted so Triage and DispatchOne
// can share the auth wiring with runRepo. Returns (client, tokenSource,
// error). If no App credentials are configured, returns an unauthenticated
// client with a nil TokenSource — caller must treat ts==nil as a signal
// to skip ghops operations.
func (r *Runner) buildGitHubClient(ctx context.Context) (*github.Client, ghops.TokenSource, error) {
	if r.Config.GitHub.AppID == 0 {
		return r.NewGitHub(nil), nil, nil
	}
	_, httpc, err := r.AuthForRepo(ctx, r.Config.GitHub)
	if err != nil {
		return nil, nil, err
	}
	ts2, _, _ := r.AuthForRepo(ctx, r.Config.GitHub)
	return r.NewGitHub(httpc), ts2, nil
}

