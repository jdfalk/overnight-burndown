package state

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// HashTask determinism + sensitivity
// ---------------------------------------------------------------------------

func TestHashTask_StableForIdenticalSource(t *testing.T) {
	src := Source{
		Type:        SourceIssue,
		Repo:        "jdfalk/audiobook-organizer",
		URL:         "https://github.com/jdfalk/audiobook-organizer/issues/42",
		ContentHash: HashContent("fix typo in README"),
		Title:       "Fix README typo",
	}
	a, b := HashTask(src), HashTask(src)
	if a != b {
		t.Fatalf("HashTask must be deterministic: %s vs %s", a, b)
	}
}

func TestHashTask_DiffersOnContentChange(t *testing.T) {
	base := Source{
		Type:        SourceIssue,
		Repo:        "jdfalk/audiobook-organizer",
		URL:         "https://github.com/jdfalk/audiobook-organizer/issues/42",
		ContentHash: HashContent("v1"),
	}
	other := base
	other.ContentHash = HashContent("v2")
	if HashTask(base) == HashTask(other) {
		t.Fatal("HashTask should differ when ContentHash differs")
	}
}

func TestHashTask_DiffersOnURLChange(t *testing.T) {
	a := Source{Type: SourceIssue, Repo: "x/y", URL: "u1", ContentHash: "abc"}
	b := a
	b.URL = "u2"
	if HashTask(a) == HashTask(b) {
		t.Fatal("HashTask should be URL-sensitive")
	}
}

// ---------------------------------------------------------------------------
// Save/Load roundtrip + atomicity
// ---------------------------------------------------------------------------

func TestSaveLoad_Roundtrip(t *testing.T) {
	td := t.TempDir()
	path := filepath.Join(td, "state.json")

	s := New()
	src := Source{Type: SourceTODO, Repo: "x/y", URL: "TODO.md#L1", ContentHash: "abc"}
	t1 := &TaskState{
		Hash:   HashTask(src),
		Source: src,
		Status: StatusQueued,
	}
	s.Upsert(t1)
	if err := s.Save(path); err != nil {
		t.Fatalf("save: %v", err)
	}

	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(loaded.Tasks) != 1 {
		t.Fatalf("expected 1 task, got %d", len(loaded.Tasks))
	}
	got, ok := loaded.Get(t1.Hash)
	if !ok {
		t.Fatal("task missing after roundtrip")
	}
	if got.Status != StatusQueued {
		t.Errorf("status: got %q want %q", got.Status, StatusQueued)
	}
	if got.FirstSeen.IsZero() {
		t.Error("FirstSeen should be set on first Upsert")
	}
}

func TestSaveDirLoadDir_Roundtrip(t *testing.T) {
	td := t.TempDir()
	s := New()
	src1 := Source{Type: SourceTODO, Repo: "x/y", URL: "TODO.md#L1", ContentHash: "aaa"}
	src2 := Source{Type: SourceIssue, Repo: "x/y", URL: "https://example/2", ContentHash: "bbb"}
	s.Upsert(&TaskState{Hash: HashTask(src1), Source: src1, Status: StatusDraft})
	s.Upsert(&TaskState{Hash: HashTask(src2), Source: src2, Status: StatusInFlight})

	if err := s.SaveDir(td); err != nil {
		t.Fatalf("SaveDir: %v", err)
	}

	loaded, err := LoadDir(td)
	if err != nil {
		t.Fatalf("LoadDir: %v", err)
	}
	if got := len(loaded.Tasks); got != 2 {
		t.Fatalf("want 2 tasks, got %d", got)
	}
	if got, ok := loaded.Get(HashTask(src1)); !ok || got.Status != StatusDraft {
		t.Errorf("task1 status mismatch: ok=%v got=%+v", ok, got)
	}
	if got, ok := loaded.Get(HashTask(src2)); !ok || got.Status != StatusInFlight {
		t.Errorf("task2 status mismatch: ok=%v got=%+v", ok, got)
	}
}

