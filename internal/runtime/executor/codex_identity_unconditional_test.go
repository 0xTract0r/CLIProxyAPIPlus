package executor

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

// 这组测试覆盖 anticorr 方案A：把 codex turn-metadata 里的 4 个真机身份字段
// （installation_id / turn_id / session_id / thread_id）的归一从 identity-confuse
// 门控里提到无条件（与开关解耦），prompt_cache_key / window_id 仍留在门控内。
//
// 生产现状 identity-confuse 默认关闭，旧逻辑下这 4 个字段裸泄漏真机指纹；本组测试
// 钉死：开关关闭时这 4 个字段在 body + header turn-metadata 全被合成、真机原值不
// 残留；prompt_cache_key 不被身份归一动；turn_id 的 response 回换正常；WS / HTTP
// 对称；开关开启时不二次派生这 4 个字段、且 prompt_cache_key / window_id 仍按开关走。

const (
	realInstallID    = "6a9aea66-9c05-4a26-8c27-038f82fabaed"
	realTurnID       = "turn-real-1"
	realSessionID    = "sess-real-1"
	realThreadID     = "thread-real-1"
	realPromptCache  = "cache-real-1"
	unconditionalAID = "auth-uncond-1"
)

// turnMetadataWithIdentity 构造一段带全部 4 身份字段 + prompt_cache_key + window_id
// 的 turn-metadata JSON（供 header 用）以及其 body client_metadata 转义副本。
func turnMetadataWithIdentity() (headerJSON string, bodyEscaped string) {
	headerJSON = `{"installation_id":"` + realInstallID + `","turn_id":"` + realTurnID +
		`","session_id":"` + realSessionID + `","thread_id":"` + realThreadID +
		`","prompt_cache_key":"` + realPromptCache + `","window_id":"` + realPromptCache + `:0"}`
	// body 副本是 header JSON 的逐字转义字符串。
	bodyEscaped = strings.NewReplacer(`"`, `\"`).Replace(headerJSON)
	return headerJSON, bodyEscaped
}

// newUnconditionalCacheCall 跑一次 cacheHelper + 头归一，返回 body 与 httpReq.Header
// 上的 turn-metadata，供断言。disabled=true 时 identity-confuse 门控关闭。
func newUnconditionalCacheCall(t *testing.T, identityConfuseOn bool) (bodyTM string, headerTM string, bodyInstallID string, identityState codexIdentityConfuseState) {
	t.Helper()
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Request = httptest.NewRequest("POST", "/v1/responses", nil)
	headerJSON, bodyEscaped := turnMetadataWithIdentity()
	ginCtx.Request.Header.Set("X-Codex-Turn-Metadata", headerJSON)
	ctx := context.WithValue(context.Background(), "gin", ginCtx)

	cfg := &config.Config{}
	if identityConfuseOn {
		cfg.Routing = config.RoutingConfig{Strategy: "fill-first"}
		cfg.Codex = config.CodexConfig{IdentityConfuse: true}
	}
	executor := &CodexExecutor{cfg: cfg}
	auth := &cliproxyauth.Auth{ProxyURL: "direct", ID: unconditionalAID, Provider: "codex"}

	rawJSON := []byte(`{"model":"gpt-5-codex","stream":true,"client_metadata":{"x-codex-installation-id":"` + realInstallID +
		`","x-codex-turn-metadata":"` + bodyEscaped + `","x-codex-window-id":"` + realPromptCache + `:0"}}`)
	req := cliproxyexecutor.Request{
		Model:   "gpt-5-codex",
		Payload: []byte(`{"model":"gpt-5-codex","prompt_cache_key":"` + realPromptCache + `","client_metadata":{"x-codex-installation-id":"` + realInstallID + `"}}`),
	}

	httpReq, body, state, err := executor.cacheHelper(ctx, sdktranslator.FromString("openai-response"), "https://example.com/responses", auth, req, req.Payload, rawJSON)
	if err != nil {
		t.Fatalf("cacheHelper error: %v", err)
	}
	applyCodexHeaders(httpReq, auth, "oauth-token", true, executor.cfg)
	applyCodexIdentityConfuseHeaders(httpReq.Header, &state)

	bodyTM = gjson.GetBytes(body, "client_metadata.x-codex-turn-metadata").String()
	headerTM = httpReq.Header.Get("X-Codex-Turn-Metadata")
	bodyInstallID = gjson.GetBytes(body, "client_metadata.x-codex-installation-id").String()
	return bodyTM, headerTM, bodyInstallID, state
}

