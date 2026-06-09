package management

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/andybalholm/brotli"
	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

type quotaSnapshotTestExecutor struct {
	provider        string
	body            string
	bodyBytes       []byte
	contentEncoding string
	responses       map[string]quotaSnapshotTestResponse
	responsesByAuth map[string]quotaSnapshotTestResponse

	mu           sync.Mutex
	calls        int
	refreshCalls int
	callsByAuth  map[string]int
}

type quotaSnapshotTestResponse struct {
	statusCode      int
	body            string
	bodyBytes       []byte
	contentEncoding string
	delay           time.Duration
	err             error
}

func (e *quotaSnapshotTestExecutor) Identifier() string { return e.provider }

func (e *quotaSnapshotTestExecutor) Execute(context.Context, *coreauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}

func (e *quotaSnapshotTestExecutor) ExecuteStream(context.Context, *coreauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	return nil, nil
}

func (e *quotaSnapshotTestExecutor) Refresh(context.Context, *coreauth.Auth) (*coreauth.Auth, error) {
	e.mu.Lock()
	e.refreshCalls++
	e.mu.Unlock()
	return nil, nil
}

func (e *quotaSnapshotTestExecutor) CountTokens(context.Context, *coreauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}

func (e *quotaSnapshotTestExecutor) HttpRequest(ctx context.Context, auth *coreauth.Auth, req *http.Request) (*http.Response, error) {
	e.mu.Lock()
	e.calls++
	if e.callsByAuth == nil {
		e.callsByAuth = make(map[string]int)
	}
	authID := ""
	if auth != nil {
		authID = auth.ID
	}
	e.callsByAuth[authID]++
	e.mu.Unlock()

	response := quotaSnapshotTestResponse{
		statusCode:      http.StatusOK,
		body:            `{"rate_limit":{"used_percent":25},"plan_type":"plus"}`,
		contentEncoding: e.contentEncoding,
	}
	if e.body != "" {
		response.body = e.body
	}
	if e.bodyBytes != nil {
		response.bodyBytes = e.bodyBytes
	}
	if specific, ok := e.responses[req.URL.String()]; ok {
		response = specific
	}
	if specific, ok := e.responsesByAuth[authID]; ok {
		response = specific
	}
	if response.statusCode == 0 {
		response.statusCode = http.StatusOK
	}
	if response.delay > 0 {
		timer := time.NewTimer(response.delay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
	if response.err != nil {
		return nil, response.err
	}
	bodyBytes := []byte(response.body)
	if response.bodyBytes != nil {
		bodyBytes = response.bodyBytes
	}
	header := http.Header{"Content-Type": []string{"application/json"}}
	if response.contentEncoding != "" {
		header.Set("Content-Encoding", response.contentEncoding)
	}
	return &http.Response{
		StatusCode: response.statusCode,
		Header:     header,
		Body:       io.NopCloser(bytes.NewReader(bodyBytes)),
		Request:    req.WithContext(ctx),
	}, nil
}

func (e *quotaSnapshotTestExecutor) Calls() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.calls
}

func (e *quotaSnapshotTestExecutor) RefreshCalls() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.refreshCalls
}

func (e *quotaSnapshotTestExecutor) CallsForAuth(authID string) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.callsByAuth[authID]
}

func defaultQuotaSnapshotTestPolicy() QuotaSnapshotRefreshPolicy {
	return QuotaSnapshotRefreshPolicyFromConfig(nil)
}

func immediateStartupQuotaSnapshotTestPolicy() QuotaSnapshotRefreshPolicy {
	policy := defaultQuotaSnapshotTestPolicy()
	policy.Jitter = 0
	policy.StartupCatchUp = true
	return policy
}

func TestQuotaSnapshotsRefreshPersistsCoreSnapshot(t *testing.T) {
	t.Parallel()

	gin.SetMode(gin.TestMode)
	manager := coreauth.NewManager(nil, nil, nil)
	exec := &quotaSnapshotTestExecutor{provider: "codex"}
	manager.RegisterExecutor(exec)
	if _, err := manager.Register(context.Background(), &coreauth.Auth{
		ID:       "codex-plus",
		Provider: "codex",
		Metadata: map[string]any{"plan_type": "plus"},
	}); err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	handler := NewHandlerWithoutConfigFilePath(nil, manager)
	router := gin.New()
	router.POST("/v0/management/quota/refresh", handler.RefreshQuotaSnapshots)
	router.GET("/v0/management/quota/snapshots", handler.GetQuotaSnapshots)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v0/management/quota/refresh", strings.NewReader(`{"auth_id":"codex-plus"}`))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("refresh status = %d, want 200 body=%s", rec.Code, rec.Body.String())
	}
	payload := decodeQuotaSnapshotPayload(t, rec)
	if payload.Policy.IntervalSeconds != int64(defaultQuotaSnapshotRefreshInterval/time.Second) ||
		payload.Policy.JitterSeconds != int64(config.DefaultQuotaSnapshotRefreshJitter/time.Second) ||
		!payload.Policy.Enabled ||
		!payload.Policy.StartupCatchUp ||
		payload.Policy.StartupMaxStalenessSeconds != int64(config.DefaultQuotaSnapshotRefreshStartupMaxStaleness/time.Second) {
		t.Fatalf("quota policy payload = %#v, want default policy", payload.Policy)
	}
	if exec.Calls() != 1 {
		t.Fatalf("HttpRequest calls = %d, want 1", exec.Calls())
	}

	updated, ok := manager.GetByID("codex-plus")
	if !ok {
		t.Fatal("updated auth missing")
	}
	if got := metadataString(updated.Metadata, quotaRefreshStatusMetadataKey); got != "ok" {
		t.Fatalf("quota status = %q, want ok", got)
	}
	if _, ok := updated.Metadata[quotaSnapshotMetadataKey].(map[string]any); !ok {
		t.Fatalf("quota snapshot missing or wrong type: %#v", updated.Metadata[quotaSnapshotMetadataKey])
	}
	if _, ok := metadataTime(updated.Metadata, quotaLastRefreshedMetadataKey); !ok {
		t.Fatal("last refreshed timestamp missing")
	}
	if _, ok := metadataTime(updated.Metadata, quotaNextRefreshMetadataKey); !ok {
		t.Fatal("next refresh timestamp missing")
	}

	getRec := httptest.NewRecorder()
	router.ServeHTTP(getRec, httptest.NewRequest(http.MethodGet, "/v0/management/quota/snapshots", nil))
	if getRec.Code != http.StatusOK {
		t.Fatalf("GET status = %d, want 200 body=%s", getRec.Code, getRec.Body.String())
	}
	if exec.Calls() != 1 {
		t.Fatalf("GET should not call provider; calls = %d, want 1", exec.Calls())
	}
	if !strings.Contains(getRec.Body.String(), `"status":"ok"`) {
		t.Fatalf("GET body missing ok snapshot: %s", getRec.Body.String())
	}
}

