package dispatch

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// Worktree describes a git worktree the dispatcher created for one task.
type Worktree struct {
	Path   string
	Branch string
}

// AddWorktree creates a worktree at `path` on a new branch `branch`,
// branched off of the parent repo's HEAD. The parent repo lives at
// `repoPath`. Returns an error if the branch already exists or the path
// is already a worktree.
//
// We shell out to `git` directly rather than going through safe-ai-util:
// worktree management is a driver-side concern (the agent never touches
// branches), and the directory we're creating sits outside the repo's
// SAFE_AI_UTIL_REPO_ROOT, so safe-ai-util would reject the operation
// anyway.
func AddWorktree(ctx context.Context, repoPath, branch, path string) (*Worktree, error) {
	cmd := exec.CommandContext(ctx, "git", "-C", repoPath, "worktree", "add", "-b", branch, path)
	if out, err := cmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("git worktree add: %w (output: %s)", err, strings.TrimSpace(string(out)))
	}
	return &Worktree{Path: path, Branch: branch}, nil
}

// RemoveWorktree drops a worktree previously created via AddWorktree.
// The branch is left intact; callers can decide whether to delete it
// separately depending on whether the task succeeded.
//
// `--force` is used so a worktree with uncommitted changes still gets
// removed — the dispatcher uses this for failure cleanup where leaving
// stale worktrees on disk is worse than losing in-progress work.
func RemoveWorktree(ctx context.Context, repoPath, path string) error {
	cmd := exec.CommandContext(ctx, "git", "-C", repoPath, "worktree", "remove", "--force", path)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git worktree remove: %w (output: %s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// DeleteBranch deletes a local branch in the parent repo. Used after a
// failed run to leave no trace; passes -D so the branch goes regardless
// of merge state.
func DeleteBranch(ctx context.Context, repoPath, branch string) error {
	cmd := exec.CommandContext(ctx, "git", "-C", repoPath, "branch", "-D", branch)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git branch -D: %w (output: %s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// SlugifyForBranch turns an arbitrary string into a safe branch component:
// lowercase, kebab-case, ASCII alphanumeric + dash only, collapsed
// dashes, capped at 40 chars. Empty input yields "task". Used to build a
// branch name from the triage agent's suggested-branch text or from the
// task title.
func SlugifyForBranch(s string) string {
	s = strings.ToLower(s)
	s = nonSlugRe.ReplaceAllString(s, "-")
	s = collapseDashRe.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	if len(s) > 40 {
		s = strings.TrimRight(s[:40], "-")
	}
	if s == "" {
		s = "task"
	}
	return s
}

var (
	nonSlugRe      = regexp.MustCompile(`[^a-z0-9]+`)
	collapseDashRe = regexp.MustCompile(`-+`)
)

// WorktreePath returns the canonical worktree path for a (repo, slug) pair
// under the dispatcher's worktree root.
func WorktreePath(worktreeRoot, repoName, branchSlug string) string {
	return filepath.Join(worktreeRoot, repoName, branchSlug)
}