// assertIdentityFieldsSynthesized 断言 4 个身份字段在一段 turn-metadata JSON 里都被
// 合成成派生值、真机原值不残留。
func assertIdentityFieldsSynthesized(t *testing.T, where string, tm string) {
	t.Helper()
	expectInstall := codexIdentityConfuseUUID(unconditionalAID, "installation", realInstallID)
	expectTurn := codexIdentityConfuseUUID(unconditionalAID, "turn", realTurnID)
	expectSession := codexIdentityConfuseUUID(unconditionalAID, "session", realSessionID)
	expectThread := codexIdentityConfuseUUID(unconditionalAID, "thread", realThreadID)

	if got := gjson.Get(tm, "installation_id").String(); got != expectInstall {
		t.Fatalf("%s installation_id = %q, want %q", where, got, expectInstall)
	}
	if got := gjson.Get(tm, "turn_id").String(); got != expectTurn {
		t.Fatalf("%s turn_id = %q, want %q", where, got, expectTurn)
	}
	if got := gjson.Get(tm, "session_id").String(); got != expectSession {
		t.Fatalf("%s session_id = %q, want %q", where, got, expectSession)
	}
	if got := gjson.Get(tm, "thread_id").String(); got != expectThread {
		t.Fatalf("%s thread_id = %q, want %q", where, got, expectThread)
	}
	for _, real := range []string{realInstallID, realTurnID, realSessionID, realThreadID} {
		if strings.Contains(tm, real) {
			t.Fatalf("%s 仍残留真机身份值 %q: %s", where, real, tm)
		}
	}
}

// TestCodexIdentity_Unconditional_WhenConfuseDisabled 核心用例：identity-confuse 关闭
// 时，4 个真机身份字段在 body + header turn-metadata 全被合成，真机原值不残留；
// prompt_cache_key 不被身份归一动（仍是源值）；window_id 不被动（门控关，保持源值）。
func TestCodexIdentity_Unconditional_WhenConfuseDisabled(t *testing.T) {
	bodyTM, headerTM, bodyInstallID, _ := newUnconditionalCacheCall(t, false)

	assertIdentityFieldsSynthesized(t, "body turn-metadata", bodyTM)
	assertIdentityFieldsSynthesized(t, "header turn-metadata", headerTM)

	// client_metadata.x-codex-installation-id 顶层字段同样无条件合成。
	expectInstall := codexIdentityConfuseUUID(unconditionalAID, "installation", realInstallID)
	if bodyInstallID != expectInstall {
		t.Fatalf("body client_metadata.x-codex-installation-id = %q, want %q", bodyInstallID, expectInstall)
	}

	// prompt_cache_key 不受身份归一影响：门控关，turn-metadata 里仍是真机源值。
	if got := gjson.Get(bodyTM, "prompt_cache_key").String(); got != realPromptCache {
		t.Fatalf("门控关时 body turn-metadata.prompt_cache_key = %q, want 源值 %q（不应被身份归一动）", got, realPromptCache)
	}
	if got := gjson.Get(headerTM, "prompt_cache_key").String(); got != realPromptCache {
		t.Fatalf("门控关时 header turn-metadata.prompt_cache_key = %q, want 源值 %q", got, realPromptCache)
	}
	// window_id 同样受门控，关时保持源值。
	if got := gjson.Get(bodyTM, "window_id").String(); got != realPromptCache+":0" {
		t.Fatalf("门控关时 body turn-metadata.window_id = %q, want 源值 %q", got, realPromptCache+":0")
	}
}

