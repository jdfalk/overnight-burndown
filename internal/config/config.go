// Package config defines the burndown driver's YAML configuration schema and
// the loader that parses + validates it.
//
// A typical config file:
//
//	anthropic:
//	  triage_model: claude-opus-4-7
//	  implementer_model: claude-haiku-4-5-20251001
//	  api_key_env: ANTHROPIC_API_KEY
//
//	github:
//	  app_id: 1234567
//	  installation_id: 7654321
//	  private_key_path: ~/.burndown/burndown-bot.pem
//
//	paths:
//	  state_dir: ~/.burndown
//	  worktree_root: ~/.burndown/worktrees
//	  digest_dir: ~
//
//	budget:
//	  max_dollars: 5.00
//	  max_wall_seconds: 7200
//	  abort_threshold: 0.80
//
//	concurrency:
//	  max_parallel_agents: 4
//
//	defaults:
//	  mode: dry-run
//	  ci_watch_timeout_seconds: 1800
//	  diff_size_cap_lines: 200
//	  task_priority: cheap-first
//	  auto_merge_paths: ["*.md", "CHANGELOG*", "tests/**", "**/*_test.go"]
//	  forced_review_paths: [".github/workflows/**", "**/migrations/**"]
//
//	repos:
//	  - name: audiobook-organizer
//	    owner: jdfalk
//	    local_path: ~/repos/github.com/jdfalk/audiobook-organizer
//	    mode: dry-run
//
// Path values that begin with "~/" are expanded against $HOME on Load.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// Mode is the per-repo operating mode. Modes form a strict ordering of
// blast radius: DryRun < DraftOnly < Full.
type Mode string

const (
	ModeDryRun    Mode = "dry-run"
	ModeDraftOnly Mode = "draft-only"
	ModeFull      Mode = "full"
)

// validModes is the canonical set; lookup-friendly.
var validModes = map[Mode]struct{}{
	ModeDryRun:    {},
	ModeDraftOnly: {},
	ModeFull:      {},
}

// Priority is the queue ordering policy under budget pressure.
type Priority string

const (
	PriorityCheapFirst Priority = "cheap-first"
	PriorityFIFO       Priority = "fifo"
	PriorityLabelFirst Priority = "label-first"
)

var validPriorities = map[Priority]struct{}{
	PriorityCheapFirst: {},
	PriorityFIFO:       {},
	PriorityLabelFirst: {},
}

// Config is the top-level YAML schema.
type Config struct {
	Anthropic   AnthropicConfig   `yaml:"anthropic"`
	GitHub      GitHubConfig      `yaml:"github"`
	Paths       PathsConfig       `yaml:"paths"`
	Budget      BudgetConfig      `yaml:"budget"`
	Concurrency ConcurrencyConfig `yaml:"concurrency"`
	Defaults    Defaults          `yaml:"defaults"`
	Repos       []RepoConfig      `yaml:"repos"`
}

// AnthropicConfig is the LLM provider settings.
type AnthropicConfig struct {
	TriageModel      string `yaml:"triage_model"`
	ImplementerModel string `yaml:"implementer_model"`
	// APIKeyEnv is the name of the env var holding the API key. The validator
	// checks that the name is set; the runtime checks that the var is populated.
	APIKeyEnv string `yaml:"api_key_env"`
}

// GitHubConfig holds App-based authentication settings. All three fields are
// required for any repo not in dry-run mode.
type GitHubConfig struct {
	AppID          int64  `yaml:"app_id"`
	InstallationID int64  `yaml:"installation_id"`
	PrivateKeyPath string `yaml:"private_key_path"`
}

// PathsConfig is where on disk burndown writes state, audit, logs, etc.
// All paths support ~ expansion against $HOME.
type PathsConfig struct {
	StateDir     string `yaml:"state_dir"`
	WorktreeRoot string `yaml:"worktree_root"`
	DigestDir    string `yaml:"digest_dir"`
	AuditDir     string `yaml:"audit_dir"`
	LogDir       string `yaml:"log_dir"`
}

// BudgetConfig caps per-night spend. Either trigger aborts the run cleanly.
type BudgetConfig struct {
	MaxDollars      float64 `yaml:"max_dollars"`
	MaxWallSeconds  int     `yaml:"max_wall_seconds"`
	AbortThreshold  float64 `yaml:"abort_threshold"`
}

// ConcurrencyConfig caps parallelism.
type ConcurrencyConfig struct {
	MaxParallelAgents int `yaml:"max_parallel_agents"`
}

