package executor

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestCodexExecutorCacheHelper_OpenAIChatCompletions_StablePromptCacheKeyFromAPIKey(t *testing.T) {
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Set("userApiKey", "test-api-key")

	ctx := context.WithValue(context.Background(), "gin", ginCtx)
	executor := &CodexExecutor{}
	rawJSON := []byte(`{"model":"gpt-5.3-codex","stream":true}`)
	req := cliproxyexecutor.Request{
		Model:   "gpt-5.3-codex",
		Payload: []byte(`{"model":"gpt-5.3-codex"}`),
	}
	url := "https://example.com/responses"

	httpReq, _, _, err := executor.cacheHelper(ctx, sdktranslator.FromString("openai"), url, nil, req, req.Payload, rawJSON)
	if err != nil {
		t.Fatalf("cacheHelper error: %v", err)
	}

	body, errRead := io.ReadAll(httpReq.Body)
	if errRead != nil {
		t.Fatalf("read request body: %v", errRead)
	}

	expectedKey := uuid.NewSHA1(uuid.NameSpaceOID, []byte("cli-proxy-api:codex:prompt-cache:test-api-key")).String()
	gotKey := gjson.GetBytes(body, "prompt_cache_key").String()
	if gotKey != expectedKey {
		t.Fatalf("prompt_cache_key = %q, want %q", gotKey, expectedKey)
	}
	if gotConversation := httpReq.Header.Get("Conversation_id"); gotConversation != "" {
		t.Fatalf("Conversation_id = %q, want empty", gotConversation)
	}
	if gotSession := httpReq.Header["Session_id"]; len(gotSession) != 1 || gotSession[0] != expectedKey {
		t.Fatalf("Session_id = %#v, want [%q]", gotSession, expectedKey)
	}
	if gotCanonicalSession := httpReq.Header.Get("Session-Id"); gotCanonicalSession != "" {
		t.Fatalf("Session-Id = %q, want empty", gotCanonicalSession)
	}

	httpReq2, _, _, err := executor.cacheHelper(ctx, sdktranslator.FromString("openai"), url, nil, req, req.Payload, rawJSON)
	if err != nil {
		t.Fatalf("cacheHelper error (second call): %v", err)
	}
	body2, errRead2 := io.ReadAll(httpReq2.Body)
	if errRead2 != nil {
		t.Fatalf("read request body (second call): %v", errRead2)
	}
	gotKey2 := gjson.GetBytes(body2, "prompt_cache_key").String()
	if gotKey2 != expectedKey {
		t.Fatalf("prompt_cache_key (second call) = %q, want %q", gotKey2, expectedKey)
	}
}

