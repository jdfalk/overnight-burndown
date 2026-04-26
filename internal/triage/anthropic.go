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
	client anthropic.Client
	model  anthropic.Model
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

// NewTriager is a backward-compat alias for NewAnthropic. Existing call
// sites and tests continue to work unchanged.
func NewTriager(model string, opts ...option.RequestOption) *AnthropicTriager {
	return NewAnthropic(model, opts...)
}

// Triager is a backward-compat alias for AnthropicTriager.
type Triager = AnthropicTriager

// Triage implements Provider via Anthropic's tool-forced output.
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

	params := anthropic.MessageNewParams{
		Model:     t.model,
		MaxTokens: 16000,
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
