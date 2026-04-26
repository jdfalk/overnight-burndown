package digest

import (
	"strings"
	"testing"
	"time"

	"github.com/jdfalk/overnight-burndown/internal/agent"
	"github.com/jdfalk/overnight-burndown/internal/budget"
	"github.com/jdfalk/overnight-burndown/internal/dispatch"
	"github.com/jdfalk/overnight-burndown/internal/sources"
	"github.com/jdfalk/overnight-burndown/internal/state"
	"github.com/jdfalk/overnight-burndown/internal/triage"
)

// fixtureDate is a stable date so test output doesn't drift.
var fixtureDate = time.Date(2026, 4, 25, 23, 0, 0, 0, time.UTC)

func makeOutcome(branch, title string, status state.Status, cls triage.Classification) dispatch.Outcome {
	return dispatch.Outcome{
		Task: sources.Task{
			Source: state.Source{
				Type:  state.SourceIssue,
				URL:   "https://example/" + branch,
				Title: title,
			},
		},
		Decision: triage.Decision{
			Classification: cls,
			Reason:         "test reason",
		},
		Status: status,
		Branch: branch,
		AgentResult: &agent.Result{
			Summary:       "Did the thing\nwith multiple lines.",
			ToolCallCount: 3,
		},
	}
}

func fixtureStats() budget.Stats {
	return budget.Stats{
		DollarsSpent:  1.2345,
		DollarsCap:    5.0,
		Elapsed:       45 * time.Minute,
		WallCap:       2 * time.Hour,
		TokensInput:   12000,
		TokensOutput:  3000,
		TokensCached:  8000,
		TokensWritten: 1500,
		Threshold:     0.8,
	}
}

// ---------------------------------------------------------------------------
// TL;DR is always present
// ---------------------------------------------------------------------------

func TestRender_TLDRAlwaysPresent(t *testing.T) {
	got := Render(Input{RunDate: fixtureDate, Stats: fixtureStats()})
	if !strings.Contains(got, "## TL;DR") {
		t.Errorf("missing TL;DR section: %s", got)
	}
	if !strings.Contains(got, "0 shipped") {
		t.Errorf("expected zero-counts in TL;DR: %s", got)
	}
}

// ---------------------------------------------------------------------------
// Bucketing — outcomes land in the right sections
// ---------------------------------------------------------------------------

func TestRender_BucketsCorrectly(t *testing.T) {
	in := Input{
		RunDate: fixtureDate,
		Stats:   fixtureStats(),
		Outcomes: []dispatch.Outcome{
			makeOutcome("auto/typo", "fix typo", state.StatusInFlight, triage.ClassAutoMergeSafe),
			makeOutcome("draft/refactor", "big refactor", state.StatusInFlight, triage.ClassNeedsReview),
			makeOutcome("blocked/x", "ambiguous", state.StatusInFlight, triage.ClassBlocked),
			makeOutcome("auto/broken", "broken thing", state.StatusFailed, triage.ClassAutoMergeSafe),
		},
		PRs: map[string]PRInfo{
			"auto/typo":      {Number: 1, URL: "https://gh/1"},
			"draft/refactor": {Number: 2, URL: "https://gh/2"},
		},
		MergedBranches: map[string]bool{"auto/typo": true},
	}
	got := Render(in)

	// Shipped section should contain auto/typo, NOT draft/refactor.
	if !strings.Contains(got, "## Shipped (1)") {
		t.Errorf("expected Shipped section with count 1: %s", got)
	}
	// Draft section: draft/refactor.
	if !strings.Contains(got, "## Draft PRs awaiting review (1)") {
		t.Errorf("expected Draft section with count 1: %s", got)
	}
	// Blocked section.
	if !strings.Contains(got, "## Blocked (1)") {
		t.Errorf("expected Blocked section: %s", got)
	}
	// Failed section.
	if !strings.Contains(got, "## Failed (1)") {
		t.Errorf("expected Failed section: %s", got)
	}
	// TL;DR counts.
	if !strings.Contains(got, "**1 shipped**") {
		t.Errorf("TL;DR shipped count wrong: %s", got)
	}
}

// ---------------------------------------------------------------------------
// Empty sections are omitted (to keep digests scannable)
// ---------------------------------------------------------------------------

func TestRender_OmitsEmptySections(t *testing.T) {
	got := Render(Input{RunDate: fixtureDate, Stats: fixtureStats()})
	for _, section := range []string{
		"## Shipped",
		"## Draft PRs awaiting review",
		"## Blocked",
		"## Failed",
		"## Requeued for tomorrow",
		"## Policy violations",
	} {
		if strings.Contains(got, section) {
			t.Errorf("zero-count digest should not include %q section, got:\n%s", section, got)
		}
	}
}

// ---------------------------------------------------------------------------
// Determinism — same input → same bytes
// ---------------------------------------------------------------------------

