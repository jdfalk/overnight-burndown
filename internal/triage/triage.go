// Package triage classifies tasks into AUTO_MERGE_SAFE / NEEDS_REVIEW / BLOCKED
// using a single batched call to a configurable LLM provider.
//
// Why one batched call: triage is the cheapest opportunity to compose multiple
// tasks into one prefix-cacheable system prompt + one user message, avoiding
// per-task overhead. A 50-task night fits comfortably in a single Opus 4.7
// or GPT-5 request and gets the best possible cache-read economics on
// subsequent nights (the system prompt is identical run-over-run).
//
// Why tool-forced output: the model returns a single forced tool call with
// a strict JSON Schema. Both Anthropic's tool_use and OpenAI's function
// calling support this pattern symmetrically; free-form JSON in text content
// drifts (markdown fences, off-schema fields, trailing prose) on either side.
//
// Backends: see anthropic.go (default) and openai.go for the two
// implementations of Provider. The orchestrator picks one based on
// `triage.provider` in the config.
package triage

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

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
	TaskID          string         `json:"task_id"`
	Classification  Classification `json:"classification"`
	Reason          string         `json:"reason"`
	EstComplexity   int            `json:"est_complexity"`
	SuggestedBranch string         `json:"suggested_branch,omitempty"`
}

// Provider is the LLM-backend abstraction every triage backend satisfies.
// The orchestrator constructs one (Anthropic or OpenAI) based on config and
// passes it to the runner.
type Provider interface {
	Triage(ctx context.Context, tasks []sources.Task) ([]Decision, error)
}

// ---------------------------------------------------------------------------
// shared helpers used by both backend implementations
// ---------------------------------------------------------------------------

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

// validateAgainstInput enforces the post-call invariants every backend must
// honor: count match, valid enum, known task IDs, branch present for
// non-blocked decisions.
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

// jsonSchemaForDecisions returns the JSON schema both providers send as the
// tool's input schema. Identical text on both sides keeps the rulebook
// changes single-sourced.
func jsonSchemaForDecisions() map[string]any {
	return map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"required":             []string{"decisions"},
		"properties": map[string]any{
			"decisions": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type":                 "object",
					"additionalProperties": false,
					"required":             []string{"task_id", "classification", "reason", "est_complexity"},
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
	}
}

