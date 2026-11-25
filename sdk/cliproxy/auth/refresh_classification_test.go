package auth

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// TestTerminalRefreshAuthError covers the refresh error classification used to
// decide whether a credential must be re-authenticated (terminal) or may keep
// retrying (transient). The production incident error
// `invalid_grant: Refresh token not found or invalid` must be terminal.
func TestTerminalRefreshAuthError(t *testing.T) {
	cases := []struct {
		name     string
		err      error
		wantTerm bool
		wantCode string
	}{
		{
			name:     "nil error is not terminal",
			err:      nil,
			wantTerm: false,
		},
		{
			name:     "transient network error",
			err:      errors.New("token refresh failed: dial tcp 1.2.3.4:443: connect: connection refused"),
			wantTerm: false,
		},
		{
			name:     "transient 503",
			err:      errors.New(`token refresh failed: status=503 body_preview="upstream temporarily unavailable"`),
			wantTerm: false,
		},
		{
			name:     "reuse keeps dedicated code",
			err:      errors.New(`{"error":"invalid_grant","error_description":"Refresh token has already been used"}`),
			wantTerm: true,
			wantCode: "refresh_token_reused",
		},
		{
			name:     "claude invalid_grant refresh token not found or invalid",
			err:      errors.New(`token refresh failed: status=400 body_preview="{\"error\":\"invalid_grant\",\"error_description\":\"Refresh token not found or invalid\"}"`),
			wantTerm: true,
			wantCode: "invalid_grant",
		},
		{
			name:     "invalid-grant with hyphen normalized",
			err:      errors.New(`oauth error: invalid-grant (refresh token revoked)`),
			wantTerm: true,
			wantCode: "invalid_grant",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, terminal := terminalRefreshAuthError(tc.err)
			if terminal != tc.wantTerm {
				t.Fatalf("terminal = %v, want %v (err=%v)", terminal, tc.wantTerm, tc.err)
			}
			if terminal && code != tc.wantCode {
				t.Fatalf("code = %q, want %q", code, tc.wantCode)
			}
			if IsTerminalRefreshAuthError(tc.err) != tc.wantTerm {
				t.Fatalf("IsTerminalRefreshAuthError = %v, want %v", !tc.wantTerm, tc.wantTerm)
			}
		})
	}
}

// TestClassifyTerminalRefreshFailure covers the #164 diagnostic classification
// label derived from the already-sanitized terminal refresh error code: it
// must distinguish the RFC 9700 §4.14 rotation-replay/race signal
// (refresh_token_reused) from a bare RFC 6749 §5.2 invalid_grant (ambiguous
// expired/revoked), and fall back safely for any future/unknown code.
func TestClassifyTerminalRefreshFailure(t *testing.T) {
	cases := []struct {
		code string
		want string
	}{
		{code: "refresh_token_reused", want: classConcurrentReuseRace},
		{code: "invalid_grant", want: classExpiredOrRevokedGeneric},
		{code: "", want: classUnknownTerminal},
		{code: "some_future_code", want: classUnknownTerminal},
	}
	for _, tc := range cases {
		t.Run(tc.code, func(t *testing.T) {
			if got := classifyTerminalRefreshFailure(tc.code); got != tc.want {
				t.Fatalf("classifyTerminalRefreshFailure(%q) = %q, want %q", tc.code, got, tc.want)
			}
		})
	}
}

// TestClassifyTerminalRefreshFailure_EndToEndFromError exercises the full
// path a real refresh error takes: terminalRefreshAuthError first derives the
// sanitized code, then classifyTerminalRefreshFailure labels it. This is the
// "given invalid_grant error -> classified as revoked/expired" acceptance
// case called out in the task, plus its reuse-race counterpart.
func TestClassifyTerminalRefreshFailure_EndToEndFromError(t *testing.T) {
	cases := []struct {
		name         string
		err          error
		wantClass    string
		wantTerminal bool
	}{
		{
			name:         "invalid_grant classified as expired_or_revoked_generic",
			err:          errors.New(`token refresh failed: status=400 body_preview="{\"error\":\"invalid_grant\",\"error_description\":\"Refresh token not found or invalid\"}"`),
			wantClass:    classExpiredOrRevokedGeneric,
			wantTerminal: true,
		},
		{
			name:         "refresh token reuse classified as concurrent_reuse_race",
			err:          errors.New(`{"error":"invalid_grant","error_description":"Refresh token has already been used"}`),
			wantClass:    classConcurrentReuseRace,
			wantTerminal: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, terminal := terminalRefreshAuthError(tc.err)
			if terminal != tc.wantTerminal {
				t.Fatalf("terminal = %v, want %v", terminal, tc.wantTerminal)
			}
			if got := classifyTerminalRefreshFailure(code); got != tc.wantClass {
				t.Fatalf("classifyTerminalRefreshFailure(%q) = %q, want %q", code, got, tc.wantClass)
			}
		})
	}
}

