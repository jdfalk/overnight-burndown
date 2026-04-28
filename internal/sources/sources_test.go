package sources

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-github/v84/github"

	"github.com/jdfalk/overnight-burndown/internal/state"
)

// ---------------------------------------------------------------------------
// NormalizeTitle
// ---------------------------------------------------------------------------

func TestNormalizeTitle(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"Fix typo in README", "fix typo in readme"},
		{"FIX typo IN README!", "fix typo in readme"},
		{"- [ ] [auto-ok] Fix typo in README", "fix typo in readme"},
		{"  - [x] Fix typo  in   README  ", "fix typo in readme"},
		{"[auto-ok]   Fix    typo!", "fix typo"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			if got := NormalizeTitle(tc.in); got != tc.want {
				t.Errorf("NormalizeTitle(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestHasAutoOKMarker(t *testing.T) {
	yes := []string{
		"[auto-ok] do the thing",
		" [Auto-OK] do the thing",
		"[ auto-ok ] do the thing",
	}
	no := []string{
		"do the thing",
		"do the [auto-ok] thing", // marker not at start
	}
	for _, s := range yes {
		if !HasAutoOKMarker(s) {
			t.Errorf("expected auto-ok detected on %q", s)
		}
	}
	for _, s := range no {
		if HasAutoOKMarker(s) {
			t.Errorf("expected auto-ok NOT detected on %q", s)
		}
	}
}

// ---------------------------------------------------------------------------
// Dedup — issue-wins precedence
// ---------------------------------------------------------------------------

func TestDedup_IssueWinsOverTODO(t *testing.T) {
	in := []Task{
		{
			Source: state.Source{Type: state.SourceTODO, URL: "TODO.md#L1", Title: "Fix typo in README"},
		},
		{
			Source: state.Source{Type: state.SourceIssue, URL: "https://github.com/x/y/issues/42", Title: "FIX TYPO IN README"},
		},
	}
	out := Dedup(in)
	if len(out) != 1 {
		t.Fatalf("expected 1 (issue wins), got %d: %+v", len(out), out)
	}
	if out[0].Source.Type != state.SourceIssue {
		t.Errorf("survivor must be the issue, got %q", out[0].Source.Type)
	}
	if !strings.Contains(out[0].TrackedBy, "TODO.md#L1") {
		t.Errorf("issue should record the TODO it absorbed: %q", out[0].TrackedBy)
	}
}

func TestDedup_NonOverlappingTasksKeptSeparately(t *testing.T) {
	in := []Task{
		{Source: state.Source{Type: state.SourceTODO, URL: "TODO.md#L1", Title: "task a"}},
		{Source: state.Source{Type: state.SourceIssue, URL: "https://example/3", Title: "task b"}},
		{Source: state.Source{Type: state.SourcePlan, URL: "plans/c.md", Title: "task c"}},
	}
	out := Dedup(in)
	if len(out) != 3 {
		t.Errorf("expected 3 distinct, got %d", len(out))
	}
}

func TestDedup_MultiSourceTrackingIsAdditive(t *testing.T) {
	in := []Task{
		{Source: state.Source{Type: state.SourceIssue, URL: "https://example/3", Title: "ship feature x"}},
		{Source: state.Source{Type: state.SourceTODO, URL: "TODO.md#L1", Title: "ship feature x"}},
		{Source: state.Source{Type: state.SourcePlan, URL: "plans/feature-x.md", Title: "ship feature x"}},
	}
	out := Dedup(in)
	if len(out) != 1 {
		t.Fatalf("expected 1 issue absorbing both TODO + plan, got %d", len(out))
	}
	if !strings.Contains(out[0].TrackedBy, "todo:") || !strings.Contains(out[0].TrackedBy, "plan:") {
		t.Errorf("TrackedBy should mention both sources: %q", out[0].TrackedBy)
	}
}

// ---------------------------------------------------------------------------
// TODOCollector
// ---------------------------------------------------------------------------

func TestTODOCollector_ParsesUncheckedItems(t *testing.T) {
	td := t.TempDir()
	body := strings.Join([]string{
		"# TODO",
		"",
		"- [ ] [auto-ok] Fix typo in README",
		"  Continuation line",
		"- [x] already done",
		"- [ ] Refactor the thing",
		"",
		"## Section",
		"",
		"- [ ] Another task",
	}, "\n")
	if err := os.WriteFile(filepath.Join(td, "TODO.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	c := NewTODOCollector()
	got, err := c.Collect(context.Background(), "x/y", td)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 unchecked tasks, got %d: %+v", len(got), got)
	}
	if !got[0].HasAutoOK {
		t.Error("first task should have auto-ok marker")
	}
	if got[1].HasAutoOK {
		t.Error("second task should NOT have auto-ok marker")
	}
	// Continuation line should appear in body of the first task.
	if !strings.Contains(got[0].Body, "Continuation line") {
		t.Errorf("continuation not folded into body: %q", got[0].Body)
	}
}

func TestTODOCollector_HoldMarkerExcludesItem(t *testing.T) {
	td := t.TempDir()
	body := strings.Join([]string{
		"# TODO",
		"",
		"- [ ] regular task",
		"- [ ] **ASYNC-CORE-1** something [hold] — spec under review",
		"  continuation that should also be skipped",
		"- [ ] [HOLD] case-insensitive marker",
		"- [ ] another regular task",
	}, "\n")
	if err := os.WriteFile(filepath.Join(td, "TODO.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := NewTODOCollector().Collect(context.Background(), "x/y", td)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 tasks (held items skipped), got %d: %+v", len(got), got)
	}
	for _, task := range got {
		if strings.Contains(strings.ToLower(task.Body), "hold") {
			t.Errorf("held item leaked through: %q", task.Body)
		}
	}
}

func TestHasHoldMarker(t *testing.T) {
	yes := []string{
		"foo [hold] bar",
		"FOO [HOLD] BAR",
		"foo [ hold ] bar",
		"- [ ] **TASK** thing [hold] — note",
	}
	for _, s := range yes {
		if !HasHoldMarker(s) {
			t.Errorf("HasHoldMarker(%q) = false, want true", s)
		}
	}
	no := []string{
		"foo bar",
		"household items",   // substring without brackets — fine
		"hold the door",
	}
	for _, s := range no {
		if HasHoldMarker(s) {
			t.Errorf("HasHoldMarker(%q) = true, want false", s)
		}
	}
}

func TestTODOCollector_MissingFileIsNotAnError(t *testing.T) {
	td := t.TempDir() // no TODO.md created
	c := NewTODOCollector()
	got, err := c.Collect(context.Background(), "x/y", td)
	if err != nil {
		t.Fatalf("missing TODO.md should not be an error, got %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected zero tasks, got %d", len(got))
	}
}

// ---------------------------------------------------------------------------
// PlanCollector
// ---------------------------------------------------------------------------

func TestPlanCollector_EmitsOneTaskPerFile(t *testing.T) {
	td := t.TempDir()
	plansDir := filepath.Join(td, "plans")
	if err := os.MkdirAll(plansDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(plansDir, "alpha.md"), []byte("<!-- auto-ok -->\nplan body alpha\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(plansDir, "beta.md"), []byte("plan body beta\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	c := NewPlanCollector()
	got, err := c.Collect(context.Background(), "x/y", td)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 plan tasks, got %d", len(got))
	}
	// Sorted glob: alpha first.
	if got[0].Source.Title != "alpha" || got[1].Source.Title != "beta" {
		t.Errorf("titles wrong: %q, %q", got[0].Source.Title, got[1].Source.Title)
	}
	if !got[0].HasAutoOK {
		t.Error("alpha.md should be auto-ok")
	}
	if got[1].HasAutoOK {
		t.Error("beta.md should NOT be auto-ok")
	}
}

func TestPlanCollector_NoPlansDir(t *testing.T) {
	td := t.TempDir()
	c := NewPlanCollector()
	got, err := c.Collect(context.Background(), "x/y", td)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("expected zero, got %d", len(got))
	}
}

// ---------------------------------------------------------------------------
// IssueCollector — uses httptest to fake the GitHub REST API
// ---------------------------------------------------------------------------

// fakeIssuesAPI returns a server that serves /repos/<owner>/<name>/issues
// with the provided JSON body. Pull-request entries are filtered by the
// collector, so we include one to exercise that branch.
func fakeIssuesAPI(t *testing.T, json string) (*github.Client, func()) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Make sure the label filter went through.
		if !strings.Contains(r.URL.RawQuery, "labels=auto-ok") {
			t.Errorf("expected labels=auto-ok in query, got %q", r.URL.RawQuery)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(json))
	}))
	c := github.NewClient(nil)
	u, _ := url.Parse(srv.URL + "/")
	c.BaseURL = u
	return c, srv.Close
}