// Defaults are inherited by repos that don't override them.
type Defaults struct {
	Mode                  Mode     `yaml:"mode"`
	CIWatchTimeoutSeconds int      `yaml:"ci_watch_timeout_seconds"`
	DiffSizeCapLines      int      `yaml:"diff_size_cap_lines"`
	TaskPriority          Priority `yaml:"task_priority"`
	AutoMergePaths        []string `yaml:"auto_merge_paths"`
	ForcedReviewPaths     []string `yaml:"forced_review_paths"`
}

// RepoConfig is a single managed repository. Fields left at their zero value
// inherit from Defaults.
type RepoConfig struct {
	Name                  string   `yaml:"name"`
	Owner                 string   `yaml:"owner"`
	LocalPath             string   `yaml:"local_path"`
	Mode                  Mode     `yaml:"mode,omitempty"`
	TestCommand           string   `yaml:"test_command,omitempty"`
	CIWatchTimeoutSeconds int      `yaml:"ci_watch_timeout_seconds,omitempty"`
	AutoMergePaths        []string `yaml:"auto_merge_paths,omitempty"`
	PolicyOverlayPath     string   `yaml:"policy_overlay_path,omitempty"`
}

// Load reads, parses, expands ~ in paths, applies defaults, and validates.
// Returns the first parse error verbatim; validation errors are joined into
// a single error with errors.Join so the caller sees all problems at once.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: read %q: %w", path, err)
	}
	return parse(data)
}

// LoadBytes is a test-friendly variant.
func LoadBytes(data []byte) (*Config, error) {
	return parse(data)
}

func parse(data []byte) (*Config, error) {
	var c Config
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true) // surface typos in keys
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("config: parse: %w", err)
	}
	if err := c.expandPaths(); err != nil {
		return nil, err
	}
	c.applyDefaults()
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

// expandPaths replaces leading "~/" or "~" in every path-shaped field with $HOME.
func (c *Config) expandPaths() error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("config: cannot determine home directory: %w", err)
	}
	expand := func(p *string) {
		*p = expandTilde(*p, home)
	}
	expand(&c.GitHub.PrivateKeyPath)
	expand(&c.Paths.StateDir)
	expand(&c.Paths.WorktreeRoot)
	expand(&c.Paths.DigestDir)
	expand(&c.Paths.AuditDir)
	expand(&c.Paths.LogDir)
	for i := range c.Repos {
		expand(&c.Repos[i].LocalPath)
		expand(&c.Repos[i].PolicyOverlayPath)
	}
	return nil
}

func expandTilde(p, home string) string {
	if p == "" {
		return p
	}
	if p == "~" {
		return home
	}
	if strings.HasPrefix(p, "~/") {
		return filepath.Join(home, p[2:])
	}
	return p
}

// applyDefaults fills repo fields from c.Defaults when left zero.
// Doesn't touch GitHub auth, paths, or budget — those have no per-repo override.
func (c *Config) applyDefaults() {
	for i := range c.Repos {
		r := &c.Repos[i]
		if r.Mode == "" {
			r.Mode = c.Defaults.Mode
		}
		if r.CIWatchTimeoutSeconds == 0 {
			r.CIWatchTimeoutSeconds = c.Defaults.CIWatchTimeoutSeconds
		}
		if len(r.AutoMergePaths) == 0 {
			r.AutoMergePaths = append([]string(nil), c.Defaults.AutoMergePaths...)
		}
	}
}

