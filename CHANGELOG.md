# Changelog

All notable changes to overnight-burndown.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added
- Phase 1 step 9a: `internal/ghops` — driver-side commit, push, and PR
  creation. Agents never touch git or GitHub.
  - `Publisher.CommitAndPush` stages all changes in the worktree, commits
    as the configured bot identity, and pushes via a one-shot HTTPS URL
    that carries the App installation token. Token is never written to
    `.git/config` or any file on disk.
  - `ErrNoChanges` sentinel for the empty-diff case (agent produced no
    output) — callers treat it as a successful no-op.
  - Token is **redacted** (`***`) from any error output before propagating,
    so a push failure can't leak credentials into logs or PR comments.
  - `Publisher.OpenPR` opens a PR with classification-aware draft state
    (NEEDS_REVIEW → draft, AUTO_MERGE_SAFE → not draft).
  - Pluggable `gitRunner` so tests can intercept push and verify against
    a local bare repo without network egress.
- Phase 1 step 8b: `internal/dispatch` — worktree-per-task fan-out with
  errgroup-bounded concurrency.
  - `AddWorktree` / `RemoveWorktree` / `DeleteBranch` shell-out helpers
    around `git worktree`. Tested against real `t.TempDir()` repos.
  - `SlugifyForBranch` for safe kebab-case branch components, with a
    40-char cap and a stable fallback ("task") for empty input.
  - `Dispatcher` runs every task through the agent under a configurable
    concurrency cap (default 4). Per-task failures are isolated — one
    failing task doesn't abort the others.
  - Branch deduplication when triage produces colliding suggestions
    (collisions get a numeric suffix).
  - Failed worktrees are left on disk for postmortem inspection
    (cleanup is a separate concern, retained 7d per PLAN.md).
  - Pluggable `RunAgent` and `SpawnMCP` hooks so tests don't need
    Anthropic or `safe-ai-util-mcp`.
- Phase 1 step 8a: `internal/agent` — implementer agent loop (Haiku via
  Anthropic SDK with MCP tools registered).
  - Manual tool-use loop: send to Claude → execute each ToolUseBlock by
    forwarding to MCP → feed results back as tool_result blocks → repeat
    until end_turn or iteration cap.
  - Default tool allowlist excludes git_* and gh_* — git and PR ops are
    the harness's job, not the agent's. Agent gets fs_*, run_*, py_pytest only.
  - MCP errors become `is_error: true` tool_result blocks (the agent can
    recover) rather than aborting the loop.
  - System prompt is cached via `cache_control: ephemeral`.
  - Captures the agent's final summary on every turn so the PR body /
    digest entry survives even if a panic interrupts result construction.
  - MCPClient interface (rather than concrete `*mcp.Client`) so tests
    inject a stub.
- Phase 1 step 7: `internal/mcp` — stdio JSON-RPC client for safe-ai-util-mcp.
  - Pluggable `Transport` interface — production uses subprocess pipes
    (`Spawn`); tests use `io.Pipe` so no shelling out during CI.
  - `NewClient` performs the MCP `initialize` handshake + `notifications/initialized`
    eagerly so a misconfigured server fails at construction.
  - `ListTools(ctx)` returns the server's tool catalog as a `[]ToolDef`
    that maps directly onto Anthropic SDK tool registration.
  - `CallTool(ctx, name, args)` dispatches `tools/call` and returns
    concatenated text content + `isError` flag.
  - Concurrent-safe by design: writes serialized via mutex, responses
    demuxed by request ID via `sync.Map`. 8 parallel CallTool's tested
    under `-race`.
  - Context cancellation is prompt — pending calls unblock within ~ms
    of `ctx.Done()`, even if the server never responds.
  - Transport closure mid-call fails every pending caller with a
    "transport closed" sentinel rather than hanging forever.
- Phase 1 step 6: `internal/triage` — Anthropic Opus call that classifies tasks
  AUTO_MERGE_SAFE / NEEDS_REVIEW / BLOCKED in a single batched request.
  - Uses the official `anthropic-sdk-go`. Model is config-driven (defaults to
    `claude-opus-4-7`).
  - **Tool-forced structured output**: Claude returns decisions via a single
    forced `record_classifications` tool call with a strict JSON Schema —
    eliminates free-form JSON drift and post-parse defenses.
  - **Prompt caching** on the system prompt block (`cache_control: ephemeral`)
    so repeated triage calls hit the cache at ~0.1× input price.
  - Conservative-by-default classification rules: when uncertain, the rulebook
    tells the model to choose NEEDS_REVIEW. The cost of misclassifying a
    refactor as auto-merge-safe is too high.
  - Validation: decision count must match input; classifications must be in the
    canonical enum; non-blocked decisions must carry a `suggested_branch`.
  - Decisions are reordered to match input slice order even if the model
    returns them shuffled.
