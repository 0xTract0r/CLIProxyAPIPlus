package handlers

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/constant"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

func TestOrdinaryRefusalsEndStreamBootstrap(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusUnauthorized, http.StatusForbidden, http.StatusTooManyRequests} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			const model, provider = "ordinary-stream-fixture", "codex"
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			var calls atomic.Int32
			executor := &modelExecutionCaptureExecutor{
				provider: provider,
				stream: func(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (*coreexecutor.StreamResult, error) {
					calls.Add(1)
					ch := make(chan coreexecutor.StreamChunk, 1)
					if status == http.StatusOK {
						ch <- coreexecutor.StreamChunk{Payload: []byte(`data: {"type":"response.completed"}`)}
					} else {
						ch <- coreexecutor.StreamChunk{Err: &modelExecutionStatusHeaderError{
							statusCode: status, message: "synthetic status", headers: http.Header{"Retry-After": {"61"}, "X-Private-Fixture": {"private"}},
						}}
					}
					close(ch)
					return &coreexecutor.StreamResult{Chunks: ch}, nil
				},
			}
			manager := coreauth.NewManager(nil, nil, nil)
			manager.RegisterExecutor(executor)
			manager.SetRetryConfig(0, 0, 1)
			id := fmt.Sprintf("ordinary-stream-%d", status)
			if _, err := manager.Register(ctx, &coreauth.Auth{ID: id, Provider: provider, Status: coreauth.StatusActive}); err != nil {
				t.Fatal(err)
			}
			registry.GetGlobalRegistry().RegisterClient(id, provider, []*registry.ModelInfo{{ID: model}})
			t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
			h := NewBaseAPIHandlers(&sdkconfig.SDKConfig{Streaming: sdkconfig.StreamingConfig{BootstrapRetries: 2}}, manager)
			raw := []byte(`{"model":"ordinary-stream-fixture","messages":[{"role":"user","content":"fixture"}]}`)
			if status == http.StatusOK {
				payload, _, errMsg := h.ExecuteCountWithAuthManager(ctx, constant.OpenAI, model, raw, "")
				if errMsg != nil || string(payload) != "0" {
					t.Fatal("normal count did not reach executor")
				}
			}
			stream, errMsg := h.ExecuteModelStream(ctx, ModelExecutionRequest{
				EntryProtocol: "openai", ExitProtocol: "openai", Model: model, Stream: true, Body: raw,
			})
			if status != http.StatusOK {
				if errMsg == nil || errMsg.StatusCode != status || errMsg.Addon.Get("Retry-After") != "61" || len(errMsg.Addon) != 1 {
					t.Fatalf("refusal status/headers did not survive: %+v", errMsg)
				}
			} else {
				if errMsg != nil || stream.Chunks == nil {
					t.Fatalf("ordinary stream did not start: %+v", errMsg)
				}
				var payload strings.Builder
				for chunk := range stream.Chunks {
					if chunk.Err != nil {
						t.Fatal(chunk.Err)
					}
					payload.Write(chunk.Payload)
				}
				if !strings.Contains(payload.String(), "response.completed") {
					t.Fatal("normal completion missing")
				}
			}
			if calls.Load() != 1 {
				t.Fatalf("executor called %d times", calls.Load())
			}
		})
	}
}

func TestOrdinaryRefusalRetryAfterValidation(t *testing.T) {
	const date = "Wed, 21 Oct 2015 07:28:00 GMT"
	for _, tc := range []struct {
		name       string
		value      []string
		connection string
		want       string
	}{
		{"seconds", []string{"0"}, "", "0"},
		{"HTTP date", []string{date}, "", date},
		{"obsolete valid GMT", []string{"Wednesday, 21-Oct-15 07:28:00 GMT"}, "", "Wednesday, 21-Oct-15 07:28:00 GMT"},
		{"non GMT", []string{"Wednesday, 21-Oct-15 07:28:00 PST"}, "", ""},
		{"missing", nil, "", ""},
		{"negative", []string{"-1"}, "", ""},
		{"duplicate", []string{"1", "2"}, "", ""},
		{"invalid", []string{"a"}, "", ""},
		{"connection scoped", []string{"1"}, "retrY-after", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := fmt.Errorf("outer: %w", &modelExecutionStatusHeaderError{
				statusCode: http.StatusUnauthorized, headers: http.Header{"retry-after": tc.value, "Connection": {tc.connection}, "X-Private-Fixture": {"private"}},
			})
			result := executionErrorMessage(err)
			if result.Addon.Get("Retry-After") != tc.want || result.Addon.Get("X-Private-Fixture") != "" {
				t.Fatalf("unexpected refusal headers: %v", result.Addon)
			}
		})
	}
}

func TestOrdinaryRefusalKeepsExistingPublicChallenge(t *testing.T) {
	err := &modelExecutionStatusHeaderError{statusCode: http.StatusUnauthorized, headers: http.Header{"WWW-Authenticate": {`Bearer realm="fixture"`}, "X-Private-Fixture": {"private"}}}
	result := executionErrorMessage(err)
	if result.Addon.Get("WWW-Authenticate") != `Bearer realm="fixture"` || result.Addon.Get("Retry-After") != "" || len(result.Addon) != 1 {
		t.Fatalf("unexpected authentication headers: %v", result.Addon)
	}
}
