package management

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestParseOAuthAccountSetupFromRequestValidatesProxy(t *testing.T) {
	gin.SetMode(gin.TestMode)

	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/codex-auth-url?note=work&proxy_url=socks5://proxy.example:1080", nil)

	setup, err := parseOAuthAccountSetupFromRequest(ctx)
	if err != nil {
		t.Fatalf("parse setup: %v", err)
	}
	if setup == nil || setup.Note != "work" || setup.ProxyURL != "socks5://proxy.example:1080" {
		t.Fatalf("setup = %#v", setup)
	}

	bad := httptest.NewRecorder()
	badCtx, _ := gin.CreateTestContext(bad)
	badCtx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/codex-auth-url?proxy_url=not-a-proxy", nil)
	if _, err := parseOAuthAccountSetupFromRequest(badCtx); err == nil {
		t.Fatal("expected invalid proxy_url error")
	}
}

func TestApplyOAuthAccountSetupToRecordPersistsRuntimeIdentity(t *testing.T) {
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, coreauth.NewManager(nil, nil, nil))
	record := &coreauth.Auth{
		ID:       "codex-new.json",
		FileName: "codex-new.json",
		Provider: "codex",
		Metadata: map[string]any{
			"type":  "codex",
			"email": "new@example.test",
		},
	}

	h.applyOAuthAccountSetupToRecord(record, &oauthAccountSetup{Note: "daily account", ProxyURL: "socks5://proxy.example:1080"})

	if record.ProxyURL != "socks5://proxy.example:1080" {
		t.Fatalf("record.ProxyURL = %q", record.ProxyURL)
	}
	if record.Metadata["proxy_url"] != "socks5://proxy.example:1080" || record.Metadata["note"] != "daily account" {
		t.Fatalf("metadata missing setup fields: %#v", record.Metadata)
	}
	if record.Attributes["note"] != "daily account" {
		t.Fatalf("attributes note = %q", record.Attributes["note"])
	}

	stored := readAccountSettingsMetadata(record, h.cfg)
	if stored.RuntimeIdentityState == nil || stored.RuntimeIdentityState.Current == nil {
		t.Fatalf("runtime identity missing: %#v", stored.RuntimeIdentityState)
	}
	current := stored.RuntimeIdentityState.Current
	// codex 核心托管默认出站真实是 codex_rustls_native_v1（uTLS 复刻 codex-rs rustls）。
	if current.ProfileID != "codex_rustls_native_v1" || current.TLSProfileID != "codex_rustls_native_v1" {
		t.Fatalf("profile IDs = (%q,%q)", current.ProfileID, current.TLSProfileID)
	}
	if !current.CoreManaged || !current.RuntimeEnforced {
		t.Fatalf("runtime flags = core:%v enforced:%v", current.CoreManaged, current.RuntimeEnforced)
	}
	if current.ProxyHash == "" || current.ProxyHash == "socks5://proxy.example:1080" {
		t.Fatalf("proxy hash should be present and redacted, got %q", current.ProxyHash)
	}
	if headers := coreauth.ExtractCustomHeadersFromMetadata(record.Metadata); len(headers) == 0 {
		t.Fatalf("expected generated managed headers in metadata")
	}
}

