<!-- file: docs/specs/2026-04-29-responses-api-migration.md -->
<!-- version: 1.0.0 -->
<!-- guid: 7e1f2a3b-4c5d-6e7f-8a9b-0c1d2e3f4a5b -->

# Migrate burndown to the OpenAI Responses API

**Status:** Draft, ready for bot pickup
**Owner:** burndown maintainer
**Reference:** [OpenAI migration guide](https://developers.openai.com/api/docs/guides/migrate-to-responses)

## Why

Today the burndown's implementer agent and triage classifier both call
`/v1/chat/completions`. The Responses API (`/v1/responses`) supersedes
Chat Completions for *all new development*; OpenAI is shipping the
newest models there first (codex-mini already requires it), and the
shape gives us four direct wins:

1. **Stateful conversations.** `previous_response_id` resumes a thread
   server-side instead of resending the full message history every
   iteration. The implementer loop currently sends ever-growing message
   arrays — by iter 6 we're paying ~30K prompt tokens per call. With
   Responses, only the *new* user/tool messages are uploaded each turn.
2. **Built-in tool framework.** `tools` accepts function defs the same
   way Chat Completions does, but the model also has direct access to
   `code_interpreter`, `web_search`, and `file_search` without us
   plumbing them through MCP. We don't need them yet, but the door
   opens.
3. **Reasoning controls.** `reasoning.effort` and `reasoning.summary`
   are first-class. The o-series and gpt-5 reasoning models give better
   results when we can dial effort up for hard tasks and down for
   triage.
4. **Newer-model access.** `gpt-5.1-codex-mini`, `gpt-5.4-mini`, the
   o-series — many recent models are Responses-only or
   Responses-preferred. We hit this directly when we tried codex-mini
   for the implementer and got `404 — model is only supported in
   v1/responses`.

## Scope

Two files in this repo do real OpenAI calls:

- `internal/agent/openai.go` — `RunOpenAI`. Implementer loop. Chat
  Completions with function tools. ~240 lines. Hot path; every
  dispatch cell calls it 5–15 times.
- `internal/triage/openai.go` — `Provider.Triage`. Classifies tasks +
  emits a structured branch suggestion. Single Chat Completions call
  per repo per night with a single forced tool-call.

`internal/agent/openai_retry_test.go` and the run-config knob in
`scripts/render-ci-config.py` will need follow-on tweaks (the Responses
endpoint also returns 429s; the model picker may want to default to
gpt-5.4-mini or o4-mini once migrated).

The Anthropic path (`agent.Run`) is **not** in scope — Responses is
OpenAI-only.

## Out of scope

- Audiobook-organizer migration — covered by its own spec.
- Migrating `internal/agent/agent.go` (the Anthropic path).
- Adding `web_search` / `file_search` / `code_interpreter` builtin
  tools — useful future work, but a separate spec.
- Streaming responses — the burndown is batch / non-interactive.

## API mapping (cheat sheet)

| Chat Completions | Responses |
|---|---|
| `client.Chat.Completions.New(ctx, params)` | `client.Responses.New(ctx, params)` |
| `params.Messages` (full history every call) | `params.Input` (string OR array) + `params.PreviousResponseID` (server-side history) |
| `system` role | `params.Instructions` |
| `params.Tools` (functions) | `params.Tools` (functions OR builtins like `web_search`) |
| `params.ToolChoice` | `params.ToolChoice` (same shape) |
| `resp.Choices[0].FinishReason` (`"stop"` / `"tool_calls"` / `"length"`) | `resp.Status` (`"completed"` / `"in_progress"` / `"incomplete"`) + `resp.IncompleteDetails.Reason` |
| `resp.Choices[0].Message.Content` | `resp.OutputText` (helper) OR walk `resp.Output[].Content[].Text` |
| `resp.Choices[0].Message.ToolCalls` | `resp.Output[]` items where `.Type == "function_call"` (top-level, not nested in messages) |
| Sending a tool result back | Append a `Type: "function_call_output"` item to next call's `Input`, with `CallID` + `Output` |
| `resp.Usage.{PromptTokens,CompletionTokens,TotalTokens}` | `resp.Usage.{InputTokens,OutputTokens,TotalTokens}` (note rename) |
| `resp.Usage.PromptTokensDetails.CachedTokens` | `resp.Usage.InputTokensDetails.CachedTokens` |

Key behavior diffs:
- The Responses API retains state. Pass `params.PreviousResponseID =
  resp.ID` on the next call and the model has the full prior context;
  you only attach the *new* `Input` items.
- Output is a list of items (`message`, `function_call`,
  `reasoning`, etc.), not a single message with embedded tool_calls.
- `Store: true` (default for stateful retention) keeps the response on
  OpenAI's side for 30 days; pass `Store: false` if you want one-shot
  semantics.

## Implementation plan

### Phase 1: Add `RunOpenAIResponses` alongside `RunOpenAI`

Don't replace; add a parallel path so we can A/B and fall back. Wire
selection via a config knob (`implementer.api: chat-completions |
responses`, default `responses` after rollout).

1. New file `internal/agent/openai_responses.go`:
   - `RunOpenAIResponses(ctx, client, model, opts) (*Result, error)`
     mirroring the existing signature.
   - First iteration: build `Input` from
     `[{Role: "user", Content: buildUserMessage(opts)}]`,
     `Instructions: implementerSystemPrompt`, `Tools: <converted MCP
     functions>`, no `PreviousResponseID`.
   - Subsequent iterations: build `Input` from only the new
     `function_call_output` items (one per resolved tool call), set
     `PreviousResponseID = lastResponseID`. Drop the running
     `messages` slice — Responses keeps history for us.
   - Walk `resp.Output` for items: text → `res.Summary`;
     `function_call` → run via MCP, accumulate outputs for next call.
   - Loop terminates when `resp.Status == "completed"` and the output
     contains no `function_call` items, OR when `MaxIterations` is hit.
2. Reuse `callOpenAIWithRetry` — the 429/Retry-After behavior is
   identical on both endpoints. Just point it at
   `client.Responses.New` via a generic helper, or duplicate it
   (10 lines, fine).
3. Reuse `buildOpenAIToolList` — function-tool defs are byte-identical
   between the two APIs.
4. Token usage maps `resp.Usage.InputTokens/OutputTokens` into our
   existing `TokenUsage{PromptTokens,CompletionTokens,...}` so the
   digest renderer doesn't change.

### Phase 2: Migrate triage

`internal/triage/openai.go` does ONE Chat Completions call per repo
with a forced single tool call to extract decisions. Drop in:

```go
resp, err := t.client.Responses.New(ctx, openai.ResponseNewParams{
    Model:        openai.ChatModel(t.model),
    Input:        buildTriageUserInput(tasks),
    Instructions: t.systemPrompt,
    Tools:        []openai.ResponseToolUnionParam{toolDef},
    ToolChoice:   forcedToolChoice("emit_decisions"),
    Store:        param.Bool(false), // triage is one-shot, don't retain
})
```

Then walk `resp.Output[]` for the single `function_call` item and
unmarshal its `Arguments` JSON exactly as today's
`extractOpenAIDecisions` does.

Triage is simpler than the implementer because there's no loop —
this is the lower-risk migration; do it first to shake out
SDK / model-compat issues.

### Phase 3: Plumbing + cutover

1. Add `Implementer.API` field to config (`chat-completions` |
   `responses`). Plumb through `cmdRun`'s `pickRunAgent` so
   `dispatch.RunAgentFunc` selects the right entry point.
2. Default to `responses` in `render-ci-config.py`.
3. Document a `--use-chat-completions` workflow_dispatch input on
   nightly.yml so we can fall back if Responses misbehaves on
   nightly without a code change.

### Phase 4: Tests

- `internal/agent/openai_responses_test.go` — table-driven test using
  a mocked `client.Responses` (intercept via the openai-go SDK's
  `option.WithBaseURL` pointing at httptest.NewServer). Cover:
  - Single-shot (no tool calls) — terminates after iter 1.
  - Multi-iter with tool calls — verifies `PreviousResponseID`
    threads correctly between iterations.
  - 429 retry — same scenarios as `openai_retry_test.go`.
  - `MaxIterations` cap — bails cleanly with the limit reached.
- Triage test similarly mocked — verify the single forced tool call
  + JSON decode round-trip.

### Phase 5: Cleanup (separate PR)

After two clean nightlies on Responses, delete `RunOpenAI` and the
`Implementer.API` switch. Don't do this in the same PR — leave a
revert path while the migration soaks.

## Definition of Done

- [ ] `RunOpenAIResponses` lives next to `RunOpenAI`, behind config
  flag, with green tests.
- [ ] Triage uses Responses API (no flag — single call site).
- [ ] `nightly.yml` defaults `implementer.api=responses` via
  `render-ci-config.py`.
- [ ] One green nightly run on the new path with the digest showing
  realistic token counts (smaller than the chat-completions baseline
  for the same task set, because of the no-resend-history win).
- [ ] CHANGELOG entry.
- [ ] Follow-up issue/spec to remove the old path after soak.

## Risk + rollback

- **SDK compat:** the openai-go SDK exposes `client.Responses` already
  (we saw it in dispatch); confirm the version we have on `go.mod`
  ships the type before starting.
- **Behavioral diffs:** Responses' `PreviousResponseID` requires
  `Store: true` (the default) on the *prior* call. If we ever
  set `Store: false` mid-loop, the next call breaks. Lock it in with a
  test that asserts every non-final response in the loop has
  `Store: true`.
- **Rollback:** flip `implementer.api` back to `chat-completions` in
  `render-ci-config.py`; takes one commit + one nightly.

## References

- OpenAI migration guide: https://developers.openai.com/api/docs/guides/migrate-to-responses
- openai-go SDK Responses package: `github.com/openai/openai-go/responses` (verify on go.mod first)
- This repo's current implementer:
  [`internal/agent/openai.go`](../../internal/agent/openai.go)
- This repo's current triage:
  [`internal/triage/openai.go`](../../internal/triage/openai.go)
