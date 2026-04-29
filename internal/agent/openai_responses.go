// file: internal/agent/openai_responses.go
// version: 1.0.0
// guid: a1b2c3d4-e5f6-7890-abcd-ef0123456789
//
// OpenAI Responses API implementer agent. Counterpart to RunOpenAI in
// openai.go (Chat Completions). Migration spec:
// docs/specs/2026-04-29-responses-api-migration.md.
//
// Why two implementations:
//   * Chat Completions resends the full message history every iteration.
//     By iter 6 of a typical implementer loop we're shipping ~30K prompt
//     tokens per call, which exhausts TPM at modest concurrency.
//   * Responses keeps the conversation server-side via PreviousResponseID;
//     each follow-up call only sends the new function_call_output items.
//     Same model output, far fewer tokens billed/limited per request.
//   * Several recent models (gpt-5.1-codex-mini, gpt-5.4 reasoning) ship
//     on /v1/responses only.
//
// The two paths are kept side-by-side until we soak Responses on
// production for ~2 weeks; flip the default in render-ci-config.py once
// confident, then delete openai.go's RunOpenAI in a follow-up PR.

package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/openai/openai-go"
	"github.com/openai/openai-go/packages/param"
	"github.com/openai/openai-go/responses"
	"github.com/openai/openai-go/shared"

	"github.com/jdfalk/overnight-burndown/internal/mcp"
)

// stderr is overridable in tests; production points at os.Stderr.
var stderr = os.Stderr

// RunOpenAIResponses is the Responses-API sibling of RunOpenAI. Same
// signature, same semantics: loop until the model returns no more
// function_call output items or MaxIterations is hit.
//
// `models` is an ordered fallback chain: index 0 is the primary; subsequent
// entries are tried only when the previous one's retry budget is exhausted
// (typically: persistent 429s or model unavailability). The conversation
// thread carries across the model swap via PreviousResponseID — OpenAI
// allows mid-thread model changes. We "stick" once we fall back; we don't
// flip back to the primary later in the same task because the conditions
// that exhausted retries on the primary are likely still in effect.
//
// Each iteration after the first uses PreviousResponseID to thread the
// conversation server-side; we never re-upload prior input + tool outputs.
// Token usage is accumulated per call into res.Usage so the digest can show
// the savings vs Chat Completions.
func RunOpenAIResponses(ctx context.Context, client openai.Client, models []string, opts Options) (*Result, error) {
	if len(models) == 0 {
		return nil, errors.New("agent: RunOpenAIResponses requires at least one model")
	}
	if opts.MaxIterations <= 0 {
		opts.MaxIterations = 30
	}
	if len(opts.AllowedTools) == 0 {
		opts.AllowedTools = defaultAllowedTools
	}

	tools, err := buildResponsesToolList(ctx, opts.MCP, opts.AllowedTools)
	if err != nil {
		return nil, fmt.Errorf("agent: build tool list: %w", err)
	}
	if len(tools) == 0 {
		return nil, errors.New("agent: no MCP tools matched the allowlist")
	}

	res := &Result{}
	// First iteration: fresh conversation seeded with the user task; later
	// iterations send only the resolved tool outputs and PreviousResponseID.
	input := responses.ResponseNewParamsInputUnion{
		OfString: openai.String(buildUserMessage(opts)),
	}
	var prevID string
	modelIdx := 0 // points into `models`; advances on retry-budget exhaustion

	for i := 0; i < opts.MaxIterations; i++ {
		res.Iterations = i + 1

		params := responses.ResponseNewParams{
			Model:        shared.ResponsesModel(models[modelIdx]),
			Input:        input,
			Instructions: openai.String(implementerSystemPrompt),
			Tools:        tools,
			// Default Store=true keeps the response retrievable on OpenAI's
			// side for 30 days. We rely on that default to make
			// PreviousResponseID work — explicit Store: true so a future
			// reader doesn't wonder.
			Store: param.NewOpt(true),
		}
		if prevID != "" {
			params.PreviousResponseID = openai.String(prevID)
		}

		resp, err := callResponsesWithModelFallback(ctx, client, params, models, &modelIdx)
		if err != nil {
			return nil, fmt.Errorf("agent: openai responses call (iter %d, models tried=%d): %w",
				res.Iterations, modelIdx+1, err)
		}
		prevID = resp.ID

		// Token-usage accounting per call. Responses uses InputTokens /
		// OutputTokens (vs PromptTokens / CompletionTokens on Chat
		// Completions). The cached subset is under InputTokensDetails.
		res.Usage.Add(TokenUsage{
			PromptTokens:     resp.Usage.InputTokens,
			CompletionTokens: resp.Usage.OutputTokens,
			CachedTokens:     resp.Usage.InputTokensDetails.CachedTokens,
			TotalTokens:      resp.Usage.TotalTokens,
		})

		// Walk the output items: capture text into res.Summary, collect
		// any function_call items for execution, surface stop reason.
		var toolCalls []responsesToolCall
		var hasOutputText bool
		for _, item := range resp.Output {
			switch item.Type {
			case "message":
				for _, c := range item.Content {
					if c.Text != "" {
						res.Summary = c.Text
						hasOutputText = true
					}
				}
			case "function_call":
				toolCalls = append(toolCalls, responsesToolCall{
					CallID:    item.CallID,
					Name:      item.Name,
					Arguments: item.Arguments,
				})
			}
		}

		// StopReason synthesized for digest purposes — Responses doesn't
		// have finish_reason; we map status + presence-of-tool-calls.
		res.StopReason = stopReasonForResponse(string(resp.Status), len(toolCalls), hasOutputText)

		// No tool calls → loop terminates. The model emitted its final
		// answer (or status==incomplete; either way no further input).
		if len(toolCalls) == 0 {
			return res, nil
		}

		// Execute every tool call, build the next iteration's Input as a
		// list of function_call_output items keyed by CallID. The model
		// will see these (via PreviousResponseID + the new Input) on the
		// next loop pass.
		nextItems := make(responses.ResponseInputParam, 0, len(toolCalls))
		for _, tc := range toolCalls {
			res.ToolCallCount++
			content := executeResponsesToolCall(ctx, opts.MCP, tc)
			nextItems = append(nextItems, responses.ResponseInputItemParamOfFunctionCallOutput(tc.CallID, content))
		}
		input = responses.ResponseNewParamsInputUnion{OfInputItemList: nextItems}
	}

	return res, fmt.Errorf("agent: exceeded max iterations (%d) without finish", opts.MaxIterations)
}

