// Package budget tracks per-night Anthropic spend + wall-clock and surfaces
// the 80%-of-cap abort signal that drives the graceful checkpoint flow.
//
// What this package does NOT do:
//   - Decide what "abort" means. The orchestrator (cmd/burndown) reads
//     ShouldAbort() between dispatch loops and decides to: finish in-flight
//     tasks, stop accepting new work, persist state, render the partial
//     digest. This package just reports the threshold breach.
//   - Persist anything across nights. Spend is per-run only. Cross-run
//     budget tracking is a follow-up — for v1 each night gets its own
//     fresh $5 cap.
//
// Pricing tables are model-specific and listed below. They are kept
// in sync with the values published in Anthropic's Models doc; if a new
// model lands, add it here. Rates are USD per million tokens.
package budget

import (
	"fmt"
	"sync"
	"time"

	"github.com/jdfalk/overnight-burndown/internal/config"
)

// Pricing captures the four ratecards we care about per model.
type Pricing struct {
	InputPerMillion        float64
	CacheReadPerMillion    float64 // ~10% of input
	CacheWrite5mPerMillion float64 // 1.25× input
	OutputPerMillion       float64
}

// pricing covers every model the burndown driver might use. Missing
// models fall back to opus-4-7 rates — pessimistic but safe.
var pricing = map[string]Pricing{
	"claude-opus-4-7": {
		InputPerMillion: 5.00, CacheReadPerMillion: 0.50,
		CacheWrite5mPerMillion: 6.25, OutputPerMillion: 25.00,
	},
	"claude-opus-4-6": {
		InputPerMillion: 5.00, CacheReadPerMillion: 0.50,
		CacheWrite5mPerMillion: 6.25, OutputPerMillion: 25.00,
	},
	"claude-sonnet-4-6": {
		InputPerMillion: 3.00, CacheReadPerMillion: 0.30,
		CacheWrite5mPerMillion: 3.75, OutputPerMillion: 15.00,
	},
	"claude-haiku-4-5": {
		InputPerMillion: 1.00, CacheReadPerMillion: 0.10,
		CacheWrite5mPerMillion: 1.25, OutputPerMillion: 5.00,
	},
}

// Usage is the per-call token usage. Mirrors the Anthropic Go SDK's
// Usage struct so callers can pass it through unchanged.
type Usage struct {
	InputTokens              int64
	OutputTokens             int64
	CacheReadInputTokens     int64
	CacheCreationInputTokens int64
}

// Stats is a snapshot of the budget's current state. Returned by
// Snapshot for digest rendering.
type Stats struct {
	DollarsSpent  float64
	DollarsCap    float64
	TokensInput   int64
	TokensCached  int64 // cache reads (cheap)
	TokensWritten int64 // cache creations (expensive)
	TokensOutput  int64
	Elapsed       time.Duration
	WallCap       time.Duration
	Threshold     float64 // 0..1 — the abort trigger (e.g. 0.8)
}

// FractionSpent returns spend as a fraction of the dollar cap. 0 if cap
// is zero. Useful for digests that want a "you used 73% of budget" line.
func (s Stats) FractionSpent() float64 {
	if s.DollarsCap <= 0 {
		return 0
	}
	return s.DollarsSpent / s.DollarsCap
}

// FractionElapsed returns elapsed as a fraction of the wall-clock cap.
func (s Stats) FractionElapsed() float64 {
	if s.WallCap <= 0 {
		return 0
	}
	return float64(s.Elapsed) / float64(s.WallCap)
}

// Budget tracks running cost and elapsed time. Safe for concurrent
// Record/ShouldAbort/Snapshot from multiple goroutines.
type Budget struct {
	cfg config.BudgetConfig

	mu            sync.Mutex
	startedAt     time.Time
	spent         float64
	tokensInput   int64
	tokensCached  int64
	tokensWritten int64
	tokensOutput  int64

	// nowFn is injectable so tests can drive elapsed time deterministically.
	nowFn func() time.Time
}

// New creates a fresh budget anchored to time.Now. The orchestrator
// constructs one per nightly run.
func New(cfg config.BudgetConfig) *Budget {
	now := time.Now()
	return &Budget{cfg: cfg, startedAt: now, nowFn: time.Now}
}

// Record adds one Anthropic call's usage to the running total. The model
// string is used to look up pricing; unknown models fall back to opus-4-7
// rates so the budget is biased toward conservative.
func (b *Budget) Record(model string, u Usage) {
	p, ok := pricing[model]
	if !ok {
		p = pricing["claude-opus-4-7"]
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.tokensInput += u.InputTokens
	b.tokensOutput += u.OutputTokens
	b.tokensCached += u.CacheReadInputTokens
	b.tokensWritten += u.CacheCreationInputTokens

	const million = 1_000_000.0
	b.spent += float64(u.InputTokens) * p.InputPerMillion / million
	b.spent += float64(u.OutputTokens) * p.OutputPerMillion / million
	b.spent += float64(u.CacheReadInputTokens) * p.CacheReadPerMillion / million
	b.spent += float64(u.CacheCreationInputTokens) * p.CacheWrite5mPerMillion / million
}

// ShouldAbort reports whether either cap is at or past the configured
// abort threshold. Returns a human-readable reason for the digest. The
// orchestrator polls this between dispatch waves; a true result triggers
// graceful shutdown — finish in-flight work, persist state, render
// partial digest.
//
// Either cap breaching independently triggers — they are OR-combined,
// not AND. Wall-clock 80% with $0.10 spent still aborts (probably means
// CI is taking forever, no point continuing).
func (b *Budget) ShouldAbort() (bool, string) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.cfg.AbortThreshold <= 0 || b.cfg.AbortThreshold >= 1 {
		// Validation should have rejected this — but if we got here, fall
		// back to a sane 0.8 so we don't accidentally abort always or never.
		b.cfg.AbortThreshold = 0.8
	}

	if b.cfg.MaxDollars > 0 {
		dollarThreshold := b.cfg.MaxDollars * b.cfg.AbortThreshold
		if b.spent >= dollarThreshold {
			return true, fmt.Sprintf("$%.2f spent of $%.2f cap (≥ %.0f%% threshold)",
				b.spent, b.cfg.MaxDollars, b.cfg.AbortThreshold*100)
		}
	}

	if b.cfg.MaxWallSeconds > 0 {
		wallCap := time.Duration(b.cfg.MaxWallSeconds) * time.Second
		elapsed := b.nowFn().Sub(b.startedAt)
		if elapsed >= time.Duration(float64(wallCap)*b.cfg.AbortThreshold) {
			return true, fmt.Sprintf("wall-clock %s of %s cap (≥ %.0f%% threshold)",
				elapsed.Round(time.Second), wallCap, b.cfg.AbortThreshold*100)
		}
	}

	return false, ""
}

// Snapshot returns a consistent view of the budget at this instant.
// Used by the digest renderer.
func (b *Budget) Snapshot() Stats {
	b.mu.Lock()
	defer b.mu.Unlock()
	wallCap := time.Duration(b.cfg.MaxWallSeconds) * time.Second
	return Stats{
		DollarsSpent:  b.spent,
		DollarsCap:    b.cfg.MaxDollars,
		TokensInput:   b.tokensInput,
		TokensCached:  b.tokensCached,
		TokensWritten: b.tokensWritten,
		TokensOutput:  b.tokensOutput,
		Elapsed:       b.nowFn().Sub(b.startedAt),
		WallCap:       wallCap,
		Threshold:     b.cfg.AbortThreshold,
	}
}
