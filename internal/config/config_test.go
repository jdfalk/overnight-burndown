package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// happyConfig returns a YAML string that should parse + validate cleanly when
// the local_path it references exists. Tests can mutate the bytes to inject
// specific failure modes.
func happyConfig(localPath string) string {
	return `
anthropic:
  triage_model: claude-opus-4-7
  implementer_model: claude-haiku-4-5-20251001
  api_key_env: ANTHROPIC_API_KEY

github:
  app_id: 123
  installation_id: 456
  private_key_path: ` + localPath + `/key.pem

paths:
  state_dir: /tmp/burndown
  worktree_root: /tmp/burndown/worktrees
  digest_dir: /tmp
  audit_dir: /tmp/burndown/audit
  log_dir: /tmp/burndown/logs

budget:
  max_dollars: 5.0
  max_wall_seconds: 7200
  abort_threshold: 0.8

concurrency:
  max_parallel_agents: 4

defaults:
  mode: dry-run
  ci_watch_timeout_seconds: 1800
  diff_size_cap_lines: 200
  task_priority: cheap-first
  auto_merge_paths: ["*.md", "tests/**"]
  forced_review_paths: [".github/workflows/**"]

repos:
  - name: testrepo
    owner: jdfalk
    local_path: ` + localPath + `
`
}

func writeKey(t *testing.T, dir string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "key.pem"), []byte("not-a-real-key"), 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
}

// ---------------------------------------------------------------------------
// happy path
// ---------------------------------------------------------------------------

func TestLoad_HappyPath(t *testing.T) {
	td := t.TempDir()
	writeKey(t, td)
	c, err := LoadBytes([]byte(happyConfig(td)))
	if err != nil {
		t.Fatalf("expected clean load, got: %v", err)
	}
	if c.Anthropic.TriageModel != "claude-opus-4-7" {
		t.Errorf("triage_model: %q", c.Anthropic.TriageModel)
	}
	if len(c.Repos) != 1 || c.Repos[0].Name != "testrepo" {
		t.Errorf("repos parsed wrong: %+v", c.Repos)
	}
	// Defaults inheritance: repo had no explicit mode, should pick up dry-run.
	if c.Repos[0].Mode != ModeDryRun {
		t.Errorf("repo.Mode default not applied: got %q", c.Repos[0].Mode)
	}
	// Defaults inheritance: ci_watch_timeout flowed down.
	if c.Repos[0].CIWatchTimeoutSeconds != 1800 {
		t.Errorf("repo CIWatchTimeoutSeconds default not applied: got %d",
			c.Repos[0].CIWatchTimeoutSeconds)
	}
	// auto_merge_paths copied from defaults.
	if len(c.Repos[0].AutoMergePaths) != 2 {
		t.Errorf("repo AutoMergePaths default not applied: got %v", c.Repos[0].AutoMergePaths)
	}
}

func TestLoad_ExplicitRepoOverridesDefault(t *testing.T) {
	td := t.TempDir()
	writeKey(t, td)
	yaml := strings.Replace(happyConfig(td),
		"local_path: "+td,
		"local_path: "+td+"\n    mode: draft-only\n    ci_watch_timeout_seconds: 600",
		1)
	c, err := LoadBytes([]byte(yaml))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if c.Repos[0].Mode != ModeDraftOnly {
		t.Errorf("expected draft-only override, got %q", c.Repos[0].Mode)
	}
	if c.Repos[0].CIWatchTimeoutSeconds != 600 {
		t.Errorf("expected 600s override, got %d", c.Repos[0].CIWatchTimeoutSeconds)
	}
}

// ---------------------------------------------------------------------------
// tilde expansion
// ---------------------------------------------------------------------------

func TestLoad_TildeExpansion(t *testing.T) {
	td := t.TempDir()
	writeKey(t, td)
	yaml := strings.Replace(happyConfig(td),
		"state_dir: /tmp/burndown",
		"state_dir: ~/burndown-state",
		1)
	c, err := LoadBytes([]byte(yaml))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	home, _ := os.UserHomeDir()
	want := filepath.Join(home, "burndown-state")
	if c.Paths.StateDir != want {
		t.Errorf("StateDir not expanded: got %q want %q", c.Paths.StateDir, want)
	}
}

// ---------------------------------------------------------------------------
// reject: typo in key (KnownFields catches it)
// ---------------------------------------------------------------------------

