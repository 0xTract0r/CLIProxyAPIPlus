package executor

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

// cchBillingPattern 抽取 billing-header 里的 5 位 cch 占位。
var cchBillingPattern = regexp.MustCompile(`(x-anthropic-billing-header:[^"]*?\bcch=)([0-9a-f]{5})(;)`)

func cchFromBody(t *testing.T, body []byte) string {
	t.Helper()
	match := cchBillingPattern.FindSubmatch(body)
	if match == nil {
		t.Fatalf("expected billing header with cch in body: %s", string(body))
	}
	return string(match[2])
}

// 反关联修复 A（C1）：count_tokens 在 OAuth/签名模式下必须用与 messages 完全一致的
// xxHash64 签名算法回填 cch（先把 cch 归一为 00000 再对整 body 做 xxHash64），
// 而不是此前硬钉的 sha256[:5]。本测试驱动真实 CountTokens，截获上行 body，断言其
// cch 等于对自身 body（cch 归一为 00000）做 xxHash64 的结果——即与 messages 路径
// signAnthropicMessagesBody 同算法、同种子、同 body→同 cch。
func TestClaudeExecutorCountTokens_OAuthSignsCCHWithXXHash64(t *testing.T) {
	resetClaudeDeviceProfileCache()

	var seenBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		seenBody = bytes.Clone(body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"input_tokens":1}`))
	}))
	defer server.Close()

	executor := NewClaudeExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{ProxyURL: "direct",
		ID: "auth-count-tokens-oauth",
		Attributes: map[string]string{
			"api_key":  "sk-ant-oat-test-123",
			"base_url": server.URL,
		},
	}
	payload := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"hello world"}]}]}`)

	if _, err := executor.CountTokens(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "claude-3-5-sonnet-20241022",
		Payload: payload,
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("claude")}); err != nil {
		t.Fatalf("CountTokens() error = %v", err)
	}
	if len(seenBody) == 0 {
		t.Fatal("expected count_tokens upstream body to be captured")
	}

	billingHeader := gjson.GetBytes(seenBody, "system.0.text").String()
	if !strings.HasPrefix(billingHeader, "x-anthropic-billing-header:") {
		t.Fatalf("system.0.text = %q, want billing header", billingHeader)
	}
	// 占位 cch 不得以未签名形式（00000）外发。
	if strings.Contains(billingHeader, "cch=00000;") {
		t.Fatalf("count_tokens must not forward unsigned cch placeholder, got %q", billingHeader)
	}

	actualCCH := cchFromBody(t, seenBody)
	// 用 messages 路径完全相同的签名器复算：证明 count_tokens 走的就是 xxHash64 算法。
	// signAnthropicMessagesBody 对已签名 body 是幂等的（先归一 cch=00000 再 xxHash64），
	// 因此若 count_tokens 的 cch 已经是 xxHash64 签名值，复算结果必然相等。
	wantBody := signAnthropicMessagesBody(seenBody)
	wantCCH := cchFromBody(t, wantBody)
	if actualCCH != wantCCH {
		t.Fatalf("count_tokens cch = %q, want xxHash64-signed %q (algorithm mismatch with messages)\nbody: %s",
			actualCCH, wantCCH, string(seenBody))
	}
}

// 反关联修复 A（C1）一致性：同一逻辑请求、同一 OAuth 账号，messages 与 count_tokens
// 对“同一份 body”必须得到同一个 cch。直接把 count_tokens 截获的 body 喂给 messages
// 的签名器，断言两端点对同一 body 计算出的 cch 完全一致（同算法 + 同种子）。
func TestClaudeExecutorCountTokens_SameBodyYieldsSameCCHAsMessages(t *testing.T) {
	resetClaudeDeviceProfileCache()

	var seenBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		seenBody = bytes.Clone(body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"input_tokens":1}`))
	}))
	defer server.Close()

	executor := NewClaudeExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{ProxyURL: "direct",
		ID: "auth-count-tokens-parity",
		Attributes: map[string]string{
			"api_key":  "sk-ant-oat-parity",
			"base_url": server.URL,
		},
	}
	payload := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"parity check"}]}]}`)

	if _, err := executor.CountTokens(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "claude-3-5-sonnet-20241022",
		Payload: payload,
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("claude")}); err != nil {
		t.Fatalf("CountTokens() error = %v", err)
	}
	if len(seenBody) == 0 {
		t.Fatal("expected count_tokens upstream body to be captured")
	}

	countTokensCCH := cchFromBody(t, seenBody)

	// messages 路径对同一 body 的签名结果。
	messagesSigned := signAnthropicMessagesBody(seenBody)
	messagesCCH := cchFromBody(t, messagesSigned)

	if countTokensCCH != messagesCCH {
		t.Fatalf("same body produced different cch across endpoints: count_tokens=%q messages=%q",
			countTokensCCH, messagesCCH)
	}

	// 二次回填应是幂等的（已签名 body 再签一次 cch 不变），佐证 count_tokens 已是
	// 与 messages 等价的最终签名态。
	if reSigned := cchFromBody(t, signAnthropicMessagesBody(messagesSigned)); reSigned != messagesCCH {
		t.Fatalf("signing is not idempotent: %q -> %q", messagesCCH, reSigned)
	}
}

// 反关联修复 A（C1）非 OAuth 路径不变式：纯 API key（非 sk-ant-oat）时 count_tokens
// 不应进入 xxHash64 签名，cch 维持 sha256 旧行为——这与 messages 在非 OAuth 下同样
// 走 sha256 一致，因此两端点仍然同算法。断言：截获 cch 不等于该 body 的 xxHash64
// 签名值（即签名器若运行会改变 cch），证明签名未对非 OAuth 生效。
func TestClaudeExecutorCountTokens_NonOAuthKeepsSha256(t *testing.T) {
	resetClaudeDeviceProfileCache()

	var seenBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		seenBody = bytes.Clone(body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"input_tokens":1}`))
	}))
	defer server.Close()

	executor := NewClaudeExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{ProxyURL: "direct",
		ID: "auth-count-tokens-apikey",
		Attributes: map[string]string{
			"api_key":  "sk-plain-api-key-not-oauth",
			"base_url": server.URL,
		},
	}
	payload := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"non oauth"}]}]}`)

	if _, err := executor.CountTokens(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "claude-3-5-sonnet-20241022",
		Payload: payload,
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("claude")}); err != nil {
		t.Fatalf("CountTokens() error = %v", err)
	}
	if len(seenBody) == 0 {
		t.Fatal("expected count_tokens upstream body to be captured")
	}

	actualCCH := cchFromBody(t, seenBody)
	// 若签名器对该 body 运行会得到不同的 cch，则证明实际外发的不是 xxHash64 签名值，
	// 即 count_tokens 在纯 API key 下没有进入签名路径，cch 维持 sha256 旧行为。
	signedCCH := cchFromBody(t, signAnthropicMessagesBody(seenBody))
	if actualCCH == signedCCH {
		t.Fatalf("non-OAuth count_tokens cch = %q equals xxHash64-signed value; signing must not run for plain API key", actualCCH)
	}
}