func TestPrepareOAuthSetupRuntimeAuthCanBeReusedForSavedRecord(t *testing.T) {
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, coreauth.NewManager(nil, nil, nil))
	setup := &oauthAccountSetup{Note: "daily account", ProxyURL: "socks5://proxy.example:1080"}

	exchangeAuth := h.prepareOAuthSetupRuntimeAuth("codex", setup)
	if exchangeAuth == nil {
		t.Fatal("exchange auth missing")
	}
	exchangeSeed := metadataString(exchangeAuth.Metadata, "managed_header_seed")
	if exchangeSeed == "" {
		t.Fatalf("exchange managed_header_seed missing: %#v", exchangeAuth.Metadata)
	}
	exchangeHeaders := coreauth.ExtractCustomHeadersFromMetadata(exchangeAuth.Metadata)
	// fork(anticorr Wave10-D)：codex CLI 画像出站只带 User-Agent/Version/Originator，
	// 不再带 Desktop 专属 sec-ch-ua。
	if exchangeHeaders["User-Agent"] == "" || exchangeHeaders["Version"] == "" {
		t.Fatalf("exchange identity headers incomplete: %#v", exchangeHeaders)
	}
	if got := exchangeHeaders["sec-ch-ua"]; got != "" {
		t.Fatalf("codex exchange sec-ch-ua = %q, want empty for CLI profile", got)
	}

	record := &coreauth.Auth{
		ID:       "codex-saved.json",
		FileName: "codex-saved.json",
		Provider: "codex",
		Metadata: map[string]any{
			"type":  "codex",
			"email": "saved@example.test",
		},
	}
	copyOAuthSetupSeed(record, exchangeAuth)
	h.applyOAuthAccountSetupToRecord(record, setup)

	if got := metadataString(record.Metadata, "managed_header_seed"); got != exchangeSeed {
		t.Fatalf("saved seed = %q, want exchange seed %q", got, exchangeSeed)
	}
	savedHeaders := coreauth.ExtractCustomHeadersFromMetadata(record.Metadata)
	for _, key := range []string{"User-Agent", "Version", "sec-ch-ua"} {
		if savedHeaders[key] != exchangeHeaders[key] {
			t.Fatalf("saved %s = %q, want exchange %q; saved=%#v exchange=%#v", key, savedHeaders[key], exchangeHeaders[key], savedHeaders, exchangeHeaders)
		}
	}
}

func TestOAuthIdentityHeaderRoundTripperAddsManagedHeadersWithoutOverwritingRequestHeaders(t *testing.T) {
	var captured http.Header
	rt := oauthIdentityHeaderRoundTripper{
		base: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			captured = req.Header.Clone()
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(`{}`)),
				Request:    req,
			}, nil
		}),
		headers: map[string]string{
			"User-Agent":   "managed-codex/1.0",
			"Version":      "26.318.11754",
			"Content-Type": "should-not-overwrite",
		},
	}

	req := httptest.NewRequest(http.MethodPost, "https://auth.openai.com/oauth/token", strings.NewReader("grant_type=authorization_code"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("round trip: %v", err)
	}
	_ = resp.Body.Close()

	if captured.Get("User-Agent") != "managed-codex/1.0" {
		t.Fatalf("User-Agent = %q", captured.Get("User-Agent"))
	}
	if captured.Get("Version") != "26.318.11754" {
		t.Fatalf("Version = %q", captured.Get("Version"))
	}
	if captured.Get("Content-Type") != "application/x-www-form-urlencoded" {
		t.Fatalf("Content-Type overwritten: %q", captured.Get("Content-Type"))
	}
}