- Phase 1 step 5: `internal/sources` — task collection from TODO.md, GitHub
  issues, and `plans/*.md`.
  - Three `Collector` implementations sharing a common `Task` shape.
  - `CollectAll` runs every collector and applies issue-wins dedup:
    matching titles collapse onto the issue with a `TrackedBy` annotation
    surfacing the absorbed sources in the morning digest.
  - `[auto-ok]` markers on TODO lines and `<!-- auto-ok -->` markers on
    plan files set `Task.HasAutoOK`. Issues are auto-ok by virtue of the
    `auto-ok` label filter.
  - `NormalizeTitle` strips checkbox / auto-ok prefixes, lowercases, drops
    punctuation, and collapses whitespace — the basis for strict-equality
    dedup. Fuzzy matching deferred until v1 misses real near-duplicates.
- Phase 1 step 4: `internal/auth` — GitHub App installation auth.
  - `Auth` wraps a `bradleyfalzon/ghinstallation/v2.Transport` so every
    HTTP request through `Auth.HTTPClient()` carries an auto-refreshing
    installation token (no caller-side refresh logic).
  - Eager validation: `New()` performs the first token exchange against
    GitHub at construction time, so a wrong App ID / installation ID /
    private key surfaces at startup with a clear error rather than inside
    the first PR-create call hours later.
  - `InstallationToken(ctx)` for non-Go consumers (git over HTTPS).
  - Tests use `httptest` + a freshly-generated RSA key; never touch real
    GitHub or commit any secret material.
- Phase 1 step 3: `internal/state` — atomic state.json + run lock + task hashing.
  - Stable `HashTask(Source)` keys tasks across nights (sha256 of type+repo+url+content).
  - Save() is atomic via tempfile + fsync + rename — crash mid-write cannot corrupt prior state.
  - Load() tolerates a missing file (first-night runs work without bootstrapping)
    and rejects unsupported `schema_version` so a rolled-back driver can't misread
    a forward-rolled file.
  - `AcquireLock()` uses `flock(LOCK_EX|LOCK_NB)` so a crashed run releases its
    lock automatically when the kernel reaps the process — no stale-PID dance.
  - `InFlight()` filters tasks with an open PR but no terminal status, used for
    hybrid-resume on subsequent nights.
  - Concurrent-safe `Upsert`/`Get` (sync.Mutex on Tasks map). Tested under -race.
- Bumped Go floor to 1.25 (transitive requirement of new deps).
- Phase 1 step 2: `internal/config` — YAML config loader, schema, and validator.
  - Schema covers anthropic models, github App auth, paths, budget caps,
    concurrency, defaults, and per-repo settings.
  - `~`-expansion against `$HOME` for every path-shaped field.
  - Strict YAML parsing (`KnownFields(true)`) catches typo'd keys.
  - `errors.Join` accumulates all validation problems so the operator sees
    the full list at once instead of fix-one-find-next.
  - GitHub App auth only required when at least one repo is non-dry-run —
    enables a fully-offline dry-run config.
  - Per-repo defaults inheritance (mode / ci_watch_timeout / auto_merge_paths).
- Phase 1 step 1: Go skeleton.
  - `cmd/burndown` entry point with `--version` flag.
  - `internal/version` package with first passing test.
  - Makefile with `build`, `test`, `vet`, `staticcheck`, `ci`, `install-launchd`,
    `uninstall-launchd`, `pause`, `resume`, `status` targets.
  - GitHub Actions CI workflow (vet + staticcheck + test + build).
  - launchd plist scaffold for `~/Library/LaunchAgents/`.

### Notes
- Trust boundary: filesystem, exec, and git operations route through
  [safe-ai-util](https://github.com/jdfalk/safe-ai-util) via
  [safe-ai-util-mcp](https://github.com/jdfalk/safe-ai-util-mcp). GitHub App
  operations stay in the Go driver — agents never receive the App token.
