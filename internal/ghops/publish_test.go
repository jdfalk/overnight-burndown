package ghops

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/go-github/v84/github"
)

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

type stubTokens struct {
	tok string
	err error
}

func (s *stubTokens) InstallationToken(_ context.Context) (string, error) {
	if s.err != nil {
		return "", s.err
	}
	return s.tok, nil
}

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
	}
	return string(out)
}

// makeRepoWithRemote creates a bare repo (the "remote") and a regular
// repo whose origin points at it. Returns (workingRepo, bareRepo). The
// working repo has one initial commit on `main`.
func makeRepoWithRemote(t *testing.T) (string, string) {
	t.Helper()

	bare := filepath.Join(t.TempDir(), "remote.git")
	if out, err := exec.Command("git", "init", "--bare", "-b", "main", bare).CombinedOutput(); err != nil {
		t.Fatalf("init bare: %v\n%s", err, out)
	}

	work := t.TempDir()
	runGit(t, work, "init", "-q", "-b", "main")
	runGit(t, work, "config", "user.email", "test@example.com")
	runGit(t, work, "config", "user.name", "Test")
	runGit(t, work, "remote", "add", "origin", bare)
	if err := os.WriteFile(filepath.Join(work, "README.md"), []byte("# initial\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, work, "add", "README.md")
	runGit(t, work, "commit", "-q", "-m", "initial")
	runGit(t, work, "push", "-q", "origin", "main")
	return work, bare
}

// captureRunGit returns a gitRunner that records every invocation and
// delegates to realRunGit. Tests use it to assert on the args we passed.
type recorder struct {
	calls []recorderCall
}

type recorderCall struct {
	WorkDir string
	Env     []string
	Args    []string
}

// fakeGitHub stubs the PR-create endpoint. Returns the URL + cleanup +
// pointer to the captured request body so tests can assert on it.
func fakeGitHub(t *testing.T, ownerName string, prID int) (*github.Client, *atomic.Pointer[map[string]any], func()) {
	t.Helper()
	captured := &atomic.Pointer[map[string]any]{}

	mux := http.NewServeMux()
	mux.HandleFunc("/repos/"+ownerName+"/pulls", func(w http.ResponseWriter, r *http.Request) {
		body := map[string]any{}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode: %v", err)
		}
		captured.Store(&body)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"number":   prID,
			"html_url": "https://example/" + ownerName + "/pull/" + strings.Repeat("0", 1) + "1",
			"draft":    body["draft"],
			"title":    body["title"],
		})
	})
	srv := httptest.NewServer(mux)

	client := github.NewClient(nil)
	u, _ := url.Parse(srv.URL + "/")
	client.BaseURL = u

	return client, captured, srv.Close
}

// ---------------------------------------------------------------------------
// CommitAndPush — happy path
// ---------------------------------------------------------------------------

