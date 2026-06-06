// file: internal/decompose/decompose.go
// version: 1.2.0
// guid: d3c0mp05-e000-4000-a000-f41l3db47ch0
//
// Decompose reads [failed-batch-hard]-tagged items from TODO.md and asks
// Claude to split each one into 3-5 concrete subtasks that fit within a
// single agent session. The subtasks are written back to TODO.md immediately
// after the parent task; the caller is responsible for committing the result.
//
// Model choice: claude-sonnet-4-6 — decomposition is the highest-leverage
// decision in the pipeline; a poorly split task fails again. Sonnet has
// the reasoning depth to produce genuinely well-scoped subtasks.

package decompose

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/falkcorp/overnight-burndown/internal/sources"
)

const decomposeModel = "claude-sonnet-4-6"

// Subtask is the structured output Claude returns for each parent task.
type Subtask struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Body  string `json:"body"`
	Size  string `json:"size"` // S | M | L
}

type decomposeResponse struct {
	Subtasks []Subtask `json:"subtasks"`
}

// Result reports what happened for one parent task.
type Result struct {
	ParentTitle string
	Subtasks    []Subtask
	Err         error
}

// Run scans TODO.md at repoPath for [failed-batch-hard] items, calls Claude
// to decompose each one, then rewrites TODO.md with the subtasks inserted
// after their parent. Returns one Result per parent task processed.
func Run(ctx context.Context, client anthropic.Client, repoPath string) ([]Result, error) {
	todoPath := filepath.Join(repoPath, "TODO.md")
	data, err := os.ReadFile(todoPath)
	if err != nil {
		return nil, fmt.Errorf("decompose: read TODO.md: %w", err)
	}

	lines := strings.Split(string(data), "\n")
	parents := collectHardFailed(lines)
	if len(parents) == 0 {
		return nil, nil
	}

	var results []Result
	for _, p := range parents {
		subtasks, cerr := callClaude(ctx, client, p.text)
		res := Result{ParentTitle: p.title, Subtasks: subtasks, Err: cerr}
		results = append(results, res)
		if cerr != nil {
			fmt.Fprintf(os.Stderr, "decompose: claude error for %q: %v\n", p.title, cerr)
			continue
		}
		lines = insertSubtasks(lines, p.lineIdx, p.title, subtasks)
	}

	out := strings.Join(lines, "\n")
	if err := os.WriteFile(todoPath, []byte(out), 0o644); err != nil {
		return results, fmt.Errorf("decompose: write TODO.md: %w", err)
	}
	return results, nil
}

// parent holds one [failed-batch-hard] item found in TODO.md.
type parent struct {
	lineIdx int
	title   string
	text    string // full multi-line task body
}

func collectHardFailed(lines []string) []parent {
	var out []parent
	for i, line := range lines {
		if !sources.IsUncheckedItem(line) {
			continue
		}
		if !sources.HasFailedBatchHardMarker(line) {
			continue
		}
		// Collect continuation lines.
		var sb strings.Builder
		sb.WriteString(line)
		for j := i + 1; j < len(lines); j++ {
			l := lines[j]
			if strings.HasPrefix(l, "  ") || strings.HasPrefix(l, "\t") {
				sb.WriteString("\n")
				sb.WriteString(l)
			} else {
				break
			}
		}
		title := extractTitle(line)
		out = append(out, parent{lineIdx: i, title: title, text: sb.String()})
	}
	return out
}

func extractTitle(line string) string {
	// Strip checklist prefix then take up to 80 chars.
	s := strings.TrimSpace(line)
	// Remove "- [ ] " prefix variants.
	for _, pfx := range []string{"- [ ] ", "* [ ] ", "+ [ ] "} {
		if strings.HasPrefix(s, pfx) {
			s = s[len(pfx):]
			break
		}
	}
	if len(s) > 80 {
		s = s[:80]
	}
	return strings.TrimSpace(s)
}

