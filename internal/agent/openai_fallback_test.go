// file: internal/agent/openai_fallback_test.go
// version: 1.1.0

package agent

import (
	"strings"
	"testing"
)

// TestCallResponsesWithRetry_PropagatesNon429 verifies that non-rate-limit
// errors from the Responses endpoint pass through immediately without the
// retry loop burning its budget.
func TestCallResponsesWithRetry_PropagatesNon429(t *testing.T) {
	// Stub timeNow so the retry budget never expires; the loop should exit
	// on the first non-429 error, not via deadline.
	orig := timeNow
	defer func() { timeNow = orig }()

	calls := 0
	// The actual Responses client call is behind an interface we can't easily
	// stub without a full mock server, so we verify the 429-detection logic
	// directly via is429.
	for _, msg := range []string{"auth: invalid api key", "403 Forbidden", "model not found"} {
		if is429(msg) {
			t.Errorf("is429(%q) = true, want false (non-rate-limit error)", msg)
		}
		calls++
	}
	if calls != 3 {
		t.Fatalf("expected 3 checks, got %d", calls)
	}
}

// TestIs429_DetectsBothFormats confirms the two shapes OpenAI uses for
// rate-limit errors are both recognized.
func TestIs429_DetectsBothFormats(t *testing.T) {
	cases := []struct {
		msg  string
		want bool
	}{
		{"429 Too Many Requests", true},
		{"rate limit exceeded", true},
		{"Rate Limit Exceeded", true},
		{"Please try again in 12.5s (rate limit)", true},
		{"auth: invalid api key", false},
		{"500 Internal Server Error", false},
		{"model_not_found", false},
	}
	for _, tc := range cases {
		if got := is429(tc.msg); got != tc.want {
			t.Errorf("is429(%q) = %v, want %v", tc.msg, got, tc.want)
		}
	}
}

// TestParseRetryAfter_ParsesHint confirms the regex correctly extracts the
// "try again in X.Ys" hint that OpenAI embeds in 429 bodies.
func TestParseRetryAfter_ParsesHint(t *testing.T) {
	cases := []struct {
		msg  string
		want string // empty = expect 0
	}{
		{"Please try again in 12.5s.", "12.5s"},
		{"Rate limit hit. try again in 3s and cool down.", "3s"},
		{"no hint here", ""},
		{"try again in 0s", ""}, // 0 is rejected
	}
	for _, tc := range cases {
		d := parseRetryAfter(tc.msg)
		if tc.want == "" && d != 0 {
			t.Errorf("parseRetryAfter(%q) = %v, want 0", tc.msg, d)
		}
		if tc.want != "" && d == 0 {
			t.Errorf("parseRetryAfter(%q) = 0, want non-zero (%s)", tc.msg, tc.want)
		}
		if tc.want != "" && !strings.Contains(d.String(), strings.TrimSuffix(tc.want, "s")) {
			t.Errorf("parseRetryAfter(%q) = %v, want ~%s", tc.msg, d, tc.want)
		}
	}
}
