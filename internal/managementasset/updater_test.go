package managementasset

import (
	"errors"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestAutoUpdateSkipReason(t *testing.T) {
	tests := []struct {
		name       string
		cfg        *config.Config
		wantReason string
		wantSkip   bool
	}{
		{
			name:       "nil config",
			cfg:        nil,
			wantReason: "config not yet available",
			wantSkip:   true,
		},
		{
			name: "cluster mode",
			cfg: &config.Config{
				Home: config.HomeConfig{Enabled: true},
			},
			wantReason: "cluster mode enabled",
			wantSkip:   true,
		},
		{
			name: "control panel disabled",
			cfg: &config.Config{
				RemoteManagement: config.RemoteManagement{DisableControlPanel: true},
			},
			wantReason: "control panel disabled",
			wantSkip:   true,
		},
		{
			name: "auto update disabled",
			cfg: &config.Config{
				RemoteManagement: config.RemoteManagement{DisableAutoUpdatePanel: true},
			},
			wantReason: "disable-auto-update-panel is enabled",
			wantSkip:   true,
		},
		{
			name:       "enabled",
			cfg:        &config.Config{},
			wantReason: "",
			wantSkip:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotReason, gotSkip := autoUpdateSkipReason(tt.cfg)
			if gotReason != tt.wantReason || gotSkip != tt.wantSkip {
				t.Fatalf("autoUpdateSkipReason() = (%q, %t), want (%q, %t)", gotReason, gotSkip, tt.wantReason, tt.wantSkip)
			}
		})
	}
}

func TestFetchLatestAssetOmitsAuthorizationWithoutToken(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("github_token", "")
	t.Setenv("GITSTORE_GIT_TOKEN", "")
	t.Setenv("GITSTORE_GIT_URL", "")

	var authorization string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		authorization = req.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"assets":[{"name":"management.html","browser_download_url":"https://example.com/management.html","digest":"sha256:abc123"}]}`))
	}))
	defer server.Close()

	asset, remoteHash, err := fetchLatestAsset(t.Context(), server.Client(), server.URL)
	if err != nil {
		t.Fatalf("fetchLatestAsset() error = %v", err)
	}
	if authorization != "" {
		t.Fatalf("Authorization = %q, want empty", authorization)
	}
	if asset == nil || asset.Name != managementAssetName {
		t.Fatalf("asset = %#v, want %q", asset, managementAssetName)
	}
	if remoteHash != "abc123" {
		t.Fatalf("remoteHash = %q, want %q", remoteHash, "abc123")
	}
}

func TestFetchLatestAssetSetsGitHubAuthorization(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "asset-token")
	t.Setenv("GITSTORE_GIT_TOKEN", "")
	t.Setenv("GITSTORE_GIT_URL", "")

	var authorization string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		authorization = req.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"assets":[{"name":"management.html","browser_download_url":"https://example.com/management.html","digest":"sha256:abc123"}]}`))
	}))
	defer server.Close()

	asset, remoteHash, err := fetchLatestAsset(t.Context(), server.Client(), server.URL)
	if err != nil {
		t.Fatalf("fetchLatestAsset() error = %v", err)
	}
	if authorization != "" {
		t.Fatal("token leaked to non-GitHub release URL")
	}
	client := &http.Client{Transport: githubAssetTransport(func(req *http.Request) (*http.Response, error) {
		if req.Header.Get("Authorization") != "Bearer asset-token" {
			t.Fatal("GitHub API token missing")
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"assets":[{"name":"management.html","digest":"sha256:abc123"}]}`))}, nil
	})}
	if _, _, errGitHub := fetchLatestAsset(t.Context(), client, "https://api.github.com/repos/owner/repo/releases/latest"); errGitHub != nil {
		t.Fatal(errGitHub)
	}
	if asset == nil || asset.Name != managementAssetName {
		t.Fatalf("asset = %#v, want %q", asset, managementAssetName)
	}
	if remoteHash != "abc123" {
		t.Fatalf("remoteHash = %q, want %q", remoteHash, "abc123")
	}
}

func (f githubAssetTransport) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

type githubAssetTransport func(*http.Request) (*http.Response, error)

func TestParseRateLimitReset(t *testing.T) {
	got := parseRateLimitReset("1700000600")
	want := time.Unix(1_700_000_600, 0)
	if !got.Equal(want) {
		t.Fatalf("expected %v, got %v", want, got)
	}
}

func TestParseRetryAfter(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)

	if got := parseRetryAfter("120", now); got != 120*time.Second {
		t.Fatalf("expected 120s, got %v", got)
	}

	httpDate := now.Add(90 * time.Second).UTC().Format(http.TimeFormat)
	if got := parseRetryAfter(httpDate, now); got != 90*time.Second {
		t.Fatalf("expected 90s from HTTP date, got %v", got)
	}
}

func TestReleaseCooldownFromErrorFallsBackToDefault(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)

	until, ok := releaseCooldownFromError(now, &releaseRequestError{
		StatusCode: http.StatusForbidden,
		Body:       "API rate limit exceeded",
	})
	if !ok {
		t.Fatalf("expected rate-limit cooldown")
	}

	want := now.Add(releaseRateLimitCooldown)
	if !until.Equal(want) {
		t.Fatalf("expected cooldown until %v, got %v", want, until)
	}
}

func TestReleaseCooldownFromErrorIgnoresNonRateLimitErrors(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)

	if _, ok := releaseCooldownFromError(now, errors.New("boom")); ok {
		t.Fatalf("unexpected cooldown for generic error")
	}
	if _, ok := releaseCooldownFromError(now, &releaseRequestError{
		StatusCode: http.StatusInternalServerError,
		Body:       "server exploded",
	}); ok {
		t.Fatalf("unexpected cooldown for non-rate-limit status")
	}
}

func TestReleaseCooldownFromErrorPrefersRateLimitReset(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	reset := now.Add(10 * time.Minute)

	until, ok := releaseCooldownFromError(now, &releaseRequestError{
		StatusCode:     http.StatusForbidden,
		Body:           "API rate limit exceeded",
		RateLimitReset: reset,
	})
	if !ok {
		t.Fatalf("expected rate-limit cooldown")
	}

	want := reset.Add(5 * time.Second)
	if !until.Equal(want) {
		t.Fatalf("expected cooldown until %v, got %v", want, until)
	}
}

func TestReleaseCooldownFromErrorPrefersRetryAfter(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)

	until, ok := releaseCooldownFromError(now, &releaseRequestError{
		StatusCode: http.StatusTooManyRequests,
		Body:       "too many requests",
		RetryAfter: 2 * time.Minute,
	})
	if !ok {
		t.Fatalf("expected rate-limit cooldown")
	}

	want := now.Add(2 * time.Minute)
	if !until.Equal(want) {
		t.Fatalf("expected cooldown until %v, got %v", want, until)
	}
}
