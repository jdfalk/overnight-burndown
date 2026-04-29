// Package ghops handles all driver-side GitHub interactions.
//
// Trust-boundary recap: agents never touch git or GitHub. After the
// implementer agent finishes editing files in a worktree, the dispatcher
// hands the worktree off to ghops. ghops:
//
//   1. Commits the worktree's changes locally (driver-side).
//   2. Pushes the new branch to origin using the GitHub App installation
//      token (auth.Auth from step 4).
//   3. Opens a PR with classification-aware draft state.
//
// Step 9b adds CI watch + merge gating on top of this.
//
// All git invocations shell out via exec, with the App installation
// token injected via a one-shot push URL — never written to .git/config
// or any file the agent could read.
package ghops

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"strings"

	"github.com/google/go-github/v84/github"
)

// gitRunner runs a `git -C <wd> <args>` and returns combined stdout+stderr.
// Pulled out so tests can stub it.
type gitRunner func(ctx context.Context, wd string, env []string, args ...string) (string, error)

// TokenSource produces a fresh GitHub App installation token. Implemented
// by *auth.Auth in production; tests inject a stub.
type TokenSource interface {
	InstallationToken(ctx context.Context) (string, error)
}

// Publisher wraps a single repo's gh-ops surface. Construct one per repo
// per night.
type Publisher struct {
	GitHub *github.Client
	Tokens TokenSource

	Owner string
	Name  string

	// AuthorName / AuthorEmail used for the commit. Recommended:
	//   Name:  "jdfalk-burndown-bot[bot]"
	//   Email: "<APP_ID>+jdfalk-burndown-bot[bot]@users.noreply.github.com"
	AuthorName  string
	AuthorEmail string

	// runGit lets tests intercept git invocations. Production wires
	// realRunGit (defaults applied in NewPublisher).
	runGit gitRunner
}

// NewPublisher returns a Publisher with the production gitRunner wired.
func NewPublisher(gh *github.Client, t TokenSource, owner, name, authorName, authorEmail string) *Publisher {
	return &Publisher{
		GitHub:      gh,
		Tokens:      t,
		Owner:       owner,
		Name:        name,
		AuthorName:  authorName,
		AuthorEmail: authorEmail,
		runGit:      realRunGit,
	}
}

// CommitOptions describes what to commit + push.
type CommitOptions struct {
	WorktreePath string
	Branch       string
	Message      string
}

// CommitAndPush stages all changes in WorktreePath, makes a commit
// authored as the configured bot identity, and pushes the branch to
// origin using a one-shot URL with the App installation token. The
// token never lands on disk.
//
// Returns ErrNoChanges if the agent produced no diff. Callers treat
// that as a successful no-op rather than a failure.
func (p *Publisher) CommitAndPush(ctx context.Context, opts CommitOptions) error {
	if opts.WorktreePath == "" || opts.Branch == "" {
		return errors.New("ghops: WorktreePath and Branch required")
	}
	if opts.Message == "" {
		return errors.New("ghops: empty commit message")
	}

	if _, err := p.runGit(ctx, opts.WorktreePath, nil, "add", "-A"); err != nil {
		return fmt.Errorf("ghops: git add: %w", err)
	}

	clean, err := p.stagedIsClean(ctx, opts.WorktreePath)
	if err != nil {
		return fmt.Errorf("ghops: detect staged diff: %w", err)
	}
	if clean {
		return ErrNoChanges
	}

	commitEnv := []string{
		"GIT_AUTHOR_NAME=" + p.AuthorName,
		"GIT_AUTHOR_EMAIL=" + p.AuthorEmail,
		"GIT_COMMITTER_NAME=" + p.AuthorName,
		"GIT_COMMITTER_EMAIL=" + p.AuthorEmail,
	}
	if _, err := p.runGit(ctx, opts.WorktreePath, commitEnv, "commit", "-m", opts.Message); err != nil {
		return fmt.Errorf("ghops: git commit: %w", err)
	}

	tok, err := p.Tokens.InstallationToken(ctx)
	if err != nil {
		return fmt.Errorf("ghops: mint installation token: %w", err)
	}
	pushURL := fmt.Sprintf("https://x-access-token:%s@github.com/%s/%s.git",
		url.QueryEscape(tok),
		url.PathEscape(p.Owner),
		url.PathEscape(p.Name),
	)
	// actions/checkout sets http.<github>.extraheader in the parent repo's
	// .git/config (persistent credentials with the workflow's GITHUB_TOKEN).
	// Worktrees inherit that config, and git applies the extraheader AFTER
	// URL parse so it OVERRIDES the App token we baked into pushURL — the
	// push then authenticates as github-actions[bot] (which has zero rights
	// to other jdfalk repos) and fails with "Permission denied". Clear
	// every plausible variant before the push: per-host extraheader, the
	// new lowercase form (git 2.31+), and the global fallback.
	if _, err := p.runGit(ctx, opts.WorktreePath, nil,
		"-c", "http.https://github.com/.extraheader=",
		"-c", "http.https://github.com/.extraHeader=",
		"-c", "http.extraheader=",
		"-c", "http.extraHeader=",
		"push", pushURL, opts.Branch+":"+opts.Branch); err != nil {
		// Redact the token from any output the git binary leaked into
		// the error before propagating.
		return fmt.Errorf("ghops: git push: %w", redactToken(err, tok))
	}
	return nil
}

