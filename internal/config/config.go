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
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Mode is the per-repo operating mode. Modes form a strict ordering of
// blast radius:
//
//   DryRun     — triage only; no worktrees, no PRs, no commits.
//   DraftOnly  — opens draft PRs; no CI watch, no merge.
//   Review     — opens ready-for-review (non-draft) PRs; no CI watch,
//                no merge. Use when you want a human to look at the PR
//                in the normal review queue without the bot auto-merging.
//   Full       — opens PR (draft if classification not SAFE), watches
//                CI, auto-merges SAFE classifications when green.
type Mode string

const (
	ModeDryRun    Mode = "dry-run"
	ModeDraftOnly Mode = "draft-only"
	ModeReview    Mode = "review"
	ModeFull      Mode = "full"
)

// validModes is the canonical set; lookup-friendly.
var validModes = map[Mode]struct{}{
	ModeDryRun:    {},
	ModeDraftOnly: {},
	ModeReview:    {},
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
	TaskHub     TaskHubConfig      `yaml:"task_hub"`
	Paths       PathsConfig        `yaml:"paths"`
	Budget      BudgetConfig       `yaml:"budget"`
	Concurrency ConcurrencyConfig  `yaml:"concurrency"`
	Defaults    Defaults           `yaml:"defaults"`
	Repos       []RepoConfig       `yaml:"repos"`
}