// TestCredentialFingerprint verifies the #164 diagnostic fingerprint helper:
// it must never return the plaintext secret, must be deterministic for the
// same input (so the same physical token correlates across restarts), must
// differ for different inputs, and must return "" for an empty secret.
func TestCredentialFingerprint(t *testing.T) {
	const secret = "super-secret-refresh-token-value"
	fp := credentialFingerprint(secret)
	if fp == "" {
		t.Fatal("credentialFingerprint(secret) = \"\", want non-empty")
	}
	if strings.Contains(fp, secret) {
		t.Fatalf("credentialFingerprint leaked plaintext: %q", fp)
	}
	if len(fp) != 16 {
		t.Fatalf("credentialFingerprint length = %d, want 16 (truncated sha256 hex)", len(fp))
	}
	if got := credentialFingerprint(secret); got != fp {
		t.Fatalf("credentialFingerprint not deterministic: %q vs %q", got, fp)
	}
	if other := credentialFingerprint("a-different-secret"); other == fp {
		t.Fatalf("credentialFingerprint collided for different secrets: %q", fp)
	}
	if empty := credentialFingerprint(""); empty != "" {
		t.Fatalf("credentialFingerprint(\"\") = %q, want \"\"", empty)
	}
}

// TestRefreshTokenFingerprintFromMetadata verifies the metadata-reading
// wrapper never returns the raw token and degrades to "" when metadata is
// missing/malformed rather than panicking.
func TestRefreshTokenFingerprintFromMetadata(t *testing.T) {
	meta := map[string]any{"refresh_token": "plaintext-token-abc"}
	fp := refreshTokenFingerprintFromMetadata(meta)
	if fp == "" || strings.Contains(fp, "plaintext-token-abc") {
		t.Fatalf("refreshTokenFingerprintFromMetadata = %q, want non-empty fingerprint without plaintext", fp)
	}
	if got := refreshTokenFingerprintFromMetadata(nil); got != "" {
		t.Fatalf("refreshTokenFingerprintFromMetadata(nil) = %q, want \"\"", got)
	}
	if got := refreshTokenFingerprintFromMetadata(map[string]any{}); got != "" {
		t.Fatalf("refreshTokenFingerprintFromMetadata(empty) = %q, want \"\"", got)
	}
	if got := refreshTokenFingerprintFromMetadata(map[string]any{"refresh_token": 12345}); got != "" {
		t.Fatalf("refreshTokenFingerprintFromMetadata(non-string) = %q, want \"\"", got)
	}
}

// TestProcessInstanceID verifies the #164 diagnostic instance identifier is
// non-empty and stable within the process (so repeated log lines from the
// same instance correlate), and looks like "host:pid".
func TestProcessInstanceID(t *testing.T) {
	first := processInstanceID()
	if first == "" {
		t.Fatal("processInstanceID() = \"\", want non-empty")
	}
	if !strings.Contains(first, ":") {
		t.Fatalf("processInstanceID() = %q, want host:pid shape", first)
	}
	if second := processInstanceID(); second != first {
		t.Fatalf("processInstanceID() not stable: %q vs %q", first, second)
	}
}

// TestMarkRefreshReauthRequiredWithReason verifies the persisted terminal state
// carries the supplied code and a sanitized message that never echoes a raw
// provider body or token.
func TestMarkRefreshReauthRequiredWithReason(t *testing.T) {
	a := &Auth{ID: "claude-1", Provider: "claude", Metadata: map[string]any{"refresh_token": "secret-token"}}
	a.markRefreshReauthRequiredWithReason(time.Unix(1_700_000_000, 0), "invalid_grant")

	if a.Status != StatusError || a.StatusMessage != "reauth_required" {
		t.Fatalf("status = %q/%q, want error/reauth_required", a.Status, a.StatusMessage)
	}
	if !a.RefreshDisabled() {
		t.Fatal("RefreshDisabled() = false, want true")
	}
	if got, _ := a.Metadata["refresh_error_code"].(string); got != "invalid_grant" {
		t.Fatalf("refresh_error_code = %q, want invalid_grant", got)
	}
	if a.LastError == nil || a.LastError.Code != "reauth_required" || a.LastError.Retryable {
		t.Fatalf("LastError = %+v, want non-retryable reauth_required", a.LastError)
	}
	msg, _ := a.Metadata["last_refresh_error"].(string)
	if msg == "" || strings.Contains(msg, "secret-token") || strings.Contains(strings.ToLower(msg), "body_preview") {
		t.Fatalf("last_refresh_error = %q, want sanitized message without token/raw body", msg)
	}
	if !a.NextRefreshAfter.IsZero() {
		t.Fatalf("NextRefreshAfter = %v, want zero (no further auto-retry)", a.NextRefreshAfter)
	}
}

