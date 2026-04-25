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
)

// IsTerminal reports whether the status represents a final outcome — i.e. the
// task should not be picked up again on the next night unless explicitly
// re-queued by recomputing sources.
func (s Status) IsTerminal() bool {
	switch s {
	case StatusShipped, StatusDraft, StatusBlocked, StatusFailed:
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