func TestIssueCollector_ListsAndFiltersPRs(t *testing.T) {
	body := `[
		{"number": 1, "title": "Fix flaky test", "html_url": "https://example/1", "body": "details"},
		{"number": 2, "title": "Open PR not real issue", "html_url": "https://example/2", "body": "x", "pull_request": {"url": "https://example/pulls/2"}}
	]`
	client, cleanup := fakeIssuesAPI(t, body)
	defer cleanup()

	c := NewIssueCollector(client)
	got, err := c.Collect(context.Background(), "x/y", "")
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 issue (PR filtered), got %d: %+v", len(got), got)
	}
	if got[0].Source.Title != "Fix flaky test" {
		t.Errorf("wrong title: %q", got[0].Source.Title)
	}
	if !got[0].HasAutoOK {
		t.Error("issue collector tasks must always be auto-ok")
	}
}

func TestIssueCollector_RejectsBadRepoFormat(t *testing.T) {
	c := NewIssueCollector(github.NewClient(nil))
	_, err := c.Collect(context.Background(), "no-slash", "")
	if err == nil {
		t.Fatal("expected error for bad repo format")
	}
}

func TestIssueCollector_RejectsNilClient(t *testing.T) {
	c := &IssueCollector{}
	_, err := c.Collect(context.Background(), "x/y", "")
	if err == nil {
		t.Fatal("expected nil-client error")
	}
}

