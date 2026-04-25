package triage

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/jdfalk/overnight-burndown/internal/sources"
	"github.com/jdfalk/overnight-burndown/internal/state"
)

// fakeAnthropic spins an httptest server that pretends to be /v1/messages.
//
// `responder` controls the mock:
//   - It receives the parsed request body so tests can assert on the prompt
//     shape, system block cache_control, tool_choice, etc.
//   - It returns the JSON message body the SDK should see.
//
// The returned function is the test cleanup.
type messageReq map[string]any

func fakeAnthropic(t *testing.T, responder func(req messageReq) string) (string, func()) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			http.NotFound(w, r)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var req messageReq
		if err := json.Unmarshal(body, &req); err != nil {
			t.Errorf("decode request: %v", err)
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(responder(req)))
	}))
	return srv.URL, srv.Close
}

// toolUseResponse builds a minimal Messages API response containing a single
// `record_classifications` tool_use block. The SDK parses this exactly like
// a real API response.
func toolUseResponse(decisions []Decision) string {
	input := map[string]any{"decisions": decisions}
	inputJSON, _ := json.Marshal(input)

	resp := map[string]any{
		"id":   "msg_test",
		"type": "message",
		"role": "assistant",
		"model": "claude-opus-4-7",
		"content": []map[string]any{
			{
				"type":  "tool_use",
				"id":    "toolu_01",
				"name":  "record_classifications",
				"input": json.RawMessage(inputJSON),
			},
		},
		"stop_reason":   "tool_use",
		"stop_sequence": nil,
		"usage": map[string]any{
			"input_tokens":         100,
			"output_tokens":        50,
			"cache_creation_input_tokens": 0,
			"cache_read_input_tokens":     0,
		},
	}
	out, _ := json.Marshal(resp)
	return string(out)
}

// makeTask is a small helper to build a sources.Task with sensible defaults.
func makeTask(srcType state.SourceType, url, title string) sources.Task {
	return sources.Task{
		Source: state.Source{
			Type:        srcType,
			Repo:        "jdfalk/audiobook-organizer",
			URL:         url,
			ContentHash: state.HashContent(title),
			Title:       title,
		},
		Body:      title + " body",
		HasAutoOK: true,
	}
}

// ---------------------------------------------------------------------------
// happy path
// ---------------------------------------------------------------------------

func TestTriage_HappyPath(t *testing.T) {
	tasks := []sources.Task{
		makeTask(state.SourceIssue, "https://example/1", "Fix typo in README"),
		makeTask(state.SourceTODO, "TODO.md#L7", "Refactor the dispatcher"),
		makeTask(state.SourcePlan, "plans/foo.md", "Add a new feature for users"),
	}

	url, cleanup := fakeAnthropic(t, func(req messageReq) string {
		// The request must force the record_classifications tool.
		tc, ok := req["tool_choice"].(map[string]any)
		if !ok || tc["type"] != "tool" || tc["name"] != "record_classifications" {
			t.Errorf("expected tool_choice forcing record_classifications, got %v", req["tool_choice"])
		}
		// Cache control must be present on the system block.
		sys, ok := req["system"].([]any)
		if !ok || len(sys) == 0 {
			t.Errorf("expected non-empty system array, got %v", req["system"])
		} else {
			block, _ := sys[0].(map[string]any)
			if cc, _ := block["cache_control"].(map[string]any); cc == nil || cc["type"] != "ephemeral" {
				t.Errorf("expected ephemeral cache_control on system block, got %v", block["cache_control"])
			}
		}
		return toolUseResponse([]Decision{
			{TaskID: "https://example/1", Classification: ClassAutoMergeSafe, Reason: "doc-only typo fix", EstComplexity: 1, SuggestedBranch: "auto/readme-typo"},
			{TaskID: "TODO.md#L7", Classification: ClassNeedsReview, Reason: "refactor — needs human review", EstComplexity: 3, SuggestedBranch: "draft/refactor-dispatcher"},
			{TaskID: "plans/foo.md", Classification: ClassNeedsReview, Reason: "new feature", EstComplexity: 4, SuggestedBranch: "draft/new-feature"},
		})
	})
	defer cleanup()

	tr := NewTriager("claude-opus-4-7", option.WithAPIKey("test"), option.WithBaseURL(url))
	got, err := tr.Triage(context.Background(), tasks)
	if err != nil {
		t.Fatalf("Triage: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 decisions, got %d", len(got))
	}
	if got[0].Classification != ClassAutoMergeSafe {
		t.Errorf("decision 0: got %q", got[0].Classification)
	}
	if got[1].SuggestedBranch != "draft/refactor-dispatcher" {
		t.Errorf("decision 1 branch: got %q", got[1].SuggestedBranch)
	}
}

