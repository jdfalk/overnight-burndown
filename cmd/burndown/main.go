// Command burndown is the overnight automation entry point invoked by launchd.
//
// Subcommands:
//
//	burndown --version          Print version
//	burndown run                Execute one nightly cycle
//	burndown run --dry-run      Triage-only run; never opens PRs
//	burndown run --config PATH  Use a non-default config file
//
// On a normal night launchd invokes `burndown run` at 23:00 (see
// launchd/com.jdfalk.burndown.plist).
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/openai/openai-go"
	openaiOption "github.com/openai/openai-go/option"

	"github.com/jdfalk/overnight-burndown/internal/agent"
	"github.com/jdfalk/overnight-burndown/internal/config"
	"github.com/jdfalk/overnight-burndown/internal/dispatch"
	"github.com/jdfalk/overnight-burndown/internal/mcp"
	"github.com/jdfalk/overnight-burndown/internal/runner"
	"github.com/jdfalk/overnight-burndown/internal/state"
	"github.com/jdfalk/overnight-burndown/internal/version"
)

func main() {
	if len(os.Args) >= 2 {
		switch os.Args[1] {
		case "--version", "-v", "version":
			fmt.Println(version.String())
			return
		case "run":
			os.Exit(cmdRun(os.Args[2:]))
		case "triage":
			os.Exit(cmdTriage(os.Args[2:]))
		case "dispatch-one":
			os.Exit(cmdDispatchOne(os.Args[2:]))
		case "aggregate":
			os.Exit(cmdAggregate(os.Args[2:]))
		case "--help", "-h", "help":
			printHelp()
			return
		}
	}

	// Old behavior: --version flag form.
	versionFlag := flag.Bool("version", false, "print version and exit")
	flag.Parse()
	if *versionFlag {
		fmt.Println(version.String())
		return
	}

	printHelp()
	os.Exit(2)
}

func printHelp() {
	fmt.Fprintln(os.Stderr, "Usage: burndown <subcommand> [flags]")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "Subcommands:")
	fmt.Fprintln(os.Stderr, "  run             Execute one nightly cycle (single-runner)")
	fmt.Fprintln(os.Stderr, "  triage          Matrix mode: emit task JSON for fan-out")
	fmt.Fprintln(os.Stderr, "  dispatch-one    Matrix mode: dispatch one task from triage JSON")
	fmt.Fprintln(os.Stderr, "  aggregate       Matrix mode: combine outcomes into a digest")
	fmt.Fprintln(os.Stderr, "  --version, -v   Print version and exit")
	fmt.Fprintln(os.Stderr, "  --help, -h      Show this help")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "  burndown <subcommand> --help   Subcommand-specific flags")
}

// cmdRun parses `run` flags and executes one nightly cycle.
func cmdRun(args []string) int {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	configPath := fs.String("config", defaultConfigPath(), "path to config.yaml")
	dryRun := fs.Bool("dry-run", false, "force dry-run mode for every repo (override config)")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "burndown: load config: %v\n", err)
		return 1
	}

	if *dryRun {
		for i := range cfg.Repos {
			cfg.Repos[i].Mode = config.ModeDryRun
		}
	}

	// Load (or initialize) state.
	st, err := state.LoadDir(cfg.Paths.StateDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "burndown: load state: %v\n", err)
		return 1
	}

	// Build provider clients on demand. The active providers are
	// determined by `triage.provider` and `implementer.provider` in
	// config; we only require credentials for the providers actually used.
	providers, err := buildProviderClients(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "burndown:", err)
		return 1
	}

	// Pick the implementer agent runner based on config.implementer.provider.
	runAgent, err := pickRunAgent(cfg, providers)
	if err != nil {
		fmt.Fprintln(os.Stderr, "burndown:", err)
		return 1
	}

	// Runner.Anthropic is used by the dispatcher's Options for the Anthropic
	// agent path. When the implementer is OpenAI, runAgent ignores Options.Client,
	// so the field can remain zero.
	r := &runner.Runner{
		Config:    *cfg,
		State:     st,
		Anthropic: providers.anthropic,
		SpawnMCP:  defaultSpawnMCP(cfg),
		RunAgent:  runAgent,
	}

	res, err := r.Run(context.Background())
	if err != nil {
		fmt.Fprintf(os.Stderr, "burndown: run failed: %v\n", err)
		return 1
	}

	fmt.Printf("burndown: digest written to %s\n", res.DigestPath)
	fmt.Printf("burndown: $%.4f spent / %d outcomes / %d shipped\n",
		res.Stats.DollarsSpent, len(res.Outcomes), len(res.MergedBranches))
	return 0
}

// defaultConfigPath returns the canonical config location.
func defaultConfigPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "burndown.yaml"
	}
	return filepath.Join(home, ".burndown", "config.yaml")
}

// providerClients is the per-provider SDK client bundle. Only the
// clients actually used (per config.triage.provider / config.implementer.provider)
// are initialized; anything left zero is fine.
type providerClients struct {
	anthropic   anthropic.Client
	openai      openai.Client
	openaiModel string // implementer model when provider=openai
}

