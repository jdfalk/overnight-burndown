// Package digest renders the morning markdown summary of an overnight
// burndown run.
//
// PLAN.md design D2 sections (in order):
//   1. TL;DR — one-line counts + spend.
//   2. Shipped — PRs that auto-merged.
//   3. Draft PRs awaiting review — opened but not auto-merged.
//   4. Blocked — tasks the triage agent flagged as un-actionable.
//   5. Failed — tasks that hit a runtime error (worktree, agent, push,
//      CI, gates).
//   6. Requeued for tomorrow — items that didn't run because budget
//      exceeded.
//   7. Policy violations — anything safe-ai-util blocked mid-run
//      (audit-log digest).
//   8. Spend — token + dollar breakdown.
//
// Sections with zero entries are omitted to keep the digest scannable.
// The TL;DR is always present.
package digest

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jdfalk/overnight-burndown/internal/budget"
	"github.com/jdfalk/overnight-burndown/internal/dispatch"
	"github.com/jdfalk/overnight-burndown/internal/state"
	"github.com/jdfalk/overnight-burndown/internal/triage"
)

// Input is everything the digest renderer needs. The orchestrator
// constructs one of these at end-of-run.
type Input struct {
	// RunDate is the calendar day the run was scheduled for. Used for
	// the digest title and filename.
	RunDate time.Time

	// Outcomes is one entry per task the dispatcher attempted. Includes
	// both successes and failures.
	Outcomes []dispatch.Outcome

	// PRs maps Outcome.Branch → opened PR number + URL. Empty when no
	// PR was opened (e.g. agent produced no diff, or task failed pre-PR).
	PRs map[string]PRInfo

	// MergedBranches lists branches that auto-merged. Subset of PRs.
	MergedBranches map[string]bool

	// Requeued lists hashes of tasks that didn't run because the budget
	// abort fired. The orchestrator pulls these from the state queue.
	Requeued []dispatch.TaskWithDecision

	// PolicyViolations is the list of safe-ai-util audit entries where
	// the executor refused to run a command. Free-form strings — the
	// digest just lists them.
	PolicyViolations []string

	// Stats is the budget snapshot at end-of-run.
	Stats budget.Stats
}

// PRInfo is the per-task PR record the orchestrator passes through.
type PRInfo struct {
	Number int
	URL    string
}

// Render produces the morning digest as Markdown. Output is
// deterministic — outcomes are sorted by branch so re-running the
// renderer over the same input always yields the same bytes.
func Render(in Input) string {
	var b strings.Builder

	fmt.Fprintf(&b, "# Burndown digest — %s\n\n", in.RunDate.Format("2006-01-02"))

	writeTLDR(&b, in)

	shipped, drafts, noChange, blocked, failed := bucketize(in.Outcomes, in.MergedBranches)

	writeSection(&b, "Shipped", shipped, in.PRs, formatShipped)
	writeSection(&b, "Draft PRs awaiting review", drafts, in.PRs, formatDraft)
	writeSection(&b, "Agent finished but produced no diff", noChange, in.PRs, formatNoChange)
	writeSection(&b, "Blocked", blocked, in.PRs, formatBlocked)
	writeSection(&b, "Failed", failed, in.PRs, formatFailed)

	if len(in.Requeued) > 0 {
		fmt.Fprintf(&b, "## Requeued for tomorrow (%d)\n\n", len(in.Requeued))
		for _, t := range sortedRequeued(in.Requeued) {
			fmt.Fprintf(&b, "- `%s` — %s\n", t.Task.Source.URL, t.Task.Source.Title)
		}
		b.WriteString("\n")
	}

	if len(in.PolicyViolations) > 0 {
		fmt.Fprintf(&b, "## Policy violations (%d)\n\n", len(in.PolicyViolations))
		for _, v := range in.PolicyViolations {
			fmt.Fprintf(&b, "- %s\n", v)
		}
		b.WriteString("\n")
	}

	writeSpend(&b, in.Stats)

	return b.String()
}

