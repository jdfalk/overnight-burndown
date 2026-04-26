package ghops

import (
	"path"
	"strings"

	"github.com/jdfalk/overnight-burndown/internal/triage"
)

// MergeDecision is the verdict from the auto-merge gate.
type MergeDecision struct {
	// Allow is true only when every gate passes.
	Allow bool
	// Reasons enumerates every gate that rejected the PR. Empty when
	// Allow=true. Listed in evaluation order so the caller can produce
	// a deterministic digest entry.
	Reasons []string
}

// GateInputs is everything the gate needs to render a verdict.
//
// We pass plain values (not interfaces) so the gate is fully testable
// without GitHub or Anthropic dependencies — the dispatcher gathers all
// of these and calls EvaluateGate as a pure function.
type GateInputs struct {
	Classification triage.Classification
	HasAutoOK      bool
	CIStatus       CIStatus

	ChangedFiles []ChangedFile

	// AutoMergePaths is the per-repo glob allowlist. AUTO_MERGE_SAFE PRs
	// must touch ONLY paths that match at least one of these globs.
	// Empty allowlist → no auto-merge possible. Patterns use `path.Match`
	// semantics with one extension: `**` matches any number of path
	// segments (so `tests/**` matches `tests/foo/bar.go`).
	AutoMergePaths []string

	// ForcedReviewPaths are the hard vetoes. Any matching file forces
	// NEEDS_REVIEW even if classification + auto-ok + path allowlist
	// would otherwise pass. Same glob semantics.
	ForcedReviewPaths []string

	// DiffSizeCapLines forces NEEDS_REVIEW above this size. PLAN.md
	// default is 200; 0 disables the cap (not recommended).
	DiffSizeCapLines int
}

// EvaluateGate applies the four gates from PLAN.md design B2 plus the
// hard vetoes. All gates are evaluated even if an earlier one fails, so
// the digest can show the operator every reason a PR didn't auto-merge.
func EvaluateGate(in GateInputs) MergeDecision {
	var reasons []string

	// Gate 1: classification
	if in.Classification != triage.ClassAutoMergeSafe {
		reasons = append(reasons, "classification is "+string(in.Classification)+", not AUTO_MERGE_SAFE")
	}

	// Gate 2: auto-ok marker present at the source level
	if !in.HasAutoOK {
		reasons = append(reasons, "task source did not carry the auto-ok marker")
	}

	// Gate 3: every changed file must match the per-repo allowlist
	if len(in.AutoMergePaths) == 0 {
		reasons = append(reasons, "repo has no auto_merge_paths allowlist configured")
	} else {
		for _, f := range in.ChangedFiles {
			if !matchesAny(f.Path, in.AutoMergePaths) {
				reasons = append(reasons,
					"file '"+f.Path+"' is not in the auto-merge allowlist")
			}
		}
	}

	// Gate 4: CI must be green
	switch in.CIStatus {
	case CISuccess:
		// pass
	case CIPending:
		reasons = append(reasons, "CI status is pending (timed out or still running)")
	case CIFailure:
		reasons = append(reasons, "CI status is failure")
	default:
		reasons = append(reasons, "CI status is "+string(in.CIStatus))
	}

	// Hard vetoes
	for _, f := range in.ChangedFiles {
		if matchesAny(f.Path, in.ForcedReviewPaths) {
			reasons = append(reasons,
				"file '"+f.Path+"' matches a forced-review pattern (hard veto)")
		}
	}
	if in.DiffSizeCapLines > 0 {
		if total := TotalLinesChanged(in.ChangedFiles); total > in.DiffSizeCapLines {
			reasons = append(reasons,
				diffOverCapReason(total, in.DiffSizeCapLines))
		}
	}

	return MergeDecision{
		Allow:   len(reasons) == 0,
		Reasons: reasons,
	}
}

func diffOverCapReason(actual, cap int) string {
	return "diff size " + itoa(actual) + " lines exceeds cap of " + itoa(cap)
}

// matchesAny returns true when `p` matches at least one pattern. Supports
// trailing `/**` to mean "any descendants" in addition to the standard
// path.Match semantics. Anchored: pattern must match the whole path.
func matchesAny(p string, patterns []string) bool {
	for _, pat := range patterns {
		if matchOne(p, pat) {
			return true
		}
	}
	return false
}

// matchOne implements globstar-style pattern matching over path.Match:
//
//   - `**` matches zero or more path segments (so `**/migrations/**`
//     matches both `migrations/foo.sql` and `a/b/migrations/foo.sql`).
//   - `*` matches anything except `/` (path.Match semantics).
//   - The pattern is anchored — must match the whole path, not a suffix.
//
// Cases handled explicitly:
//
//   - `**` alone → match everything.
//   - `**/X` → X at any depth (whole path or basename match).
//   - `X/**` → X itself or any descendant of X.
//   - `**/X/**` → X appears as a complete segment anywhere in the path.
//   - Anything else → path.Match (single-segment glob).
//
// Empty pattern matches nothing.
func matchOne(p, pattern string) bool {
	switch {
	case pattern == "":
		return false

	case pattern == "**":
		return true

	case strings.HasPrefix(pattern, "**/") && strings.HasSuffix(pattern, "/**"):
		// `**/X/**` — find X as a complete path segment anywhere in p.
		middle := strings.TrimPrefix(strings.TrimSuffix(pattern, "/**"), "**/")
		return containsSegment(p, middle)

	case strings.HasPrefix(pattern, "**/"):
		// `**/X` — X at any depth. Try whole-path match first, then
		// progressively trim leading segments and try again.
		rest := strings.TrimPrefix(pattern, "**/")
		if ok, _ := path.Match(rest, p); ok {
			return true
		}
		seg := p
		for {
			i := strings.Index(seg, "/")
			if i < 0 {
				return false
			}
			seg = seg[i+1:]
			if ok, _ := path.Match(rest, seg); ok {
				return true
			}
		}

	case strings.HasSuffix(pattern, "/**"):
		// `X/**` — X itself or any descendant of X. The X portion may
		// itself contain a glob (`tests/*/data/**`).
		prefix := strings.TrimSuffix(pattern, "/**")
		if p == prefix {
			return true
		}
		if strings.HasPrefix(p, prefix+"/") {
			return true
		}
		segments := strings.Split(p, "/")
		for i := 1; i <= len(segments); i++ {
			candidate := strings.Join(segments[:i], "/")
			if ok, _ := path.Match(prefix, candidate); ok {
				return true
			}
		}
		return false

	default:
		ok, _ := path.Match(pattern, p)
		return ok
	}
}

// containsSegment reports whether segment appears as a complete path
// segment in p — at the start, end, middle, or as the whole path. Used
// for `**/X/**` matching.
func containsSegment(p, segment string) bool {
	if p == segment {
		return true
	}
	if strings.HasPrefix(p, segment+"/") {
		return true
	}
	if strings.HasSuffix(p, "/"+segment) {
		return true
	}
	return strings.Contains(p, "/"+segment+"/")
}

// itoa is a small helper to avoid importing strconv in this file alone.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