// TestCodexIdentity_Unconditional_TurnIDResponseSwapBack 验证门控关闭时 turn_id 的
// response 回换仍正常：上游 SSE 里的 confused turn_id 在回客户端时换回真值。
func TestCodexIdentity_Unconditional_TurnIDResponseSwapBack(t *testing.T) {
	_, _, _, state := newUnconditionalCacheCall(t, false)

	expectTurn := codexIdentityConfuseUUID(unconditionalAID, "turn", realTurnID)
	// turnIDs 应已登记真机→confused 映射。
	if len(state.turnIDs) != 1 || state.turnIDs[0].original != realTurnID || state.turnIDs[0].confused != expectTurn {
		t.Fatalf("turnIDs = %#v, want [{original:%q confused:%q}]", state.turnIDs, realTurnID, expectTurn)
	}

	// 模拟上游回显 confused turn_id；exposeResponse 应换回真机 turn_id 给客户端。
	upstream := []byte(`{"type":"response.completed","response":{"turn_id":"` + expectTurn + `"}}`)
	client := applyCodexIdentityExposeResponsePayload(upstream, state)
	if strings.Contains(string(client), expectTurn) {
		t.Fatalf("client payload 仍含 confused turn_id: %s", client)
	}
	if !strings.Contains(string(client), realTurnID) {
		t.Fatalf("client payload 缺真机 turn_id: %s", client)
	}
}

// TestCodexIdentity_Unconditional_GateOnNoDoubleDerive 验证 identity-confuse 开启时
// 不二次派生这 4 个身份字段（仍是单次派生值），且 prompt_cache_key / window_id 按
// 开关走（被混淆）。
func TestCodexIdentity_Unconditional_GateOnNoDoubleDerive(t *testing.T) {
	bodyTM, headerTM, _, state := newUnconditionalCacheCall(t, true)

	// 4 身份字段仍是单次派生值（无二次派生）。
	assertIdentityFieldsSynthesized(t, "body turn-metadata(gate-on)", bodyTM)
	assertIdentityFieldsSynthesized(t, "header turn-metadata(gate-on)", headerTM)

	// prompt_cache_key 按开关被混淆：门控开时应是 confused 值，不再是源值。
	expectPromptCache := codexIdentityConfuseUUID(unconditionalAID, "prompt-cache", realPromptCache)
	if state.promptCacheKey != expectPromptCache {
		t.Fatalf("门控开时 state.promptCacheKey = %q, want %q", state.promptCacheKey, expectPromptCache)
	}
	if got := gjson.Get(bodyTM, "prompt_cache_key").String(); got != expectPromptCache {
		t.Fatalf("门控开时 body turn-metadata.prompt_cache_key = %q, want %q", got, expectPromptCache)
	}
	// window_id 按开关被混淆。
	if got := gjson.Get(bodyTM, "window_id").String(); got != expectPromptCache+":0" {
		t.Fatalf("门控开时 body turn-metadata.window_id = %q, want %q", got, expectPromptCache+":0")
	}
	// 真机 prompt_cache_key 不残留。
	if strings.Contains(bodyTM, realPromptCache) {
		t.Fatalf("门控开时 body turn-metadata 仍残留真机 prompt_cache_key: %s", bodyTM)
	}
}

