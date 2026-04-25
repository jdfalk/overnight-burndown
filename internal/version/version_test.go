package version

import (
	"strings"
	"testing"
)

func TestStringHasVPrefix(t *testing.T) {
	got := String()
	if !strings.HasPrefix(got, "v") {
		t.Fatalf("String() = %q, want leading 'v'", got)
	}
	if !strings.Contains(got, Current) {
		t.Fatalf("String() = %q, want it to contain Current = %q", got, Current)
	}
}

func TestCurrentIsNonEmpty(t *testing.T) {
	if Current == "" {
		t.Fatal("Current must not be empty")
	}
}