// ---------------------------------------------------------------------------
// CollectAll integration — TODO + plan + issue with dedup applied
// ---------------------------------------------------------------------------

func TestCollectAll_IntegratesAndDedupes(t *testing.T) {
	td := t.TempDir()
	// TODO.md with two items, one of which collides with the issue title.
	todoBody := strings.Join([]string{
		"# TODO",
		"- [ ] [auto-ok] Fix flaky test",
		"- [ ] Unique todo task",
	}, "\n")
	if err := os.WriteFile(filepath.Join(td, "TODO.md"), []byte(todoBody), 0o644); err != nil {
		t.Fatal(err)
	}
	// plans/foo.md — separate task, no collision.
	if err := os.MkdirAll(filepath.Join(td, "plans"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(td, "plans", "foo.md"), []byte("plan body"), 0o644); err != nil {
		t.Fatal(err)
	}

	issuesJSON := `[{"number": 1, "title": "Fix flaky test", "html_url": "https://example/1", "body": ""}]`
	client, cleanup := fakeIssuesAPI(t, issuesJSON)
	defer cleanup()

	got, err := CollectAll(
		context.Background(),
		"x/y",
		td,
		NewTODOCollector(),
		NewPlanCollector(),
		NewIssueCollector(client),
	)
	if err != nil {
		t.Fatalf("CollectAll: %v", err)
	}

	// Expected after dedup: issue (absorbs the matching TODO) + unique TODO + plan = 3
	if len(got) != 3 {
		t.Fatalf("expected 3 deduped tasks, got %d:\n%+v", len(got), got)
	}

	// The issue must come first (Pass 1 of Dedup keeps issues in order).
	if got[0].Source.Type != state.SourceIssue {
		t.Errorf("expected issue first, got %q", got[0].Source.Type)
	}
	if !strings.Contains(got[0].TrackedBy, "todo:") {
		t.Errorf("issue should track the absorbed TODO: %q", got[0].TrackedBy)
	}

	// The remaining two should be the unique TODO and the plan.
	types := []state.SourceType{got[1].Source.Type, got[2].Source.Type}
	hasType := func(want state.SourceType) bool {
		for _, t := range types {
			if t == want {
				return true
			}
		}
		return false
	}
	if !hasType(state.SourceTODO) {
		t.Error("expected the unique TODO to survive dedup")
	}
	if !hasType(state.SourcePlan) {
		t.Error("expected the plan to survive dedup")
	}
}
