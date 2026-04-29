<!-- file: docs/specs/2026-04-29-state-reconciliation.md -->
<!-- version: 1.0.0 -->
<!-- guid: b2c3d4e5-f6a7-8901-bcde-f12345678901 -->

# State reconciliation from GitHub on startup

**Status:** Ready for bot pickup
**Owner:** burndown maintainer
**Tracking:** RECONCILE-1, RECONCILE-2, RECONCILE-3

## Why

The burndown uses a per-task state file (`state/tasks/<hash>.json`) to
avoid re-dispatching tasks that already have an open PR. The matrix
workflow uploads these files as GitHub Actions artifacts at the end of
each cell, and the aggregate job downloads them before rendering the
digest.

Two real failure modes break the dedup guarantee:

### Mode 1: Artifact upload failure

A dispatch matrix cell crashes after creating a PR but before
`state.SaveTask` flushes to disk (or before the artifact upload step
runs). The next nightly has no state row for the task, so
`filterFreshTasks` passes it through and a duplicate PR is opened on
the same (or a freshly-named) branch.

### Mode 2: Partial artifact download

The aggregate step downloads per-task JSON files from each cell.
If one cell's artifact expires, is rate-limited, or times out, the
corresponding task rows are silently absent from the next run's loaded
state. Same result: duplicate PRs.

In both cases, **GitHub is the source of truth** for what work has been
done — open PRs tagged `automation` represent tasks in progress. The
local state file is just a cache.

## Scope

New function `ReconcileFromGitHub` in `internal/state/` (or
`internal/runner/`).

Modified `Runner.runRepo` to call it before `filterFreshTasks`.

The function requires a `*github.Client` (already present in `runRepo`)
and repo owner/name.

No changes to dispatch, ghops, digest, or the agent path.

## Implementation plan

### RECONCILE-1 — `ReconcileFromGitHub` function

Add to `internal/runner/runner.go` (or a new
`internal/runner/reconcile.go`):

```go
// ReconcileFromGitHub queries the target repo for all open PRs bearing
// the "automation" label (applied to every burndown PR by
// applyBurndownLabels) and upserts a state row for each one. This
// patches holes left by crash-before-state-save and artifact-upload
// failures.
//
// Each reconciled row is written with StatusDraft (an open PR = not yet
// terminal) so filterFreshTasks skips it. The branch name stored in the
// PR head ref is used as the state row's Branch field; the PR number and
// URL are stored verbatim.
//
// A row is only upserted if no row with the same branch already exists in
// s — existing rows are authoritative (they may carry more-specific status
// like StatusShipped). This keeps reconcile idempotent.
//
// The task hash for reconciled rows is derived from the branch name
// (stable across runs for the same branch) rather than from
// source+content, because we don't have the source task at reconcile time.
// filterFreshTasks deduplicates by hash, so this is sufficient to block
// re-dispatch. The full source hash will be written by the next successful
// dispatch cell that touches this task.
func ReconcileFromGitHub(
    ctx context.Context,
    gh *github.Client,
    s *state.State,
    owner, repo string,
) error {
    opts := &github.PullRequestListOptions{
        State: "open",
        ListOptions: github.ListOptions{PerPage: 100},
    }
    for {
        prs, resp, err := gh.PullRequests.List(ctx, owner, repo, opts)
        if err != nil {
            return fmt.Errorf("reconcile: list PRs: %w", err)
        }
        for _, pr := range prs {
            if !prHasBurndownLabel(pr) {
                continue
            }
            branch := pr.GetHead().GetRef()
            if branch == "" {
                continue
            }
            hash := branchHash(branch) // deterministic: sha256(branch)
            if _, exists := s.Get(hash); exists {
                continue // local state wins
            }
            s.Upsert(&state.TaskState{
                Hash:        hash,
                Branch:      branch,
                PRNumber:    pr.GetNumber(),
                PRURL:       pr.GetHTMLURL(),
                Status:      state.StatusDraft,
                Source:      state.Source{Type: state.SourceUnknown, URL: pr.GetHTMLURL()},
                LastUpdated: pr.GetUpdatedAt().Time,
            })
            fmt.Fprintf(os.Stderr,
                "runner: reconciled PR #%d (%s) from GitHub\n",
                pr.GetNumber(), branch)
        }
        if resp.NextPage == 0 {
            break
        }
        opts.Page = resp.NextPage
    }
    return nil
}

// prHasBurndownLabel returns true when the PR carries the "automation"
// label that applyBurndownLabels puts on every burndown-opened PR.
func prHasBurndownLabel(pr *github.PullRequest) bool {
    for _, l := range pr.Labels {
        if l.GetName() == "automation" {
            return true
        }
    }
    return false
}

// branchHash produces a stable state hash from a branch name alone.
// Used for reconciled rows where the original source task is unavailable.
func branchHash(branch string) string {
    sum := sha256.Sum256([]byte("branch\x00" + branch))
    return hex.EncodeToString(sum[:])
}
```

