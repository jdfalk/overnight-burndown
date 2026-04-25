package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/jdfalk/overnight-burndown/internal/mcp"
	"github.com/jdfalk/overnight-burndown/internal/sources"
	"github.com/jdfalk/overnight-burndown/internal/state"
	"github.com/jdfalk/overnight-burndown/internal/triage"
)

// stubMCP is a fake MCPClient. ListTools returns a fixed catalog. CallTool
// records each call and returns a scripted response.
type stubMCP struct {
	tools     []mcp.ToolDef
	responses map[string]*mcp.CallResult // by tool name
	errs      map[string]error           // by tool name

	mu    sync.Mutex
	calls []stubCall
}

type stubCall struct {
	Name string
	Args map[string]any
}

func (s *stubMCP) ListTools(_ context.Context) ([]mcp.ToolDef, error) {
	return s.tools, nil
}

func (s *stubMCP) CallTool(_ context.Context, name string, args any) (*mcp.CallResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, _ := args.(map[string]any)
	s.calls = append(s.calls, stubCall{Name: name, Args: a})

	if err, ok := s.errs[name]; ok {
		return nil, err
	}
	if r, ok := s.responses[name]; ok {
		return r, nil
	}
	return &mcp.CallResult{Text: "ok"}, nil
}

func (s *stubMCP) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.calls)
}

// scriptedResponses mocks /v1/messages with a queue of pre-canned response
// JSON strings. The httptest server pops one per request; the test fails if
// the queue empties before the agent stops.
func scriptedAnthropic(t *testing.T, scripts []string) (string, *atomic.Int32, func()) {
	t.Helper()
	idx := &atomic.Int32{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		i := int(idx.Add(1)) - 1
		if i >= len(scripts) {
			t.Errorf("anthropic call %d had no scripted response", i)
			http.Error(w, "no script", http.StatusInternalServerError)
			return
		}
		// Drain body for symmetry; we don't assert on it here.
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(scripts[i]))
	}))
	return srv.URL, idx, srv.Close
}

// makeMessage produces a JSON Messages-API response body. content is the
// list of content-block maps to include; stopReason is e.g. "tool_use" or
// "end_turn".
func makeMessage(stopReason string, content []map[string]any) string {
	resp := map[string]any{
		"id":            "msg_test",
		"type":          "message",
		"role":          "assistant",
		"model":         "claude-haiku-4-5",
		"content":       content,
		"stop_reason":   stopReason,
		"stop_sequence": nil,
		"usage": map[string]any{
			"input_tokens":  10,
			"output_tokens": 10,
		},
	}
	out, _ := json.Marshal(resp)
	return string(out)
}

func textBlock(s string) map[string]any {
	return map[string]any{"type": "text", "text": s}
}

func toolUseBlock(id, name string, input map[string]any) map[string]any {
	return map[string]any{
		"type":  "tool_use",
		"id":    id,
		"name":  name,
		"input": input,
	}
}

// fixtureOpts builds an Options pre-wired to httptest + stubMCP.
func fixtureOpts(t *testing.T, anthropicURL string, m MCPClient) Options {
	t.Helper()
	return Options{
		Client: anthropic.NewClient(
			option.WithAPIKey("test"),
			option.WithBaseURL(anthropicURL),
		),
		MCP:   m,
		Model: anthropic.Model("claude-haiku-4-5"),
		Task: sources.Task{
			Source: state.Source{
				Type:        state.SourceIssue,
				Repo:        "jdfalk/audiobook-organizer",
				URL:         "https://example/1",
				ContentHash: "abc",
				Title:       "Fix typo in README",
			},
			Body:      "There is a typo in README.md on line 12: 'recieve' should be 'receive'.",
			HasAutoOK: true,
		},
		Decision: triage.Decision{
			TaskID:          "https://example/1",
			Classification:  triage.ClassAutoMergeSafe,
			Reason:          "doc-only typo fix",
			EstComplexity:   1,
			SuggestedBranch: "auto/readme-typo",
		},
		Branch:        "auto/readme-typo",
		WorktreeRoot:  "/tmp/burndown-x",
		MaxIterations: 5,
	}
}

func defaultStubMCP() *stubMCP {
	return &stubMCP{
		tools: []mcp.ToolDef{
			{
				Name:        "fs_read",
				Description: "Read a file",
				InputSchema: map[string]any{
					"type":     "object",
					"required": []string{"path"},
					"properties": map[string]any{
						"path": map[string]any{"type": "string"},
					},
				},
			},
			{
				Name:        "fs_write",
				Description: "Write a file",
				InputSchema: map[string]any{
					"type":     "object",
					"required": []string{"path", "content"},
					"properties": map[string]any{
						"path":    map[string]any{"type": "string"},
						"content": map[string]any{"type": "string"},
					},
				},
			},
			// git_branch is in the catalog but NOT in the allowlist —
			// every test should confirm the agent can't see it.
			{
				Name:        "git_branch",
				Description: "Create a branch",
				InputSchema: map[string]any{"type": "object"},
			},
		},
		responses: map[string]*mcp.CallResult{
			"fs_read":  {Text: `{"code":0,"stdout":"existing file content with recieve typo","stderr":""}`},
			"fs_write": {Text: `{"code":0,"stdout":"","stderr":""}`},
		},
	}
}

