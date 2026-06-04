// file: internal/triagepoll/batch.go
// version: 2.0.0
// guid: 5b6c7d8e-9f0a-1b2c-3d4e-5f6a7b8c9d0e
//
// OpenAI Batch API operations using the Responses API endpoint (/v1/responses).
// The Responses API supports gpt-5.3-codex and gives a 50% cost discount over
// synchronous calls by scheduling work during OpenAI idle capacity (max 24h,
// typically done in minutes).
//
// Key differences from the Chat Completions batch API:
//   - Endpoint: /v1/responses (not /v1/chat/completions)
//   - Request body: input+instructions replaces messages array
//   - Structured output: text.format replaces response_format
//   - Token limit: max_output_tokens replaces max_tokens
//   - Output JSONL: response.body.output[].content[].text (not choices[].message.content)

package triagepoll

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	"github.com/openai/openai-go"
	"github.com/openai/openai-go/option"
)

// Decision is the per-issue triage result written back to the hub repo.
type Decision struct {
	IssueNumber     int    `json:"issue_number"`
	Classification  string `json:"classification"`   // AUTO_MERGE_SAFE | NEEDS_REVIEW | BLOCKED
	Priority        string `json:"priority"`          // P0 | P1 | P2 | P3
	Reason          string `json:"reason"`
	EstComplexity   int    `json:"est_complexity"`   // 1–5
	SuggestedBranch string `json:"suggested_branch"` // empty if BLOCKED
	AffectedArea    string `json:"affected_area"`
}

// BatchResult carries the outcome of a completed OpenAI batch.
type BatchResult struct {
	Decisions   []Decision
	FailedCount int
}

// triageSystemPrompt is the system instruction for gpt-5.3-codex triage.
// Identical text on every run maximises server-side prompt-cache hits.
const triageSystemPrompt = `You are a senior Go/React software engineer performing triage on GitHub issues for an audiobook organizer application.

## Project Context
This is a production audiobook management system with:
- Go backend: gin HTTP server, PebbleDB primary key-value store, NutsDB activity log, ~50K books in production
- React/TypeScript frontend: Vite build, library/book management UI, diagnostics page, activity log viewer
- Key subsystems: iTunes sync (remote Windows XML), AcoustID fingerprinting, OpenAI batch metadata enrichment, taglib tag writing, quarantine/dedup pipelines, graceful file ops
- Database: PebbleDB is the ONLY production DB. Migrations are additive; UpdateBook does FULL column replacement.
- CI gate: 80% test coverage required per file, rebase/FF-only merges, all changes via PRs with conventional commits

## Classification Rules

AUTO_MERGE_SAFE — all of the following must be true:
- Complexity ≤ 2 (trivial to well-scoped, no architectural decision needed)
- No DB migrations, no auth changes, no production data path changes
- No iTunes sync, fingerprinting, or tag-writing changes (these have silent failure modes)
- Success criteria are unambiguous from the issue text alone
- Has no dependency on other open issues

NEEDS_REVIEW — use when any of the following apply:
- Complexity 3–5, or involves DB migrations, auth, or external API contract changes
- Touches iTunes sync, AcoustID, taglib, or production file paths (risk of data loss)
- Has UX implications that require design input or user feedback
- Depends on another open issue or requires coordination
- The fix is clear but the correct approach has meaningful trade-offs

BLOCKED — use when any of the following apply:
- Missing reproduction steps for a bug
- Conflicting or contradictory requirements
- Scope unclear enough that two engineers would implement it differently
- Requires breaking changes with no stated migration path
- Issue is a duplicate or already fixed

## Priority Rules

P0: Production outage, data loss risk, or security vulnerability (auth bypass, path traversal, PII exposure)
P1: Core feature broken or severely degraded for all users; blocks other development work
P2: Standard bug fix or feature addition with clear scope and no production risk
P3: Minor improvement, cosmetic fix, docs, or low-urgency cleanup

## Affected Areas (choose the single best fit)

api, ui, db, testing, itunes, fingerprint, metadata, dedup, quarantine, auth, scheduler, maintenance, docs, infra

## Output Requirements

Respond ONLY with valid JSON — no prose, no markdown fences, no explanation outside the JSON object:

{
  "classification": "AUTO_MERGE_SAFE" | "NEEDS_REVIEW" | "BLOCKED",
  "priority": "P0" | "P1" | "P2" | "P3",
  "reason": "<one sentence — state the specific risk, gap, or condition that drove this classification>",
  "est_complexity": <1–5 integer>,
  "suggested_branch": "<kebab-case branch name, or empty string if BLOCKED>",
  "affected_area": "<one area from the list above>"
}`

