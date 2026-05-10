// file: cmd/burndown/triage_poll_cmd.go
// version: 1.0.0
// guid: 7d8e9f0a-1b2c-3d4e-5f6a-7b8c9d0e1f2a
//
// CLI entry point for `burndown triage-poll`.
// Called by the 30-minute cron workflow; exits 0 on any handled state
// (including "nothing to do"), non-zero only on unexpected errors.

package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"

	"github.com/jdfalk/overnight-burndown/internal/config"
	"github.com/jdfalk/overnight-burndown/internal/triagepoll"
)

// cmdTriagePoll parses `triage-poll` flags and runs one iteration.
func cmdTriagePoll(args []string) int {
	fs := flag.NewFlagSet("triage-poll", flag.ExitOnError)
	fromEnv := fs.Bool("from-env", false, "build config entirely from environment variables")
	dryRun := fs.Bool("dry-run", false, "log actions without writing to GitHub or OpenAI")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	var cfg *config.Config
	var err error
	if *fromEnv {
		cfg, err = config.FromEnvNoValidate()
	} else {
		cfg, err = config.Load(defaultConfigPath())
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "burndown triage-poll: load config: %v\n", err)
		return 1
	}

	if cfg.TaskHub.Repo == "" {
		fmt.Fprintln(os.Stderr, "burndown triage-poll: task_hub.repo is not configured")
		return 1
	}
	if len(cfg.Repos) == 0 {
		fmt.Fprintln(os.Stderr, "burndown triage-poll: no repos configured")
		return 1
	}

	openaiKey := os.Getenv(cfg.OpenAI.APIKeyEnv)
	if openaiKey == "" && !*dryRun {
		fmt.Fprintf(os.Stderr, "burndown triage-poll: %s is not set\n", cfg.OpenAI.APIKeyEnv)
		return 1
	}

	isDry := *dryRun || cfg.Repos[0].Mode == config.ModeDryRun

	pollCfg := triagepoll.PollConfig{
		GitHub:       cfg.GitHub,
		TaskHub:      cfg.TaskHub,
		OpenAIAPIKey: openaiKey,
		TriageModel:  cfg.Triage.Model,
		RepoName:     cfg.Repos[0].Name,
		DryRun:       isDry,
	}

	ctx := context.Background()
	result, err := triagepoll.Poll(ctx, pollCfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "burndown triage-poll: %v\n", err)
		return 1
	}

	slog.InfoContext(ctx, "triagepoll: done", "action", result.Action, "detail", result.Detail)
	fmt.Printf("triage-poll: %s — %s\n", result.Action, result.Detail)
	return 0
}
