package policy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func boolPtr(v bool) *bool { return &v }
func intPtr(v int) *int    { return &v }

// ---------------------------------------------------------------------------
// IsEmpty
// ---------------------------------------------------------------------------

func TestIsEmpty(t *testing.T) {
	if !(Overlay{}).IsEmpty() {
		t.Error("zero overlay should be empty")
	}
	if (Overlay{Blocked: []string{"bash"}}).IsEmpty() {
		t.Error("populated Blocked → not empty")
	}
}

// ---------------------------------------------------------------------------
// MarshalTOML — happy path
// ---------------------------------------------------------------------------

func TestMarshalTOML_Happy(t *testing.T) {
	o := Overlay{
		PermissiveMode: boolPtr(false),
		AlwaysAllowed:  []string{"git", "make"},
		Blocked:        []string{"bash", "curl"},
		ConditionallyAllowed: map[string]Restrictions{
			"docker": {
				MaxArgs:           intPtr(5),
				ForbiddenArgs:     []string{"--privileged"},
				ForbiddenPatterns: []string{`--volume.*:/`},
			},
		},
	}
	got := o.MarshalTOML()
	for _, want := range []string{
		"permissive_mode = false",
		`always_allowed = ["git", "make"]`,
		`blocked = ["bash", "curl"]`,
		"[conditionally_allowed.docker]",
		"max_args = 5",
		`forbidden_args = ["--privileged"]`,
		`forbidden_patterns = ["--volume.*:/"]`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in output:\n%s", want, got)
		}
	}
}

// ---------------------------------------------------------------------------
// MarshalTOML — deterministic ordering
// ---------------------------------------------------------------------------

func TestMarshalTOML_DeterministicKeyOrder(t *testing.T) {
	o := Overlay{
		ConditionallyAllowed: map[string]Restrictions{
			"zsh":    {MaxArgs: intPtr(1)},
			"docker": {MaxArgs: intPtr(2)},
			"npm":    {MaxArgs: intPtr(3)},
		},
	}
	a := o.MarshalTOML()
	b := o.MarshalTOML()
	if a != b {
		t.Fatal("MarshalTOML must be deterministic across calls")
	}
	// Sorted: docker, npm, zsh.
	di := strings.Index(a, "[conditionally_allowed.docker]")
	ni := strings.Index(a, "[conditionally_allowed.npm]")
	zi := strings.Index(a, "[conditionally_allowed.zsh]")
	if !(di < ni && ni < zi) {
		t.Errorf("keys not sorted: docker=%d npm=%d zsh=%d", di, ni, zi)
	}
}

// ---------------------------------------------------------------------------
// MarshalTOML — escaping
// ---------------------------------------------------------------------------

func TestMarshalTOML_EscapesSpecialChars(t *testing.T) {
	o := Overlay{
		Blocked: []string{`weird"name`, `back\slash`},
	}
	got := o.MarshalTOML()
	if !strings.Contains(got, `\"`) {
		t.Errorf("expected escaped quote: %s", got)
	}
	if !strings.Contains(got, `\\`) {
		t.Errorf("expected escaped backslash: %s", got)
	}
}

// ---------------------------------------------------------------------------
// MarshalTOML — empty overlay produces empty string
// ---------------------------------------------------------------------------

func TestMarshalTOML_EmptyOverlay(t *testing.T) {
	if (Overlay{}).MarshalTOML() != "" {
		t.Error("empty overlay should produce empty TOML")
	}
}

// ---------------------------------------------------------------------------
// WriteToFile — atomic write, no leftovers
// ---------------------------------------------------------------------------