// triageUserTemplate is the per-issue prompt.
const triageUserTemplate = `Triage this GitHub issue for the audiobook organizer project:

Title: %s

Body:
%s

Respond with JSON only.`

// responsesAPIBatchBody is the Responses API request body for the batch JSONL.
// Uses input+instructions+text.format instead of messages+response_format.
type responsesAPIBatchBody struct {
	Model           string            `json:"model"`
	Input           string            `json:"input"`
	Instructions    string            `json:"instructions"`
	Text            responsesTextConfig `json:"text"`
	MaxOutputTokens int               `json:"max_output_tokens"`
	Store           bool              `json:"store"`
	User            string            `json:"user,omitempty"`
	Metadata        map[string]string `json:"metadata,omitempty"`
}

type responsesTextConfig struct {
	Format responsesTextFormat `json:"format"`
}

type responsesTextFormat struct {
	Type   string         `json:"type"`
	Name   string         `json:"name,omitempty"`
	Strict bool           `json:"strict,omitempty"`
	Schema map[string]any `json:"schema,omitempty"`
}

type batchLine struct {
	CustomID string                `json:"custom_id"`
	Method   string                `json:"method"`
	URL      string                `json:"url"`
	Body     responsesAPIBatchBody `json:"body"`
}

// triageSchema enforces the structured output shape from the model.
var triageSchema = map[string]any{
	"type":                 "object",
	"additionalProperties": false,
	"required":             []string{"classification", "priority", "reason", "est_complexity", "suggested_branch", "affected_area"},
	"properties": map[string]any{
		"classification":   map[string]any{"type": "string", "enum": []string{"AUTO_MERGE_SAFE", "NEEDS_REVIEW", "BLOCKED"}},
		"priority":         map[string]any{"type": "string", "enum": []string{"P0", "P1", "P2", "P3"}},
		"reason":           map[string]any{"type": "string"},
		"est_complexity":   map[string]any{"type": "integer", "minimum": 1, "maximum": 5},
		"suggested_branch": map[string]any{"type": "string"},
		"affected_area":    map[string]any{"type": "string"},
	},
}

// SubmitBatch builds a Responses API JSONL batch request, uploads it, and
// creates the batch. Returns the OpenAI batch ID.
func SubmitBatch(ctx context.Context, apiKey, model string, issues []HubIssue) (string, error) {
	cl := openai.NewClient(option.WithAPIKey(apiKey))

	var buf bytes.Buffer
	for _, iss := range issues {
		body := iss.Body
		if len(body) > 1200 {
			body = body[:1200] + "\n…(truncated)"
		}

		line := batchLine{
			CustomID: fmt.Sprintf("issue-%d", iss.Number),
			Method:   "POST",
			URL:      "/v1/responses",
			Body: responsesAPIBatchBody{
				Model:        model,
				Input:        fmt.Sprintf(triageUserTemplate, iss.Title, body),
				Instructions: triageSystemPrompt,
				Text: responsesTextConfig{
					Format: responsesTextFormat{
						Type:   "json_schema",
						Name:   "triage_decision",
						Strict: true,
						Schema: triageSchema,
					},
				},
				MaxOutputTokens: 512,
				Store:           false,
				User:            "ao-triage-poll",
				Metadata: map[string]string{
					"service":      "ao-triage-poll",
					"issue_number": fmt.Sprintf("%d", iss.Number),
				},
			},
		}

		b, err := json.Marshal(line)
		if err != nil {
			return "", fmt.Errorf("triagepoll: marshal batch line for issue #%d: %w", iss.Number, err)
		}
		buf.Write(b)
		buf.WriteByte('\n')
	}

	fileObj, err := cl.Files.New(ctx, openai.FileNewParams{
		File:    bytes.NewReader(buf.Bytes()),
		Purpose: openai.FilePurposeBatch,
	})
	if err != nil {
		return "", fmt.Errorf("triagepoll: upload batch file: %w", err)
	}

	batch, err := cl.Batches.New(ctx, openai.BatchNewParams{
		InputFileID:      fileObj.ID,
		Endpoint:         openai.BatchNewParamsEndpointV1Responses,
		CompletionWindow: openai.BatchNewParamsCompletionWindow24h,
	})
	if err != nil {
		return "", fmt.Errorf("triagepoll: create batch: %w", err)
	}

	slog.InfoContext(ctx, "triagepoll: batch submitted via Responses API",
		"batch_id", batch.ID,
		"model", model,
		"issue_count", len(issues))
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

// PollBatch checks the current status of a batch. If completed, downloads and
// parses decisions. Errors from the error_file are logged for diagnosis.
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

	// Download error file for diagnostics before checking output.
	if batch.ErrorFileID != "" {
		logBatchErrors(ctx, cl, batch.ErrorFileID, batchID)
	}

	if batch.OutputFileID == "" {
		slog.WarnContext(ctx, "triagepoll: batch completed with no output_file_id, treating as failed",
			"batch_id", batchID,
			"request_counts_failed", batch.RequestCounts.Failed,
			"request_counts_total", batch.RequestCounts.Total)
		return BatchStatusFailed, nil, nil
	}

	result, err := downloadAndParse(ctx, cl, batch.OutputFileID, int(batch.RequestCounts.Failed))
	if err != nil {
		return BatchStatusFailed, nil, err
	}
	return BatchStatusCompleted, result, nil
}

