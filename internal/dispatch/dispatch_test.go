package dispatch

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go"

	"github.com/jdfalk/overnight-burndown/internal/agent"
	"github.com/jdfalk/overnight-burndown/internal/mcp"
	"github.com/jdfalk/overnight-burndown/internal/sources"
	"github.com/jdfalk/overnight-burndown/internal/state"
	"github.com/jdfalk/overnight-burndown/internal/triage"
)

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// stubMCPClient is the in-test MCPClient. The dispatcher tests don't
// exercise tools at all (the agent stub never calls anything), so this
// just needs to satisfy the interface.
type stubMCPClient struct{}

func (s *stubMCPClient) ListTools(_ context.Context) ([]mcp.ToolDef, error) { return nil, nil }
func (s *stubMCPClient) CallTool(_ context.Context, _ string, _ any) (*mcp.CallResult, error) {
	return &mcp.CallResult{}, nil
}

// fixtureItem builds a TaskWithDecision with sensible defaults.
func fixtureItem(taskID, title string, cls triage.Classification, branch string) TaskWithDecision {
	return TaskWithDecision{
		Task: sources.Task{
			Source: state.Source{
				Type:        state.SourceIssue,
				Repo:        "jdfalk/x",
				URL:         taskID,
				ContentHash: "abc",
				Title:       title,
			},
			Body: title + " body",
		},
		Decision: triage.Decision{
			TaskID:          taskID,
			Classification:  cls,
			Reason:          "test",
			EstComplexity:   1,
			SuggestedBranch: branch,
		},
	}
}

// fixtureDispatcher returns a Dispatcher with hooks pre-wired to test stubs.
// `runAgent` and `spawnMCP` can be overridden by callers.
func fixtureDispatcher(t *testing.T, repoPath string,
	runAgent RunAgentFunc, spawnMCP SpawnMCPFunc) *Dispatcher {
	t.Helper()
	return &Dispatcher{
		AnthropicClient: anthropic.Client{},
		Model:           "claude-haiku-4-5",
		RepoLocalPath:   repoPath,
		RepoName:        "x",
		WorktreeRoot:    filepath.Join(t.TempDir(), "worktrees"),
		MaxParallel:     4,
		RunAgent:        runAgent,
		SpawnMCP:        spawnMCP,
	}
}

func defaultSpawnMCP() SpawnMCPFunc {
	return func(_ context.Context, _ string) (agent.MCPClient, func() error, error) {
		return &stubMCPClient{}, func() error { return nil }, nil
	}
}

// ---------------------------------------------------------------------------
// happy path: two tasks, both succeed
// ---------------------------------------------------------------------------

func TestDispatch_HappyPath(t *testing.T) {
	repo := makeRepo(t)

	var calls atomic.Int32
	runAgent := func(_ context.Context, opts agent.Options) (*agent.Result, error) {
		calls.Add(1)
		return &agent.Result{
			Iterations:    2,
			StopReason:    anthropic.StopReasonEndTurn,
			Summary:       "edited " + opts.Task.Source.URL,
			ToolCallCount: 3,
		}, nil
	}

	d := fixtureDispatcher(t, repo, runAgent, defaultSpawnMCP())

	items := []TaskWithDecision{
		fixtureItem("https://example/1", "Fix typo", triage.ClassAutoMergeSafe, "auto/readme-typo"),
		fixtureItem("https://example/2", "Refactor X", triage.ClassNeedsReview, "draft/refactor-x"),
	}
	out, err := d.Dispatch(context.Background(), items)
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("expected 2 outcomes, got %d", len(out))
	}
	for i, o := range out {
		if o.Status != state.StatusInFlight {
			t.Errorf("outcome[%d] status: got %q want %q", i, o.Status, state.StatusInFlight)
		}
		if o.AgentResult == nil || o.AgentResult.Summary == "" {
			t.Errorf("outcome[%d] AgentResult missing summary: %+v", i, o.AgentResult)
		}
		if o.WorktreePath == "" {
			t.Errorf("outcome[%d] WorktreePath empty", i)
		}
	}
	if calls.Load() != 2 {
		t.Errorf("agent called %d times, want 2", calls.Load())
	}
}

// ---------------------------------------------------------------------------
// failure isolation: one task's failure must not abort the others
// ---------------------------------------------------------------------------

