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
//
// LLM-provider selection: each LLM-using feature (triage, implementer agent)
// picks its provider via its own block (`triage.provider`, `implementer.provider`).
// The provider's credentials live under `anthropic` or `openai` at the top
// level. A config can use Anthropic for one feature and OpenAI for the other;
// missing credentials for unused providers are tolerated.
//
// The legacy `anthropic.{triage_model,implementer_model}` shape from before the
// pluggable-providers refactor is still recognized — when `triage` /
// `implementer` blocks are absent the loader falls back to the legacy fields,
// using "anthropic" as the implicit provider.
type Config struct {
	Anthropic   AnthropicConfig    `yaml:"anthropic"`
	OpenAI      OpenAIConfig       `yaml:"openai"`
	Triage      LLMFeatureConfig   `yaml:"triage"`
	Implementer LLMFeatureConfig   `yaml:"implementer"`
	GitHub      GitHubConfig       `yaml:"github"`
	Paths       PathsConfig        `yaml:"paths"`
	Budget      BudgetConfig       `yaml:"budget"`
	Concurrency ConcurrencyConfig  `yaml:"concurrency"`
	Defaults    Defaults           `yaml:"defaults"`
	Repos       []RepoConfig       `yaml:"repos"`
}

// AnthropicConfig holds Anthropic credentials + the legacy model fields.
//
// `TriageModel` / `ImplementerModel` are deprecated — supply per-feature
// blocks (`triage`, `implementer`) instead. They are preserved here so
// existing configs keep working without edits.
type AnthropicConfig struct {
	APIKeyEnv string `yaml:"api_key_env"`
	BaseURL   string `yaml:"base_url,omitempty"`

	TriageModel      string `yaml:"triage_model,omitempty"`      // Deprecated: use triage.model
	ImplementerModel string `yaml:"implementer_model,omitempty"` // Deprecated: use implementer.model
}

// OpenAIConfig holds OpenAI / OpenAI-compatible credentials.
//
// BaseURL lets users point at any compatible endpoint (Azure OpenAI,
// OpenRouter, a self-hosted vLLM gateway, etc.) — the openai-go SDK
// honors it transparently.
type OpenAIConfig struct {
	APIKeyEnv string `yaml:"api_key_env"`
	BaseURL   string `yaml:"base_url,omitempty"`
}

// ProviderName identifies which LLM backend a feature should use.
type ProviderName string

const (
	ProviderAnthropic ProviderName = "anthropic"
	ProviderOpenAI    ProviderName = "openai"
)

// LLMFeatureConfig picks an LLM provider + model for one feature
// (triage or implementer agent). Both fields are required when this
// block is present.
type LLMFeatureConfig struct {
	Provider ProviderName `yaml:"provider"`
	Model    string       `yaml:"model"`
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

	// WorktreeExcludePaths are directory prefixes (relative to repo root) to
	// omit from worktree checkouts via non-cone sparse-checkout. Use this for
	// large fixtures (checked-in audio testdata, compiled artifacts) the
	// burndown bot doesn't need — it's the difference between a runner that
	// fits 15 worktrees in scratch and one that ENOSPCs at task 5.
	WorktreeExcludePaths []string `yaml:"worktree_exclude_paths,omitempty"`
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

// applyDefaults fills repo fields from c.Defaults when left zero, and
// fills the Triage/Implementer feature blocks from the legacy Anthropic
// fields when the new blocks are absent.
//
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

	// Legacy fallback: fill Triage from anthropic.triage_model.
	if c.Triage.Provider == "" && c.Anthropic.TriageModel != "" {
		c.Triage.Provider = ProviderAnthropic
		c.Triage.Model = c.Anthropic.TriageModel
	}
	// Legacy fallback: fill Implementer from anthropic.implementer_model.
	if c.Implementer.Provider == "" && c.Anthropic.ImplementerModel != "" {
		c.Implementer.Provider = ProviderAnthropic
		c.Implementer.Model = c.Anthropic.ImplementerModel
	}
}

// Validate checks required fields, enum values, and cross-field invariants.
// All problems are joined into a single error so the caller sees the full list.
func (c *Config) Validate() error {
	var errs []error

	// --- LLM features (triage + implementer) ---
	for _, fc := range []struct {
		name string
		f    LLMFeatureConfig
	}{
		{"triage", c.Triage},
		{"implementer", c.Implementer},
	} {
		if fc.f.Provider == "" {
			errs = append(errs, fmt.Errorf("%s.provider: required (one of: anthropic, openai)", fc.name))
			continue
		}
		switch fc.f.Provider {
		case ProviderAnthropic, ProviderOpenAI:
			// fine
		default:
			errs = append(errs, fmt.Errorf("%s.provider: must be 'anthropic' or 'openai', got %q", fc.name, fc.f.Provider))
			continue
		}
		if fc.f.Model == "" {
			errs = append(errs, fmt.Errorf("%s.model: required when provider is set", fc.name))
		}
	}

	// --- provider credentials: required if either feature uses that provider ---
	usesAnthropic := c.Triage.Provider == ProviderAnthropic || c.Implementer.Provider == ProviderAnthropic
	usesOpenAI := c.Triage.Provider == ProviderOpenAI || c.Implementer.Provider == ProviderOpenAI

	if usesAnthropic && c.Anthropic.APIKeyEnv == "" {
		errs = append(errs, errors.New("anthropic.api_key_env: required when triage or implementer uses provider=anthropic"))
	}
	if usesOpenAI && c.OpenAI.APIKeyEnv == "" {
		errs = append(errs, errors.New("openai.api_key_env: required when triage or implementer uses provider=openai"))
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
