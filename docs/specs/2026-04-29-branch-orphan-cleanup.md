<!-- file: docs/specs/2026-04-29-branch-orphan-cleanup.md -->
<!-- version: 1.0.0 -->
<!-- guid: a1b2c3d4-e5f6-7890-abcd-ef1234567890 -->

# Idempotent worktree creation: clean up orphaned branches

**Status:** Ready for bot pickup
**Owner:** burndown maintainer
**Tracking:** BRANCH-1, BRANCH-2

## Why

`AddWorktree` always runs `git worktree add -b <branch> <path>`, which
creates a *new* local branch from HEAD. If a previous run crashed after
creating the worktree but before saving state (e.g. the matrix cell was
killed mid-flight), the local branch already exists the next night. The
`-b` flag is then rejected by git:

```
fatal: A branch named 'auto/fix-readme-typo' already exists.
exit status 128
```

The outcome is `StatusFailed`, the task is re-queued, and the orphaned
branch stays forever. Each crash compounds: N crashes → N orphaned
branches with incrementing numeric suffixes (`auto/fix-readme-typo-2`,
`auto/fix-readme-typo-3`, …) as `uniqueBranchNames` deduplicates.

Root cause: the dispatcher has no concept of "the branch I want already
exists; decide what to do." There is no state row (it wasn't saved), so
`filterFreshTasks` passes the task through. `AddWorktree` then hard-fails.

The test `TestAddWorktree_RejectsExistingBranch` in
`internal/dispatch/worktree_test.go` documents the current behaviour as
*expected* — the spec changes what "expected" means.

## What "orphaned" means here

An orphaned branch is a local branch that:
- Was created by a previous `AddWorktree` call
- Has no open PR associated with it (if there were an open PR, state
  would have been saved and `filterFreshTasks` would have skipped the
  task before we ever reached `AddWorktree`)
- Has no commits beyond the fork point (the driver commits; the agent
  only edits files via MCP)