func TestDispatch_FailureIsolation(t *testing.T) {
	repo := makeRepo(t)

	runAgent := func(_ context.Context, opts agent.Options) (*agent.Result, error) {
		if opts.Task.Source.URL == "https://example/2" {
			return nil, errors.New("simulated agent failure")
		}
		return &agent.Result{StopReason: anthropic.StopReasonEndTurn, Summary: "ok"}, nil
	}

	d := fixtureDispatcher(t, repo, runAgent, defaultSpawnMCP())
	items := []TaskWithDecision{
		fixtureItem("https://example/1", "task 1", triage.ClassAutoMergeSafe, "auto/one"),
		fixtureItem("https://example/2", "task 2", triage.ClassAutoMergeSafe, "auto/two"),
		fixtureItem("https://example/3", "task 3", triage.ClassAutoMergeSafe, "auto/three"),
	}
	out, err := d.Dispatch(context.Background(), items)
	if err != nil {
		t.Fatalf("Dispatch: %v (the batch should succeed even if one task fails)", err)
	}

	wantStatus := []state.Status{state.StatusInFlight, state.StatusFailed, state.StatusInFlight}
	for i, w := range wantStatus {
		if out[i].Status != w {
			t.Errorf("outcome[%d]: got %q want %q", i, out[i].Status, w)
		}
	}
	if out[1].Error == "" || !contains(out[1].Error, "simulated agent failure") {
		t.Errorf("outcome[1] should carry the underlying error: %q", out[1].Error)
	}
}

// ---------------------------------------------------------------------------
// concurrency cap: never more than MaxParallel agents running at once
// ---------------------------------------------------------------------------

func TestDispatch_RespectsConcurrencyCap(t *testing.T) {
	repo := makeRepo(t)

	var (
		current atomic.Int32
		peak    atomic.Int32
	)
	updatePeak := func(v int32) {
		for {
			cur := peak.Load()
			if v <= cur || peak.CompareAndSwap(cur, v) {
				return
			}
		}
	}

	runAgent := func(_ context.Context, _ agent.Options) (*agent.Result, error) {
		v := current.Add(1)
		updatePeak(v)
		// Hold the slot long enough that other workers race to start.
		time.Sleep(80 * time.Millisecond)
		current.Add(-1)
		return &agent.Result{StopReason: anthropic.StopReasonEndTurn, Summary: "ok"}, nil
	}

	d := fixtureDispatcher(t, repo, runAgent, defaultSpawnMCP())
	d.MaxParallel = 2

	items := make([]TaskWithDecision, 6)
	for i := range items {
		items[i] = fixtureItem(
			"https://example/"+string(rune('a'+i)),
			"t",
			triage.ClassAutoMergeSafe,
			"auto/t-"+string(rune('a'+i)),
		)
	}
	if _, err := d.Dispatch(context.Background(), items); err != nil {
		t.Fatal(err)
	}
	if peak.Load() > 2 {
		t.Errorf("peak concurrency = %d, want <= 2", peak.Load())
	}
}

// ---------------------------------------------------------------------------
// SpawnMCP error propagates as a failed outcome (not a Dispatch error)
// ---------------------------------------------------------------------------

