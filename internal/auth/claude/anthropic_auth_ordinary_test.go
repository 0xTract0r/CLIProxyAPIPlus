package claude

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"github.com/klauspost/compress/zstd"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type ordinaryClaudeTransport func(*http.Request) (*http.Response, error)

func (f ordinaryClaudeTransport) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestOrdinaryClaudeExchangeAndStorage(t *testing.T) {
	calls := 0
	auth := &ClaudeAuth{httpClient: &http.Client{Transport: ordinaryClaudeTransport(func(req *http.Request) (*http.Response, error) {
		calls++
		if calls != 1 || req.Method != http.MethodPost || req.URL.String() != TokenURL {
			t.Fatalf("unexpected request %d %s %s", calls, req.Method, req.URL)
		}
		var body map[string]string
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["code_verifier"] != "verifier" || body["state"] != "state" || body["code"] != "code" || body["grant_type"] != "authorization_code" {
			t.Fatal("invalid OAuth exchange payload")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"access_token":"a","refresh_token":"r","expires_in":120,"account":{"uuid":"acct","email_address":"sample@example.test"},"organization":{"uuid":"org","name":"Example"}}`))}, nil
	})}}
	bundle, err := auth.ExchangeCodeForTokens(context.Background(), "code#state", "state", &PKCECodes{CodeVerifier: "verifier"})
	if err != nil {
		t.Fatal(err)
	}
	store := auth.CreateTokenStorage(bundle)
	if calls != 1 || store.AccessToken != "a" || store.RefreshToken != "r" || store.AccountUUID != "acct" || store.Email != "sample@example.test" || store.OrganizationUUID != "org" || store.OrganizationName != "Example" || store.Expire == "" {
		t.Fatal("ordinary credential fields were not preserved")
	}
	path := filepath.Join(t.TempDir(), "claude-test.json")
	if err := store.SaveTokenToFile(path); err != nil {
		t.Fatal(err)
	}
	persisted, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var disk ClaudeTokenStorage
	if err := json.Unmarshal(persisted, &disk); err != nil {
		t.Fatal(err)
	}
	if disk.AccountUUID != store.AccountUUID || disk.OrganizationUUID != store.OrganizationUUID || disk.RefreshToken != store.RefreshToken {
		t.Fatal("ordinary credential fields did not reach the file")
	}

}
func TestOrdinaryClaudeStateMismatchStopsExchange(t *testing.T) {
	auth := &ClaudeAuth{httpClient: &http.Client{Transport: ordinaryClaudeTransport(func(*http.Request) (*http.Response, error) {
		t.Fatal("state mismatch reached transport")
		return nil, nil
	})}}
	_, err := auth.ExchangeCodeForTokens(context.Background(), "code#other", "expected", &PKCECodes{CodeVerifier: "verifier"})
	if err == nil {
		t.Fatal("expected state mismatch")
	}
}
func TestOrdinaryClaudeErrorBodyStaysPrivate(t *testing.T) {
	for _, test := range []struct {
		status int
		body   string
	}{
		{401, `{"error":"credential-secret"}`},
		{200, `{"access_token":"credential-secret",`},
		{200, `{}`},
	} {
		auth := &ClaudeAuth{httpClient: &http.Client{Transport: ordinaryClaudeTransport(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: test.status, Body: io.NopCloser(strings.NewReader(test.body))}, nil
		})}}
		_, err := auth.ExchangeCodeForTokens(context.Background(), "code", "state", &PKCECodes{CodeVerifier: "verifier"})
		if err == nil || strings.Contains(err.Error(), "credential-secret") {
			t.Fatal("expected a sanitized failure")
		}
	}
}

func TestOrdinaryClaudeBoundedCompressedResponse(t *testing.T) {
	for _, oversized := range []bool{false, true} {
		payload := []byte("sample")
		if oversized {
			payload = bytes.Repeat([]byte{'x'}, maxClaudeOAuthBodyBytes+1)
		}
		var gz bytes.Buffer
		gzipWriter := gzip.NewWriter(&gz)
		if _, err := gzipWriter.Write(payload); err != nil {
			t.Fatal(err)
		}
		if err := gzipWriter.Close(); err != nil {
			t.Fatal(err)
		}
		writer, err := zstd.NewWriter(nil, zstd.WithEncoderConcurrency(1))
		if err != nil {
			t.Fatal(err)
		}
		wire := writer.EncodeAll(gz.Bytes(), nil)
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
		resp := &http.Response{Header: http.Header{"Content-Encoding": {"gzip, zstd"}}, Body: io.NopCloser(bytes.NewReader(wire))}
		body, err := readClaudeOAuthResponseBody(resp)
		if oversized {
			if err == nil || !strings.Contains(err.Error(), "exceeds size limit") {
				t.Fatal("expected compressed body to exceed decoded limit")
			}
		} else if err != nil || !bytes.Equal(body, payload) {
			t.Fatal("stacked encodings did not decode in reverse order")
		}
	}
}

func TestOrdinaryClaudeExplicitProfile(t *testing.T) {
	auth := NewClaudeAuthWithHTTPClient(&http.Client{Transport: ordinaryClaudeTransport(func(req *http.Request) (*http.Response, error) {
		if req.Method != http.MethodGet || req.URL.String() != ProfileURL || req.Header.Get("Authorization") != "Bearer profile-token" {
			t.Fatal("unexpected profile request")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"account":{"uuid":"account","email":"sample@example.test"},"organization":{"uuid":"org","name":"Example"}}`))}, nil
	})})
	result, err := auth.FetchOAuthProfile(context.Background(), "profile-token")
	if err != nil {
		t.Fatal(err)
	}
	if result.Account.UUID != "account" || result.Account.Email != "sample@example.test" || result.Organization.UUID != "org" || result.Organization.Name != "Example" {
		t.Fatal("incorrect profile fields")
	}
}

