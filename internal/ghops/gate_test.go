package ghops

import (
	"strings"
	"testing"

	"github.com/jdfalk/overnight-burndown/internal/triage"
)

// happyInputs returns a GateInputs that should pass every gate. Each
// test mutates one field to trigger one specific rejection.
func happyInputs() GateInputs {
	return GateInputs{
		Classification:    triage.ClassAutoMergeSafe,
		HasAutoOK:         true,
		CIStatus:          CISuccess,
		ChangedFiles:      []ChangedFile{{Path: "README.md", Additions: 1, Deletions: 1}},
		AutoMergePaths:    []string{"*.md", "tests/**"},
		ForcedReviewPaths: []string{".github/workflows/**", "**/migrations/**"},
		DiffSizeCapLines:  200,
	}
}

// ---------------------------------------------------------------------------
// Happy path — all gates pass
// ---------------------------------------------------------------------------

func TestEvaluateGate_AllowsHappyPath(t *testing.T) {
	d := EvaluateGate(happyInputs())
	if !d.Allow {
		t.Errorf("expected Allow=true, got reasons: %v", d.Reasons)
	}
}

// ---------------------------------------------------------------------------
// Gate 1: classification must be AUTO_MERGE_SAFE
// ---------------------------------------------------------------------------

func TestEvaluateGate_BlocksOnNeedsReview(t *testing.T) {
	in := happyInputs()
	in.Classification = triage.ClassNeedsReview
	d := EvaluateGate(in)
	if d.Allow {
		t.Fatal("expected Allow=false")
	}
	if !containsSub(d.Reasons, "NEEDS_REVIEW") {
		t.Errorf("missing classification reason: %v", d.Reasons)
	}
}

func TestEvaluateGate_BlocksOnBlocked(t *testing.T) {
	in := happyInputs()
	in.Classification = triage.ClassBlocked
	d := EvaluateGate(in)
	if d.Allow || !containsSub(d.Reasons, "BLOCKED") {
		t.Errorf("expected blocked rejection, got: %+v", d)
	}
}

// ---------------------------------------------------------------------------
// Gate 2: auto-ok marker must be present
// ---------------------------------------------------------------------------

func TestEvaluateGate_BlocksWithoutAutoOK(t *testing.T) {
	in := happyInputs()
	in.HasAutoOK = false
	d := EvaluateGate(in)
	if d.Allow || !containsSub(d.Reasons, "auto-ok marker") {
		t.Errorf("expected auto-ok rejection, got: %+v", d)
	}
}

// ---------------------------------------------------------------------------
// Gate 3: every file must be in the allowlist
// ---------------------------------------------------------------------------

func TestEvaluateGate_BlocksFileOutsideAllowlist(t *testing.T) {
	in := happyInputs()
	in.ChangedFiles = []ChangedFile{
		{Path: "README.md", Additions: 1},   // allowed
		{Path: "main.go", Additions: 5},     // NOT allowed
	}
	d := EvaluateGate(in)
	if d.Allow {
		t.Fatal("expected Allow=false")
	}
	if !containsSub(d.Reasons, "main.go") {
		t.Errorf("rejection should call out the bad path: %v", d.Reasons)
	}
}

func TestEvaluateGate_BlocksWhenAllowlistEmpty(t *testing.T) {
	in := happyInputs()
	in.AutoMergePaths = nil
	d := EvaluateGate(in)
	if d.Allow || !containsSub(d.Reasons, "no auto_merge_paths") {
		t.Errorf("expected empty-allowlist rejection: %v", d)
	}
}

// ---------------------------------------------------------------------------
// Gate 4: CI must be green
// ---------------------------------------------------------------------------

func TestEvaluateGate_BlocksOnCIFailure(t *testing.T) {
	in := happyInputs()
	in.CIStatus = CIFailure
	d := EvaluateGate(in)
	if d.Allow || !containsSub(d.Reasons, "failure") {
		t.Errorf("expected CI-failure rejection: %v", d)
	}
}

func TestEvaluateGate_BlocksOnCIPending(t *testing.T) {
	in := happyInputs()
	in.CIStatus = CIPending
	d := EvaluateGate(in)
	if d.Allow || !containsSub(d.Reasons, "pending") {
		t.Errorf("expected CI-pending rejection: %v", d)
	}
}

// ---------------------------------------------------------------------------
// Hard veto: forced-review paths
// ---------------------------------------------------------------------------