func TestDispatch_SpawnMCPFailure(t *testing.T) {
	repo := makeRepo(t)
	runAgent := func(_ context.Context, _ agent.Options) (*agent.Result, error) {
		t.Error("agent should not be called when MCP spawn fails")
		return nil, nil
	}
	spawnMCP := func(_ context.Context, _ string) (agent.MCPClient, func() error, error) {
		return nil, nil, errors.New("safe-ai-util-mcp not in PATH")
	}
	d := fixtureDispatcher(t, repo, runAgent, spawnMCP)

	out, err := d.Dispatch(context.Background(), []TaskWithDecision{
		fixtureItem("https://example/1", "x", triage.ClassAutoMergeSafe, "auto/x"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if out[0].Status != state.StatusFailed {
		t.Errorf("status: got %q want failed", out[0].Status)
	}
	if !contains(out[0].Error, "spawn MCP") {
		t.Errorf("error should mention MCP spawn: %q", out[0].Error)
	}
}

// ---------------------------------------------------------------------------
// MCP closer is always called, even when agent fails
// ---------------------------------------------------------------------------

func TestDispatch_MCPCloserCalledOnAgentFailure(t *testing.T) {
	repo := makeRepo(t)

	var (
		mu        sync.Mutex
		closeHits int
	)
	spawnMCP := func(_ context.Context, _ string) (agent.MCPClient, func() error, error) {
		return &stubMCPClient{}, func() error {
			mu.Lock()
			closeHits++
			mu.Unlock()
			return nil
		}, nil
	}
	runAgent := func(_ context.Context, _ agent.Options) (*agent.Result, error) {
		return nil, errors.New("boom")
	}
	d := fixtureDispatcher(t, repo, runAgent, spawnMCP)

	if _, err := d.Dispatch(context.Background(), []TaskWithDecision{
		fixtureItem("https://example/1", "x", triage.ClassAutoMergeSafe, "auto/x"),
	}); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	defer mu.Unlock()
	if closeHits != 1 {
		t.Errorf("MCP closer hit %d times, want 1", closeHits)
	}
}

// ---------------------------------------------------------------------------
// branch deduplication when triage produces colliding suggestions
// ---------------------------------------------------------------------------

func TestUniqueBranchNames(t *testing.T) {
	items := []TaskWithDecision{
		{Decision: triage.Decision{Classification: triage.ClassAutoMergeSafe, SuggestedBranch: "auto/foo"}},
		{Decision: triage.Decision{Classification: triage.ClassAutoMergeSafe, SuggestedBranch: "auto/foo"}},
		{Decision: triage.Decision{Classification: triage.ClassNeedsReview, SuggestedBranch: "draft/bar"}},
		{Decision: triage.Decision{Classification: triage.ClassAutoMergeSafe, SuggestedBranch: ""},
			Task: sources.Task{Source: state.Source{Title: "Empty branch hint"}}},
	}
	got := uniqueBranchNames(items)
	if got[0] != "auto/foo" || got[1] != "auto/foo-2" {
		t.Errorf("collision dedup: %v", got)
	}
	if got[2] != "draft/bar" {
		t.Errorf("non-colliding branch lost: %q", got[2])
	}
	if got[3] == "" || got[3] == got[0] {
		t.Errorf("empty SuggestedBranch should fall back to title-derived slug, got %q", got[3])
	}
}

// ---------------------------------------------------------------------------
// branchSeed prefix handling
// ---------------------------------------------------------------------------

func TestBranchSeed(t *testing.T) {
	cases := []struct {
		name     string
		item     TaskWithDecision
		want     string
	}{
		{
			"honors auto/ prefix from triage",
			TaskWithDecision{Decision: triage.Decision{Classification: triage.ClassAutoMergeSafe, SuggestedBranch: "auto/Fix Typo"}},
			"auto/fix-typo",
		},
		{
			"honors draft/ prefix from triage",
			TaskWithDecision{Decision: triage.Decision{Classification: triage.ClassNeedsReview, SuggestedBranch: "draft/Refactor"}},
			"draft/refactor",
		},
		{
			"adds auto/ when no prefix and SAFE",
			TaskWithDecision{Decision: triage.Decision{Classification: triage.ClassAutoMergeSafe, SuggestedBranch: "fix-stuff"}},
			"auto/fix-stuff",
		},
		{
			"adds draft/ when no prefix and NEEDS_REVIEW",
			TaskWithDecision{Decision: triage.Decision{Classification: triage.ClassNeedsReview, SuggestedBranch: "rework-thing"}},
			"draft/rework-thing",
		},
		{
			"empty suggestion → empty seed (caller falls back to title)",
			TaskWithDecision{Decision: triage.Decision{SuggestedBranch: ""}},
			"",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := branchSeed(tc.item); got != tc.want {
				t.Errorf("branchSeed(%+v) = %q, want %q", tc.item, got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// guard: empty input is a no-op
// ---------------------------------------------------------------------------

func TestDispatch_EmptyInput(t *testing.T) {
	repo := makeRepo(t)
	d := fixtureDispatcher(t, repo,
		func(context.Context, agent.Options) (*agent.Result, error) {
			t.Error("agent should not be called for empty input")
			return nil, nil
		},
		defaultSpawnMCP())

	out, err := d.Dispatch(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if out != nil {
		t.Errorf("expected nil outcomes, got %v", out)
	}
}

// ---------------------------------------------------------------------------
// guard: missing required fields
// ---------------------------------------------------------------------------

func TestDispatch_RequiresHooks(t *testing.T) {
	d := &Dispatcher{
		RepoLocalPath: "/tmp/x",
		RepoName:      "x",
		WorktreeRoot:  "/tmp/wt",
		// RunAgent + SpawnMCP missing
	}
	_, err := d.Dispatch(context.Background(), []TaskWithDecision{
		fixtureItem("u", "t", triage.ClassAutoMergeSafe, "auto/t"),
	})
	if err == nil {
		t.Fatal("expected validation error for missing hooks")
	}
}

// ---------------------------------------------------------------------------
// helpers shared with worktree_test.go's makeRepo
// ---------------------------------------------------------------------------

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