// ---------------------------------------------------------------------------
// happy path: read → write → end_turn
// ---------------------------------------------------------------------------

func TestRun_HappyPath(t *testing.T) {
	mc := defaultStubMCP()
	scripts := []string{
		// Iter 1: agent calls fs_read
		makeMessage("tool_use", []map[string]any{
			textBlock("Reading the file first."),
			toolUseBlock("toolu_1", "fs_read", map[string]any{"path": "README.md"}),
		}),
		// Iter 2: agent calls fs_write with the corrected content
		makeMessage("tool_use", []map[string]any{
			toolUseBlock("toolu_2", "fs_write", map[string]any{
				"path":    "README.md",
				"content": "fixed file content with receive",
			}),
		}),
		// Iter 3: agent emits final summary and end_turn
		makeMessage("end_turn", []map[string]any{
			textBlock("Fixed the typo in README.md (recieve → receive). No tests to run for a docs change."),
		}),
	}
	url, _, cleanup := scriptedAnthropic(t, scripts)
	defer cleanup()

	res, err := Run(context.Background(), fixtureOpts(t, url, mc))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Iterations != 3 {
		t.Errorf("Iterations: got %d want 3", res.Iterations)
	}
	if res.StopReason != anthropic.StopReasonEndTurn {
		t.Errorf("StopReason: got %q", res.StopReason)
	}
	if !strings.Contains(res.Summary, "Fixed the typo") {
		t.Errorf("Summary not captured: %q", res.Summary)
	}
	if mc.callCount() != 2 {
		t.Errorf("MCP call count: got %d want 2", mc.callCount())
	}
}

// ---------------------------------------------------------------------------
// allowlist filtering — git_branch must NOT be in the registered tools
// ---------------------------------------------------------------------------

func TestRun_FiltersDisallowedTools(t *testing.T) {
	mc := defaultStubMCP()
	scripts := []string{
		makeMessage("end_turn", []map[string]any{textBlock("Done.")}),
	}
	url, _, cleanup := scriptedAnthropic(t, scripts)
	defer cleanup()

	// Capture the request the SDK sends to verify the tool list.
	var requestBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		requestBody = string(body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(scripts[0]))
	}))
	defer srv.Close()

	opts := fixtureOpts(t, srv.URL, mc)
	if _, err := Run(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(requestBody, `"name":"fs_read"`) {
		t.Errorf("expected fs_read in request: %s", requestBody)
	}
	if strings.Contains(requestBody, `"name":"git_branch"`) {
		t.Errorf("git_branch leaked into the agent's tool list: %s", requestBody)
	}

	// Also use the url variable so the scripted server defer doesn't leak.
	_ = url
}

// ---------------------------------------------------------------------------
// MCP errors are surfaced to the agent as is_error tool results, not
// returned from Run — the agent can recover.
// ---------------------------------------------------------------------------

func TestRun_MCPErrorBecomesIsErrorToolResult(t *testing.T) {
	mc := defaultStubMCP()
	mc.errs = map[string]error{
		"fs_read": errors.New("simulated mcp failure"),
	}

	scripts := []string{
		// Iter 1: agent tries to read; MCP errors
		makeMessage("tool_use", []map[string]any{
			toolUseBlock("toolu_1", "fs_read", map[string]any{"path": "missing.md"}),
		}),
		// Iter 2: agent gives up cleanly
		makeMessage("end_turn", []map[string]any{
			textBlock("BLOCKED: tool returned an error reading the source file."),
		}),
	}

	requests := []string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		requests = append(requests, string(body))
		w.Header().Set("Content-Type", "application/json")
		i := len(requests) - 1
		_, _ = w.Write([]byte(scripts[i]))
	}))
	defer srv.Close()

	res, err := Run(context.Background(), fixtureOpts(t, srv.URL, mc))
	if err != nil {
		t.Fatalf("Run should not return an error for an MCP-level failure: %v", err)
	}
	if res.StopReason != anthropic.StopReasonEndTurn {
		t.Errorf("expected end_turn after recovery, got %q", res.StopReason)
	}

	// Iter 2's request should include the tool_result with is_error=true.
	if !strings.Contains(requests[1], `"is_error":true`) {
		t.Errorf("iter 2 request should carry is_error tool_result, got: %s", requests[1])
	}
	if !strings.Contains(requests[1], "simulated mcp failure") {
		t.Errorf("iter 2 should surface the MCP error message: %s", requests[1])
	}
}

// ---------------------------------------------------------------------------
// invalid JSON in a ToolUseBlock's input — the agent gets an is_error
// rather than the loop crashing
// ---------------------------------------------------------------------------