const decomposePrompt = `You are decomposing a software engineering task that has failed automated execution twice due to being too large for a single agent session (context overflow).

Break it into 3-5 concrete subtasks where each subtask:
- Can be completed by an automated coding agent in one session (under ~40 tool calls)
- References specific file paths, function names, or interfaces where relevant
- Has clear, testable acceptance criteria
- Builds logically on prior subtasks if sequential

Return ONLY valid JSON matching this schema (no markdown, no explanation):
{"subtasks":[{"id":"<parent_id>.1","title":"<short title>","body":"<full task description with file paths and acceptance criteria>","size":"S|M|L"}]}

The parent task:`

func callClaude(ctx context.Context, client anthropic.Client, taskText string) ([]Subtask, error) {
	msg, err := client.Messages.New(ctx, anthropic.MessageNewParams{
		Model:     anthropic.Model(decomposeModel),
		MaxTokens: 2048,
		Messages: []anthropic.MessageParam{
			anthropic.NewUserMessage(anthropic.NewTextBlock(decomposePrompt + "\n\n" + taskText)),
		},
	})
	if err != nil {
		return nil, fmt.Errorf("anthropic: %w", err)
	}
	if len(msg.Content) == 0 {
		return nil, fmt.Errorf("anthropic: empty response")
	}

	raw := msg.Content[0].Text
	// Strip markdown fences if Claude added them despite instructions.
	raw = strings.TrimSpace(raw)
	if strings.HasPrefix(raw, "```") {
		scanner := bufio.NewScanner(strings.NewReader(raw))
		var sb strings.Builder
		inFence := false
		for scanner.Scan() {
			line := scanner.Text()
			if strings.HasPrefix(line, "```") {
				inFence = !inFence
				continue
			}
			if inFence {
				sb.WriteString(line)
				sb.WriteByte('\n')
			}
		}
		raw = strings.TrimSpace(sb.String())
	}

	var resp decomposeResponse
	if err := json.Unmarshal([]byte(raw), &resp); err != nil {
		return nil, fmt.Errorf("parse claude response: %w\nraw: %s", err, raw)
	}
	return resp.Subtasks, nil
}

// insertSubtasks writes the generated subtasks into lines immediately after
// the parent task block (after all its continuation lines). The parent's
// [failed-batch-hard] marker is replaced with [decomposed] so it stays held
// but signals that subtasks exist.
func insertSubtasks(lines []string, parentIdx int, parentTitle string, subtasks []Subtask) []string {
	// Upgrade parent marker: [failed-batch-hard] → [decomposed]
	lines[parentIdx] = sources.RemoveFailedBatchMarkers(lines[parentIdx])
	lines[parentIdx] = strings.Replace(lines[parentIdx], "[hold]", "[hold][decomposed]", 1)

	// Find end of parent's continuation block.
	insertAt := parentIdx + 1
	for insertAt < len(lines) {
		l := lines[insertAt]
		if strings.HasPrefix(l, "  ") || strings.HasPrefix(l, "\t") {
			insertAt++
		} else {
			break
		}
	}

	var newLines []string
	newLines = append(newLines, fmt.Sprintf("  <!-- decomposed from: %s -->", parentTitle))
	for _, st := range subtasks {
		sz := st.Size
		if sz == "" {
			sz = "M"
		}
		newLines = append(newLines, fmt.Sprintf("- [ ] **%s** %s (**%s**)", st.ID, st.Title, sz))
		if st.Body != "" {
			for _, bodyLine := range strings.Split(st.Body, "\n") {
				if strings.TrimSpace(bodyLine) != "" {
					newLines = append(newLines, "  "+bodyLine)
				}
			}
		}
	}
	newLines = append(newLines, "")

	// Splice into lines.
	result := make([]string, 0, len(lines)+len(newLines))
	result = append(result, lines[:insertAt]...)
	result = append(result, newLines...)
	result = append(result, lines[insertAt:]...)
	return result
}
