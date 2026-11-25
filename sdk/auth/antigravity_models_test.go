package auth

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestNormalizeAntigravityModelID(t *testing.T) {
	cases := []struct {
		input    string
		expected string
	}{
		{"gemini-3-flash", "gemini-3-flash"},
		{"  GEMINI-3-FLASH  ", "gemini-3-flash"},
		{"", ""},
		{"   ", ""},
		{strings.Repeat("a", 256), strings.Repeat("a", 256)},
		{strings.Repeat("a", 257), ""},
	}
	for _, c := range cases {
		got := NormalizeAntigravityModelID(c.input)
		if got != c.expected {
			t.Errorf("NormalizeAntigravityModelID(%q) = %q, want %q", c.input, got, c.expected)
		}
	}
}

func TestParseAntigravityModelCapabilityHints(t *testing.T) {
	t.Run("valid payload with models and web search", func(t *testing.T) {
		body := []byte(`{"models":{"gemini-3-flash":{"displayName":"Gemini 3 Flash"},"claude-sonnet-5-5-medium":{"displayName":"Claude Sonnet 5.5"}},"webSearchModelIds":["gemini-3-flash"]}`)
		hints, ok := ParseAntigravityModelCapabilityHints(body)
		if !ok {
			t.Fatal("expected ok to be true")
		}
		if hints.ModelIDs == nil {
			t.Fatal("expected non-nil ModelIDs")
		}
		if len(hints.ModelIDs) != 2 {
			t.Errorf("got %d models, want 2", len(hints.ModelIDs))
		}
		if _, exists := hints.ModelIDs["gemini-3-flash"]; !exists {
			t.Errorf("missing gemini-3-flash in ModelIDs")
		}
		if len(hints.WebSearchModelIDs) != 1 {
			t.Errorf("got %d webSearchModelIds, want 1", len(hints.WebSearchModelIDs))
		}
		if _, exists := hints.WebSearchModelIDs["gemini-3-flash"]; !exists {
			t.Errorf("missing gemini-3-flash in WebSearchModelIDs")
		}
	})

	t.Run("empty catalog is authoritative empty", func(t *testing.T) {
		body := []byte(`{"models":{}}`)
		hints, ok := ParseAntigravityModelCapabilityHints(body)
		if !ok {
			t.Fatal("expected ok to be true")
		}
		if hints.ModelIDs == nil {
			t.Fatal("expected non-nil empty ModelIDs")
		}
		if len(hints.ModelIDs) != 0 {
			t.Errorf("expected 0 models, got %d", len(hints.ModelIDs))
		}
	})

	t.Run("missing models field leaves ModelIDs nil", func(t *testing.T) {
		body := []byte(`{"webSearchModelIds":["gemini-3.5-flash-lite"]}`)
		hints, ok := ParseAntigravityModelCapabilityHints(body)
		if !ok {
			t.Fatal("expected ok to be true")
		}
		if hints.ModelIDs != nil {
			t.Fatal("expected nil ModelIDs for unknown catalog")
		}
		if len(hints.WebSearchModelIDs) != 1 {
			t.Errorf("got %d webSearchModelIds, want 1", len(hints.WebSearchModelIDs))
		}
	})

	t.Run("invalid json returns false", func(t *testing.T) {
		_, ok := ParseAntigravityModelCapabilityHints([]byte(`{not valid json`))
		if ok {
			t.Fatal("expected false for invalid JSON")
		}
	})

	t.Run("clones model IDs out of caller buffer", func(t *testing.T) {
		buf := []byte(`{"models":{"gemini-3-flash":{}},"webSearchModelIds":["gemini-3-flash"]}`)
		hints, ok := ParseAntigravityModelCapabilityHints(buf)
		if !ok {
			t.Fatal("expected ok to be true")
		}
		for i := range buf {
			buf[i] = 'x'
		}
		if _, exists := hints.ModelIDs["gemini-3-flash"]; !exists {
			t.Fatal("expected gemini-3-flash in ModelIDs after buffer mutation")
		}
		if _, exists := hints.WebSearchModelIDs["gemini-3-flash"]; !exists {
			t.Fatal("expected gemini-3-flash in WebSearchModelIDs after buffer mutation")
		}
	})
}