func TestRender_Deterministic(t *testing.T) {
	in := Input{
		RunDate: fixtureDate,
		Stats:   fixtureStats(),
		Outcomes: []dispatch.Outcome{
			makeOutcome("zzz/last", "z", state.StatusInFlight, triage.ClassAutoMergeSafe),
			makeOutcome("aaa/first", "a", state.StatusInFlight, triage.ClassAutoMergeSafe),
			makeOutcome("mmm/middle", "m", state.StatusInFlight, triage.ClassAutoMergeSafe),
		},
		MergedBranches: map[string]bool{"aaa/first": true, "mmm/middle": true, "zzz/last": true},
	}
	a := Render(in)
	b := Render(in)
	if a != b {
		t.Fatalf("Render must be deterministic; diff:\n--- a ---\n%s\n--- b ---\n%s", a, b)
	}
	// Sort order: aaa < mmm < zzz.
	ai := strings.Index(a, "aaa/first")
	mi := strings.Index(a, "mmm/middle")
	zi := strings.Index(a, "zzz/last")
	if !(ai < mi && mi < zi) {
		t.Errorf("entries not sorted by branch in Shipped: aaa=%d mmm=%d zzz=%d", ai, mi, zi)
	}
}

// ---------------------------------------------------------------------------
// Multi-line summaries collapse to one line in list items
// ---------------------------------------------------------------------------

func TestRender_OneLineSummary(t *testing.T) {
	oc := makeOutcome("auto/x", "task", state.StatusInFlight, triage.ClassAutoMergeSafe)
	oc.AgentResult.Summary = "Line one.\n\n  Line two.\n\n\nLine three."

	in := Input{
		RunDate:        fixtureDate,
		Stats:          fixtureStats(),
		Outcomes:       []dispatch.Outcome{oc},
		PRs:            map[string]PRInfo{"auto/x": {Number: 1, URL: "https://gh/1"}},
		MergedBranches: map[string]bool{"auto/x": true},
	}
	got := Render(in)
	if strings.Contains(got, "Line one.\n  Line two.") {
		t.Errorf("multi-line summary not collapsed: %s", got)
	}
	if !strings.Contains(got, "Line one. Line two. Line three.") {
		t.Errorf("expected collapsed text: %s", got)
	}
}

// ---------------------------------------------------------------------------
// Requeued / Policy violations sections
// ---------------------------------------------------------------------------

func TestRender_RequeuedSection(t *testing.T) {
	in := Input{
		RunDate: fixtureDate,
		Stats:   fixtureStats(),
		Requeued: []dispatch.TaskWithDecision{
			{Task: sources.Task{Source: state.Source{URL: "u-b", Title: "task b"}}},
			{Task: sources.Task{Source: state.Source{URL: "u-a", Title: "task a"}}},
		},
	}
	got := Render(in)
	if !strings.Contains(got, "## Requeued for tomorrow (2)") {
		t.Errorf("expected requeued section count 2: %s", got)
	}
	// Sorted: u-a before u-b.
	ai := strings.Index(got, "u-a")
	bi := strings.Index(got, "u-b")
	if ai > bi {
		t.Errorf("requeued not sorted: u-a=%d u-b=%d", ai, bi)
	}
}

func TestRender_PolicyViolationsSection(t *testing.T) {
	in := Input{
		RunDate:          fixtureDate,
		Stats:            fixtureStats(),
		PolicyViolations: []string{"git push --force blocked", "rm -rf / blocked"},
	}
	got := Render(in)
	if !strings.Contains(got, "## Policy violations (2)") {
		t.Errorf("expected violations section: %s", got)
	}
	if !strings.Contains(got, "force blocked") {
		t.Errorf("violation text missing: %s", got)
	}
	// TL;DR carries the warning emoji.
	if !strings.Contains(got, "⚠️") {
		t.Errorf("TL;DR should warn about violations: %s", got)
	}
}

// ---------------------------------------------------------------------------
// Spend section formatting
// ---------------------------------------------------------------------------

func TestRender_SpendSection(t *testing.T) {
	got := Render(Input{RunDate: fixtureDate, Stats: fixtureStats()})
	for _, want := range []string{
		"## Spend",
		"$1.2345",
		"$5.00 cap",
		"45m of 2h cap",
		"12000 input / 3000 output / 8000 cached / 1500 cache-write",
		"Abort threshold: 80%",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing spend detail %q in:\n%s", want, got)
		}
	}
}

// ---------------------------------------------------------------------------
// formatDuration
// ---------------------------------------------------------------------------

func TestFormatDuration(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{0, "0s"},
		{500 * time.Millisecond, "1s"},
		{45 * time.Second, "45s"},
		{90 * time.Second, "1m30s"},
		{time.Hour + 23*time.Minute + 17*time.Second, "1h23m17s"},
		{2 * time.Hour, "2h"},
	}
	for _, tc := range cases {
		if got := formatDuration(tc.d); got != tc.want {
			t.Errorf("formatDuration(%v) = %q want %q", tc.d, got, tc.want)
		}
	}
}

// ---------------------------------------------------------------------------
// FilenameFor
// ---------------------------------------------------------------------------

func TestFilenameFor(t *testing.T) {
	got := FilenameFor(time.Date(2026, 4, 25, 23, 0, 0, 0, time.UTC))
	if got != "burndown-digest-2026-04-25.md" {
		t.Errorf("FilenameFor: got %q", got)
	}
}
