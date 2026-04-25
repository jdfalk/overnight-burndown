package auth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jdfalk/overnight-burndown/internal/config"
)

// generateTestKey creates a fresh RSA private key and writes it to a PEM file
// under t.TempDir(). Returns the path. Tests use this so we never commit a
// real key and never share keys between test runs.
func generateTestKey(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa keygen: %v", err)
	}
	der := x509.MarshalPKCS1PrivateKey(key)
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: der})
	path := filepath.Join(t.TempDir(), "key.pem")
	if err := os.WriteFile(path, pemBytes, 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	return path
}

// fakeGitHub returns an httptest.Server that pretends to be api.github.com.
// It implements just enough endpoints for the test:
//   - POST /app/installations/<id>/access_tokens — mints a stub token
//   - GET  /repos/<owner>/<name>                 — verifies the Authorization header
//
// The accessTokenCalls counter lets tests assert how many times the transport
// hit the token endpoint.
func fakeGitHub(t *testing.T, expectedAuthPrefix string) (*httptest.Server, *atomic.Int32, *atomic.Int32) {
	t.Helper()
	var tokenCalls atomic.Int32
	var apiCalls atomic.Int32

	mux := http.NewServeMux()

	// Token-mint endpoint: ghinstallation hits this with a Bearer JWT.
	mux.HandleFunc("/app/installations/", func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/access_tokens") {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, "method", http.StatusMethodNotAllowed)
			return
		}
		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, "Bearer ") {
			http.Error(w, "no bearer", http.StatusUnauthorized)
			return
		}
		tokenCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token":      "ghs_stubtoken_" + r.URL.Path,
			"expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
			"permissions": map[string]string{
				"contents":      "write",
				"pull_requests": "write",
			},
		})
	})

	// Sample API endpoint: must carry the installation token.
	mux.HandleFunc("/repos/", func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, expectedAuthPrefix) {
			t.Errorf("API call missing expected auth prefix %q, got %q", expectedAuthPrefix, auth)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		apiCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"name": "stub"}`))
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &tokenCalls, &apiCalls
}

// ---------------------------------------------------------------------------
// happy path: token exchange + signed API request
// ---------------------------------------------------------------------------

func TestNew_TokenExchangeAndSignedRequest(t *testing.T) {
	keyPath := generateTestKey(t)
	srv, tokenCalls, apiCalls := fakeGitHub(t, "token ")

	a, err := newWithBase(context.Background(), config.GitHubConfig{
		AppID:          12345,
		InstallationID: 67890,
		PrivateKeyPath: keyPath,
	}, srv.URL)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Initial mint should have happened during construction (eager validation).
	if got := tokenCalls.Load(); got != 1 {
		t.Errorf("expected 1 token mint during New, got %d", got)
	}

	// Make an API call — token should be reused, not re-minted.
	req, _ := http.NewRequest("GET", srv.URL+"/repos/jdfalk/audiobook-organizer", nil)
	resp, err := a.HTTPClient().Do(req)
	if err != nil {
		t.Fatalf("API call: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("API status: %d", resp.StatusCode)
	}
	if got := apiCalls.Load(); got != 1 {
		t.Errorf("expected 1 API call, got %d", got)
	}
	// The same token mint should still be cached by ghinstallation; no extra mint.
	if got := tokenCalls.Load(); got != 1 {
		t.Errorf("expected token to be cached after first mint, but got %d total mints", got)
	}
}

// ---------------------------------------------------------------------------
// InstallationToken returns the cached raw string
// ---------------------------------------------------------------------------

func TestInstallationToken_ReturnsRawString(t *testing.T) {
	keyPath := generateTestKey(t)
	srv, _, _ := fakeGitHub(t, "token ")

	a, err := newWithBase(context.Background(), config.GitHubConfig{
		AppID:          1,
		InstallationID: 2,
		PrivateKeyPath: keyPath,
	}, srv.URL)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	tok, err := a.InstallationToken(context.Background())
	if err != nil {
		t.Fatalf("InstallationToken: %v", err)
	}
	if !strings.HasPrefix(tok, "ghs_stubtoken_") {
		t.Errorf("token should be the stub from fakeGitHub, got %q", tok)
	}
}

// ---------------------------------------------------------------------------
// constructor validation
// ---------------------------------------------------------------------------

func TestNew_RejectsZeroAppID(t *testing.T) {
	_, err := New(context.Background(), config.GitHubConfig{
		AppID:          0,
		InstallationID: 1,
		PrivateKeyPath: "/tmp/nope",
	})
	if err == nil || !strings.Contains(err.Error(), "app_id") {
		t.Errorf("expected app_id error, got %v", err)
	}
}

func TestNew_RejectsZeroInstallationID(t *testing.T) {
	_, err := New(context.Background(), config.GitHubConfig{
		AppID:          1,
		InstallationID: 0,
		PrivateKeyPath: "/tmp/nope",
	})
	if err == nil || !strings.Contains(err.Error(), "installation_id") {
		t.Errorf("expected installation_id error, got %v", err)
	}
}

func TestNew_RejectsEmptyKeyPath(t *testing.T) {
	_, err := New(context.Background(), config.GitHubConfig{
		AppID:          1,
		InstallationID: 1,
		PrivateKeyPath: "",
	})
	if err == nil || !strings.Contains(err.Error(), "private_key_path") {
		t.Errorf("expected private_key_path error, got %v", err)
	}
}

func TestNew_RejectsBadKeyFile(t *testing.T) {
	td := t.TempDir()
	bogus := filepath.Join(td, "not-a-key.pem")
	if err := os.WriteFile(bogus, []byte("not pem at all"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := New(context.Background(), config.GitHubConfig{
		AppID:          1,
		InstallationID: 1,
		PrivateKeyPath: bogus,
	})
	if err == nil {
		t.Fatal("expected error from invalid PEM")
	}
}

// ---------------------------------------------------------------------------
// upstream rejection surfaces clearly during eager validation
// ---------------------------------------------------------------------------

func TestNew_FailsOnUpstreamRejection(t *testing.T) {
	keyPath := generateTestKey(t)

	// Server that always rejects token exchange. New() must fail eagerly so
	// misconfigured App credentials don't silently delay errors until the
	// first PR write.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message": "Bad credentials"}`, http.StatusUnauthorized)
	}))
	defer srv.Close()

	_, err := newWithBase(context.Background(), config.GitHubConfig{
		AppID:          1,
		InstallationID: 1,
		PrivateKeyPath: keyPath,
	}, srv.URL)
	if err == nil {
		t.Fatal("expected eager validation failure")
	}
	if !strings.Contains(err.Error(), "initial token exchange") {
		t.Errorf("error should mention initial token exchange: %v", err)
	}
}