func TestWriteToFile_AtomicAndNoLeftovers(t *testing.T) {
	td := t.TempDir()
	path := filepath.Join(td, "subdir", "overlay.toml")
	o := Overlay{Blocked: []string{"bash"}}
	if err := o.WriteToFile(path); err != nil {
		t.Fatalf("WriteToFile: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(got), `blocked = ["bash"]`) {
		t.Errorf("file content wrong: %s", got)
	}
	// No tempfile leftovers in the parent directory.
	entries, _ := os.ReadDir(filepath.Join(td, "subdir"))
	for _, e := range entries {
		if e.Name() != "overlay.toml" {
			t.Errorf("unexpected leftover: %q", e.Name())
		}
	}
}

// ---------------------------------------------------------------------------
// Tighten — Blocked union
// ---------------------------------------------------------------------------

func TestTighten_BlockedUnion(t *testing.T) {
	a := Overlay{Blocked: []string{"bash", "curl"}}
	b := Overlay{Blocked: []string{"curl", "wget"}}
	got := a.Tighten(b)
	want := []string{"bash", "curl", "wget"}
	if !equalStrings(got.Blocked, want) {
		t.Errorf("Blocked: got %v want %v", got.Blocked, want)
	}
}

// ---------------------------------------------------------------------------
// Tighten — AlwaysAllowed intersection
// ---------------------------------------------------------------------------

func TestTighten_AlwaysAllowedIntersection(t *testing.T) {
	a := Overlay{AlwaysAllowed: []string{"git", "make", "go"}}
	b := Overlay{AlwaysAllowed: []string{"git", "go", "npm"}}
	got := a.Tighten(b)
	want := []string{"git", "go"}
	if !equalStrings(got.AlwaysAllowed, want) {
		t.Errorf("AlwaysAllowed: got %v want %v", got.AlwaysAllowed, want)
	}
}

// ---------------------------------------------------------------------------
// Tighten — only one side has AlwaysAllowed → wins
// ---------------------------------------------------------------------------

func TestTighten_AlwaysAllowedOneSidedWins(t *testing.T) {
	a := Overlay{AlwaysAllowed: []string{"git"}}
	b := Overlay{}
	got := a.Tighten(b)
	if !equalStrings(got.AlwaysAllowed, []string{"git"}) {
		t.Errorf("got %v", got.AlwaysAllowed)
	}
}

// ---------------------------------------------------------------------------
// Tighten — Restrictions tighten correctly
// ---------------------------------------------------------------------------

func TestTighten_RestrictionsMerge(t *testing.T) {
	a := Overlay{ConditionallyAllowed: map[string]Restrictions{
		"docker": {
			MaxArgs:       intPtr(20),
			ForbiddenArgs: []string{"--privileged"},
		},
	}}
	b := Overlay{ConditionallyAllowed: map[string]Restrictions{
		"docker": {
			MaxArgs:       intPtr(5),
			ForbiddenArgs: []string{"--rm"},
		},
	}}
	got := a.Tighten(b)
	r := got.ConditionallyAllowed["docker"]
	if r.MaxArgs == nil || *r.MaxArgs != 5 {
		t.Errorf("MaxArgs: should be min, got %v", r.MaxArgs)
	}
	if !equalStrings(r.ForbiddenArgs, []string{"--privileged", "--rm"}) {
		t.Errorf("ForbiddenArgs: got %v", r.ForbiddenArgs)
	}
}

// ---------------------------------------------------------------------------
// Tighten — PermissiveMode false wins
// ---------------------------------------------------------------------------

func TestTighten_PermissiveFalseWins(t *testing.T) {
	a := Overlay{PermissiveMode: boolPtr(true)}
	b := Overlay{PermissiveMode: boolPtr(false)}
	got := a.Tighten(b)
	if got.PermissiveMode == nil || *got.PermissiveMode != false {
		t.Errorf("expected false, got %v", got.PermissiveMode)
	}
}

// ---------------------------------------------------------------------------
// Tighten — single-sided restriction is preserved
// ---------------------------------------------------------------------------

func TestTighten_OnlyOneSideHasCommand(t *testing.T) {
	a := Overlay{ConditionallyAllowed: map[string]Restrictions{
		"docker": {MaxArgs: intPtr(10)},
	}}
	b := Overlay{}
	got := a.Tighten(b)
	if got.ConditionallyAllowed["docker"].MaxArgs == nil ||
		*got.ConditionallyAllowed["docker"].MaxArgs != 10 {
		t.Errorf("docker rules dropped: %+v", got.ConditionallyAllowed)
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