func TestEvaluateGate_HardVetoOnWorkflowChange(t *testing.T) {
	in := happyInputs()
	in.ChangedFiles = []ChangedFile{
		{Path: ".github/workflows/ci.yml", Additions: 2},
	}
	// The path is NOT in auto_merge_paths either, so we'd reject anyway.
	// Add it to verify the forced-review reason still fires.
	in.AutoMergePaths = []string{".github/workflows/**", "*.md"}
	d := EvaluateGate(in)
	if d.Allow {
		t.Fatal("expected Allow=false")
	}
	if !containsSub(d.Reasons, "forced-review") {
		t.Errorf("expected forced-review veto: %v", d.Reasons)
	}
}

func TestEvaluateGate_HardVetoOnMigration(t *testing.T) {
	in := happyInputs()
	in.ChangedFiles = []ChangedFile{
		{Path: "internal/store/migrations/0042_add_index.sql", Additions: 5},
	}
	in.AutoMergePaths = []string{"**/migrations/**", "*.md"}
	d := EvaluateGate(in)
	if d.Allow || !containsSub(d.Reasons, "forced-review") {
		t.Errorf("expected forced-review veto on migration: %v", d)
	}
}

// ---------------------------------------------------------------------------
// Hard veto: diff size cap
// ---------------------------------------------------------------------------

func TestEvaluateGate_BlocksOnLargeDiff(t *testing.T) {
	in := happyInputs()
	in.ChangedFiles = []ChangedFile{
		{Path: "README.md", Additions: 150, Deletions: 100}, // 250 > 200
	}
	d := EvaluateGate(in)
	if d.Allow {
		t.Fatal("expected Allow=false")
	}
	if !containsSub(d.Reasons, "diff size") {
		t.Errorf("expected diff-size rejection: %v", d.Reasons)
	}
}

func TestEvaluateGate_DiffSizeCapZeroDisablesCheck(t *testing.T) {
	in := happyInputs()
	in.DiffSizeCapLines = 0
	in.ChangedFiles = []ChangedFile{{Path: "README.md", Additions: 9999}}
	d := EvaluateGate(in)
	if !d.Allow {
		t.Errorf("cap=0 should disable the check, got reasons: %v", d.Reasons)
	}
}

// ---------------------------------------------------------------------------
// Multiple-failure aggregation: gate reports every rejection, not just the first
// ---------------------------------------------------------------------------

func TestEvaluateGate_AggregatesAllReasons(t *testing.T) {
	in := happyInputs()
	in.Classification = triage.ClassNeedsReview
	in.HasAutoOK = false
	in.CIStatus = CIFailure
	d := EvaluateGate(in)
	if d.Allow {
		t.Fatal("expected Allow=false")
	}
	if len(d.Reasons) < 3 {
		t.Errorf("expected ≥3 reasons (classification, auto-ok, CI), got %d: %v", len(d.Reasons), d.Reasons)
	}
}

// ---------------------------------------------------------------------------
// matchOne pattern matching
// ---------------------------------------------------------------------------

func TestMatchOne(t *testing.T) {
	cases := []struct {
		path    string
		pattern string
		want    bool
	}{
		// simple globs
		{"README.md", "*.md", true},
		{"README.md", "*.go", false},
		{"docs/foo.md", "*.md", false}, // path.Match doesn't cross /
		// recursive **
		{"tests/foo.go", "tests/**", true},
		{"tests/a/b/c.go", "tests/**", true},
		{"tests", "tests/**", true},
		{"src/main.go", "tests/**", false},
		// `**/X/**` — X anywhere as a complete segment
		{"internal/store/migrations/0042.sql", "**/migrations/**", true},
		{"migrations/0042.sql", "**/migrations/**", true},   // top-level allowed (zero parent segments)
		{"a/migrationsplus/x.sql", "**/migrations/**", false}, // partial-segment match must not count
		{".github/workflows/ci.yml", ".github/workflows/**", true},
		// `**/X` — X at any depth
		{"a/b/c.md", "**/c.md", true},
		{"c.md", "**/c.md", true},
		// `**` alone matches everything
		{"anything/here.md", "**", true},
		// empty pattern matches nothing
		{"x", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.path+"|"+tc.pattern, func(t *testing.T) {
			got := matchOne(tc.path, tc.pattern)
			if got != tc.want {
				t.Errorf("matchOne(%q, %q) = %v, want %v", tc.path, tc.pattern, got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func containsSub(strs []string, sub string) bool {
	for _, s := range strs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}
