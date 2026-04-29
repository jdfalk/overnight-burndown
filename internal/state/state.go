// Package state persists the burndown driver's cross-night queue and run lock.
//
// The state file (~/.burndown/state.json by default) holds one entry per task
// keyed by a stable hash of the source URL + content. Atomic writes ensure a
// crash mid-write cannot corrupt prior state — we write to a temp file under
// the same directory and rename().
//
// AcquireLock uses flock(LOCK_EX|LOCK_NB) so a crashed run automatically
// releases its lock when the kernel reaps the process. No staleness detection
// or PID tracking required.
package state

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// SchemaVersion is bumped any time the on-disk JSON shape changes
// incompatibly. Load refuses unknown versions to prevent a rolled-back driver
// from misreading a forward-rolled state file.
const SchemaVersion = 1

// Status is the lifecycle of a single task.
type Status string

const (
	StatusQueued    Status = "queued"
	StatusInFlight  Status = "in-flight"
	StatusShipped   Status = "shipped"
	StatusDraft     Status = "draft"
	StatusBlocked   Status = "blocked"
	StatusFailed    Status = "failed"
	StatusRequeued  Status = "requeued"
	// StatusNoChange marks a task where the agent ran successfully but
	// produced no file diff — nothing to commit, no PR. Distinct from
	// StatusShipped (which implies a merged PR exists) so the digest
	// can show the difference between "merged" and "agent did nothing".
	StatusNoChange Status = "no-change"
)

// IsTerminal reports whether the status represents a final outcome — i.e. the
// task should not be picked up again on the next night unless explicitly
// re-queued by recomputing sources.
func (s Status) IsTerminal() bool {
	switch s {
	case StatusShipped, StatusDraft, StatusBlocked, StatusFailed, StatusNoChange:
		return true
	}
	return false
}

// SourceType is the origin of a task.
type SourceType string

const (
	SourceIssue SourceType = "issue"
	SourceTODO  SourceType = "todo"
	SourcePlan  SourceType = "plan"
)

// Source identifies where a task came from. The hash() of (URL, ContentHash)
// is stable across runs so re-collecting the same task returns the same key.
type Source struct {
	Type        SourceType `json:"type"`
	Repo        string     `json:"repo"`        // "owner/name"
	URL         string     `json:"url"`         // canonical reference (issue URL, file path, etc.)
	ContentHash string     `json:"content_hash"` // sha256 of the task's textual content
	Title       string     `json:"title,omitempty"`
}

// TaskState is the per-task record persisted across runs.
type TaskState struct {
	Hash           string    `json:"hash"`
	Source         Source    `json:"source"`
	FirstSeen      time.Time `json:"first_seen"`
	LastSeen       time.Time `json:"last_seen"`
	Attempts       int       `json:"attempts"`
	Status         Status    `json:"status"`
	Classification string    `json:"classification,omitempty"`
	PRNumber       int       `json:"pr_number,omitempty"`
	PRURL          string    `json:"pr_url,omitempty"`
	Branch         string    `json:"branch,omitempty"`
	LastError      string    `json:"last_error,omitempty"`
	LastUpdated    time.Time `json:"last_updated"`
}

// State is the on-disk shape.
type State struct {
	SchemaVersion int                   `json:"schema_version"`
	LastRun       time.Time             `json:"last_run"`
	Tasks         map[string]*TaskState `json:"tasks"`

	mu sync.Mutex // guards Tasks for concurrent dispatch goroutines
}

// New returns an empty State at the current schema version.
func New() *State {
	return &State{
		SchemaVersion: SchemaVersion,
		Tasks:         make(map[string]*TaskState),
	}
}

// Load reads state from path. A missing file yields an empty State + nil err
// so first-night runs work without bootstrapping.
func Load(path string) (*State, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return New(), nil
	}
	if err != nil {
		return nil, fmt.Errorf("state: read %q: %w", path, err)
	}
	var s State
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("state: decode %q: %w", path, err)
	}
	if s.SchemaVersion == 0 {
		s.SchemaVersion = SchemaVersion // tolerate pre-versioned files
	}
	if s.SchemaVersion != SchemaVersion {
		return nil, fmt.Errorf("state: unsupported schema_version=%d (this build expects %d)",
			s.SchemaVersion, SchemaVersion)
	}
	if s.Tasks == nil {
		s.Tasks = make(map[string]*TaskState)
	}
	return &s, nil
}