func TestQuotaSnapshotsRefreshReturnsPerAccountResults(t *testing.T) {
	t.Parallel()

	gin.SetMode(gin.TestMode)
	manager := coreauth.NewManager(nil, nil, nil)
	exec := &quotaSnapshotTestExecutor{
		provider: "codex",
		responsesByAuth: map[string]quotaSnapshotTestResponse{
			"codex-ruleset": {
				err: errors.New("socks connect tcp 80.174.217.1:12324->api.anthropic.com:443: unknown error connection not allowed by ruleset"),
			},
		},
	}
	manager.RegisterExecutor(exec)
	if _, err := manager.Register(context.Background(), &coreauth.Auth{
		ID:       "codex-ok",
		Provider: "codex",
		Metadata: map[string]any{"plan_type": "plus"},
	}); err != nil {
		t.Fatalf("Register ok auth error = %v", err)
	}
	if _, err := manager.Register(context.Background(), &coreauth.Auth{
		ID:       "codex-ruleset",
		Provider: "codex",
		ProxyURL: "socks5://user:pass@80.174.217.1:12324",
		Metadata: map[string]any{"plan_type": "plus"},
	}); err != nil {
		t.Fatalf("Register ruleset auth error = %v", err)
	}
	handler := NewHandlerWithoutConfigFilePath(nil, manager)
	router := gin.New()
	router.POST("/v0/management/quota/refresh", handler.RefreshQuotaSnapshots)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v0/management/quota/refresh", strings.NewReader(`{"provider":"codex"}`))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("refresh status = %d, want 200 body=%s", rec.Code, rec.Body.String())
	}
	payload := decodeQuotaSnapshotPayload(t, rec)
	if len(payload.RefreshResults) != 2 {
		t.Fatalf("refresh_results length = %d, want 2 body=%s", len(payload.RefreshResults), rec.Body.String())
	}
	okResult := quotaRefreshResultForAuth(t, payload, "codex-ok")
	if okResult.Status != quotaRefreshStatusOK || !okResult.Refreshed || okResult.ErrorClass != "" {
		t.Fatalf("ok result = %#v, want ok/refreshed/no error_class", okResult)
	}
	failResult := quotaRefreshResultForAuth(t, payload, "codex-ruleset")
	if failResult.Status != quotaRefreshStatusError || failResult.Refreshed {
		t.Fatalf("ruleset result status/refreshed = %q/%v, want error/false", failResult.Status, failResult.Refreshed)
	}
	if failResult.ErrorClass != "proxy_ruleset_reject" {
		t.Fatalf("ruleset error_class = %q, want proxy_ruleset_reject; result=%#v", failResult.ErrorClass, failResult)
	}
	if failResult.ProxySource != "account" || !strings.HasPrefix(failResult.ProxyHash, "sha256:") {
		t.Fatalf("ruleset proxy fields = %q/%q, want account/sha256 hash", failResult.ProxySource, failResult.ProxyHash)
	}
	if strings.Contains(rec.Body.String(), "user:pass") {
		t.Fatalf("response leaked proxy credentials: %s", rec.Body.String())
	}
	entry := quotaSnapshotEntryForAuth(t, payload, "codex-ruleset")
	if entry.Status != quotaRefreshStatusError || !strings.Contains(entry.Error, "connection not allowed by ruleset") {
		t.Fatalf("ruleset entry = %#v, want persisted error status", entry)
	}
}

func TestQuotaSnapshotRefreshResultClassifiesProviderTimeout(t *testing.T) {
	t.Parallel()

	manager := coreauth.NewManager(nil, nil, nil)
	exec := &quotaSnapshotTestExecutor{
		provider: "codex",
		responsesByAuth: map[string]quotaSnapshotTestResponse{
			"codex-slow": {delay: time.Second},
		},
	}
	manager.RegisterExecutor(exec)
	auth, err := manager.Register(context.Background(), &coreauth.Auth{
		ID:       "codex-slow",
		Provider: "codex",
		Metadata: map[string]any{"plan_type": "plus"},
	})
	if err != nil {
		t.Fatalf("Register slow auth error = %v", err)
	}
	handler := NewHandlerWithoutConfigFilePath(nil, manager)
	policy := defaultQuotaSnapshotTestPolicy()
	policy.ProviderTimeout = 20 * time.Millisecond

	start := time.Now()
	result := handler.refreshQuotaSnapshotResult(context.Background(), auth, policy)
	elapsed := time.Since(start)
	if elapsed > 500*time.Millisecond {
		t.Fatalf("refresh elapsed = %s, want bounded by provider timeout", elapsed)
	}
	if result.Status != quotaRefreshStatusError || result.ErrorClass != "timeout" || result.Refreshed {
		t.Fatalf("timeout result = %#v, want error/timeout/not refreshed", result)
	}
	if got := exec.CallsForAuth("codex-slow"); got != 1 {
		t.Fatalf("slow auth calls = %d, want 1", got)
	}
	updated, ok := manager.GetByID("codex-slow")
	if !ok {
		t.Fatal("updated auth missing")
	}
	if got := metadataString(updated.Metadata, quotaRefreshErrorMetadataKey); !strings.Contains(got, "deadline exceeded") {
		t.Fatalf("metadata quota_refresh_error = %q, want deadline exceeded", got)
	}
}

func TestQuotaSnapshotsClaudeUnauthorizedRequiresReauth(t *testing.T) {
	t.Parallel()

	const (
		profileURL = "https://api.anthropic.com/api/oauth/profile"
		usageURL   = "https://api.anthropic.com/api/oauth/usage"
	)

	tests := []struct {
		name      string
		responses map[string]quotaSnapshotTestResponse
	}{
		{
			name: "profile 401",
			responses: map[string]quotaSnapshotTestResponse{
				profileURL: {statusCode: http.StatusUnauthorized, body: `{"error":"provider body 401 invalid token"}`},
			},
		},
		{
			name: "usage 403",
			responses: map[string]quotaSnapshotTestResponse{
				profileURL: {body: `{"plan_type":"pro"}`},
				usageURL:   {statusCode: http.StatusForbidden, body: `{"error":"provider body 403 forbidden"}`},
			},
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			manager := coreauth.NewManager(nil, nil, nil)
			exec := &quotaSnapshotTestExecutor{provider: "claude", responses: tt.responses}
			manager.RegisterExecutor(exec)
			if _, err := manager.Register(context.Background(), &coreauth.Auth{
				ID:       "claude-oauth",
				Provider: "claude",
			}); err != nil {
				t.Fatalf("Register() error = %v", err)
			}
			handler := NewHandlerWithoutConfigFilePath(nil, manager)
			router := gin.New()
			router.POST("/v0/management/quota/refresh", handler.RefreshQuotaSnapshots)

			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/v0/management/quota/refresh", strings.NewReader(`{"auth_id":"claude-oauth"}`))
			req.Header.Set("Content-Type", "application/json")
			router.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("refresh status = %d, want 200 body=%s", rec.Code, rec.Body.String())
			}

			entry := quotaSnapshotEntryForAuth(t, decodeQuotaSnapshotPayload(t, rec), "claude-oauth")
			if entry.Status != quotaRefreshStatusReauthRequired {
				t.Fatalf("entry status = %q, want %q", entry.Status, quotaRefreshStatusReauthRequired)
			}
			if entry.Error != claudeQuotaCredentialUnauthorizedMessage {
				t.Fatalf("entry error = %q, want sanitized reauth message", entry.Error)
			}
			for _, forbidden := range []string{"401", "403"} {
				if strings.Contains(entry.Error, forbidden) {
					t.Fatalf("reauth error leaked %q: entry=%#v body=%s", forbidden, entry, rec.Body.String())
				}
			}
			for _, forbidden := range []string{"provider body", "invalid token", "forbidden"} {
				if strings.Contains(rec.Body.String(), forbidden) || strings.Contains(entry.Error, forbidden) {
					t.Fatalf("reauth response leaked %q: entry=%#v body=%s", forbidden, entry, rec.Body.String())
				}
			}

			updated, ok := manager.GetByID("claude-oauth")
			if !ok {
				t.Fatal("updated auth missing")
			}
			if updated.Disabled {
				t.Fatal("reauth-required quota refresh must not disable auth")
			}
			if got := metadataString(updated.Metadata, quotaRefreshStatusMetadataKey); got != quotaRefreshStatusReauthRequired {
				t.Fatalf("persisted quota status = %q, want %q", got, quotaRefreshStatusReauthRequired)
			}
			if got := metadataString(updated.Metadata, quotaRefreshErrorMetadataKey); got != claudeQuotaCredentialUnauthorizedMessage {
				t.Fatalf("persisted quota error = %q, want sanitized reauth message", got)
			}
		})
	}
}

