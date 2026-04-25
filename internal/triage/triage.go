// Package triage classifies tasks into AUTO_MERGE_SAFE / NEEDS_REVIEW / BLOCKED
// using a single batched call to Claude.
//
// Why one batched call: triage is the cheapest opportunity to compose multiple
// tasks into one prefix-cacheable system prompt + one user message, avoiding
// per-task overhead. A 50-task night fits comfortably in a single Opus 4.7
// request and gets the best possible cache-read economics on subsequent
// nights (the system prompt is identical run-over-run).
//
// Why tool-forced output instead of free-form JSON: forcing a single
// `record_classifications` tool call gives us a hard schema validated by
// the API. Free-form JSON in `text` content drifts (extra prose, markdown
// fences, off-schema fields) and requires defensive post-parsing.
package triage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/jdfalk/overnight-burndown/internal/sources"
)

// Classification is the triage decision. Strings match the system prompt's
// vocabulary exactly so the model sees what we expect to receive.
type Classification string

const (
	ClassAutoMergeSafe Classification = "AUTO_MERGE_SAFE"
	ClassNeedsReview   Classification = "NEEDS_REVIEW"
	ClassBlocked       Classification = "BLOCKED"
)

// IsValid returns true for the three canonical values.
func (c Classification) IsValid() bool {
	switch c {
	case ClassAutoMergeSafe, ClassNeedsReview, ClassBlocked:
		return true
	}
	return false
}

// Decision is one classification result. The TaskID matches the request's
// task_id (we use the source URL as the stable id since hashes are long
// hex strings the model wastes attention on).
type Decision struct {
	TaskID           string         `json:"task_id"`
	Classification   Classification `json:"classification"`
	Reason           string         `json:"reason"`
	EstComplexity    int            `json:"est_complexity"`
	SuggestedBranch  string         `json:"suggested_branch,omitempty"`
}

// Triager wraps the Anthropic client and remembers the model name. Construct
// once per run; safe for concurrent use.
type Triager struct {
	client anthropic.Client
	model  anthropic.Model
}

// NewTriager builds a Triager. The api key is read from ANTHROPIC_API_KEY by
// default; the optional opts let callers inject `option.WithBaseURL` for
// tests.
func NewTriager(model string, opts ...option.RequestOption) *Triager {
	return &Triager{
		client: anthropic.NewClient(opts...),
		model:  anthropic.Model(model),
	}
}

// Triage classifies a batch of tasks in a single Anthropic call.
//
// Behavior:
//   - Returns one Decision per input task, in the same order.
//   - Errors are returned without partial results; the caller decides whether
//     to retry or skip the night.
//   - The system prompt is marked cache_control=ephemeral so subsequent
//     calls (same night, next night) read from cache at ~0.1× input price.
func (t *Triager) Triage(ctx context.Context, tasks []sources.Task) ([]Decision, error) {
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
				Properties: map[string]any{
					"decisions": map[string]any{
						"type": "array",
						"items": map[string]any{
							"type": "object",
							"additionalProperties": false,
							"required": []string{"task_id", "classification", "reason", "est_complexity"},
							"properties": map[string]any{
								"task_id":          map[string]any{"type": "string"},
								"classification":   map[string]any{"type": "string", "enum": []string{string(ClassAutoMergeSafe), string(ClassNeedsReview), string(ClassBlocked)}},
								"reason":           map[string]any{"type": "string"},
								"est_complexity":   map[string]any{"type": "integer", "minimum": 1, "maximum": 5},
								"suggested_branch": map[string]any{"type": "string"},
							},
						},
					},
				},
				Required: []string{"decisions"},
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

	decisions, err := extractDecisions(resp)
	if err != nil {
		return nil, fmt.Errorf("triage: extract decisions: %w", err)
	}

	if err := validateAgainstInput(tasks, decisions); err != nil {
		return nil, fmt.Errorf("triage: validate: %w", err)
	}

	// Reorder to match the input slice. The model is instructed to preserve
	// order but small models occasionally swap; defensive reorder keeps the
	// caller's invariants simple.
	return reorderToInput(tasks, decisions), nil
}

// taskInput is the per-task shape we hand the model. We deliberately omit
// the full body — too many tokens for triage. A 600-char excerpt is plenty
// to classify, and keeping it short improves cache write/read ratios.
type taskInput struct {
	TaskID      string `json:"task_id"`
	Repo        string `json:"repo"`
	SourceType  string `json:"source_type"`
	HasAutoOK   bool   `json:"has_auto_ok"`
	Title       string `json:"title"`
	BodyExcerpt string `json:"body_excerpt"`
}

const bodyExcerptMaxBytes = 600

func buildUserPayload(tasks []sources.Task) (string, error) {
	in := make([]taskInput, 0, len(tasks))
	for _, t := range tasks {
		body := t.Body
		if len(body) > bodyExcerptMaxBytes {
			body = body[:bodyExcerptMaxBytes] + "…(truncated)"
		}
		in = append(in, taskInput{
			TaskID:      t.Source.URL,
			Repo:        t.Source.Repo,
			SourceType:  string(t.Source.Type),
			HasAutoOK:   t.HasAutoOK,
			Title:       t.Source.Title,
			BodyExcerpt: body,
		})
	}
	b, err := json.Marshal(map[string]any{"tasks": in})
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func extractDecisions(resp *anthropic.Message) ([]Decision, error) {
	for _, block := range resp.Content {
		switch v := block.AsAny().(type) {
		case anthropic.ToolUseBlock:
			if v.Name != "record_classifications" {
				continue
			}
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

func validateAgainstInput(tasks []sources.Task, decisions []Decision) error {
	if len(decisions) != len(tasks) {
		return fmt.Errorf("decision count %d != task count %d", len(decisions), len(tasks))
	}
	want := make(map[string]struct{}, len(tasks))
	for _, t := range tasks {
		want[t.Source.URL] = struct{}{}
	}
	for _, d := range decisions {
		if !d.Classification.IsValid() {
			return fmt.Errorf("invalid classification %q for task %q", d.Classification, d.TaskID)
		}
		if _, ok := want[d.TaskID]; !ok {
			return fmt.Errorf("decision references unknown task %q", d.TaskID)
		}
		if d.Classification != ClassBlocked && strings.TrimSpace(d.SuggestedBranch) == "" {
			return fmt.Errorf("non-blocked task %q has empty suggested_branch", d.TaskID)
		}
	}
	return nil
}

func reorderToInput(tasks []sources.Task, decisions []Decision) []Decision {
	byID := make(map[string]Decision, len(decisions))
	for _, d := range decisions {
		byID[d.TaskID] = d
	}
	out := make([]Decision, 0, len(tasks))
	for _, t := range tasks {
		out = append(out, byID[t.Source.URL])
	}
	return out
}