// Save writes state to path atomically: write to a tempfile in the same
// directory, fsync, then rename. A crash mid-write cannot corrupt the prior
// state.
func (s *State) Save(path string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.SchemaVersion == 0 {
		s.SchemaVersion = SchemaVersion
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("state: marshal: %w", err)
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("state: mkdir %q: %w", dir, err)
	}

	tmp, err := os.CreateTemp(dir, ".state-*.json")
	if err != nil {
		return fmt.Errorf("state: temp file: %w", err)
	}
	tmpName := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpName)
		}
	}()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("state: write tmp: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("state: fsync tmp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("state: close tmp: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("state: rename: %w", err)
	}
	cleanup = false
	return nil
}

// LoadDir reads per-task state files from <dir>/tasks/*.json into a single
// State. Missing dir or missing tasks/ subdir yield an empty State + nil err
// (first-night runs, or a freshly-cleaned state directory).
//
// Per-task layout — one JSON file per task hash — is the durability layer
// for the matrix workflow: each dispatch cell can SaveTask its own row
// independently without contention, artifact upload preserves the per-file
// shape, and corrupting / losing one file doesn't poison the catalog.
//
// As a migration affordance: if <dir>/state.json exists (the legacy
// monolithic shape), it's read first and merged in. The next SaveDir call
// will write per-task files; the legacy state.json is then renamed to
// state.json.migrated so a re-load doesn't double-count.
func LoadDir(dir string) (*State, error) {
	s := New()

	// Legacy monolithic file. Read if present; the per-task files (read
	// next) win on overlap because they're the post-migration shape.
	legacyPath := filepath.Join(dir, "state.json")
	if _, err := os.Stat(legacyPath); err == nil {
		legacy, err := Load(legacyPath)
		if err != nil {
			return nil, fmt.Errorf("state: load legacy %q: %w", legacyPath, err)
		}
		for k, v := range legacy.Tasks {
			s.Tasks[k] = v
		}
		s.LastRun = legacy.LastRun
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("state: stat %q: %w", legacyPath, err)
	}

	tasksDir := filepath.Join(dir, "tasks")
	entries, err := os.ReadDir(tasksDir)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, fmt.Errorf("state: read tasks dir %q: %w", tasksDir, err)
	}
	for _, ent := range entries {
		if ent.IsDir() || !strings.HasSuffix(ent.Name(), ".json") {
			continue
		}
		p := filepath.Join(tasksDir, ent.Name())
		data, err := os.ReadFile(p)
		if err != nil {
			return nil, fmt.Errorf("state: read %q: %w", p, err)
		}
		var t TaskState
		if err := json.Unmarshal(data, &t); err != nil {
			return nil, fmt.Errorf("state: decode %q: %w", p, err)
		}
		if t.Hash == "" {
			// Filename-as-hash fallback: <hash>.json
			t.Hash = strings.TrimSuffix(ent.Name(), ".json")
		}
		s.Tasks[t.Hash] = &t
	}
	return s, nil
}

// SaveDir writes one JSON file per task to <dir>/tasks/<hash>.json, plus a
// small <dir>/meta.json with the schema version and last-run timestamp.
// Per-task writes are atomic (tempfile + rename), independent of each
// other, and idempotent.
//
// On successful write, a legacy <dir>/state.json (if any) is renamed to
// state.json.migrated to prevent a re-load from double-counting after the
// migration.
func (s *State) SaveDir(dir string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.SchemaVersion == 0 {
		s.SchemaVersion = SchemaVersion
	}
	tasksDir := filepath.Join(dir, "tasks")
	if err := os.MkdirAll(tasksDir, 0o755); err != nil {
		return fmt.Errorf("state: mkdir %q: %w", tasksDir, err)
	}

	for hash, t := range s.Tasks {
		if hash == "" {
			continue
		}
		if t.Hash == "" {
			t.Hash = hash
		}
		if err := writeTaskFile(tasksDir, t); err != nil {
			return err
		}
	}

	meta := struct {
		SchemaVersion int       `json:"schema_version"`
		LastRun       time.Time `json:"last_run"`
	}{SchemaVersion: s.SchemaVersion, LastRun: s.LastRun}
	if err := writeJSONAtomic(filepath.Join(dir, "meta.json"), meta); err != nil {
		return err
	}

	// Sweep the legacy monolithic file out of the way once we've written
	// the new shape successfully. Idempotent on subsequent runs (rename
	// of a non-existent file is a no-op below).
	legacy := filepath.Join(dir, "state.json")
	if _, err := os.Stat(legacy); err == nil {
		_ = os.Rename(legacy, legacy+".migrated")
	}
	return nil
}