func TestQuotaSnapshotsCodexUnauthorizedRequiresReauth(t *testing.T) {
	t.Parallel()

	const (
		usageURL = "https://chatgpt.com/backend-api/wham/usage"
		wantErr  = "Codex credential unauthorized; reauthenticate this credential to refresh quota."
	)

	tests := []struct {
		name       string
		statusCode int
		body       string
	}{
		{
			name:       "usage 401",
			statusCode: http.StatusUnauthorized,
			body:       `{"error":{"type":"authentication_error","message":"Invalid authentication credentials"},"provider_body":"codex provider body marker 401"}`,
		},
		{
			name:       "usage 403",
			statusCode: http.StatusForbidden,
			body:       `{"error":{"type":"authentication_error","message":"Invalid authentication credentials"},"provider_body":"codex provider body marker 403"}`,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			manager := coreauth.NewManager(nil, nil, nil)
			exec := &quotaSnapshotTestExecutor{
				provider: "codex",
				responses: map[string]quotaSnapshotTestResponse{
					usageURL: {statusCode: tt.statusCode, body: tt.body},
				},
			}
			manager.RegisterExecutor(exec)
			if _, err := manager.Register(context.Background(), &coreauth.Auth{
				ID:       "codex-oauth",
				Provider: "codex",
			}); err != nil {
				t.Fatalf("Register() error = %v", err)
			}
			handler := NewHandlerWithoutConfigFilePath(nil, manager)
			router := gin.New()
			router.POST("/v0/management/quota/refresh", handler.RefreshQuotaSnapshots)

			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/v0/management/quota/refresh", strings.NewReader(`{"auth_id":"codex-oauth"}`))
			req.Header.Set("Content-Type", "application/json")
			router.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("refresh status = %d, want 200 body=%s", rec.Code, rec.Body.String())
			}
			if got := exec.CallsForAuth("codex-oauth"); got != 1 {
				t.Fatalf("codex quota calls = %d, want 1", got)
			}

			entry := quotaSnapshotEntryForAuth(t, decodeQuotaSnapshotPayload(t, rec), "codex-oauth")
			if entry.Status != quotaRefreshStatusReauthRequired {
				t.Fatalf("entry status = %q, want %q", entry.Status, quotaRefreshStatusReauthRequired)
			}
			if entry.Error != wantErr {
				t.Fatalf("entry error = %q, want sanitized reauth message", entry.Error)
			}
			for _, forbidden := range []string{"401", "403", "authentication_error", "Invalid authentication credentials", "provider body", "provider_body"} {
				if strings.Contains(entry.Error, forbidden) {
					t.Fatalf("entry error leaked %q: %q", forbidden, entry.Error)
				}
			}

			updated, ok := manager.GetByID("codex-oauth")
			if !ok {
				t.Fatal("updated auth missing")
			}
			if updated.Disabled {
				t.Fatal("reauth-required quota refresh must not disable auth")
			}
			if got := metadataString(updated.Metadata, quotaRefreshStatusMetadataKey); got != quotaRefreshStatusReauthRequired {
				t.Fatalf("persisted quota status = %q, want %q", got, quotaRefreshStatusReauthRequired)
			}
			gotErr := metadataString(updated.Metadata, quotaRefreshErrorMetadataKey)
			if gotErr != wantErr {
				t.Fatalf("persisted quota error = %q, want sanitized reauth message", gotErr)
			}
			for _, forbidden := range []string{"401", "403", "authentication_error", "Invalid authentication credentials", "provider body", "provider_body"} {
				if strings.Contains(gotErr, forbidden) {
					t.Fatalf("persisted quota error leaked %q: %q", forbidden, gotErr)
				}
			}
		})
	}
}

func TestQuotaSnapshotsUnauthorizedDoesNotDecodeErrorBodyAndPreservesLastKnownPlan(t *testing.T) {
	t.Parallel()

	gin.SetMode(gin.TestMode)
	manager := coreauth.NewManager(nil, nil, nil)
	exec := &quotaSnapshotTestExecutor{
		provider: "codex",
		responses: map[string]quotaSnapshotTestResponse{
			"https://chatgpt.com/backend-api/wham/usage": {
				statusCode:      http.StatusUnauthorized,
				bodyBytes:       []byte("not a gzip body"),
				contentEncoding: "gzip",
			},
		},
	}
	manager.RegisterExecutor(exec)
	if _, err := manager.Register(context.Background(), &coreauth.Auth{
		ID:       "codex-stale-pro",
		Provider: "codex",
		Metadata: map[string]any{
			quotaSnapshotPlanTypeKey: "pro",
			quotaSnapshotMetadataKey: map[string]any{"usage": map[string]any{"plan_type": "pro"}},
		},
	}); err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	handler := NewHandlerWithoutConfigFilePath(nil, manager)
	router := gin.New()
	router.POST("/v0/management/quota/refresh", handler.RefreshQuotaSnapshots)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v0/management/quota/refresh", strings.NewReader(`{"auth_id":"codex-stale-pro"}`))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("refresh status = %d, want 200 body=%s", rec.Code, rec.Body.String())
	}

	entry := quotaSnapshotEntryForAuth(t, decodeQuotaSnapshotPayload(t, rec), "codex-stale-pro")
	if entry.Status != quotaRefreshStatusReauthRequired {
		t.Fatalf("entry status = %q, want %q", entry.Status, quotaRefreshStatusReauthRequired)
	}
	if entry.PlanType != "pro" {
		t.Fatalf("entry plan_type = %q, want last known pro after quota reauth", entry.PlanType)
	}
	updated, ok := manager.GetByID("codex-stale-pro")
	if !ok {
		t.Fatal("updated auth missing")
	}
	if got := metadataString(updated.Metadata, quotaSnapshotPlanTypeKey); got != "pro" {
		t.Fatalf("persisted plan_type = %q, want last known pro after quota reauth", got)
	}
	// Keep the last successful quota snapshot as stale observability data; the
	// reauth status is recorded separately and must not erase routing capability.
	if _, ok := updated.Metadata[quotaSnapshotMetadataKey]; !ok {
		t.Fatal("quota_snapshot should keep last known data after quota reauth")
	}
	if strings.Contains(rec.Body.String(), "gzip") || strings.Contains(rec.Body.String(), "not a gzip body") {
		t.Fatalf("response leaked error body/decode details: %s", rec.Body.String())
	}
}

func TestQuotaSnapshotsImplicitRefreshSkipsReauthButRefreshesRefreshDisabled(t *testing.T) {
	t.Parallel()

	gin.SetMode(gin.TestMode)
	manager := coreauth.NewManager(nil, nil, nil)
	exec := &quotaSnapshotTestExecutor{provider: "claude"}
	manager.RegisterExecutor(exec)
	past := time.Now().UTC().Add(-time.Minute).Format(time.RFC3339)
	for _, auth := range []*coreauth.Auth{
		{
			ID:       "claude-reauth",
			Provider: "claude",
			Metadata: map[string]any{
				quotaRefreshStatusMetadataKey: quotaRefreshStatusReauthRequired,
				quotaRefreshErrorMetadataKey:  claudeQuotaCredentialUnauthorizedMessage,
				quotaNextRefreshMetadataKey:   past,
			},
		},
		{
			ID:       "claude-refresh-disabled",
			Provider: "claude",
			Metadata: map[string]any{
				"refresh_disabled":          true,
				quotaNextRefreshMetadataKey: past,
			},
		},
		{
			ID:       "claude-active",
			Provider: "claude",
			Metadata: map[string]any{
				quotaNextRefreshMetadataKey: past,
			},
		},
	} {
		if _, err := manager.Register(context.Background(), auth); err != nil {
			t.Fatalf("Register(%s) error = %v", auth.ID, err)
		}
	}
	handler := NewHandlerWithoutConfigFilePath(nil, manager)
	router := gin.New()
	router.POST("/v0/management/quota/refresh", handler.RefreshQuotaSnapshots)

	handler.refreshDueQuotaSnapshots(context.Background(), defaultQuotaSnapshotTestPolicy(), false)

	fullRec := httptest.NewRecorder()
	fullReq := httptest.NewRequest(http.MethodPost, "/v0/management/quota/refresh", strings.NewReader(`{}`))
	fullReq.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(fullRec, fullReq)
	if fullRec.Code != http.StatusOK {
		t.Fatalf("full refresh status = %d, want 200 body=%s", fullRec.Code, fullRec.Body.String())
	}

	providerRec := httptest.NewRecorder()
	providerReq := httptest.NewRequest(http.MethodPost, "/v0/management/quota/refresh", strings.NewReader(`{"provider":"claude"}`))
	providerReq.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(providerRec, providerReq)
	if providerRec.Code != http.StatusOK {
		t.Fatalf("provider refresh status = %d, want 200 body=%s", providerRec.Code, providerRec.Body.String())
	}

	if got := exec.CallsForAuth("claude-reauth"); got != 0 {
		t.Fatalf("reauth auth calls = %d, want 0", got)
	}
	if got := exec.CallsForAuth("claude-refresh-disabled"); got != 6 {
		t.Fatalf("refresh-disabled auth calls = %d, want 6", got)
	}
	if got := exec.CallsForAuth("claude-active"); got != 6 {
		t.Fatalf("active auth calls = %d, want 6", got)
	}
	if got := exec.RefreshCalls(); got != 0 {
		t.Fatalf("quota refresh must not call credential Refresh; calls = %d, want 0", got)
	}
}

