package handlers

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

// ginPinContext builds a context carrying a gin request with the given farm pin
// header and authenticated principal, mirroring how GetContextWithCancel embeds
// the gin context under the "gin" key.
func ginPinContext(header, principal string) context.Context {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	if header != "" {
		c.Request.Header.Set(FarmAccountPinHeader, header)
	}
	if principal != "" {
		c.Set("userApiKey", principal)
	}
	//nolint:staticcheck // string key mirrors production handlers.go context wiring.
	return context.WithValue(context.Background(), "gin", c)
}

func TestApplyFarmAccountPin_DisabledIsNoop(t *testing.T) {
	// FARM_PIN_ENABLED unset -> total no-op even when the header is present.
	t.Setenv("FARM_PIN_ENABLED", "")
	h := &BaseAPIHandler{}
	reqMeta := map[string]any{}
	h.applyFarmAccountPin(ginPinContext("some-account", "farm-key"), reqMeta)
	if _, ok := reqMeta[coreexecutor.PinnedAuthMetadataKey]; ok {
		t.Fatalf("pin set while FARM_PIN_ENABLED unset: %#v", reqMeta)
	}
	if _, ok := reqMeta[farmPinMarkerMetadataKey]; ok {
		t.Fatalf("farm marker set while disabled: %#v", reqMeta)
	}
}

func TestApplyFarmAccountPin_EnabledNoAllowListPinsRawValue(t *testing.T) {
	t.Setenv("FARM_PIN_ENABLED", "1")
	h := &BaseAPIHandler{} // no AuthManager -> raw value is pinned as-is (fail-closed if unknown)
	reqMeta := map[string]any{}
	h.applyFarmAccountPin(ginPinContext("acct-123", "any-key"), reqMeta)
	if got := reqMeta[coreexecutor.PinnedAuthMetadataKey]; got != "acct-123" {
		t.Fatalf("PinnedAuthMetadataKey = %v, want acct-123", got)
	}
	if got := reqMeta[farmPinMarkerMetadataKey]; got != "acct-123" {
		t.Fatalf("farmPinMarkerMetadataKey = %v, want acct-123", got)
	}
}

func TestApplyFarmAccountPin_NoHeaderIsNoop(t *testing.T) {
	t.Setenv("FARM_PIN_ENABLED", "1")
	h := &BaseAPIHandler{}
	reqMeta := map[string]any{}
	h.applyFarmAccountPin(ginPinContext("", "any-key"), reqMeta)
	if _, ok := reqMeta[coreexecutor.PinnedAuthMetadataKey]; ok {
		t.Fatalf("pin set without farm header: %#v", reqMeta)
	}
}

func TestApplyFarmAccountPin_AllowListGating(t *testing.T) {
	t.Setenv("FARM_PIN_ENABLED", "true")
	t.Setenv("FARM_PIN_ALLOWED_KEYS", "farm-key-a, farm-key-b")
	h := &BaseAPIHandler{}

	// Principal not in allow-list -> header ignored (anti-abuse).
	rejected := map[string]any{}
	h.applyFarmAccountPin(ginPinContext("acct-9", "outsider-key"), rejected)
	if _, ok := rejected[coreexecutor.PinnedAuthMetadataKey]; ok {
		t.Fatalf("pin honoured for unauthorised principal: %#v", rejected)
	}

	// Principal in allow-list -> header honoured.
	allowed := map[string]any{}
	h.applyFarmAccountPin(ginPinContext("acct-9", "farm-key-b"), allowed)
	if got := allowed[coreexecutor.PinnedAuthMetadataKey]; got != "acct-9" {
		t.Fatalf("PinnedAuthMetadataKey = %v, want acct-9 for allow-listed principal", got)
	}
}

func TestApplyFarmAccountPin_DoesNotOverrideExistingPin(t *testing.T) {
	t.Setenv("FARM_PIN_ENABLED", "1")
	h := &BaseAPIHandler{}
	reqMeta := map[string]any{coreexecutor.PinnedAuthMetadataKey: "websocket-pinned"}
	h.applyFarmAccountPin(ginPinContext("acct-override", "any-key"), reqMeta)
	if got := reqMeta[coreexecutor.PinnedAuthMetadataKey]; got != "websocket-pinned" {
		t.Fatalf("existing pin overridden: got %v, want websocket-pinned", got)
	}
	if _, ok := reqMeta[farmPinMarkerMetadataKey]; ok {
		t.Fatalf("farm marker set when an existing pin was present: %#v", reqMeta)
	}
}

func TestApplyFarmAccountPin_ResolvesAccountNameToAuthID(t *testing.T) {
	t.Setenv("FARM_PIN_ENABLED", "1")
	mgr := coreauth.NewManager(nil, nil, nil)
	if _, err := mgr.Register(context.Background(), &coreauth.Auth{
		ID:       "auth-uuid-xyz",
		Provider: "claude",
		Label:    "farm-node-7",
	}); err != nil {
		t.Fatalf("Register error = %v", err)
	}
	h := &BaseAPIHandler{AuthManager: mgr}
	reqMeta := map[string]any{}
	h.applyFarmAccountPin(ginPinContext("farm-node-7", "any-key"), reqMeta)
	if got := reqMeta[coreexecutor.PinnedAuthMetadataKey]; got != "auth-uuid-xyz" {
		t.Fatalf("PinnedAuthMetadataKey = %v, want resolved auth-uuid-xyz", got)
	}
	if got := reqMeta[farmPinMarkerMetadataKey]; got != "farm-node-7" {
		t.Fatalf("farmPinMarkerMetadataKey = %v, want raw farm-node-7", got)
	}
}

func TestAnnotateFarmPinError(t *testing.T) {
	marker := map[string]any{farmPinMarkerMetadataKey: "acct-1"}

	t.Run("relabels auth_unavailable when pinned", func(t *testing.T) {
		in := &coreauth.Error{Code: "auth_unavailable", Message: "no auth available", HTTPStatus: http.StatusServiceUnavailable}
		out := annotateFarmPinError(in, marker)
		var authErr *coreauth.Error
		if !errors.As(out, &authErr) || authErr == nil {
			t.Fatalf("out is not *coreauth.Error: %v", out)
		}
		if authErr.Code != farmPinAuthUnavailableCode {
			t.Fatalf("code = %q, want %q", authErr.Code, farmPinAuthUnavailableCode)
		}
		if authErr.HTTPStatus != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want %d (must be preserved)", authErr.HTTPStatus, http.StatusServiceUnavailable)
		}
	})

	t.Run("no marker leaves error unchanged", func(t *testing.T) {
		in := &coreauth.Error{Code: "auth_unavailable", Message: "no auth available"}
		if out := annotateFarmPinError(in, map[string]any{}); out != error(in) {
			t.Fatalf("error changed without farm marker: %v", out)
		}
	})

	t.Run("unrelated code left unchanged", func(t *testing.T) {
		in := &coreauth.Error{Code: "executor_not_found", Message: "executor not registered"}
		out := annotateFarmPinError(in, marker)
		var authErr *coreauth.Error
		if !errors.As(out, &authErr) || authErr.Code != "executor_not_found" {
			t.Fatalf("unrelated error was relabelled: %v", out)
		}
	})

	t.Run("non-auth error left unchanged", func(t *testing.T) {
		in := errors.New("boom")
		if out := annotateFarmPinError(in, marker); out != in {
			t.Fatalf("non-auth error changed: %v", out)
		}
	})
}
