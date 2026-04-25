package dispatch

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// makeRepo creates a brand-new git repo in a fresh temp dir, makes one
// initial commit so HEAD exists (required by `worktree add`), and returns
// the repo path. Cleanup happens via t.TempDir.
func makeRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()

	run := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	run("init", "-q", "-b", "main")
	run("config", "user.email", "test@example.com")
	run("config", "user.name", "Test")

	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# test repo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "README.md")
	run("commit", "-q", "-m", "initial")

	return dir
}

// ---------------------------------------------------------------------------
// AddWorktree / RemoveWorktree round-trip
// ---------------------------------------------------------------------------

func TestAddWorktree_CreatesBranchAndDir(t *testing.T) {
	repo := makeRepo(t)
	wtPath := filepath.Join(t.TempDir(), "wt")

	wt, err := AddWorktree(context.Background(), repo, "auto/feature-x", wtPath)
	if err != nil {
		t.Fatalf("AddWorktree: %v", err)
	}
	if wt.Path != wtPath || wt.Branch != "auto/feature-x" {
		t.Errorf("Worktree fields: got %+v", wt)
	}
	// Worktree dir should exist with the README from the initial commit.
	if _, err := os.Stat(filepath.Join(wtPath, "README.md")); err != nil {
		t.Fatalf("worktree README missing: %v", err)
	}
	// Branch should be listed in the parent repo.
	out, _ := exec.Command("git", "-C", repo, "branch", "--list", "auto/feature-x").CombinedOutput()
	if !strings.Contains(string(out), "auto/feature-x") {
		t.Errorf("branch not created in parent: %s", out)
	}
}

func TestRemoveWorktree_CleansUp(t *testing.T) {
	repo := makeRepo(t)
	wtPath := filepath.Join(t.TempDir(), "wt")

	if _, err := AddWorktree(context.Background(), repo, "auto/x", wtPath); err != nil {
		t.Fatal(err)
	}
	if err := RemoveWorktree(context.Background(), repo, wtPath); err != nil {
		t.Fatalf("RemoveWorktree: %v", err)
	}
	if _, err := os.Stat(wtPath); !os.IsNotExist(err) {
		t.Errorf("worktree dir still exists: err=%v", err)
	}
}

func TestRemoveWorktree_ForceWorksOnDirtyTree(t *testing.T) {
	repo := makeRepo(t)
	wtPath := filepath.Join(t.TempDir(), "wt")
	if _, err := AddWorktree(context.Background(), repo, "auto/x", wtPath); err != nil {
		t.Fatal(err)
	}

	// Make the worktree dirty (uncommitted change).
	if err := os.WriteFile(filepath.Join(wtPath, "dirty.txt"), []byte("uncommitted"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Should still succeed because we pass --force internally.
	if err := RemoveWorktree(context.Background(), repo, wtPath); err != nil {
		t.Fatalf("RemoveWorktree on dirty tree: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Branch already exists → AddWorktree must fail
// ---------------------------------------------------------------------------

func TestAddWorktree_RejectsExistingBranch(t *testing.T) {
	repo := makeRepo(t)
	// Pre-create the branch in the parent repo.
	if out, err := exec.Command("git", "-C", repo, "branch", "auto/x").CombinedOutput(); err != nil {
		t.Fatalf("seed branch: %v\n%s", err, out)
	}

	_, err := AddWorktree(context.Background(), repo, "auto/x", filepath.Join(t.TempDir(), "wt"))
	if err == nil {
		t.Fatal("expected error from worktree add when branch exists")
	}
}

// ---------------------------------------------------------------------------
// DeleteBranch
// ---------------------------------------------------------------------------

func TestDeleteBranch(t *testing.T) {
	repo := makeRepo(t)
	wtPath := filepath.Join(t.TempDir(), "wt")
	if _, err := AddWorktree(context.Background(), repo, "auto/x", wtPath); err != nil {
		t.Fatal(err)
	}
	// Have to remove the worktree first; you can't delete a branch that's checked out.
	if err := RemoveWorktree(context.Background(), repo, wtPath); err != nil {
		t.Fatal(err)
	}

	if err := DeleteBranch(context.Background(), repo, "auto/x"); err != nil {
		t.Fatalf("DeleteBranch: %v", err)
	}
	out, _ := exec.Command("git", "-C", repo, "branch", "--list", "auto/x").CombinedOutput()
	if strings.Contains(string(out), "auto/x") {
		t.Errorf("branch still present: %s", out)
	}
}

// ---------------------------------------------------------------------------
// SlugifyForBranch
// ---------------------------------------------------------------------------

func TestSlugifyForBranch(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"Fix typo in README", "fix-typo-in-readme"},
		{"  WHAT??!  Big-Issue!", "what-big-issue"},
		{"unicode: café résumé 你好", "unicode-caf-r-sum"}, // non-ASCII drops
		{"---a---", "a"},
		{"", "task"},
		{strings.Repeat("a", 60), strings.Repeat("a", 40)},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			if got := SlugifyForBranch(tc.in); got != tc.want {
				t.Errorf("SlugifyForBranch(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// WorktreePath layout
// ---------------------------------------------------------------------------

func TestWorktreePath(t *testing.T) {
	got := WorktreePath("/tmp/burndown/worktrees", "audiobook-organizer", "auto/typo")
	want := filepath.Join("/tmp/burndown/worktrees", "audiobook-organizer", "auto/typo")
	if got != want {
		t.Errorf("WorktreePath = %q, want %q", got, want)
	}
}