func TestQuotaSnapshotEntryMarksRefreshDisabled(t *testing.T) {
	t.Parallel()

	entry := quotaSnapshotEntryFromAuth(&coreauth.Auth{
		ID:       "claude-access-token-only",
		Provider: "claude",
		Metadata: map[string]any{
			"refresh_disabled": true,
		},
	})
	if entry.Status != quotaRefreshStatusRefreshDisabled {
		t.Fatalf("entry status = %q, want %q", entry.Status, quotaRefreshStatusRefreshDisabled)
	}
	if entry.Error != "" {
		t.Fatalf("entry error = %q, want empty", entry.Error)
	}
}

func TestQuotaSnapshotEntryIncludesDisabledFlag(t *testing.T) {
	t.Parallel()

	entry := quotaSnapshotEntryFromAuth(&coreauth.Auth{
		ID:       "claude-disabled",
		Provider: "claude",
		Disabled: true,
		Metadata: map[string]any{
			quotaRefreshStatusMetadataKey: quotaRefreshStatusOK,
			quotaSnapshotMetadataKey:      map[string]any{"profile": map[string]any{}},
		},
	})
	if !entry.Disabled {
		t.Fatal("disabled auth entry should include disabled=true")
	}
}

func TestQuotaSnapshotResponseOmitsZeroRefreshTimesForReauth(t *testing.T) {
	t.Parallel()

	gin.SetMode(gin.TestMode)
	manager := coreauth.NewManager(nil, nil, nil)
	if _, err := manager.Register(context.Background(), &coreauth.Auth{
		ID:       "claude-reauth",
		Provider: "claude",
		Metadata: map[string]any{
			quotaRefreshStatusMetadataKey: quotaRefreshStatusReauthRequired,
			quotaRefreshErrorMetadataKey:  claudeQuotaCredentialUnauthorizedMessage,
		},
	}); err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	handler := NewHandlerWithoutConfigFilePath(nil, manager)
	router := gin.New()
	router.GET("/v0/management/quota/snapshots", handler.GetQuotaSnapshots)

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v0/management/quota/snapshots", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("snapshots status = %d, want 200 body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, forbidden := range []string{"0001-01-01T00:00:00Z", `"last_refreshed_at"`, `"next_refresh_at"`} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("snapshot response leaked zero refresh time %q: %s", forbidden, body)
		}
	}
	entry := quotaSnapshotEntryForAuth(t, decodeQuotaSnapshotPayload(t, rec), "claude-reauth")
	if entry.LastRefreshedAt != nil || entry.NextRefreshAt != nil {
		t.Fatalf("entry refresh times = %v/%v, want nil/nil", entry.LastRefreshedAt, entry.NextRefreshAt)
	}
}

func TestQuotaSnapshotsBulkRefreshReturnsEntriesWhenAllImplicitTargetsSkipped(t *testing.T) {
	t.Parallel()

	gin.SetMode(gin.TestMode)
	manager := coreauth.NewManager(nil, nil, nil)
	exec := &quotaSnapshotTestExecutor{provider: "claude"}
	manager.RegisterExecutor(exec)
	if _, err := manager.Register(context.Background(), &coreauth.Auth{
		ID:       "claude-reauth",
		Provider: "claude",
		Metadata: map[string]any{
			quotaRefreshStatusMetadataKey: quotaRefreshStatusReauthRequired,
			quotaRefreshErrorMetadataKey:  claudeQuotaCredentialUnauthorizedMessage,
		},
	}); err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	handler := NewHandlerWithoutConfigFilePath(nil, manager)
	router := gin.New()
	router.POST("/v0/management/quota/refresh", handler.RefreshQuotaSnapshots)

	for _, body := range []string{`{}`, `{"provider":"claude"}`} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v0/management/quota/refresh", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("bulk refresh %s status = %d, want 200 body=%s", body, rec.Code, rec.Body.String())
		}
		entry := quotaSnapshotEntryForAuth(t, decodeQuotaSnapshotPayload(t, rec), "claude-reauth")
		if entry.Status != quotaRefreshStatusReauthRequired {
			t.Fatalf("entry status = %q, want %q", entry.Status, quotaRefreshStatusReauthRequired)
		}
	}
	if got := exec.CallsForAuth("claude-reauth"); got != 0 {
		t.Fatalf("reauth auth calls = %d, want 0", got)
	}
}

// TestQuotaSnapshotsBackgroundRefreshReprobesRecoveredAuth covers T008: after an
// operator re-authenticates, the credential becomes StatusActive but a stale
// quota_refresh_status=reauth_required may still linger. PR #3 only let an
// explicit user-initiated global refresh re-probe such a credential, so the
// background auto-refresh kept it pinned forever. The background scheduler must
// now also re-probe a recovered (StatusActive) credential once its next-refresh
// schedule is due, while a still-unavailable (non-recovered) credential stays
// skipped.
func TestQuotaSnapshotsBackgroundRefreshReprobesRecoveredAuth(t *testing.T) {
	t.Parallel()

	gin.SetMode(gin.TestMode)
	manager := coreauth.NewManager(nil, nil, nil)
	exec := &quotaSnapshotTestExecutor{provider: "claude"}
	manager.RegisterExecutor(exec)
	overdue := time.Now().UTC().Add(-time.Minute).Format(time.RFC3339)
	if _, err := manager.Register(context.Background(), &coreauth.Auth{
		ID:       "claude-recovered",
		Provider: "claude",
		Status:   coreauth.StatusActive,
		Metadata: map[string]any{
			quotaRefreshStatusMetadataKey: quotaRefreshStatusReauthRequired,
			quotaRefreshErrorMetadataKey:  claudeQuotaCredentialUnauthorizedMessage,
			quotaNextRefreshMetadataKey:   overdue,
		},
	}); err != nil {
		t.Fatalf("Register(recovered) error = %v", err)
	}
	// A still-unavailable credential is not recovered and must stay skipped even
	// when its next-refresh schedule is due.
	if _, err := manager.Register(context.Background(), &coreauth.Auth{
		ID:          "claude-unavailable",
		Provider:    "claude",
		Status:      coreauth.StatusActive,
		Unavailable: true,
		Metadata: map[string]any{
			quotaRefreshStatusMetadataKey: quotaRefreshStatusReauthRequired,
			quotaRefreshErrorMetadataKey:  claudeQuotaCredentialUnauthorizedMessage,
			quotaNextRefreshMetadataKey:   overdue,
		},
	}); err != nil {
		t.Fatalf("Register(unavailable) error = %v", err)
	}
	handler := NewHandlerWithoutConfigFilePath(nil, manager)
	router := gin.New()
	router.GET("/v0/management/quota/snapshots", handler.GetQuotaSnapshots)

	// Background auto-refresh now re-probes the recovered + due credential...
	handler.refreshDueQuotaSnapshots(context.Background(), defaultQuotaSnapshotTestPolicy(), false)
	if got := exec.CallsForAuth("claude-recovered"); got == 0 {
		t.Fatalf("background refresh did not re-probe recovered auth (calls=0)")
	}
	// ...but still skips the unavailable (non-recovered) credential.
	if got := exec.CallsForAuth("claude-unavailable"); got != 0 {
		t.Fatalf("background refresh re-probed unavailable auth, calls = %d, want 0", got)
	}

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v0/management/quota/snapshots", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("snapshots status = %d, want 200 body=%s", rec.Code, rec.Body.String())
	}
	payload := decodeQuotaSnapshotPayload(t, rec)
	recovered := quotaSnapshotEntryForAuth(t, payload, "claude-recovered")
	if recovered.Status != quotaRefreshStatusOK {
		t.Fatalf("recovered entry status = %q, want %q after re-probe", recovered.Status, quotaRefreshStatusOK)
	}
	if recovered.Error != "" {
		t.Fatalf("recovered entry error = %q, want cleared after re-probe", recovered.Error)
	}
	unavailable := quotaSnapshotEntryForAuth(t, payload, "claude-unavailable")
	if unavailable.Status != quotaRefreshStatusReauthRequired {
		t.Fatalf("unavailable entry status = %q, want %q", unavailable.Status, quotaRefreshStatusReauthRequired)
	}
}