// ErrNoChanges signals a clean worktree — the agent produced no diff.
var ErrNoChanges = errors.New("ghops: no changes to commit")

// stagedIsClean returns true when there is nothing staged. We can't
// rely on `git commit` exiting nonzero on an empty diff because some
// configs allow empty commits; explicit check is more portable.
func (p *Publisher) stagedIsClean(ctx context.Context, wd string) (bool, error) {
	_, err := p.runGit(ctx, wd, nil, "diff", "--cached", "--quiet")
	if err == nil {
		return true, nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) && ee.ExitCode() == 1 {
		return false, nil
	}
	return false, err
}

// PROptions describes the PR to open.
type PROptions struct {
	Branch     string
	Title      string
	Body       string
	BaseBranch string // usually "main"
	Draft      bool   // true for NEEDS_REVIEW; false for AUTO_MERGE_SAFE
}

// OpenPR creates the pull request. Returns the created PR so the
// driver can record `Number` and `HTMLURL` in state.
func (p *Publisher) OpenPR(ctx context.Context, opts PROptions) (*github.PullRequest, error) {
	if opts.Branch == "" || opts.Title == "" || opts.BaseBranch == "" {
		return nil, errors.New("ghops: Branch, Title, BaseBranch required")
	}
	req := &github.NewPullRequest{
		Title: github.Ptr(opts.Title),
		Head:  github.Ptr(opts.Branch),
		Base:  github.Ptr(opts.BaseBranch),
		Body:  github.Ptr(opts.Body),
		Draft: github.Ptr(opts.Draft),
	}
	pr, _, err := p.GitHub.PullRequests.Create(ctx, p.Owner, p.Name, req)
	if err != nil {
		return nil, fmt.Errorf("ghops: create PR: %w", err)
	}
	return pr, nil
}

// ---------------------------------------------------------------------------
// internals
// ---------------------------------------------------------------------------

// realRunGit is the production gitRunner. It always inherits os.Environ
// (so PATH and HOME are present) and layers caller-supplied env on top.
// stdout and stderr are merged; non-zero exit returns an *exec.ExitError
// wrapped with the combined output.
func realRunGit(ctx context.Context, wd string, env []string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", wd}, args...)...)
	cmd.Env = append(os.Environ(), env...)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	if err := cmd.Run(); err != nil {
		return buf.String(), fmt.Errorf("%w: %s", err, strings.TrimSpace(buf.String()))
	}
	return buf.String(), nil
}

// redactToken replaces every occurrence of the token in the error's
// message with "***". The wrapped exit-error is dropped — that's
// intentional, the message is what we care about for log safety; the
// caller has already failed and is reporting up.
func redactToken(err error, tok string) error {
	if tok == "" {
		return err
	}
	msg := err.Error()
	if !strings.Contains(msg, tok) {
		return err
	}
	return errors.New(strings.ReplaceAll(msg, tok, "***"))
}