func TestLoadDir_MigratesLegacyMonolithicFile(t *testing.T) {
	td := t.TempDir()
	// Plant a legacy state.json + load it via LoadDir; then SaveDir should
	// rename state.json → state.json.migrated.
	legacy := New()
	src := Source{Type: SourceTODO, Repo: "x/y", URL: "TODO.md#L1", ContentHash: "aaa"}
	legacy.Upsert(&TaskState{Hash: HashTask(src), Source: src, Status: StatusShipped})
	if err := legacy.Save(filepath.Join(td, "state.json")); err != nil {
		t.Fatalf("seed legacy: %v", err)
	}

	loaded, err := LoadDir(td)
	if err != nil {
		t.Fatalf("LoadDir with legacy: %v", err)
	}
	if got, ok := loaded.Get(HashTask(src)); !ok || got.Status != StatusShipped {
		t.Errorf("legacy task missing or wrong status: ok=%v got=%+v", ok, got)
	}
	if err := loaded.SaveDir(td); err != nil {
		t.Fatalf("SaveDir after migration: %v", err)
	}
	if _, err := os.Stat(filepath.Join(td, "state.json")); err == nil {
		t.Error("legacy state.json still present after migration; should be renamed")
	}
	if _, err := os.Stat(filepath.Join(td, "state.json.migrated")); err != nil {
		t.Errorf("state.json.migrated missing after SaveDir: %v", err)
	}
}

func TestSaveTask_WritesSingleFile(t *testing.T) {
	td := t.TempDir()
	s := New()
	src := Source{Type: SourceTODO, Repo: "x/y", URL: "TODO.md#L1", ContentHash: "aaa"}
	t1 := &TaskState{Hash: HashTask(src), Source: src, Status: StatusQueued}
	s.Upsert(t1)
	if err := s.SaveTask(td, t1.Hash); err != nil {
		t.Fatalf("SaveTask: %v", err)
	}
	if _, err := os.Stat(filepath.Join(td, "tasks", t1.Hash+".json")); err != nil {
		t.Errorf("per-task file not written: %v", err)
	}
	// Unknown hash → error.
	if err := s.SaveTask(td, "nope"); err == nil {
		t.Error("SaveTask with unknown hash should error")
	}
}

func TestLoad_MissingFileReturnsEmpty(t *testing.T) {
	td := t.TempDir()
	s, err := Load(filepath.Join(td, "nonexistent.json"))
	if err != nil {
		t.Fatalf("missing file should be empty-ok, got: %v", err)
	}
	if s == nil || len(s.Tasks) != 0 {
		t.Fatalf("expected empty state, got %+v", s)
	}
	if s.SchemaVersion != SchemaVersion {
		t.Errorf("schema version: got %d want %d", s.SchemaVersion, SchemaVersion)
	}
}