func TestPersistedAntigravityCapabilityHintsRoundTrip(t *testing.T) {
	updatedAt := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	expiresAt := updatedAt.Add(3 * time.Hour)
	userAgent := "antigravity/hub/2.34.0 (aidev_client; os_type=darwin; arch=arm64; cl=1026637980)"
	proxyWithCreds := "http://proxy-user:proxy-secret@proxy.example.com:8080"
	routeFingerprint := AntigravityCapabilityRouteFingerprint(proxyWithCreds, userAgent)
	if routeFingerprint == "" {
		t.Fatal("expected non-empty route fingerprint")
	}
	if strings.Contains(routeFingerprint, "proxy-user") || strings.Contains(routeFingerprint, "proxy-secret") || strings.Contains(routeFingerprint, "proxy.example.com") {
		t.Fatalf("route fingerprint leaked proxy details: %q", routeFingerprint)
	}
	if AntigravityCapabilityRouteFingerprint("", userAgent) == routeFingerprint {
		t.Fatal("expected different route fingerprints for direct vs proxy routes")
	}
	if AntigravityCapabilityRouteFingerprint("http://other-proxy:8080", userAgent) == routeFingerprint {
		t.Fatal("expected different route fingerprints for distinct proxy URLs")
	}
	if AntigravityCapabilityRouteFingerprint(proxyWithCreds, "antigravity/hub/2.35.0") == routeFingerprint {
		t.Fatal("expected different route fingerprints for distinct user agents")
	}
	if gotEmpty := AntigravityCapabilityRouteFingerprint(proxyWithCreds, "   "); gotEmpty != "" {
		t.Fatalf("expected empty fingerprint for empty user agent, got %q", gotEmpty)
	}

	hints := AntigravityModelCapabilityHints{
		ModelIDs: map[string]struct{}{
			"gemini-3-flash": {},
			"gemini-4-argon": {},
		},
		WebSearchModelIDs: map[string]struct{}{
			"gemini-3-flash": {},
		},
		UpdatedAt:        updatedAt,
		ExpiresAt:        expiresAt,
		RouteFingerprint: routeFingerprint,
	}
	cloned := hints.Clone()
	if !cloned.UpdatedAt.Equal(updatedAt) || !cloned.ExpiresAt.Equal(expiresAt) || cloned.RouteFingerprint != routeFingerprint {
		t.Fatalf("Clone() did not preserve freshness metadata: %+v", cloned)
	}

	project := "aicode-consumers"
	endpointWithUser := "https://user:password@cloudcode-pa.googleapis.com"
	expectedEndpoint := "https://cloudcode-pa.googleapis.com"

	serialized := BuildPersistedAntigravityCapabilityHints(project, endpointWithUser, hints)
	if ep, ok := serialized["endpoint"].(string); !ok || ep != expectedEndpoint {
		t.Errorf("expected endpoint %q, got %q", expectedEndpoint, ep)
	}
	if gotFP, ok := serialized["route_fingerprint"].(string); !ok || gotFP != routeFingerprint {
		t.Errorf("expected route_fingerprint %q, got %q", routeFingerprint, gotFP)
	}
	if gotUpdated, ok := serialized["updated_at"].(string); !ok || gotUpdated != updatedAt.Format(time.RFC3339) {
		t.Errorf("expected updated_at %q, got %q", updatedAt.Format(time.RFC3339), gotUpdated)
	}
	if gotExpires, ok := serialized["expires_at"].(string); !ok || gotExpires != expiresAt.Format(time.RFC3339) {
		t.Errorf("expected expires_at %q, got %q", expiresAt.Format(time.RFC3339), gotExpires)
	}

	metadata := map[string]any{
		AntigravityPersistedHintsMetaKey: serialized,
	}

	parsed, ok := ParsePersistedAntigravityCapabilityHints(metadata, project, expectedEndpoint)
	if !ok {
		t.Fatal("expected parse to succeed")
	}
	if len(parsed.ModelIDs) != 2 {
		t.Errorf("got %d models, want 2", len(parsed.ModelIDs))
	}
	if len(parsed.WebSearchModelIDs) != 1 {
		t.Errorf("got %d web search models, want 1", len(parsed.WebSearchModelIDs))
	}
	if !parsed.UpdatedAt.Equal(updatedAt) {
		t.Errorf("parsed.UpdatedAt = %v, want %v", parsed.UpdatedAt, updatedAt)
	}
	if !parsed.ExpiresAt.Equal(expiresAt) {
		t.Errorf("parsed.ExpiresAt = %v, want %v", parsed.ExpiresAt, expiresAt)
	}
	if parsed.RouteFingerprint != routeFingerprint {
		t.Errorf("parsed.RouteFingerprint = %q, want %q", parsed.RouteFingerprint, routeFingerprint)
	}

	// Legacy persisted entries without freshness fields remain valid as stale fallback.
	legacyMetadata := map[string]any{
		AntigravityPersistedHintsMetaKey: map[string]any{
			"project_id": project,
			"endpoint":   expectedEndpoint,
			"model_ids":  []any{"gemini-3-flash"},
		},
	}
	legacyParsed, okLegacy := ParsePersistedAntigravityCapabilityHints(legacyMetadata, project, expectedEndpoint)
	if !okLegacy {
		t.Fatal("expected legacy persisted metadata without timestamps to parse")
	}
	if !legacyParsed.UpdatedAt.IsZero() || !legacyParsed.ExpiresAt.IsZero() || legacyParsed.RouteFingerprint != "" {
		t.Errorf("expected zero freshness fields for legacy metadata, got %+v", legacyParsed)
	}

	// Malformed timestamp fields degrade to zero time without discarding valid model_ids.
	malformedMetadata := map[string]any{
		AntigravityPersistedHintsMetaKey: map[string]any{
			"project_id": project,
			"endpoint":   expectedEndpoint,
			"model_ids":  []any{"gemini-3-flash"},
			"updated_at": "not-a-timestamp",
			"expires_at": 12345,
		},
	}
	malformedParsed, okMalformed := ParsePersistedAntigravityCapabilityHints(malformedMetadata, project, expectedEndpoint)
	if !okMalformed {
		t.Fatal("expected persisted metadata with malformed timestamps to keep model catalog")
	}
	if !malformedParsed.UpdatedAt.IsZero() || !malformedParsed.ExpiresAt.IsZero() {
		t.Errorf("expected malformed timestamps to parse as zero time, got %+v", malformedParsed)
	}

	// Mismatched project
	if _, okMismatch := ParsePersistedAntigravityCapabilityHints(metadata, "other-project", expectedEndpoint); okMismatch {
		t.Error("expected mismatch for different project")
	}

	// Mismatched endpoint
	if _, okMismatch := ParsePersistedAntigravityCapabilityHints(metadata, project, "https://other.com"); okMismatch {
		t.Error("expected mismatch for different endpoint")
	}

	// Nil ModelIDs (unknown catalog) must not serialize as authoritative empty catalog
	if gotNil := BuildPersistedAntigravityCapabilityHints(project, expectedEndpoint, AntigravityModelCapabilityHints{}); gotNil != nil {
		t.Errorf("expected nil persisted hints for unknown catalog, got %v", gotNil)
	}
}

