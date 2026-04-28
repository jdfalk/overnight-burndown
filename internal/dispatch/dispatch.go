// Package dispatch fans out triaged tasks to the implementer agent in
// parallel, one git worktree per task, capped at MaxParallel concurrent
// agents.
//
// What this package does:
//   * Slug + dedup branch names from the triage agent's suggestions.
//   * Create a git worktree per task off the parent repo's HEAD.
//   * Spawn an MCP subprocess scoped to that worktree (or use the
//     injected SpawnMCP for tests).
//   * Run the implementer agent against the worktree.
//   * Capture an Outcome with status + agent summary + worktree path.
//   * On any failure, leave the worktree in place under
//     <worktree-root>/<repo>/<slug>/ so a human can inspect it the
//     next morning. Cleanup of old failed worktrees is a separate
//     concern (PLAN.md retains them for 7 days).
//
// What this package does NOT do:
//   * Open PRs. The driver-side gh ops (PR create, CI watch, merge)
//     land in step 9 and consume the Outcome list this package returns.
//   * Push to the remote. Same reason.
//   * Commit. The agent commits via MCP if its tools allow it; for
//     the burndown the agent's allowlist excludes git_*, so commit is
//     also a step-9 concern.
//
// In other words, after Dispatch returns, every successful Outcome has
// a worktree on disk with file edits but no commits — step 9 takes it
// from there.
package dispatch

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/anthropics/anthropic-sdk-go"
	"golang.org/x/sync/errgroup"

	"github.com/jdfalk/overnight-burndown/internal/agent"
	"github.com/jdfalk/overnight-burndown/internal/sources"
	"github.com/jdfalk/overnight-burndown/internal/state"
	"github.com/jdfalk/overnight-burndown/internal/triage"
)

// TaskWithDecision pairs a collected task with its triage decision. This
// is the per-task input the dispatcher consumes.
type TaskWithDecision struct {
	Task     sources.Task
	Decision triage.Decision
}

// Outcome is the per-task result. The driver folds these into state
// updates (in-flight ↔ shipped/draft/failed) after step 9 takes the
// worktree to a PR.
type Outcome struct {
	Task         sources.Task
	Decision     triage.Decision
	Status       state.Status   // in-flight on success, failed on error
	Branch       string         // empty if the task never made it to worktree creation
	WorktreePath string         // present even on failure for postmortem inspection
	AgentResult  *agent.Result  // present on success
	Error        string         // populated when Status == failed
}

// SpawnMCPFunc returns an MCPClient configured to act on the given
// worktree. The returned closer is called when the dispatcher is done
// with the client (typically a subprocess kill / pipe close).
type SpawnMCPFunc func(ctx context.Context, worktreePath string) (agent.MCPClient, func() error, error)

// RunAgentFunc runs the implementer agent against a prepared worktree.
// Production wiring uses agent.Run; tests inject a stub.
type RunAgentFunc func(ctx context.Context, opts agent.Options) (*agent.Result, error)

// Dispatcher fans tasks out to the implementer agent. Construct one per
// repo per night; safe to call Dispatch concurrently from multiple
// goroutines, though typical use is a single call per repo.
type Dispatcher struct {
	// Anthropic client + model passed through to agent.Run.
	AnthropicClient anthropic.Client
	Model           anthropic.Model

	// Repository specifics — where the parent repo lives and where to
	// drop new worktrees. RepoName is used for branch namespacing and
	// worktree path layout.
	RepoLocalPath string
	RepoName      string
	WorktreeRoot  string

	// MaxParallel caps concurrent agents. Defaults to 4 if 0.
	MaxParallel int

	// WorktreeExcludePaths are passed to AddWorktree to materialize each
	// worktree via non-cone sparse-checkout, omitting these directory
	// prefixes. Empty = full checkout (current behavior).
	WorktreeExcludePaths []string

	// RunAgent / SpawnMCP let tests override the slow paths. Defaults
	// are wired by NewDispatcher.
	RunAgent RunAgentFunc
	SpawnMCP SpawnMCPFunc
}