func TestQuotaSnapshotsLegacyUnauthorizedErrorMapsToReauthAndRetriesExplicitly(t *testing.T) {
	t.Parallel()

	const legacyError = `quota endpoint returned 401: {"type":"error","error":{"type":"authentication_error","message":"Invalid authentication credentials"},"provider_body":"legacy raw auth failure"}`

	gin.SetMode(gin.TestMode)
	manager := coreauth.NewManager(nil, nil, nil)
	exec := &quotaSnapshotTestExecutor{provider: "claude"}
	manager.RegisterExecutor(exec)
	if _, err := manager.Register(context.Background(), &coreauth.Auth{
		ID:       "claude-legacy",
		Provider: "claude",
		FileName: "claude-legacy.json",
		Metadata: map[string]any{
			quotaRefreshStatusMetadataKey: quotaRefreshStatusError,
			quotaRefreshErrorMetadataKey:  legacyError,
			quotaNextRefreshMetadataKey:   time.Now().UTC().Add(-time.Minute).Format(time.RFC3339),
		},
	}); err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	handler := NewHandlerWithoutConfigFilePath(nil, manager)
	router := gin.New()
	router.GET("/v0/management/quota/snapshots", handler.GetQuotaSnapshots)
	router.POST("/v0/management/quota/refresh", handler.RefreshQuotaSnapshots)

	snapshotRec := httptest.NewRecorder()
	router.ServeHTTP(snapshotRec, httptest.NewRequest(http.MethodGet, "/v0/management/quota/snapshots", nil))
	if snapshotRec.Code != http.StatusOK {
		t.Fatalf("snapshots status = %d, want 200 body=%s", snapshotRec.Code, snapshotRec.Body.String())
	}
	legacyEntry := quotaSnapshotEntryForAuth(t, decodeQuotaSnapshotPayload(t, snapshotRec), "claude-legacy")
	if legacyEntry.Status != quotaRefreshStatusReauthRequired {
		t.Fatalf("legacy entry status = %q, want %q", legacyEntry.Status, quotaRefreshStatusReauthRequired)
	}
	if legacyEntry.Error != claudeQuotaCredentialUnauthorizedMessage {
		t.Fatalf("legacy entry error = %q, want sanitized reauth message", legacyEntry.Error)
	}
	for _, forbidden := range []string{"401", "authentication_error", "Invalid authentication credentials", "provider_body", "legacy raw auth failure"} {
		if strings.Contains(snapshotRec.Body.String(), forbidden) || strings.Contains(legacyEntry.Error, forbidden) {
			t.Fatalf("legacy snapshot leaked %q: entry=%#v body=%s", forbidden, legacyEntry, snapshotRec.Body.String())
		}
	}

	handler.refreshDueQuotaSnapshots(context.Background(), defaultQuotaSnapshotTestPolicy(), false)
	fullRec := httptest.NewRecorder()
	fullReq := httptest.NewRequest(http.MethodPost, "/v0/management/quota/refresh", strings.NewReader(`{}`))
	fullReq.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(fullRec, fullReq)
	if fullRec.Code != http.StatusOK {
		t.Fatalf("full refresh status = %d, want 200 body=%s", fullRec.Code, fullRec.Body.String())
	}
	if got := exec.CallsForAuth("claude-legacy"); got != 0 {
		t.Fatalf("legacy reauth auth implicit calls = %d, want 0", got)
	}
	fullEntry := quotaSnapshotEntryForAuth(t, decodeQuotaSnapshotPayload(t, fullRec), "claude-legacy")
	if fullEntry.Status != quotaRefreshStatusReauthRequired {
		t.Fatalf("full refresh legacy entry status = %q, want %q", fullEntry.Status, quotaRefreshStatusReauthRequired)
	}

	explicitRec := httptest.NewRecorder()
	explicitReq := httptest.NewRequest(http.MethodPost, "/v0/management/quota/refresh", strings.NewReader(`{"auth_id":"claude-legacy"}`))
	explicitReq.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(explicitRec, explicitReq)
	if explicitRec.Code != http.StatusOK {
		t.Fatalf("explicit refresh status = %d, want 200 body=%s", explicitRec.Code, explicitRec.Body.String())
	}
	if got := exec.CallsForAuth("claude-legacy"); got != 2 {
		t.Fatalf("explicit legacy refresh calls = %d, want 2", got)
	}
	entry := quotaSnapshotEntryForAuth(t, decodeQuotaSnapshotPayload(t, explicitRec), "claude-legacy")
	if entry.Status != quotaRefreshStatusOK {
		t.Fatalf("explicit legacy entry status = %q, want %q", entry.Status, quotaRefreshStatusOK)
	}
	if entry.Error != "" {
		t.Fatalf("explicit legacy entry error = %q, want empty", entry.Error)
	}
	updated, ok := manager.GetByID("claude-legacy")
	if !ok {
		t.Fatal("updated auth missing")
	}
	if updated.Disabled {
		t.Fatal("successful explicit legacy retry must not disable auth")
	}
	if got := metadataString(updated.Metadata, quotaRefreshStatusMetadataKey); got != quotaRefreshStatusOK {
		t.Fatalf("persisted quota status = %q, want %q", got, quotaRefreshStatusOK)
	}
	if got := metadataString(updated.Metadata, quotaRefreshErrorMetadataKey); got != "" {
		t.Fatalf("persisted quota error = %q, want empty", got)
	}
}

