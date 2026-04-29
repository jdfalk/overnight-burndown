// file: internal/agent/openai_fallback_test.go
// version: 1.0.0

package agent

import (
	"errors"
	"fmt"
	"testing"
)

func TestErrRetriesExhausted_IsWraps(t *testing.T) {
	// callResponsesWithRetry wraps errRetriesExhausted via fmt.Errorf("%w ...");
	// the fallback wrapper relies on errors.Is detecting the sentinel.
	wrapped := fmt.Errorf("%w (last err: %v)", errRetriesExhausted, errors.New("429 boom"))
	if !errors.Is(wrapped, errRetriesExhausted) {
		t.Fatalf("errors.Is(wrapped, errRetriesExhausted) = false, want true. msg=%q", wrapped)
	}
	other := fmt.Errorf("auth: invalid api key")
	if errors.Is(other, errRetriesExhausted) {
		t.Errorf("errors.Is on unrelated error returned true; should be false")
	}
}
