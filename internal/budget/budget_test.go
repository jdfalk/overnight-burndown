package budget

import (
	"math"
	"sync"
	"testing"
	"time"

	"github.com/jdfalk/overnight-burndown/internal/config"
)

// fixtureCfg returns a BudgetConfig that's plausible for a nightly run.
func fixtureCfg() config.BudgetConfig {
	return config.BudgetConfig{
		MaxDollars:     5.0,
		MaxWallSeconds: 7200, // 2h
		AbortThreshold: 0.8,
	}
}

// withClock returns a Budget whose nowFn is controlled by the returned
// setter. Lets tests drive elapsed time without sleeping.
func withClock(t *testing.T, cfg config.BudgetConfig) (*Budget, func(time.Time)) {
	t.Helper()
	current := time.Date(2026, 1, 1, 23, 0, 0, 0, time.UTC)
	b := New(cfg)
	b.startedAt = current
	b.nowFn = func() time.Time { return current }
	return b, func(t time.Time) { current = t }
}

// approxEqual tolerates the float64 rounding inherent to per-token math.
func approxEqual(a, b float64) bool {
	const eps = 1e-6
	return math.Abs(a-b) < eps
}

// ---------------------------------------------------------------------------
// Pricing math — Opus 4.7 rates
// ---------------------------------------------------------------------------

func TestRecord_OpusPricingMath(t *testing.T) {
	b := New(fixtureCfg())
	b.Record("claude-opus-4-7", Usage{
		InputTokens:              1_000_000, // $5 of input
		OutputTokens:             1_000_000, // $25 of output
		CacheReadInputTokens:     1_000_000, // $0.50 cached
		CacheCreationInputTokens: 1_000_000, // $6.25 cache writes
	})
	want := 5.0 + 25.0 + 0.50 + 6.25
	if !approxEqual(b.Snapshot().DollarsSpent, want) {
		t.Errorf("DollarsSpent: got %.6f, want %.6f", b.Snapshot().DollarsSpent, want)
	}
}

// ---------------------------------------------------------------------------
// Pricing math — Haiku rates (10× cheaper input than Opus)
// ---------------------------------------------------------------------------

func TestRecord_HaikuPricingMath(t *testing.T) {
	b := New(fixtureCfg())
	b.Record("claude-haiku-4-5", Usage{
		InputTokens:  1_000_000, // $1
		OutputTokens: 1_000_000, // $5
	})
	want := 1.0 + 5.0
	if !approxEqual(b.Snapshot().DollarsSpent, want) {
		t.Errorf("DollarsSpent: got %.6f, want %.6f", b.Snapshot().DollarsSpent, want)
	}
}

// ---------------------------------------------------------------------------
// Unknown model falls back to Opus rates (pessimistic-safe)
// ---------------------------------------------------------------------------

func TestRecord_UnknownModelUsesOpusRates(t *testing.T) {
	b := New(fixtureCfg())
	b.Record("claude-future-7-9", Usage{InputTokens: 1_000_000})
	if !approxEqual(b.Snapshot().DollarsSpent, 5.0) {
		t.Errorf("expected $5 for 1M input at opus rates, got %.6f", b.Snapshot().DollarsSpent)
	}
}

// ---------------------------------------------------------------------------
// Token totals accumulate independently of pricing
// ---------------------------------------------------------------------------

func TestRecord_AccumulatesTokenCountsAcrossCalls(t *testing.T) {
	b := New(fixtureCfg())
	b.Record("claude-haiku-4-5", Usage{InputTokens: 100, OutputTokens: 50, CacheReadInputTokens: 200})
	b.Record("claude-haiku-4-5", Usage{InputTokens: 50, OutputTokens: 25, CacheReadInputTokens: 100})

	s := b.Snapshot()
	if s.TokensInput != 150 {
		t.Errorf("TokensInput: %d", s.TokensInput)
	}
	if s.TokensOutput != 75 {
		t.Errorf("TokensOutput: %d", s.TokensOutput)
	}
	if s.TokensCached != 300 {
		t.Errorf("TokensCached: %d", s.TokensCached)
	}
}

// ---------------------------------------------------------------------------
// ShouldAbort — dollar cap fires at threshold
// ---------------------------------------------------------------------------

func TestShouldAbort_DollarThreshold(t *testing.T) {
	b, _ := withClock(t, fixtureCfg())

	// 79% of $5 = $3.95 — under threshold.
	b.Record("claude-haiku-4-5", Usage{InputTokens: 3_950_000})
	if abort, _ := b.ShouldAbort(); abort {
		t.Errorf("at 79%% spent, should not abort; got Snapshot=%+v", b.Snapshot())
	}

	// One more cent — across the 80% line.
	b.Record("claude-haiku-4-5", Usage{InputTokens: 100_000}) // +$0.10 → $4.05
	abort, reason := b.ShouldAbort()
	if !abort {
		t.Fatalf("at $4.05 of $5 cap (>80%%), expected abort")
	}
	if !contains(reason, "$") || !contains(reason, "80%") {
		t.Errorf("reason should explain dollar threshold, got: %q", reason)
	}
}