// logBatchErrors downloads the error file and logs each individual request error.
func logBatchErrors(ctx context.Context, cl openai.Client, errorFileID, batchID string) {
	resp, err := cl.Files.Content(ctx, errorFileID)
	if err != nil {
		slog.WarnContext(ctx, "triagepoll: could not download error file",
			"batch_id", batchID, "error_file_id", errorFileID, "err", err)
		return
	}
	defer resp.Body.Close()

	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var errLine struct {
			CustomID string `json:"custom_id"`
			Error    *struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal([]byte(line), &errLine) == nil && errLine.Error != nil {
			slog.ErrorContext(ctx, "triagepoll: batch request error",
				"batch_id", batchID,
				"custom_id", errLine.CustomID,
				"code", errLine.Error.Code,
				"message", errLine.Error.Message)
		}
	}
}

// responsesOutputLine is one result line from the Responses API batch output JSONL.
// Text lives at output[N].content[M].text, not choices[].message.content.
type responsesOutputLine struct {
	CustomID string `json:"custom_id"`
	Response struct {
		StatusCode int `json:"status_code"`
		Body       struct {
			Output []struct {
				Type    string `json:"type"`
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			} `json:"output"`
		} `json:"body"`
	} `json:"response"`
	Error *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

type triageDecisionJSON struct {
	Classification  string `json:"classification"`
	Priority        string `json:"priority"`
	Reason          string `json:"reason"`
	EstComplexity   int    `json:"est_complexity"`
	SuggestedBranch string `json:"suggested_branch"`
	AffectedArea    string `json:"affected_area"`
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

		var out responsesOutputLine
		if err := json.Unmarshal([]byte(line), &out); err != nil {
			return nil, fmt.Errorf("triagepoll: parse output line: %w", err)
		}
		if out.Error != nil {
			slog.WarnContext(ctx, "triagepoll: individual request error in output",
				"custom_id", out.CustomID,
				"code", out.Error.Code,
				"message", out.Error.Message)
			continue
		}

		issNum, err := parseIssueNumber(out.CustomID)
		if err != nil {
			return nil, fmt.Errorf("triagepoll: bad custom_id %q: %w", out.CustomID, err)
		}

		text := extractResponseText(out)
		if text == "" {
			slog.WarnContext(ctx, "triagepoll: no output text for request", "custom_id", out.CustomID)
			continue
		}

		var dec triageDecisionJSON
		if err := json.Unmarshal([]byte(text), &dec); err != nil {
			return nil, fmt.Errorf("triagepoll: parse decision JSON for %s: %w", out.CustomID, err)
		}

		decisions = append(decisions, Decision{
			IssueNumber:     issNum,
			Classification:  dec.Classification,
			Priority:        dec.Priority,
			Reason:          dec.Reason,
			EstComplexity:   dec.EstComplexity,
			SuggestedBranch: dec.SuggestedBranch,
			AffectedArea:    dec.AffectedArea,
		})
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("triagepoll: scan output: %w", err)
	}

	return &BatchResult{Decisions: decisions, FailedCount: failedCount}, nil
}

// extractResponseText finds the first output_text content item in a Responses API output line.
func extractResponseText(out responsesOutputLine) string {
	for _, item := range out.Response.Body.Output {
		if item.Type != "message" {
			continue
		}
		for _, c := range item.Content {
			if c.Type == "output_text" && c.Text != "" {
				return c.Text
			}
		}
	}
	return ""
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
