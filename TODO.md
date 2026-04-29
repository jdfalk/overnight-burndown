<!-- file: TODO.md -->
<!-- version: 1.0.0 -->
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

_(empty — track non-trivial future work here)_

## Recently completed

_(see CHANGELOG.md)_
