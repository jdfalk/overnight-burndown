package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/openai/openai-go"
	"github.com/openai/openai-go/shared"

	"github.com/jdfalk/overnight-burndown/internal/mcp"
)

// RunOpenAI is the OpenAI-backed sibling of Run. Same Options shape as the
// Anthropic path; the Client/Model fields on Options are ignored — the
// caller passes the OpenAI client + model in directly.
//
// Loop semantics match Run: exit on a non-tool-call finish reason or when
// MaxIterations is hit. Per-tool errors become is_error tool messages, not
// loop terminations.
func RunOpenAI(ctx context.Context, client openai.Client, model string, opts Options) (*Result, error) {
	if opts.MaxIterations <= 0 {
		opts.MaxIterations = 30
	}
	if len(opts.AllowedTools) == 0 {
		opts.AllowedTools = defaultAllowedTools
	}

	tools, err := buildOpenAIToolList(ctx, opts.MCP, opts.AllowedTools)
	if err != nil {
		return nil, fmt.Errorf("agent: build tool list: %w", err)
	}
	if len(tools) == 0 {
		return nil, errors.New("agent: no MCP tools matched the allowlist")
	}

	messages := []openai.ChatCompletionMessageParamUnion{
		openai.SystemMessage(implementerSystemPrompt),
		openai.UserMessage(buildUserMessage(opts)),
	}

	res := &Result{}
	for i := 0; i < opts.MaxIterations; i++ {
		res.Iterations = i + 1

		resp, err := client.Chat.Completions.New(ctx, openai.ChatCompletionNewParams{
			Model:    openai.ChatModel(model),
			Messages: messages,
			Tools:    tools,
		})
		if err != nil {
			return nil, fmt.Errorf("agent: openai call (iter %d): %w", res.Iterations, err)
		}
		if len(resp.Choices) == 0 {
			return nil, fmt.Errorf("agent: openai response has no choices (iter %d)", res.Iterations)
		}

		choice := resp.Choices[0]
		msg := choice.Message
		// StopReason is OpenAI's finish_reason ("stop", "tool_calls", "length", ...).
		// We coerce to the Anthropic-named subset for digest purposes; the
		// abstraction lives in Result, not here.
		res.StopReason = anthropicStopFor(string(choice.FinishReason))

		if msg.Content != "" {
			res.Summary = msg.Content
		}

		// Append the assistant message to history before processing tool calls.
		messages = append(messages, msg.ToParam())

		if string(choice.FinishReason) != "tool_calls" || len(msg.ToolCalls) == 0 {
			return res, nil
		}

		// Execute each tool call and append a tool message per call.
		for _, tc := range msg.ToolCalls {
			res.ToolCallCount++
			content := executeOpenAIToolCall(ctx, opts.MCP, tc)
			messages = append(messages, openai.ToolMessage(content, tc.ID))
		}
	}

	return res, fmt.Errorf("agent: exceeded max iterations (%d) without finish", opts.MaxIterations)
}

// buildOpenAIToolList fetches the MCP catalog, filters to the allowlist,
// converts each survivor to an OpenAI ChatCompletionToolParam.
func buildOpenAIToolList(ctx context.Context, m MCPClient, allowed []string) ([]openai.ChatCompletionToolParam, error) {
	allowSet := make(map[string]struct{}, len(allowed))
	for _, name := range allowed {
		allowSet[name] = struct{}{}
	}

	mcpTools, err := m.ListTools(ctx)
	if err != nil {
		return nil, err
	}

	var out []openai.ChatCompletionToolParam
	for _, t := range mcpTools {
		if _, ok := allowSet[t.Name]; !ok {
			continue
		}
		out = append(out, mcpToOpenAI(t))
	}
	return out, nil
}

// mcpToOpenAI converts an MCP ToolDef to an OpenAI tool param. MCP's
// inputSchema is a full JSON Schema; OpenAI's Parameters field accepts
// the same shape so we pass it through unchanged.
func mcpToOpenAI(t mcp.ToolDef) openai.ChatCompletionToolParam {
	schema := t.InputSchema
	if schema == nil {
		schema = map[string]any{"type": "object", "properties": map[string]any{}}
	}
	return openai.ChatCompletionToolParam{
		Function: shared.FunctionDefinitionParam{
			Name:        t.Name,
			Description: openai.String(t.Description),
			Parameters:  schema,
		},
	}
}

// executeOpenAIToolCall forwards one tool call to MCP and returns the
// content string for the tool message. Tool errors / bad JSON inputs are
// surfaced as text content (not loop-aborting errors) so the agent can
// recover.
func executeOpenAIToolCall(ctx context.Context, m MCPClient, tc openai.ChatCompletionMessageToolCall) string {
	var args map[string]any
	if tc.Function.Arguments != "" {
		if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
			return fmt.Sprintf("agent: invalid tool input json: %v", err)
		}
	}
	callRes, err := m.CallTool(ctx, tc.Function.Name, args)
	if err != nil {
		return fmt.Sprintf("tool error: %v", err)
	}
	if callRes.IsError {
		return "tool error: " + callRes.Text
	}
	return callRes.Text
}

// anthropicStopFor maps OpenAI finish_reason strings to the Anthropic
// stop-reason vocabulary so Result.StopReason is uniform across backends.
// `Result.StopReason` is `anthropic.StopReason` which is a string type, so
// we coerce — this keeps the runner / digest unaware of which backend ran.
func anthropicStopFor(openaiFinish string) anthropic.StopReason {
	switch openaiFinish {
	case "stop":
		return anthropic.StopReasonEndTurn
	case "tool_calls":
		return anthropic.StopReasonToolUse
	case "length":
		return anthropic.StopReasonMaxTokens
	default:
		return anthropic.StopReason(openaiFinish)
	}
}