// TestPreserveNewerTokenOwnedFields verifies the stale write-back guard: a clone
// carrying older token state must not roll back newer stored tokens, while its
// non-token metadata still applies. A same-or-newer incoming record (real
// refresh / re-auth) is left untouched.
func TestPreserveNewerTokenOwnedFields(t *testing.T) {
	newExpiry := "2026-06-03T12:00:00Z"
	oldExpiry := "2026-06-03T04:35:48Z"

	t.Run("stale clone does not roll back tokens", func(t *testing.T) {
		existing := &Auth{
			ID:       "claude-1",
			Provider: "claude",
			Metadata: map[string]any{
				"refresh_token": "NEW_REFRESH",
				"access_token":  "NEW_ACCESS",
				"expired":       newExpiry,
			},
		}
		incoming := &Auth{
			ID:       "claude-1",
			Provider: "claude",
			Metadata: map[string]any{
				"refresh_token":            "OLD_REFRESH",
				"access_token":             "OLD_ACCESS",
				"expired":                  oldExpiry,
				"quota_refresh_status":     "ok",
				"quota_next_refresh_after": "2026-06-03T13:00:00Z",
			},
		}
		if !preserveNewerTokenOwnedFields(incoming, existing) {
			t.Fatal("preserveNewerTokenOwnedFields = false, want true (stale token detected)")
		}
		if got, _ := incoming.Metadata["refresh_token"].(string); got != "NEW_REFRESH" {
			t.Fatalf("refresh_token = %q, want NEW_REFRESH (must not roll back)", got)
		}
		if got, _ := incoming.Metadata["access_token"].(string); got != "NEW_ACCESS" {
			t.Fatalf("access_token = %q, want NEW_ACCESS", got)
		}
		if got, _ := incoming.Metadata["expired"].(string); got != newExpiry {
			t.Fatalf("expired = %q, want %q", got, newExpiry)
		}
		// Non-token metadata still applies.
		if got, _ := incoming.Metadata["quota_refresh_status"].(string); got != "ok" {
			t.Fatalf("quota_refresh_status = %q, want ok (non-token write must apply)", got)
		}
	})

	t.Run("real refresh is not blocked", func(t *testing.T) {
		existing := &Auth{
			ID:       "claude-1",
			Provider: "claude",
			Metadata: map[string]any{"refresh_token": "OLD_REFRESH", "expired": oldExpiry},
		}
		incoming := &Auth{
			ID:       "claude-1",
			Provider: "claude",
			Metadata: map[string]any{"refresh_token": "ROTATED", "expired": newExpiry},
		}
		if preserveNewerTokenOwnedFields(incoming, existing) {
			t.Fatal("preserveNewerTokenOwnedFields = true, want false (incoming is newer)")
		}
		if got, _ := incoming.Metadata["refresh_token"].(string); got != "ROTATED" {
			t.Fatalf("refresh_token = %q, want ROTATED (refresh must win)", got)
		}
	})
}

// TestManagerUpdate_DoesNotRollBackTokensFromStaleWriteback exercises the guard
// through the real Manager.Update path: a quota-style metadata write built from a
// stale clone must not overwrite tokens that a refresh advanced in the meantime.
func TestManagerUpdate_DoesNotRollBackTokensFromStaleWriteback(t *testing.T) {
	ctx := context.Background()
	store := &captureStore{}
	manager := NewManager(store, nil, nil)

	// Stored credential already advanced by a refresh.
	current := &Auth{
		ID:       "claude-stale",
		Provider: "claude",
		Metadata: map[string]any{
			"refresh_token": "NEW_REFRESH",
			"access_token":  "NEW_ACCESS",
			"expired":       "2026-06-03T12:00:00Z",
		},
	}
	if _, err := manager.Register(ctx, current); err != nil {
		t.Fatalf("register: %v", err)
	}

	// A stale clone (older tokens) writes unrelated quota metadata.
	stale := &Auth{
		ID:       "claude-stale",
		Provider: "claude",
		Metadata: map[string]any{
			"refresh_token":        "OLD_REFRESH",
			"access_token":         "OLD_ACCESS",
			"expired":              "2026-06-03T04:35:48Z",
			"quota_refresh_status": "ok",
		},
	}
	if _, err := manager.Update(ctx, stale); err != nil {
		t.Fatalf("update: %v", err)
	}

	got, ok := manager.GetByID("claude-stale")
	if !ok || got == nil {
		t.Fatal("auth missing after update")
	}
	if rt, _ := got.Metadata["refresh_token"].(string); rt != "NEW_REFRESH" {
		t.Fatalf("refresh_token = %q, want NEW_REFRESH (stale write must not roll back)", rt)
	}
	if at, _ := got.Metadata["access_token"].(string); at != "NEW_ACCESS" {
		t.Fatalf("access_token = %q, want NEW_ACCESS", at)
	}
	if qs, _ := got.Metadata["quota_refresh_status"].(string); qs != "ok" {
		t.Fatalf("quota_refresh_status = %q, want ok (non-token write must apply)", qs)
	}
}
