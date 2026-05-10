// file: internal/config/fromenv.go
// version: 1.0.0
// guid: 3d4e5f6a-7b8c-9d0e-1f2a-3b4c5d6e7f80
//
// FromEnv builds a complete Config from well-known environment variables so
// callers (CI jobs) never need to render an intermediate config.yaml file.

package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// FromEnv builds a Config entirely from well-known environment variables.
// No config file is read or written. Validation is applied before returning
// so callers get the same invariant guarantees as Load().
//
// Required env:
//
//	MODE        - dry-run | draft-only | review | full
//	WORKSPACE   - GitHub Actions workspace root (used as repo local_path prefix)
//	RUNNER_TEMP - temp dir for state/digest/log paths
//
// Optional env (all have reasonable defaults):
//
//	REPO_NAME              - target repo name            (default: audiobook-organizer)
//	REPO_OWNER             - target repo owner           (default: jdfalk)
//	IMPLEMENTER_PROVIDER   - openai | anthropic          (default: openai)
//	IMPLEMENTER_MODEL      - override default model for the provider (skips tier table)
//	TRIAGE_MODEL           - OpenAI model for triage     (default: o4-mini)
//	CHEAPEST_ONLY          - 1|true|yes → cheapest single model, no tier escalation
//	POWERFUL_ONLY          - 1|true|yes → most powerful model only (wins over CHEAPEST_ONLY)
//	MAX_DOLLARS            - budget cap in USD           (default: 5.0 / 20.0 if POWERFUL_ONLY)
//	MAX_WALL_SECONDS       - wall-clock cap              (default: 3000 / 5400 if POWERFUL_ONLY)
//	WORKTREE_EXCLUDE_PATHS - comma-separated repo-relative paths to omit from worktrees
//
// GitHub App auth (required when any repo is not dry-run):
//
//	GH_APP_ID              - GitHub App numeric ID
//	GH_APP_INSTALLATION_ID - installation ID
//	GH_APP_PEM_PATH        - path to materialized PEM file
//	GH_APP_PRIVATE_KEY     - raw PEM content (alternative to GH_APP_PEM_PATH;
//	                         the loader writes it to a temp file automatically)
//
// API keys (read directly — no api_key_env YAML indirection needed):
//
//	OPENAI_API_KEY
//	ANTHROPIC_API_KEY
func FromEnv() (*Config, error) {
	cfg, err := fromEnvCore()
	if err != nil {
		return nil, err
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// FromEnvNoValidate is like FromEnv but skips Validate(). Use for
// read-only subcommands (e.g. aggregate) that don't require local paths
// to exist on the runner host.
func FromEnvNoValidate() (*Config, error) {
	return fromEnvCore()
}

func fromEnvCore() (*Config, error) {
	mode := Mode(strings.TrimSpace(os.Getenv("MODE")))
	workspace := strings.TrimSpace(os.Getenv("WORKSPACE"))
	runnerTemp := strings.TrimSpace(os.Getenv("RUNNER_TEMP"))

	var reqErrs []error
	if mode == "" {
		reqErrs = append(reqErrs, errors.New("MODE is required"))
	} else if _, ok := validModes[mode]; !ok {
		reqErrs = append(reqErrs, fmt.Errorf("MODE must be one of [dry-run, draft-only, review, full], got %q", mode))
	}
	if workspace == "" {
		reqErrs = append(reqErrs, errors.New("WORKSPACE is required"))
	}
	if runnerTemp == "" {
		reqErrs = append(reqErrs, errors.New("RUNNER_TEMP is required"))
	}
	if len(reqErrs) > 0 {
		return nil, fmt.Errorf("config fromenv: %w", errors.Join(reqErrs...))
	}

	repoName := envOrDefault("REPO_NAME", "audiobook-organizer")
	repoOwner := envOrDefault("REPO_OWNER", "jdfalk")
	implProvider := ProviderName(strings.ToLower(envOrDefault("IMPLEMENTER_PROVIDER", "openai")))
	triageModel := envOrDefault("TRIAGE_MODEL", "o4-mini")
	cheapestOnly := isTruthy(os.Getenv("CHEAPEST_ONLY"))
	powerfulOnly := isTruthy(os.Getenv("POWERFUL_ONLY"))
	if powerfulOnly {
		cheapestOnly = false
	}

	maxDollars := 5.0
	maxWallSecs := 3000
	if powerfulOnly {
		maxDollars = 20.0
		maxWallSecs = 5400
	}
	if v := strings.TrimSpace(os.Getenv("MAX_DOLLARS")); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return nil, fmt.Errorf("config fromenv: MAX_DOLLARS: %w", err)
		}
		maxDollars = f
	}
	if v := strings.TrimSpace(os.Getenv("MAX_WALL_SECONDS")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return nil, fmt.Errorf("config fromenv: MAX_WALL_SECONDS: %w", err)
		}
		maxWallSecs = n
	}

	cfg := &Config{
		OpenAI:      OpenAIConfig{APIKeyEnv: "OPENAI_API_KEY"},
		Anthropic:   AnthropicConfig{APIKeyEnv: "ANTHROPIC_API_KEY"},
		Triage:      LLMFeatureConfig{Provider: ProviderOpenAI, Model: triageModel},
		Implementer: implementerFromEnv(implProvider, cheapestOnly, powerfulOnly),
		GitHub: GitHubConfig{
			AppIDEnv:          "GH_APP_ID",
			InstallationIDEnv: "GH_APP_INSTALLATION_ID",
			PrivateKeyPath:    strings.TrimSpace(os.Getenv("GH_APP_PEM_PATH")),
			PrivateKeyEnv:     "GH_APP_PRIVATE_KEY",
		},
		Paths: PathsConfig{
			StateDir:     runnerTemp + "/burndown-state",
			WorktreeRoot: runnerTemp + "/burndown-state/worktrees",
			DigestDir:    runnerTemp + "/burndown-digest",
			AuditDir:     runnerTemp + "/burndown-state/audit",
			LogDir:       runnerTemp + "/burndown-state/logs",
		},
		Budget: BudgetConfig{
			MaxDollars:     maxDollars,
			MaxWallSeconds: maxWallSecs,
			AbortThreshold: 0.8,
		},
		Concurrency: ConcurrencyConfig{MaxParallelAgents: 4},
		Defaults: Defaults{
			Mode:                  mode,
			CIWatchTimeoutSeconds: 1800,
			DiffSizeCapLines:      200,
			TaskPriority:          PriorityCheapFirst,
			AutoMergePaths:        []string{"*.md"},
			ForcedReviewPaths:     []string{".github/workflows/**"},
		},
	}

	repo := RepoConfig{
		Name:      repoName,
		Owner:     repoOwner,
		LocalPath: workspace + "/targets/" + repoName,
		Mode:      mode,
	}
	if excl := strings.TrimSpace(os.Getenv("WORKTREE_EXCLUDE_PATHS")); excl != "" {
		for _, p := range strings.Split(excl, ",") {
			if p = strings.TrimSpace(p); p != "" {
				repo.WorktreeExcludePaths = append(repo.WorktreeExcludePaths, p)
			}
		}
	} else if repoName == "audiobook-organizer" {
		// Default: exclude large LibriVox fixtures so 4 parallel worktrees
		// fit on a GHA runner's 14 GB ephemeral disk.
		repo.WorktreeExcludePaths = []string{"testdata/audio"}
	}
	cfg.Repos = []RepoConfig{repo}

	cfg.TaskHub = TaskHubConfig{
		Repo:        envOrDefault("HUB_REPO", "jdfalk/burndown-tasks"),
		LabelPrefix: envOrDefault("LABEL_PREFIX", "repo:"),
	}

	if err := cfg.resolveGitHubEnvVars(); err != nil {
		return nil, err
	}
	cfg.applyDefaults()
	return cfg, nil
}

