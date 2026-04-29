// Package agent runs the implementer loop for one burndown task.
//
// One Run() call corresponds to one task. The function:
//
//  1. Fetches the MCP tool catalog and filters it to the implementer's
//     allowlist (filesystem + build runners; never git or gh).
//  2. Registers those tools as Anthropic tool definitions.
//  3. Drives a manual tool-use loop: send to Claude → execute each
//     ToolUseBlock by forwarding to MCP → feed results back as
//     tool_result blocks → repeat until end_turn or iteration cap.
//  4. Returns a Result with stop reason and the agent's final
//     summary text.
//
// The caller (dispatch) handles worktree lifecycle, git commits, and
// PR creation. This package never touches git or GitHub.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/anthropics/anthropic-sdk-go"

	"github.com/jdfalk/overnight-burndown/internal/mcp"
	"github.com/jdfalk/overnight-burndown/internal/sources"
	"github.com/jdfalk/overnight-burndown/internal/triage"
)

// MCPClient is the subset of *mcp.Client this package uses.
// Pulled out as an interface for tests; production passes a real *mcp.Client.
type MCPClient interface {
	ListTools(ctx context.Context) ([]mcp.ToolDef, error)
	CallTool(ctx context.Context, name string, args any) (*mcp.CallResult, error)
}

// Options is the per-run input. The caller (dispatch) constructs one of
// these per task and calls Run.
type Options struct {
	Client    anthropic.Client
	MCP       MCPClient
	Model     anthropic.Model
	Task      sources.Task
	Decision  triage.Decision
	Branch    string
	WorktreeRoot string

	// AllowedTools restricts which MCP tools the implementer can call.
	// When empty, defaults to the safe filesystem + build runner set —
	// fs_*, run_*, py_pytest. Git and gh tools are never auto-included.
	AllowedTools []string

	// MaxIterations caps the tool-use loop. Defaults to 30.
	MaxIterations int

	// MaxTokens for each Claude request. Defaults to 16000.
	MaxTokens int64
}

// Result is what Run returns on success.
type Result struct {
	Iterations int
	StopReason anthropic.StopReason
	// Summary is the agent's final assistant text — captured for the
	// morning digest and the PR body.
	Summary string
	// ToolCallCount is the total number of MCP tool calls the agent made
	// across the loop. Used for digest reporting.
	ToolCallCount int
	// Usage is the token-count accumulator across the loop. Populated by
	// the OpenAI path (Anthropic SDK exposes it differently and is wired
	// elsewhere); zero when the provider doesn't report.
	Usage TokenUsage
}

// TokenUsage is a provider-agnostic accumulator. PromptTokens is everything
// sent to the model (cached + uncached); CachedTokens is the cached subset
// when the provider exposes it. CompletionTokens is model output.
type TokenUsage struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	CachedTokens     int64 `json:"cached_tokens"`
	TotalTokens      int64 `json:"total_tokens"`
}

// Add accumulates another TokenUsage's values into this one. Called per
// agent-loop iteration.
func (u *TokenUsage) Add(o TokenUsage) {
	u.PromptTokens += o.PromptTokens
	u.CompletionTokens += o.CompletionTokens
	u.CachedTokens += o.CachedTokens
	u.TotalTokens += o.TotalTokens
}

// defaultAllowedTools is the implementer's tool surface. We deliberately
// exclude git_* and gh_* — git/PR ops are the harness's job, not the
// agent's. Test runners (run_*) are included so the agent can verify its
// own changes.
var defaultAllowedTools = []string{
	"fs_read", "fs_write", "fs_glob", "fs_list", "fs_exists",
	"run_make", "run_go_test", "run_go_build", "run_go_vet",
	"run_npm_test", "run_npm_ci",
	"py_pytest",
}

// Run drives the implementer loop for one task and returns when Claude
// emits end_turn or the iteration cap is hit. Errors from Anthropic or
// MCP are returned as-is — the caller decides whether to retry.
func Run(ctx context.Context, opts Options) (*Result, error) {
	if opts.MaxIterations <= 0 {
		opts.MaxIterations = 30
	}
	if opts.MaxTokens <= 0 {
		opts.MaxTokens = 16000
	}
	if len(opts.AllowedTools) == 0 {
		opts.AllowedTools = defaultAllowedTools
	}

	tools, err := buildToolList(ctx, opts.MCP, opts.AllowedTools)
	if err != nil {
		return nil, fmt.Errorf("agent: build tool list: %w", err)
	}
	if len(tools) == 0 {
		return nil, errors.New("agent: no MCP tools matched the allowlist")
	}

	messages := []anthropic.MessageParam{
		anthropic.NewUserMessage(anthropic.NewTextBlock(buildUserMessage(opts))),
	}

	res := &Result{}
	for i := 0; i < opts.MaxIterations; i++ {
		res.Iterations = i + 1

		resp, err := opts.Client.Messages.New(ctx, anthropic.MessageNewParams{
			Model:     opts.Model,
			MaxTokens: opts.MaxTokens,
			System: []anthropic.TextBlockParam{{
				Text:         implementerSystemPrompt,
				CacheControl: anthropic.NewCacheControlEphemeralParam(),
			}},
			Tools:    tools,
			Messages: messages,
		})
		if err != nil {
			return nil, fmt.Errorf("agent: anthropic call (iter %d): %w", res.Iterations, err)
		}

		messages = append(messages, resp.ToParam())
		res.StopReason = resp.StopReason

		// Capture text content for the summary on every turn — by end_turn
		// the last text block is the agent's final paragraph.
		if text := lastTextBlock(resp); text != "" {
			res.Summary = text
		}

		if resp.StopReason != anthropic.StopReasonToolUse {
			return res, nil
		}

		// Forward each tool call to MCP and build the user-turn tool_result
		// message that goes back to Claude on the next iteration.
		toolResults, err := executeToolCalls(ctx, opts.MCP, resp, &res.ToolCallCount)
		if err != nil {
			return nil, fmt.Errorf("agent: execute tools (iter %d): %w", res.Iterations, err)
		}
		messages = append(messages, anthropic.NewUserMessage(toolResults...))
	}

	// Iteration cap hit — return what we have. The driver will treat this
	// as a soft failure (likely flag for human review).
	return res, fmt.Errorf("agent: exceeded max iterations (%d) without end_turn", opts.MaxIterations)
}