func TestCodexExecutorCacheHelper_ClaudeUsesClaudeCodeSessionID(t *testing.T) {
	executor := &CodexExecutor{}
	ctx := context.Background()
	url := "https://example.com/responses"
	rawJSON := []byte(`{"model":"gpt-5.4","stream":true}`)
	firstReq := cliproxyexecutor.Request{
		Model: "gpt-5.4-claude-cache-session",
		Payload: []byte(`{
			"model":"gpt-5.4",
			"metadata":{"user_id":"{\"device_id\":\"device-a\",\"account_uuid\":\"\",\"session_id\":\"cache-session-1\"}"},
			"messages":[{"role":"user","content":[{"type":"text","text":"first"}]}]
		}`),
	}
	secondReq := cliproxyexecutor.Request{
		Model: "gpt-5.4-claude-cache-session",
		Payload: []byte(`{
			"model":"gpt-5.4",
			"metadata":{"user_id":"{\"device_id\":\"device-b\",\"account_uuid\":\"\",\"session_id\":\"cache-session-1\"}"},
			"messages":[{"role":"user","content":[{"type":"text","text":"next"}]}]
		}`),
	}

	firstHTTPReq, _, _, err := executor.cacheHelper(ctx, sdktranslator.FromString("claude"), url, nil, firstReq, firstReq.Payload, rawJSON)
	if err != nil {
		t.Fatalf("cacheHelper first error: %v", err)
	}
	secondHTTPReq, _, _, err := executor.cacheHelper(ctx, sdktranslator.FromString("claude"), url, nil, secondReq, secondReq.Payload, rawJSON)
	if err != nil {
		t.Fatalf("cacheHelper second error: %v", err)
	}

	firstBody, errRead := io.ReadAll(firstHTTPReq.Body)
	if errRead != nil {
		t.Fatalf("read first request body: %v", errRead)
	}
	secondBody, errRead := io.ReadAll(secondHTTPReq.Body)
	if errRead != nil {
		t.Fatalf("read second request body: %v", errRead)
	}
	firstKey := gjson.GetBytes(firstBody, "prompt_cache_key").String()
	secondKey := gjson.GetBytes(secondBody, "prompt_cache_key").String()
	if firstKey == "" {
		t.Fatalf("first prompt_cache_key is empty; body=%s", string(firstBody))
	}
	if secondKey != firstKey {
		t.Fatalf("same Claude Code session_id produced different prompt_cache_key: first=%q second=%q", firstKey, secondKey)
	}
	if gotSession := firstHTTPReq.Header["Session_id"]; len(gotSession) != 1 || gotSession[0] != firstKey {
		t.Fatalf("first Session_id = %#v, want [%q]", gotSession, firstKey)
	}
	if gotSession := secondHTTPReq.Header["Session_id"]; len(gotSession) != 1 || gotSession[0] != firstKey {
		t.Fatalf("second Session_id = %#v, want [%q]", gotSession, firstKey)
	}
}

func TestCodexExecutorCacheHelper_ClaudeRejectsBareUserID(t *testing.T) {
	executor := &CodexExecutor{}
	req := cliproxyexecutor.Request{
		Model:   "gpt-5.4-claude-cache-bare-user",
		Payload: []byte(`{"model":"gpt-5.4","metadata":{"user_id":"same-user-across-chats"},"messages":[{"role":"user","content":[{"type":"text","text":"first"}]}]}`),
	}

	httpReq, _, _, err := executor.cacheHelper(context.Background(), sdktranslator.FromString("claude"), "https://example.com/responses", nil, req, req.Payload, []byte(`{"model":"gpt-5.4","stream":true}`))
	if err != nil {
		t.Fatalf("cacheHelper error: %v", err)
	}

	body, errRead := io.ReadAll(httpReq.Body)
	if errRead != nil {
		t.Fatalf("read request body: %v", errRead)
	}
	if got := gjson.GetBytes(body, "prompt_cache_key").String(); got != "" {
		t.Fatalf("bare metadata.user_id must not create prompt_cache_key, got %q; body=%s", got, string(body))
	}
	if got := httpReq.Header["Session_id"]; len(got) != 0 {
		t.Fatalf("bare metadata.user_id must not create Session_id, got %#v", got)
	}
	if got := httpReq.Header.Get("Session-Id"); got != "" {
		t.Fatalf("bare metadata.user_id must not create Session-Id, got %q", got)
	}
}