// implementerFromEnv builds the LLMFeatureConfig for the implementer agent.
// IMPLEMENTER_MODEL overrides everything; otherwise CHEAPEST_ONLY / POWERFUL_ONLY
// select a single flat model; the default is a tier table (cheap-first escalation).
func implementerFromEnv(provider ProviderName, cheapestOnly, powerfulOnly bool) LLMFeatureConfig {
	if m := strings.TrimSpace(os.Getenv("IMPLEMENTER_MODEL")); m != "" {
		return LLMFeatureConfig{Provider: provider, Model: m}
	}
	switch provider {
	case ProviderAnthropic:
		switch {
		case cheapestOnly:
			return LLMFeatureConfig{Provider: ProviderAnthropic, Model: "claude-haiku-4-5-20251001"}
		case powerfulOnly:
			return LLMFeatureConfig{Provider: ProviderAnthropic, Model: "claude-opus-4-7"}
		default:
			return LLMFeatureConfig{
				Provider: ProviderAnthropic,
				Model:    "claude-haiku-4-5-20251001",
				ModelTiers: []ModelTier{
					{Model: "claude-haiku-4-5-20251001", MaxComplexity: 2},
					{Model: "claude-sonnet-4-6", MaxComplexity: 4},
					{Model: "claude-opus-4-7"},
				},
			}
		}
	default: // openai
		switch {
		case cheapestOnly:
			return LLMFeatureConfig{Provider: ProviderOpenAI, Model: "gpt-5.1-codex-mini"}
		case powerfulOnly:
			return LLMFeatureConfig{Provider: ProviderOpenAI, Model: "gpt-5"}
		default:
			return LLMFeatureConfig{
				Provider: ProviderOpenAI,
				Model:    "gpt-5.1-codex-mini",
				ModelTiers: []ModelTier{
					{Model: "gpt-5.1-codex-mini", MaxComplexity: 2},
					{Model: "gpt-5.3-codex", MaxComplexity: 4},
					{Model: "gpt-5"},
				},
			}
		}
	}
}

func envOrDefault(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func isTruthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes":
		return true
	}
	return false
}