// writeTLDR writes the always-present one-block summary at the top.
func writeTLDR(b *strings.Builder, in Input) {
	shipped, drafts, noChange, blocked, failed := bucketize(in.Outcomes, in.MergedBranches)
	dollarPct := in.Stats.FractionSpent() * 100
	wallPct := in.Stats.FractionElapsed() * 100

	b.WriteString("## TL;DR\n\n")
	fmt.Fprintf(b, "- **%d shipped** · %d draft · %d no-diff · %d blocked · %d failed · %d requeued\n",
		len(shipped), len(drafts), len(noChange), len(blocked), len(failed), len(in.Requeued))
	fmt.Fprintf(b, "- Spend: $%.2f of $%.2f cap (%.0f%%)\n",
		in.Stats.DollarsSpent, in.Stats.DollarsCap, dollarPct)
	fmt.Fprintf(b, "- Wall-clock: %s of %s cap (%.0f%%)\n",
		formatDuration(in.Stats.Elapsed),
		formatDuration(in.Stats.WallCap),
		wallPct)
	if len(in.PolicyViolations) > 0 {
		fmt.Fprintf(b, "- ⚠️ %d policy violation(s) — see section below\n", len(in.PolicyViolations))
	}
	b.WriteString("\n")
}

// writeSection renders a list section; skipped entirely when empty so
// the digest stays compact.
func writeSection(b *strings.Builder, title string, items []dispatch.Outcome,
	prs map[string]PRInfo, format func(*strings.Builder, dispatch.Outcome, PRInfo)) {
	if len(items) == 0 {
		return
	}
	fmt.Fprintf(b, "## %s (%d)\n\n", title, len(items))
	for _, oc := range items {
		format(b, oc, prs[oc.Branch])
	}
	b.WriteString("\n")
}

func formatShipped(b *strings.Builder, oc dispatch.Outcome, pr PRInfo) {
	fmt.Fprintf(b, "- [#%d](%s) `%s` — %s\n", pr.Number, pr.URL, oc.Branch, oc.Task.Source.Title)
	if oc.AgentResult != nil && oc.AgentResult.Summary != "" {
		fmt.Fprintf(b, "  - %s\n", oneLine(oc.AgentResult.Summary))
	}
	writeUsage(b, oc)
}

func formatDraft(b *strings.Builder, oc dispatch.Outcome, pr PRInfo) {
	prRef := "(no PR)"
	if pr.Number > 0 {
		prRef = fmt.Sprintf("[#%d](%s)", pr.Number, pr.URL)
	}
	fmt.Fprintf(b, "- %s `%s` — %s\n", prRef, oc.Branch, oc.Task.Source.Title)
	fmt.Fprintf(b, "  - **Why draft:** %s\n", oc.Decision.Reason)
	if oc.AgentResult != nil && oc.AgentResult.Summary != "" {
		fmt.Fprintf(b, "  - **Summary:** %s\n", oneLine(oc.AgentResult.Summary))
	}
	writeUsage(b, oc)
}

// writeUsage appends a token-usage line to the entry when the outcome
// includes an AgentResult with non-zero usage. Skipped when zero so cells
// from providers that don't expose usage don't show a noisy "0 / 0" line.
func writeUsage(b *strings.Builder, oc dispatch.Outcome) {
	if oc.AgentResult == nil {
		return
	}
	u := oc.AgentResult.Usage
	if u.TotalTokens == 0 && u.PromptTokens == 0 && u.CompletionTokens == 0 {
		return
	}
	fmt.Fprintf(b, "  - **Tokens:** %d prompt / %d completion / %d cached / %d total (iter=%d, tools=%d)\n",
		u.PromptTokens, u.CompletionTokens, u.CachedTokens, u.TotalTokens,
		oc.AgentResult.Iterations, oc.AgentResult.ToolCallCount)
}

func formatNoChange(b *strings.Builder, oc dispatch.Outcome, _ PRInfo) {
	fmt.Fprintf(b, "- `%s` — %s\n", oc.Task.Source.URL, oc.Task.Source.Title)
	if oc.AgentResult != nil && oc.AgentResult.Summary != "" {
		fmt.Fprintf(b, "  - **Agent said:** %s\n", oneLine(oc.AgentResult.Summary))
	}
	writeUsage(b, oc)
}

func formatBlocked(b *strings.Builder, oc dispatch.Outcome, _ PRInfo) {
	fmt.Fprintf(b, "- `%s` — %s\n", oc.Task.Source.URL, oc.Task.Source.Title)
	fmt.Fprintf(b, "  - **Reason:** %s\n", oc.Decision.Reason)
}

func formatFailed(b *strings.Builder, oc dispatch.Outcome, pr PRInfo) {
	prRef := ""
	if pr.Number > 0 {
		prRef = fmt.Sprintf(" [#%d](%s)", pr.Number, pr.URL)
	}
	fmt.Fprintf(b, "- `%s`%s — %s\n", oc.Branch, prRef, oc.Task.Source.Title)
	if oc.Error != "" {
		fmt.Fprintf(b, "  - **Error:** %s\n", oneLine(oc.Error))
	}
	if oc.WorktreePath != "" {
		fmt.Fprintf(b, "  - Worktree retained: `%s`\n", oc.WorktreePath)
	}
	writeUsage(b, oc)
}

