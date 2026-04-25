# Changelog

All notable changes to overnight-burndown.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added
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
