// file: internal/triagepoll/batch.go
// version: 1.0.0
// guid: 5b6c7d8e-9f0a-1b2c-3d4e-5f6a7b8c9d0e
//
// OpenAI Batch API operations for async triage. The batch API gives a 50%
// cost discount over synchronous calls by letting OpenAI schedule the work
// during idle capacity (max 24h window, usually done in minutes).

package triagepoll

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/openai/openai-go"
	"github.com/openai/openai-go/option"
)

// Decision is the per-issue triage result written back to the hub repo.
type Decision struct {
	IssueNumber     int    `json:"issue_number"`
	Classification  string `json:"classification"`   // AUTO_MERGE_SAFE | NEEDS_REVIEW | BLOCKED
	Reason          string `json:"reason"`
	EstComplexity   int    `json:"est_complexity"`   // 1–5
	SuggestedBranch string `json:"suggested_branch"` // empty if BLOCKED
}

// BatchResult carries the outcome of a completed OpenAI batch.
type BatchResult struct {
	Decisions []Decision
	// FailedCount is the number of requests that failed within the batch.
	FailedCount int
}

// triageSystemPrompt is the system-level instruction for the triage model.
// Identical text on every run maximises server-side prompt-cache hits.
const triageSystemPrompt = `You are a senior software engineer performing triage on GitHub issues.

For each issue, output a JSON object with:
- classification: one of AUTO_MERGE_SAFE, NEEDS_REVIEW, BLOCKED
- reason: one sentence explaining the decision
- est_complexity: integer 1–5 (1=trivial, 5=very complex)
- suggested_branch: short kebab-case branch name (empty string if BLOCKED)

Classification rules:
- AUTO_MERGE_SAFE: low-risk change, well-described, no ambiguity, complexity ≤ 2
- NEEDS_REVIEW: valid task but needs human review before or after implementation
- BLOCKED: unclear, conflicting, or missing information; cannot be implemented safely

Respond ONLY with valid JSON matching the schema. No prose, no markdown fences.`

// triageUserTemplate is the per-issue prompt template.
const triageUserTemplate = `Triage this GitHub issue:

Title: %s
Body:
%s

Respond with JSON only.`

// batchRequestBody is the shape of each line in the uploaded JSONL file.
type batchRequestBody struct {
	Model    string              `json:"model"`
	Messages []batchChatMessage  `json:"messages"`
	ResponseFormat batchResponseFormat `json:"response_format"`
	MaxTokens int               `json:"max_tokens"`
}

type batchChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type batchResponseFormat struct {
	Type       string             `json:"type"`
	JSONSchema *batchJSONSchema   `json:"json_schema,omitempty"`
}

type batchJSONSchema struct {
	Name   string         `json:"name"`
	Strict bool           `json:"strict"`
	Schema map[string]any `json:"schema"`
}

type batchLine struct {
	CustomID string           `json:"custom_id"`
	Method   string           `json:"method"`
	URL      string           `json:"url"`
	Body     batchRequestBody `json:"body"`
}

// triageSchema is the JSON Schema enforced on each model response.
var triageSchema = map[string]any{
	"type":                 "object",
	"additionalProperties": false,
	"required":             []string{"classification", "reason", "est_complexity", "suggested_branch"},
	"properties": map[string]any{
		"classification":   map[string]any{"type": "string", "enum": []string{"AUTO_MERGE_SAFE", "NEEDS_REVIEW", "BLOCKED"}},
		"reason":           map[string]any{"type": "string"},
		"est_complexity":   map[string]any{"type": "integer", "minimum": 1, "maximum": 5},
		"suggested_branch": map[string]any{"type": "string"},
	},
}

// SubmitBatch builds a JSONL batch request, uploads it, and creates the batch.
// Returns the OpenAI batch ID that callers should store in the tracking issue.
func SubmitBatch(ctx context.Context, apiKey, model string, issues []HubIssue) (string, error) {
	cl := openai.NewClient(option.WithAPIKey(apiKey))

	// Build JSONL — one line per issue.
	var buf bytes.Buffer
	for _, iss := range issues {
		body := iss.Body
		if len(body) > 800 {
			body = body[:800] + "\n…(truncated)"
		}
		userMsg := fmt.Sprintf(triageUserTemplate, iss.Title, body)

		line := batchLine{
			CustomID: fmt.Sprintf("issue-%d", iss.Number),
			Method:   "POST",
			URL:      "/v1/chat/completions",
			Body: batchRequestBody{
				Model: model,
				Messages: []batchChatMessage{
					{Role: "system", Content: triageSystemPrompt},
					{Role: "user", Content: userMsg},
				},
				ResponseFormat: batchResponseFormat{
					Type: "json_schema",
					JSONSchema: &batchJSONSchema{
						Name:   "triage_decision",
						Strict: true,
						Schema: triageSchema,
					},
				},
				MaxTokens: 256,
			},
		}

		b, err := json.Marshal(line)
		if err != nil {
			return "", fmt.Errorf("triagepoll: marshal batch line for issue #%d: %w", iss.Number, err)
		}
		buf.Write(b)
		buf.WriteByte('\n')
	}

	// Upload the JSONL file.
	fileObj, err := cl.Files.New(ctx, openai.FileNewParams{
		File:    bytes.NewReader(buf.Bytes()),
		Purpose: openai.FilePurposeBatch,
	})
	if err != nil {
		return "", fmt.Errorf("triagepoll: upload batch file: %w", err)
	}

	// Create the batch.
	batch, err := cl.Batches.New(ctx, openai.BatchNewParams{
		InputFileID:      fileObj.ID,
		Endpoint:         openai.BatchNewParamsEndpointV1ChatCompletions,
		CompletionWindow: openai.BatchNewParamsCompletionWindow24h,
	})
	if err != nil {
		return "", fmt.Errorf("triagepoll: create batch: %w", err)
	}

	return batch.ID, nil
}