func TestApplyOAuthAccountSetupToRecordGeneratesPerAccountManagedHeaders(t *testing.T) {
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, coreauth.NewManager(nil, nil, nil))

	for _, provider := range []string{"codex", "claude"} {
		first := &coreauth.Auth{
			ID:       provider + "-first.json",
			FileName: provider + "-first.json",
			Provider: provider,
			Metadata: map[string]any{
				"type":                provider,
				"email":               "first@example.test",
				"managed_header_seed": "11b3d415349fbaccd2feca85cc8a3dbe",
			},
		}
		second := &coreauth.Auth{
			ID:       provider + "-second.json",
			FileName: provider + "-second.json",
			Provider: provider,
			Metadata: map[string]any{
				"type":                provider,
				"email":               "second@example.test",
				"managed_header_seed": "e74214945825c9675327dc29d6daf1a7",
			},
		}

		h.applyOAuthAccountSetupToRecord(first, nil)
		h.applyOAuthAccountSetupToRecord(second, nil)

		firstHeaders := coreauth.ExtractCustomHeadersFromMetadata(first.Metadata)
		secondHeaders := coreauth.ExtractCustomHeadersFromMetadata(second.Metadata)
		if len(firstHeaders) == 0 || len(secondHeaders) == 0 {
			t.Fatalf("%s headers missing: first=%#v second=%#v", provider, firstHeaders, secondHeaders)
		}
		if firstHeaders["User-Agent"] == "" {
			t.Fatalf("%s User-Agent missing: first=%#v", provider, firstHeaders)
		}
		if provider == "codex" {
			// fork(anticorr Wave10-D)：codex 切到 CLI 画像后，出站是单一自洽的 codex_cli_rs
			// 版本，不再做 per-account Desktop 变体；像 claude 一样停在 coherent CLI baseline，
			// 也不带 sec-ch-ua。
			if firstHeaders["User-Agent"] != secondHeaders["User-Agent"] {
				t.Fatalf("codex User-Agent should stay on coherent CLI baseline, first=%q second=%q", firstHeaders["User-Agent"], secondHeaders["User-Agent"])
			}
			if firstHeaders["Version"] != secondHeaders["Version"] {
				t.Fatalf("codex Version should stay on coherent CLI baseline, first=%q second=%q", firstHeaders["Version"], secondHeaders["Version"])
			}
			if firstHeaders["sec-ch-ua"] != "" || secondHeaders["sec-ch-ua"] != "" {
				t.Fatalf("codex CLI profile should not emit sec-ch-ua, first=%q second=%q", firstHeaders["sec-ch-ua"], secondHeaders["sec-ch-ua"])
			}
			if firstHeaders["Originator"] != "codex_cli_rs" {
				t.Fatalf("codex Originator = %q, want codex_cli_rs", firstHeaders["Originator"])
			}
		} else if provider == "claude" {
			if firstHeaders["User-Agent"] != secondHeaders["User-Agent"] {
				t.Fatalf("claude User-Agent should stay on the coherent CLI baseline, first=%q second=%q", firstHeaders["User-Agent"], secondHeaders["User-Agent"])
			}
			if firstHeaders["X-Stainless-Package-Version"] != secondHeaders["X-Stainless-Package-Version"] {
				t.Fatalf("claude package version must preserve sourced value, first=%q second=%q", firstHeaders["X-Stainless-Package-Version"], secondHeaders["X-Stainless-Package-Version"])
			}
			if firstHeaders["X-Stainless-Runtime-Version"] != secondHeaders["X-Stainless-Runtime-Version"] {
				t.Fatalf("claude runtime version must preserve sourced value, first=%q second=%q", firstHeaders["X-Stainless-Runtime-Version"], secondHeaders["X-Stainless-Runtime-Version"])
			}
		}
		if first.Attributes["header:User-Agent"] != firstHeaders["User-Agent"] {
			t.Fatalf("%s runtime attribute header not synced: %#v", provider, first.Attributes)
		}
		firstStored := readAccountSettingsMetadata(first, h.cfg)
		secondStored := readAccountSettingsMetadata(second, h.cfg)
		if firstStored.ManagedHeaderSeedHash == "" || firstStored.ManagedHeaderSeedHash == secondStored.ManagedHeaderSeedHash {
			t.Fatalf("%s seed hash should be persisted per account: first=%q second=%q", provider, firstStored.ManagedHeaderSeedHash, secondStored.ManagedHeaderSeedHash)
		}
		if firstStored.ManagedHeaderState == nil || firstStored.ManagedHeaderState.Current == nil || firstStored.ManagedHeaderState.Current.VersionVariant == "" {
			t.Fatalf("%s first version variant should be persisted: %#v", provider, firstStored.ManagedHeaderState)
		}
		if secondStored.ManagedHeaderState == nil || secondStored.ManagedHeaderState.Current == nil || secondStored.ManagedHeaderState.Current.VersionVariant == "" {
			t.Fatalf("%s second version variant should be persisted: %#v", provider, secondStored.ManagedHeaderState)
		}
		if provider == "codex" {
			// CLI 画像下不再做版本偏移，version variant 固定为 latest。
			if firstStored.ManagedHeaderState.Current.VersionVariant != "latest" || secondStored.ManagedHeaderState.Current.VersionVariant != "latest" {
				t.Fatalf("codex CLI version variants should stay latest: first=%#v second=%#v", firstStored.ManagedHeaderState.Current, secondStored.ManagedHeaderState.Current)
			}
		} else if provider == "claude" {
			if firstStored.ManagedHeaderState.Current.VersionVariant != "latest" || secondStored.ManagedHeaderState.Current.VersionVariant != "latest" {
				t.Fatalf("claude version variants should not drift from latest: first=%#v second=%#v", firstStored.ManagedHeaderState.Current, secondStored.ManagedHeaderState.Current)
			}
		}
	}
}