func writeTaskFile(tasksDir string, t *TaskState) error {
	if t.Hash == "" {
		return errors.New("state: writeTaskFile: empty hash")
	}
	return writeJSONAtomic(filepath.Join(tasksDir, t.Hash+".json"), t)
}

func writeJSONAtomic(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("state: marshal %q: %w", path, err)
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".task-*.json")
	if err != nil {
		return fmt.Errorf("state: temp file %q: %w", dir, err)
	}
	tmpName := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpName)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("state: write tmp %q: %w", tmpName, err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("state: fsync tmp %q: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("state: close tmp %q: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("state: rename %q→%q: %w", tmpName, path, err)
	}
	cleanup = false
	return nil
}

// SaveTask writes a single task's file. Used by matrix dispatch cells to
// publish their own outcome row without lock contention against siblings.
// Caller is responsible for the surrounding mutex if the in-memory map is
// also being mutated; the file write itself is atomic.
func (s *State) SaveTask(dir, hash string) error {
	s.mu.Lock()
	t, ok := s.Tasks[hash]
	s.mu.Unlock()
	if !ok {
		return fmt.Errorf("state: SaveTask: unknown hash %s", hash)
	}
	tasksDir := filepath.Join(dir, "tasks")
	if err := os.MkdirAll(tasksDir, 0o755); err != nil {
		return fmt.Errorf("state: mkdir %q: %w", tasksDir, err)
	}
	return writeTaskFile(tasksDir, t)
}

// HashTask returns a stable hash usable as a TaskState key. Two collections
// of the same source (same URL + same content) produce the same hash.
func HashTask(src Source) string {
	h := sha256.New()
	io.WriteString(h, string(src.Type))
	h.Write([]byte{0})
	io.WriteString(h, src.Repo)
	h.Write([]byte{0})
	io.WriteString(h, src.URL)
	h.Write([]byte{0})
	io.WriteString(h, src.ContentHash)
	return hex.EncodeToString(h.Sum(nil))
}

// HashContent is a convenience helper for callers building a Source.
func HashContent(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

// Upsert inserts or updates a task. FirstSeen is preserved; LastSeen and
// LastUpdated are bumped to now. Caller-set Status, Classification, etc. are
// taken as-is — if you want to preserve a status, fetch first then mutate.
func (s *State) Upsert(t *TaskState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Tasks == nil {
		s.Tasks = make(map[string]*TaskState)
	}
	now := time.Now().UTC()
	t.LastSeen = now
	t.LastUpdated = now
	if existing, ok := s.Tasks[t.Hash]; ok {
		t.FirstSeen = existing.FirstSeen
	} else if t.FirstSeen.IsZero() {
		t.FirstSeen = now
	}
	s.Tasks[t.Hash] = t
}

// Get returns the task with the given hash and whether it exists.
func (s *State) Get(hash string) (*TaskState, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.Tasks[hash]
	return t, ok
}

// InFlight returns tasks that have an open PR (PRNumber > 0) and have not yet
// reached a terminal status. Used by the hybrid-resume path: at the start of
// a new run we resume these and recompute the rest of the queue from sources.
func (s *State) InFlight() []*TaskState {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*TaskState
	for _, t := range s.Tasks {
		if t.PRNumber > 0 && !t.Status.IsTerminal() {
			out = append(out, t)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Hash < out[j].Hash })
	return out
}

// AcquireLock takes an exclusive flock on path. The returned release func
// drops the lock and closes the file; if the process crashes without calling
// it, the kernel releases the lock automatically. ErrLocked is returned if
// another holder has it.
//
// The lock file is created if missing but never deleted — repeated lock /
// release cycles re-use the same inode so flock semantics stay consistent.
func AcquireLock(path string) (release func(), err error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("state: mkdir lock dir: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("state: open lock file %q: %w", path, err)
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, ErrLocked
		}
		return nil, fmt.Errorf("state: flock %q: %w", path, err)
	}
	released := false
	return func() {
		if released {
			return
		}
		released = true
		_ = unix.Flock(int(f.Fd()), unix.LOCK_UN)
		_ = f.Close()
	}, nil
}

// ErrLocked is returned by AcquireLock when another process holds the lock.
var ErrLocked = errors.New("state: another burndown run is active")