func TestOrdinaryClaudeExplicitRoles(t *testing.T) {
	const roles = `{"organization_role":"member"}`
	auth := NewClaudeAuthWithHTTPClient(&http.Client{Transport: ordinaryClaudeTransport(func(req *http.Request) (*http.Response, error) {
		if req.Method != http.MethodGet || req.URL.String() != RolesURL || req.Header.Get("Authorization") != "Bearer roles-token" {
			t.Fatal("unexpected roles request")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(roles))}, nil
	})})
	body, err := auth.FetchOAuthRoles(context.Background(), "roles-token")
	if err != nil || string(body) != roles {
		t.Fatal("unexpected roles response")
	}
}

func TestOrdinaryClaudeSparseRotationKeepsOldFileFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credential.json")
	old := []byte(`{"access_token":"old-access","refresh_token":"old-refresh","email":"sample@example.test","account_uuid":"account","organization_uuid":"org","organization_name":"Example","subscription_type":"fixture-plan"}`)
	if err := os.WriteFile(path, old, 0600); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var storage ClaudeTokenStorage
	var extension struct {
		Subscription string `json:"subscription_type"`
	}
	if err := json.Unmarshal(raw, &storage); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &extension); err != nil {
		t.Fatal(err)
	}
	storage.SetMetadata(map[string]any{"subscription_type": extension.Subscription})
	calls := 0
	auth := NewClaudeAuthWithHTTPClient(&http.Client{Transport: ordinaryClaudeTransport(func(req *http.Request) (*http.Response, error) {
		calls++
		if req.Method != http.MethodPost || req.URL.String() != RefreshTokenURL {
			t.Fatal("unexpected refresh companion request")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"access_token":"new-access","refresh_token":"new-refresh","expires_in":120,"account":{"email_address":" "}}`))}, nil
	})})
	tokens, err := auth.RefreshTokens(context.Background(), storage.RefreshToken)
	if err != nil {
		t.Fatal(err)
	}
	auth.UpdateTokenStorage(&storage, tokens)
	if err := storage.SaveTokenToFile(path); err != nil {
		t.Fatal(err)
	}
	raw, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	for k, want := range map[string]string{"access_token": "new-access", "refresh_token": "new-refresh", "email": "sample@example.test", "account_uuid": "account", "organization_uuid": "org", "organization_name": "Example", "subscription_type": "fixture-plan"} {
		if got[k] != want {
			t.Errorf("unexpected value for %s", k)
		}
	}
	if calls != 1 {
		t.Fatalf("refresh made %d requests", calls)
	}
}