func TestQuotaSnapshotsExplicitReauthRefreshRetriesAndClearsState(t *testing.T) {
	t.Parallel()

	const (
		profileURL = "https://api.anthropic.com/api/oauth/profile"
		usageURL   = "https://api.anthropic.com/api/oauth/usage"
	)

	tests := []struct {
		name string
		body string
	}{
		{name: "auth_id", body: `{"auth_id":"claude-reauth"}`},
		{name: "name", body: `{"name":"claude-main.json"}`},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			manager := coreauth.NewManager(nil, nil, nil)
			exec := &quotaSnapshotTestExecutor{
				provider: "claude",
				responses: map[string]quotaSnapshotTestResponse{
					profileURL: {body: `{"plan_type":"pro"}`},
					usageURL:   {body: `{"extra_usage":{"is_enabled":true}}`},
				},
			}
			manager.RegisterExecutor(exec)
			if _, err := manager.Register(context.Background(), &coreauth.Auth{
				ID:       "claude-reauth",
				Provider: "claude",
				FileName: "claude-main.json",
				Metadata: map[string]any{
					quotaRefreshStatusMetadataKey: quotaRefreshStatusReauthRequired,
					quotaRefreshErrorMetadataKey:  claudeQuotaCredentialUnauthorizedMessage,
				},
			}); err != nil {
				t.Fatalf("Register() error = %v", err)
			}
			handler := NewHandlerWithoutConfigFilePath(nil, manager)
			router := gin.New()
			router.POST("/v0/management/quota/refresh", handler.RefreshQuotaSnapshots)

			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/v0/management/quota/refresh", strings.NewReader(tt.body))
			req.Header.Set("Content-Type", "application/json")
			router.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("refresh status = %d, want 200 body=%s", rec.Code, rec.Body.String())
			}
			if got := exec.CallsForAuth("claude-reauth"); got != 2 {
				t.Fatalf("explicit refresh calls = %d, want 2", got)
			}

			entry := quotaSnapshotEntryForAuth(t, decodeQuotaSnapshotPayload(t, rec), "claude-reauth")
			if entry.Status != quotaRefreshStatusOK {
				t.Fatalf("entry status = %q, want %q", entry.Status, quotaRefreshStatusOK)
			}
			if entry.Error != "" {
				t.Fatalf("entry error = %q, want empty", entry.Error)
			}
			if entry.PlanType != "pro" {
				t.Fatalf("entry plan_type = %q, want pro", entry.PlanType)
			}

			updated, ok := manager.GetByID("claude-reauth")
			if !ok {
				t.Fatal("updated auth missing")
			}
			if updated.Disabled {
				t.Fatal("successful explicit retry must not disable auth")
			}
			if got := metadataString(updated.Metadata, quotaRefreshStatusMetadataKey); got != quotaRefreshStatusOK {
				t.Fatalf("persisted quota status = %q, want %q", got, quotaRefreshStatusOK)
			}
			if got := metadataString(updated.Metadata, quotaRefreshErrorMetadataKey); got != "" {
				t.Fatalf("persisted quota error = %q, want empty", got)
			}
			if _, ok := updated.Metadata[quotaSnapshotMetadataKey].(map[string]any); !ok {
				t.Fatalf("quota snapshot missing or wrong type: %#v", updated.Metadata[quotaSnapshotMetadataKey])
			}
			if got := metadataString(updated.Metadata, quotaSnapshotPlanTypeKey); got != "pro" {
				t.Fatalf("persisted plan_type = %q, want pro", got)
			}
		})
	}
}

func TestQuotaSnapshotsRefreshNormalizesANSIWrappedJSON(t *testing.T) {
	t.Parallel()

	gin.SetMode(gin.TestMode)
	manager := coreauth.NewManager(nil, nil, nil)
	exec := &quotaSnapshotTestExecutor{
		provider: "codex",
		body:     "\x1b[32mhttps://chatgpt.com/backend-api/wham/usage\n{\"rate_limit\":{\"used_percent\":25},\"plan_type\":\"plus\"}\x1b[0m\n",
	}
	manager.RegisterExecutor(exec)
	if _, err := manager.Register(context.Background(), &coreauth.Auth{
		ID:       "codex-plus",
		Provider: "codex",
		Metadata: map[string]any{"plan_type": "plus"},
	}); err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	handler := NewHandlerWithoutConfigFilePath(nil, manager)
	router := gin.New()
	router.POST("/v0/management/quota/refresh", handler.RefreshQuotaSnapshots)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v0/management/quota/refresh", strings.NewReader(`{"auth_id":"codex-plus"}`))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("refresh status = %d, want 200 body=%s", rec.Code, rec.Body.String())
	}

	updated, ok := manager.GetByID("codex-plus")
	if !ok {
		t.Fatal("updated auth missing")
	}
	if got := metadataString(updated.Metadata, quotaRefreshStatusMetadataKey); got != "ok" {
		t.Fatalf("quota status = %q, want ok", got)
	}
	if got := metadataString(updated.Metadata, quotaRefreshErrorMetadataKey); got != "" {
		t.Fatalf("quota refresh error = %q, want empty", got)
	}
}

func TestQuotaSnapshotsRefreshDecodesBrotliJSON(t *testing.T) {
	t.Parallel()

	gin.SetMode(gin.TestMode)
	manager := coreauth.NewManager(nil, nil, nil)
	exec := &quotaSnapshotTestExecutor{
		provider:        "codex",
		bodyBytes:       brotliEncodeQuotaTest(t, `{"rate_limit":{"used_percent":25},"plan_type":"plus"}`),
		contentEncoding: "br",
	}
	manager.RegisterExecutor(exec)
	if _, err := manager.Register(context.Background(), &coreauth.Auth{
		ID:       "codex-plus",
		Provider: "codex",
		Metadata: map[string]any{"plan_type": "plus"},
	}); err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	handler := NewHandlerWithoutConfigFilePath(nil, manager)
	router := gin.New()
	router.POST("/v0/management/quota/refresh", handler.RefreshQuotaSnapshots)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v0/management/quota/refresh", strings.NewReader(`{"auth_id":"codex-plus"}`))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("refresh status = %d, want 200 body=%s", rec.Code, rec.Body.String())
	}

	updated, ok := manager.GetByID("codex-plus")
	if !ok {
		t.Fatal("updated auth missing")
	}
	if got := metadataString(updated.Metadata, quotaRefreshStatusMetadataKey); got != "ok" {
		t.Fatalf("quota status = %q, want ok", got)
	}
}

func TestNormalizeQuotaJSONPayloadStripsBOM(t *testing.T) {
	t.Parallel()

	var payload map[string]any
	if err := json.Unmarshal(normalizeQuotaJSONPayload([]byte("\xef\xbb\xbf{\"ok\":true}")), &payload); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	if payload["ok"] != true {
		t.Fatalf("payload = %#v, want ok=true", payload)
	}
}

func TestInferCodexPlanTypeUsesCodexNormalizer(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		authPlan  string
		authAttrs map[string]string
		usage     map[string]any
		want      string
	}{
		{
			name:     "metadata chatgpt plus slug",
			authPlan: "chatgpt-plus",
			want:     "plus",
		},
		{
			name:  "usage chatgpt plus label",
			usage: map[string]any{"planType": "ChatGPT Plus"},
			want:  "plus",
		},
		{
			name:  "usage plan prefix",
			usage: map[string]any{"chatgpt_plan_type": "plan_plus"},
			want:  "plus",
		},
		{
			name:  "usage pro",
			usage: map[string]any{"plan_type": "pro"},
			want:  "pro",
		},
		{
			name:     "usage plus overrides stale metadata pro",
			authPlan: "pro",
			usage:    map[string]any{"plan_type": "plus"},
			want:     "plus",
		},
		{
			name:     "usage pro overrides stale metadata plus",
			authPlan: "plus",
			usage:    map[string]any{"plan_type": "pro"},
			want:     "pro",
		},
		{
			name:      "attributes fallback",
			authAttrs: map[string]string{"chatgptPlanType": "ChatGPT Plus"},
			want:      "plus",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			auth := &coreauth.Auth{}
			if tt.authPlan != "" {
				auth.Metadata = map[string]any{"plan_type": tt.authPlan}
			}
			if tt.authAttrs != nil {
				auth.Attributes = tt.authAttrs
			}
			if got := inferCodexPlanType(auth, tt.usage); got != tt.want {
				t.Fatalf("inferCodexPlanType() = %q, want %q", got, tt.want)
			}
		})
	}
}

