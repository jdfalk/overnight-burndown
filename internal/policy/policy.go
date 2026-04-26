// Package policy renders per-repo allowlist overlays as TOML for safe-ai-util's
// --policy-overlay flag.
//
// The schema mirrors safe-ai-util's `AllowlistOverlay` struct (see
// safe-ai-util PR #7). Every field is optional — an empty overlay is a
// no-op. When a field is present, safe-ai-util enforces the
// narrow-only invariant: the overlay can only tighten the base allowlist,
// never widen it.
//
// We hand-render TOML rather than depend on a TOML encoder. The schema
// is tiny, the keys are stable, and avoiding the dep keeps this package
// trivially auditable.
package policy

import (
	"fmt"
	"os"
	"sort"
	"strings"
)

// Overlay is the TOML-renderable narrowing overlay.
type Overlay struct {
	// PermissiveMode, when set, must be false (overlay can flip permissive
	// off but not on; safe-ai-util will reject the latter).
	PermissiveMode *bool

	// AlwaysAllowed restricts the base's always-allowed set to the
	// listed commands. Empty slice = drop everything; nil slice = no
	// override.
	AlwaysAllowed []string

	// Blocked is unioned with the base — overlay can only ADD bans.
	Blocked []string

	// ConditionallyAllowed tightens per-command restrictions. Each key
	// must already exist in the base; safe-ai-util rejects new keys.
	ConditionallyAllowed map[string]Restrictions
}

// Restrictions are the per-command limits in the conditional table.
type Restrictions struct {
	MaxArgs           *int
	RequiredArgs      []string
	ForbiddenArgs     []string
	AllowedPatterns   []string
	ForbiddenPatterns []string
}

// IsEmpty reports whether the overlay would render an empty TOML file.
// The orchestrator uses this to skip --policy-overlay entirely when the
// caller has nothing to add.
func (o Overlay) IsEmpty() bool {
	return o.PermissiveMode == nil &&
		o.AlwaysAllowed == nil &&
		len(o.Blocked) == 0 &&
		len(o.ConditionallyAllowed) == 0
}

// MarshalTOML renders the overlay. Output is deterministic — keys are
// sorted, slices are emitted in input order — so the same input always
// produces byte-identical output. Useful for cache hashing and review.
func (o Overlay) MarshalTOML() string {
	var b strings.Builder

	if o.PermissiveMode != nil {
		fmt.Fprintf(&b, "permissive_mode = %t\n", *o.PermissiveMode)
	}
	if o.AlwaysAllowed != nil {
		fmt.Fprintf(&b, "always_allowed = %s\n", tomlStringArray(o.AlwaysAllowed))
	}
	if len(o.Blocked) > 0 {
		fmt.Fprintf(&b, "blocked = %s\n", tomlStringArray(o.Blocked))
	}

	if len(o.ConditionallyAllowed) > 0 {
		// Sort keys for deterministic output.
		keys := make([]string, 0, len(o.ConditionallyAllowed))
		for k := range o.ConditionallyAllowed {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Fprintf(&b, "\n[conditionally_allowed.%s]\n", k)
			r := o.ConditionallyAllowed[k]
			if r.MaxArgs != nil {
				fmt.Fprintf(&b, "max_args = %d\n", *r.MaxArgs)
			}
			if len(r.RequiredArgs) > 0 {
				fmt.Fprintf(&b, "required_args = %s\n", tomlStringArray(r.RequiredArgs))
			}
			if len(r.ForbiddenArgs) > 0 {
				fmt.Fprintf(&b, "forbidden_args = %s\n", tomlStringArray(r.ForbiddenArgs))
			}
			if len(r.AllowedPatterns) > 0 {
				fmt.Fprintf(&b, "allowed_patterns = %s\n", tomlStringArray(r.AllowedPatterns))
			}
			if len(r.ForbiddenPatterns) > 0 {
				fmt.Fprintf(&b, "forbidden_patterns = %s\n", tomlStringArray(r.ForbiddenPatterns))
			}
		}
	}

	return b.String()
}

// WriteToFile renders the overlay and writes it atomically (temp file +
// rename). The path's parent directory is created if missing.
func (o Overlay) WriteToFile(path string) error {
	if err := os.MkdirAll(parentDir(path), 0o755); err != nil {
		return fmt.Errorf("policy: mkdir overlay parent: %w", err)
	}
	tmp, err := os.CreateTemp(parentDir(path), ".overlay-*.toml")
	if err != nil {
		return fmt.Errorf("policy: temp file: %w", err)
	}
	tmpName := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpName)
		}
	}()

	if _, err := tmp.WriteString(o.MarshalTOML()); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("policy: write overlay: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("policy: close overlay: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("policy: rename overlay: %w", err)
	}
	cleanup = false
	return nil
}