func TestApplyOAuthAccountSetupToRecordPreservesExistingManagedHeaderVariantSlots(t *testing.T) {
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, coreauth.NewManager(nil, nil, nil))
	record := &coreauth.Auth{
		ID:       "codex-persisted-slot.json",
		FileName: "codex-persisted-slot.json",
		Provider: "codex",
		Metadata: map[string]any{
			"type":                "codex",
			"email":               "slot@example.test",
			"managed_header_seed": "11b3d415349fbaccd2feca85cc8a3dbe",
			"account_settings": map[string]any{
				"schema_version": 1,
				"managed_header_state": map[string]any{
					"current": map[string]any{
						"version_variant":     "latest-1",
						"brand_order_variant": "slot-1",
					},
				},
			},
		},
	}

	h.applyOAuthAccountSetupToRecord(record, nil)

	// fork(anticorr Wave10-D)：codex 切到 CLI 画像后，历史 Desktop 变体槽（latest-1 /
	// slot-1）不再被应用——出站固定为自洽的 codex_cli_rs CLI baseline（floor 0.140.0），
	// 不带 Desktop sec-ch-ua。
	headers := coreauth.ExtractCustomHeadersFromMetadata(record.Metadata)
	if got := headers["Version"]; got != "0.140.0" {
		t.Fatalf("Version = %q, want coherent CLI baseline 0.140.0 (Desktop slot must not apply)", got)
	}
	if !strings.HasPrefix(headers["User-Agent"], "codex_cli_rs/0.140.0") {
		t.Fatalf("User-Agent = %q, want codex_cli_rs CLI baseline", headers["User-Agent"])
	}
	if got := headers["sec-ch-ua"]; got != "" {
		t.Fatalf("sec-ch-ua = %q, want empty for CLI profile (Desktop slot must not apply)", got)
	}
}

func TestApplyOAuthAccountSetupToRecordGeneratesIndependentSeedForSameAccountMetadata(t *testing.T) {
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, coreauth.NewManager(nil, nil, nil))
	first := &coreauth.Auth{
		ID:       "codex-first.json",
		FileName: "codex-first.json",
		Provider: "codex",
		Metadata: map[string]any{
			"type":  "codex",
			"email": "same@example.test",
		},
	}
	second := &coreauth.Auth{
		ID:       "codex-second.json",
		FileName: "codex-second.json",
		Provider: "codex",
		Metadata: map[string]any{
			"type":  "codex",
			"email": "same@example.test",
		},
	}

	h.applyOAuthAccountSetupToRecord(first, nil)
	h.applyOAuthAccountSetupToRecord(second, nil)

	firstSeed := metadataString(first.Metadata, "managed_header_seed")
	secondSeed := metadataString(second.Metadata, "managed_header_seed")
	if firstSeed == "" || secondSeed == "" || firstSeed == secondSeed {
		t.Fatalf("managed_header_seed should be independently generated, first=%q second=%q", firstSeed, secondSeed)
	}
}

func TestGetAuthStatusReturnsCompletedOAuthSavedInfo(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	authDir := t.TempDir()
	savedPath := filepath.Join(authDir, "codex-saved.json")
	record := &coreauth.Auth{
		ID:       "codex-saved.json",
		FileName: "codex-saved.json",
		Provider: "codex",
		ProxyURL: "socks5://proxy.example:1080",
		Attributes: map[string]string{
			"note": "daily account",
		},
		Metadata: map[string]any{
			"type":      "codex",
			"email":     "saved@example.test",
			"note":      "daily account",
			"proxy_url": "socks5://proxy.example:1080",
		},
	}

	state := "saved-info-state"
	RegisterOAuthSession(state, "codex")
	t.Cleanup(func() { CompleteOAuthSession(state) })
	CompleteOAuthSessionWithRecord(state, savedPath, record)

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, coreauth.NewManager(nil, nil, nil))
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/get-auth-status?state="+state, nil)
	h.GetAuthStatus(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("status code = %d, body = %s", rec.Code, rec.Body.String())
	}
	var payload map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode status payload: %v", err)
	}
	for key, want := range map[string]string{
		"status":     "ok",
		"provider":   "codex",
		"saved_path": savedPath,
		"auth_name":  "codex-saved.json",
		"note":       "daily account",
		"proxy_url":  "socks5://proxy.example:1080",
	} {
		if got, _ := payload[key].(string); got != want {
			t.Fatalf("payload[%s] = %#v, want %q; payload=%#v", key, payload[key], want, payload)
		}
	}
}

