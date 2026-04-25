# Changelog

All notable changes to overnight-burndown.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added
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
