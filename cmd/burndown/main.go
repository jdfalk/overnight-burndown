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
	fmt.Fprintln(os.Stderr, "  run             Execute one nightly cycle")
	fmt.Fprintln(os.Stderr, "  --version, -v   Print version and exit")
	fmt.Fprintln(os.Stderr, "  --help, -h      Show this help")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "  burndown run --help   Subcommand-specific flags")
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
	statePath := filepath.Join(cfg.Paths.StateDir, "state.json")
	st, err := state.Load(statePath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "burndown: load state: %v\n", err)
		return 1
	}

	// Anthropic client uses the API key from the configured env var.
	apiKey := os.Getenv(cfg.Anthropic.APIKeyEnv)
	if apiKey == "" {
		fmt.Fprintf(os.Stderr, "burndown: %s is not set\n", cfg.Anthropic.APIKeyEnv)
		return 1
	}
	anthropicClient := anthropic.NewClient(option.WithAPIKey(apiKey))

	r := &runner.Runner{
		Config:    *cfg,
		State:     st,
		Anthropic: anthropicClient,
		SpawnMCP:  defaultSpawnMCP(cfg),
		RunAgent:  agent.Run,
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