// Tighten returns the strict-tightening of o by other. The result's
// rules are no looser than either operand:
//
//   - Blocked sets are unioned.
//   - AlwaysAllowed becomes the intersection when both specify; if only
//     one specifies, that one wins (more restrictive direction).
//   - ConditionallyAllowed entries are merged per-command with
//     restrictions tightened (forbidden lists unioned, max_args min'd).
//   - PermissiveMode false wins (more restrictive).
//
// This mirrors the narrow-only semantics safe-ai-util enforces, but on
// the Go side — letting the burndown driver compose multiple overlay
// sources (config defaults + per-repo file + driver-supplied bans)
// before writing the final overlay file.
func (o Overlay) Tighten(other Overlay) Overlay {
	out := Overlay{}

	// PermissiveMode: false wins.
	switch {
	case o.PermissiveMode != nil && other.PermissiveMode != nil:
		v := *o.PermissiveMode && *other.PermissiveMode
		out.PermissiveMode = &v
	case o.PermissiveMode != nil:
		out.PermissiveMode = o.PermissiveMode
	case other.PermissiveMode != nil:
		out.PermissiveMode = other.PermissiveMode
	}

	// AlwaysAllowed: intersection when both populated.
	switch {
	case o.AlwaysAllowed != nil && other.AlwaysAllowed != nil:
		out.AlwaysAllowed = intersectStrings(o.AlwaysAllowed, other.AlwaysAllowed)
	case o.AlwaysAllowed != nil:
		out.AlwaysAllowed = append([]string(nil), o.AlwaysAllowed...)
	case other.AlwaysAllowed != nil:
		out.AlwaysAllowed = append([]string(nil), other.AlwaysAllowed...)
	}

	// Blocked: union.
	out.Blocked = unionStrings(o.Blocked, other.Blocked)
	if len(out.Blocked) == 0 {
		out.Blocked = nil
	}

	// ConditionallyAllowed: merge per-command with tightened restrictions.
	if len(o.ConditionallyAllowed) > 0 || len(other.ConditionallyAllowed) > 0 {
		out.ConditionallyAllowed = make(map[string]Restrictions)
		for k, v := range o.ConditionallyAllowed {
			out.ConditionallyAllowed[k] = v
		}
		for k, v := range other.ConditionallyAllowed {
			if existing, ok := out.ConditionallyAllowed[k]; ok {
				out.ConditionallyAllowed[k] = tightenRestrictions(existing, v)
			} else {
				out.ConditionallyAllowed[k] = v
			}
		}
	}

	return out
}

// tightenRestrictions takes the stricter combination of two Restrictions.
func tightenRestrictions(a, b Restrictions) Restrictions {
	out := Restrictions{
		RequiredArgs:      unionStrings(a.RequiredArgs, b.RequiredArgs),
		ForbiddenArgs:     unionStrings(a.ForbiddenArgs, b.ForbiddenArgs),
		ForbiddenPatterns: unionStrings(a.ForbiddenPatterns, b.ForbiddenPatterns),
	}
	// AllowedPatterns: intersection when both populated; populated wins
	// otherwise (empty allowed_patterns means "anything goes" in
	// safe-ai-util — it's a footgun to drop a populated list to empty).
	switch {
	case len(a.AllowedPatterns) > 0 && len(b.AllowedPatterns) > 0:
		out.AllowedPatterns = intersectStrings(a.AllowedPatterns, b.AllowedPatterns)
	case len(a.AllowedPatterns) > 0:
		out.AllowedPatterns = append([]string(nil), a.AllowedPatterns...)
	case len(b.AllowedPatterns) > 0:
		out.AllowedPatterns = append([]string(nil), b.AllowedPatterns...)
	}
	// MaxArgs: minimum when both set.
	switch {
	case a.MaxArgs != nil && b.MaxArgs != nil:
		v := *a.MaxArgs
		if *b.MaxArgs < v {
			v = *b.MaxArgs
		}
		out.MaxArgs = &v
	case a.MaxArgs != nil:
		out.MaxArgs = a.MaxArgs
	case b.MaxArgs != nil:
		out.MaxArgs = b.MaxArgs
	}
	return out
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func tomlStringArray(items []string) string {
	if len(items) == 0 {
		return "[]"
	}
	parts := make([]string, len(items))
	for i, s := range items {
		parts[i] = `"` + tomlEscape(s) + `"`
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

// tomlEscape handles the small subset of TOML basic-string escapes we
// might encounter in command names and patterns.
func tomlEscape(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	s = strings.ReplaceAll(s, "\n", `\n`)
	return s
}

func intersectStrings(a, b []string) []string {
	bs := make(map[string]struct{}, len(b))
	for _, s := range b {
		bs[s] = struct{}{}
	}
	var out []string
	seen := make(map[string]struct{}, len(a))
	for _, s := range a {
		if _, ok := bs[s]; !ok {
			continue
		}
		if _, dup := seen[s]; dup {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}

func unionStrings(a, b []string) []string {
	seen := make(map[string]struct{}, len(a)+len(b))
	var out []string
	for _, s := range a {
		if _, ok := seen[s]; !ok {
			seen[s] = struct{}{}
			out = append(out, s)
		}
	}
	for _, s := range b {
		if _, ok := seen[s]; !ok {
			seen[s] = struct{}{}
			out = append(out, s)
		}
	}
	return out
}

func parentDir(path string) string {
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '/' {
			return path[:i]
		}
	}
	return "."
}
