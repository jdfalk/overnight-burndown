# Phase 2 staged rollout

Per PLAN.md design E2: every repo starts in `dry-run` for one night,
graduates to `draft-only` for one week, then to `full`. Soft launch
trades velocity for confidence — the cost of waking up to an
auto-merged regression is much higher than the cost of waiting a week.

## Day 1 — `dry-run`, single repo

Goal: confirm the harness works end-to-end without any external
side effects.

1. Set the target repo's `mode: dry-run` in `~/.burndown/config.yaml`.
2. Run by hand: `burndown run --config ~/.burndown/config.yaml`.
3. Inspect `~/burndown-digest-$(date +%Y-%m-%d).md`. Look for:
   - TL;DR shows non-zero `blocked` (every dry-run task surfaces
     under "Blocked" since dispatch never happens).
   - Spend section is sane (under $1 for a typical first run).
   - No "policy violations" warning.
4. If anything looks off, look at `~/.burndown/audit/` for the
   per-call JSONL. Iterate on config until the dry-run is clean.
5. Once a clean dry-run lands, install the LaunchAgent: `make install-launchd`.

## Day 2-8 — `draft-only`

Goal: confirm classifier accuracy + agent quality without auto-merge
risk. Every PR opens as draft regardless of classification.

1. Switch the target repo's `mode: draft-only`. Don't touch any
   other repo yet.
2. Wait for the next launchd run (23:00 local time).
3. Each morning:
   - Read the digest.
   - Open every "Draft PRs awaiting review" entry.
   - For PRs that look correct + would have auto-merged: leave a
     comment, mark ready for review, merge by hand. Track these —
     they're your evidence that the classifier+agent combination is
     trustworthy.
   - For PRs that look wrong: close them, write down what went wrong,
     update the rulebook (system prompts) or the per-repo config
     to prevent recurrence.
4. After 7 nights of clean draft-only runs (no PRs that would have
   auto-merged but shouldn't have), promote.

If you see:

- A PR the classifier marked AUTO_MERGE_SAFE but the diff actually
  modifies non-trivial logic → tighten `defaults.auto_merge_paths` in
  config.
- The agent producing low-quality fixes (forgot context, broke an
  unrelated thing) → review whether the `implementer_model` is right;
  consider bumping to Sonnet for that repo via `implementer_model:
  claude-sonnet-4-6` in config (per-repo override is a future feature;
  for now it's global).
- Triage being too aggressive on AUTO_MERGE_SAFE → the system prompt
  in `internal/triage/prompt.go` is the rulebook. Open a PR there,
  ship a fix, redeploy.

## Day 9+ — `full`, single repo

Goal: actually realize the time savings.

1. Switch the target repo to `mode: full`.
2. Watch the next morning's digest carefully.
3. For shipped PRs: spot-check the diff and the merged commit on
   `origin/main`. Anything dubious → revert + tighten config.
4. After 2 weeks of clean full-mode runs, add a second repo at the
   start of its own staged rollout (`dry-run` → `draft-only` → `full`).

## Indicators it's working

- Digest each morning shows ≥1 shipped PR that you would have
  reviewed-and-merged anyway.
- Spend stays under $5/night.
- Failed worktrees in `~/.burndown/worktrees/<repo>/` get cleaned
  up by the 7-day retention sweep.
- No `burndown-failed` labels accumulating on PRs (a few are normal;
  many = the gate is too loose or the agent is unreliable).
- The audit log (`~/.burndown/audit/`) shows zero `Blocked` events
  per night. Any `Blocked` event = the agent attempted something
  outside its allowlist; investigate.

## Indicators it's not working

- Repeated `burndown-failed` PRs with the same root cause → tighten
  rulebook or configuration.
- Agent loops hitting the iteration cap on the same task two nights
  in a row → BLOCKED is the right call; mark the task `[auto-ok]`
  removed in TODO.md or close the issue.
- Spend trending up week-over-week with the same workload → check
  prompt-cache hit ratio (Anthropic dashboard); look for system-prompt
  invalidation in `internal/triage` or `internal/agent`.

## Rollback

- **Pause the next run without uninstalling:** `make pause`.
- **Hard stop:** `make uninstall-launchd`.
- **Revert a bad merge:** `git revert <sha>`. Burndown only auto-rebases
  forward; nothing depends on a particular merge sticking around.

## Adding repos

After audiobook-organizer has been in `full` mode for ≥2 weeks, add
the next repo at `dry-run` and walk the same ladder. Don't promote
multiple repos at once; isolate variables.