func TestCommitAndPush_HappyPath(t *testing.T) {
	work, bare := makeRepoWithRemote(t)
	// Make a change in the working copy on a new branch.
	runGit(t, work, "checkout", "-q", "-b", "auto/feature-x")
	if err := os.WriteFile(filepath.Join(work, "added.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	pub := &Publisher{
		Tokens:      &stubTokens{tok: "ghs_TESTTOKEN"},
		Owner:       "jdfalk",
		Name:        "x",
		AuthorName:  "burndown-bot[bot]",
		AuthorEmail: "1+burndown-bot[bot]@users.noreply.github.com",
		runGit:      realRunGit,
	}
	// We can't actually push to a bare repo via https://x-access-token:...
	// (no network); intercept push and forward to a regular `git push origin`.
	rec := &recorder{}
	pub.runGit = func(ctx context.Context, wd string, env []string, args ...string) (string, error) {
		rec.calls = append(rec.calls, recorderCall{WorkDir: wd, Env: append([]string(nil), env...), Args: append([]string(nil), args...)})
		// Rewrite the production-style HTTPS push to a local-bare push so
		// the test can verify the branch actually lands on disk.
		// The push command may have leading `-c http....=` config-clear
		// flags (added to override actions/checkout's inherited extraheader),
		// so find the "push" subcommand index dynamically.
		pushIdx := -1
		for i, a := range args {
			if a == "push" {
				pushIdx = i
				break
			}
		}
		if pushIdx >= 0 && pushIdx+1 < len(args) && strings.HasPrefix(args[pushIdx+1], "https://x-access-token:") {
			// Keep any `-c` prefix flags so the test exercises the same
			// command shape as production; just swap the URL for the bare
			// repo path.
			rewritten := make([]string, 0, len(args))
			rewritten = append(rewritten, args[:pushIdx+1]...) // up to and including "push"
			rewritten = append(rewritten, bare)
			rewritten = append(rewritten, args[pushIdx+2:]...) // refspec + anything after
			return realRunGit(ctx, wd, env, rewritten...)
		}
		return realRunGit(ctx, wd, env, args...)
	}

	err := pub.CommitAndPush(context.Background(), CommitOptions{
		WorktreePath: work,
		Branch:       "auto/feature-x",
		Message:      "chore: add a file",
	})
	if err != nil {
		t.Fatalf("CommitAndPush: %v", err)
	}

	// Verify a commit landed on the bare repo's auto/feature-x ref.
	out := runGit(t, bare, "log", "--oneline", "auto/feature-x")
	if !strings.Contains(out, "chore: add a file") {
		t.Errorf("expected commit on bare/auto/feature-x, got log:\n%s", out)
	}

	// Verify the commit author was the bot identity (not the local
	// user.name from `git config`).
	out = runGit(t, bare, "log", "-1", "--pretty=format:%an <%ae>", "auto/feature-x")
	if !strings.Contains(out, "burndown-bot[bot]") {
		t.Errorf("commit author should be the bot, got %q", out)
	}

	// Verify we never wrote the token to the worktree's .git/config.
	cfg, _ := os.ReadFile(filepath.Join(work, ".git", "config"))
	if strings.Contains(string(cfg), "ghs_TESTTOKEN") {
		t.Errorf("token leaked into .git/config: %s", cfg)
	}
}

// ---------------------------------------------------------------------------
// CommitAndPush — empty diff returns ErrNoChanges
// ---------------------------------------------------------------------------

func TestCommitAndPush_EmptyDiffReturnsErrNoChanges(t *testing.T) {
	work, _ := makeRepoWithRemote(t)
	runGit(t, work, "checkout", "-q", "-b", "auto/empty")

	pub := &Publisher{
		Tokens:      &stubTokens{tok: "ghs_x"},
		Owner:       "jdfalk",
		Name:        "x",
		AuthorName:  "bot",
		AuthorEmail: "bot@example.com",
		runGit:      realRunGit,
	}
	err := pub.CommitAndPush(context.Background(), CommitOptions{
		WorktreePath: work,
		Branch:       "auto/empty",
		Message:      "noop",
	})
	if !errors.Is(err, ErrNoChanges) {
		t.Errorf("expected ErrNoChanges, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// CommitAndPush — token never appears in error output
// ---------------------------------------------------------------------------

func TestCommitAndPush_TokenRedactedFromPushFailure(t *testing.T) {
	work, _ := makeRepoWithRemote(t)
	runGit(t, work, "checkout", "-q", "-b", "auto/leak-test")
	if err := os.WriteFile(filepath.Join(work, "x.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	const sensitiveTok = "ghs_SENSITIVE_DO_NOT_LEAK"

	pub := &Publisher{
		Tokens:      &stubTokens{tok: sensitiveTok},
		Owner:       "jdfalk",
		Name:        "x",
		AuthorName:  "bot",
		AuthorEmail: "bot@example.com",
		runGit:      realRunGit,
	}
	// Force the push to fail by intercepting only the push; let everything
	// else succeed via realRunGit.
	pub.runGit = func(ctx context.Context, wd string, env []string, args ...string) (string, error) {
		// We may pass `-c http....=` flags before the subcommand to clear
		// inherited extraheader auth, so look for "push" anywhere rather
		// than only at args[0].
		isPush := false
		for _, a := range args {
			if a == "push" {
				isPush = true
				break
			}
		}
		if isPush {
			// Emit an error string that contains the token, mimicking what
			// a real `git push` to a bad URL would produce.
			return "", errors.New("fatal: could not authenticate to https://x-access-token:" + sensitiveTok + "@github.com/jdfalk/x.git/")
		}
		return realRunGit(ctx, wd, env, args...)
	}

	err := pub.CommitAndPush(context.Background(), CommitOptions{
		WorktreePath: work,
		Branch:       "auto/leak-test",
		Message:      "test",
	})
	if err == nil {
		t.Fatal("expected push error")
	}
	if strings.Contains(err.Error(), sensitiveTok) {
		t.Errorf("token leaked into error: %v", err)
	}
	if !strings.Contains(err.Error(), "***") {
		t.Errorf("expected redacted '***' marker in error: %v", err)
	}
}

// ---------------------------------------------------------------------------
// CommitAndPush — token-source failure surfaces clearly
// ---------------------------------------------------------------------------

func TestCommitAndPush_TokenSourceFailure(t *testing.T) {
	work, _ := makeRepoWithRemote(t)
	runGit(t, work, "checkout", "-q", "-b", "auto/x")
	if err := os.WriteFile(filepath.Join(work, "x.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	pub := &Publisher{
		Tokens:      &stubTokens{err: errors.New("token mint failed")},
		Owner:       "jdfalk",
		Name:        "x",
		AuthorName:  "bot",
		AuthorEmail: "bot@example.com",
		runGit:      realRunGit,
	}
	err := pub.CommitAndPush(context.Background(), CommitOptions{
		WorktreePath: work,
		Branch:       "auto/x",
		Message:      "test",
	})
	if err == nil || !strings.Contains(err.Error(), "installation token") {
		t.Errorf("expected token-mint error, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// CommitAndPush — argument validation
// ---------------------------------------------------------------------------

func TestCommitAndPush_RequiresFields(t *testing.T) {
	pub := &Publisher{
		Tokens:      &stubTokens{tok: "x"},
		AuthorName:  "bot",
		AuthorEmail: "bot@example.com",
		runGit:      func(_ context.Context, _ string, _ []string, _ ...string) (string, error) { return "", nil },
	}
	cases := []CommitOptions{
		{Branch: "x", Message: "m"},
		{WorktreePath: "/tmp", Message: "m"},
		{WorktreePath: "/tmp", Branch: "x"},
	}
	for i, c := range cases {
		if err := pub.CommitAndPush(context.Background(), c); err == nil {
			t.Errorf("case %d: expected validation error for %+v", i, c)
		}
	}
}

// ---------------------------------------------------------------------------
// OpenPR — happy path with classification-aware draft state
// ---------------------------------------------------------------------------

func TestOpenPR_DraftPropagatesToAPI(t *testing.T) {
	client, captured, cleanup := fakeGitHub(t, "jdfalk/x", 42)
	defer cleanup()

	pub := &Publisher{
		GitHub: client,
		Owner:  "jdfalk",
		Name:   "x",
	}

	pr, err := pub.OpenPR(context.Background(), PROptions{
		Branch:     "draft/refactor",
		Title:      "WIP: refactor X",
		Body:       "draft body",
		BaseBranch: "main",
		Draft:      true,
	})
	if err != nil {
		t.Fatalf("OpenPR: %v", err)
	}
	if pr.GetNumber() != 42 {
		t.Errorf("PR number: %d", pr.GetNumber())
	}

	body := captured.Load()
	if body == nil {
		t.Fatal("captured request body is nil")
	}
	if (*body)["draft"] != true {
		t.Errorf("draft flag not forwarded, request body was: %+v", *body)
	}
	if (*body)["head"] != "draft/refactor" {
		t.Errorf("head wrong: %v", (*body)["head"])
	}
	if (*body)["base"] != "main" {
		t.Errorf("base wrong: %v", (*body)["base"])
	}
}

func TestOpenPR_NotDraftForAutoMerge(t *testing.T) {
	client, captured, cleanup := fakeGitHub(t, "jdfalk/x", 7)
	defer cleanup()

	pub := &Publisher{GitHub: client, Owner: "jdfalk", Name: "x"}

	if _, err := pub.OpenPR(context.Background(), PROptions{
		Branch:     "auto/typo",
		Title:      "fix typo",
		Body:       "body",
		BaseBranch: "main",
		Draft:      false,
	}); err != nil {
		t.Fatalf("OpenPR: %v", err)
	}
	body := captured.Load()
	if (*body)["draft"] != false {
		t.Errorf("draft flag should be false for AUTO_MERGE_SAFE, got %v", (*body)["draft"])
	}
}

// ---------------------------------------------------------------------------
// OpenPR — argument validation
// ---------------------------------------------------------------------------

func TestOpenPR_RequiresFields(t *testing.T) {
	pub := &Publisher{GitHub: github.NewClient(nil), Owner: "x", Name: "y"}
	cases := []PROptions{
		{Title: "t", BaseBranch: "main"},
		{Branch: "b", BaseBranch: "main"},
		{Branch: "b", Title: "t"},
	}
	for i, c := range cases {
		if _, err := pub.OpenPR(context.Background(), c); err == nil {
			t.Errorf("case %d: expected validation error for %+v", i, c)
		}
	}
}
