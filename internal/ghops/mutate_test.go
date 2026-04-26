package ghops

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/go-github/v84/github"
)

// fakeMutateAPI is the parallel of fakeWatchAPI for mutate.go.
type fakeMutateAPI struct {
	prHandler         http.HandlerFunc
	graphqlHandler    http.HandlerFunc
	addLabelsHandler  http.HandlerFunc
	commentsHandler   http.HandlerFunc

	graphqlBody atomic.Pointer[string]
}

func (f *fakeMutateAPI) start(t *testing.T, ownerName string) (*github.Client, func()) {
	t.Helper()
	mux := http.NewServeMux()

	mux.HandleFunc("/repos/"+ownerName+"/pulls/", func(w http.ResponseWriter, r *http.Request) {
		if f.prHandler != nil {
			f.prHandler(w, r)
			return
		}
		http.NotFound(w, r)
	})
	mux.HandleFunc("/repos/"+ownerName+"/issues/", func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/labels"):
			if f.addLabelsHandler != nil {
				f.addLabelsHandler(w, r)
				return
			}
		case strings.HasSuffix(r.URL.Path, "/comments"):
			if f.commentsHandler != nil {
				f.commentsHandler(w, r)
				return
			}
		}
		http.NotFound(w, r)
	})
	mux.HandleFunc("/graphql", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		s := string(body)
		f.graphqlBody.Store(&s)
		if f.graphqlHandler != nil {
			f.graphqlHandler(w, r)
			return
		}
		writeJSON(w, map[string]any{"data": map[string]any{}})
	})

	srv := httptest.NewServer(mux)
	client := github.NewClient(nil)
	u, _ := url.Parse(srv.URL + "/")
	client.BaseURL = u
	return client, srv.Close
}

// ---------------------------------------------------------------------------
// AutoMerge → GraphQL enablePullRequestAutoMerge
// ---------------------------------------------------------------------------

func TestAutoMerge_SendsCorrectGraphQL(t *testing.T) {
	api := &fakeMutateAPI{
		prHandler: func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, map[string]any{
				"number":  42,
				"node_id": "PR_kwDO123",
			})
		},
	}
	client, cleanup := api.start(t, "jdfalk/x")
	defer cleanup()

	pub := &Publisher{GitHub: client, Owner: "jdfalk", Name: "x"}
	if err := pub.AutoMerge(context.Background(), 42); err != nil {
		t.Fatalf("AutoMerge: %v", err)
	}
	bodyPtr := api.graphqlBody.Load()
	if bodyPtr == nil {
		t.Fatal("graphql endpoint never hit")
	}
	body := *bodyPtr
	if !strings.Contains(body, "enablePullRequestAutoMerge") {
		t.Errorf("expected enablePullRequestAutoMerge in body: %s", body)
	}
	if !strings.Contains(body, "REBASE") {
		t.Errorf("expected REBASE merge method (matches repo policy): %s", body)
	}
	if !strings.Contains(body, `"id":"PR_kwDO123"`) {
		t.Errorf("expected PR node_id in variables: %s", body)
	}
}

func TestAutoMerge_PropagatesGraphQLError(t *testing.T) {
	api := &fakeMutateAPI{
		prHandler: func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, map[string]any{"number": 1, "node_id": "PR_x"})
		},
		graphqlHandler: func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, map[string]any{"errors": []any{
				map[string]any{"message": "auto-merge not enabled on this repo", "type": "FORBIDDEN"},
			}})
		},
	}
	client, cleanup := api.start(t, "jdfalk/x")
	defer cleanup()

	pub := &Publisher{GitHub: client, Owner: "jdfalk", Name: "x"}
	err := pub.AutoMerge(context.Background(), 1)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "auto-merge not enabled") {
		t.Errorf("error should surface graphql message: %v", err)
	}
}

// ---------------------------------------------------------------------------
// ConvertToDraft → GraphQL convertPullRequestToDraft
// ---------------------------------------------------------------------------

func TestConvertToDraft_SendsCorrectGraphQL(t *testing.T) {
	api := &fakeMutateAPI{
		prHandler: func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, map[string]any{"number": 5, "node_id": "PR_5"})
		},
	}
	client, cleanup := api.start(t, "jdfalk/x")
	defer cleanup()

	pub := &Publisher{GitHub: client, Owner: "jdfalk", Name: "x"}
	if err := pub.ConvertToDraft(context.Background(), 5); err != nil {
		t.Fatalf("ConvertToDraft: %v", err)
	}
	body := *api.graphqlBody.Load()
	if !strings.Contains(body, "convertPullRequestToDraft") {
		t.Errorf("expected convertPullRequestToDraft mutation: %s", body)
	}
	if !strings.Contains(body, `"id":"PR_5"`) {
		t.Errorf("expected node_id PR_5: %s", body)
	}
}

// ---------------------------------------------------------------------------
// AddLabel → POST /issues/{n}/labels
// ---------------------------------------------------------------------------

func TestAddLabel_PostsLabelsArray(t *testing.T) {
	var captured atomic.Pointer[[]string]
	api := &fakeMutateAPI{
		// go-github sends the labels as a bare JSON array body, not
		// wrapped in an object — match the wire format exactly.
		addLabelsHandler: func(w http.ResponseWriter, r *http.Request) {
			var labels []string
			_ = json.NewDecoder(r.Body).Decode(&labels)
			captured.Store(&labels)
			writeJSON(w, []any{map[string]any{"name": "burndown-failed"}})
		},
	}
	client, cleanup := api.start(t, "jdfalk/x")
	defer cleanup()

	pub := &Publisher{GitHub: client, Owner: "jdfalk", Name: "x"}
	if err := pub.AddLabel(context.Background(), 7, "burndown-failed"); err != nil {
		t.Fatalf("AddLabel: %v", err)
	}
	got := *captured.Load()
	if len(got) != 1 || got[0] != "burndown-failed" {
		t.Errorf("expected labels=[burndown-failed], got %v", got)
	}
}

func TestAddLabel_RejectsEmpty(t *testing.T) {
	pub := &Publisher{GitHub: github.NewClient(nil), Owner: "x", Name: "y"}
	if err := pub.AddLabel(context.Background(), 1, ""); err == nil {
		t.Fatal("expected validation error")
	}
}

// ---------------------------------------------------------------------------
// CommentOnPR → POST /issues/{n}/comments
// ---------------------------------------------------------------------------

func TestCommentOnPR_PostsBody(t *testing.T) {
	var captured atomic.Pointer[string]
	api := &fakeMutateAPI{
		commentsHandler: func(w http.ResponseWriter, r *http.Request) {
			body := map[string]string{}
			_ = json.NewDecoder(r.Body).Decode(&body)
			b := body["body"]
			captured.Store(&b)
			writeJSON(w, map[string]any{"id": 1})
		},
	}
	client, cleanup := api.start(t, "jdfalk/x")
	defer cleanup()

	pub := &Publisher{GitHub: client, Owner: "jdfalk", Name: "x"}
	const msg = "auto-merge gates failed: CI status is failure"
	if err := pub.CommentOnPR(context.Background(), 7, msg); err != nil {
		t.Fatalf("CommentOnPR: %v", err)
	}
	got := *captured.Load()
	if got != msg {
		t.Errorf("body forwarded wrong: got %q want %q", got, msg)
	}
}

func TestCommentOnPR_RejectsEmpty(t *testing.T) {
	pub := &Publisher{GitHub: github.NewClient(nil), Owner: "x", Name: "y"}
	if err := pub.CommentOnPR(context.Background(), 1, ""); err == nil {
		t.Fatal("expected validation error")
	}
}
