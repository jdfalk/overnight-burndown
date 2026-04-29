// file: cmd/burndown/matrix_cmds.go
// version: 1.0.0
// guid: 8b9c0d1e-2f3a-4b5c-9d6e-7f8a9b0c1d2e
//
// Matrix-mode subcommands: triage, dispatch-one, aggregate.
// Each maps 1:1 to a job in .github/workflows/nightly-matrix.yml.

package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/jdfalk/overnight-burndown/internal/config"
	"github.com/jdfalk/overnight-burndown/internal/dispatch"
	"github.com/jdfalk/overnight-burndown/internal/runner"
	"github.com/jdfalk/overnight-burndown/internal/state"
)

// cmdTriage runs collect+triage for every repo and writes the result
// JSON to --out. The matrix workflow's `triage` job calls this; its
// `outputs.tasks_json` is `jq -c '[.tasks | range(0;length)]'` over
// the file (just the indices, since the matrix can't fan out on the
// task objects directly without exceeding GitHub's 256-output limit).
func cmdTriage(args []string) int {
	fs := flag.NewFlagSet("triage", flag.ExitOnError)
	configPath := fs.String("config", defaultConfigPath(), "path to config.yaml")
	outPath := fs.String("out", "", "write triage result JSON here (required)")
	dryRun := fs.Bool("dry-run", false, "force dry-run mode for every repo")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *outPath == "" {
		fmt.Fprintln(os.Stderr, "burndown triage: --out is required")
		return 2
	}

	r, err := buildRunnerFromConfig(*configPath, *dryRun)
	if err != nil {
		fmt.Fprintln(os.Stderr, "burndown triage:", err)
		return 1
	}

	res, err := r.Triage(context.Background())
	if err != nil {
		fmt.Fprintf(os.Stderr, "burndown triage: %v\n", err)
		return 1
	}

	if err := os.MkdirAll(filepath.Dir(*outPath), 0o755); err != nil {
		fmt.Fprintln(os.Stderr, "burndown triage: mkdir:", err)
		return 1
	}
	body, err := json.MarshalIndent(res, "", "  ")
	if err != nil {
		fmt.Fprintln(os.Stderr, "burndown triage: marshal:", err)
		return 1
	}
	if err := os.WriteFile(*outPath, body, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "burndown triage: write:", err)
		return 1
	}

	fmt.Printf("burndown triage: %d dispatchable / %d dry-run-only tasks\n",
		len(res.Tasks), len(res.DryRunOutcomes))
	return 0
}

