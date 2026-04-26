# overnight-burndown

Launchd-driven nightly automation that drains a queue of small, safe work items
across configured GitHub repositories. Safe items merge themselves; risky items
become draft PRs; blocked items surface in a morning digest.

**Status:** v1 ready for staged rollout. See
[`docs/PHASE-2-ROLLOUT.md`](docs/PHASE-2-ROLLOUT.md) for the soft-launch
playbook (dry-run → draft-only → full).

## How it works

```
launchd @ 23:00
  -> burndown run
       -> collect tasks (TODO.md, GitHub issues, plans/*.md)
       -> Triage agent (Opus): classifies AUTO_MERGE_SAFE / NEEDS_REVIEW / BLOCKED
       -> Implementer agents (Haiku, parallel): produce diffs via safe-ai-util MCP
       -> driver opens PRs, watches CI, merges on green (AUTO_MERGE_SAFE)
                                     or demotes to draft (anything else)
       -> Morning digest written to ~/burndown-digest-YYYY-MM-DD.md
```

Source: [`PLAN.md`](PLAN.md) for the full architecture and locked design decisions.

## Quickstart

### 1. Install the binary

```bash
git clone https://github.com/jdfalk/overnight-burndown.git
cd overnight-burndown
make build
sudo install -m 755 bin/burndown /usr/local/bin/burndown
# or: cp bin/burndown ~/.local/bin/
```

Requires Go 1.25+. The same `Makefile` provides `make ci` (vet + staticcheck +
test + build), which is what this project's CI runs on every PR.

### 2. Install safe-ai-util + safe-ai-util-mcp

```bash
# safe-ai-util (the Rust trust boundary)
cargo install --git https://github.com/jdfalk/safe-ai-util safe-ai-util

# safe-ai-util-mcp (the Python MCP shim)
pip install --user 'safe-ai-util-mcp @ git+https://github.com/jdfalk/safe-ai-util-mcp'
```

Both must be on `$PATH` for the burndown driver to spawn them.

### 3. Configure

```bash
mkdir -p ~/.burndown
cp examples/config.yaml ~/.burndown/config.yaml
$EDITOR ~/.burndown/config.yaml
```

Every key is documented inline in `examples/config.yaml`. The minimum viable
config has:

- An Anthropic API key in `ANTHROPIC_API_KEY` (or whatever env var you set
  `anthropic.api_key_env` to).
- A GitHub App created at https://github.com/settings/apps with these scopes:
  `contents:write`, `pull_requests:write`, `issues:read`, `checks:read`,
  `metadata:read`. **Do not** grant `workflows:write`, `administration`, or
  `secrets`. Save the App ID, installation ID, and private key path in the
  config.
- At least one repo entry. Start in `mode: dry-run`.

### 4. Try a dry run by hand

```bash
burndown run --dry-run --config ~/.burndown/config.yaml
```

This collects + triages without opening any PRs. Look at
`~/burndown-digest-$(date +%Y-%m-%d).md` to see what would have happened.

### 5. Schedule the LaunchAgent (macOS)

```bash
make install-launchd
```

Runs nightly at 23:00. Inspect with `make status`; pause without uninstalling
via `make pause`; resume via `make resume`.

## Trust model

Every filesystem, exec, and git operation that the implementer agent performs
routes through [`safe-ai-util`](https://github.com/jdfalk/safe-ai-util) (Rust
trust boundary) via
[`safe-ai-util-mcp`](https://github.com/jdfalk/safe-ai-util-mcp). The driver
holds the GitHub App credentials and is the only thing that opens PRs or
merges. **Agents cannot:**

- run arbitrary shell, `curl`, `wget`, `sudo` (blocked by safe-ai-util allowlist)
- touch `.github/workflows/**` (path allowlist + driver-side veto)
- force-push (`--force*` flags stripped by policy)
- read or use GitHub App tokens (driver-owned, never passed to MCP subprocess)
- merge with `--admin` (capability not exposed)
- modify branches in any repo other than their assigned worktree
- run `git_*` or `gh_*` tools at all (filtered out of the agent's MCP tool list)

The auto-merge gate has four AND'd conditions plus hard vetoes — see PLAN.md
section B2.

## Repository layout

```
cmd/burndown/        CLI entry point (`burndown run` / `burndown --version`)
internal/
  agent/             Implementer-agent loop with MCP tool forwarding
  auth/              GitHub App JWT → installation token
  budget/            Per-night spend + wall-clock + abort gate
  config/            YAML loader + validator
  digest/            Morning markdown summary
  dispatch/          Worktree-per-task fan-out, capped concurrency
  ghops/             Driver-side commit/push/PR/CI/merge
  mcp/               Stdio MCP client wrapping safe-ai-util-mcp
  policy/            Per-repo AllowlistOverlay TOML rendering
  runner/            Orchestrator (the integration step)
  sources/           TODO.md / issues / plans/*.md collectors with dedup
  state/             Atomic state.json + flock + task hashing
  triage/            Anthropic Opus batch classification
launchd/             com.jdfalk.burndown.plist
examples/            Sample config + policies + TODO format
docs/                Operator docs (PHASE-2-ROLLOUT.md, etc.)
```

## Operating

- **Pause without uninstall:** `make pause` — touches `~/.burndown/PAUSE`.
  Burndown sees this on startup and exits before doing any work.
- **Resume:** `make resume` — removes the PAUSE flag.
- **Status:** `make status` — shows binary, plist, pause flag, state dir.
- **Inspect last night's digest:** `~/burndown-digest-$(date +%Y-%m-%d).md`
- **Inspect failures:** failed tasks leave their worktree at
  `~/.burndown/worktrees/<repo>/<branch>/` for postmortem (cleaned up after
  7 days).
- **Audit log:** `~/.burndown/audit/` — JSONL of every safe-ai-util call.

## License

MIT
