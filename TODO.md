<!-- file: TODO.md -->
<!-- version: 1.1.0 -->
<!-- guid: 9e3a4b5c-6d7e-8f9a-0b1c-2d3e4f5a6b7c -->

# overnight-burndown — TODO

Canonical index of outstanding work. Details live in linked specs.

## Active

### OpenAI Responses API migration

Chat Completions is in maintenance. Newer models (codex-mini, gpt-5.4)
ship on `/v1/responses` only or first. Plus `PreviousResponseID`
collapses prompt-token cost for our 5–15 iteration agent loop. Spec:
[`docs/specs/2026-04-29-responses-api-migration.md`](docs/specs/2026-04-29-responses-api-migration.md).

- [ ] **RESP-1** Add `RunOpenAIResponses` alongside `RunOpenAI`; gate via config
- [ ] **RESP-2** Migrate `internal/triage/openai.go` to Responses (single forced tool call — lowest risk first)
- [ ] **RESP-3** Default `implementer.api=responses` in `render-ci-config.py`
- [ ] **RESP-4** Tests: mocked Responses round-trip incl. multi-iter `PreviousResponseID` threading
- [ ] **RESP-5** Soak two clean nightlies, then delete the Chat Completions path

## Backlog

### Worktree durability: clean up orphaned branches

When a matrix cell crashes after `git worktree add -b` but before state is
saved, the local branch is left behind. The next run fails on `AddWorktree`
because `-b` rejects an existing branch. Spec:
[`docs/specs/2026-04-29-branch-orphan-cleanup.md`](docs/specs/2026-04-29-branch-orphan-cleanup.md).

- [ ] **BRANCH-1** `AddWorktree`: detect local branch exists → remove orphaned worktree + delete branch → retry fresh
- [ ] **BRANCH-2** Tests: `TestAddWorktree_CleansOrphanedBranch` + update existing "rejects" test to expect recovery

### State reconciliation from GitHub

If state artifact upload fails in the matrix workflow, the next nightly
re-dispatches tasks that already have open PRs, creating duplicates.
GitHub is the authoritative source of truth; query it on startup to patch
the hole. Spec:
[`docs/specs/2026-04-29-state-reconciliation.md`](docs/specs/2026-04-29-state-reconciliation.md).

- [ ] **RECONCILE-1** Add `ReconcileFromGitHub`: query open `automation`-labeled PRs → upsert `StatusDraft` rows for any missing from local state
- [ ] **RECONCILE-2** Call reconcile in `runRepo` before `filterFreshTasks`; best-effort (log + continue on API error)
- [ ] **RECONCILE-3** Tests: happy path, idempotency, non-burndown PRs ignored, pagination, API error tolerance

### GitHub issue creation for blocked tasks

Blocked tasks create a draft PR only. Draft PRs get batch-closed during
cleanups, making blocked tasks invisible. A GitHub issue provides a
persistent, filterable, undeniable artifact in the issue tracker. Spec:
[`docs/specs/2026-04-29-blocked-issue-creation.md`](docs/specs/2026-04-29-blocked-issue-creation.md).

- [ ] **ISSUE-1** `ghops.Publisher.CreateIssue` + `RepoPublisher` interface extension + `ensureBurndownBlockedLabel` helper
- [ ] **ISSUE-2** `publishOutcome`: call `CreateIssue` when `reported == "blocked"`; `buildBlockedIssueBody` with source link, reason, agent summary, PR ref

## Recently completed

_(see CHANGELOG.md)_