// BatchStatus represents the terminal or intermediate state of a batch.
type BatchStatus string

const (
	BatchStatusInProgress BatchStatus = "in_progress"
	BatchStatusFinalizing BatchStatus = "finalizing"
	BatchStatusCompleted  BatchStatus = "completed"
	BatchStatusFailed     BatchStatus = "failed"
	BatchStatusExpired    BatchStatus = "expired"
	BatchStatusCancelled  BatchStatus = "cancelled"
)

// IsTerminal returns true when the batch will not make further progress.
func (s BatchStatus) IsTerminal() bool {
	switch s {
	case BatchStatusCompleted, BatchStatusFailed, BatchStatusExpired, BatchStatusCancelled:
		return true
	}
	return false
}

// PollBatch checks the current status of a batch. If completed, it downloads
// the output and parses decisions keyed by issue number.
func PollBatch(ctx context.Context, apiKey, batchID string) (BatchStatus, *BatchResult, error) {
	cl := openai.NewClient(option.WithAPIKey(apiKey))

	batch, err := cl.Batches.Get(ctx, batchID)
	if err != nil {
		return "", nil, fmt.Errorf("triagepoll: poll batch %s: %w", batchID, err)
	}

	status := BatchStatus(batch.Status)
	if status != BatchStatusCompleted {
		return status, nil, nil
	}

	if batch.OutputFileID == "" {
		return BatchStatusFailed, nil, fmt.Errorf("triagepoll: batch %s completed but has no output_file_id", batchID)
	}

	result, err := downloadAndParse(ctx, cl, batch.OutputFileID, int(batch.RequestCounts.Failed))
	if err != nil {
		return BatchStatusFailed, nil, err
	}
	return BatchStatusCompleted, result, nil
}

// batchOutputLine is one result line from the batch output JSONL.
type batchOutputLine struct {
	CustomID string `json:"custom_id"`
	Response struct {
		StatusCode int `json:"status_code"`
		Body       struct {
			Choices []struct {
				Message struct {
					Content string `json:"content"`
				} `json:"message"`
			} `json:"choices"`
		} `json:"body"`
	} `json:"response"`
	Error *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

type triageDecisionJSON struct {
	Classification  string `json:"classification"`
	Reason          string `json:"reason"`
	EstComplexity   int    `json:"est_complexity"`
	SuggestedBranch string `json:"suggested_branch"`
}

func downloadAndParse(ctx context.Context, cl openai.Client, outputFileID string, failedCount int) (*BatchResult, error) {
	resp, err := cl.Files.Content(ctx, outputFileID)
	if err != nil {
		return nil, fmt.Errorf("triagepoll: download output file %s: %w", outputFileID, err)
	}
	defer resp.Body.Close()

	var decisions []Decision
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		var out batchOutputLine
		if err := json.Unmarshal([]byte(line), &out); err != nil {
			return nil, fmt.Errorf("triagepoll: parse output line: %w", err)
		}
		if out.Error != nil {
			// Individual request failed — already counted in failedCount.
			continue
		}

		issNum, err := parseIssueNumber(out.CustomID)
		if err != nil {
			return nil, fmt.Errorf("triagepoll: bad custom_id %q: %w", out.CustomID, err)
		}

		if len(out.Response.Body.Choices) == 0 {
			return nil, fmt.Errorf("triagepoll: no choices in response for %s", out.CustomID)
		}
		content := out.Response.Body.Choices[0].Message.Content

		var dec triageDecisionJSON
		if err := json.Unmarshal([]byte(content), &dec); err != nil {
			return nil, fmt.Errorf("triagepoll: parse decision JSON for %s: %w", out.CustomID, err)
		}

		decisions = append(decisions, Decision{
			IssueNumber:     issNum,
			Classification:  dec.Classification,
			Reason:          dec.Reason,
			EstComplexity:   dec.EstComplexity,
			SuggestedBranch: dec.SuggestedBranch,
		})
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("triagepoll: scan output: %w", err)
	}

	return &BatchResult{Decisions: decisions, FailedCount: failedCount}, nil
}

// parseIssueNumber extracts the integer from a "issue-N" custom_id.
func parseIssueNumber(customID string) (int, error) {
	const prefix = "issue-"
	if !strings.HasPrefix(customID, prefix) {
		return 0, fmt.Errorf("expected prefix %q", prefix)
	}
	n, err := strconv.Atoi(customID[len(prefix):])
	if err != nil {
		return 0, err
	}
	return n, nil
}