// TestCodexIdentity_Unconditional_BodyHelperDirect 直接驱动 applyCodexIdentityConfuseBody
// （HTTP 与 WS 共用的 body 归一 chokepoint），验证门控关时 identityNormalize 生效、
// enabled 不生效，4 身份字段被合成。
func TestCodexIdentity_Unconditional_BodyHelperDirect(t *testing.T) {
	headerJSON, bodyEscaped := turnMetadataWithIdentity()
	_ = headerJSON
	auth := &cliproxyauth.Auth{ProxyURL: "direct", ID: unconditionalAID, Provider: "codex"}
	rawJSON := []byte(`{"model":"gpt-5-codex","client_metadata":{"x-codex-installation-id":"` + realInstallID +
		`","x-codex-turn-metadata":"` + bodyEscaped + `"}}`)
	userPayload := []byte(`{"prompt_cache_key":"` + realPromptCache + `","client_metadata":{"x-codex-installation-id":"` + realInstallID + `"}}`)

	// cfg 不开 identity-confuse。
	out, state := applyCodexIdentityConfuseBody(&config.Config{}, auth, userPayload, rawJSON)
	if !state.identityNormalize {
		t.Fatalf("identityNormalize = false, want true（无条件归一应始终生效）")
	}
	if state.enabled {
		t.Fatalf("enabled = true, want false（门控关时不应启用 prompt_cache_key 混淆）")
	}
	if state.promptCacheKey != "" {
		t.Fatalf("门控关时 state.promptCacheKey = %q, want 空（不混淆 prompt_cache_key）", state.promptCacheKey)
	}
	bodyTM := gjson.GetBytes(out, "client_metadata.x-codex-turn-metadata").String()
	assertIdentityFieldsSynthesized(t, "body helper direct", bodyTM)
}

// TestCodexIdentity_Unconditional_MissingFieldsNotInjected 验证缺省语义：turn_id /
// session_id / thread_id 缺失时不注入（贴真实 codex"没有就不发"形态）；installation_id
// 缺失时按 "default" 兜底派生注入（贴 A-3）。
func TestCodexIdentity_Unconditional_MissingFieldsNotInjected(t *testing.T) {
	auth := &cliproxyauth.Auth{ProxyURL: "direct", ID: unconditionalAID, Provider: "codex"}
	// turn-metadata 只带 installation_id，缺 turn_id / session_id / thread_id。
	bodyEscaped := strings.NewReplacer(`"`, `\"`).Replace(`{"installation_id":"` + realInstallID + `"}`)
	rawJSON := []byte(`{"model":"gpt-5-codex","client_metadata":{"x-codex-turn-metadata":"` + bodyEscaped + `"}}`)
	// userPayload 不带 x-codex-installation-id → 顶层 installation_id 走 "default" 兜底。
	userPayload := []byte(`{"model":"gpt-5-codex"}`)

	out, _ := applyCodexIdentityConfuseBody(&config.Config{}, auth, userPayload, rawJSON)
	bodyTM := gjson.GetBytes(out, "client_metadata.x-codex-turn-metadata").String()

	// turn_id / session_id / thread_id 缺失，不应被注入。
	for _, field := range []string{"turn_id", "session_id", "thread_id"} {
		if gjson.Get(bodyTM, field).Exists() {
			t.Fatalf("缺省字段 %s 不应被注入: %s", field, bodyTM)
		}
	}
	// installation_id 用 state.installationID 改写——这里 userPayload 缺
	// x-codex-installation-id，state.installationID 走 "default" 兜底派生（与顶层
	// client_metadata.x-codex-installation-id 同一个值，保证 turn-metadata 与
	// client_metadata 两处一致、不残留真机值）。
	expectDefault := codexIdentityConfuseUUID(unconditionalAID, "installation", "default")
	if got := gjson.Get(bodyTM, "installation_id").String(); got != expectDefault {
		t.Fatalf("turn-metadata.installation_id = %q, want default 兜底 %q", got, expectDefault)
	}
	if strings.Contains(bodyTM, realInstallID) {
		t.Fatalf("turn-metadata 仍残留真机 installation_id: %s", bodyTM)
	}
	if got := gjson.GetBytes(out, "client_metadata.x-codex-installation-id").String(); got != expectDefault {
		t.Fatalf("client_metadata.x-codex-installation-id = %q, want default 兜底 %q", got, expectDefault)
	}
}
