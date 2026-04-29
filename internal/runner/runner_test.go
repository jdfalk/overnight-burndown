package runner

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/google/go-github/v84/github"

	"github.com/jdfalk/overnight-burndown/internal/config"
	"github.com/jdfalk/overnight-burndown/internal/state"
	"github.com/jdfalk/overnight-burndown/internal/triage"
)

// fixtureRepo creates a minimal local repo with a TODO.md and returns its path.
func fixtureRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "TODO.md"), []byte(`# TODO

- [ ] [auto-ok] Fix typo in README
- [ ] Refactor the dispatcher
`), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// fakeIssuesAPI returns a github.Client wired to an httptest server
// that serves an empty issue list.
func fakeIssuesAPI(t *testing.T) (*github.Client, func()) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("[]"))
	}))
	c := github.NewClient(nil)
	u, _ := url.Parse(srv.URL + "/")
	c.BaseURL = u
	return c, srv.Close
}

// fakeAnthropic returns a server that replies with a triage tool-use call.
func fakeAnthropic(t *testing.T, decisions []map[string]any) (string, func()) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		input := map[string]any{"decisions": decisions}
		inputJSON, _ := json.Marshal(input)
		resp := map[string]any{
			"id":      "msg",
			"type":    "message",
			"role":    "assistant",
			"model":   "claude-opus-4-7",
			"content": []map[string]any{{
				"type":  "tool_use",
				"id":    "toolu_1",
				"name":  "record_classifications",
				"input": json.RawMessage(inputJSON),
			}},
			"stop_reason": "tool_use",
			"usage":       map[string]any{"input_tokens": 100, "output_tokens": 50},
		}
		out, _ := json.Marshal(resp)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(out)
	}))
	return srv.URL, srv.Close
}

// minimalConfig returns a Config that's ready for runner.Run with no
// non-dry-run repos (so no GitHub auth is required).
func minimalConfig(t *testing.T, stateDir, digestDir, repoPath string) config.Config {
	t.Helper()
	return config.Config{
		Anthropic: config.AnthropicConfig{
			TriageModel:      "claude-opus-4-7",
			ImplementerModel: "claude-haiku-4-5",
			APIKeyEnv:        "ANTHROPIC_API_KEY",
		},
		Paths: config.PathsConfig{
			StateDir:     stateDir,
			WorktreeRoot: filepath.Join(stateDir, "worktrees"),
			DigestDir:    digestDir,
			AuditDir:     filepath.Join(stateDir, "audit"),
			LogDir:       filepath.Join(stateDir, "logs"),
		},
		Budget: config.BudgetConfig{
			MaxDollars:     5.0,
			MaxWallSeconds: 7200,
			AbortThreshold: 0.8,
		},
		Concurrency: config.ConcurrencyConfig{MaxParallelAgents: 4},
		Defaults: config.Defaults{
			Mode:                  config.ModeDryRun,
			CIWatchTimeoutSeconds: 1800,
			DiffSizeCapLines:      200,
			TaskPriority:          config.PriorityCheapFirst,
			AutoMergePaths:        []string{"*.md"},
		},
		Repos: []config.RepoConfig{{
			Name:      "x",
			Owner:     "jdfalk",
			LocalPath: repoPath,
			Mode:      config.ModeDryRun,
		}},
	}
}

// ---------------------------------------------------------------------------
// dry-run end-to-end: collects, triages, records decisions, renders digest
// ---------------------------------------------------------------------------