func TestLoad_RejectsUnknownField(t *testing.T) {
	td := t.TempDir()
	writeKey(t, td)
	yaml := strings.Replace(happyConfig(td),
		"max_parallel_agents: 4",
		"max_parallel_agents: 4\n  max_parellel_agents: 4", // typo'd duplicate
		1)
	_, err := LoadBytes([]byte(yaml))
	if err == nil {
		t.Fatal("expected parse error from KnownFields strict mode")
	}
	if !strings.Contains(err.Error(), "max_parellel_agents") {
		t.Errorf("error should call out the unknown field, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// validation table
// ---------------------------------------------------------------------------

func TestValidate_RejectsBadValues(t *testing.T) {
	td := t.TempDir()
	writeKey(t, td)
	base := happyConfig(td)

	cases := []struct {
		name    string
		mutate  func(string) string
		wantSub string // substring expected somewhere in the joined error
	}{
		{
			"empty triage_model",
			func(s string) string { return strings.Replace(s, "claude-opus-4-7", "", 1) },
			"triage_model",
		},
		{
			"empty api_key_env",
			func(s string) string { return strings.Replace(s, "ANTHROPIC_API_KEY", "", 1) },
			"api_key_env",
		},
		{
			"abort_threshold out of range",
			func(s string) string { return strings.Replace(s, "abort_threshold: 0.8", "abort_threshold: 1.5", 1) },
			"abort_threshold",
		},
		{
			"max_dollars zero",
			func(s string) string { return strings.Replace(s, "max_dollars: 5.0", "max_dollars: 0", 1) },
			"max_dollars",
		},
		{
			"concurrency too high",
			func(s string) string { return strings.Replace(s, "max_parallel_agents: 4", "max_parallel_agents: 100", 1) },
			"capped at 16",
		},
		{
			"invalid mode",
			func(s string) string { return strings.Replace(s, "mode: dry-run", "mode: yolo", 1) },
			"defaults.mode",
		},
		{
			"invalid task_priority",
			func(s string) string { return strings.Replace(s, "task_priority: cheap-first", "task_priority: random", 1) },
			"task_priority",
		},
		{
			"empty repos",
			func(s string) string {
				return strings.Replace(s,
					"repos:\n  - name: testrepo\n    owner: jdfalk\n    local_path: "+td,
					"repos: []", 1)
			},
			"repos",
		},
		{
			"local_path missing on disk",
			func(s string) string {
				return strings.Replace(s,
					"local_path: "+td,
					"local_path: /nonexistent/burndown-test-path",
					1)
			},
			"local_path",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadBytes([]byte(tc.mutate(base)))
			if err == nil {
				t.Fatal("expected validation error")
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("error %v should contain %q", err, tc.wantSub)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// github auth required only when any repo is non-dry-run
// ---------------------------------------------------------------------------

func TestValidate_GitHubAuthRequiredOnlyWhenNonDryRun(t *testing.T) {
	td := t.TempDir()
	writeKey(t, td)

	// All dry-run + missing github auth → ok.
	yaml := strings.Replace(happyConfig(td),
		"github:\n  app_id: 123\n  installation_id: 456\n  private_key_path: "+td+"/key.pem",
		"github: {}",
		1)
	if _, err := LoadBytes([]byte(yaml)); err != nil {
		t.Fatalf("dry-run-only repos should not require github auth, got: %v", err)
	}

	// Same config but flip the repo to draft-only → should now fail.
	yaml2 := strings.Replace(yaml,
		"local_path: "+td,
		"local_path: "+td+"\n    mode: draft-only",
		1)
	_, err := LoadBytes([]byte(yaml2))
	if err == nil {
		t.Fatal("expected validation error: github auth required for non-dry-run")
	}
	if !strings.Contains(err.Error(), "github.app_id") {
		t.Errorf("error should call out missing github.app_id: %v", err)
	}
}

// ---------------------------------------------------------------------------
// duplicate repos
// ---------------------------------------------------------------------------

func TestValidate_RejectsDuplicateRepos(t *testing.T) {
	td := t.TempDir()
	writeKey(t, td)
	yaml := strings.Replace(happyConfig(td),
		"repos:\n  - name: testrepo\n    owner: jdfalk\n    local_path: "+td,
		"repos:\n  - name: testrepo\n    owner: jdfalk\n    local_path: "+td+
			"\n  - name: testrepo\n    owner: jdfalk\n    local_path: "+td,
		1)
	_, err := LoadBytes([]byte(yaml))
	if err == nil {
		t.Fatal("expected duplicate-repo error")
	}
	if !strings.Contains(err.Error(), "duplicate") {
		t.Errorf("error should call out duplicate: %v", err)
	}
}

// ---------------------------------------------------------------------------
// lookup helper
// ---------------------------------------------------------------------------

func TestRepoByOwnerName(t *testing.T) {
	td := t.TempDir()
	writeKey(t, td)
	c, err := LoadBytes([]byte(happyConfig(td)))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	r, ok := c.RepoByOwnerName("jdfalk", "testrepo")
	if !ok {
		t.Fatal("expected to find testrepo")
	}
	if r.Name != "testrepo" {
		t.Errorf("got %q", r.Name)
	}
	if _, ok := c.RepoByOwnerName("jdfalk", "nope"); ok {
		t.Error("did not expect to find nope")
	}
}

// ---------------------------------------------------------------------------
// joined errors surface ALL problems, not just the first
// ---------------------------------------------------------------------------

func TestValidate_JoinsMultipleErrors(t *testing.T) {
	td := t.TempDir()
	writeKey(t, td)
	// Three bad fields at once.
	yaml := happyConfig(td)
	yaml = strings.Replace(yaml, "max_dollars: 5.0", "max_dollars: 0", 1)
	yaml = strings.Replace(yaml, "abort_threshold: 0.8", "abort_threshold: 2.0", 1)
	yaml = strings.Replace(yaml, "task_priority: cheap-first", "task_priority: random", 1)

	_, err := LoadBytes([]byte(yaml))
	if err == nil {
		t.Fatal("expected validation errors")
	}
	for _, must := range []string{"max_dollars", "abort_threshold", "task_priority"} {
		if !strings.Contains(err.Error(), must) {
			t.Errorf("joined error missing %q: %v", must, err)
		}
	}
}