// buildProviderClients inspects config and constructs the SDK clients for
// every provider that's actually used. Missing credentials for unused
// providers are tolerated (the validator already enforces credentials for
// active providers).
func buildProviderClients(cfg *config.Config) (*providerClients, error) {
	out := &providerClients{}

	usesAnthropic := cfg.Triage.Provider == config.ProviderAnthropic ||
		cfg.Implementer.Provider == config.ProviderAnthropic
	usesOpenAI := cfg.Triage.Provider == config.ProviderOpenAI ||
		cfg.Implementer.Provider == config.ProviderOpenAI

	if usesAnthropic {
		key := os.Getenv(cfg.Anthropic.APIKeyEnv)
		if key == "" {
			return nil, fmt.Errorf("%s is not set (required by triage/implementer with provider=anthropic)", cfg.Anthropic.APIKeyEnv)
		}
		opts := []option.RequestOption{option.WithAPIKey(key)}
		if cfg.Anthropic.BaseURL != "" {
			opts = append(opts, option.WithBaseURL(cfg.Anthropic.BaseURL))
		}
		out.anthropic = anthropic.NewClient(opts...)
	}
	if usesOpenAI {
		key := os.Getenv(cfg.OpenAI.APIKeyEnv)
		if key == "" {
			return nil, fmt.Errorf("%s is not set (required by triage/implementer with provider=openai)", cfg.OpenAI.APIKeyEnv)
		}
		opts := []openaiOption.RequestOption{openaiOption.WithAPIKey(key)}
		if cfg.OpenAI.BaseURL != "" {
			opts = append(opts, openaiOption.WithBaseURL(cfg.OpenAI.BaseURL))
		}
		out.openai = openai.NewClient(opts...)
	}
	if cfg.Implementer.Provider == config.ProviderOpenAI {
		out.openaiModel = cfg.Implementer.Model
	}
	return out, nil
}

// pickRunAgent returns the dispatch.RunAgentFunc for the active
// implementer provider. The Anthropic path uses agent.Run; the OpenAI
// path closes over the OpenAI client + model and ignores the Anthropic
// fields on agent.Options.
func pickRunAgent(cfg *config.Config, providers *providerClients) (dispatch.RunAgentFunc, error) {
	switch cfg.Implementer.Provider {
	case config.ProviderOpenAI:
		client := providers.openai
		model := providers.openaiModel
		// Default to the Responses API. PreviousResponseID lets the agent
		// loop avoid re-sending the full conversation each iter, which
		// previously exhausted TPM at modest concurrency. Set
		// implementer.api: chat-completions in config.yaml to fall back
		// to the legacy path while we soak the migration. Spec:
		// docs/specs/2026-04-29-responses-api-migration.md.
		if cfg.Implementer.API == config.OpenAIAPIChatCompletions {
			return func(ctx context.Context, opts agent.Options) (*agent.Result, error) {
				return agent.RunOpenAI(ctx, client, model, opts)
			}, nil
		}
		return func(ctx context.Context, opts agent.Options) (*agent.Result, error) {
			// Select the model tier that matches this task's complexity score
			// (1–5 from triage). Falls back to implementer.model when no
			// model_tiers are configured.
			selected := cfg.Implementer.SelectModel(opts.Decision.EstComplexity)
			return agent.RunOpenAIResponses(ctx, client, selected, opts)
		}, nil
	case config.ProviderAnthropic:
		// agent.Run reads opts.Client and opts.Model that the dispatcher
		// fills from runner.Anthropic and config.Implementer.Model.
		// Override Model on Options here so it matches Implementer.Model
		// regardless of what the dispatcher fills in (the dispatcher
		// currently uses Anthropic.ImplementerModel for legacy reasons).
		model := anthropic.Model(cfg.Implementer.Model)
		return func(ctx context.Context, opts agent.Options) (*agent.Result, error) {
			opts.Model = model
			return agent.Run(ctx, opts)
		}, nil
	default:
		return nil, fmt.Errorf("unknown implementer.provider: %q", cfg.Implementer.Provider)
	}
}

// defaultSpawnMCP returns a SpawnMCPFunc that launches safe-ai-util-mcp
// with the worktree-pinned env vars. The MCP subprocess's environment
// inherits SAFE_AI_UTIL_REPO_ROOT (the worktree path), so safe-ai-util
// sandboxes every file op to that directory.
func defaultSpawnMCP(cfg *config.Config) dispatch.SpawnMCPFunc {
	auditDir := cfg.Paths.AuditDir
	logDir := cfg.Paths.LogDir
	return func(ctx context.Context, worktreePath string) (agent.MCPClient, func() error, error) {
		env := append(os.Environ(),
			"SAFE_AI_UTIL_REPO_ROOT="+worktreePath,
			"SAFE_AI_UTIL_AUDIT_PATH="+auditDir,
			"SAFE_AI_UTIL_LOG_DIR="+logDir,
			"SAFE_AI_UTIL_QUIET=1",
		)
		client, err := mcp.Spawn(ctx, "safe-ai-util-mcp", nil, env)
		if err != nil {
			return nil, nil, err
		}
		return client, client.Close, nil
	}
}
