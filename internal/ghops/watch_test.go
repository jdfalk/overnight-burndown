package ghops

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-github/v84/github"
)

// fakeWatchAPI returns a github.Client wired to an httptest server that
// fakes the three endpoints WatchCI hits. The handlers vary per test;
// we expose pluggable funcs.
type fakeWatchAPI struct {
	prHandler            http.HandlerFunc
	checkRunsHandler     http.HandlerFunc
	combinedStatusHandler http.HandlerFunc
	listFilesHandler     http.HandlerFunc

	prGetCount     atomic.Int32
	checkRunsCount atomic.Int32
}

func (f *fakeWatchAPI) start(t *testing.T, ownerName string) (*github.Client, func()) {
	t.Helper()
	mux := http.NewServeMux()

	mux.HandleFunc("/repos/"+ownerName+"/pulls/", func(w http.ResponseWriter, r *http.Request) {
		// `/repos/owner/name/pulls/N` (Get) or `/repos/owner/name/pulls/N/files`
		if strings.HasSuffix(r.URL.Path, "/files") {
			if f.listFilesHandler != nil {
				f.listFilesHandler(w, r)
				return
			}
			http.NotFound(w, r)
			return
		}
		f.prGetCount.Add(1)
		if f.prHandler != nil {
			f.prHandler(w, r)
			return
		}
		http.NotFound(w, r)
	})
	mux.HandleFunc("/repos/"+ownerName+"/commits/", func(w http.ResponseWriter, r *http.Request) {
		// /commits/<sha>/check-runs OR /commits/<sha>/status
		switch {
		case strings.HasSuffix(r.URL.Path, "/check-runs"):
			f.checkRunsCount.Add(1)
			if f.checkRunsHandler != nil {
				f.checkRunsHandler(w, r)
				return
			}
		case strings.HasSuffix(r.URL.Path, "/status"):
			if f.combinedStatusHandler != nil {
				f.combinedStatusHandler(w, r)
				return
			}
		}
		http.NotFound(w, r)
	})

	srv := httptest.NewServer(mux)
	client := github.NewClient(nil)
	u, _ := url.Parse(srv.URL + "/")
	client.BaseURL = u
	return client, srv.Close
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// ---------------------------------------------------------------------------
// WatchCI — happy path: pending → success
// ---------------------------------------------------------------------------

func TestWatchCI_TransitionsToSuccess(t *testing.T) {
	api := &fakeWatchAPI{
		prHandler: func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, map[string]any{
				"number": 7,
				"head":   map[string]any{"sha": "deadbeef"},
			})
		},
	}

	// Two polls: first returns pending, second returns success.
	api.checkRunsHandler = func(w http.ResponseWriter, r *http.Request) {
		switch api.checkRunsCount.Load() {
		case 1:
			writeJSON(w, map[string]any{"check_runs": []any{
				map[string]any{"status": "in_progress"},
			}})
		default:
			writeJSON(w, map[string]any{"check_runs": []any{
				map[string]any{"status": "completed", "conclusion": "success"},
			}})
		}
	}
	api.combinedStatusHandler = func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"state": "success"})
	}

	client, cleanup := api.start(t, "jdfalk/x")
	defer cleanup()

	pub := &Publisher{GitHub: client, Owner: "jdfalk", Name: "x"}
	status, err := pub.WatchCI(context.Background(), 7, WatchOptions{
		Timeout:      2 * time.Second,
		PollInterval: 5 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("WatchCI: %v", err)
	}
	if status != CISuccess {
		t.Errorf("status: got %q want CISuccess", status)
	}
	if api.checkRunsCount.Load() < 2 {
		t.Errorf("expected at least 2 polls, got %d", api.checkRunsCount.Load())
	}
}

// ---------------------------------------------------------------------------
// WatchCI — failure short-circuits without further polling
// ---------------------------------------------------------------------------

func TestWatchCI_FailureSeenImmediately(t *testing.T) {
	api := &fakeWatchAPI{
		prHandler: func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, map[string]any{"number": 1, "head": map[string]any{"sha": "abc"}})
		},
		checkRunsHandler: func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, map[string]any{"check_runs": []any{
				map[string]any{"status": "completed", "conclusion": "failure"},
			}})
		},
		combinedStatusHandler: func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, map[string]any{"state": "success"})
		},
	}
	client, cleanup := api.start(t, "jdfalk/x")
	defer cleanup()

	pub := &Publisher{GitHub: client, Owner: "jdfalk", Name: "x"}
	status, err := pub.WatchCI(context.Background(), 1, WatchOptions{
		Timeout:      2 * time.Second,
		PollInterval: 5 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("WatchCI: %v", err)
	}
	if status != CIFailure {
		t.Errorf("status: got %q want CIFailure", status)
	}
}