Because the agent never commits, the worktree's content is either:
- Empty/HEAD-only (agent didn't run)
- Contains un-committed edits (agent ran, then crash happened before
  `publishOutcome` called `CommitAndPush`)

In both cases, starting fresh is the right call. The agent will
re-generate the same edits from scratch, and the second run will
complete normally.

## Scope

One file: `internal/dispatch/worktree.go`.
One file: `internal/dispatch/worktree_test.go`.

No changes to `dispatch.go`, `runner.go`, or state.

## Implementation plan

### BRANCH-1 — Idempotent `AddWorktree`

Add two helpers and change the creation logic:

```go
// localBranchExists reports whether `branch` exists as a local ref in
// `repoPath`. Uses rev-parse --verify which exits 0 iff the ref exists.
func localBranchExists(ctx context.Context, repoPath, branch string) bool {
    cmd := exec.CommandContext(ctx, "git", "-C", repoPath,
        "rev-parse", "--verify", "--quiet", branch)
    return cmd.Run() == nil
}

// worktreeDirRegistered reports whether `path` is already listed in the
// repo's worktree index (i.e. a previous `git worktree add` ran there).
// Uses `git worktree list --porcelain` and scans for the path.
func worktreeDirRegistered(ctx context.Context, repoPath, path string) bool {
    out, err := exec.CommandContext(ctx, "git", "-C", repoPath,
        "worktree", "list", "--porcelain").Output()
    if err != nil {
        return false
    }
    clean := filepath.Clean(path)
    for _, line := range strings.Split(string(out), "\n") {
        if strings.HasPrefix(line, "worktree ") {
            if filepath.Clean(strings.TrimPrefix(line, "worktree ")) == clean {
                return true
            }
        }
    }
    return false
}
```

Then change `AddWorktree` to detect and clean up before creating:

```go
func AddWorktree(ctx context.Context, repoPath, branch, path string, excludePaths ...string) (*Worktree, error) {
    // If the branch already exists from a prior crashed run, clean it up
    // so we can start fresh. There is no open PR for this task (if there
    // were, filterFreshTasks would have skipped it and we'd never be here),
    // so the orphaned branch has no useful state worth preserving.
    if localBranchExists(ctx, repoPath, branch) {
        if worktreeDirRegistered(ctx, repoPath, path) {
            _ = RemoveWorktree(ctx, repoPath, path)
        }
        if err := DeleteBranch(ctx, repoPath, branch); err != nil {
            return nil, fmt.Errorf("clean orphaned branch %q: %w", branch, err)
        }
        fmt.Fprintf(os.Stderr,
            "dispatch: cleaned orphaned branch %q before retry\n", branch)
    }

    cmd := exec.CommandContext(ctx, "git", "-C", repoPath, "worktree", "add", "-b", branch, path)
    if out, err := cmd.CombinedOutput(); err != nil {
        return nil, fmt.Errorf("git worktree add: %w (output: %s)", err, strings.TrimSpace(string(out)))
    }
    // … sparse-checkout unchanged …
}
```

Note: `os` is already an implicit import via `exec`; add it explicitly.

### BRANCH-2 — Tests

Update `TestAddWorktree_RejectsExistingBranch` to assert the *new*
behaviour — the second `AddWorktree` call should **succeed** and the
orphaned branch should be gone after the first call:

```go
func TestAddWorktree_CleansOrphanedBranch(t *testing.T) {
    repo := gitInitWithCommit(t)
    wt1 := filepath.Join(t.TempDir(), "wt1")

    // First call creates branch.
    if _, err := AddWorktree(ctx, repo, "auto/x", wt1); err != nil {
        t.Fatalf("first AddWorktree: %v", err)
    }
    // Simulate crash: remove the worktree directory (as if the process died
    // mid-flight and the OS cleaned up the directory but not the git index).
    _ = RemoveWorktree(ctx, repo, wt1)

    // Second call with the same branch should clean up and succeed.
    wt2 := filepath.Join(t.TempDir(), "wt2")
    if _, err := AddWorktree(ctx, repo, "auto/x", wt2); err != nil {
        t.Fatalf("second AddWorktree (orphan recovery): %v", err)
    }
    if _, err := os.Stat(wt2); err != nil {
        t.Errorf("worktree path should exist: %v", err)
    }
}
```

Also add a test that verifies the branch is re-created fresh (not at a
stale commit): after recovery, `git rev-parse auto/x` should equal HEAD
of the parent repo.

Keep the existing `TestAddWorktree_RejectsExistingBranch` test but rename
it to `TestAddWorktree_CleansUpInsteadOfRejecting` with updated assertions.

## Definition of Done

- [ ] **BRANCH-1** `AddWorktree` cleans orphaned local branches before
  retry; logs a single stderr line for observability.
- [ ] **BRANCH-2** `TestAddWorktree_CleansOrphanedBranch` passes;
  `TestAddWorktree_CleansUpInsteadOfRejecting` replaces the old
  "rejects" test; `go test ./internal/dispatch/...` green.
- [ ] No changes to any file outside `internal/dispatch/`.
- [ ] CHANGELOG entry.

## Risk + rollback

- **False-positive cleanup:** The guard (`localBranchExists`) fires only
  when state has no record of an open PR for this task (because
  `filterFreshTasks` would have skipped it otherwise). There is no
  scenario where we'd clean up a branch that has a live PR.
- **Race with concurrent cells:** Two cells with the same branch (which
  shouldn't happen given `uniqueBranchNames`, but could if a bug
  produced duplicates) would both try to clean up. `DeleteBranch`
  returns an error if the branch is already gone; the second cell would
  then try `AddWorktree -b` and fail with "branch already exists" (from
  the first cell re-creating it). This is the same failure mode as
  today — no regression. If we want to tolerate it, we can make the
  "branch already exists" error from the second `worktree add` a
  non-fatal retry; out of scope for this spec.
- **Rollback:** The only change is pre-flight cleanup before a call that
  was already failing. Reverting just restores the hard-fail behaviour.

## References

- [`internal/dispatch/worktree.go`](../../internal/dispatch/worktree.go) — `AddWorktree`
- [`internal/dispatch/worktree_test.go`](../../internal/dispatch/worktree_test.go) — existing tests
- [`internal/dispatch/dispatch.go:174`](../../internal/dispatch/dispatch.go) — call site
