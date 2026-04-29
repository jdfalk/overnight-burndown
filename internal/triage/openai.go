package triage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/openai/openai-go"
	"github.com/openai/openai-go/option"
	"github.com/openai/openai-go/packages/param"
	"github.com/openai/openai-go/responses"
	"github.com/openai/openai-go/shared"

	"github.com/jdfalk/overnight-burndown/internal/sources"
)

// OpenAITriager classifies tasks via OpenAI's Responses API + function calling.
//
// The contract matches the Anthropic backend exactly: a single forced tool
// call (`record_classifications`) with a strict JSON Schema. The agent
// returns one Decision per input task, in any order; we reorder to match
// input.
//
// Migrated from /v1/chat/completions to /v1/responses on 2026-04-29 — the
// Responses API is OpenAI's go-forward surface (codex-mini and several gpt-5.x
// variants ship there only). Spec:
// docs/specs/2026-04-29-responses-api-migration.md.
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

// Triage implements Provider via OpenAI Responses + function calling.
func (t *OpenAITriager) Triage(ctx context.Context, tasks []sources.Task) ([]Decision, error) {
	if len(tasks) == 0 {
		return nil, nil
	}

	userPayload, err := buildUserPayload(tasks)
	if err != nil {
		return nil, fmt.Errorf("triage: build payload: %w", err)
	}

	tool := responses.ToolParamOfFunction(
		"record_classifications",
		jsonSchemaForDecisions(),
		true, // strict
	)
	if tool.OfFunction != nil {
		tool.OfFunction.Description = openai.String("Record one classification per input task. Must be called exactly once with one entry per task in the input.")
	}

	params := responses.ResponseNewParams{
		Model: shared.ResponsesModel(t.model),
		Input: responses.ResponseNewParamsInputUnion{
			OfString: openai.String(userPayload),
		},
		Instructions: openai.String(classificationSystemPrompt),
		Tools:        []responses.ToolUnionParam{tool},
		ToolChoice: responses.ResponseNewParamsToolChoiceUnion{
			OfFunctionTool: &responses.ToolChoiceFunctionParam{
				Name: "record_classifications",
			},
		},
		// Triage is one-shot — no follow-up call references this response,
		// so don't waste server-side storage on it.
		Store: param.NewOpt(false),
	}

	resp, err := t.client.Responses.New(ctx, params)
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

// extractOpenAIDecisions walks the Response output items looking for the
// single forced function_call to record_classifications. Returns an error
// if the call is missing or its arguments don't decode.
func extractOpenAIDecisions(resp *responses.Response) ([]Decision, error) {
	if len(resp.Output) == 0 {
		return nil, errors.New("openai response has no output items")
	}
	for _, item := range resp.Output {
		if item.Type != "function_call" {
			continue
		}
		if item.Name != "record_classifications" {
			continue
		}
		var payload struct {
			Decisions []Decision `json:"decisions"`
		}
		if err := json.Unmarshal([]byte(item.Arguments), &payload); err != nil {
			return nil, fmt.Errorf("decode tool args: %w", err)
		}
		return payload.Decisions, nil
	}
	return nil, errors.New("no record_classifications function_call in response output")
}