// responsesToolCall flattens the fields we care about from a function_call
// output item. The SDK union type makes direct access verbose; this is just
// scaffolding.
type responsesToolCall struct {
	CallID    string
	Name      string
	Arguments string
}

// callResponsesWithModelFallback wraps callResponsesWithRetry with a
// model fallback chain. On retry-budget exhaustion (i.e. the inner
// helper returns its "rate-limit retries exhausted" error) we advance
// `*modelIdx` and retry against the next model in the chain. Once we
// fall back, we stay on the new model — flapping back to the primary
// would just re-trigger the original 429s.
//
// Non-retry-related errors (e.g. invalid request, auth) propagate
// immediately without falling back; switching models won't help those.
func callResponsesWithModelFallback(ctx context.Context, client openai.Client, params responses.ResponseNewParams, models []string, modelIdx *int) (*responses.Response, error) {
	for {
		resp, err := callResponsesWithRetry(ctx, client, params)
		if err == nil {
			return resp, nil
		}
		// Only fall back on retry-budget exhaustion. Other errors mean
		// switching models won't help (auth, schema, 4xx body, etc.).
		if !errors.Is(err, errRetriesExhausted) {
			return nil, err
		}
		// Try the next model in the chain.
		if *modelIdx+1 >= len(models) {
			return nil, fmt.Errorf("all %d models exhausted retries: %w", len(models), err)
		}
		old := models[*modelIdx]
		*modelIdx++
		next := models[*modelIdx]
		fmt.Fprintf(stderr, "::warning::agent: model %s exhausted retries, falling back to %s\n", old, next)
		params.Model = shared.ResponsesModel(next)
	}
}