func TestCodexExecutorCacheHelper_IdentityConfuseRemapsBodyAndHeaders(t *testing.T) {
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Request = httptest.NewRequest("POST", "/v1/responses", nil)
	ginCtx.Request.Header.Set("X-Codex-Turn-Metadata", `{"prompt_cache_key":"cache-1","turn_id":"turn-1","window_id":"cache-1:0"}`)
	ginCtx.Request.Header.Set("X-Client-Request-Id", "client-request-1")

	ctx := context.WithValue(context.Background(), "gin", ginCtx)
	executor := &CodexExecutor{cfg: &config.Config{
		Routing: config.RoutingConfig{Strategy: "fill-first"},
		Codex:   config.CodexConfig{IdentityConfuse: true},
	}}
	auth := &cliproxyauth.Auth{ProxyURL: "direct", ID: "auth-1", Provider: "codex"}
	rawJSON := []byte(`{"model":"gpt-5-codex","stream":true,"client_metadata":{"x-codex-turn-metadata":"{\"prompt_cache_key\":\"cache-1\",\"turn_id\":\"turn-1\",\"window_id\":\"cache-1:0\"}","x-codex-window-id":"cache-1:0"}}`)
	req := cliproxyexecutor.Request{
		Model:   "gpt-5-codex",
		Payload: []byte(`{"model":"gpt-5-codex","prompt_cache_key":"cache-1","client_metadata":{"x-codex-installation-id":"install-1"}}`),
	}
	url := "https://example.com/responses"

	httpReq, body, identityState, err := executor.cacheHelper(ctx, sdktranslator.FromString("openai-response"), url, auth, req, req.Payload, rawJSON)
	if err != nil {
		t.Fatalf("cacheHelper error: %v", err)
	}
	applyCodexHeaders(httpReq, auth, "oauth-token", true, executor.cfg)
	applyCodexIdentityConfuseHeaders(httpReq.Header, &identityState)

	expectedPromptCacheKey := codexIdentityConfuseUUID("auth-1", "prompt-cache", "cache-1")
	expectedTurnID := codexIdentityConfuseUUID("auth-1", "turn", "turn-1")
	if gotKey := gjson.GetBytes(body, "prompt_cache_key").String(); gotKey != expectedPromptCacheKey {
		t.Fatalf("prompt_cache_key = %q, want %q", gotKey, expectedPromptCacheKey)
	}
	expectedInstallationID := codexIdentityConfuseUUID("auth-1", "installation", "install-1")
	if gotID := gjson.GetBytes(body, "client_metadata.x-codex-installation-id").String(); gotID != expectedInstallationID {
		t.Fatalf("installation id = %q, want %q", gotID, expectedInstallationID)
	}
	gotBodyMetadata := gjson.GetBytes(body, "client_metadata.x-codex-turn-metadata").String()
	if gotMetadataPromptCacheKey := gjson.Get(gotBodyMetadata, "prompt_cache_key").String(); gotMetadataPromptCacheKey != expectedPromptCacheKey {
		t.Fatalf("client_metadata.x-codex-turn-metadata.prompt_cache_key = %q, want %q", gotMetadataPromptCacheKey, expectedPromptCacheKey)
	}
	if gotMetadataTurnID := gjson.Get(gotBodyMetadata, "turn_id").String(); gotMetadataTurnID != expectedTurnID {
		t.Fatalf("client_metadata.x-codex-turn-metadata.turn_id = %q, want %q", gotMetadataTurnID, expectedTurnID)
	}
	if gotMetadataWindowID := gjson.Get(gotBodyMetadata, "window_id").String(); gotMetadataWindowID != expectedPromptCacheKey+":0" {
		t.Fatalf("client_metadata.x-codex-turn-metadata.window_id = %q, want %q", gotMetadataWindowID, expectedPromptCacheKey+":0")
	}
	if gotWindowID := gjson.GetBytes(body, "client_metadata.x-codex-window-id").String(); gotWindowID != expectedPromptCacheKey+":0" {
		t.Fatalf("client_metadata.x-codex-window-id = %q, want %q", gotWindowID, expectedPromptCacheKey+":0")
	}
	if gotHeader := httpReq.Header["Session_id"]; len(gotHeader) != 1 || gotHeader[0] != expectedPromptCacheKey {
		t.Fatalf("Session_id = %#v, want [%q]", gotHeader, expectedPromptCacheKey)
	}
	for _, headerName := range []string{"X-Client-Request-Id", "Thread-Id"} {
		if gotHeader := httpReq.Header.Get(headerName); gotHeader != expectedPromptCacheKey {
			t.Fatalf("%s = %q, want %q", headerName, gotHeader, expectedPromptCacheKey)
		}
	}
	if gotCanonicalSession := httpReq.Header.Get("Session-Id"); gotCanonicalSession != "" {
		t.Fatalf("Session-Id = %q, want empty", gotCanonicalSession)
	}
	if gotWindow := httpReq.Header.Get("X-Codex-Window-Id"); gotWindow != expectedPromptCacheKey+":0" {
		t.Fatalf("X-Codex-Window-Id = %q, want %q", gotWindow, expectedPromptCacheKey+":0")
	}
	gotHeaderMetadata := httpReq.Header.Get("X-Codex-Turn-Metadata")
	if gotMetadataPromptCacheKey := gjson.Get(gotHeaderMetadata, "prompt_cache_key").String(); gotMetadataPromptCacheKey != expectedPromptCacheKey {
		t.Fatalf("X-Codex-Turn-Metadata.prompt_cache_key = %q, want %q", gotMetadataPromptCacheKey, expectedPromptCacheKey)
	}
	if gotMetadataTurnID := gjson.Get(gotHeaderMetadata, "turn_id").String(); gotMetadataTurnID != expectedTurnID {
		t.Fatalf("X-Codex-Turn-Metadata.turn_id = %q, want %q", gotMetadataTurnID, expectedTurnID)
	}
	if gotMetadataWindowID := gjson.Get(gotHeaderMetadata, "window_id").String(); gotMetadataWindowID != expectedPromptCacheKey+":0" {
		t.Fatalf("X-Codex-Turn-Metadata.window_id = %q, want %q", gotMetadataWindowID, expectedPromptCacheKey+":0")
	}
}

