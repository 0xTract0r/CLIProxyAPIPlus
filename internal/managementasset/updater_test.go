package managementasset

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

type githubAssetTransport func(*http.Request) (*http.Response, error)

func (f githubAssetTransport) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestFetchLatestAssetSetsGitHubAuthorization(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "asset-token")
	t.Setenv("GITSTORE_GIT_TOKEN", "")
	t.Setenv("GITSTORE_GIT_URL", "")

	var authorization string
	clientLocal := &http.Client{Transport: githubAssetTransport(func(req *http.Request) (*http.Response, error) {
		authorization = req.Header.Get("Authorization")
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"assets":[{"name":"management.html","browser_download_url":"https://example.com/management.html","digest":"sha256:abc123"}]}`))}, nil
	})}
	asset, remoteHash, err := fetchLatestAsset(t.Context(), clientLocal, "https://example.com/release")
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

func TestFetchLatestAssetOmitsAuthorizationWithoutToken(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("github_token", "")
	t.Setenv("GITSTORE_GIT_TOKEN", "")
	t.Setenv("GITSTORE_GIT_URL", "")

	var authorization string
	clientLocal := &http.Client{Transport: githubAssetTransport(func(req *http.Request) (*http.Response, error) {
		authorization = req.Header.Get("Authorization")
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"assets":[{"name":"management.html","browser_download_url":"https://example.com/management.html","digest":"sha256:abc123"}]}`))}, nil
	})}
	asset, remoteHash, err := fetchLatestAsset(t.Context(), clientLocal, "https://example.com/release")
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

func TestParseRateLimitReset(t *testing.T) {
	got := parseRateLimitReset("1700000600")
	want := time.Unix(1_700_000_600, 0)
	if !got.Equal(want) {
		t.Fatalf("expected %v, got %v", want, got)
	}
}

func TestFetchLatestAssetReleaseResponse(t *testing.T) {
	for _, tc := range []struct {
		name         string
		code         int
		body         string
		wantCooldown bool
		wantAsset    bool
	}{
		{"success", http.StatusCreated, `{"assets":[{"name":"management.html"}]}`, false, true},
		{"invalid JSON suffix", http.StatusOK, `{"assets":[{"name":"management.html"}]} broken`, false, false},
		{"retry after", http.StatusTooManyRequests, "limited", true, false},
		{"not found", http.StatusNotFound, "missing", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := &http.Client{Transport: githubAssetTransport(func(req *http.Request) (*http.Response, error) {
				h := http.Header{}
				if tc.wantCooldown {
					h.Set("Retry-After", "60")
				}
				return &http.Response{StatusCode: tc.code, Header: h, Body: io.NopCloser(strings.NewReader(tc.body))}, nil
			})}
			got, _, err := fetchLatestAsset(t.Context(), client, "https://example.com/release")
			if (err == nil) != tc.wantAsset || (got != nil) != tc.wantAsset {
				t.Fatalf("asset=%v err=%v", got, err)
			}
			now := time.Unix(1700000000, 0)
			until, blocked := releaseCooldownFromError(now, err)
			if blocked != tc.wantCooldown || (blocked && !until.Equal(now.Add(time.Minute))) {
				t.Fatalf("cooldown=%v until=%v", blocked, until)
			}
		})
	}
}

func TestPanelReleaseURLUsesConfiguredRepository(t *testing.T) {
	const custom = "https://api.github.com/repos/example/local-panel/releases/latest"
	for _, tc := range []struct{ repository, want string }{
		{"", defaultManagementReleaseURL},
		{"https://github.com/example/local-panel.git", custom},
		{"https://api.github.com/repos/example/local-panel", custom},
		{custom, custom},
	} {
		if got := resolveReleaseURL(tc.repository); got != tc.want {
			t.Errorf("resolveReleaseURL(%q) = %q, want %q", tc.repository, got, tc.want)
		}
	}
}
