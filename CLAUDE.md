<!-- file: CLAUDE.md -->
<!-- version: 1.0.0 -->
<!-- guid: b9c4d7e2-3a1f-4e85-8d62-7f0b5c9a3e17 -->
<!-- last-edited: 2026-06-13 -->

# overnight-burndown

Launchd-driven nightly automation that drains a queue of small, safe work items
across configured GitHub repos. Language: Go. Requires Go 1.25+.

## Coding Standards

Org-wide coding standards are in the `.standards/` git submodule (cloned from
`https://github.com/falkcorp/.github`).
Always clone with `git clone --recurse-submodules` so these are available.

Key files:
- **File headers (MANDATORY):** `.standards/instructions/file-headers.md`
- **Go rules:** `.standards/instructions/go.md`
- **Commit format:** `.standards/instructions/commit-messages.md`
