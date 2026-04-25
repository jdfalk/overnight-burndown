# Overnight Burndown System — Final Plan

**Status:** Design locked. Ready to build. User approved all recommendations 2026-04-25.

## Goal
Build a launchd-driven nightly automation that drains a queue of small, safe work items
from `TODO.md`, GitHub issues labeled `auto-ok`, and `plans/*.md` across configured repos.
Safe items merge themselves; risky items become draft PRs; blocked items surface in a
morning digest. Every action gated through `safe-ai-util` for structural (not prompt) security.

## Repos involved

| Repo | Role | State |
|------|------|-------|
| `jdfalk/overnight-burndown` | New. Go driver + launchd plist + digest. | To create. Public + CI. |
| `jdfalk/safe-ai-util` | Existing. Rust trust boundary. | v1.4.2 installed. Needs Phase 0 work. |
| `jdfalk/safe-ai-util-mcp` | Existing. MCP server bridging safe-ai-util to LLM tool use. | Bootstrap. Needs tool expansion. |

## Architecture

```
launchd @ 23:00 -> burndown (Go, official Anthropic SDK)
                    |
                    +- GitHub App: jdfalk-burndown-bot (NEW, narrow scope)
                    |   Allowed: contents:write, pull_requests:write, issues:read,
                    |            checks:read, metadata:read
                    |   Denied:  workflows, administration, secrets
                    |   -> go-github + ghinstallation; driver-owned
                    |
                    +- Triage agent (Opus, batch JSON output)
                    |   classifies -> AUTO_MERGE_SAFE / NEEDS_REVIEW / BLOCKED
                    |
                    +- Implementer agents (Haiku, <=4 parallel)
                    |   tool use -> MCP stdio -> safe-ai-util-mcp ->
                    |     safe-ai-util (with per-repo policy overlay)
                    |     -> audit JSONL ~/.burndown/audit/<date>.jsonl
                    |
                    +- State: ~/.burndown/state.json (hybrid resume)
                    +- Budget: $5 OR 2h, either trips -> checkpoint + digest
                    +- Concurrency: errgroup semaphore, 4 global
                    +- Digest: ~/burndown-digest-YYYY-MM-DD.md + terminal-notifier ping
```

### Locked design decisions (from "yes to all")