func brotliEncodeQuotaTest(t *testing.T, text string) []byte {
	t.Helper()
	var buf bytes.Buffer
	writer := brotli.NewWriter(&buf)
	if _, err := writer.Write([]byte(text)); err != nil {
		t.Fatalf("brotli write: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("brotli close: %v", err)
	}
	return buf.Bytes()
}

func decodeQuotaSnapshotPayload(t *testing.T, rec *httptest.ResponseRecorder) quotaSnapshotPayload {
	t.Helper()
	var payload quotaSnapshotPayload
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode quota payload: %v body=%s", err, rec.Body.String())
	}
	return payload
}

func quotaSnapshotEntryForAuth(t *testing.T, payload quotaSnapshotPayload, authID string) quotaSnapshotEntry {
	t.Helper()
	for _, entry := range payload.Entries {
		if entry.AuthID == authID {
			return entry
		}
	}
	t.Fatalf("quota entry for auth %q not found in %#v", authID, payload.Entries)
	return quotaSnapshotEntry{}
}

func quotaRefreshResultForAuth(t *testing.T, payload quotaSnapshotPayload, authID string) quotaRefreshResult {
	t.Helper()
	for _, result := range payload.RefreshResults {
		if result.AuthID == authID {
			return result
		}
	}
	t.Fatalf("quota refresh result for auth %q not found in %#v", authID, payload.RefreshResults)
	return quotaRefreshResult{}
}

func TestQuotaSnapshotAutoRefreshSchedulesMissingNextOnRegularTick(t *testing.T) {
	t.Parallel()

	manager := coreauth.NewManager(nil, nil, nil)
	exec := &quotaSnapshotTestExecutor{provider: "codex"}
	manager.RegisterExecutor(exec)
	if _, err := manager.Register(context.Background(), &coreauth.Auth{
		ID:       "codex-plus",
		Provider: "codex",
		Metadata: map[string]any{"plan_type": "plus"},
	}); err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	handler := NewHandlerWithoutConfigFilePath(nil, manager)

	handler.refreshDueQuotaSnapshots(context.Background(), defaultQuotaSnapshotTestPolicy(), false)
	if exec.Calls() != 0 {
		t.Fatalf("regular auto tick should only schedule jitter; HttpRequest calls = %d, want 0", exec.Calls())
	}

	updated, ok := manager.GetByID("codex-plus")
	if !ok {
		t.Fatal("updated auth missing")
	}
	if _, ok := metadataTime(updated.Metadata, quotaNextRefreshMetadataKey); !ok {
		t.Fatal("next refresh timestamp missing")
	}
}

func TestQuotaSnapshotStartupCatchUpRefreshesMissingNextWhenJitterZero(t *testing.T) {
	t.Parallel()

	manager := coreauth.NewManager(nil, nil, nil)
	exec := &quotaSnapshotTestExecutor{provider: "codex"}
	manager.RegisterExecutor(exec)
	if _, err := manager.Register(context.Background(), &coreauth.Auth{
		ID:       "codex-plus",
		Provider: "codex",
		Metadata: map[string]any{"plan_type": "plus"},
	}); err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	handler := NewHandlerWithoutConfigFilePath(nil, manager)

	handler.refreshDueQuotaSnapshots(context.Background(), immediateStartupQuotaSnapshotTestPolicy(), true)
	if exec.Calls() == 0 {
		t.Fatal("startup catch-up should refresh a missing next_refresh_after when jitter is zero")
	}

	updated, ok := manager.GetByID("codex-plus")
	if !ok {
		t.Fatal("updated auth missing")
	}
	if got := metadataString(updated.Metadata, quotaRefreshStatusMetadataKey); got != quotaRefreshStatusOK {
		t.Fatalf("status = %q, want ok", got)
	}
	if _, ok := updated.Metadata[quotaSnapshotMetadataKey].(map[string]any); !ok {
		t.Fatalf("quota snapshot missing after startup catch-up: %#v", updated.Metadata[quotaSnapshotMetadataKey])
	}
}

func TestQuotaSnapshotStartupCatchUpRefreshesStaleSnapshotWithFutureNext(t *testing.T) {
	t.Parallel()

	manager := coreauth.NewManager(nil, nil, nil)
	exec := &quotaSnapshotTestExecutor{provider: "codex"}
	manager.RegisterExecutor(exec)
	future := time.Now().UTC().Add(defaultQuotaSnapshotRefreshInterval).Format(time.RFC3339)
	stale := time.Now().UTC().Add(-25 * time.Hour).Format(time.RFC3339)
	if _, err := manager.Register(context.Background(), &coreauth.Auth{
		ID:       "codex-stale",
		Provider: "codex",
		Metadata: map[string]any{
			quotaSnapshotMetadataKey:      map[string]any{"usage": map[string]any{"rate_limit": map[string]any{"used_percent": 80}}},
			quotaRefreshStatusMetadataKey: quotaRefreshStatusOK,
			quotaLastRefreshedMetadataKey: stale,
			quotaNextRefreshMetadataKey:   future,
		},
	}); err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	handler := NewHandlerWithoutConfigFilePath(nil, manager)

	handler.refreshDueQuotaSnapshots(context.Background(), immediateStartupQuotaSnapshotTestPolicy(), true)
	if exec.Calls() == 0 {
		t.Fatal("startup catch-up should refresh stale snapshot despite future next_refresh_after")
	}
}

func TestQuotaSnapshotStartupCatchUpRefreshesRefreshDisabledOldOKSnapshot(t *testing.T) {
	t.Parallel()

	manager := coreauth.NewManager(nil, nil, nil)
	exec := &quotaSnapshotTestExecutor{provider: "codex"}
	manager.RegisterExecutor(exec)
	policy := immediateStartupQuotaSnapshotTestPolicy()
	stale := time.Now().UTC().Add(-(policy.StartupMaxStaleness + time.Hour)).Format(time.RFC3339)
	future := time.Now().UTC().Add(defaultQuotaSnapshotRefreshInterval).Format(time.RFC3339)
	if _, err := manager.Register(context.Background(), &coreauth.Auth{
		ID:       "codex-refresh-disabled",
		Provider: "codex",
		Metadata: map[string]any{
			"refresh_disabled":            true,
			quotaSnapshotMetadataKey:      map[string]any{"usage": map[string]any{"rate_limit": map[string]any{"used_percent": 80}}},
			quotaRefreshStatusMetadataKey: quotaRefreshStatusOK,
			quotaLastRefreshedMetadataKey: stale,
			quotaNextRefreshMetadataKey:   future,
			quotaSnapshotPlanTypeKey:      "plus",
		},
	}); err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	handler := NewHandlerWithoutConfigFilePath(nil, manager)

	handler.refreshDueQuotaSnapshots(context.Background(), policy, true)
	if got := exec.CallsForAuth("codex-refresh-disabled"); got != 1 {
		t.Fatalf("refresh-disabled old ok snapshot calls = %d, want 1", got)
	}
	if got := exec.RefreshCalls(); got != 0 {
		t.Fatalf("quota refresh must not call credential Refresh; calls = %d, want 0", got)
	}
	updated, ok := manager.GetByID("codex-refresh-disabled")
	if !ok {
		t.Fatal("updated auth missing")
	}
	if got := metadataString(updated.Metadata, quotaRefreshStatusMetadataKey); got != quotaRefreshStatusOK {
		t.Fatalf("status after catch-up = %q, want ok", got)
	}
	last, ok := metadataTime(updated.Metadata, quotaLastRefreshedMetadataKey)
	if !ok {
		t.Fatal("last refreshed timestamp missing after catch-up")
	}
	oldLast, _ := time.Parse(time.RFC3339, stale)
	if !last.After(oldLast) {
		t.Fatalf("last refreshed = %s, want after stale %s", last.Format(time.RFC3339), stale)
	}
}

