package claude

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
)

// TestClaudeOAuthUserAgentMatchesDeviceProfileFloor is the anti-drift guard for
// the deliberately duplicated Claude floor User-Agent. claudeOAuthUserAgent is a
// local copy of the serving device-profile floor
// (helps.defaultClaudeFingerprintUserAgent), duplicated only because importing the
// executor package here would create an import cycle. This test crosses that
// boundary from the safe direction (package claude already imports helps) and fails
// if the two floors ever diverge — e.g. the device-profile floor is modernized but
// the OAuth refresh/token path is left emitting a stale version.
func TestClaudeOAuthUserAgentMatchesDeviceProfileFloor(t *testing.T) {
	got, ok := helps.ClaudeVersionFromUserAgent(claudeOAuthUserAgent)
	if !ok {
		t.Fatalf("claudeOAuthUserAgent %q does not parse as a claude-cli User-Agent", claudeOAuthUserAgent)
	}
	want := helps.DefaultClaudeVersion(nil)
	if got != want {
		t.Fatalf("claudeOAuthUserAgent version = %q, device-profile floor version = %q; keep the two floors in lockstep", got, want)
	}
	if wantUA := "claude-cli/" + want + " (external, cli)"; claudeOAuthUserAgent != wantUA {
		t.Fatalf("claudeOAuthUserAgent = %q, want %q (reconstructed from the device-profile floor)", claudeOAuthUserAgent, wantUA)
	}
}

// TestNewClaudeAuthWithHTTPClientDefaultsToOAuthFloorUserAgent is the negative
// regression test for the "reauth UA degrades to Go-http-client/1.1" bug:
// NewClaudeAuthWithHTTPClient previously left userAgent empty, so refresh
// requests picked up net/http's default User-Agent instead of any
// claude-cli-shaped identity. It must now default to the claudeOAuthUserAgent
// floor so egress never falls back to the Go stdlib default.
func TestNewClaudeAuthWithHTTPClientDefaultsToOAuthFloorUserAgent(t *testing.T) {
	var captured string
	auth := NewClaudeAuthWithHTTPClient(&http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			captured = req.Header.Get("User-Agent")
			return okClaudeRefreshResponse(req, "floor-access", "floor-refresh"), nil
		}),
	})

	if _, err := auth.RefreshTokens(context.Background(), "ua-floor-refresh-token"); err != nil {
		t.Fatalf("refresh tokens: %v", err)
	}

	if captured == "" {
		t.Fatal("expected a non-empty outbound User-Agent")
	}
	if captured == "Go-http-client/1.1" {
		t.Fatalf("outbound User-Agent regressed to the Go net/http default: %q", captured)
	}
	if captured != claudeOAuthUserAgent {
		t.Fatalf("outbound User-Agent = %q, want the OAuth floor %q", captured, claudeOAuthUserAgent)
	}
}

// TestClaudeAuthWithUserAgentOverridesFloor verifies that WithUserAgent lets
// callers raise the OAuth request User-Agent to an account's persisted
// device-profile high-water mark, taking priority over the generic floor.
func TestClaudeAuthWithUserAgentOverridesFloor(t *testing.T) {
	const highWaterUA = "claude-cli/2.1.209 (external, cli)"

	var captured string
	auth := NewClaudeAuthWithHTTPClient(&http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			captured = req.Header.Get("User-Agent")
			return okClaudeRefreshResponse(req, "hw-access", "hw-refresh"), nil
		}),
	}).WithUserAgent(highWaterUA)

	if _, err := auth.RefreshTokens(context.Background(), "ua-highwater-refresh-token"); err != nil {
		t.Fatalf("refresh tokens: %v", err)
	}

	if captured != highWaterUA {
		t.Fatalf("outbound User-Agent = %q, want high-water override %q", captured, highWaterUA)
	}
}

// TestClaudeAuthWithUserAgentEmptyDoesNotOverrideFloor verifies that an
// empty (or whitespace-only) argument to WithUserAgent leaves the existing
// User-Agent value untouched, so callers that read a missing/blank
// high-water User-Agent can call WithUserAgent unconditionally without
// risking a downgrade to an empty User-Agent header.
func TestClaudeAuthWithUserAgentEmptyDoesNotOverrideFloor(t *testing.T) {
	auth := NewClaudeAuthWithHTTPClient(&http.Client{})

	if got := auth.userAgent; got != claudeOAuthUserAgent {
		t.Fatalf("precondition failed: userAgent = %q, want floor %q", got, claudeOAuthUserAgent)
	}

	auth.WithUserAgent("")
	if got := auth.userAgent; got != claudeOAuthUserAgent {
		t.Fatalf("WithUserAgent(\"\") overrode the floor: userAgent = %q, want %q", got, claudeOAuthUserAgent)
	}

	auth.WithUserAgent("   ")
	if got := auth.userAgent; got != claudeOAuthUserAgent {
		t.Fatalf("WithUserAgent(\"   \") overrode the floor: userAgent = %q, want %q", got, claudeOAuthUserAgent)
	}

	// Also verify at the wire level for good measure.
	var captured string
	auth = auth.WithUserAgent("")
	auth.httpClient = &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			captured = req.Header.Get("User-Agent")
			return okClaudeRefreshResponse(req, "blank-access", "blank-refresh"), nil
		}),
	}
	if _, err := auth.RefreshTokens(context.Background(), "ua-blank-refresh-token"); err != nil {
		t.Fatalf("refresh tokens: %v", err)
	}
	if captured != claudeOAuthUserAgent {
		t.Fatalf("outbound User-Agent = %q, want floor %q to survive an empty WithUserAgent call", captured, claudeOAuthUserAgent)
	}
}

// okClaudeRefreshResponse builds a minimal successful token-refresh HTTP
// response for the capturing RoundTripper tests above.
func okClaudeRefreshResponse(req *http.Request, accessToken, refreshToken string) *http.Response {
	body := `{
		"access_token":"` + accessToken + `",
		"refresh_token":"` + refreshToken + `",
		"token_type":"Bearer",
		"expires_in":3600,
		"account":{"email_address":"ua-test@example.com"}
	}`
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Request:    req,
	}
}
