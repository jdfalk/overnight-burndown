<!-- file: TODO.md -->
<!-- version: 1.3.0 -->
<!-- guid: 9e3a4b5c-6d7e-8f9a-0b1c-2d3e4f5a6b7c -->

# overnight-burndown — TODO

Canonical index of outstanding work. Details live in linked specs.

## Active

### OpenAI Responses API migration

Chat Completions is in maintenance. Newer models (codex-mini, gpt-5.4)
ship on `/v1/responses` only or first. Plus `PreviousResponseID`
collapses prompt-token cost for our 5–15 iteration agent loop. Spec:
[`docs/specs/2026-04-29-responses-api-migration.md`](docs/specs/2026-04-29-responses-api-migration.md).

- [x] **RESP-1** Add `RunOpenAIResponses` alongside `RunOpenAI`; gate via config (`2ea6fdd`)
- [ ] **RESP-2** Migrate `internal/triage/openai.go` to Responses (single forced tool call — lowest risk first)
- [x] **RESP-3** Default `implementer.api=responses` — Responses is now the default path in `main.go`; Chat Completions is opt-in via `api: chat-completions` (`2ea6fdd`)
- [x] **RESP-4** Complexity-based model tier selection replaces runtime fallback chain; retry tests + tier tests added (`e6ece17`) — full mocked PreviousResponseID round-trip still pending
- [ ] **RESP-5** Soak two clean nightlies, then delete the Chat Completions path

## Backlog

### Worktree durability: clean up orphaned branches

When a matrix cell crashes after `git worktree add -b` but before state is
saved, the local branch is left behind. The next run fails on `AddWorktree`
because `-b` rejects an existing branch. Spec:
[`docs/specs/2026-04-29-branch-orphan-cleanup.md`](docs/specs/2026-04-29-branch-orphan-cleanup.md).

- [ ] **BRANCH-1** `AddWorktree`: detect local branch exists → remove orphaned worktree + delete branch → retry fresh
- [ ] **BRANCH-2** Tests: `TestAddWorktree_CleansOrphanedBranch` + update existing "rejects" test to expect recovery

### Label-based merge: `merge-approved` → auto-merge on next nightly

Add `merge-approved` label to any burndown PR to signal it's ready to
land. The next nightly run calls AutoMerge on it automatically, so a human
reviewer (or a review bot) can approve without waiting for a new dispatch
cycle. Implemented alongside RECONCILE.

- [ ] **MERGE-1** Document `merge-approved` label in README/wiki so reviewers know to use it

### GitHub issue creation for blocked tasks

Blocked tasks create a draft PR only. Draft PRs get batch-closed during
cleanups, making blocked tasks invisible. A GitHub issue provides a
persistent, filterable, undeniable artifact in the issue tracker. Spec:
[`docs/specs/2026-04-29-blocked-issue-creation.md`](docs/specs/2026-04-29-blocked-issue-creation.md).

- [ ] **ISSUE-1** `ghops.Publisher.CreateIssue` + `RepoPublisher` interface extension + `ensureBurndownBlockedLabel` helper
- [ ] **ISSUE-2** `publishOutcome`: call `CreateIssue` when `reported == "blocked"`; `buildBlockedIssueBody` with source link, reason, agent summary, PR ref

## Recently completed

- **State reconciliation + label-merge** (this PR) — `ReconcileFromGitHub` patches state holes from crashed/incomplete prior runs; `mergeApprovedPRs` merges PRs carrying `merge-approved` on every nightly; hash-key bug fixed in runner; 5 reconcile tests added
- **Model tier selection** (`e6ece17`) — `config.ModelTier` + `LLMFeatureConfig.SelectModel(complexity)` maps triage score (1–5) to a model before the agent loop; replaces runtime fallback chain
- **Model fallback chain** (`3379dfa`) — codex-mini → gpt-5.3-codex → gpt-5 (superseded by tier selection)
- **Responses API migration** (`2ea6fdd`) — `RunOpenAIResponses` with `PreviousResponseID` threading; codex-mini as primary
- **CI cache resilience** (`2d90b26`) — replaced `setup-go cache:true` (hard-fails on HTTP 400) with explicit `actions/cache/restore` + `actions/cache/save` with `continue-on-error`
- **ST1011 fix** (`24a0ed3`) — renamed `jitterMs→jitter` in `openai.go` + `openai_responses.go`
- **Generic packages 404 fix** (`b2a56e0`) — `packages.registries.github: false` in `.github/repository-config.yml`
- **Auth fix for git push** (`5e1023d`) — clear inherited `extraheader` before push in `ghops`
- **Per-task state files** (`5cb7840`) — `state/tasks/<hash>.json`; 7-day dedup prevents re-dispatching tasks with open PRs
- **Status self-report** (`5d071d5`) — agent calls `report_status`; harness sets PR labels and draft/ready accordingly
- **review mode** (`b4a9176`) — ready-for-review PRs, no CI watch or auto-merge
- **Durability specs** (`ee6d8ee`) — BRANCH/RECONCILE/ISSUE specs + TODO entries (backlog)

_(older entries: see CHANGELOG.md)_