// ---------------------------------------------------------------------------
// ShouldAbort — wall-clock fires at threshold
// ---------------------------------------------------------------------------

func TestShouldAbort_WallClockThreshold(t *testing.T) {
	cfg := fixtureCfg()
	cfg.MaxWallSeconds = 100 // 100s cap, threshold 80% → abort at 80s
	b, setNow := withClock(t, cfg)

	setNow(b.startedAt.Add(79 * time.Second))
	if abort, _ := b.ShouldAbort(); abort {
		t.Errorf("at 79%% wall, should not abort")
	}

	setNow(b.startedAt.Add(80 * time.Second))
	abort, reason := b.ShouldAbort()
	if !abort {
		t.Fatalf("at 80s of 100s cap, expected abort")
	}
	if !contains(reason, "wall-clock") || !contains(reason, "80%") {
		t.Errorf("reason should explain wall-clock threshold, got: %q", reason)
	}
}

// ---------------------------------------------------------------------------
// ShouldAbort — either cap independently triggers (OR semantics)
// ---------------------------------------------------------------------------

func TestShouldAbort_DollarTriggersWhenWallStillSafe(t *testing.T) {
	b, _ := withClock(t, fixtureCfg())
	b.Record("claude-opus-4-7", Usage{OutputTokens: 200_000}) // $5 — over cap
	abort, reason := b.ShouldAbort()
	if !abort {
		t.Fatalf("dollar cap exceeded should trigger abort regardless of wall-clock")
	}
	if !contains(reason, "$") {
		t.Errorf("reason should be dollar-shaped: %q", reason)
	}
}

func TestShouldAbort_WallTriggersWhenDollarStillSafe(t *testing.T) {
	cfg := fixtureCfg()
	cfg.MaxWallSeconds = 10
	b, setNow := withClock(t, cfg)

	// Spend nothing.
	setNow(b.startedAt.Add(9 * time.Second))
	if abort, _ := b.ShouldAbort(); !abort {
		t.Errorf("at 90%% wall and 0%% dollar, should still abort on wall")
	}
}

// ---------------------------------------------------------------------------
// Threshold validation: out-of-range falls back to 0.8
// ---------------------------------------------------------------------------

func TestShouldAbort_ThresholdFallback(t *testing.T) {
	cfg := fixtureCfg()
	cfg.AbortThreshold = 0 // invalid
	b, _ := withClock(t, cfg)
	b.Record("claude-opus-4-7", Usage{OutputTokens: 200_000}) // $5
	if abort, _ := b.ShouldAbort(); !abort {
		t.Errorf("threshold=0 should fall back to 0.8 → $4 trigger; got no abort with $5 spent")
	}
}

// ---------------------------------------------------------------------------
// Stats helpers — fractions
// ---------------------------------------------------------------------------

func TestStats_FractionSpent(t *testing.T) {
	s := Stats{DollarsSpent: 2.5, DollarsCap: 5.0}
	if got := s.FractionSpent(); !approxEqual(got, 0.5) {
		t.Errorf("FractionSpent: got %.3f", got)
	}
	zero := Stats{DollarsCap: 0}
	if got := zero.FractionSpent(); got != 0 {
		t.Errorf("Cap=0 should yield 0, got %v", got)
	}
}

func TestStats_FractionElapsed(t *testing.T) {
	s := Stats{Elapsed: 30 * time.Minute, WallCap: time.Hour}
	if got := s.FractionElapsed(); !approxEqual(got, 0.5) {
		t.Errorf("FractionElapsed: got %.3f", got)
	}
}

// ---------------------------------------------------------------------------
// Concurrency — Record + Snapshot under -race
// ---------------------------------------------------------------------------

func TestRecord_ConcurrentSafe(t *testing.T) {
	b := New(fixtureCfg())
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			b.Record("claude-haiku-4-5", Usage{InputTokens: 1000, OutputTokens: 500})
		}()
	}
	// Snapshot/ShouldAbort racing with Records.
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = b.Snapshot()
			_, _ = b.ShouldAbort()
		}()
	}
	wg.Wait()

	s := b.Snapshot()
	if s.TokensInput != 50_000 {
		t.Errorf("TokensInput: got %d want 50000", s.TokensInput)
	}
	if s.TokensOutput != 25_000 {
		t.Errorf("TokensOutput: got %d want 25000", s.TokensOutput)
	}
}

// ---------------------------------------------------------------------------
// Snapshot — Elapsed reflects nowFn
// ---------------------------------------------------------------------------

func TestSnapshot_ReflectsElapsed(t *testing.T) {
	b, setNow := withClock(t, fixtureCfg())
	setNow(b.startedAt.Add(15 * time.Minute))
	if s := b.Snapshot(); s.Elapsed != 15*time.Minute {
		t.Errorf("Elapsed: got %v", s.Elapsed)
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