// ---------------------------------------------------------------------------
// reorder when the model returns decisions out of order
// ---------------------------------------------------------------------------

func TestTriage_ReordersToMatchInput(t *testing.T) {
	tasks := []sources.Task{
		makeTask(state.SourceTODO, "url-a", "task a"),
		makeTask(state.SourceTODO, "url-b", "task b"),
	}
	url, cleanup := fakeAnthropic(t, func(_ messageReq) string {
		// Return swapped order.
		return toolUseResponse([]Decision{
			{TaskID: "url-b", Classification: ClassAutoMergeSafe, Reason: "b", EstComplexity: 1, SuggestedBranch: "auto/b"},
			{TaskID: "url-a", Classification: ClassNeedsReview, Reason: "a", EstComplexity: 2, SuggestedBranch: "draft/a"},
		})
	})
	defer cleanup()

	tr := NewTriager("claude-opus-4-7", option.WithAPIKey("test"), option.WithBaseURL(url))
	got, err := tr.Triage(context.Background(), tasks)
	if err != nil {
		t.Fatalf("Triage: %v", err)
	}
	if got[0].TaskID != "url-a" || got[1].TaskID != "url-b" {
		t.Errorf("expected reordered to match input, got %v %v", got[0].TaskID, got[1].TaskID)
	}
}

// ---------------------------------------------------------------------------
// validation: counts must match
// ---------------------------------------------------------------------------