func TestRun_DryRun_EndToEnd(t *testing.T) {
	repoPath := fixtureRepo(t)
	tmp := t.TempDir()

	// Stub Anthropic with two decisions matching the two TODO items.
	anthropicURL, cleanupA := fakeAnthropic(t, []map[string]any{
		{
			"task_id":          repoPath + "/TODO.md#L3",
			"classification":   "AUTO_MERGE_SAFE",
			"reason":           "doc-only typo",
			"est_complexity":   1,
			"suggested_branch": "auto/readme-typo",
		},
		{
			"task_id":          repoPath + "/TODO.md#L4",
			"classification":   "NEEDS_REVIEW",
			"reason":           "refactor — needs human review",
			"est_complexity":   3,
			"suggested_branch": "draft/refactor-dispatcher",
		},
	})
	defer cleanupA()

	// Stub GitHub issues API (empty list).
	ghClient, cleanupG := fakeIssuesAPI(t)
	defer cleanupG()

	cfg := minimalConfig(t, filepath.Join(tmp, "state"), filepath.Join(tmp, "digests"), repoPath)
	st := state.New()

	r := &Runner{
		Config:    cfg,
		State:     st,
		Anthropic: anthropic.NewClient(option.WithAPIKey("test"), option.WithBaseURL(anthropicURL)),
		Triager:   triage.NewTriager("claude-opus-4-7", option.WithAPIKey("test"), option.WithBaseURL(anthropicURL)),
		Now:       func() time.Time { return time.Date(2026, 4, 25, 23, 0, 0, 0, time.UTC) },
		NewGitHub: func(_ *http.Client) *github.Client { return ghClient },
	}

	t.Setenv("ANTHROPIC_API_KEY", "test")

	res, err := r.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Two outcomes (one per TODO item), both marked Blocked because dry-run mode.
	if len(res.Outcomes) != 2 {
		t.Fatalf("expected 2 outcomes, got %d", len(res.Outcomes))
	}

	// Digest written to the configured directory.
	wantPath := filepath.Join(tmp, "digests", "burndown-digest-2026-04-25.md")
	if res.DigestPath != wantPath {
		t.Errorf("DigestPath: got %q want %q", res.DigestPath, wantPath)
	}
	body, err := os.ReadFile(wantPath)
	if err != nil {
		t.Fatalf("read digest: %v", err)
	}
	for _, want := range []string{
		"# Burndown digest — 2026-04-25",
		"## TL;DR",
		"## Spend",
	} {
		if !strings.Contains(string(body), want) {
			t.Errorf("digest missing %q:\n%s", want, body)
		}
	}

	// State written as per-task files under state/tasks/ + a meta.json.
	if _, err := os.Stat(filepath.Join(tmp, "state", "meta.json")); err != nil {
		t.Errorf("state meta.json not written: %v", err)
	}
	tasksDir := filepath.Join(tmp, "state", "tasks")
	ents, err := os.ReadDir(tasksDir)
	if err != nil {
		t.Errorf("state tasks dir missing: %v", err)
	} else if len(ents) == 0 {
		t.Errorf("state tasks dir is empty")
	}
}

// ---------------------------------------------------------------------------
// PAUSE file aborts before any work happens
// ---------------------------------------------------------------------------

func TestRun_PauseFileAborts(t *testing.T) {
	repoPath := fixtureRepo(t)
	tmp := t.TempDir()
	stateDir := filepath.Join(tmp, "state")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, "PAUSE"), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := minimalConfig(t, stateDir, filepath.Join(tmp, "digests"), repoPath)
	r := &Runner{Config: cfg, State: state.New()}

	_, err := r.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "PAUSE") {
		t.Errorf("expected PAUSE abort, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Lock contention: a second concurrent Run blocks
// ---------------------------------------------------------------------------

func TestRun_LockContention(t *testing.T) {
	repoPath := fixtureRepo(t)
	tmp := t.TempDir()
	cfg := minimalConfig(t, filepath.Join(tmp, "state"), filepath.Join(tmp, "digests"), repoPath)
	if err := os.MkdirAll(cfg.Paths.StateDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// First lock (held).
	release, err := state.AcquireLock(filepath.Join(cfg.Paths.StateDir, "run.lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	r := &Runner{Config: cfg, State: state.New()}
	_, err = r.Run(context.Background())
	if err == nil {
		t.Fatal("expected lock-contention error")
	}
	if !strings.Contains(err.Error(), "lock") {
		t.Errorf("error should mention lock: %v", err)
	}
}

// ---------------------------------------------------------------------------
// helper bot-author utilities are deterministic
// ---------------------------------------------------------------------------

func TestBotAuthorEmail(t *testing.T) {
	got := botAuthorEmail(config.GitHubConfig{AppID: 1234567})
	want := "1234567+jdfalk-burndown-bot[bot]@users.noreply.github.com"
	if got != want {
		t.Errorf("got %q want %q", got, want)
	}
}