// Validate checks required fields, enum values, and cross-field invariants.
// All problems are joined into a single error so the caller sees the full list.
func (c *Config) Validate() error {
	var errs []error

	// --- anthropic ---
	if c.Anthropic.TriageModel == "" {
		errs = append(errs, errors.New("anthropic.triage_model: required"))
	}
	if c.Anthropic.ImplementerModel == "" {
		errs = append(errs, errors.New("anthropic.implementer_model: required"))
	}
	if c.Anthropic.APIKeyEnv == "" {
		errs = append(errs, errors.New("anthropic.api_key_env: required (env-var name, not the key itself)"))
	}

	// --- paths ---
	if c.Paths.StateDir == "" {
		errs = append(errs, errors.New("paths.state_dir: required"))
	}
	if c.Paths.WorktreeRoot == "" {
		errs = append(errs, errors.New("paths.worktree_root: required"))
	}
	if c.Paths.DigestDir == "" {
		errs = append(errs, errors.New("paths.digest_dir: required"))
	}

	// --- budget ---
	if c.Budget.MaxDollars <= 0 {
		errs = append(errs, errors.New("budget.max_dollars: must be > 0"))
	}
	if c.Budget.MaxWallSeconds <= 0 {
		errs = append(errs, errors.New("budget.max_wall_seconds: must be > 0"))
	}
	if c.Budget.AbortThreshold <= 0 || c.Budget.AbortThreshold >= 1 {
		errs = append(errs, fmt.Errorf("budget.abort_threshold: must be in (0,1), got %v", c.Budget.AbortThreshold))
	}

	// --- concurrency ---
	if c.Concurrency.MaxParallelAgents <= 0 {
		errs = append(errs, errors.New("concurrency.max_parallel_agents: must be > 0"))
	}
	if c.Concurrency.MaxParallelAgents > 16 {
		errs = append(errs, fmt.Errorf("concurrency.max_parallel_agents: capped at 16, got %d", c.Concurrency.MaxParallelAgents))
	}

	// --- defaults ---
	if _, ok := validModes[c.Defaults.Mode]; !ok {
		errs = append(errs, fmt.Errorf("defaults.mode: must be one of [dry-run, draft-only, full], got %q", c.Defaults.Mode))
	}
	if _, ok := validPriorities[c.Defaults.TaskPriority]; !ok {
		errs = append(errs, fmt.Errorf("defaults.task_priority: must be one of [cheap-first, fifo, label-first], got %q", c.Defaults.TaskPriority))
	}
	if c.Defaults.CIWatchTimeoutSeconds <= 0 {
		errs = append(errs, errors.New("defaults.ci_watch_timeout_seconds: must be > 0"))
	}
	if c.Defaults.DiffSizeCapLines <= 0 {
		errs = append(errs, errors.New("defaults.diff_size_cap_lines: must be > 0"))
	}

	// --- repos ---
	if len(c.Repos) == 0 {
		errs = append(errs, errors.New("repos: at least one repo required"))
	}

	anyNonDryRun := false
	seenName := make(map[string]bool, len(c.Repos))
	for i, r := range c.Repos {
		prefix := fmt.Sprintf("repos[%d] (%s/%s)", i, r.Owner, r.Name)
		if r.Name == "" {
			errs = append(errs, fmt.Errorf("%s.name: required", prefix))
		}
		if r.Owner == "" {
			errs = append(errs, fmt.Errorf("%s.owner: required", prefix))
		}
		if r.LocalPath == "" {
			errs = append(errs, fmt.Errorf("%s.local_path: required", prefix))
		} else if _, err := os.Stat(r.LocalPath); err != nil {
			errs = append(errs, fmt.Errorf("%s.local_path: %w", prefix, err))
		}
		if r.Name != "" {
			key := r.Owner + "/" + r.Name
			if seenName[key] {
				errs = append(errs, fmt.Errorf("%s: duplicate repo (must be unique by owner/name)", prefix))
			}
			seenName[key] = true
		}
		if _, ok := validModes[r.Mode]; !ok {
			errs = append(errs, fmt.Errorf("%s.mode: must be one of [dry-run, draft-only, full], got %q", prefix, r.Mode))
		}
		if r.Mode != ModeDryRun {
			anyNonDryRun = true
		}
		if r.PolicyOverlayPath != "" {
			if _, err := os.Stat(r.PolicyOverlayPath); err != nil {
				errs = append(errs, fmt.Errorf("%s.policy_overlay_path: %w", prefix, err))
			}
		}
	}

	// --- github auth: only required if any repo is non-dry-run ---
	if anyNonDryRun {
		if c.GitHub.AppID == 0 {
			errs = append(errs, errors.New("github.app_id: required when any repo is not in dry-run mode"))
		}
		if c.GitHub.InstallationID == 0 {
			errs = append(errs, errors.New("github.installation_id: required when any repo is not in dry-run mode"))
		}
		if c.GitHub.PrivateKeyPath == "" {
			errs = append(errs, errors.New("github.private_key_path: required when any repo is not in dry-run mode"))
		} else if _, err := os.Stat(c.GitHub.PrivateKeyPath); err != nil {
			errs = append(errs, fmt.Errorf("github.private_key_path: %w", err))
		}
	}

	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return nil
}

// RepoByOwnerName returns the named repo's config or false.
func (c *Config) RepoByOwnerName(owner, name string) (RepoConfig, bool) {
	for _, r := range c.Repos {
		if r.Owner == owner && r.Name == name {
			return r, true
		}
	}
	return RepoConfig{}, false
}