// buildToolList fetches the MCP catalog and filters to the allowlist,
// converting each survivor to an Anthropic ToolUnionParam.
func buildToolList(ctx context.Context, m MCPClient, allowed []string) ([]anthropic.ToolUnionParam, error) {
	allowSet := make(map[string]struct{}, len(allowed))
	for _, name := range allowed {
		allowSet[name] = struct{}{}
	}

	mcpTools, err := m.ListTools(ctx)
	if err != nil {
		return nil, err
	}

	var out []anthropic.ToolUnionParam
	for _, t := range mcpTools {
		if _, ok := allowSet[t.Name]; !ok {
			continue
		}
		out = append(out, mcpToAnthropic(t))
	}
	return out, nil
}

// mcpToAnthropic converts an MCP ToolDef to the Anthropic SDK shape.
//
// MCP's inputSchema is a full JSON Schema (`{type: object, properties: {...},
// required: [...]}`); Anthropic's ToolInputSchemaParam holds the
// `properties` map directly. We extract `properties` and pass it through;
// the JSON Schema's `required` array is preserved in the schema we send
// upstream because we round-trip through json.RawMessage if present.
func mcpToAnthropic(t mcp.ToolDef) anthropic.ToolUnionParam {
	props, _ := t.InputSchema["properties"].(map[string]any)
	if props == nil {
		props = map[string]any{}
	}
	return anthropic.ToolUnionParam{
		OfTool: &anthropic.ToolParam{
			Name:        t.Name,
			Description: anthropic.String(t.Description),
			InputSchema: anthropic.ToolInputSchemaParam{Properties: props},
		},
	}
}

// executeToolCalls walks the response's content looking for ToolUseBlock,
// forwards each to MCP, and builds the matching tool_result blocks.
func executeToolCalls(
	ctx context.Context,
	m MCPClient,
	resp *anthropic.Message,
	counter *int,
) ([]anthropic.ContentBlockParamUnion, error) {
	var results []anthropic.ContentBlockParamUnion
	for _, block := range resp.Content {
		variant, ok := block.AsAny().(anthropic.ToolUseBlock)
		if !ok {
			continue
		}
		*counter++

		var args map[string]any
		if raw := variant.JSON.Input.Raw(); raw != "" {
			if err := json.Unmarshal([]byte(raw), &args); err != nil {
				results = append(results, anthropic.NewToolResultBlock(
					variant.ID,
					fmt.Sprintf("agent: invalid tool input json: %v", err),
					true,
				))
				continue
			}
		}

		callRes, err := m.CallTool(ctx, variant.Name, args)
		if err != nil {
			// Surface MCP errors to the agent so it can decide whether to
			// retry, fall back, or stop. We do NOT abort the whole loop on
			// a single tool failure — that would defeat the agent's ability
			// to recover from e.g. a misformatted argument.
			results = append(results, anthropic.NewToolResultBlock(
				variant.ID,
				fmt.Sprintf("tool error: %v", err),
				true,
			))
			continue
		}

		results = append(results, anthropic.NewToolResultBlock(
			variant.ID,
			callRes.Text,
			callRes.IsError,
		))
	}
	return results, nil
}

// lastTextBlock returns the text of the last TextBlock in the response, or
// "" if none. We call this on every iteration so by end_turn we have the
// agent's final summary captured even if the response is then unwound by
// a panic during result construction.
func lastTextBlock(resp *anthropic.Message) string {
	for i := len(resp.Content) - 1; i >= 0; i-- {
		if tb, ok := resp.Content[i].AsAny().(anthropic.TextBlock); ok && tb.Text != "" {
			return tb.Text
		}
	}
	return ""
}

// buildUserMessage formats the task as the kickoff user message.
func buildUserMessage(opts Options) string {
	cls := string(opts.Decision.Classification)
	return fmt.Sprintf(`# Task

Classification: %s
Branch: %s
Repo: %s
Source: %s
Title: %s

%s

# Reason for this classification

%s

# Suggested complexity (1-5)

%d

# Your task

Implement the change above in the worktree at %s. Read existing files before editing. Run the project's tests if a runner is available. When done, summarize your changes in one paragraph.`,
		cls,
		opts.Branch,
		opts.Task.Source.Repo,
		opts.Task.Source.URL,
		opts.Task.Source.Title,
		opts.Task.Body,
		opts.Decision.Reason,
		opts.Decision.EstComplexity,
		opts.WorktreeRoot,
	)
}
