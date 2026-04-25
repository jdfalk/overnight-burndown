# Changelog

All notable changes to overnight-burndown.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added
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
