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

// AddWorktree creates a worktree at `path` on branch `branch`, branched
// off of the parent repo's HEAD. The parent repo lives at `repoPath`.
//
// If the branch already exists on the remote (a previous interrupted run),
// it is fetched and the worktree is created from that state so the agent
// can continue where it left off and the eventual push is fast-forward.
//
// We shell out to `git` directly rather than going through safe-ai-util:
// worktree management is a driver-side concern (the agent never touches
// branches), and the directory we're creating sits outside the repo's
// SAFE_AI_UTIL_REPO_ROOT, so safe-ai-util would reject the operation
// anyway.
//
// If `excludePaths` is non-empty, the worktree is materialized with
// non-cone sparse-checkout that includes everything except those
// directories. This keeps disk usage down on runners with limited scratch
// space when the repo has heavy fixtures (e.g. checked-in audio testdata)
// the burndown bot doesn't need. Each entry is interpreted as a directory
// prefix relative to repo root; leading/trailing "/" are normalized.
func AddWorktree(ctx context.Context, repoPath, branch, path string, excludePaths ...string) (*Worktree, error) {
	var cmd *exec.Cmd
	if remoteHasBranch(ctx, repoPath, branch) {
		// Fetch the existing remote branch into a local tracking branch so
		// the worktree starts from the prior run's state.
		if out, err := exec.CommandContext(ctx, "git", "-C", repoPath,
			"fetch", "origin", branch+":"+branch).CombinedOutput(); err != nil {
			return nil, fmt.Errorf("git fetch existing branch: %w (output: %s)", err, strings.TrimSpace(string(out)))
		}
		cmd = exec.CommandContext(ctx, "git", "-C", repoPath, "worktree", "add", path, branch)
	} else {
		cmd = exec.CommandContext(ctx, "git", "-C", repoPath, "worktree", "add", "-b", branch, path)
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("git worktree add: %w (output: %s)", err, strings.TrimSpace(string(out)))
	}
	if len(excludePaths) > 0 {
		if err := applySparseExclude(ctx, path, excludePaths); err != nil {
			// Best-effort cleanup so a half-configured worktree doesn't
			// linger and confuse the next run.
			_ = RemoveWorktree(ctx, repoPath, path)
			_ = DeleteBranch(ctx, repoPath, branch)
			return nil, fmt.Errorf("apply sparse-checkout exclude: %w", err)
		}
	}
	return &Worktree{Path: path, Branch: branch}, nil
}

// applySparseExclude switches the worktree at `path` to non-cone
// sparse-checkout that includes everything except `excludes`. Tree objects
// remain in .git, so a later `sparse-checkout disable` restores the full
// working copy if a downstream step needs it.
func applySparseExclude(ctx context.Context, path string, excludes []string) error {
	// Non-cone mode uses gitignore-style patterns. "/*" includes everything
	// at root; each "!/<dir>/" line negates a subtree so it stays out of the
	// working copy.
	patterns := []string{"/*"}
	for _, e := range excludes {
		e = strings.Trim(e, "/")
		if e == "" {
			continue
		}
		patterns = append(patterns, "!/"+e+"/")
	}
	if out, err := exec.CommandContext(ctx, "git", "-C", path, "sparse-checkout", "init", "--no-cone").CombinedOutput(); err != nil {
		return fmt.Errorf("sparse-checkout init: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	args := append([]string{"-C", path, "sparse-checkout", "set", "--no-cone"}, patterns...)
	if out, err := exec.CommandContext(ctx, "git", args...).CombinedOutput(); err != nil {
		return fmt.Errorf("sparse-checkout set: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
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

// remoteHasBranch reports whether `branch` exists on origin without
// fetching it. Uses ls-remote which is a read-only network call.
func remoteHasBranch(ctx context.Context, repoPath, branch string) bool {
	out, err := exec.CommandContext(ctx, "git", "-C", repoPath,
		"ls-remote", "--heads", "origin", branch).Output()
	return err == nil && strings.Contains(string(out), "refs/heads/"+branch)
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