// TaskHubConfig identifies a central GitHub repository that holds task specs
// as Issues with routing labels. When set, the IssueCollector reads from this
// hub repo and filters by "{LabelPrefix}{repo.name}" instead of reading
// auto-ok issues from each target repo directly.
type TaskHubConfig struct {
	// Repo is "owner/name" of the hub repository, e.g. "jdfalk/burndown-tasks".
	Repo string `yaml:"repo"`
	// LabelPrefix is prepended to the target repo name to form the routing
	// label, e.g. "repo:" produces "repo:audiobook-organizer".
	LabelPrefix string `yaml:"label_prefix"`
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

// ModelTier pairs a model name with a complexity ceiling. When a task's
// EstComplexity (1–5 from triage) is ≤ MaxComplexity, that tier's model is
// selected. MaxComplexity == 0 means "no ceiling" — use this as the catch-all
// last tier. Tiers are evaluated in order; the first match wins.
//
// Example config:
//
//	model_tiers:
//	  - model: gpt-5.1-codex-mini
//	    max_complexity: 2
//	  - model: gpt-5.3-codex
//	    max_complexity: 4
//	  - model: gpt-5
//	    # no max_complexity → catch-all for complexity 5+
type ModelTier struct {
	Model         string `yaml:"model"`
	MaxComplexity int    `yaml:"max_complexity,omitempty"`
}

// LLMFeatureConfig picks an LLM provider + model for one feature
// (triage or implementer agent). Both fields are required when this
// block is present.
type LLMFeatureConfig struct {
	Provider ProviderName `yaml:"provider"`
	Model    string       `yaml:"model"`
	// ModelTiers selects a model based on task complexity (1–5 from triage).
	// The first tier whose MaxComplexity >= complexity (or MaxComplexity == 0)
	// wins. When empty, Model is used for all tasks. Only applies to the
	// OpenAI Responses path.
	ModelTiers []ModelTier `yaml:"model_tiers,omitempty"`
	// API selects the OpenAI endpoint to call. Empty / "responses" uses
	// /v1/responses (default). "chat-completions" uses the legacy
	// /v1/chat/completions path; kept while we soak the Responses
	// migration. Ignored when Provider != openai.
	API OpenAIAPIName `yaml:"api,omitempty"`
}

// SelectModel returns the best model for the given task complexity (1–5).
// It walks ModelTiers in declaration order; the first tier whose MaxComplexity
// is 0 (catch-all) or >= complexity is returned. Falls back to Model when
// ModelTiers is empty or no tier matches.
func (f LLMFeatureConfig) SelectModel(complexity int) string {
	for _, t := range f.ModelTiers {
		if t.MaxComplexity == 0 || complexity <= t.MaxComplexity {
			return t.Model
		}
	}
	return f.Model
}

// FallbacksFrom returns the models from tiers above the one selected for
// complexity, in ascending order. These are passed to RunOpenAIResponses as
// the runtime escalation chain — if the primary model exhausts its 429-retry
// budget, the next entry is tried while the conversation thread is preserved
// via PreviousResponseID.
//
// Returns nil when ModelTiers is empty, no tier matched, or the selected tier
// is already the highest (no tiers above it). In all those cases the caller
// runs with just the primary model and fails hard on retry exhaustion.
func (f LLMFeatureConfig) FallbacksFrom(complexity int) []string {
	if len(f.ModelTiers) == 0 {
		return nil
	}
	selectedIdx := -1
	for i, t := range f.ModelTiers {
		if t.MaxComplexity == 0 || complexity <= t.MaxComplexity {
			selectedIdx = i
			break
		}
	}
	if selectedIdx < 0 || selectedIdx >= len(f.ModelTiers)-1 {
		return nil
	}
	fallbacks := make([]string, 0, len(f.ModelTiers)-selectedIdx-1)
	for _, t := range f.ModelTiers[selectedIdx+1:] {
		fallbacks = append(fallbacks, t.Model)
	}
	return fallbacks
}

// OpenAIAPIName chooses which OpenAI endpoint we hit for a feature.
type OpenAIAPIName string

const (
	OpenAIAPIResponses       OpenAIAPIName = "responses"
	OpenAIAPIChatCompletions OpenAIAPIName = "chat-completions"
)

// GitHubConfig holds App-based authentication settings. All three resolved
// fields (AppID, InstallationID, PrivateKeyPath) are required for any repo
// not in dry-run mode. Supply values directly or via env-var lookups:
//
//	github:
//	  app_id_env: BURNDOWN_BOT_APP_ID
//	  installation_id_env: BURNDOWN_BOT_INSTALLATION_ID
//	  private_key_path: ~/.burndown/burndown-bot.pem
//	  # or supply the PEM content via env var; loader writes it to a temp file:
//	  private_key_env: BURNDOWN_BOT_PRIVATE_KEY
type GitHubConfig struct {
	AppID          int64  `yaml:"app_id"`
	InstallationID int64  `yaml:"installation_id"`
	PrivateKeyPath string `yaml:"private_key_path"`

	// Env-var alternatives; resolved during Load and merged into the above fields.
	AppIDEnv          string `yaml:"app_id_env,omitempty"`
	InstallationIDEnv string `yaml:"installation_id_env,omitempty"`
	PrivateKeyEnv     string `yaml:"private_key_env,omitempty"`
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
	c, err := parseNoValidate(data)
	if err != nil {
		return nil, err
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return c, nil
}

// parseNoValidate is the shared decode + defaults + path-expand pipeline
// without the Validate() check. The aggregate subcommand uses this because
// it operates on JSON outcomes from prior phases — local_path / private_key
// don't need to exist on the aggregate runner, only on the triage and
// dispatch runners.
func parseNoValidate(data []byte) (*Config, error) {
	var c Config
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("config: parse: %w", err)
	}
	if err := c.expandPaths(); err != nil {
		return nil, err
	}
	if err := c.resolveGitHubEnvVars(); err != nil {
		return nil, err
	}
	c.applyDefaults()
	return &c, nil
}

// resolveGitHubEnvVars merges env-var alternatives into the GitHubConfig
// integer/path fields. This supports GitHub Actions secrets without requiring
// the caller to write secret values into the YAML file.
func (c *Config) resolveGitHubEnvVars() error {
	gh := &c.GitHub
	if gh.AppIDEnv != "" && gh.AppID == 0 {
		raw := os.Getenv(gh.AppIDEnv)
		if raw != "" {
			id, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
			if err != nil {
				return fmt.Errorf("config: github.app_id_env %q: %w", gh.AppIDEnv, err)
			}
			gh.AppID = id
		}
	}
	if gh.InstallationIDEnv != "" && gh.InstallationID == 0 {
		raw := os.Getenv(gh.InstallationIDEnv)
		if raw != "" {
			id, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
			if err != nil {
				return fmt.Errorf("config: github.installation_id_env %q: %w", gh.InstallationIDEnv, err)
			}
			gh.InstallationID = id
		}
	}
	if gh.PrivateKeyEnv != "" && gh.PrivateKeyPath == "" {
		pem := os.Getenv(gh.PrivateKeyEnv)
		if pem != "" {
			home, _ := os.UserHomeDir()
			keyPath := filepath.Join(home, ".burndown", "burndown-bot.pem")
			if err := os.WriteFile(keyPath, []byte(pem), 0600); err != nil {
				return fmt.Errorf("config: writing private_key_env to %s: %w", keyPath, err)
			}
			gh.PrivateKeyPath = keyPath
		}
	}
	return nil
}

// LoadNoValidate reads a config file and applies defaults but skips the
// Validate() step that asserts on-disk paths exist. Use it for read-only
// commands like aggregate that consume already-collected outcomes and
// don't touch the source repos.
func LoadNoValidate(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: read %q: %w", path, err)
	}
	return parseNoValidate(data)
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
		// ModelTiers validation: each tier needs a model; catch-all (0) must
		// be last; no more than one catch-all.
		catchAllSeen := false
		for j, tier := range fc.f.ModelTiers {
			if catchAllSeen {
				errs = append(errs, fmt.Errorf("%s.model_tiers[%d]: tier after catch-all (max_complexity=0) is unreachable", fc.name, j))
			}
			if tier.Model == "" {
				errs = append(errs, fmt.Errorf("%s.model_tiers[%d].model: required", fc.name, j))
			}
			if tier.MaxComplexity == 0 {
				catchAllSeen = true
			} else if tier.MaxComplexity < 1 || tier.MaxComplexity > 5 {
				errs = append(errs, fmt.Errorf("%s.model_tiers[%d].max_complexity: must be 1–5 or 0 (catch-all), got %d", fc.name, j, tier.MaxComplexity))
			}
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