func TestBuildAuthFromFileDataRestoresRuntimeProxyURL(t *testing.T) {
	authDir := t.TempDir()
	path := filepath.Join(authDir, "codex-proxy.json")
	data := []byte(`{"type":"codex","email":"proxy@example.test","proxy_url":"socks5://proxy.example:1080","headers":{"User-Agent":"managed-ua/1.0"}}`)

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, coreauth.NewManager(nil, nil, nil))
	auth, err := h.buildAuthFromFileData(path, data)
	if err != nil {
		t.Fatalf("build auth: %v", err)
	}
	if got := auth.ProxyURL; got != "socks5://proxy.example:1080" {
		t.Fatalf("ProxyURL = %q, want %q", got, "socks5://proxy.example:1080")
	}
	if got := auth.Attributes["header:User-Agent"]; got != "managed-ua/1.0" {
		t.Fatalf("header:User-Agent = %q, want managed header", got)
	}
}

func TestSyncAuthManagedHeaderStateAddsSeedForExistingOAuthCredential(t *testing.T) {
	store := &memoryAuthStore{}
	manager := coreauth.NewManager(store, nil, nil)
	auth := &coreauth.Auth{
		ID:       "codex-existing.json",
		FileName: "codex-existing.json",
		Provider: "codex",
		Metadata: map[string]any{
			"type":          "codex",
			"email":         "existing@example.test",
			"access_token":  "access",
			"refresh_token": "refresh",
		},
	}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("register auth: %v", err)
	}
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)

	updated := h.syncAuthManagedHeaderState(context.Background(), auth)

	if updated == nil {
		t.Fatal("updated auth is nil")
	}
	if seed := metadataString(updated.Metadata, "managed_header_seed"); seed == "" {
		t.Fatalf("managed_header_seed missing: %#v", updated.Metadata)
	}
	if headers := coreauth.ExtractCustomHeadersFromMetadata(updated.Metadata); headers["User-Agent"] == "" {
		t.Fatalf("managed headers missing after seed migration: %#v", headers)
	}
	persisted, ok := manager.GetByID("codex-existing.json")
	if !ok || metadataString(persisted.Metadata, "managed_header_seed") == "" {
		t.Fatalf("manager did not persist seed migration: %#v", persisted)
	}
}

func TestOAuthStartResponseIncludesCountdown(t *testing.T) {
	before := time.Now().UTC()
	payload := oauthStartResponse("https://example.test/auth", "state-1")
	if payload["expires_in_seconds"] != int(oauthCallbackWaitTimeout.Seconds()) {
		t.Fatalf("expires_in_seconds = %#v", payload["expires_in_seconds"])
	}
	expiresAt, ok := payload["expires_at"].(string)
	if !ok || expiresAt == "" {
		t.Fatalf("expires_at missing: %#v", payload)
	}
	parsed, err := time.Parse(time.RFC3339, expiresAt)
	if err != nil {
		t.Fatalf("parse expires_at: %v", err)
	}
	if parsed.Before(before.Add(oauthCallbackWaitTimeout-time.Second)) || parsed.After(before.Add(oauthCallbackWaitTimeout+time.Second)) {
		t.Fatalf("expires_at = %s, want about %s after start", parsed, oauthCallbackWaitTimeout)
	}
	if _, err := json.Marshal(payload); err != nil {
		t.Fatalf("response should be JSON serializable: %v", err)
	}
}