func TestLoad_RejectsFutureSchema(t *testing.T) {
	td := t.TempDir()
	path := filepath.Join(td, "state.json")
	if err := os.WriteFile(path, []byte(`{"schema_version": 999, "tasks": {}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for unsupported schema_version")
	}
}

func TestSave_LeavesNoTempFile(t *testing.T) {
	td := t.TempDir()
	path := filepath.Join(td, "state.json")
	s := New()
	if err := s.Save(path); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(td)
	for _, e := range entries {
		name := e.Name()
		if name != "state.json" {
			t.Errorf("unexpected leftover after Save: %q", name)
		}
	}
}

// ---------------------------------------------------------------------------
// Upsert preserves FirstSeen, bumps LastSeen+LastUpdated
// ---------------------------------------------------------------------------

func TestUpsert_PreservesFirstSeen(t *testing.T) {
	s := New()
	src := Source{Type: SourceIssue, Repo: "x/y", URL: "u", ContentHash: "abc"}
	hash := HashTask(src)
	first := &TaskState{Hash: hash, Source: src, Status: StatusQueued}
	s.Upsert(first)
	originalFirstSeen := first.FirstSeen

	second := &TaskState{Hash: hash, Source: src, Status: StatusInFlight}
	s.Upsert(second)

	got, _ := s.Get(hash)
	if !got.FirstSeen.Equal(originalFirstSeen) {
		t.Errorf("FirstSeen should be preserved across upserts: original=%v got=%v",
			originalFirstSeen, got.FirstSeen)
	}
	if got.Status != StatusInFlight {
		t.Errorf("Status should reflect latest upsert, got %q", got.Status)
	}
}

// ---------------------------------------------------------------------------
// InFlight returns only tasks with PR + non-terminal status
// ---------------------------------------------------------------------------

func TestInFlight(t *testing.T) {
	s := New()
	mk := func(hashSeed, prNum int, status Status) *TaskState {
		src := Source{Type: SourceTODO, URL: "u", ContentHash: HashContent(string(rune(hashSeed)))}
		return &TaskState{
			Hash:     HashTask(src),
			Source:   src,
			Status:   status,
			PRNumber: prNum,
		}
	}
	s.Upsert(mk(1, 0, StatusQueued))    // queued, no PR yet
	s.Upsert(mk(2, 100, StatusInFlight)) // PR open, in flight  ← expected
	s.Upsert(mk(3, 101, StatusDraft))   // draft is terminal
	s.Upsert(mk(4, 102, StatusShipped)) // shipped is terminal
	s.Upsert(mk(5, 103, StatusFailed))  // failed is terminal

	got := s.InFlight()
	if len(got) != 1 {
		t.Fatalf("expected 1 in-flight task, got %d: %+v", len(got), got)
	}
	if got[0].PRNumber != 100 {
		t.Errorf("expected PR 100, got %d", got[0].PRNumber)
	}
}

func TestStatus_IsTerminal(t *testing.T) {
	terminal := []Status{StatusShipped, StatusDraft, StatusBlocked, StatusFailed}
	for _, s := range terminal {
		if !s.IsTerminal() {
			t.Errorf("%q should be terminal", s)
		}
	}
	live := []Status{StatusQueued, StatusInFlight, StatusRequeued}
	for _, s := range live {
		if s.IsTerminal() {
			t.Errorf("%q should not be terminal", s)
		}
	}
}

// ---------------------------------------------------------------------------
// AcquireLock — second acquisition fails; release lets it succeed again
// ---------------------------------------------------------------------------

func TestAcquireLock_Contention(t *testing.T) {
	td := t.TempDir()
	path := filepath.Join(td, "run.lock")

	release1, err := AcquireLock(path)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}

	// Second acquire from the same process must fail.
	_, err2 := AcquireLock(path)
	if !errors.Is(err2, ErrLocked) {
		t.Fatalf("second acquire should return ErrLocked, got: %v", err2)
	}

	// After release, a fresh acquire should succeed.
	release1()
	release2, err := AcquireLock(path)
	if err != nil {
		t.Fatalf("re-acquire after release: %v", err)
	}
	release2()
}

func TestAcquireLock_ReleaseIsIdempotent(t *testing.T) {
	td := t.TempDir()
	path := filepath.Join(td, "run.lock")
	release, err := AcquireLock(path)
	if err != nil {
		t.Fatal(err)
	}
	release()
	release() // should not panic
}

// ---------------------------------------------------------------------------
// Concurrent Upsert is goroutine-safe (race detector would flag any issue)
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// MarkStale
// ---------------------------------------------------------------------------

func TestMarkStale_ExpiresOldInFlight(t *testing.T) {
	s := New()
	now := time.Now().UTC()
	src := Source{Type: SourceIssue, Repo: "x/y", URL: "u", ContentHash: "abc"}
	hash := HashTask(src)
	old := &TaskState{
		Hash:        hash,
		Source:      src,
		Status:      StatusInFlight,
		LastUpdated: now.Add(-13 * time.Hour), // 13 h ago — past the 12 h TTL
	}
	s.Tasks[hash] = old

	n := s.MarkStale(InFlightTTL, now)
	if n != 1 {
		t.Fatalf("expected 1 expiry, got %d", n)
	}
	got, _ := s.Get(hash)
	if got.Status != StatusRequeued {
		t.Errorf("stale task should become StatusRequeued, got %q", got.Status)
	}
	if !got.LastUpdated.Equal(now) {
		t.Errorf("LastUpdated should be refreshed to now")
	}
}

func TestMarkStale_SkipsRecentInFlight(t *testing.T) {
	s := New()
	now := time.Now().UTC()
	src := Source{Type: SourceIssue, Repo: "x/y", URL: "u", ContentHash: "abc"}
	hash := HashTask(src)
	s.Tasks[hash] = &TaskState{
		Hash:        hash,
		Source:      src,
		Status:      StatusInFlight,
		LastUpdated: now.Add(-1 * time.Hour), // 1 h ago — well within TTL
	}

	if n := s.MarkStale(InFlightTTL, now); n != 0 {
		t.Fatalf("recent in-flight task should not be expired, got count=%d", n)
	}
	got, _ := s.Get(hash)
	if got.Status != StatusInFlight {
		t.Errorf("recent task should remain StatusInFlight, got %q", got.Status)
	}
}

func TestMarkStale_OnlyAffectsInFlight(t *testing.T) {
	s := New()
	now := time.Now().UTC()
	old := now.Add(-24 * time.Hour) // definitely past any TTL
	statuses := []Status{StatusQueued, StatusShipped, StatusDraft, StatusBlocked, StatusFailed, StatusNoChange, StatusRequeued}
	for i, st := range statuses {
		src := Source{Type: SourceIssue, URL: "u", ContentHash: HashContent(string(rune(i + 1)))}
		hash := HashTask(src)
		s.Tasks[hash] = &TaskState{Hash: hash, Source: src, Status: st, LastUpdated: old}
	}

	if n := s.MarkStale(InFlightTTL, now); n != 0 {
		t.Errorf("MarkStale should only affect StatusInFlight, but expired %d other tasks", n)
	}
	// Verify no statuses were changed.
	for i, want := range statuses {
		src := Source{Type: SourceIssue, URL: "u", ContentHash: HashContent(string(rune(i + 1)))}
		got, _ := s.Get(HashTask(src))
		if got.Status != want {
			t.Errorf("status %q should be unchanged, got %q", want, got.Status)
		}
	}
}

func TestMarkStale_MixedBatch(t *testing.T) {
	s := New()
	now := time.Now().UTC()
	// Two stale in-flight, one fresh in-flight.
	for i, age := range []time.Duration{13 * time.Hour, 25 * time.Hour, 30 * time.Minute} {
		src := Source{Type: SourceIssue, URL: "u", ContentHash: HashContent(string(rune(i + 1)))}
		hash := HashTask(src)
		s.Tasks[hash] = &TaskState{Hash: hash, Source: src, Status: StatusInFlight, LastUpdated: now.Add(-age)}
	}

	if n := s.MarkStale(InFlightTTL, now); n != 2 {
		t.Fatalf("expected 2 expirations, got %d", n)
	}
}

func TestUpsert_ConcurrentSafe(t *testing.T) {
	s := New()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			src := Source{Type: SourceTODO, URL: "u", ContentHash: HashContent(string(rune(i)))}
			s.Upsert(&TaskState{Hash: HashTask(src), Source: src, Status: StatusQueued})
		}(i)
	}
	wg.Wait()
	if len(s.Tasks) != 50 {
		t.Errorf("expected 50 tasks after concurrent upsert, got %d", len(s.Tasks))
	}
}