func TestTriage_RejectsCountMismatch(t *testing.T) {
	tasks := []sources.Task{
		makeTask(state.SourceTODO, "url-a", "a"),
		makeTask(state.SourceTODO, "url-b", "b"),
	}
	url, cleanup := fakeAnthropic(t, func(_ messageReq) string {
		// Only return one decision when two were requested.
		return toolUseResponse([]Decision{
			{TaskID: "url-a", Classification: ClassNeedsReview, Reason: "x", EstComplexity: 1, SuggestedBranch: "draft/a"},
		})
	})
	defer cleanup()

	tr := NewTriager("claude-opus-4-7", option.WithAPIKey("test"), option.WithBaseURL(url))
	_, err := tr.Triage(context.Background(), tasks)
	if err == nil || !strings.Contains(err.Error(), "decision count") {
		t.Errorf("expected count-mismatch error, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// validation: classifications must be in the canonical enum
// ---------------------------------------------------------------------------

func TestTriage_RejectsInvalidClassification(t *testing.T) {
	tasks := []sources.Task{makeTask(state.SourceTODO, "url-a", "a")}
	url, cleanup := fakeAnthropic(t, func(_ messageReq) string {
		return toolUseResponse([]Decision{
			{TaskID: "url-a", Classification: "MAYBE_OK", Reason: "x", EstComplexity: 1, SuggestedBranch: "auto/a"},
		})
	})
	defer cleanup()

	tr := NewTriager("claude-opus-4-7", option.WithAPIKey("test"), option.WithBaseURL(url))
	_, err := tr.Triage(context.Background(), tasks)
	if err == nil || !strings.Contains(err.Error(), "invalid classification") {
		t.Errorf("expected invalid-classification error, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// validation: non-blocked decisions must have a suggested branch
// ---------------------------------------------------------------------------

func TestTriage_RejectsEmptyBranchOnNonBlocked(t *testing.T) {
	tasks := []sources.Task{makeTask(state.SourceTODO, "url-a", "a")}
	url, cleanup := fakeAnthropic(t, func(_ messageReq) string {
		return toolUseResponse([]Decision{
			{TaskID: "url-a", Classification: ClassAutoMergeSafe, Reason: "x", EstComplexity: 1, SuggestedBranch: "  "},
		})
	})
	defer cleanup()

	tr := NewTriager("claude-opus-4-7", option.WithAPIKey("test"), option.WithBaseURL(url))
	_, err := tr.Triage(context.Background(), tasks)
	if err == nil || !strings.Contains(err.Error(), "empty suggested_branch") {
		t.Errorf("expected empty-branch error, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// blocked classification is allowed to omit suggested_branch
// ---------------------------------------------------------------------------

func TestTriage_BlockedTaskMayOmitBranch(t *testing.T) {
	tasks := []sources.Task{makeTask(state.SourceTODO, "url-a", "a")}
	url, cleanup := fakeAnthropic(t, func(_ messageReq) string {
		return toolUseResponse([]Decision{
			{TaskID: "url-a", Classification: ClassBlocked, Reason: "ambiguous", EstComplexity: 1},
		})
	})
	defer cleanup()

	tr := NewTriager("claude-opus-4-7", option.WithAPIKey("test"), option.WithBaseURL(url))
	got, err := tr.Triage(context.Background(), tasks)
	if err != nil {
		t.Fatalf("Triage: %v", err)
	}
	if got[0].Classification != ClassBlocked {
		t.Errorf("expected BLOCKED, got %q", got[0].Classification)
	}
}

// ---------------------------------------------------------------------------
// no record_classifications block in response → error
// ---------------------------------------------------------------------------

func TestTriage_NoToolUseBlock(t *testing.T) {
	tasks := []sources.Task{makeTask(state.SourceTODO, "url-a", "a")}
	url, cleanup := fakeAnthropic(t, func(_ messageReq) string {
		// Return only text content (model failed to call the tool).
		return `{"id":"msg_test","type":"message","role":"assistant","model":"claude-opus-4-7","content":[{"type":"text","text":"sorry"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`
	})
	defer cleanup()

	tr := NewTriager("claude-opus-4-7", option.WithAPIKey("test"), option.WithBaseURL(url))
	_, err := tr.Triage(context.Background(), tasks)
	if err == nil || !strings.Contains(err.Error(), "no record_classifications") {
		t.Errorf("expected no-tool-use error, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// empty input is a no-op
// ---------------------------------------------------------------------------

func TestTriage_EmptyInputNoCall(t *testing.T) {
	calls := 0
	url, cleanup := fakeAnthropic(t, func(_ messageReq) string {
		calls++
		return toolUseResponse(nil)
	})
	defer cleanup()

	tr := NewTriager("claude-opus-4-7", option.WithAPIKey("test"), option.WithBaseURL(url))
	got, err := tr.Triage(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("expected empty result, got %d", len(got))
	}
	if calls != 0 {
		t.Errorf("expected no Anthropic call for empty input, got %d", calls)
	}
}

// ---------------------------------------------------------------------------
// long task bodies are truncated in the user payload
// ---------------------------------------------------------------------------

func TestBuildUserPayload_TruncatesLongBodies(t *testing.T) {
	tasks := []sources.Task{
		{
			Source: state.Source{Type: state.SourceTODO, URL: "u", Title: "t"},
			Body:   strings.Repeat("x", bodyExcerptMaxBytes*2),
		},
	}
	got, err := buildUserPayload(tasks)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "(truncated)") {
		t.Errorf("expected truncation marker; payload was %d chars", len(got))
	}
	if len(got) > bodyExcerptMaxBytes*2 {
		t.Errorf("payload should be roughly bounded by excerpt max, got %d", len(got))
	}
}