// (Hard to trigger via the public API — anthropic SDK only emits valid JSON.
// Skipped: the conversion path runs json.Unmarshal on raw bytes; if the API
// ever returned malformed JSON we'd fall through to the is_error path.
// Covered indirectly by inspection of the code path.)

// ---------------------------------------------------------------------------
// iteration cap — we error out instead of looping forever
// ---------------------------------------------------------------------------

func TestRun_ExceedsIterationCap(t *testing.T) {
	mc := defaultStubMCP()
	// Server always returns "tool_use" — the agent never finishes.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(makeMessage("tool_use", []map[string]any{
			toolUseBlock("toolu_x", "fs_read", map[string]any{"path": "README.md"}),
		})))
	}))
	defer srv.Close()

	opts := fixtureOpts(t, srv.URL, mc)
	opts.MaxIterations = 3

	_, err := Run(context.Background(), opts)
	if err == nil {
		t.Fatal("expected iteration-cap error")
	}
	if !strings.Contains(err.Error(), "exceeded max iterations") {
		t.Errorf("error should mention iteration cap, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// ListTools error from MCP — Run returns it
// ---------------------------------------------------------------------------

type errorListToolsMCP struct{ stubMCP }

func (e *errorListToolsMCP) ListTools(_ context.Context) ([]mcp.ToolDef, error) {
	return nil, errors.New("mcp catalog unavailable")
}

func TestRun_PropagatesListToolsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("anthropic should not be called when ListTools fails")
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	opts := fixtureOpts(t, srv.URL, &errorListToolsMCP{})
	_, err := Run(context.Background(), opts)
	if err == nil || !strings.Contains(err.Error(), "build tool list") {
		t.Errorf("expected build-tool-list error, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// empty allowlist is rejected
// ---------------------------------------------------------------------------

func TestRun_RejectsEmptyAllowlist(t *testing.T) {
	mc := &stubMCP{tools: []mcp.ToolDef{
		{Name: "git_branch", Description: "x", InputSchema: map[string]any{"type": "object"}},
	}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(makeMessage("end_turn", []map[string]any{textBlock("done")})))
	}))
	defer srv.Close()

	opts := fixtureOpts(t, srv.URL, mc)
	_, err := Run(context.Background(), opts)
	if err == nil || !strings.Contains(err.Error(), "no MCP tools matched") {
		t.Errorf("expected no-matching-tools error, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// system prompt is sent with cache_control and is the implementer prompt
// ---------------------------------------------------------------------------

func TestRun_SystemPromptIsCachedAndCorrect(t *testing.T) {
	mc := defaultStubMCP()
	var requestBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		requestBody = string(body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(makeMessage("end_turn", []map[string]any{textBlock("done")})))
	}))
	defer srv.Close()

	opts := fixtureOpts(t, srv.URL, mc)
	if _, err := Run(context.Background(), opts); err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(requestBody, `"cache_control":{"type":"ephemeral"}`) {
		t.Errorf("expected cache_control on system block: %s", requestBody)
	}
	if !strings.Contains(requestBody, "automated implementation agent") {
		t.Errorf("expected implementer system prompt: %s", requestBody[:min(400, len(requestBody))])
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// ---------------------------------------------------------------------------
// counter increments per ToolUseBlock processed
// ---------------------------------------------------------------------------

func TestRun_ToolCallCountIncludesAllInvocations(t *testing.T) {
	mc := defaultStubMCP()
	scripts := []string{
		// Two parallel tool uses in one turn
		makeMessage("tool_use", []map[string]any{
			toolUseBlock("toolu_1", "fs_read", map[string]any{"path": "a.md"}),
			toolUseBlock("toolu_2", "fs_read", map[string]any{"path": "b.md"}),
		}),
		makeMessage("end_turn", []map[string]any{textBlock("done")}),
	}
	url, _, cleanup := scriptedAnthropic(t, scripts)
	defer cleanup()

	res, err := Run(context.Background(), fixtureOpts(t, url, mc))
	if err != nil {
		t.Fatal(err)
	}
	if res.ToolCallCount != 2 {
		t.Errorf("expected ToolCallCount=2 (both parallel tools counted), got %d", res.ToolCallCount)
	}
	if mc.callCount() != 2 {
		t.Errorf("MCP saw %d calls, expected 2", mc.callCount())
	}
}

// ---------------------------------------------------------------------------
// guard: ensure we don't accidentally include git_* in the default allowlist
// ---------------------------------------------------------------------------

func TestDefaultAllowlistExcludesGitAndGH(t *testing.T) {
	for _, name := range defaultAllowedTools {
		if strings.HasPrefix(name, "git_") || strings.HasPrefix(name, "gh_") {
			t.Errorf("default allowlist must not include git/gh tools: found %q", name)
		}
	}
}

// (smoke) — confirm fmt is referenced so unused-import doesn't bite if a test
// is removed in a future edit.
var _ = fmt.Sprintf
