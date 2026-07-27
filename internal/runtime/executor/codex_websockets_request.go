package executor

import (
	"context"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/misc"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/thinking"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

func applyCodexPromptCacheHeaders(from sdktranslator.Format, req cliproxyexecutor.Request, rawJSON []byte) ([]byte, http.Header) {
	body, headers, _ := applyCodexPromptCacheHeadersWithContext(context.Background(), from, req, rawJSON)
	return body, headers
}

func applyCodexPromptCacheHeadersWithContext(ctx context.Context, from sdktranslator.Format, req cliproxyexecutor.Request, rawJSON []byte, headerSets ...http.Header) ([]byte, http.Header, error) {
	headers := http.Header{}
	if len(rawJSON) == 0 {
		return rawJSON, headers, nil
	}

	var requestHeaders http.Header
	if len(headerSets) > 0 {
		requestHeaders = headerSets[0]
	}
	var cache helps.CodexCache
	if sourceFormatEqual(from, sdktranslator.FormatClaude) {
		modelName := strings.TrimSpace(gjson.GetBytes(rawJSON, "model").String())
		if modelName == "" {
			modelName = thinking.ParseSuffix(req.Model).ModelName
		}
		cached, ok, errCache := helps.ClaudeCodePromptCache(ctx, modelName, req.Payload, requestHeaders)
		if errCache != nil {
			return nil, nil, errCache
		}
		if ok {
			cache = cached
		}
	} else if sourceFormatEqual(from, sdktranslator.FormatOpenAIResponse) {
		if promptCacheKey := gjson.GetBytes(req.Payload, "prompt_cache_key"); promptCacheKey.Exists() {
			cache.ID = promptCacheKey.String()
		}
	}
	if cache.ID == "" {
		cache.ID = helps.ProviderSessionUUID("codex", req.Metadata)
	}

	if cache.ID != "" {
		rawJSON = helps.SetStringIfDifferent(rawJSON, "prompt_cache_key", cache.ID)
		// fork(anticorr): 出站 session 头名对齐真实 codex 的 session-id（小写连字符），
		// 而非旧的 session_id（下划线）。
		setCodexLowerSessionHeader(headers, cache.ID)
		headers.Set("Conversation_id", cache.ID)
	}

	return rawJSON, headers, nil
}

func applyCodexWebsocketHeaders(ctx context.Context, headers http.Header, auth *cliproxyauth.Auth, token string, cfg *config.Config) http.Header {
	if headers == nil {
		headers = http.Header{}
	}
	if strings.TrimSpace(token) != "" {
		headers.Set("Authorization", "Bearer "+token)
	}

	var ginHeaders http.Header
	if ginCtx, ok := ctx.Value("gin").(*gin.Context); ok && ginCtx != nil && ginCtx.Request != nil {
		ginHeaders = ginCtx.Request.Header.Clone()
	}

	var profile helps.CodexClientProfile
	if auth != nil {
		profile = helps.ResolveCodexClientProfile(auth, ginHeaders, cfg)
	}
	_, cfgBetaFeatures := codexHeaderDefaults(cfg, auth)
	ensureHeaderWithPriority(headers, ginHeaders, "x-codex-beta-features", cfgBetaFeatures, "")
	misc.EnsureHeader(headers, ginHeaders, "x-codex-turn-state", "")
	misc.EnsureHeader(headers, ginHeaders, "x-codex-turn-metadata", "")
	// fork(anticorr ⑦-codex): turn-metadata header cwd/git normalization is now
	// DORMANT and gated by the same account-env switch as the body path (mirrors
	// applyCodexHeaders). Previously unconditional; with body-side cwd
	// normalization off the header must pass the real cwd/git through unchanged
	// (透传) to avoid a fake-header / real-body contradiction. The switch is forced
	// off in LoadConfig, so this branch never runs in production. Identity fields
	// remain handled by applyCodexTurnMetadataIdentityConfuse.
	if config.NormalizeAccountEnvEnabled(cfg) {
		// ws header keys are lower-case; the helper is case-insensitive. WithRestore
		// captures the header's real cwd into the response-restore collector when one
		// is attached to ctx.
		helps.NormalizeCodexTurnMetadataHeaderWithRestore(ctx, headers, "x-codex-turn-metadata", auth, token)
	}
	misc.EnsureHeader(headers, ginHeaders, "x-client-request-id", "")
	misc.EnsureHeader(headers, ginHeaders, "x-responsesapi-include-timing-metrics", "")
	// fork(anticorr): 真实 codex 出站没有独立 Version 头，版本只体现在 UA。删除任何
	// 客户端透传或 device-profile 注入的 Version（CPA 独有指纹）。
	deleteHeaderCaseInsensitive(headers, "Version")
	applyCodexCommunityDesktopHeaders(headers, profile)

	betaHeader := strings.TrimSpace(headers.Get("OpenAI-Beta"))
	if betaHeader == "" && ginHeaders != nil {
		betaHeader = strings.TrimSpace(ginHeaders.Get("OpenAI-Beta"))
	}
	if betaHeader == "" || !strings.Contains(betaHeader, "responses_websockets=") {
		betaHeader = codexResponsesWebsocketBetaHeaderValue
	}
	headers.Set("OpenAI-Beta", betaHeader)
	if strings.Contains(headers.Get("User-Agent"), "Mac OS") {
		// fork(anticorr): 出站 session 头名对齐真实 codex 的 session-id（小写连字符）。
		ensureCodexLowerSessionHeader(headers, ginHeaders, uuid.NewString())
	}

	isAPIKey := codexAuthUsesAPIKey(auth)
	// Managed (OAuth/device-profile) requests run under the fork's anti-risk
	// identity, which deliberately drops any client-provided User-Agent so the
	// managed snapshot stays authoritative. Explicit api-key requests instead
	// preserve the caller's own User-Agent, falling back to empty.
	headers.Del("User-Agent")
	if isAPIKey {
		ensureHeaderWithPriority(headers, ginHeaders, "User-Agent", "", "")
	}

	if originator := strings.TrimSpace(ginHeaders.Get("Originator")); originator != "" {
		headers.Set("Originator", originator)
	} else if auth != nil && !isAPIKey && strings.TrimSpace(profile.Originator) != "" {
		originator := strings.TrimSpace(profile.Originator)
		headers.Set("Originator", originator)
	} else if !isAPIKey {
		headers.Set("Originator", codexOriginator)
	}
	if !isAPIKey {
		if auth != nil && auth.Metadata != nil {
			if accountID, ok := auth.Metadata["account_id"].(string); ok {
				if trimmed := strings.TrimSpace(accountID); trimmed != "" {
					setHeaderCasePreserved(headers, "ChatGPT-Account-ID", trimmed)
				}
			}
		}
	}
	managedHeaderSnapshot := captureManagedHeaderSnapshot(headers, []string{
		"Version",
		"X-Codex-Beta-Features",
		"Originator",
		"OpenAI-Beta",
		"sec-ch-ua",
		"sec-ch-ua-mobile",
		"sec-ch-ua-platform",
		"Accept-Encoding",
		"Accept-Language",
		"sec-fetch-site",
		"sec-fetch-mode",
		"sec-fetch-dest",
	})
	var attrs map[string]string
	if auth != nil {
		attrs = auth.Attributes
	}
	util.ApplyCustomHeadersFromAttrs(&http.Request{Header: headers}, attrs)
	if cliproxyauth.HasStructuredAccountSettingsMetadata(auth) {
		applyManagedHeaderSnapshot(headers, managedHeaderSnapshot)
	}
	// fork(anticorr): 兜底再删一次 Version —— 真实 codex 出站无独立 Version 头。
	// header:Version 自定义属性可能在 snapshot 恢复后又写回，这里最终统一剥离。
	deleteHeaderCaseInsensitive(headers, "Version")

	return headers
}

func ensureCodexWebsocketSessionHeader(target http.Header, source http.Header, fallbackValue string) {
	if target == nil {
		return
	}
	sessionID := codexSessionHeaderValue(target)
	if sessionID == "" {
		sessionID = codexSessionHeaderValue(source)
	}
	if sessionID == "" {
		sessionID = strings.TrimSpace(fallbackValue)
	}
	if sessionID != "" {
		setHeaderCasePreserved(target, "session_id", sessionID)
	}
	deleteHeaderCaseInsensitive(target, "Session-Id")
}

func codexSessionHeaderValue(headers http.Header) string {
	for _, key := range []string{"session-id", "Session-Id", "Session_id", "session_id"} {
		if value := strings.TrimSpace(headerValueCaseInsensitive(headers, key)); value != "" {
			return value
		}
	}
	return ""
}

// setCodexLowerSessionHeader 把出站 session 头钉成真实 codex 的小写连字符
// session-id（删除任何 Session_id / Session-Id 等大小写/下划线变体），并直写 map
// key 防止 Header.Set 规范化成 Session-Id。
func setCodexLowerSessionHeader(headers http.Header, value string) {
	if headers == nil {
		return
	}
	value = strings.TrimSpace(value)
	if value == "" {
		return
	}
	for existingKey := range headers {
		if codexSessionHeaderKey(existingKey) {
			delete(headers, existingKey)
		}
	}
	headers["session-id"] = []string{value}
}

// ensureCodexLowerSessionHeader 在出站缺少任何 session 头变体时，按 source 优先、
// 否则 fallback 注入小写连字符 session-id；已有变体则统一收敛成 session-id 不改值。
func ensureCodexLowerSessionHeader(target http.Header, source http.Header, fallbackValue string) {
	if target == nil {
		return
	}
	if existing := strings.TrimSpace(codexSessionHeaderValue(target)); existing != "" {
		setCodexLowerSessionHeader(target, existing)
		return
	}
	value := ""
	if source != nil {
		value = strings.TrimSpace(codexSessionHeaderValue(source))
	}
	if value == "" {
		value = strings.TrimSpace(fallbackValue)
	}
	if value != "" {
		setCodexLowerSessionHeader(target, value)
	}
}

func codexAuthUsesAPIKey(auth *cliproxyauth.Auth) bool {
	if auth == nil || auth.Attributes == nil {
		return false
	}
	return strings.TrimSpace(auth.Attributes["api_key"]) != ""
}

func ensureHeaderCasePreserved(target http.Header, source http.Header, key, configValue, fallbackValue string) {
	if target == nil {
		return
	}
	if strings.TrimSpace(headerValueCaseInsensitive(target, key)) != "" {
		return
	}
	if source != nil {
		if val := strings.TrimSpace(headerValueCaseInsensitive(source, key)); val != "" {
			setHeaderCasePreserved(target, key, val)
			return
		}
	}
	if val := strings.TrimSpace(configValue); val != "" {
		setHeaderCasePreserved(target, key, val)
		return
	}
	if val := strings.TrimSpace(fallbackValue); val != "" {
		setHeaderCasePreserved(target, key, val)
	}
}

func setHeaderCasePreserved(headers http.Header, key string, value string) {
	if headers == nil {
		return
	}
	key = strings.TrimSpace(key)
	value = strings.TrimSpace(value)
	if key == "" || value == "" {
		return
	}
	deleteHeaderCaseInsensitive(headers, key)
	headers[key] = []string{value}
}

func setCodexSessionHeaderCasePreserved(headers http.Header, fallbackKey string, value string) {
	if headers == nil {
		return
	}
	fallbackKey = strings.TrimSpace(fallbackKey)
	value = strings.TrimSpace(value)
	if fallbackKey == "" || value == "" {
		return
	}

	selectedKey := ""
	if _, ok := headers[fallbackKey]; ok && codexSessionHeaderKeyUsesUnderscore(fallbackKey) {
		selectedKey = fallbackKey
	} else {
		for existingKey := range headers {
			if codexSessionHeaderKeyUsesUnderscore(existingKey) {
				selectedKey = existingKey
				break
			}
		}
	}
	if selectedKey == "" {
		selectedKey = fallbackKey
	}
	for existingKey := range headers {
		if codexSessionHeaderKey(existingKey) && existingKey != selectedKey {
			delete(headers, existingKey)
		}
	}
	headers[selectedKey] = []string{value}
}

func codexSessionHeaderKey(key string) bool {
	normalized := strings.ToLower(strings.TrimSpace(key))
	return normalized == "session_id" || normalized == "session-id"
}

func codexSessionHeaderKeyUsesUnderscore(key string) bool {
	return strings.ToLower(strings.TrimSpace(key)) == "session_id"
}

func headerValueCaseInsensitive(headers http.Header, key string) string {
	key = strings.TrimSpace(key)
	if headers == nil || key == "" {
		return ""
	}
	if val := strings.TrimSpace(headers.Get(key)); val != "" {
		return val
	}
	for existingKey, values := range headers {
		if !strings.EqualFold(existingKey, key) {
			continue
		}
		for _, value := range values {
			if trimmed := strings.TrimSpace(value); trimmed != "" {
				return trimmed
			}
		}
	}
	return ""
}

func deleteHeaderCaseInsensitive(headers http.Header, key string) {
	for existingKey := range headers {
		if strings.EqualFold(existingKey, key) {
			delete(headers, existingKey)
		}
	}
}

func codexHeaderDefaults(cfg *config.Config, auth *cliproxyauth.Auth) (string, string) {
	if cfg == nil || auth == nil {
		return "", ""
	}
	if auth.Attributes != nil {
		if v := strings.TrimSpace(auth.Attributes["api_key"]); v != "" {
			return "", ""
		}
	}
	return strings.TrimSpace(cfg.CodexHeaderDefaults.UserAgent), strings.TrimSpace(cfg.CodexHeaderDefaults.BetaFeatures)
}

func ensureHeaderWithPriority(target http.Header, source http.Header, key, configValue, fallbackValue string) {
	if target == nil {
		return
	}
	if strings.TrimSpace(target.Get(key)) != "" {
		return
	}
	if source != nil {
		if val := strings.TrimSpace(source.Get(key)); val != "" {
			target.Set(key, val)
			return
		}
	}
	if val := strings.TrimSpace(configValue); val != "" {
		target.Set(key, val)
		return
	}
	if val := strings.TrimSpace(fallbackValue); val != "" {
		target.Set(key, val)
	}
}

func ensureHeaderWithConfigPrecedence(target http.Header, source http.Header, key, configValue, fallbackValue string) {
	if target == nil {
		return
	}
	if strings.TrimSpace(target.Get(key)) != "" {
		return
	}
	if val := strings.TrimSpace(configValue); val != "" {
		target.Set(key, val)
		return
	}
	if source != nil {
		if val := strings.TrimSpace(source.Get(key)); val != "" {
			target.Set(key, val)
			return
		}
	}
	if val := strings.TrimSpace(fallbackValue); val != "" {
		target.Set(key, val)
	}
}
