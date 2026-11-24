package executor

import (
	"net/http"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestOrdinaryAntigravitySDKUsesBoundAuthorizationAndProtocolHeaders(t *testing.T) {
	executor := NewAntigravityExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{"base_url": "https://fixture.invalid"},
		Metadata: map[string]any{"access_token": "fixture-bearer", "project_id": "explicit-project"}}
	request, err := http.NewRequest(http.MethodPost, "https://fixture.invalid/ordinary", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer caller-override")
	request.Header.Set("X-Fixture-Private-Header", "ignored")
	if err := executor.PrepareRequest(request, auth); err != nil {
		t.Fatal(err)
	}
	if request.Header.Get("Accept") != "application/json" || request.Header.Get("Content-Type") != "application/json" ||
		request.Header.Get("Authorization") != "Bearer fixture-bearer" || request.Header.Get("X-Fixture-Private-Header") != "" {
		t.Fatal("standard headers or selected credential changed")
	}
}