func TestApplyCodexHeadersUsesAccountHeaderForOAuth(t *testing.T) {
	httpReq := httptest.NewRequest("POST", "https://example.com/responses", nil)
	auth := &cliproxyauth.Auth{ProxyURL: "direct",
		Provider: "codex",
		Metadata: map[string]any{"account_id": "acct-1"},
	}

	applyCodexHeaders(httpReq, auth, "oauth-token", true, nil)

	if got := httpReq.Header.Get("Chatgpt-Account-Id"); got != "acct-1" {
		t.Fatalf("Chatgpt-Account-Id = %q, want acct-1", got)
	}
}

// newCodexHeadersTestRequest 构造一个携带指定客户端 header（含 Originator）的 gin 上下文
// 请求，供 A-1 Originator 钉死测试驱动 applyCodexHeaders。
func newCodexHeadersTestRequest(t *testing.T, clientHeaders map[string]string) *http.Request {
	t.Helper()
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Request = httptest.NewRequest("POST", "/v1/responses", nil)
	for name, value := range clientHeaders {
		ginCtx.Request.Header.Set(name, value)
	}
	ctx := context.WithValue(context.Background(), "gin", ginCtx)
	httpReq, err := http.NewRequestWithContext(ctx, "POST", "https://example.com/responses", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	return httpReq
}

// TestApplyCodexHeaders_ManagedOriginatorPinnedAgainstForgedClientValue 覆盖 A-1：
// managed（非 api-key）账号下，客户端传入的非法 Originator 必须被钉死覆盖为 managed 值，
// 不能透传污染出站身份。
func TestApplyCodexHeaders_ManagedOriginatorPinnedAgainstForgedClientValue(t *testing.T) {
	auth := &cliproxyauth.Auth{ProxyURL: "direct", ID: "managed-codex-auth", Provider: "codex"}
	httpReq := newCodexHeadersTestRequest(t, map[string]string{"Originator": "evil-client"})

	applyCodexHeaders(httpReq, auth, "oauth-token", true, nil)

	if got := httpReq.Header.Get("Originator"); got == "evil-client" {
		t.Fatalf("forged client Originator %q leaked outbound, want managed value pinned", got)
	}
	if got := httpReq.Header.Get("Originator"); got != helps.DefaultCodexManagedOriginator() {
		t.Fatalf("Originator = %q, want managed default %q", got, helps.DefaultCodexManagedOriginator())
	}
}

// TestApplyCodexHeaders_ManagedOriginatorAcceptsWhitelistedClientValue 覆盖 A-1 白名单：
// managed 账号下，客户端传入的合法 first-party Originator 允许保留。
func TestApplyCodexHeaders_ManagedOriginatorAcceptsWhitelistedClientValue(t *testing.T) {
	auth := &cliproxyauth.Auth{ProxyURL: "direct", ID: "managed-codex-auth-wl", Provider: "codex"}
	httpReq := newCodexHeadersTestRequest(t, map[string]string{"Originator": "codex_cli_rs"})

	applyCodexHeaders(httpReq, auth, "oauth-token", true, nil)

	if got := httpReq.Header.Get("Originator"); got != "codex_cli_rs" {
		t.Fatalf("whitelisted client Originator = %q, want codex_cli_rs preserved", got)
	}
}

// TestApplyCodexHeaders_ApiKeyOriginatorStillPassthrough 覆盖 A-1 边界：api-key 账号
// 不在本次加固范围内，仍按旧行为透传客户端 Originator。
func TestApplyCodexHeaders_ApiKeyOriginatorStillPassthrough(t *testing.T) {
	auth := &cliproxyauth.Auth{ProxyURL: "direct", ID: "apikey-codex-auth", Provider: "codex",
		Attributes: map[string]string{"api_key": "k"},
	}
	httpReq := newCodexHeadersTestRequest(t, map[string]string{"Originator": "evil-client"})

	applyCodexHeaders(httpReq, auth, "oauth-token", true, nil)

	if got := httpReq.Header.Get("Originator"); got != "evil-client" {
		t.Fatalf("api-key Originator = %q, want passthrough evil-client (unchanged behavior)", got)
	}
}

// TestApplyCodexHeaders_CLIDefaultProfileOutbound 覆盖 Wave10-D ①：默认 managed codex
// 账号（无残留 bundle）出站应是 codex_cli_rs CLI 画像（三段式 UA、floor 0.140.0），
// 且不带 Desktop 专属 sec-ch-ua / sec-fetch-* 系列。
func TestApplyCodexHeaders_CLIDefaultProfileOutbound(t *testing.T) {
	helps.ResetCodexClientProfileCacheForTests()
	auth := &cliproxyauth.Auth{ProxyURL: "direct", ID: "codex-cli-default", Provider: "codex"}
	httpReq := newCodexHeadersTestRequest(t, nil)

	applyCodexHeaders(httpReq, auth, "oauth-token", true, &config.Config{})

	if got := httpReq.Header.Get("Originator"); got != "codex_cli_rs" {
		t.Fatalf("Originator = %q, want codex_cli_rs", got)
	}
	ua := httpReq.Header.Get("User-Agent")
	if !strings.HasPrefix(ua, "codex_cli_rs/0.140.0 (Mac OS 15.7.4; arm64) iTerm.app/3.6.8 (codex_cli_rs; 0.140.0)") {
		t.Fatalf("User-Agent = %q, want codex_cli_rs CLI three-segment UA", ua)
	}
	for _, name := range []string{"sec-ch-ua", "sec-ch-ua-mobile", "sec-ch-ua-platform", "sec-fetch-site", "sec-fetch-mode", "sec-fetch-dest"} {
		if got := httpReq.Header.Get(name); got != "" {
			t.Fatalf("%s = %q, want empty for CLI profile", name, got)
		}
	}
}

// TestApplyCodexHeaders_PersistedDesktopBundleDoesNotPolluteCLIOutbound 是本 PR 最关键的
// 正确性测试（Wave10-D 要点2/4）。模拟测试端 codex 账号：metadata.headers + 顶层
// header:* 属性仍是历史 "Codex Desktop" bundle（Desktop UA/Originator + sec-ch-ua 系列），
// 并带结构化 account_settings。断言出站被 CLI 画像压过：
//   - Originator = codex_cli_rs（不是 Codex Desktop）
//   - User-Agent = codex_cli_rs/...（不含 Codex Desktop）
//   - 完全不带 sec-ch-ua / sec-ch-ua-platform 等 Desktop 专属指纹头
func TestApplyCodexHeaders_PersistedDesktopBundleDoesNotPolluteCLIOutbound(t *testing.T) {
	helps.ResetCodexClientProfileCacheForTests()
	desktopBundle := map[string]any{
		"User-Agent":         "Codex Desktop/26.318.11754 (darwin; arm64)",
		"Version":            "26.318.11754",
		"Originator":         "Codex Desktop",
		"sec-ch-ua":          `"Chromium";v="144", "Not:A-Brand";v="24"`,
		"sec-ch-ua-mobile":   "?0",
		"sec-ch-ua-platform": `"macOS"`,
		"sec-fetch-site":     "same-origin",
		"sec-fetch-mode":     "cors",
		"sec-fetch-dest":     "empty",
	}
	auth := &cliproxyauth.Auth{
		ProxyURL: "direct",
		ID:       "codex-desktop-residue",
		Provider: "codex",
		Metadata: map[string]any{
			"auth_method": "oauth",
			"headers":     desktopBundle,
			"account_settings": map[string]any{
				"schema_version": 1,
			},
		},
		// 模拟真实 loader：metadata.headers 被投影成 header:* 属性，
		// ApplyCustomHeadersFromAttrs 会把它们 set 回出站请求。
		Attributes: map[string]string{
			"header:User-Agent":         "Codex Desktop/26.318.11754 (darwin; arm64)",
			"header:Version":            "26.318.11754",
			"header:Originator":         "Codex Desktop",
			"header:sec-ch-ua":          `"Chromium";v="144", "Not:A-Brand";v="24"`,
			"header:sec-ch-ua-mobile":   "?0",
			"header:sec-ch-ua-platform": `"macOS"`,
			"header:sec-fetch-site":     "same-origin",
			"header:sec-fetch-mode":     "cors",
			"header:sec-fetch-dest":     "empty",
		},
	}

	httpReq := newCodexHeadersTestRequest(t, nil)
	applyCodexHeaders(httpReq, auth, "oauth-token", true, &config.Config{})

	if got := httpReq.Header.Get("Originator"); got != "codex_cli_rs" {
		t.Fatalf("Originator = %q, persisted Desktop bundle leaked, want codex_cli_rs", got)
	}
	ua := httpReq.Header.Get("User-Agent")
	if strings.Contains(ua, "Codex Desktop") {
		t.Fatalf("User-Agent = %q leaked Codex Desktop, want codex_cli_rs CLI UA", ua)
	}
	if !strings.HasPrefix(ua, "codex_cli_rs/") {
		t.Fatalf("User-Agent = %q, want codex_cli_rs CLI UA", ua)
	}
	for _, name := range []string{"sec-ch-ua", "sec-ch-ua-mobile", "sec-ch-ua-platform", "sec-fetch-site", "sec-fetch-mode", "sec-fetch-dest"} {
		if got := httpReq.Header.Get(name); got != "" {
			t.Fatalf("%s = %q leaked from Desktop bundle, want stripped for CLI profile", name, got)
		}
	}
}

// TestApplyCodexHeaders_OSArchTerminalPinnedAcrossObservedClients 覆盖 Wave10-D ⑤：
// 不同客户端上报不同 OS/terminal 时，出站 UA 的 OS/arch/terminal 稳定 pin 到 baseline。
func TestApplyCodexHeaders_OSArchTerminalPinnedAcrossObservedClients(t *testing.T) {
	helps.ResetCodexClientProfileCacheForTests()
	auth := &cliproxyauth.Auth{ProxyURL: "direct", ID: "codex-pin", Provider: "codex"}
	httpReq := newCodexHeadersTestRequest(t, map[string]string{
		"User-Agent": "codex_cli_rs/0.145.0 (Mac OS 14.0.0; arm64) Ghostty/9.9.9 (codex_cli_rs; 0.145.0)",
		"Version":    "0.145.0",
		"Originator": "codex_cli_rs",
	})

	applyCodexHeaders(httpReq, auth, "oauth-token", true, &config.Config{})

	ua := httpReq.Header.Get("User-Agent")
	if !strings.Contains(ua, "Mac OS 15.7.4; arm64") {
		t.Fatalf("User-Agent = %q, want pinned Mac OS 15.7.4; arm64", ua)
	}
	if strings.Contains(ua, "Ghostty") || strings.Contains(ua, "Mac OS 14.0.0") {
		t.Fatalf("User-Agent = %q leaked observed OS/terminal, want pinned", ua)
	}
	if !strings.Contains(ua, "iTerm.app/3.6.8 (codex_cli_rs; 0.145.0)") {
		t.Fatalf("User-Agent = %q, want pinned terminal with bumped version 0.145.0", ua)
	}
}

// TestApplyCodexIdentityConfuseBody_DerivesInstallationIDWhenMissing 覆盖 A-3：
// 客户端 body 没带 x-codex-installation-id 时，必须用每账号稳定派生兜底注入，
// 且同账号跨请求稳定、跨账号不同。
func TestApplyCodexIdentityConfuseBody_DerivesInstallationIDWhenMissing(t *testing.T) {
	cfg := &config.Config{
		Routing: config.RoutingConfig{Strategy: "fill-first"},
		Codex:   config.CodexConfig{IdentityConfuse: true},
	}
	authA := &cliproxyauth.Auth{ProxyURL: "direct", ID: "auth-A", Provider: "codex"}
	authB := &cliproxyauth.Auth{ProxyURL: "direct", ID: "auth-B", Provider: "codex"}
	body := []byte(`{"model":"gpt-5-codex","client_metadata":{}}`)

	outA, _ := applyCodexIdentityConfuseBody(cfg, authA, body, body)
	gotA := gjson.GetBytes(outA, "client_metadata.x-codex-installation-id").String()
	wantA := codexIdentityConfuseUUID("auth-A", "installation", "default")
	if gotA != wantA {
		t.Fatalf("derived installation id = %q, want %q", gotA, wantA)
	}

	// 同账号再来一次，结果稳定。
	outA2, _ := applyCodexIdentityConfuseBody(cfg, authA, body, body)
	if got := gjson.GetBytes(outA2, "client_metadata.x-codex-installation-id").String(); got != wantA {
		t.Fatalf("installation id not stable across requests: %q vs %q", got, wantA)
	}

	// 跨账号不同。
	outB, _ := applyCodexIdentityConfuseBody(cfg, authB, body, body)
	if got := gjson.GetBytes(outB, "client_metadata.x-codex-installation-id").String(); got == wantA {
		t.Fatalf("installation id collides across accounts: %q", got)
	}
}

func TestCodexIdentityConfuseKeepsClientBodySeparateFromUpstreamBody(t *testing.T) {
	cfg := &config.Config{
		Routing: config.RoutingConfig{Strategy: "fill-first"},
		Codex:   config.CodexConfig{IdentityConfuse: true},
	}
	auth := &cliproxyauth.Auth{ProxyURL: "direct", ID: "auth-1", Provider: "codex"}
	clientBody := []byte(`{"model":"gpt-5-codex","prompt_cache_key":"cache-1"}`)

	upstreamBody, identityState := applyCodexIdentityConfuseBody(cfg, auth, clientBody, clientBody)
	expectedPromptCacheKey := codexIdentityConfuseUUID("auth-1", "prompt-cache", "cache-1")
	if identityState.promptCacheKey != expectedPromptCacheKey {
		t.Fatalf("identity prompt_cache_key = %q, want %q", identityState.promptCacheKey, expectedPromptCacheKey)
	}
	if gotKey := gjson.GetBytes(upstreamBody, "prompt_cache_key").String(); gotKey != expectedPromptCacheKey {
		t.Fatalf("upstream prompt_cache_key = %q, want %q", gotKey, expectedPromptCacheKey)
	}
	if gotKey := gjson.GetBytes(clientBody, "prompt_cache_key").String(); gotKey != "cache-1" {
		t.Fatalf("client prompt_cache_key = %q, want cache-1", gotKey)
	}
}