func TestQuotaSnapshotStartupCatchUpZeroMaxStalenessSkipsOldOKSnapshot(t *testing.T) {
	t.Parallel()

	manager := coreauth.NewManager(nil, nil, nil)
	exec := &quotaSnapshotTestExecutor{provider: "codex"}
	manager.RegisterExecutor(exec)
	policy := immediateStartupQuotaSnapshotTestPolicy()
	policy.StartupMaxStaleness = 0
	old := time.Now().UTC().Add(-72 * time.Hour).Format(time.RFC3339)
	future := time.Now().UTC().Add(defaultQuotaSnapshotRefreshInterval).Format(time.RFC3339)
	if _, err := manager.Register(context.Background(), &coreauth.Auth{
		ID:       "codex-old-ok",
		Provider: "codex",
		Metadata: map[string]any{
			quotaSnapshotMetadataKey:      map[string]any{"usage": map[string]any{"rate_limit": map[string]any{"used_percent": 80}}},
			quotaRefreshStatusMetadataKey: quotaRefreshStatusOK,
			quotaLastRefreshedMetadataKey: old,
			quotaNextRefreshMetadataKey:   future,
			quotaSnapshotPlanTypeKey:      "plus",
		},
	}); err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	handler := NewHandlerWithoutConfigFilePath(nil, manager)

	handler.refreshDueQuotaSnapshots(context.Background(), policy, true)
	if got := exec.CallsForAuth("codex-old-ok"); got != 0 {
		t.Fatalf("startup max staleness 0 should not refresh old but valid snapshot by age; calls = %d, want 0", got)
	}
}

func TestQuotaSnapshotStartupReschedulesFutureNextWhenPolicyShortens(t *testing.T) {
	t.Parallel()

	manager := coreauth.NewManager(nil, nil, nil)
	exec := &quotaSnapshotTestExecutor{provider: "codex"}
	manager.RegisterExecutor(exec)
	oldNext := time.Now().UTC().Add(45 * time.Minute).Format(time.RFC3339)
	last := time.Now().UTC().Add(-5 * time.Minute).Format(time.RFC3339)
	if _, err := manager.Register(context.Background(), &coreauth.Auth{
		ID:       "codex-short-policy",
		Provider: "codex",
		Metadata: map[string]any{
			quotaSnapshotMetadataKey:      map[string]any{"usage": map[string]any{"rate_limit": map[string]any{"used_percent": 80}}},
			quotaRefreshStatusMetadataKey: quotaRefreshStatusOK,
			quotaLastRefreshedMetadataKey: last,
			quotaNextRefreshMetadataKey:   oldNext,
			quotaSnapshotPlanTypeKey:      "plus",
		},
	}); err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	handler := NewHandlerWithoutConfigFilePath(nil, manager)
	policy := immediateStartupQuotaSnapshotTestPolicy()
	policy.Interval = time.Minute
	policy.Jitter = time.Minute
	policy.StartupMaxStaleness = 24 * time.Hour
	start := time.Now().UTC()

	handler.refreshDueQuotaSnapshots(context.Background(), policy, true)
	if got := exec.CallsForAuth("codex-short-policy"); got != 0 {
		t.Fatalf("policy shortening should reschedule future next without refreshing immediately; calls = %d, want 0", got)
	}
	updated, ok := manager.GetByID("codex-short-policy")
	if !ok {
		t.Fatal("updated auth missing")
	}
	next, ok := metadataTime(updated.Metadata, quotaNextRefreshMetadataKey)
	if !ok {
		t.Fatal("next refresh timestamp missing")
	}
	minNext := start.Add(time.Minute)
	maxNext := start.Add(2*time.Minute + 2*time.Second)
	if next.Before(minNext) || next.After(maxNext) {
		t.Fatalf("next refresh = %s, want within %s..%s", next.Format(time.RFC3339Nano), minNext.Format(time.RFC3339Nano), maxNext.Format(time.RFC3339Nano))
	}
}

func TestQuotaSnapshotRefreshPolicyKeepsZeroStartupMaxStaleness(t *testing.T) {
	t.Parallel()

	policy := QuotaSnapshotRefreshPolicy{
		Enabled:             true,
		Interval:            time.Minute,
		Jitter:              time.Minute,
		StartupCatchUp:      true,
		StartupMaxStaleness: 0,
	}.normalized()
	if policy.StartupMaxStaleness != 0 {
		t.Fatalf("startup max staleness = %s, want 0", policy.StartupMaxStaleness)
	}
	if payload := policy.payload(); payload.StartupMaxStalenessSeconds != 0 {
		t.Fatalf("payload startup max staleness seconds = %d, want 0", payload.StartupMaxStalenessSeconds)
	}
}

func TestQuotaSnapshotMissingExecutorDoesNotPersistUnsupportedForSupportedProvider(t *testing.T) {
	t.Parallel()

	manager := coreauth.NewManager(nil, nil, nil)
	if _, err := manager.Register(context.Background(), &coreauth.Auth{
		ID:       "codex-plus",
		Provider: "codex",
		Metadata: map[string]any{
			quotaNextRefreshMetadataKey: time.Now().UTC().Add(-time.Minute).Format(time.RFC3339),
		},
	}); err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	handler := NewHandlerWithoutConfigFilePath(nil, manager)

	handler.refreshDueQuotaSnapshots(context.Background(), defaultQuotaSnapshotTestPolicy(), false)

	updated, ok := manager.GetByID("codex-plus")
	if !ok {
		t.Fatal("updated auth missing")
	}
	if got := metadataString(updated.Metadata, quotaRefreshStatusMetadataKey); got == quotaRefreshStatusUnsupported {
		t.Fatalf("missing executor must not persist unsupported status for supported provider")
	}
	if got := metadataString(updated.Metadata, quotaRefreshErrorMetadataKey); got == quotaUnsupportedProviderMessage {
		t.Fatalf("missing executor must not persist unsupported provider error")
	}
	next, ok := metadataTime(updated.Metadata, quotaNextRefreshMetadataKey)
	if !ok {
		t.Fatal("next refresh timestamp missing")
	}
	if !next.After(time.Now().UTC()) {
		t.Fatalf("next refresh = %s, want short future retry", next.Format(time.RFC3339))
	}
}

func TestQuotaSnapshotLegacyUnsupportedProviderErrorIsStaleAndRetried(t *testing.T) {
	t.Parallel()

	manager := coreauth.NewManager(nil, nil, nil)
	exec := &quotaSnapshotTestExecutor{provider: "codex"}
	manager.RegisterExecutor(exec)
	future := time.Now().UTC().Add(defaultQuotaSnapshotRefreshInterval).Format(time.RFC3339)
	if _, err := manager.Register(context.Background(), &coreauth.Auth{
		ID:       "codex-plus",
		Provider: "codex",
		Metadata: map[string]any{
			quotaRefreshStatusMetadataKey: quotaRefreshStatusUnsupported,
			quotaRefreshErrorMetadataKey:  quotaUnsupportedProviderMessage,
			quotaNextRefreshMetadataKey:   future,
		},
	}); err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	handler := NewHandlerWithoutConfigFilePath(nil, manager)

	entry := quotaSnapshotEntryFromAuth(&coreauth.Auth{
		ID:       "codex-plus",
		Provider: "codex",
		Metadata: map[string]any{
			quotaRefreshStatusMetadataKey: quotaRefreshStatusUnsupported,
			quotaRefreshErrorMetadataKey:  quotaUnsupportedProviderMessage,
		},
	})
	if entry.Status != quotaRefreshStatusStale || entry.Error != "" {
		t.Fatalf("legacy unsupported entry status/error = %q/%q, want stale/empty", entry.Status, entry.Error)
	}

	handler.refreshDueQuotaSnapshots(context.Background(), defaultQuotaSnapshotTestPolicy(), false)
	if exec.Calls() == 0 {
		t.Fatal("legacy unsupported status should be retried even when next refresh is in the future")
	}

	updated, ok := manager.GetByID("codex-plus")
	if !ok {
		t.Fatal("updated auth missing")
	}
	if got := metadataString(updated.Metadata, quotaRefreshStatusMetadataKey); got != quotaRefreshStatusOK {
		t.Fatalf("status after retry = %q, want ok", got)
	}
}