// ---------------------------------------------------------------------------
// WatchCI — timeout returns CIPending without error
// ---------------------------------------------------------------------------

func TestWatchCI_TimeoutReturnsPending(t *testing.T) {
	api := &fakeWatchAPI{
		prHandler: func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, map[string]any{"number": 1, "head": map[string]any{"sha": "abc"}})
		},
		checkRunsHandler: func(w http.ResponseWriter, _ *http.Request) {
			// Always pending.
			writeJSON(w, map[string]any{"check_runs": []any{
				map[string]any{"status": "in_progress"},
			}})
		},
		combinedStatusHandler: func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, map[string]any{"state": "pending"})
		},
	}
	client, cleanup := api.start(t, "jdfalk/x")
	defer cleanup()

	pub := &Publisher{GitHub: client, Owner: "jdfalk", Name: "x"}
	status, err := pub.WatchCI(context.Background(), 1, WatchOptions{
		Timeout:      40 * time.Millisecond,
		PollInterval: 5 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("WatchCI: %v (timeout is not an error condition)", err)
	}
	if status != CIPending {
		t.Errorf("status: got %q want CIPending", status)
	}
}

// ---------------------------------------------------------------------------
// WatchCI — combined status FAILURE wins over check-runs SUCCESS
// ---------------------------------------------------------------------------

func TestWatchCI_LegacyStatusFailureWins(t *testing.T) {
	api := &fakeWatchAPI{
		prHandler: func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, map[string]any{"number": 1, "head": map[string]any{"sha": "abc"}})
		},
		checkRunsHandler: func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, map[string]any{"check_runs": []any{
				map[string]any{"status": "completed", "conclusion": "success"},
			}})
		},
		combinedStatusHandler: func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, map[string]any{"state": "failure"})
		},
	}
	client, cleanup := api.start(t, "jdfalk/x")
	defer cleanup()

	pub := &Publisher{GitHub: client, Owner: "jdfalk", Name: "x"}
	status, err := pub.WatchCI(context.Background(), 1, WatchOptions{
		Timeout: 100 * time.Millisecond, PollInterval: 5 * time.Millisecond,
	})
	if err != nil || status != CIFailure {
		t.Errorf("expected CIFailure (legacy status), got status=%q err=%v", status, err)
	}
}

// ---------------------------------------------------------------------------
// ListChangedFiles — happy path with pagination
// ---------------------------------------------------------------------------

func TestListChangedFiles_Paginated(t *testing.T) {
	api := &fakeWatchAPI{
		listFilesHandler: func(w http.ResponseWriter, r *http.Request) {
			page := r.URL.Query().Get("page")
			if page == "" || page == "1" {
				w.Header().Set("Link", `<http://stub/?page=2>; rel="next"`)
				writeJSON(w, []any{
					map[string]any{"filename": "a.md", "additions": 1, "deletions": 0},
					map[string]any{"filename": "b.go", "additions": 5, "deletions": 2},
				})
				return
			}
			// page 2 — no Link header; pagination ends.
			writeJSON(w, []any{
				map[string]any{"filename": "c.txt", "additions": 0, "deletions": 0},
			})
		},
	}
	client, cleanup := api.start(t, "jdfalk/x")
	defer cleanup()

	pub := &Publisher{GitHub: client, Owner: "jdfalk", Name: "x"}
	files, err := pub.ListChangedFiles(context.Background(), 7)
	if err != nil {
		t.Fatalf("ListChangedFiles: %v", err)
	}
	if len(files) != 3 {
		t.Errorf("expected 3 files (paginated), got %d: %+v", len(files), files)
	}
	if files[0].Path != "a.md" || files[1].Additions != 5 {
		t.Errorf("unexpected fields: %+v", files)
	}
	if total := TotalLinesChanged(files); total != 8 {
		t.Errorf("TotalLinesChanged: got %d want 8", total)
	}
}
