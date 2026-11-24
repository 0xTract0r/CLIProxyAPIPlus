package executor

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	authsdk "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	executorapi "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	translation "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

const ordinaryAntigravityManagerModel = "fixture-model"

func executeOrdinaryAntigravityManager(t *testing.T, status int, headers http.Header, response string) (*authsdk.Auth, error) {
	t.Helper()
	cfg := &config.Config{}
	manager := authsdk.NewManager(nil, &authsdk.RoundRobinSelector{}, nil)
	manager.SetConfig(cfg)
	manager.SetRetryConfig(0, 0, 0)
	manager.RegisterExecutor(NewAntigravityExecutor(cfg))
	auth := ordinaryPortAuth()
	models := registry.GetGlobalRegistry()
	models.RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{
		{ID: ordinaryAntigravityManagerModel}, {ID: "fixture-secondary"},
	})
	t.Cleanup(func() { models.UnregisterClient(auth.ID) })
	if _, err := manager.Register(t.Context(), auth); err != nil {
		t.Fatal(err)
	}
	sends := 0
	ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", ordinaryAntigravityRT(func(req *http.Request) (*http.Response, error) {
		sends++
		if req.URL.Host != "fixture.invalid" || req.Header.Get("Authorization") != "Bearer fixture" {
			t.Fatal("ordinary manager changed the configured destination or credential")
		}
		return &http.Response{StatusCode: status, Header: headers.Clone(),
			Body: io.NopCloser(strings.NewReader(response)), Request: req}, nil
	}))
	_, err := manager.Execute(ctx, []string{auth.Provider},
		executorapi.Request{Model: ordinaryAntigravityManagerModel, Payload: []byte(`{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}`)},
		executorapi.Options{SourceFormat: translation.FormatGemini, ResponseFormat: translation.FormatAntigravity})
	after, found := manager.GetByID(auth.ID)
	if sends != 1 || !found {
		t.Fatalf("expected one upstream dispatch for the selected credential, got %d", sends)
	}
	return after, err
}

func TestAntigravityIssue7821_MarkResultHonorsRetryAfterHeaderWindow(t *testing.T) {
	const delay = 23 * time.Second
	before := time.Now()
	auth, err := executeOrdinaryAntigravityManager(t, http.StatusTooManyRequests, http.Header{"Retry-After": {"23"}},
		`{"error":{"status":"RESOURCE_EXHAUSTED","message":"ordinary wait"}}`)
	var retry interface{ RetryAfter() *time.Duration }
	var status interface{ StatusCode() int }
	if !errors.As(err, &status) || status.StatusCode() != http.StatusTooManyRequests ||
		!errors.As(err, &retry) || retry.RetryAfter() == nil ||
		*retry.RetryAfter() > delay || *retry.RetryAfter() < delay-time.Since(before) {
		t.Fatalf("manager did not expose the remaining provider wait: %v", err)
	}
	state := auth.ModelStates[ordinaryAntigravityManagerModel]
	if state == nil || !state.Unavailable || !state.Quota.Exceeded ||
		state.NextRetryAfter.Before(before.Add(delay)) || state.NextRetryAfter.After(time.Now().Add(delay)) {
		t.Fatal("published selected-model deadline did not follow the provider Retry-After")
	}
}
