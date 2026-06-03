package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	runtimehelps "github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestRunProviderTLSProbeEndpointResolvesAuthAndReturnsDiagnosticEvidence(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	store := &memoryAuthStore{}
	manager := coreauth.NewManager(store, nil, nil)
	record := &coreauth.Auth{
		ID:       "claude.json",
		FileName: "claude.json",
		Provider: "claude",
		Metadata: map[string]any{
			"auth_method":   "oauth",
			"email":         "claude@example.test",
			"refresh_token": "must-not-be-read",
			"account_settings": map[string]any{
				"schema_version": 1,
				"transport_profile": map[string]any{
					"provider": "claude",
					"preset":   "claude_chrome_like_mac_v2",
				},
				"tls_profile": map[string]any{
					"provider": "claude",
					"preset":   "claude_chrome_like_mac_v2",
				},
			},
		},
	}
	if _, errRegister := manager.Register(context.Background(), record); errRegister != nil {
		t.Fatalf("failed to register auth record: %v", errRegister)
	}

	originalProbe := runProviderTLSProbe
	t.Cleanup(func() { runProviderTLSProbe = originalProbe })
	runProviderTLSProbe = func(ctx context.Context, cfg *config.Config, auth *coreauth.Auth, opts runtimehelps.ProviderTLSProbeOptions) (runtimehelps.ProviderTLSProbeResult, error) {
		if auth == nil || auth.Provider != "claude" || auth.FileName != "claude.json" {
			t.Fatalf("auth = %#v, want claude.json", auth)
		}
		if opts.CorrelationID != "corr-endpoint" {
			t.Fatalf("CorrelationID = %q, want corr-endpoint", opts.CorrelationID)
		}
		if opts.TargetHost != "api.anthropic.com" || opts.Method != "HEAD" || opts.Path != "/" {
			t.Fatalf("probe opts = %#v", opts)
		}
		return runtimehelps.ProviderTLSProbeResult{
			EvidenceType:           runtimehelps.ProviderTLSProbeEvidenceType,
			ClaimScope:             runtimehelps.ProviderTLSProbeClaimScope,
			CorrelationID:          opts.CorrelationID,
			Provider:               "claude",
			TargetHost:             "api.anthropic.com",
			OutboundURL:            "https://api.anthropic.com/",
			Method:                 "HEAD",
			HTTPStatus:             http.StatusForbidden,
			HTTPStatusText:         "403 Forbidden",
			AuthorizationSent:      false,
			ProviderObserved:       false,
			SecretValuesStored:     false,
			RuntimeProfileEnforced: true,
			RuntimeProfileSource:   runtimehelps.ProviderTLSProbeRuntimeProfileSourceExplicit,
			AccountRuntimeSummary: runtimehelps.ProviderTLSProbeRuntimeSummary{
				EvidenceType:       runtimehelps.AccountRuntimeEvidenceType,
				ClaimScope:         runtimehelps.AccountRuntimeClaimScope,
				Provider:           "claude",
				AuthorizationSent:  false,
				ProviderObserved:   false,
				SecretValuesStored: false,
			},
			Transport: runtimehelps.ProviderTLSProbeTransportSummary{
				RuntimeProfileConfigured: true,
				RuntimeProfileEnforced:   true,
				RuntimeProfileSource:     runtimehelps.ProviderTLSProbeRuntimeProfileSourceExplicit,
				TransportProfileID:       "claude_chrome_like_mac_v2",
				TLSProfileID:             "claude_chrome_like_mac_v2",
			},
		}, nil
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	body := `{"name":"claude.json","provider":"claude","target_host":"api.anthropic.com","method":"HEAD","path":"/","correlation_id":"corr-endpoint"}`
	req := httptest.NewRequest(http.MethodPost, "/v0/management/diagnostics/provider-tls-probe", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	ctx.Request = req

	h.RunProviderTLSProbe(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp runtimehelps.ProviderTLSProbeResult
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.EvidenceType != runtimehelps.ProviderTLSProbeEvidenceType {
		t.Fatalf("EvidenceType = %q", resp.EvidenceType)
	}
	if resp.AuthorizationSent {
		t.Fatal("AuthorizationSent = true, want false")
	}
	if resp.ProviderObserved {
		t.Fatal("ProviderObserved = true, want false")
	}
	if !resp.RuntimeProfileEnforced {
		t.Fatal("RuntimeProfileEnforced = false, want true")
	}
	if resp.RuntimeProfileSource != runtimehelps.ProviderTLSProbeRuntimeProfileSourceExplicit {
		t.Fatalf("RuntimeProfileSource = %q, want explicit source", resp.RuntimeProfileSource)
	}
	if !resp.Transport.RuntimeProfileEnforced {
		t.Fatalf("transport runtime_profile_enforced = false, summary=%#v", resp.Transport)
	}
	if resp.Transport.RuntimeProfileSource != runtimehelps.ProviderTLSProbeRuntimeProfileSourceExplicit {
		t.Fatalf("transport runtime_profile_source = %q, want explicit source", resp.Transport.RuntimeProfileSource)
	}
}

func TestRunProviderTLSProbeEndpointRejectsProviderMismatch(t *testing.T) {
	gin.SetMode(gin.TestMode)

	manager := coreauth.NewManager(&memoryAuthStore{}, nil, nil)
	if _, errRegister := manager.Register(context.Background(), &coreauth.Auth{
		ID:       "codex.json",
		FileName: "codex.json",
		Provider: "codex",
	}); errRegister != nil {
		t.Fatalf("failed to register auth record: %v", errRegister)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, "/v0/management/diagnostics/provider-tls-probe", strings.NewReader(`{"name":"codex.json","provider":"claude"}`))
	req.Header.Set("Content-Type", "application/json")
	ctx.Request = req

	h.RunProviderTLSProbe(ctx)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}
