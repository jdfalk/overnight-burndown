// Package auth wraps GitHub App installation auth.
//
// The burndown driver authenticates as a GitHub App (jdfalk-burndown-bot)
// rather than as a personal user. The flow is:
//
//  1. Sign a short-lived JWT with the App's private key (proves we are the App).
//  2. Exchange the JWT at /app/installations/<id>/access_tokens for an
//     installation token scoped to a single installation (= our owner).
//  3. Use that installation token as Bearer auth on every API request.
//
// `bradleyfalzon/ghinstallation/v2.Transport` does steps 1–3 transparently:
// it wraps an `http.RoundTripper`, mints a JWT, exchanges it on first use
// (and on token expiry, which is roughly hourly), and injects the resulting
// token into outgoing requests.
//
// The Auth struct here is a thin facade: it owns the transport, exposes a
// pre-configured `*http.Client`, and offers `InstallationToken(ctx)` for the
// rare case (e.g. handing a token to `git` over HTTPS for push) where you
// need the raw string instead of the wrapped client.
package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/bradleyfalzon/ghinstallation/v2"

	"github.com/jdfalk/overnight-burndown/internal/config"
)

// DefaultGitHubAPI is the canonical github.com REST endpoint. Tests override
// it to point at an httptest server.
const DefaultGitHubAPI = "https://api.github.com"

// Auth holds a ready-to-use HTTP client that auto-refreshes its installation
// token. Construction validates the App credentials by performing the first
// token exchange eagerly so misconfigurations surface at startup, not under
// the first PR write.
type Auth struct {
	transport *ghinstallation.Transport
	client    *http.Client
}

// New builds an Auth from a config.GitHubConfig.
//
// It reads the private key from cfg.PrivateKeyPath, configures the transport,
// and performs an initial token exchange against the configured GitHub API
// (defaults to github.com). A failure here means the App ID, installation ID,
// or key are wrong — we fail fast.
func New(ctx context.Context, cfg config.GitHubConfig) (*Auth, error) {
	return newWithBase(ctx, cfg, DefaultGitHubAPI)
}

// newWithBase is the test-friendly variant: it lets the test inject a stub
// base URL pointing at an httptest server. Kept unexported because callers
// outside tests should never need to override this.
func newWithBase(ctx context.Context, cfg config.GitHubConfig, baseURL string) (*Auth, error) {
	if cfg.AppID == 0 {
		return nil, errors.New("auth: github.app_id is required")
	}
	if cfg.InstallationID == 0 {
		return nil, errors.New("auth: github.installation_id is required")
	}
	if cfg.PrivateKeyPath == "" {
		return nil, errors.New("auth: github.private_key_path is required")
	}

	tr, err := ghinstallation.NewKeyFromFile(
		http.DefaultTransport,
		cfg.AppID,
		cfg.InstallationID,
		cfg.PrivateKeyPath,
	)
	if err != nil {
		return nil, fmt.Errorf("auth: load private key: %w", err)
	}
	tr.BaseURL = baseURL

	a := &Auth{
		transport: tr,
		client:    &http.Client{Transport: tr},
	}

	// Eager validation: minting the token round-trips against GitHub. If the
	// App ID / installation ID / private key triplet is wrong, this fails now
	// with a clear error rather than later inside an opaque PR-create call.
	if _, err := a.InstallationToken(ctx); err != nil {
		return nil, fmt.Errorf("auth: initial token exchange failed: %w", err)
	}
	return a, nil
}

// HTTPClient returns the configured http.Client. Every request made through
// it carries a fresh installation token; callers do not have to refresh
// anything themselves.
func (a *Auth) HTTPClient() *http.Client {
	return a.client
}

// InstallationToken returns the current installation token as a raw string.
// Useful for handing off to non-Go consumers (git over HTTPS, gh CLI). The
// returned token is short-lived (~1 hour); callers should fetch fresh on
// every use rather than caching.
func (a *Auth) InstallationToken(ctx context.Context) (string, error) {
	tok, err := a.transport.Token(ctx)
	if err != nil {
		return "", fmt.Errorf("auth: mint installation token: %w", err)
	}
	return tok, nil
}