// Dispatch runs every task in parallel under the configured concurrency
// cap and returns one Outcome per input task in the same order.
//
// A task that fails at any stage (worktree creation, MCP spawn, agent
// run) yields a `failed` Outcome with the error captured; processing
// continues for the rest of the batch. Errors that affect the whole
// batch (context cancellation, pre-flight validation) are returned
// alongside whatever partial outcomes were produced.
func (d *Dispatcher) Dispatch(ctx context.Context, items []TaskWithDecision) ([]Outcome, error) {
	if d.RunAgent == nil {
		return nil, errors.New("dispatch: RunAgent is nil")
	}
	if d.SpawnMCP == nil {
		return nil, errors.New("dispatch: SpawnMCP is nil")
	}
	if d.RepoLocalPath == "" || d.WorktreeRoot == "" || d.RepoName == "" {
		return nil, errors.New("dispatch: RepoLocalPath, RepoName, WorktreeRoot are required")
	}
	if len(items) == 0 {
		return nil, nil
	}

	max := d.MaxParallel
	if max <= 0 {
		max = 4
	}

	branches := uniqueBranchNames(items)
	outcomes := make([]Outcome, len(items))

	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(max)
	var mu sync.Mutex // guards outcomes (errgroup goroutines can write in any order)

	for i := range items {
		i := i
		g.Go(func() error {
			out := d.runOne(gctx, items[i], branches[i])
			mu.Lock()
			outcomes[i] = out
			mu.Unlock()
			// Per-task failures are captured in Outcome.Status — never
			// returned from the goroutine. Returning here would cancel
			// the whole errgroup and skip the remaining tasks, which is
			// the opposite of what we want.
			return nil
		})
	}

	// Only context cancellation can produce an error from the group, so
	// surface it without dropping outcomes.
	if err := g.Wait(); err != nil {
		return outcomes, fmt.Errorf("dispatch: %w", err)
	}
	return outcomes, nil
}

// runOne handles a single task end-to-end. Returns an Outcome with
// Status == failed on any error, capturing the error message verbatim
// so the digest can show what went wrong.
func (d *Dispatcher) runOne(ctx context.Context, item TaskWithDecision, branch string) Outcome {
	out := Outcome{
		Task:     item.Task,
		Decision: item.Decision,
		Branch:   branch,
		Status:   state.StatusFailed,
	}

	wtPath := WorktreePath(d.WorktreeRoot, d.RepoName, branch)
	out.WorktreePath = wtPath

	wt, err := AddWorktree(ctx, d.RepoLocalPath, branch, wtPath, d.WorktreeExcludePaths...)
	if err != nil {
		out.Error = "create worktree: " + err.Error()
		return out
	}

	mcpClient, mcpClose, err := d.SpawnMCP(ctx, wt.Path)
	if err != nil {
		out.Error = "spawn MCP: " + err.Error()
		return out
	}
	defer func() { _ = mcpClose() }()

	res, err := d.RunAgent(ctx, agent.Options{
		Client:       d.AnthropicClient,
		MCP:          mcpClient,
		Model:        d.Model,
		Task:         item.Task,
		Decision:     item.Decision,
		Branch:       branch,
		WorktreeRoot: wt.Path,
	})
	if err != nil {
		out.Error = "agent run: " + err.Error()
		return out
	}

	out.AgentResult = res
	out.Status = state.StatusInFlight // agent finished; PR creation pending in step 9
	return out
}

// uniqueBranchNames produces a stable per-item branch name and ensures
// no two items collide. The triage agent suggests a branch in
// SuggestedBranch; we slugify and prepend a per-classification prefix.
// Collisions get a numeric suffix.
func uniqueBranchNames(items []TaskWithDecision) []string {
	seen := make(map[string]int, len(items))
	out := make([]string, len(items))
	for i, item := range items {
		base := branchSeed(item)
		if base == "" {
			base = SlugifyForBranch(item.Task.Source.Title)
		}
		name := base
		if n, ok := seen[base]; ok {
			n++
			seen[base] = n
			name = fmt.Sprintf("%s-%d", base, n)
		} else {
			seen[base] = 1
		}
		out[i] = name
	}
	return out
}

// branchSeed turns the triage decision's SuggestedBranch into a clean
// slug that already carries a classification prefix. The triage system
// prompt asks the model to use "auto/" for AUTO_MERGE_SAFE and "draft/"
// for NEEDS_REVIEW, so we honor that prefix when present.
func branchSeed(item TaskWithDecision) string {
	suggested := item.Decision.SuggestedBranch
	if suggested == "" {
		return ""
	}
	prefix := ""
	body := suggested
	for _, p := range []string{"auto/", "draft/", "feat/", "fix/"} {
		if len(body) > len(p) && body[:len(p)] == p {
			prefix = p
			body = body[len(p):]
			break
		}
	}
	slug := SlugifyForBranch(body)
	if prefix == "" {
		// Fall back to classification-based prefix.
		switch item.Decision.Classification {
		case triage.ClassAutoMergeSafe:
			prefix = "auto/"
		case triage.ClassNeedsReview:
			prefix = "draft/"
		default:
			prefix = "task/"
		}
	}
	return prefix + slug
}