// errRetriesExhausted is returned by callResponsesWithRetry when its retry
// budget is consumed. The model-fallback wrapper looks for it.
var errRetriesExhausted = errors.New("rate-limit retries exhausted")

// callResponsesWithRetry mirrors callOpenAIWithRetry from openai.go, but
// against the Responses endpoint. Same retry budget and backoff curve;
// duplicated rather than abstracted because the SDK param + return types
// don't share a common interface.
func callResponsesWithRetry(ctx context.Context, client openai.Client, params responses.ResponseNewParams) (*responses.Response, error) {
	deadline := timeNow().Add(retryBudget)
	attempt := 0
	for {
		attempt++
		resp, err := client.Responses.New(ctx, params)
		if err == nil {
			return resp, nil
		}
		msg := err.Error()
		if !is429(msg) {
			return nil, err
		}
		if timeNow().After(deadline) {
			return nil, fmt.Errorf("%w (last err: %v)", errRetriesExhausted, err)
		}
		wait := backoffFor(attempt, baseBackoff, maxBackoff)
		if hinted := parseRetryAfter(msg); hinted > 0 && hinted <= maxBackoff {
			wait = hinted + jitterMs
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-timeAfter(wait):
		}
	}
}

// buildResponsesToolList wraps each MCP tool in a responses.ToolUnionParam
// (function variant). The schema layer is identical to the Chat Completions
// equivalent — OpenAI accepts the same JSON Schema in both APIs.
func buildResponsesToolList(ctx context.Context, m MCPClient, allowed []string) ([]responses.ToolUnionParam, error) {
	allowSet := make(map[string]struct{}, len(allowed))
	for _, name := range allowed {
		allowSet[name] = struct{}{}
	}
	mcpTools, err := m.ListTools(ctx)
	if err != nil {
		return nil, err
	}
	var out []responses.ToolUnionParam
	for _, t := range mcpTools {
		if _, ok := allowSet[t.Name]; !ok {
			continue
		}
		out = append(out, mcpToResponses(t))
	}
	return out, nil
}

func mcpToResponses(t mcp.ToolDef) responses.ToolUnionParam {
	schema := t.InputSchema
	if schema == nil {
		schema = map[string]any{"type": "object", "properties": map[string]any{}}
	}
	tool := responses.ToolParamOfFunction(t.Name, schema, false)
	if tool.OfFunction != nil {
		tool.OfFunction.Description = openai.String(t.Description)
	}
	return tool
}

// executeResponsesToolCall is the Responses-shaped sibling of
// executeOpenAIToolCall. Same MCP forwarding logic; tool errors come back
// as text in the function_call_output item rather than aborting the loop.
func executeResponsesToolCall(ctx context.Context, m MCPClient, tc responsesToolCall) string {
	var args map[string]any
	if tc.Arguments != "" {
		if err := json.Unmarshal([]byte(tc.Arguments), &args); err != nil {
			return fmt.Sprintf("agent: invalid tool input json: %v", err)
		}
	}
	callRes, err := m.CallTool(ctx, tc.Name, args)
	if err != nil {
		return fmt.Sprintf("tool error: %v", err)
	}
	if callRes.IsError {
		return "tool error: " + callRes.Text
	}
	return callRes.Text
}

// stopReasonForResponse maps the Responses API's status + output-shape into
// the Anthropic-style StopReason vocabulary the rest of the runner expects.
// Lossy by design; the digest only needs the broad class.
func stopReasonForResponse(status string, toolCount int, hasText bool) anthropic.StopReason {
	switch status {
	case "incomplete":
		return anthropic.StopReasonMaxTokens
	case "in_progress":
		// Background mode would land here; we don't use it. Treat as tool_use
		// so the loop continues if there happen to be tool calls.
		return anthropic.StopReasonToolUse
	}
	// status == "completed"
	if toolCount > 0 {
		return anthropic.StopReasonToolUse
	}
	return anthropic.StopReasonEndTurn
}