// cmdDispatchOne runs the worktree+agent+ghops sequence for a single
// task. The matrix workflow passes --task-file (the same triage JSON
// every cell sees) and --task-index (which entry to dispatch).
//
// Each cell writes its outcome to --out as JSON. The aggregate job
// downloads all outcome artifacts and concatenates them.
func cmdDispatchOne(args []string) int {
	fs := flag.NewFlagSet("dispatch-one", flag.ExitOnError)
	configPath := fs.String("config", defaultConfigPath(), "path to config.yaml")
	taskFile := fs.String("task-file", "", "path to triage result JSON (required)")
	taskIndex := fs.Int("task-index", -1, "0-based index into TriageResult.Tasks (required)")
	outPath := fs.String("out", "", "write outcome JSON here (required)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *taskFile == "" || *outPath == "" || *taskIndex < 0 {
		fmt.Fprintln(os.Stderr, "burndown dispatch-one: --task-file, --task-index, --out are required")
		return 2
	}

	body, err := os.ReadFile(*taskFile)
	if err != nil {
		fmt.Fprintln(os.Stderr, "burndown dispatch-one: read task file:", err)
		return 1
	}
	var triage runner.TriageResult
	if err := json.Unmarshal(body, &triage); err != nil {
		fmt.Fprintln(os.Stderr, "burndown dispatch-one: parse task file:", err)
		return 1
	}
	if *taskIndex >= len(triage.Tasks) {
		fmt.Fprintf(os.Stderr, "burndown dispatch-one: task-index %d out of range (%d tasks)\n",
			*taskIndex, len(triage.Tasks))
		return 1
	}
	mt := triage.Tasks[*taskIndex]

	r, err := buildRunnerFromConfig(*configPath, false)
	if err != nil {
		fmt.Fprintln(os.Stderr, "burndown dispatch-one:", err)
		return 1
	}

	oc, err := r.DispatchOne(context.Background(), mt)
	if err != nil {
		// Even on error, persist what we have so the aggregator can show
		// a "failed" cell rather than dropping it silently.
		oc = &dispatch.Outcome{
			Task:     mt.Item.Task,
			Decision: mt.Item.Decision,
			Status:   state.StatusFailed,
			Error:    err.Error(),
		}
	}

	if err := writeOutcome(*outPath, oc); err != nil {
		fmt.Fprintln(os.Stderr, "burndown dispatch-one: write:", err)
		return 1
	}
	fmt.Printf("burndown dispatch-one: %s → %s (%s)\n",
		mt.Item.Task.Source.Title, oc.Status, oc.Branch)
	if oc.AgentResult != nil {
		u := oc.AgentResult.Usage
		fmt.Printf("burndown dispatch-one: tokens prompt=%d completion=%d cached=%d total=%d (iter=%d, tools=%d)\n",
			u.PromptTokens, u.CompletionTokens, u.CachedTokens, u.TotalTokens,
			oc.AgentResult.Iterations, oc.AgentResult.ToolCallCount)
	}

	// Reflect the outcome in the exit code so a matrix cell with a truly
	// failed agent shows red in the GH Actions UI rather than
	// green-with-failure-in-JSON. Aggregate uses if: always(), so a red
	// cell doesn't prevent the digest from rendering.
	//
	// StatusNoChange is exit 0 — the agent ran cleanly but produced no diff.
	// That's a signal to revisit the task wording, not a CI failure.
	// StatusFailed is the only truly broken outcome.
	switch oc.Status {
	case state.StatusFailed:
		fmt.Fprintf(os.Stderr,
			"::error::dispatch-one failed (status=%s, branch=%s): %s\n",
			oc.Status, oc.Branch, oc.Error)
		return 1
	default:
		return 0
	}
}