func TestFetchAntigravityAvailableModels(t *testing.T) {
	t.Run("successful probe", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost {
				t.Errorf("expected POST, got %s", r.Method)
			}
			if r.Header.Get("Authorization") != "Bearer test-token" {
				t.Errorf("unexpected authorization header: %s", r.Header.Get("Authorization"))
			}
			if r.Header.Get("Content-Type") != "application/json" {
				t.Errorf("unexpected content-type: %s", r.Header.Get("Content-Type"))
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"models":{"gemini-3-flash":{}},"webSearchModelIds":["gemini-3-flash"]}`))
		}))
		defer server.Close()

		hints, status := FetchAntigravityAvailableModels(context.Background(), AntigravityModelProbeRequest{
			BaseURLs:    []string{server.URL},
			AccessToken: "test-token",
			ProjectID:   "aicode-consumers",
		})
		if status != AntigravityProbeStatusSuccess {
			t.Fatalf("expected success, got %v", status)
		}
		if len(hints.ModelIDs) != 1 {
			t.Errorf("got %d models, want 1", len(hints.ModelIDs))
		}
	})

	t.Run("auth error 401", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
		}))
		defer server.Close()

		_, status := FetchAntigravityAvailableModels(context.Background(), AntigravityModelProbeRequest{
			BaseURLs:    []string{server.URL},
			AccessToken: "bad-token",
		})
		if status != AntigravityProbeStatusAuthError {
			t.Fatalf("expected auth error, got %v", status)
		}
	})

	t.Run("forbidden 403 distinct from 401 auth error", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusForbidden)
		}))
		defer server.Close()

		_, status := FetchAntigravityAvailableModels(context.Background(), AntigravityModelProbeRequest{
			BaseURLs:    []string{server.URL},
			AccessToken: "forbidden-token",
		})
		if status != AntigravityProbeStatusForbidden {
			t.Fatalf("expected forbidden status, got %v", status)
		}
	})

	t.Run("empty base urls returns transient error", func(t *testing.T) {
		_, status := FetchAntigravityAvailableModels(context.Background(), AntigravityModelProbeRequest{
			BaseURLs:    nil,
			AccessToken: "token",
		})
		if status != AntigravityProbeStatusTransientError {
			t.Fatalf("expected transient error, got %v", status)
		}
	})

	t.Run("empty access token returns auth error", func(t *testing.T) {
		_, status := FetchAntigravityAvailableModels(context.Background(), AntigravityModelProbeRequest{
			BaseURLs:    []string{"https://example.com"},
			AccessToken: "",
		})
		if status != AntigravityProbeStatusAuthError {
			t.Fatalf("expected auth error, got %v", status)
		}
	})

	t.Run("redirect is not followed", func(t *testing.T) {
		redirectTargetVisited := false
		targetServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			redirectTargetVisited = true
			w.WriteHeader(http.StatusOK)
		}))
		defer targetServer.Close()

		sourceServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, targetServer.URL, http.StatusFound)
		}))
		defer sourceServer.Close()

		_, status := FetchAntigravityAvailableModels(context.Background(), AntigravityModelProbeRequest{
			BaseURLs:    []string{sourceServer.URL},
			AccessToken: "token",
		})
		if redirectTargetVisited {
			t.Fatal("redirect target should not have been visited")
		}
		if status != AntigravityProbeStatusTransientError {
			t.Fatalf("expected transient error for 302 without redirect, got %v", status)
		}
	})
}

type stubRoundTripper func(*http.Request) (*http.Response, error)

func (f stubRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func TestFetchAntigravityAvailableModels_PreservesCustomDefaultRoundTripper(t *testing.T) {
	origDefault := http.DefaultTransport
	t.Cleanup(func() {
		http.DefaultTransport = origDefault
	})

	called := false
	http.DefaultTransport = stubRoundTripper(func(r *http.Request) (*http.Response, error) {
		called = true
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"models":{"gemini-3-flash":{}}}`)),
			Request:    r,
		}, nil
	})

	hints, status := FetchAntigravityAvailableModels(context.Background(), AntigravityModelProbeRequest{
		BaseURLs:    []string{"https://custom-roundtripper.invalid"},
		AccessToken: "test-token",
	})
	if !called {
		t.Fatal("expected custom http.DefaultTransport RoundTripper to be called")
	}
	if status != AntigravityProbeStatusSuccess {
		t.Fatalf("status = %v, want %v", status, AntigravityProbeStatusSuccess)
	}
	if _, exists := hints.ModelIDs["gemini-3-flash"]; !exists {
		t.Fatalf("missing gemini-3-flash in hints: %+v", hints)
	}
}