| ID | Decision |
|----|----------|
| A1 | MCP via stdio (extends existing safe-ai-util-mcp) |
| A2 | Layered policy: global base + per-repo overlay; overlays may only narrow |
| A3 | New GitHub App `jdfalk-burndown-bot`, narrow scope |
| A4 | Tool allowlist as proposed; gh ops stay in driver, not MCP |
| B1 | Issue-wins precedence; fuzzy match TODO/plans entries against issues by title |
| B2 | 4 gates for AUTO_MERGE: classification + auto-ok marker + path allowlist + CI green; hard vetoes: workflows/**, migrations/**, diff > 200 lines |
| B3 | Cheap-first ordering, `priority:high` label overrides |
| C1 | 4 concurrent agents global, adaptive backoff on 429 |
| C2 | $5 / 2h budget caps; either trips -> clean checkpoint + requeue |
| C3 | Hybrid resume: in-flight tasks resume; queue recomputed |
| D1 | 30 min CI watch timeout, configurable per repo; failed worktrees retained 7 days |
| D2 | Markdown digest with TL;DR / Shipped / Draft / Blocked / Failed / Requeued / Policy violations / Spend; terminal-notifier on completion |
| E1 | Public repo + CI (vet, staticcheck, test, dry-run smoke) |
| E2 | Stages: dry-run (1 night) -> draft-only (1 week) -> full (per repo) |
| E3 | audiobook-organizer only for first 2 weeks |

### MCP tool surface (final)

These are exposed to Claude via tool use. All file/exec/git ops route through safe-ai-util.

| Tool | Backed by | Notes |
|------|-----------|-------|
| `fs_read` | `safe-ai-util file read` (NEW) | Path must match repo's read allowlist |
| `fs_write` | `safe-ai-util file write` (NEW) | Path must match repo's write allowlist |
| `fs_glob` | `safe-ai-util file glob` (NEW) | Read-only |
| `run_make` | safe-ai-util via allowlist | Only allowlisted targets |
| `run_go_test` | safe-ai-util (go in always_allowed) | |
| `run_npm_test` | safe-ai-util | |
| `run_pytest` | existing `tool_py_pytest` | |
| `git_status` / `git_diff` / `git_log` | existing/extended | |
| `git_branch` | NEW | Must not be `main`/`master` |
| `git_checkout` | NEW | Driver creates branch first |
| `git_add` | existing | |
| `git_commit` | existing | |
| `git_push` | existing (extended) | `--force*` flags stripped by policy |
| `git_rebase` | NEW | |

**NOT in MCP** (driver-owned): `gh pr create/merge/checks/comment`, GitHub App token, branch naming, PR body templates, merge strategy choice.

## Phase 0 — safe-ai-util / safe-ai-util-mcp prerequisites

Real improvements to the util, regardless of burndown. PR'd separately to those repos.

### Phase 0a — safe-ai-util (Rust) changes

1. **Implement `commands/file.rs`** — currently stubbed. Add subcommands:
   - `file read --path <p>` -> stdout, with path validation
   - `file write --path <p> --content-stdin` -> reads stdin, writes file
   - `file write --path <p> --content <c>` -> inline content (size-limited)
   - `file glob --pattern <p>` -> newline-separated matches
   - All gated by `validate_file_ops` + a configurable path allowlist
2. **Add `allowlist` section to `Config` struct** in `config.rs`:
   ```rust
   pub struct Config {
       // ... existing fields
       pub allowlist: Option<AllowlistConfig>,
   }
   ```
   Then make `Executor` honor `Config::allowlist` if present, falling back to `AllowlistConfig::secure_default()`.
3. **Layered config support** — `--policy-overlay <file>` flag that loads on top of `--config`. Overlay can only narrow.
4. **`SAFE_AI_UTIL_LOG_DIR` env var** in `logger.rs` — fall back to `./logs/` only if unset, so worktrees don't get polluted.
5. **`SAFE_AI_UTIL_AUDIT_PATH` env var** so burndown can collect audit into `~/.burndown/audit/<date>.jsonl`.

### Phase 0b — safe-ai-util-mcp (Python) changes

Branch: `feat/burndown-tool-expansion`.

1. Add tools: `fs_read`, `fs_write`, `fs_glob` (delegate to new `safe-ai-util file` subcommands)
2. Add tools: `git_branch`, `git_checkout`, `git_diff`, `git_log`, `git_rebase`
3. Add tools: `run_make`, `run_go_test`, `run_go_build`, `run_go_vet`, `run_npm_test`, `run_npm_ci`
4. Pass through `SAFE_AI_UTIL_LOG_DIR` and `SAFE_AI_UTIL_AUDIT_PATH` from caller env
5. Suppress safe-ai-util logging preamble when `SAFE_AI_UTIL_QUIET=1`
6. Pytest coverage for new tools

## Phase 1 — burndown driver MVP

Repo: `jdfalk/overnight-burndown` (public, MIT or Apache-2.0)

```
overnight-burndown/
+- cmd/
|   +- burndown/
|       +- main.go              # entry point: CLI flags, orchestration
+- internal/
|   +- auth/                    # GitHub App JWT -> installation token
|   +- budget/                  # token + wall-clock tracking, abort gate
|   +- config/                  # YAML config load + validate
|   +- digest/                  # morning digest renderer (text/template)
|   +- dispatch/                # worktree-per-task, semaphore, CI watch
|   +- mcp/                     # MCP client (stdio) wrapping safe-ai-util-mcp
|   +- policy/                  # per-repo policy overlays, path allowlists
|   +- sources/                 # collect TODO.md / issues / plans/*.md
|   +- state/                   # state.json atomic read/write, task hashing
|   +- triage/                  # Anthropic batch call -> JSON classifications
+- prompts/
|   +- triage.md
|   +- implementer.md
|   +- needs-review-pr.md
+- policies/                    # default + example per-repo policy YAML
|   +- default.yaml
|   +- audiobook-organizer.yaml
+- launchd/
|   +- com.jdfalk.burndown.plist
+- testdata/                    # fixture repo for dry-run smoke
+- go.mod
+- Makefile                     # build, test, install-launchd, uninstall
+- README.md
+- CHANGELOG.md
+- .github/workflows/
    +- ci.yml                   # vet, staticcheck, test, smoke
```

### Build sequence (Phase 1)

1. **Skeleton** — `go.mod`, Makefile, CI yml, README. First test passing.
2. **`internal/config`** — schema + validation (repo list, mode per repo, budget, model assignments). Test fixtures.
3. **`internal/state`** — atomic JSON read/write, task hash (sha256 of source URL + content), in-flight tracking.
4. **`internal/auth`** — GitHub App JWT -> installation token via `bradleyfalzon/ghinstallation/v2`. Test with mocked transport.
5. **`internal/sources`** — collect from local `TODO.md`, `gh issue list -l auto-ok` (via go-github with App token), `plans/*.md`. Dedup by hash. Test with fixture repo.
6. **`internal/triage`** — Anthropic SDK call (Opus, JSON-mode), batch all tasks in one prompt. Validate result schema.
7. **`internal/mcp`** — stdio MCP client. Spawn `safe-ai-util-mcp` subprocess, send tool-list/call, parse responses. Test with stub MCP server.
8. **`internal/dispatch`** — semaphore (errgroup), worktree creation per task, agent loop (Haiku via Anthropic SDK with MCP tools registered). Drives implementer until tool calls stop.
9. **Driver-side gh ops** — `dispatch.openPR()`, `dispatch.watchCI()`, `dispatch.merge()`. Use go-github with App token. Strip `--force` from any branch operations.
10. **`internal/budget`** — track token usage from Anthropic response headers, wall-clock from start. 80% threshold -> graceful abort: finish in-flight, requeue rest, render partial digest.
11. **`internal/policy`** — load default + per-repo overlay, narrow-only validation, render to YAML for safe-ai-util's `--policy-overlay`.
12. **`internal/digest`** — text/template rendering all sections. Pull from state.json + audit log.
13. **launchd plist** + `make install-launchd` / `make uninstall-launchd` / `make pause` (touch `~/.burndown/PAUSE`).
14. **CI smoke** — `--dry-run --once --repo testdata/fixture` runs in GitHub Actions, asserts no writes.

## Phase 2 — soft launch (after Phase 1 ships)

| Day | Action |
|-----|--------|
| 1 | `mode: dry-run` against audiobook-organizer. Wake up, read digest, no PRs created. |
| 2-8 | `mode: draft-only` against audiobook-organizer. Real PRs but never auto-merged. Review draft PR quality each morning. |
| 9 | `mode: full`. Auto-merge enabled. Watch closely for first week. |
| 22 | If clean for 2 weeks: add second repo in `dry-run` mode. |

## Hard safeguards (always-on)

- `~/.burndown/PAUSE` file presence aborts at startup
- `~/.burndown/run.lock` flock — only one run at a time
- Workflow file change -> forced NEEDS_REVIEW (defense in depth: also blocked by App scopes, also blocked by path allowlist)
- Diff > 200 lines -> forced NEEDS_REVIEW
- safe-ai-util audit log captures every action; digest summarizes blocked attempts
- App token never in env exposed to MCP subprocess
- Worktree paths are sandbox: `~/.burndown/worktrees/<repo>/<task-slug>/`

## Test strategy

- **Unit**: `go test ./...` — config, state hashing, source dedup, triage parse, budget arithmetic, digest render, policy narrow-only validation
- **Integration**: stubbed Anthropic + MCP + go-github; full-run scenarios:
  - SAFE task with `.github/workflows/foo.yml` change -> forced NEEDS_REVIEW
  - Budget exceeded mid-wave -> clean abort + state requeued
  - Lock contention -> second run no-ops
  - CI red -> PR drafted, not merged
  - safe-ai-util policy violation -> logged, task marked failed
- **Smoke**: `--dry-run --once --repo testdata/fixture` in CI

## Rollback

- `~/.burndown/PAUSE` — instant kill
- `make uninstall-launchd` — disable schedule
- All actions are normal git/PR operations; revert is a normal `git revert`
- State file is per-night; easy to inspect/rewind
- Phase 0 changes to safe-ai-util are additive (new subcommands, new env vars, optional config field) — no breaking changes to existing consumers

## Verification (concrete first night before merge enabled)

The night before flipping to `mode: full`:
1. Confirm digest format renders cleanly
2. Confirm at least 5 draft PRs were opened with reasonable bodies
3. Confirm `~/.burndown/audit/` has structured entries
4. Confirm no policy violations were attempted
5. Confirm budget came in under cap with margin

## Out of scope (explicit non-goals for v1)

- Slack/email digest delivery (markdown + macOS notification only)
- Multi-machine coordination (single-host)
- Auto-merging dependency upgrades (Dependabot's job)
- Modifying any non-jdfalk repos
- Cross-repo PR coordination