// cmdAggregate concatenates dispatch-one Outcome JSON files plus the
// dry-run outcomes from the original triage result, renders the
// digest, and writes both a JSON manifest + the markdown digest.
func cmdAggregate(args []string) int {
	fs := flag.NewFlagSet("aggregate", flag.ExitOnError)
	configPath := fs.String("config", defaultConfigPath(), "path to config.yaml")
	triageFile := fs.String("triage-file", "", "path to the triage result JSON (required, for dry-run outcomes)")
	outcomesDir := fs.String("outcomes-dir", "", "directory containing dispatch-one outcome JSONs (required)")
	digestOut := fs.String("digest-out", "", "write rendered digest markdown here (required)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *triageFile == "" || *outcomesDir == "" || *digestOut == "" {
		fmt.Fprintln(os.Stderr, "burndown aggregate: --triage-file, --outcomes-dir, --digest-out are required")
		return 2
	}

	body, err := os.ReadFile(*triageFile)
	if err != nil {
		fmt.Fprintln(os.Stderr, "burndown aggregate: read triage:", err)
		return 1
	}
	var triage runner.TriageResult
	if err := json.Unmarshal(body, &triage); err != nil {
		fmt.Fprintln(os.Stderr, "burndown aggregate: parse triage:", err)
		return 1
	}

	// Collect outcomes from the per-task artifacts. Order them by
	// (repo, task) for deterministic digest output.
	matches, err := filepath.Glob(filepath.Join(*outcomesDir, "*.json"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "burndown aggregate: glob:", err)
		return 1
	}
	sort.Strings(matches)

	var outcomes []dispatch.Outcome
	for _, p := range matches {
		oc, err := readOutcome(p)
		if err != nil {
			fmt.Fprintf(os.Stderr, "burndown aggregate: skip %s: %v\n", p, err)
			continue
		}
		outcomes = append(outcomes, *oc)
	}

	r, err := buildRunnerNoValidate(*configPath, false)
	if err != nil {
		fmt.Fprintln(os.Stderr, "burndown aggregate:", err)
		return 1
	}

	path, err := r.AggregateDigest(runner.AggregateInputs{
		GeneratedAt:    triageGeneratedAt(triage),
		Outcomes:       outcomes,
		DryRunOutcomes: triage.DryRunOutcomes,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "burndown aggregate: render:", err)
		return 1
	}

	// Copy / move to --digest-out so the workflow's upload step doesn't
	// need to know the autogenerated filename inside DigestDir.
	if path != *digestOut {
		body, err := os.ReadFile(path)
		if err != nil {
			fmt.Fprintln(os.Stderr, "burndown aggregate: read internal digest:", err)
			return 1
		}
		if err := os.MkdirAll(filepath.Dir(*digestOut), 0o755); err != nil {
			fmt.Fprintln(os.Stderr, "burndown aggregate: mkdir:", err)
			return 1
		}
		if err := os.WriteFile(*digestOut, body, 0o644); err != nil {
			fmt.Fprintln(os.Stderr, "burndown aggregate: write:", err)
			return 1
		}
	}

	fmt.Printf("burndown aggregate: %d task outcomes + %d dry-run → %s\n",
		len(outcomes), len(triage.DryRunOutcomes), *digestOut)
	return 0
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// buildRunnerFromConfig is the shared `*runner.Runner` constructor for
// every matrix subcommand. It mirrors cmdRun's setup minus the actual
// `r.Run()` call so each subcommand can call its own entry point.
//
// validate=true runs config.Load (full Validate on local_path / private key);
// validate=false uses LoadNoValidate so read-only commands (aggregate) can
// run on a host that doesn't have the source repos checked out.
func buildRunnerFromConfig(configPath string, dryRun bool) (*runner.Runner, error) {
	return buildRunner(configPath, dryRun, true)
}

func buildRunnerNoValidate(configPath string, dryRun bool) (*runner.Runner, error) {
	return buildRunner(configPath, dryRun, false)
}

func buildRunner(configPath string, dryRun, validate bool) (*runner.Runner, error) {
	loader := config.Load
	if !validate {
		loader = config.LoadNoValidate
	}
	cfg, err := loader(configPath)
	if err != nil {
		return nil, fmt.Errorf("load config: %w", err)
	}
	if dryRun {
		for i := range cfg.Repos {
			cfg.Repos[i].Mode = config.ModeDryRun
		}
	}

	st, err := state.LoadDir(cfg.Paths.StateDir)
	if err != nil {
		return nil, fmt.Errorf("load state: %w", err)
	}

	providers, err := buildProviderClients(cfg)
	if err != nil {
		return nil, err
	}
	runAgent, err := pickRunAgent(cfg, providers)
	if err != nil {
		return nil, err
	}

	return &runner.Runner{
		Config:    *cfg,
		State:     st,
		Anthropic: providers.anthropic,
		SpawnMCP:  defaultSpawnMCP(cfg),
		RunAgent:  runAgent,
	}, nil
}

func writeOutcome(path string, oc *dispatch.Outcome) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	body, err := json.MarshalIndent(oc, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, body, 0o644)
}

func readOutcome(path string) (*dispatch.Outcome, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var oc dispatch.Outcome
	if err := json.Unmarshal(body, &oc); err != nil {
		return nil, err
	}
	return &oc, nil
}

func triageGeneratedAt(tr runner.TriageResult) time.Time {
	if tr.GeneratedAt.IsZero() {
		return time.Now().UTC()
	}
	return tr.GeneratedAt
}
