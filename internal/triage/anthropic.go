package triage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/jdfalk/overnight-burndown/internal/sources"
)

// AnthropicTriager classifies tasks via Anthropic Messages API + tool-forced output.
type AnthropicTriager struct {
	client        anthropic.Client
	model         anthropic.Model
	thinkingBudget int64 // 0 = disabled; ≥1024 = enabled (tokens Opus may use to think)
}

// NewAnthropic constructs an Anthropic-backed triage Provider. The api key
// flows in via option.WithAPIKey or the ANTHROPIC_API_KEY env var; tests
// inject option.WithBaseURL to point at httptest servers.
func NewAnthropic(model string, opts ...option.RequestOption) *AnthropicTriager {
	return &AnthropicTriager{
		client: anthropic.NewClient(opts...),
		model:  anthropic.Model(model),
	}
}

// NewAnthropicWithThinking is like NewAnthropic but enables extended thinking
// with the given budget. budgetTokens must be ≥ 1024; the model thinks for up
// to that many tokens before producing its tool-forced output. Recommended
// range for triage: 4096 (medium) to 10000 (high) — see docs/burndown-thinking.md.
func NewAnthropicWithThinking(model string, budgetTokens int64, opts ...option.RequestOption) *AnthropicTriager {
	t := NewAnthropic(model, opts...)
	t.thinkingBudget = budgetTokens
	return t
}

// NewTriager is a backward-compat alias for NewAnthropic. Existing call
// sites and tests continue to work unchanged.
func NewTriager(model string, opts ...option.RequestOption) *AnthropicTriager {
	return NewAnthropic(model, opts...)
}

// Triager is a backward-compat alias for AnthropicTriager.
type Triager = AnthropicTriager

// Triage implements Provider via Anthropic's tool-forced output.
//
// When thinkingBudget > 0, extended thinking is enabled: Opus reasons through
// cross-cutting constraints and risk before writing its classification JSON.
// Thinking blocks in the response are skipped transparently by
// extractAnthropicDecisions — only the ToolUseBlock matters.
func (t *AnthropicTriager) Triage(ctx context.Context, tasks []sources.Task) ([]Decision, error) {
	if len(tasks) == 0 {
		return nil, nil
	}

	userPayload, err := buildUserPayload(tasks)
	if err != nil {
		return nil, fmt.Errorf("triage: build payload: %w", err)
	}

	tools := []anthropic.ToolUnionParam{
		{OfTool: &anthropic.ToolParam{
			Name:        "record_classifications",
			Description: anthropic.String("Record one classification per input task. Must be called exactly once with one entry per task in the input."),
			InputSchema: anthropic.ToolInputSchemaParam{
				Properties: jsonSchemaForDecisions()["properties"].(map[string]any),
				Required:   []string{"decisions"},
			},
		}},
	}

	// MaxTokens must cover both thinking tokens (if enabled) and output tokens.
	// Output for N tasks is small (< 2K), so thinking budget + 4096 is always safe.
	maxTokens := int64(16000)
	if t.thinkingBudget > 0 && t.thinkingBudget+4096 > maxTokens {
		maxTokens = t.thinkingBudget + 4096
	}

	params := anthropic.MessageNewParams{
		Model:     t.model,
		MaxTokens: maxTokens,
		System: []anthropic.TextBlockParam{{
			Text:         classificationSystemPrompt,
			CacheControl: anthropic.NewCacheControlEphemeralParam(),
		}},
		Tools: tools,
		ToolChoice: anthropic.ToolChoiceUnionParam{
			OfTool: &anthropic.ToolChoiceToolParam{Name: "record_classifications"},
		},
		Messages: []anthropic.MessageParam{
			anthropic.NewUserMessage(anthropic.NewTextBlock(userPayload)),
		},
	}

	if t.thinkingBudget >= 1024 {
		params.Thinking = anthropic.ThinkingConfigParamOfEnabled(t.thinkingBudget)
		// Anthropic rejects tool_choice=forced when thinking is enabled.
		// Switch to auto — the system prompt instructs the model to call
		// record_classifications; extractAnthropicDecisions validates it did.
		params.ToolChoice = anthropic.ToolChoiceUnionParam{
			OfAuto: &anthropic.ToolChoiceAutoParam{},
		}
	}

	resp, err := t.client.Messages.New(ctx, params)
	if err != nil {
		return nil, fmt.Errorf("triage: anthropic call: %w", err)
	}

	decisions, err := extractAnthropicDecisions(resp)
	if err != nil {
		return nil, fmt.Errorf("triage: extract decisions: %w", err)
	}
	if err := validateAgainstInput(tasks, decisions); err != nil {
		return nil, fmt.Errorf("triage: validate: %w", err)
	}
	return reorderToInput(tasks, decisions), nil
}

func extractAnthropicDecisions(resp *anthropic.Message) ([]Decision, error) {
	for _, block := range resp.Content {
		if v, ok := block.AsAny().(anthropic.ToolUseBlock); ok && v.Name == "record_classifications" {
			var payload struct {
				Decisions []Decision `json:"decisions"`
			}
			if err := json.Unmarshal([]byte(v.JSON.Input.Raw()), &payload); err != nil {
				return nil, fmt.Errorf("decode tool input: %w", err)
			}
			return payload.Decisions, nil
		}
	}
	return nil, errors.New("no record_classifications tool call in response")
}
