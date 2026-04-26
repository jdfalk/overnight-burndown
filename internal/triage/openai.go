package triage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/openai/openai-go"
	"github.com/openai/openai-go/option"
	"github.com/openai/openai-go/shared"

	"github.com/jdfalk/overnight-burndown/internal/sources"
)

// OpenAITriager classifies tasks via OpenAI's chat-completion + function calling.
//
// The contract matches the Anthropic backend exactly: a single forced tool
// call (`record_classifications`) with a strict JSON Schema. The agent
// returns one Decision per input task, in any order; we reorder to match
// input.
type OpenAITriager struct {
	client openai.Client
	model  string
}

// NewOpenAI constructs an OpenAI-backed triage Provider. opts forwards to
// openai.NewClient (api key, base URL, etc.).
func NewOpenAI(model string, opts ...option.RequestOption) *OpenAITriager {
	return &OpenAITriager{
		client: openai.NewClient(opts...),
		model:  model,
	}
}

// Triage implements Provider via OpenAI function calling.
func (t *OpenAITriager) Triage(ctx context.Context, tasks []sources.Task) ([]Decision, error) {
	if len(tasks) == 0 {
		return nil, nil
	}

	userPayload, err := buildUserPayload(tasks)
	if err != nil {
		return nil, fmt.Errorf("triage: build payload: %w", err)
	}

	tool := openai.ChatCompletionToolParam{
		Function: shared.FunctionDefinitionParam{
			Name:        "record_classifications",
			Description: openai.String("Record one classification per input task. Must be called exactly once with one entry per task in the input."),
			Parameters:  jsonSchemaForDecisions(),
			Strict:      openai.Bool(true),
		},
	}

	params := openai.ChatCompletionNewParams{
		Model: openai.ChatModel(t.model),
		Messages: []openai.ChatCompletionMessageParamUnion{
			openai.SystemMessage(classificationSystemPrompt),
			openai.UserMessage(userPayload),
		},
		Tools: []openai.ChatCompletionToolParam{tool},
		ToolChoice: openai.ChatCompletionToolChoiceOptionUnionParam{
			OfChatCompletionNamedToolChoice: &openai.ChatCompletionNamedToolChoiceParam{
				Function: openai.ChatCompletionNamedToolChoiceFunctionParam{
					Name: "record_classifications",
				},
			},
		},
	}

	resp, err := t.client.Chat.Completions.New(ctx, params)
	if err != nil {
		return nil, fmt.Errorf("triage: openai call: %w", err)
	}

	decisions, err := extractOpenAIDecisions(resp)
	if err != nil {
		return nil, fmt.Errorf("triage: extract decisions: %w", err)
	}
	if err := validateAgainstInput(tasks, decisions); err != nil {
		return nil, fmt.Errorf("triage: validate: %w", err)
	}
	return reorderToInput(tasks, decisions), nil
}

func extractOpenAIDecisions(resp *openai.ChatCompletion) ([]Decision, error) {
	if len(resp.Choices) == 0 {
		return nil, errors.New("openai response has no choices")
	}
	for _, tc := range resp.Choices[0].Message.ToolCalls {
		if tc.Function.Name != "record_classifications" {
			continue
		}
		var payload struct {
			Decisions []Decision `json:"decisions"`
		}
		if err := json.Unmarshal([]byte(tc.Function.Arguments), &payload); err != nil {
			return nil, fmt.Errorf("decode tool args: %w", err)
		}
		return payload.Decisions, nil
	}
	return nil, errors.New("no record_classifications tool call in response")
}
