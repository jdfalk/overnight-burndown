# Example TODO.md format

The burndown driver picks up unchecked markdown task-list items from a
repo's `TODO.md` at the repo root. Format conventions:

- Use `- [ ]` for unchecked tasks (any list marker works: `-`, `*`, `+`, `1.`)
- Mark with `[auto-ok]` to opt the task into auto-merge eligibility
- Indent continuation lines two spaces; they fold into the task body
- Already-checked items (`- [x]`) are skipped

## Examples

- [ ] [auto-ok] Fix typo in README.md line 42 — "recieve" should be "receive"
- [ ] [auto-ok] Add CHANGELOG entry for v1.2.0 release
  Should describe the new caching behavior added in PR #341.
- [ ] Refactor the dispatcher to pull settings from etcd instead of env vars
- [x] Already-shipped task — burndown ignores this
- [ ] [auto-ok] Pin actions/checkout to commit SHA in workflows
  Note: this might trip the workflows/** hard-veto since safe-ai-util
  blocks edits to .github/workflows/**. The triage agent will see the
  context and likely classify it NEEDS_REVIEW anyway.

## What happens

For each unchecked item, the triage agent classifies it as:

- **AUTO_MERGE_SAFE** → opens PR, watches CI, auto-merges on green if
  every B2 gate passes (path allowlist, no forced-review paths, diff
  size under cap).
- **NEEDS_REVIEW** → opens draft PR for human review the next morning.
- **BLOCKED** → no PR; surfaces in the digest with the reason.

The `[auto-ok]` marker is one of four AND'd gates required for
auto-merge. Without it, the most a task can land is a draft PR.