// writeSpend renders the cost breakdown at the bottom.
func writeSpend(b *strings.Builder, s budget.Stats) {
	b.WriteString("## Spend\n\n")
	fmt.Fprintf(b, "- Dollars: **$%.4f** of $%.2f cap\n", s.DollarsSpent, s.DollarsCap)
	fmt.Fprintf(b, "- Wall-clock: %s of %s cap\n",
		formatDuration(s.Elapsed), formatDuration(s.WallCap))
	fmt.Fprintf(b, "- Tokens: %d input / %d output / %d cached / %d cache-write\n",
		s.TokensInput, s.TokensOutput, s.TokensCached, s.TokensWritten)
	if s.Threshold > 0 {
		fmt.Fprintf(b, "- Abort threshold: %.0f%%\n", s.Threshold*100)
	}
	b.WriteString("\n")
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// bucketize partitions outcomes into the four high-level buckets the
// digest cares about. A task is:
//
//   * shipped if its branch is in mergedBranches.
//   * blocked if its triage classification is BLOCKED.
//   * failed if Outcome.Status == StatusFailed.
//   * draft otherwise (in-flight that didn't merge).
//
// Each bucket is sorted by branch for deterministic output.
func bucketize(items []dispatch.Outcome, merged map[string]bool) (shipped, drafts, noChange, blocked, failed []dispatch.Outcome) {
	for _, oc := range items {
		switch {
		case oc.Status == state.StatusFailed:
			failed = append(failed, oc)
		case oc.Status == state.StatusNoChange:
			noChange = append(noChange, oc)
		case oc.Decision.Classification == triage.ClassBlocked:
			blocked = append(blocked, oc)
		case merged[oc.Branch]:
			shipped = append(shipped, oc)
		default:
			drafts = append(drafts, oc)
		}
	}
	sort.Slice(shipped, func(i, j int) bool { return shipped[i].Branch < shipped[j].Branch })
	sort.Slice(drafts, func(i, j int) bool { return drafts[i].Branch < drafts[j].Branch })
	sort.Slice(noChange, func(i, j int) bool { return noChange[i].Task.Source.URL < noChange[j].Task.Source.URL })
	sort.Slice(blocked, func(i, j int) bool { return blocked[i].Task.Source.URL < blocked[j].Task.Source.URL })
	sort.Slice(failed, func(i, j int) bool { return failed[i].Branch < failed[j].Branch })
	return
}

// sortedRequeued returns a stable ordering of requeued tasks for the
// digest. Sort by source URL so the same input always produces the same
// digest bytes.
func sortedRequeued(items []dispatch.TaskWithDecision) []dispatch.TaskWithDecision {
	out := make([]dispatch.TaskWithDecision, len(items))
	copy(out, items)
	sort.Slice(out, func(i, j int) bool { return out[i].Task.Source.URL < out[j].Task.Source.URL })
	return out
}

// oneLine collapses multi-line text to a single line for compact list
// rendering. Newlines become spaces; runs of whitespace collapse.
func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\n", " ")
	for strings.Contains(s, "  ") {
		s = strings.ReplaceAll(s, "  ", " ")
	}
	return strings.TrimSpace(s)
}

// formatDuration renders a Duration in human-readable form (e.g.
// "1h23m" or "45s"). Distinct from time.Duration.String() in two ways:
//   - drops zero-valued components (no "0h45m17s").
//   - rounds to the second so digests don't show nanos.
func formatDuration(d time.Duration) string {
	if d <= 0 {
		return "0s"
	}
	d = d.Round(time.Second)
	h := d / time.Hour
	d -= h * time.Hour
	m := d / time.Minute
	d -= m * time.Minute
	s := d / time.Second

	var out strings.Builder
	if h > 0 {
		fmt.Fprintf(&out, "%dh", h)
	}
	if m > 0 {
		fmt.Fprintf(&out, "%dm", m)
	}
	if s > 0 || out.Len() == 0 {
		fmt.Fprintf(&out, "%ds", s)
	}
	return out.String()
}

// FilenameFor returns the canonical digest filename for a given run
// date. The orchestrator writes the rendered digest to
// `<digest_dir>/burndown-digest-<filename>`.
func FilenameFor(t time.Time) string {
	return "burndown-digest-" + t.Format("2006-01-02") + ".md"
}
