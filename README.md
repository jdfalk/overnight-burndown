# overnight-burndown

Launchd-driven nightly automation that drains a queue of small, safe work items
across configured GitHub repositories. Safe items merge themselves; risky items
become draft PRs; blocked items surface in a morning digest.

**Status:** in design. See [`PLAN.md`](PLAN.md) for the full architecture and build sequence.

## How it works

```
launchd @ 23:00
  -> burndown driver (Go, official Anthropic SDK)
       -> Triage agent (Opus): classifies tasks
       -> Implementer agents (Haiku, parallel): produce diffs via safe-ai-util MCP
       -> Driver opens PRs, watches CI, merges on green
       -> Morning digest at ~/burndown-digest-YYYY-MM-DD.md
```

## Trust model

Every filesystem, exec, and git operation routes through
[`safe-ai-util`](https://github.com/jdfalk/safe-ai-util) (Rust trust boundary)
via [`safe-ai-util-mcp`](https://github.com/jdfalk/safe-ai-util-mcp). Agents
cannot:

- run arbitrary shell, `curl`, `wget`, `sudo` (blocked by safe-ai-util allowlist)
- touch `.github/workflows/**` (path allowlist + driver-side veto)
- force-push (`--force*` flags stripped by policy)
- access GitHub App tokens (driver-owned, never passed to MCP subprocess)
- merge with `--admin` (capability not exposed)

## Status

Pre-alpha. See `PLAN.md`.

## License

MIT (TBD)
