// file: internal/agent/openai_retry_test.go
// version: 1.0.0

package agent

import (
	"testing"
	"time"
)

func TestParseRetryAfter(t *testing.T) {
	cases := []struct {
		in   string
		want time.Duration
	}{
		{"Please try again in 3.881s. Visit ...", 3881 * time.Millisecond},
		{"please try again in 12s.", 12 * time.Second},
		{"try again in 0.5s", 500 * time.Millisecond},
		{"no hint here", 0},
		{"try again in 0s", 0},
	}
	for _, c := range cases {
		got := parseRetryAfter(c.in)
		if got != c.want {
			t.Errorf("parseRetryAfter(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestBackoffFor(t *testing.T) {
	base, max := 2*time.Second, 30*time.Second
	want := []time.Duration{
		2 * time.Second,
		4 * time.Second,
		8 * time.Second,
		16 * time.Second,
		max,
		max,
	}
	for i, w := range want {
		got := backoffFor(i+1, base, max)
		if got != w {
			t.Errorf("backoffFor(%d) = %v, want %v", i+1, got, w)
		}
	}
}
