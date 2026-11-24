package management

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/pluginhost"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func TestOrdinaryManagementQuotaResetProtectsRecordedLimits(t *testing.T) {
	m := coreauth.NewManager(nil, nil, nil)
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, m)
	host := pluginhost.New()
	pluginCalls := 0
	provider := &mockQuotaProvider{identifier: "quota-fixture", resetFn: func(_ context.Context, req pluginapi.QuotaResetRequest) (pluginapi.QuotaResetResponse, error) {
		t.Error("manual reset reached plugin")
		pluginCalls++
		return pluginapi.QuotaResetResponse{Success: true}, nil
	}}
	host.RegisterPluginForTest("quota-fixture", pluginapi.Plugin{Capabilities: pluginapi.Capabilities{QuotaProvider: provider}})
	h.SetPluginHost(host)
	routes := []struct {
		method, pattern string
		handler         gin.HandlerFunc
	}{
		{"POST", "/v0/management/quota/reset", h.ResetCredentialQuota},
		{"POST", "/v8/management/routing/cooldown/reset", h.ResetQuota},
		{"DELETE", "/v0/management/plugins/:id/quota", h.ResetPluginQuota},
		{"POST", "/v0/management/plugins/:id/quota/reset", h.ResetPluginQuota},
		{"DELETE", "/v8/management/plugins/:id/quota", h.ResetPluginQuota},
	}
	router := gin.New()
	for _, route := range routes {
		router.Handle(route.method, route.pattern, route.handler)
	}
	now := time.Now()
	future := now.Add(time.Hour)
	past := now.Add(-time.Hour)
	fixtures := []*coreauth.Auth{
		{ID: "active429", Status: coreauth.StatusError, Unavailable: true, NextRetryAfter: future, LastError: &coreauth.Error{HTTPStatus: 429}, Quota: coreauth.QuotaState{Exceeded: true, NextRecoverAt: future}},
		{ID: "ambiguous-old-window", Status: coreauth.StatusError, NextRetryAfter: future},
		{ID: "historical-429-not-linked", Status: coreauth.StatusError, Unavailable: true, NextRetryAfter: past, LastError: &coreauth.Error{HTTPStatus: 429}},
		{ID: "legacy-quota", Quota: coreauth.QuotaState{Exceeded: true}},
		{ID: "model-retry-after", ModelStates: map[string]*coreauth.ModelState{"model": {NextRetryAfter: future, LastError: &coreauth.Error{HTTPStatus: 503}}}},
		{ID: "expired", Status: coreauth.StatusError, Unavailable: true, NextRetryAfter: past, LastError: &coreauth.Error{HTTPStatus: 429}, Quota: coreauth.QuotaState{Exceeded: true, NextRecoverAt: past}},
		{ID: "local", Status: coreauth.StatusError, Unavailable: true, LastError: &coreauth.Error{HTTPStatus: 500}},
	}
	for _, fixture := range fixtures {
		fixture.Provider = "quota-fixture"
		if _, err := m.Register(t.Context(), fixture); err != nil {
			t.Fatal(err)
		}
	}
	for _, fixture := range fixtures {
		t.Run(fixture.ID, func(t *testing.T) {
			protected := fixture.ID != "local" && fixture.ID != "expired"
			actual, _ := m.GetByID(fixture.ID)
			for _, route := range routes {
				before, _ := m.GetByID(fixture.ID)
				previousCalls := pluginCalls
				req := httptest.NewRequest(route.method, strings.ReplaceAll(route.pattern, ":id", "quota-fixture"), strings.NewReader(`{"auth_index":"`+actual.Index+`"}`))
				if route.pattern == "/v0/management/quota/reset" && !protected {
					external := httptest.NewRequest("POST", route.pattern, strings.NewReader(`{"auth_index":"`+actual.Index+`","plugin_id":"quota-fixture"}`))
					external.Header.Set("Content-Type", "application/json")
					stopped := httptest.NewRecorder()
					router.ServeHTTP(stopped, external)
					state, _ := m.GetByID(fixture.ID)
					if stopped.Code != 501 || pluginCalls != previousCalls || !reflect.DeepEqual(before, state) {
						t.Fatal("explicit plugin operation was not safely unavailable")
					}
				}
				req.Header.Set("Content-Type", "application/json")
				out := httptest.NewRecorder()
				router.ServeHTTP(out, req)
				after, _ := m.GetByID(fixture.ID)
				if protected {
					if out.Code != http.StatusConflict || pluginCalls != previousCalls || !reflect.DeepEqual(before, after) {
						t.Fatalf("protected selection changed: %s %s status=%d", route.method, route.pattern, out.Code)
					}
				} else if strings.Contains(route.pattern, "/plugins/") {
					if out.Code != 501 || pluginCalls != previousCalls || !reflect.DeepEqual(before, after) {
						t.Fatal("plugin route pretended to reset or mutated the local record")
					}
				} else if out.Code != http.StatusOK || after.Status != coreauth.StatusActive || after.Unavailable || after.LastError != nil ||
					(route.pattern == "/v0/management/quota/reset" && !strings.Contains(out.Body.String(), `"local_only":true`)) {
					t.Fatalf("allowed local or expired reset failed: %s %s status=%d", route.method, route.pattern, out.Code)
				}
			}
		})
	}
}
