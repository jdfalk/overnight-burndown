// file: internal/runner/reconcile_test.go
// version: 1.0.0
// guid: d5e6f7a8-b9c0-1d2e-3f4a-5b6c7d8e9f0a

package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/google/go-github/v84/github"

	"github.com/jdfalk/overnight-burndown/internal/state"
)

// stubPRServer returns an httptest.Server that serves a fixed slice of PRs
// from GET /repos/owner/repo/pulls, split across pages of pageSize.
// It also handles the second-page sentinel (returns empty list).
func stubPRServer(t *testing.T, allPRs []*github.PullRequest, pageSize int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		page := 1
		if p := r.URL.Query().Get("page"); p != "" {
			fmt.Sscan(p, &page)
		}
		start := (page - 1) * pageSize
		end := start + pageSize
		if end > len(allPRs) {
			end = len(allPRs)
		}
		slice := []*github.PullRequest{}
		if start < len(allPRs) {
			slice = allPRs[start:end]
		}
		w.Header().Set("Content-Type", "application/json")
		if end < len(allPRs) {
			nextURL := fmt.Sprintf(`http://%s/repos/owner/repo/pulls?page=%d&per_page=%d`,
				r.Host, page+1, pageSize)
			w.Header().Set("Link", fmt.Sprintf(`<%s>; rel="next"`, nextURL))
		}
		json.NewEncoder(w).Encode(slice)
	}))
}

func makePR(n int, branch string, labels ...string) *github.PullRequest {
	var ls []*github.Label
	for _, l := range labels {
		name := l
		ls = append(ls, &github.Label{Name: &name})
	}
	num := n
	url := fmt.Sprintf("https://github.com/owner/repo/pull/%d", n)
	ref := branch
	updated := github.Timestamp{Time: time.Now()}
	return &github.PullRequest{
		Number:    &num,
		HTMLURL:   &url,
		Labels:    ls,
		UpdatedAt: &updated,
		Head:      &github.PullRequestBranch{Ref: &ref},
	}
}

func makeGHClient(t *testing.T, server *httptest.Server) *github.Client {
	t.Helper()
	c := github.NewClient(nil)
	u, _ := url.Parse(server.URL + "/")
	c.BaseURL = u
	return c
}

// TestReconcileFromGitHub_HappyPath: two open automation PRs → two new state rows.
func TestReconcileFromGitHub_HappyPath(t *testing.T) {
	prs := []*github.PullRequest{
		makePR(1, "auto/fix-foo", "automation"),
		makePR(2, "auto/fix-bar", "automation"),
	}
	srv := stubPRServer(t, prs, 100)
	defer srv.Close()

	gh := makeGHClient(t, srv)
	s := state.New()

	if err := ReconcileFromGitHub(context.Background(), gh, s, "owner", "repo"); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	for _, pr := range prs {
		branch := pr.GetHead().GetRef()
		hash := BranchHash(branch)
		ts, ok := s.Get(hash)
		if !ok {
			t.Errorf("expected state row for branch %q (hash %s)", branch, hash)
			continue
		}
		if ts.Status != state.StatusDraft {
			t.Errorf("branch %q: got status %q, want StatusDraft", branch, ts.Status)
		}
		if ts.PRNumber != pr.GetNumber() {
			t.Errorf("branch %q: got PRNumber %d, want %d", branch, ts.PRNumber, pr.GetNumber())
		}
	}
}

// TestReconcileFromGitHub_Idempotency: calling reconcile twice must not overwrite
// an existing row or create duplicates.
func TestReconcileFromGitHub_Idempotency(t *testing.T) {
	pr := makePR(10, "auto/idempotent", "automation")
	srv := stubPRServer(t, []*github.PullRequest{pr}, 100)
	defer srv.Close()
	gh := makeGHClient(t, srv)

	s := state.New()
	// Pre-populate with StatusShipped — reconcile must leave it alone.
	branch := pr.GetHead().GetRef()
	hash := BranchHash(branch)
	s.Upsert(&state.TaskState{
		Hash:     hash,
		Branch:   branch,
		PRNumber: 10,
		Status:   state.StatusShipped,
	})

	if err := ReconcileFromGitHub(context.Background(), gh, s, "owner", "repo"); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	// Second call.
	if err := ReconcileFromGitHub(context.Background(), gh, s, "owner", "repo"); err != nil {
		t.Fatalf("reconcile second: %v", err)
	}

	ts, ok := s.Get(hash)
	if !ok {
		t.Fatal("state row disappeared")
	}
	if ts.Status != state.StatusShipped {
		t.Errorf("existing row overwritten: got %q, want StatusShipped", ts.Status)
	}
}

// TestReconcileFromGitHub_NonBurndownIgnored: PRs without "automation" label are skipped.
func TestReconcileFromGitHub_NonBurndownIgnored(t *testing.T) {
	prs := []*github.PullRequest{
		makePR(20, "feature/human-work"), // no automation label
		makePR(21, "auto/bot-work", "automation"),
	}
	srv := stubPRServer(t, prs, 100)
	defer srv.Close()
	gh := makeGHClient(t, srv)
	s := state.New()

	if err := ReconcileFromGitHub(context.Background(), gh, s, "owner", "repo"); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	// Human PR should NOT be in state.
	if _, ok := s.Get(BranchHash("feature/human-work")); ok {
		t.Error("non-automation PR was reconciled — should be skipped")
	}
	// Bot PR should be in state.
	if _, ok := s.Get(BranchHash("auto/bot-work")); !ok {
		t.Error("automation PR missing from state")
	}
}

// TestReconcileFromGitHub_Pagination: PRs across two pages are all reconciled.
func TestReconcileFromGitHub_Pagination(t *testing.T) {
	var prs []*github.PullRequest
	for i := 1; i <= 5; i++ {
		prs = append(prs, makePR(i, fmt.Sprintf("auto/task-%d", i), "automation"))
	}
	srv := stubPRServer(t, prs, 3) // page size 3 → two pages
	defer srv.Close()
	gh := makeGHClient(t, srv)
	s := state.New()

	if err := ReconcileFromGitHub(context.Background(), gh, s, "owner", "repo"); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	for _, pr := range prs {
		branch := pr.GetHead().GetRef()
		if _, ok := s.Get(BranchHash(branch)); !ok {
			t.Errorf("PR #%d branch %q missing after pagination", pr.GetNumber(), branch)
		}
	}
}

// TestReconcileFromGitHub_APIError: a list error is returned to the caller.
func TestReconcileFromGitHub_APIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "internal server error", http.StatusInternalServerError)
	}))
	defer srv.Close()
	gh := makeGHClient(t, srv)
	s := state.New()

	err := ReconcileFromGitHub(context.Background(), gh, s, "owner", "repo")
	if err == nil {
		t.Fatal("expected error from reconcile, got nil")
	}
}
