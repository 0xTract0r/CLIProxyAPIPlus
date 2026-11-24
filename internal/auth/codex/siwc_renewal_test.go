package codex

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestOrdinaryRefreshUsesOfficialScope(t *testing.T) {
	calls := 0
	auth := &CodexAuth{httpClient: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.Method != http.MethodPost || r.URL.String() != TokenURL || r.ParseForm() != nil {
			t.Fatal("wrong official OAuth request")
		}
		if r.PostForm.Get("client_id") != ClientID || r.PostForm.Get("grant_type") != "refresh_token" || r.PostForm.Get("scope") != "openid profile email" || r.PostForm.Get("refresh_token") != "official-scope-fixture" {
			t.Fatal("official refresh did not use the selected grant and standard scope")
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"access_token":"next","refresh_token":"rotation","expires_in":3600}`))}, nil
	})}}
	auth.SetRefreshJournal(&memoryRefreshJournal{})
	tok, err := auth.RefreshTokens(t.Context(), "official-scope-fixture")
	if err != nil || tok == nil || tok.AccessToken != "next" || calls != 1 {
		t.Fatalf("refresh error=%v requests=%d", err, calls)
	}
}

func TestOrdinaryRefreshValidatesExpiry(t *testing.T) {
	for _, tc := range []struct {
		name, seconds string
		valid         bool
	}{
		{"integer", "3600", true}, {"float integer", "3600.0", true}, {"scientific integer", "3.6e3", true},
		{"fractional", "3600.5", false}, {"precision beyond float", "3600.000000000000000001", false}, {"overflow", "1e20", false}, {"huge exponent", "1e1000000000", false}, {"nonfinite string", `"Inf"`, false}, {"negative", "-3600", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			auth := &CodexAuth{httpClient: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"access_token":"next","refresh_token":"rotation","expires_in":` + tc.seconds + `}`))}, nil
			})}}
			auth.SetRefreshJournal(&memoryRefreshJournal{})
			started := time.Now().Truncate(time.Second)
			tok, err := auth.RefreshTokens(t.Context(), "expiry-"+tc.name)
			if !tc.valid {
				if tok != nil || !IsRefreshOutcomeUnknown(err) {
					t.Fatalf("invalid successful response accepted or replayable: %v", err)
				}
				return
			}
			if err != nil || tok == nil {
				t.Fatalf("integral expiry rejected: %v", err)
			}
			expiry, parseErr := time.Parse(time.RFC3339, tok.Expire)
			if parseErr != nil || expiry.Before(started.Add(time.Hour)) || expiry.After(time.Now().Add(time.Hour)) {
				t.Fatalf("expiry outside provider window: %q", tok.Expire)
			}
		})
	}
}

func TestOrdinaryRefreshReceiptPreservesExpiry(t *testing.T) {
	calls := 0
	auth := &CodexAuth{httpClient: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"access_token":"next","refresh_token":"rotated","expires_in":3600}`))}, nil
	})}}
	journal := &memoryRefreshJournal{}
	auth.SetRefreshJournal(journal)
	first, err := auth.RefreshTokens(t.Context(), "ordinary-receipt-fixture")
	if err != nil || first == nil || first.Expire == "" {
		t.Fatalf("first refresh error: %v", err)
	}
	// A different client instance reads the original receipt without exchanging the grant again.
	secondClient := &CodexAuth{httpClient: auth.httpClient}
	secondClient.SetRefreshJournal(journal)
	second, err := secondClient.RefreshTokens(t.Context(), "ordinary-receipt-fixture")
	if err != nil || second == nil || second.Expire != first.Expire || second.RefreshToken != "rotated" || calls != 1 {
		t.Fatalf("cached receipt changed or triggered another request: err=%v requests=%d", err, calls)
	}
}

func TestOrdinaryRefreshFailureRetainsPublicDiagnostic(t *testing.T) {
	auth := &CodexAuth{httpClient: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusBadRequest, Header: http.Header{"X-Request-Id": {"private-header-fixture"}, "Retry-After": {"7"}}, Body: io.NopCloser(strings.NewReader(`{"error":"invalid_grant","error_description":"private-body-fixture"}`))}, nil
	})}}
	auth.SetRefreshJournal(&memoryRefreshJournal{})
	denied, err := auth.RefreshTokens(t.Context(), "diagnostic-fixture")
	var status *refreshResponseError
	if denied != nil || !errors.As(err, &status) || status.StatusCode() != http.StatusBadRequest || status.Headers().Get("Retry-After") != "7" || !strings.Contains(err.Error(), "invalid_grant") || strings.Contains(err.Error(), "private-") {
		t.Fatalf("public diagnostic or redaction changed: %v", err)
	}
}

func TestGenerateSIWCReauthURLRejectsUnissuedClient(t *testing.T) {
	for _, clientID := range []string{"", " ", SIWCDynamicClientID} {
		if _, errGenerate := (&CodexAuth{}).GenerateSIWCReauthURL(clientID, "state", "nonce", "e61bbe28-07ef-466d-8e5d-a344f94ab305", "", &PKCECodes{CodeChallenge: "challenge"}); errGenerate == nil {
			t.Errorf("accepted an unissued client ID %q", clientID)
		}
	}
}
