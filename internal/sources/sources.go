// Package sources collects candidate tasks for an overnight burndown run from
// three places: a repo's TODO.md, GitHub issues labeled `auto-ok`, and the
// markdown files under plans/. Each source is collected independently and
// then deduped by normalized title under the issue-wins precedence rule —
// when a TODO/plan entry matches an open issue, only the issue is kept.
package sources

import (
	"context"
	"regexp"
	"strings"

	"github.com/jdfalk/overnight-burndown/internal/state"
)

// Task is the unit handed off to the triage agent. The raw Body is what the
// agent reads; Source records where the task came from for state-tracking
// and digest reporting.
type Task struct {
	Source    state.Source
	Body      string
	HasAutoOK bool

	// TrackedBy is set when a non-issue task was merged into an issue during
	// dedup. Empty otherwise. Populated as e.g. "issue:42" so the digest can
	// surface the redirection clearly.
	TrackedBy string
}

// Collector is a single task source. The driver wires up one of each and
// runs them in sequence per repo.
type Collector interface {
	// Collect returns the tasks the source produces for a single repo. The
	// repo argument is owner/name (e.g. "jdfalk/audiobook-organizer") so
	// collectors can attribute their output uniformly.
	Collect(ctx context.Context, repo string, localPath string) ([]Task, error)
}

// CollectAll runs every collector in order, concatenates the results, and
// applies issue-wins dedup. Errors from any single collector are returned;
// CollectAll fails the whole pull rather than producing a partial queue —
// callers can decide whether to recover (e.g. log + skip a repo).
func CollectAll(ctx context.Context, repo string, localPath string, collectors ...Collector) ([]Task, error) {
	var all []Task
	for _, c := range collectors {
		got, err := c.Collect(ctx, repo, localPath)
		if err != nil {
			return nil, err
		}
		all = append(all, got...)
	}
	return Dedup(all), nil
}

// Dedup applies the issue-wins precedence rule:
//
//   - Issues are always kept.
//   - For each non-issue task, if any issue has the same NormalizeTitle, the
//     non-issue task is dropped and an annotation is recorded on the issue
//     so the digest can show "tracked by issue:N (originally TODO.md:line)".
//   - Non-issue tasks that don't collide with an issue are kept.
//
// Order is preserved within each bucket: issues first (in collection order),
// then non-issue tasks (in collection order).
func Dedup(in []Task) []Task {
	byTitle := make(map[string]int) // normalized title → index of issue in `out`
	var out []Task

	// Pass 1: keep every issue.
	for _, t := range in {
		if t.Source.Type == state.SourceIssue {
			out = append(out, t)
			byTitle[NormalizeTitle(t.Source.Title)] = len(out) - 1
		}
	}

	// Pass 2: keep non-issues unless they collide with an issue. Note any
	// collision on the surviving issue's TrackedBy field (additive).
	for _, t := range in {
		if t.Source.Type == state.SourceIssue {
			continue
		}
		key := NormalizeTitle(t.Source.Title)
		if idx, ok := byTitle[key]; ok {
			// Annotate the issue but don't repeat duplicate annotations.
			note := string(t.Source.Type) + ":" + t.Source.URL
			if !strings.Contains(out[idx].TrackedBy, note) {
				if out[idx].TrackedBy == "" {
					out[idx].TrackedBy = "duplicates: " + note
				} else {
					out[idx].TrackedBy += "; " + note
				}
			}
			continue
		}
		out = append(out, t)
	}

	return out
}

// NormalizeTitle lowercases, strips Markdown checkbox / auto-ok prefix, drops
// punctuation, and collapses whitespace. Two titles that differ only in
// case, punctuation, or formatting will compare equal.
//
// Strict equality on the normalized form is enough for v1. Fuzzy matching
// (Levenshtein, embedding cosine, etc.) is deferred until we observe real
// near-duplicates that escape this normalization.
func NormalizeTitle(s string) string {
	s = strings.ToLower(s)
	s = stripChecklistPrefix(s)
	s = stripAutoOK(s)
	s = punct.ReplaceAllString(s, " ")
	s = ws.ReplaceAllString(s, " ")
	return strings.TrimSpace(s)
}

// stripChecklistPrefix removes the "- [ ]" / "- [x]" / "* [ ]" / "1. [ ]"
// pattern that markdown-style task lists begin with.
func stripChecklistPrefix(s string) string {
	return checklistPrefix.ReplaceAllString(s, "")
}

// stripAutoOK removes a leading "[auto-ok]" marker from a title (case
// insensitive). The marker still ends up tracked on the Task via HasAutoOK;
// stripping it from the title is purely so dedup matches an issue version
// of the same task without the marker.
func stripAutoOK(s string) string {
	return autoOKMarker.ReplaceAllString(s, "")
}

var (
	checklistPrefix = regexp.MustCompile(`(?i)^\s*(?:[-*+]|\d+\.)\s*\[\s*[xX ]?\s*\]\s*`)
	autoOKMarker    = regexp.MustCompile(`(?i)^\s*\[\s*auto-ok\s*\]\s*`)
	holdMarker      = regexp.MustCompile(`(?i)\[\s*hold\s*\]`)
	punct           = regexp.MustCompile(`[^\w\s]+`)
	ws              = regexp.MustCompile(`\s+`)
)

// HasAutoOKMarker reports whether `[auto-ok]` appears as a leading marker on
// the line. Used by the TODO and plan collectors to populate Task.HasAutoOK.
func HasAutoOKMarker(line string) bool {
	return autoOKMarker.MatchString(strings.TrimSpace(line))
}

// HasHoldMarker reports whether `[hold]` appears anywhere in the line. Items
// tagged this way are excluded from collection so the burndown bot won't pick
// them up. Use it for spec-pending or under-review items where an unchecked
// `[ ]` is appropriate but auto-execution is not.
func HasHoldMarker(line string) bool {
	return holdMarker.MatchString(line)
}
