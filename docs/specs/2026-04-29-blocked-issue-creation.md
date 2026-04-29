<!-- file: docs/specs/2026-04-29-blocked-issue-creation.md -->
<!-- version: 1.0.0 -->
<!-- guid: c3d4e5f6-a7b8-9012-cdef-123456789012 -->

# Create a GitHub issue for every blocked task

**Status:** Ready for bot pickup
**Owner:** burndown maintainer
**Tracking:** ISSUE-1, ISSUE-2

## Why

When an agent reports `status=blocked`, `publishOutcome` opens a **draft
PR** titled `BLOCKED: <task>`. The draft PR contains the agent's blocking
reason and is labeled `status:blocked`. This is the *only* persistent
artifact.

The problem: **draft PRs are ephemeral in practice.**

- A human glancing at the repo sees 20 WIP draft PRs and batch-closes them
  as noise, wiping all visibility into what the bot tried and failed at.
- If the PR is merged without the block being resolved (shouldn't happen
  with a draft, but stale PRs do get merged during squash-all cleanups),
  the task appears "done" when it isn't.
- Filtering the issue tracker for "things that need human attention" shows
  nothing — burndown blocks don't appear there, they hide in the PR list.

A GitHub **issue** is the right artifact for a blocked task:
- Issues survive PR cleanups.
- Issues are searchable/filterable by label (`burndown-blocked`).
- Issues support `@mention` routing — the bot can assign the issue to the
  repo's code owner or leave it unassigned for triage.
- Issues stay open until explicitly closed, making blocked tasks
  undeniable backlog items rather than opt-in PR watches.

The PR should still be created (it carries the diff, however partial), but
the *issue* becomes the tracking artifact.

## Scope

- New method `CreateIssue` on `ghops.Publisher` and on the `RepoPublisher`
  interface in `runner.go`.
- `publishOutcome` calls `CreateIssue` when `reported == "blocked"`.
- Issue body links to the PR (if one was opened), includes the blocking
  reason, and cites the source task URL.
- New label `burndown-blocked` on the target repo (created lazily on first
  use; `EnsureLabel` helper already exists or should be added).

No changes to dispatch, state, triage, or digest.

## Implementation plan

### ISSUE-1 — `CreateIssue` in ghops

Add to `internal/ghops/publish.go`:

```go
// IssueOptions describes a GitHub issue to open for a blocked task.
type IssueOptions struct {
    Title  string
    Body   string
    Labels []string
}

// CreateIssue opens a GitHub issue. Returns the issue number and URL.
func (p *Publisher) CreateIssue(ctx context.Context, opts IssueOptions) (int, string, error) {
    req := &github.IssueRequest{
        Title:  github.String(opts.Title),
        Body:   github.String(opts.Body),
        Labels: &opts.Labels,
    }
    issue, _, err := p.gh.Issues.Create(ctx, p.owner, p.repo, req)
    if err != nil {
        return 0, "", fmt.Errorf("create issue: %w", err)
    }
    return issue.GetNumber(), issue.GetHTMLURL(), nil
}
```

Add `CreateIssue` to the `RepoPublisher` interface in
`internal/runner/runner.go`:

```go
type RepoPublisher interface {
    // … existing methods …
    CreateIssue(ctx context.Context, opts ghops.IssueOptions) (number int, url string, err error)
}
```

Add a `ensureBurndownBlockedLabel` helper that calls
`gh.Issues.GetLabel`; if the label is absent (404), calls
`gh.Issues.CreateLabel` with color `#e11d48` (red) and description
`"Task blocked by the overnight-burndown bot"`. Called once per blocked
task, best-effort.

### ISSUE-2 — Wire into `publishOutcome`

In `Runner.publishOutcome`, after the PR is opened and before the
`ModeDraftOnly` early-return:

```go
// Blocked tasks get a GitHub issue for persistent tracking.
// Draft PRs are routinely batch-closed; issues are not.
// Best-effort: issue creation failure is logged but does not fail
// the publish sequence — the draft PR is still visible.
if reported == "blocked" {
    _ = ensureBurndownBlockedLabel(ctx, pub)
    issueTitle := "burndown blocked: " + strings.TrimPrefix(buildPRTitle(oc, reported), "BLOCKED: ")
    issueBody  := buildBlockedIssueBody(oc, prNum, prURL)
    issueNum, issueURL, err := pub.CreateIssue(ctx, ghops.IssueOptions{
        Title:  issueTitle,
        Body:   issueBody,
        Labels: []string{"burndown-blocked"},
    })
    if err != nil {
        fmt.Fprintf(os.Stderr, "burndown: create blocked issue for PR #%d: %v\n", prNum, err)
    } else {
        fmt.Fprintf(os.Stderr, "burndown: created blocked issue #%d: %s\n", issueNum, issueURL)
        // Store issue URL in state for the digest (future: link from digest).
        _ = issueURL // currently unused by digest; tracked by ISSUE-3 (future spec)
    }
}
```

Add `buildBlockedIssueBody` in `runner.go`:

```go
func buildBlockedIssueBody(oc *dispatch.Outcome, prNum int, prURL string) string {
    var b strings.Builder
    fmt.Fprintf(&b, "## Blocked task\n\n")
    fmt.Fprintf(&b, "**Source:** %s\n\n", sourceLink(oc.Task.Source))
    fmt.Fprintf(&b, "**Triage classification:** %s\n\n", oc.Decision.Classification)
    if prNum > 0 {
        fmt.Fprintf(&b, "**Draft PR:** [#%d](%s) (contains partial diff if any)\n\n", prNum, prURL)
    }
    if oc.AgentResult != nil {
        if oc.AgentResult.ReportedReason != "" {
            fmt.Fprintf(&b, "## Why blocked\n\n%s\n\n", oc.AgentResult.ReportedReason)
        }
        if oc.AgentResult.Summary != "" {
            fmt.Fprintf(&b, "## Agent summary\n\n%s\n\n", oc.AgentResult.Summary)
        }
    }
    fmt.Fprintf(&b, "## Next steps\n\n")
    fmt.Fprintf(&b, "- [ ] Read the agent summary above and the draft PR diff\n")
    fmt.Fprintf(&b, "- [ ] Provide missing context or fix the blocker\n")
    fmt.Fprintf(&b, "- [ ] Close this issue once the task is resolved or the TODO item is removed\n\n")
    fmt.Fprint(&b, "---\n_Opened by overnight-burndown._\n")
    return b.String()
}
```

### Dedup: avoid duplicate issues on re-runs

`filterFreshTasks` already prevents re-dispatch of tasks with
`StatusDraft` within 7 days, so `CreateIssue` shouldn't fire for the
same task twice. But if state is lost (RECONCILE spec covers this),
reconciled rows come back as `StatusDraft` (no issue URL tracked) and
`filterFreshTasks` drops them — no duplicate.

If state is fully wiped and the task re-dispatches, a second issue would
be created. Dedup guard: before `CreateIssue`, search open issues with
`gh.Search.Issues` for `"burndown blocked" in:title label:burndown-blocked`
filtered by the task title. If a match exists, skip. This is ISSUE-3
(future spec); for now, duplicate issues are acceptable (can be manually
closed).

### Tests

Add to `internal/runner/publish_test.go` (or a new
`internal/runner/issue_test.go`):

1. **Blocked task creates issue:** stub `pub.CreateIssue` to capture the
   call; assert it is invoked exactly once when `reported == "blocked"`.
   Assert issue title starts with `"burndown blocked: "`.
2. **Issue body includes PR link:** assert `#<prNum>` and `prURL` appear
   in the issue body.
3. **Issue body includes blocking reason:** set `AgentResult.ReportedReason`;
   assert it appears in the issue body section "Why blocked".
4. **Non-blocked tasks do not create issues:** `reported == "complete"` and
   `reported == "partial"` — assert `CreateIssue` is not called.
5. **Issue creation failure is non-fatal:** stub `CreateIssue` to return an
   error; assert `publishOutcome` still returns nil (PR was opened, issue is
   best-effort).

Update the `RepoPublisher` stub in existing tests to add a no-op
`CreateIssue` method.

## Definition of Done

- [ ] **ISSUE-1** `Publisher.CreateIssue` implemented; `RepoPublisher`
  interface updated; `ensureBurndownBlockedLabel` helper in place.
- [ ] **ISSUE-2** `publishOutcome` calls `CreateIssue` for blocked tasks;
  `buildBlockedIssueBody` formats the issue body with source link,
  blocking reason, agent summary, and draft PR reference.
- [ ] All five test cases pass; `go test ./internal/...` green.
- [ ] Issue is created in a real nightly run for a blocked task (verified
  by checking the target repo's issue tracker the morning after).
- [ ] `burndown-blocked` label is created on first use without manual setup.
- [ ] CHANGELOG entry.

## Risk + rollback

- **Issue noise:** If the bot is dispatching many tasks and many are
  blocked, the issue tracker fills up. Mitigations: (a) `burndown-blocked`
  label makes them easy to bulk-close; (b) the RECONCILE dedup means the
  same task won't re-open an issue for 7 days even with state loss.
- **Label creation permission:** `EnsureLabel` requires `issues: write`
  on the token. The App already has this permission (it opens PRs). No
  new permissions needed.
- **Rollback:** Remove the `CreateIssue` call from `publishOutcome`. Blocked
  tasks revert to draft-PR-only behaviour. The `burndown-blocked` label
  and any existing issues remain harmlessly.
- **Interface break:** Adding `CreateIssue` to `RepoPublisher` requires all
  test stubs to implement it. Add a no-op stub implementation to the
  existing `fakePublisher` in runner_test.go before merging.

## Future: ISSUE-3 (out of scope for this spec)

- Store the created issue URL in `TaskState` so the digest can render
  "Blocked — see issue #42" instead of just "blocked".
- Dedup guard: search existing open issues before creating to prevent
  duplicates on state-wiped re-runs.
- Auto-close the issue when the PR is eventually merged (follow-up on
  the PR merge event or a separate nightly sweep).

## References

- [`internal/runner/runner.go:publishOutcome`](../../internal/runner/runner.go)
  — call site for `CreateIssue`
- [`internal/ghops/publish.go`](../../internal/ghops/publish.go)
  — `Publisher` struct; add `CreateIssue` here
- [`internal/runner/runner.go:RepoPublisher`](../../internal/runner/runner.go)
  — interface to extend
- go-github Issues API: `gh.Issues.Create`, `gh.Issues.GetLabel`,
  `gh.Issues.CreateLabel`