Add `SourceUnknown SourceType = "unknown"` to `internal/state/state.go`
so reconciled rows have a valid source type.

### RECONCILE-2 — Wire into `runRepo`

In `Runner.runRepo`, call `ReconcileFromGitHub` after the GitHub client is
built and before `filterFreshTasks`:

```go
// Reconcile: patch any state holes from crashed/incomplete prior runs
// by querying open burndown PRs on GitHub before filtering.
// Best-effort: a reconcile failure (e.g. rate limit) is logged but
// does not abort the run — we'd rather re-dispatch a task than skip
// the whole repo.
if r.Config.GitHub.AppID != 0 {
    if err := ReconcileFromGitHub(ctx, ghClient, r.State,
            repoCfg.Owner, repoCfg.Name); err != nil {
        fmt.Fprintf(os.Stderr, "runner: reconcile: %v\n", err)
    }
}

tasks = r.filterFreshTasks(tasks)
```

The guard on `AppID != 0` keeps dry-run and test configs from hitting
GitHub when they don't have auth. Tests that inject a fake `ghClient` can
still exercise the reconcile path.

### RECONCILE-3 — Tests

Add `TestReconcileFromGitHub` in `internal/runner/reconcile_test.go` (or
alongside runner_test.go):

1. **Happy path:** stub `gh.PullRequests.List` to return two PRs with the
   `automation` label. Assert that `state.Get(branchHash(branch))` returns
   `StatusDraft` for both after reconcile.
2. **Idempotency:** call reconcile twice; assert no duplicate state rows,
   and that a pre-existing row with `StatusShipped` is not overwritten.
3. **Non-burndown PRs ignored:** a PR without the `automation` label does
   not create a state row.
4. **Pagination:** stub two pages of PRs; assert rows from both pages are
   present.
5. **API error — best-effort:** reconcile returns an error; caller logs
   and continues (tested via `runRepo` integration test if feasible, else
   unit-tested via direct call).

Also: add a `filterFreshTasks` integration test that verifies a task
whose branch hash matches a reconciled row is dropped.

## Definition of Done

- [ ] **RECONCILE-1** `ReconcileFromGitHub` is implemented and compiles.
  `SourceUnknown` added to `state.go`.
- [ ] **RECONCILE-2** `Runner.runRepo` calls reconcile before
  `filterFreshTasks`; best-effort error handling in place.
- [ ] **RECONCILE-3** All five test cases pass; `go test ./internal/...` green.
- [ ] Duplicate-PR scenario is prevented: a task whose branch has an open
  `automation`-labeled PR will not be re-dispatched.
- [ ] CHANGELOG entry.

## Risk + rollback

- **Rate limits:** `PullRequests.List` counts against the App installation's
  5,000 req/hr quota. One list call per repo per night (paginated at 100
  PRs) is negligible. If the repo has thousands of open burndown PRs
  (a bug in its own right), pagination overhead could spike; add a max-pages
  guard (`if page > 10 { break }`) if needed.
- **Label dependency:** reconcile relies on the `automation` label applied
  by `applyBurndownLabels`. If a prior burndown version didn't apply this
  label (e.g. runs before the label feature landed), those PRs won't be
  reconciled. Acceptable — the gap only affects runs from before that
  feature, which is short-lived historical state.
- **Branch-hash vs source-hash mismatch:** reconciled rows use
  `branchHash(branch)` as key, while normal rows use `HashTask(source)`.
  If the same task completes normally on the next run, it creates a second
  state row under its source hash (a different key), while the reconciled
  row remains. Both rows are present; `filterFreshTasks` deduplicates by
  source hash only, so the reconciled row becomes an orphan. It will age
  out in 7 days when its `LastUpdated` crosses the `pendingCutoff`.
  Acceptable for v1; a follow-up can add reconcile-key cleanup.
- **Rollback:** Remove the `ReconcileFromGitHub` call in `runRepo`. The
  function itself is harmless to leave in place.

## References

- [`internal/runner/runner.go:filterFreshTasks`](../../internal/runner/runner.go)
  — where reconciled rows prevent re-dispatch
- [`internal/state/state.go:InFlight`](../../internal/state/state.go)
  — existing "open PR" query (reconcile adds rows that satisfy this)
- [`internal/runner/runner.go:applyBurndownLabels`](../../internal/runner/runner.go)
  — where the `automation` label is applied (the anchor for reconcile)
- go-github `PullRequests.List`: `github.com/google/go-github/v84/github`
