package executor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	_ "github.com/router-for-me/CLIProxyAPI/v7/internal/translator"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

// drainStream 把 ExecuteStream 返回的 chunk 通道消费完，遇到非致命错误也忽略，
// 仅为让 SSE 完整扫描后触发 cyber_policy 回写。
func drainStream(t *testing.T, result *cliproxyexecutor.StreamResult) {
	t.Helper()
	if result == nil {
		return
	}
	for chunk := range result.Chunks {
		_ = chunk
	}
}

// newCyberPolicyTestExecutor 用 NewCodexExecutorWithManager 构造 executor，
// 同时返回受 Manager 管理的 auth 引用，供测试断言计数字段。
func newCyberPolicyTestExecutor(t *testing.T, serverURL string) (*CodexExecutor, *cliproxyauth.Manager, *cliproxyauth.Auth) {
	t.Helper()
	manager := cliproxyauth.NewManager(nil, nil, nil)
	auth := &cliproxyauth.Auth{ProxyURL: "direct",
		ID:       "test-auth-cyber-policy",
		Provider: "codex",
		Attributes: map[string]string{
			"base_url": serverURL,
			"api_key":  "test",
		},
	}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("register auth: %v", err)
	}
	executor := NewCodexExecutorWithManager(&config.Config{}, manager)
	return executor, manager, auth
}

// runCyberPolicyStream 触发一次 ExecuteStream 并返回 manager 内 auth 的最新快照。
func runCyberPolicyStream(t *testing.T, executor *CodexExecutor, manager *cliproxyauth.Manager, authID, serverURL string) *cliproxyauth.Auth {
	t.Helper()
	result, err := executor.ExecuteStream(context.Background(), &cliproxyauth.Auth{ProxyURL: "direct",
		ID:       authID,
		Provider: "codex",
		Attributes: map[string]string{
			"base_url": serverURL,
			"api_key":  "test",
		},
	}, cliproxyexecutor.Request{
		Model:   "gpt-5.4-mini",
		Payload: []byte(`{"model":"gpt-5.4-mini","input":"hi"}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai-response"),
		Stream:       true,
	})
	if err != nil {
		t.Fatalf("ExecuteStream error: %v", err)
	}
	drainStream(t, result)
	updated, ok := manager.GetByID(authID)
	if !ok || updated == nil {
		t.Fatalf("auth %q missing from manager after stream", authID)
	}
	return updated
}

func TestCodexExecutorExecuteStream_CyberPolicyErrorEventIncrementsCount(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		// 单独的 type=error 事件，错误体里 code=cyber_policy。
		_, _ = w.Write([]byte(`data: {"type":"error","error":{"code":"cyber_policy","message":"flagged"}}` + "\n\n"))
	}))
	defer server.Close()

	executor, manager, auth := newCyberPolicyTestExecutor(t, server.URL)

	updated := runCyberPolicyStream(t, executor, manager, auth.ID, server.URL)
	if updated.CyberPolicyFlagCount != 1 {
		t.Fatalf("CyberPolicyFlagCount = %d, want 1", updated.CyberPolicyFlagCount)
	}
	if updated.LastCyberPolicyAt.IsZero() {
		t.Fatalf("LastCyberPolicyAt is zero, want non-zero")
	}
}

func TestCodexExecutorExecuteStream_CyberPolicyDualEventIdempotent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		// 上游常见序列：先 type=error，再 type=response.failed，二者都含 cyber_policy。
		_, _ = w.Write([]byte(`data: {"type":"error","error":{"code":"cyber_policy","message":"flagged"}}` + "\n\n"))
		_, _ = w.Write([]byte(`data: {"type":"response.failed","response":{"error":{"code":"cyber_policy","message":"flagged"}}}` + "\n\n"))
	}))
	defer server.Close()

	executor, manager, auth := newCyberPolicyTestExecutor(t, server.URL)

	updated := runCyberPolicyStream(t, executor, manager, auth.ID, server.URL)
	if updated.CyberPolicyFlagCount != 1 {
		t.Fatalf("CyberPolicyFlagCount = %d, want 1 (idempotent within one stream)", updated.CyberPolicyFlagCount)
	}
	if updated.LastCyberPolicyAt.IsZero() {
		t.Fatalf("LastCyberPolicyAt is zero, want non-zero")
	}
}

func TestCodexExecutorExecuteStream_NoCyberPolicyLeavesCounterZero(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"ok\"}]},\"output_index\":0}\n"))
		_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"object\":\"response\",\"created_at\":1775555723,\"status\":\"completed\",\"model\":\"gpt-5.4-mini-2026-03-17\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n"))
	}))
	defer server.Close()

	executor, manager, auth := newCyberPolicyTestExecutor(t, server.URL)

	updated := runCyberPolicyStream(t, executor, manager, auth.ID, server.URL)
	if updated.CyberPolicyFlagCount != 0 {
		t.Fatalf("CyberPolicyFlagCount = %d, want 0", updated.CyberPolicyFlagCount)
	}
	if !updated.LastCyberPolicyAt.IsZero() {
		t.Fatalf("LastCyberPolicyAt = %v, want zero", updated.LastCyberPolicyAt)
	}
}
