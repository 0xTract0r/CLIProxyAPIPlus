package executor

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/misc"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/thinking"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	codexUserAgent             = "codex-tui/0.135.0 (Mac OS 26.5.0; arm64) iTerm.app/3.6.10 (codex-tui; 0.135.0)"
	codexOriginator            = "codex-tui"
	codexDefaultImageToolModel = "gpt-image-2"
	codexResponsesLiteHeader   = "X-OpenAI-Internal-Codex-Responses-Lite"
	codexResponsesLiteMetadata = "client_metadata.ws_request_header_x_openai_internal_codex_responses_lite"
)

var dataTag = []byte("data:")

func translateCodexRequestPair(from, to sdktranslator.Format, model string, originalPayload, payload []byte, stream bool) ([]byte, []byte) {
	if bytes.Equal(originalPayload, payload) {
		body := sdktranslator.TranslateRequest(from, to, model, payload, stream)
		return body, body
	}
	originalTranslated := sdktranslator.TranslateRequest(from, to, model, originalPayload, stream)
	body := sdktranslator.TranslateRequest(from, to, model, payload, stream)
	return originalTranslated, body
}

// PrepareRequest injects Codex credentials into the outgoing HTTP request.
func (e *CodexExecutor) PrepareRequest(req *http.Request, auth *cliproxyauth.Auth) error {
	if req == nil {
		return nil
	}
	apiKey, _ := codexCreds(auth)
	if strings.TrimSpace(apiKey) != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	var attrs map[string]string
	if auth != nil {
		attrs = auth.Attributes
	}
	util.ApplyCustomHeadersFromAttrs(req, attrs)
	return nil
}

// HttpRequest injects Codex credentials into the request and executes it.
func (e *CodexExecutor) HttpRequest(ctx context.Context, auth *cliproxyauth.Auth, req *http.Request) (*http.Response, error) {
	if req == nil {
		return nil, fmt.Errorf("codex executor: request is nil")
	}
	if ctx == nil {
		ctx = req.Context()
	}
	httpReq := req.WithContext(ctx)
	if err := e.PrepareRequest(httpReq, auth); err != nil {
		return nil, err
	}
	httpClient := helps.NewUtlsHTTPClient(ctx, e.cfg, auth, 0)
	return httpClient.Do(httpReq)
}

type codexIdentityConfuseState struct {
	// enabled 表示 identity-confuse 开关分支生效（受 codexIdentityConfuseEnabled
	// 门控），仅驱动 prompt_cache_key / window_id 这两个字段的混淆。
	enabled bool
	// identityNormalize 表示真机身份字段（installation_id / turn_id /
	// session_id / thread_id）的无条件归一生效。它与 enabled 解耦：只要 authID
	// 有效就为 true，无论 identity-confuse 开关如何，都对这 4 个字段做归一，
	// 避免生产关掉 identity-confuse 时裸泄漏真机指纹（anticorr 方案A）。
	identityNormalize      bool
	authID                 string
	originalPromptCacheKey string
	promptCacheKey         string
	// installationID 是本请求派生出的混淆 installation_id（与 body
	// client_metadata.x-codex-installation-id 同一个派生值）。turn-metadata
	// header/body 副本里的 installation_id 都用它改写，保证处处一致、且不泄漏真机值。
	installationID string
	turnIDs        []codexIdentityReplacement
}

type codexIdentityReplacement struct {
	original string
	confused string
}

func (e *CodexExecutor) cacheHelper(ctx context.Context, from sdktranslator.Format, url string, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, userPayload []byte, rawJSON []byte, headerSets ...http.Header) (*http.Request, []byte, codexIdentityConfuseState, error) {
	var headers http.Header
	if len(headerSets) > 0 {
		headers = headerSets[0]
	}
	var cache helps.CodexCache
	if sourceFormatEqual(from, sdktranslator.FormatClaude) {
		modelName := strings.TrimSpace(gjson.GetBytes(rawJSON, "model").String())
		if modelName == "" {
			modelName = thinking.ParseSuffix(req.Model).ModelName
		}
		cached, ok, errCache := helps.ClaudeCodePromptCache(ctx, modelName, req.Payload, headers)
		if errCache != nil {
			return nil, nil, codexIdentityConfuseState{}, errCache
		}
		if ok {
			cache = cached
		}
	} else if sourceFormatEqual(from, sdktranslator.FormatOpenAIResponse) {
		promptCacheKey := gjson.GetBytes(req.Payload, "prompt_cache_key")
		if promptCacheKey.Exists() {
			cache.ID = promptCacheKey.String()
		}
	} else if sourceFormatEqual(from, sdktranslator.FormatOpenAI) {
		if promptCacheKey := gjson.GetBytes(req.Payload, "prompt_cache_key"); promptCacheKey.Exists() {
			cache.ID = strings.TrimSpace(promptCacheKey.String())
		}
		// fork(anticorr item6): do NOT seed prompt_cache_key from the
		// account-independent derived-session UUID (helps.ProviderSessionUUID). It is
		// keyed on downstream request metadata, not on the account, so the same
		// conversation context routed through two accounts would collide on one
		// prompt_cache_key and cross-link them. The per-account apiKey seed below is
		// the only fallback; with neither a client key nor an apiKey present, cache.ID
		// stays empty and no prompt_cache_key / session header is written.
		if cache.ID == "" {
			if apiKey := strings.TrimSpace(helps.APIKeyFromContext(ctx)); apiKey != "" {
				cache.ID = uuid.NewSHA1(uuid.NameSpaceOID, []byte("cli-proxy-api:codex:prompt-cache:"+apiKey)).String()
			}
		}
	}

	if cache.ID != "" {
		rawJSON = helps.SetStringIfDifferent(rawJSON, "prompt_cache_key", cache.ID)
	}
	rawJSON = helps.SanitizeCodexInputItemIDs(rawJSON)
	var identityState codexIdentityConfuseState
	rawJSON, identityState = applyCodexIdentityConfuseBody(e.cfg, auth, userPayload, rawJSON)
	if identityState.promptCacheKey != "" {
		cache.ID = identityState.promptCacheKey
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(rawJSON))
	if err != nil {
		return nil, nil, codexIdentityConfuseState{}, err
	}
	if cache.ID != "" {
		// fork(anticorr): 出站 session 头名对齐真实 codex 的 session-id（小写连字符）；
		// http.Header.Set 会规范化成 Session-Id，故用 case-preserved 直写 map key。
		setHeaderCasePreserved(httpReq.Header, "session-id", cache.ID)
	}
	return httpReq, rawJSON, identityState, nil
}

func applyCodexIdentityConfuseBody(cfg *config.Config, auth *cliproxyauth.Auth, userPayload []byte, rawJSON []byte) ([]byte, codexIdentityConfuseState) {
	if auth == nil || strings.TrimSpace(auth.ID) == "" || len(rawJSON) == 0 {
		return rawJSON, codexIdentityConfuseState{}
	}

	// identityNormalize 永远生效（与 identity-confuse 开关解耦）：只要 authID 有效，
	// 就对真机身份字段做无条件归一。enabled 仅在 identity-confuse 门控通过时为 true，
	// 额外驱动 prompt_cache_key / window_id 的混淆。
	state := codexIdentityConfuseState{
		identityNormalize: true,
		enabled:           codexIdentityConfuseEnabled(cfg),
		authID:            strings.TrimSpace(auth.ID),
	}

	// prompt_cache_key（受 identity-confuse 门控，保持开关语义）。
	if state.enabled {
		if promptCacheKey := strings.TrimSpace(gjson.GetBytes(userPayload, "prompt_cache_key").String()); promptCacheKey != "" {
			state.originalPromptCacheKey = promptCacheKey
			state.promptCacheKey = codexIdentityConfuseUUID(auth.ID, "prompt-cache", promptCacheKey)
			rawJSON = helps.SetStringIfDifferent(rawJSON, "prompt_cache_key", state.promptCacheKey)
		}
	}

	// fork(anticorr 方案A): installation_id 真机身份字段无条件归一（与开关解耦）。
	// fork(anticorr A-3): installation_id 缺省每账号稳定派生兜底。
	// 旧逻辑仅当 body 已带 x-codex-installation-id 时才改写；客户端没传则字段缺失/空，
	// 出站缺这个字段（与真实 codex 客户端不一致，且无法做到每账号稳定）。现在缺失/空时
	// 用固定种子 "default" 按 auth.ID 派生兜底注入，保证同账号跨请求稳定、跨账号不同；
	// 客户端有传时仍按原值派生，行为不变。
	if installationID := strings.TrimSpace(gjson.GetBytes(userPayload, "client_metadata.x-codex-installation-id").String()); installationID != "" {
		state.installationID = codexIdentityConfuseUUID(auth.ID, "installation", installationID)
	} else {
		state.installationID = codexIdentityConfuseUUID(auth.ID, "installation", "default")
	}
	rawJSON, _ = sjson.SetBytes(rawJSON, "client_metadata.x-codex-installation-id", state.installationID)

	// turn-metadata（body client_metadata 副本）：installation_id / turn_id /
	// session_id / thread_id 无条件归一；prompt_cache_key / window_id 仅在门控开时。
	if turnMetadata := strings.TrimSpace(gjson.GetBytes(rawJSON, "client_metadata.x-codex-turn-metadata").String()); turnMetadata != "" {
		rawJSON, _ = sjson.SetBytes(rawJSON, "client_metadata.x-codex-turn-metadata", applyCodexTurnMetadataIdentityConfuse(turnMetadata, &state))
	}

	// window_id（受 identity-confuse 门控，保持开关语义）。
	if state.enabled && state.promptCacheKey != "" {
		if windowID := strings.TrimSpace(gjson.GetBytes(rawJSON, "client_metadata.x-codex-window-id").String()); windowID != "" {
			rawJSON, _ = sjson.SetBytes(rawJSON, "client_metadata.x-codex-window-id", state.promptCacheKey+":0")
		}
	}

	return rawJSON, state
}

func applyCodexIdentityConfuseHeaders(headers http.Header, state *codexIdentityConfuseState) {
	if headers == nil {
		return
	}
	if state == nil {
		return
	}

	// header X-Codex-Turn-Metadata 里的真机身份字段无条件归一（与开关解耦）；
	// 内部 applyCodexTurnMetadataIdentityConfuse 会按 identityNormalize / enabled
	// 分别处理身份字段与 prompt_cache_key / window_id。
	if state.identityNormalize && strings.TrimSpace(state.authID) != "" {
		if rawTurnMetadata := strings.TrimSpace(headers.Get("X-Codex-Turn-Metadata")); rawTurnMetadata != "" {
			headers.Set("X-Codex-Turn-Metadata", applyCodexTurnMetadataIdentityConfuse(rawTurnMetadata, state))
		}
	}
	if !state.enabled || state.promptCacheKey == "" {
		return
	}

	// fork(anticorr): 出站头名对齐真实 codex —— session-id / thread-id 均为
	// 小写连字符（旧实现是 Session_id 下划线 + Thread-Id 大写）。Header.Set 会
	// 规范化成 Session-Id / Thread-Id，故对这两个头用 case-preserved 直写 map key。
	setCodexOutboundLowerSessionHeader(headers, state.promptCacheKey)
	if headerValueCaseInsensitive(headers, "Conversation_id") != "" {
		setHeaderCasePreserved(headers, "Conversation_id", state.promptCacheKey)
	}
	headers.Set("X-Client-Request-Id", state.promptCacheKey)
	setHeaderCasePreserved(headers, "thread-id", state.promptCacheKey)
	headers.Set("X-Codex-Window-Id", state.promptCacheKey+":0")
}

// setCodexOutboundLowerSessionHeader forces the codex session header onto the
// real-codex lowercase-hyphen "session-id" form, deleting any Session_id /
// Session-Id case variants first (http.Header.Set would canonicalize to Session-Id).
// This is the fork anti-corr equivalent of the codex-websocket setCodexLowerSessionHeader,
// defined locally so this executor family does not depend on the WS executor file.
func setCodexOutboundLowerSessionHeader(headers http.Header, value string) {
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

// ensureCodexOutboundLowerSessionHeader injects a lowercase-hyphen "session-id"
// when the outbound request carries no session header variant yet (source-first,
// else fallback), and collapses any existing variant onto "session-id" without
// changing its value. Fork anti-corr equivalent of the WS ensureCodexLowerSessionHeader.
func ensureCodexOutboundLowerSessionHeader(target http.Header, source http.Header, fallbackValue string) {
	if target == nil {
		return
	}
	if existing := strings.TrimSpace(codexSessionHeaderValue(target)); existing != "" {
		setCodexOutboundLowerSessionHeader(target, existing)
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
		setCodexOutboundLowerSessionHeader(target, value)
	}
}

func applyCodexTurnMetadataIdentityConfuse(rawTurnMetadata string, state *codexIdentityConfuseState) string {
	updatedTurnMetadata := rawTurnMetadata
	if state == nil {
		return updatedTurnMetadata
	}

	// 真机身份字段（installation_id / turn_id / session_id / thread_id）无条件归一，
	// 与 identity-confuse 开关解耦（anticorr 方案A）。只要 identityNormalize 且
	// authID 有效就改写，避免生产关掉 identity-confuse 时这些字段裸泄漏真机指纹。
	if state.identityNormalize && strings.TrimSpace(state.authID) != "" {
		// installation_id 与 client_metadata.x-codex-installation-id 用同一个派生值改写，
		// 避免 turn-metadata（header + body 副本）里残留真机 installation_id 或与
		// client_metadata 那个混淆值发散。
		if state.installationID != "" && gjson.Get(updatedTurnMetadata, "installation_id").Exists() {
			updatedTurnMetadata, _ = sjson.Set(updatedTurnMetadata, "installation_id", state.installationID)
		}
		// turn_id 存在才改写（缺失不注入），并登记到 turnIDs 供上游 SSE 回换真值。
		if turnID := strings.TrimSpace(gjson.Get(rawTurnMetadata, "turn_id").String()); turnID != "" {
			updatedTurnMetadata, _ = sjson.Set(updatedTurnMetadata, "turn_id", state.confuseTurnID(turnID))
		}
		// session_id / thread_id：request 单向、不回显 → 只改写、不回换。存在才改、缺失不注入。
		if sessionID := strings.TrimSpace(gjson.Get(rawTurnMetadata, "session_id").String()); sessionID != "" {
			updatedTurnMetadata, _ = sjson.Set(updatedTurnMetadata, "session_id", codexIdentityConfuseUUID(state.authID, "session", sessionID))
		}
		if threadID := strings.TrimSpace(gjson.Get(rawTurnMetadata, "thread_id").String()); threadID != "" {
			updatedTurnMetadata, _ = sjson.Set(updatedTurnMetadata, "thread_id", codexIdentityConfuseUUID(state.authID, "thread", threadID))
		}
	}

	// prompt_cache_key / window_id 仍受 identity-confuse 门控（保持开关语义）。
	if !state.enabled {
		return updatedTurnMetadata
	}
	if state.promptCacheKey != "" && gjson.Get(rawTurnMetadata, "prompt_cache_key").Exists() {
		updatedTurnMetadata, _ = sjson.Set(updatedTurnMetadata, "prompt_cache_key", state.promptCacheKey)
	} else if state.promptCacheKey != "" && state.originalPromptCacheKey != "" {
		updatedTurnMetadata = strings.ReplaceAll(updatedTurnMetadata, state.originalPromptCacheKey, state.promptCacheKey)
	}
	if state.promptCacheKey != "" && gjson.Get(rawTurnMetadata, "window_id").Exists() {
		updatedTurnMetadata, _ = sjson.Set(updatedTurnMetadata, "window_id", state.promptCacheKey+":0")
	}
	return updatedTurnMetadata
}

func applyCodexIdentityConfuseResponsePayload(payload []byte, state codexIdentityConfuseState) []byte {
	payload = replaceCodexIdentityResponsePayload(payload, state.originalPromptCacheKey, state.promptCacheKey)
	for _, turnID := range state.turnIDs {
		payload = replaceCodexIdentityResponsePayload(payload, turnID.original, turnID.confused)
	}
	return payload
}

func applyCodexIdentityExposeResponsePayload(payload []byte, state codexIdentityConfuseState) []byte {
	payload = replaceCodexIdentityResponsePayload(payload, state.promptCacheKey, state.originalPromptCacheKey)
	for _, turnID := range state.turnIDs {
		payload = replaceCodexIdentityResponsePayload(payload, turnID.confused, turnID.original)
	}
	return payload
}

func (state *codexIdentityConfuseState) confuseTurnID(turnID string) string {
	turnID = strings.TrimSpace(turnID)
	// turn_id 归一与 identity-confuse 开关解耦：identityNormalize 生效即处理（anticorr 方案A）。
	if state == nil || !state.identityNormalize || strings.TrimSpace(state.authID) == "" || turnID == "" {
		return turnID
	}
	for _, replacement := range state.turnIDs {
		if replacement.original == turnID || replacement.confused == turnID {
			return replacement.confused
		}
	}
	confusedTurnID := codexIdentityConfuseUUID(state.authID, "turn", turnID)
	state.turnIDs = append(state.turnIDs, codexIdentityReplacement{original: turnID, confused: confusedTurnID})
	return confusedTurnID
}

func replaceCodexIdentityResponsePayload(payload []byte, from string, to string) []byte {
	from = strings.TrimSpace(from)
	to = strings.TrimSpace(to)
	if len(payload) == 0 || from == "" || to == "" || from == to || !bytes.Contains(payload, []byte(from)) {
		return payload
	}
	return bytes.ReplaceAll(payload, []byte(from), []byte(to))
}

func codexIdentityConfuseEnabled(cfg *config.Config) bool {
	if cfg == nil || !cfg.Codex.IdentityConfuse {
		return false
	}
	strategy := strings.ToLower(strings.TrimSpace(cfg.Routing.Strategy))
	return cfg.Routing.SessionAffinity || strategy == "fill-first" || strategy == "fillfirst" || strategy == "ff"
}

func codexIdentityConfuseUUID(authID string, kind string, value string) string {
	name := strings.Join([]string{"cli-proxy-api", "codex", "identity-confuse", kind, strings.TrimSpace(authID), strings.TrimSpace(value)}, ":")
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte(name)).String()
}

func applyCodexHeaders(r *http.Request, auth *cliproxyauth.Auth, token string, stream bool, cfg *config.Config) {
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+token)

	var ginHeaders http.Header
	if ginCtx, ok := r.Context().Value("gin").(*gin.Context); ok && ginCtx != nil && ginCtx.Request != nil {
		ginHeaders = ginCtx.Request.Header
	}

	if ginHeaders.Get("X-Codex-Beta-Features") != "" {
		r.Header.Set("X-Codex-Beta-Features", ginHeaders.Get("X-Codex-Beta-Features"))
	}
	// fork: codex client device-profile / fingerprint 稳定化（反风控）。
	var profile helps.CodexClientProfile
	if auth != nil {
		profile = helps.ResolveCodexClientProfile(auth, ginHeaders, cfg)
	}
	misc.EnsureHeader(r.Header, ginHeaders, "X-Codex-Turn-Metadata", "")
	misc.EnsureHeader(r.Header, ginHeaders, "X-Client-Request-Id", "")
	// fork(anticorr): 真实 codex 出站没有独立 Version 头，版本只体现在 UA 里。
	// 旧实现会从客户端或 device-profile 注入 Version，是 CPA 独有指纹，删除（含
	// 客户端透传值），不再回写。
	deleteHeaderCaseInsensitive(r.Header, "Version")
	if auth != nil && strings.TrimSpace(profile.UserAgent) != "" {
		userAgent := strings.TrimSpace(profile.UserAgent)
		r.Header.Set("User-Agent", userAgent)
	} else {
		cfgUserAgent, _ := codexHeaderDefaults(cfg, auth)
		ensureHeaderWithConfigPrecedence(r.Header, ginHeaders, "User-Agent", cfgUserAgent, helps.DefaultCodexManagedUserAgent())
	}
	applyCodexCommunityDesktopHeaders(r.Header, profile)

	if strings.Contains(r.Header.Get("User-Agent"), "Mac OS") {
		// fork(anticorr): 出站 session 头名对齐真实 codex 的 session-id（小写连字符）。
		// 缺失时优先沿用客户端值，否则生成 uuid。已有任意大小写变体则不覆盖。
		ensureCodexOutboundLowerSessionHeader(r.Header, ginHeaders, uuid.NewString())
	}

	if stream {
		r.Header.Set("Accept", "text/event-stream")
	} else {
		r.Header.Set("Accept", "application/json")
	}
	// fork(anticorr): 真实 codex 出站不带 Connection 头（由 Go transport 自行管理
	// keep-alive），旧实现写死 Connection: Keep-Alive 是 CPA 独有指纹，删除。

	isAPIKey := false
	if auth != nil && auth.Attributes != nil {
		if v := strings.TrimSpace(auth.Attributes["api_key"]); v != "" {
			isAPIKey = true
		}
	}
	// fork(anticorr A-1): managed（非 api-key）codex 账号的 Originator 钉死。
	// 旧逻辑无条件第一优先透传客户端 Originator，客户端可任意覆盖出站身份（伪造污染）。
	// 现在对 managed 账号只接受白名单内的 first-party Originator（codex-tui /
	// codex_cli_rs / codex_vscode / codex_exec / "Codex " 前缀），其余一律钉死为
	// profile.Originator（无则 DefaultCodexManagedOriginator），不可被下游覆盖。
	// api-key 账号沿用旧行为（透传客户端 Originator），不在本次加固范围内。
	clientOriginator := strings.TrimSpace(ginHeaders.Get("Originator"))
	if isAPIKey {
		if clientOriginator != "" {
			r.Header.Set("Originator", clientOriginator)
		}
	} else {
		pinnedOriginator := strings.TrimSpace(profile.Originator)
		if pinnedOriginator == "" {
			pinnedOriginator = helps.DefaultCodexManagedOriginator()
		}
		// 只接受白名单内的客户端值；其余覆盖为 managed 钉死值。
		if clientOriginator != "" && helps.IsFirstPartyCodexOriginator(clientOriginator) {
			pinnedOriginator = clientOriginator
		}
		r.Header.Set("Originator", pinnedOriginator)
	}
	if !isAPIKey {
		if auth != nil && auth.Metadata != nil {
			if accountID, ok := auth.Metadata["account_id"].(string); ok {
				r.Header.Set("Chatgpt-Account-Id", accountID)
			}
		}
	}
	managedHeaderSnapshot := captureManagedHeaderSnapshot(r.Header, []string{
		"User-Agent",
		"Version",
		"Originator",
		"X-Codex-Beta-Features",
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
	util.ApplyCustomHeadersFromAttrs(r, attrs)
	if cliproxyauth.HasStructuredAccountSettingsMetadata(auth) {
		applyManagedHeaderSnapshot(r.Header, managedHeaderSnapshot)
	}
	// fork(anticorr Wave10-D 要点4)：CLI 画像下兜底剥离 Desktop 专属指纹头。
	// 账号 metadata.headers / header:* 属性里可能残留历史 Desktop bundle 的
	// sec-ch-ua / sec-ch-ua-* （真实 codex-rs CLI 不发这些）。ApplyCustomHeadersFromAttrs
	// 会把这些残留 set 回 r.Header，而 managed snapshot 只恢复 snapshot 内已有的名字
	// （CLI snapshot 不含 sec-ch-ua），无法挡住。这里在 CLI 画像（profile 非 Desktop）下
	// 显式删除，确保出站不带 sec-ch-ua 系列，与 CLI body/TLS 自洽。
	stripCodexDesktopOnlyHeadersForCLIProfile(r.Header, profile)
	// fork(anticorr): 兜底再删一次 Version —— 真实 codex 出站无独立 Version 头。
	// 上面的自定义 header（header:Version 属性）可能在 snapshot 恢复后又写回 Version，
	// 这里最终统一剥离，确保出站任何来源的 Version 都不上线。
	deleteHeaderCaseInsensitive(r.Header, "Version")
}

// stripCodexDesktopOnlyHeadersForCLIProfile 在 CLI 画像下删除 Desktop 专属指纹头。
// 仅当 profile 非 Desktop 家族（即 CLI 画像）时生效；Desktop 画像不受影响，保持兼容。
func stripCodexDesktopOnlyHeadersForCLIProfile(headers http.Header, profile helps.CodexClientProfile) {
	if headers == nil || helps.IsCodexDesktopProfile(profile) {
		return
	}
	for _, name := range []string{
		"sec-ch-ua",
		"sec-ch-ua-mobile",
		"sec-ch-ua-platform",
		"sec-fetch-site",
		"sec-fetch-mode",
		"sec-fetch-dest",
	} {
		headers.Del(name)
	}
}

func applyCodexCommunityDesktopHeaders(headers http.Header, profile helps.CodexClientProfile) {
	if headers == nil {
		return
	}
	for name, value := range map[string]string{
		"sec-ch-ua":          profile.SecCHUA,
		"sec-ch-ua-mobile":   profile.SecCHUAMobile,
		"sec-ch-ua-platform": profile.SecCHUAPlatform,
		"Accept-Encoding":    profile.AcceptEncoding,
		"Accept-Language":    profile.AcceptLanguage,
		"sec-fetch-site":     profile.SecFetchSite,
		"sec-fetch-mode":     profile.SecFetchMode,
		"sec-fetch-dest":     profile.SecFetchDest,
	} {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			headers.Set(name, trimmed)
		}
	}
}

// applyModelHeaderOverrides forces models.json config.override_header onto upstream headers.
func applyModelHeaderOverrides(headers http.Header, modelName string) {
	if headers == nil {
		return
	}
	overrides := registry.ModelOverrideHeaders(modelName)
	if len(overrides) == 0 {
		return
	}
	for key, value := range overrides {
		headers.Set(key, value)
	}
	if strings.Contains(headers.Get("User-Agent"), "Mac OS") && codexSessionHeaderValue(headers) == "" {
		headers.Set("Session_id", uuid.NewString())
	}
}

// applyCodexDirectImageHeaders sets Codex upstream headers for direct /images/* calls.
// Downstream client User-Agent values are not forwarded to reduce Cloudflare 1010 blocks.
func applyCodexDirectImageHeaders(r *http.Request, auth *cliproxyauth.Auth, token string, stream bool, cfg *config.Config) {
	var ginHeaders http.Header
	if ginCtx, ok := r.Context().Value("gin").(*gin.Context); ok && ginCtx != nil && ginCtx.Request != nil {
		ginHeaders = ginCtx.Request.Header.Clone()
		ginHeaders.Del("User-Agent")
	}
	applyCodexHeadersFromSources(r, auth, token, stream, cfg, ginHeaders)
}

func applyCodexHeadersFromSources(r *http.Request, auth *cliproxyauth.Auth, token string, stream bool, cfg *config.Config, ginHeaders http.Header) {
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+token)

	if ginHeaders != nil && ginHeaders.Get("X-Codex-Beta-Features") != "" {
		r.Header.Set("X-Codex-Beta-Features", ginHeaders.Get("X-Codex-Beta-Features"))
	}
	misc.EnsureHeader(r.Header, ginHeaders, "Version", "")
	misc.EnsureHeader(r.Header, ginHeaders, "X-Codex-Turn-Metadata", "")
	misc.EnsureHeader(r.Header, ginHeaders, "X-Client-Request-Id", "")
	cfgUserAgent, _ := codexHeaderDefaults(cfg, auth)
	ensureHeaderWithConfigPrecedence(r.Header, ginHeaders, "User-Agent", cfgUserAgent, codexUserAgent)

	if strings.Contains(r.Header.Get("User-Agent"), "Mac OS") {
		misc.EnsureHeader(r.Header, ginHeaders, "Session_id", uuid.NewString())
	}

	if stream {
		r.Header.Set("Accept", "text/event-stream")
	} else {
		r.Header.Set("Accept", "application/json")
	}
	r.Header.Set("Connection", "Keep-Alive")

	isAPIKey := false
	if auth != nil && auth.Attributes != nil {
		if v := strings.TrimSpace(auth.Attributes["api_key"]); v != "" {
			isAPIKey = true
		}
	}
	if originator := strings.TrimSpace(ginHeaders.Get("Originator")); originator != "" {
		r.Header.Set("Originator", originator)
	} else if !isAPIKey {
		r.Header.Set("Originator", codexOriginator)
	}
	if !isAPIKey {
		if auth != nil && auth.Metadata != nil {
			if accountID, ok := auth.Metadata["account_id"].(string); ok {
				r.Header.Set("Chatgpt-Account-Id", accountID)
			}
		}
	}
	var attrs map[string]string
	if auth != nil {
		attrs = auth.Attributes
	}
	util.ApplyCustomHeadersFromAttrs(r, attrs)
}

func normalizeCodexInstructions(body []byte) []byte {
	instructions := gjson.GetBytes(body, "instructions")
	if !instructions.Exists() || instructions.Type == gjson.Null {
		body, _ = sjson.SetBytes(body, "instructions", "")
	}
	return body
}

var imageGenToolJSON = []byte(`{"type":"image_generation","output_format":"png"}`)
var imageGenToolArrayJSON = []byte(`[{"type":"image_generation","output_format":"png"}]`)

func isCodexFreePlanAuth(auth *cliproxyauth.Auth) bool {
	if auth == nil || auth.Attributes == nil {
		return false
	}
	if !strings.EqualFold(strings.TrimSpace(auth.Provider), "codex") {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(auth.Attributes["plan_type"]), "free")
}

func isImageGenerationFunctionTool(tool gjson.Result) bool {
	switch tool.Get("type").String() {
	case "function":
		return tool.Get("name").String() == "image_gen.imagegen"
	case "namespace":
		if tool.Get("name").String() != "image_gen" {
			return false
		}
		tools := tool.Get("tools")
		if !tools.IsArray() {
			return false
		}
		for _, nestedTool := range tools.Array() {
			if nestedTool.Get("type").String() == "function" && nestedTool.Get("name").String() == "imagegen" {
				return true
			}
		}
	}
	return false
}

func isCodexResponsesLiteRequest(body []byte, headers http.Header) bool {
	if strings.EqualFold(strings.TrimSpace(headers.Get(codexResponsesLiteHeader)), "true") {
		return true
	}
	// Codex Desktop mirrors websocket-only request headers into client_metadata.
	value := gjson.GetBytes(body, codexResponsesLiteMetadata)
	if !value.Exists() {
		return false
	}
	return value.Type == gjson.True || value.Type == gjson.String && strings.EqualFold(strings.TrimSpace(value.String()), "true")
}

func ensureImageGenerationTool(body []byte, baseModel string, auth *cliproxyauth.Auth, headers http.Header) []byte {
	if isCodexResponsesLiteRequest(body, headers) {
		return body
	}
	if strings.HasSuffix(baseModel, "spark") {
		return body
	}
	if isCodexFreePlanAuth(auth) {
		return body
	}

	tools := gjson.GetBytes(body, "tools")
	if !tools.Exists() || !tools.IsArray() {
		body, _ = sjson.SetRawBytes(body, "tools", imageGenToolArrayJSON)
		return body
	}
	for _, t := range tools.Array() {
		if t.Get("type").String() == "image_generation" || isImageGenerationFunctionTool(t) {
			return body
		}
	}
	body, _ = sjson.SetRawBytes(body, "tools.-1", imageGenToolJSON)
	return body
}

// stripImageGenerationTool 从请求体 tools 数组里移除所有 type==image_generation
// 的工具（包括 Codex 客户端自带的完整 gpt-image-2 定义）。若移除后 tools 变为空
// 数组，则删除整个 tools 字段，避免空 tools + tool_choice 触发上游报错。无 tools
// 字段时原样返回。
func stripImageGenerationTool(body []byte) []byte {
	tools := gjson.GetBytes(body, "tools")
	if !tools.Exists() || !tools.IsArray() {
		return body
	}
	arr := tools.Array()
	// 从后往前删，避免下标在删除过程中漂移。
	for i := len(arr) - 1; i >= 0; i-- {
		if arr[i].Get("type").String() == "image_generation" {
			body, _ = sjson.DeleteBytes(body, "tools."+strconv.Itoa(i))
		}
	}
	// 重新读取，若 tools 已为空数组则删除整个字段。
	if remaining := gjson.GetBytes(body, "tools"); remaining.IsArray() && len(remaining.Array()) == 0 {
		body, _ = sjson.DeleteBytes(body, "tools")
	}
	return body
}

// applyImageGenerationPolicy decides how to handle the Codex image_generation tool
// based on the tri-state config (DisableImageGenerationMode):
//
//	DisableImageGenerationOff  (config "false") → inject the tool via
//	    ensureImageGenerationTool, which still skips free-plan / spark / responses-lite.
//	DisableImageGenerationAll  (config "true")  → strip the tool.
//	DisableImageGenerationChat (config "chat")  → strip on this Codex (chat-style) path.
//
// The loaded config default is Off, so by default the Codex image_generation tool is
// injected on this chat-style path (matching upstream). nil cfg strips, defensively.
func applyImageGenerationPolicy(cfg *config.Config, body []byte, baseModel string, auth *cliproxyauth.Auth, headers http.Header) []byte {
	if cfg != nil && cfg.DisableImageGeneration == config.DisableImageGenerationOff {
		return ensureImageGenerationTool(body, baseModel, auth, headers)
	}
	return stripImageGenerationTool(body)
}

func normalizeCodexParallelToolCalls(body []byte, headers http.Header) []byte {
	if isCodexResponsesLiteRequest(body, headers) {
		body = helps.SetBoolIfDifferent(body, "parallel_tool_calls", false)
		return body
	}
	return normalizeCodexParallelToolCallsForTools(body)
}

func normalizeCodexParallelToolCallsForTools(body []byte) []byte {
	if !gjson.GetBytes(body, "parallel_tool_calls").Exists() {
		return body
	}

	tools := gjson.GetBytes(body, "tools")
	hasTools := tools.Exists() && tools.IsArray() && len(tools.Array()) > 0
	if hasTools {
		return body
	}

	body, _ = sjson.DeleteBytes(body, "parallel_tool_calls")
	return body
}

func publishCodexImageToolUsage(ctx context.Context, reporter *helps.UsageReporter, body []byte, completedData []byte) {
	detail, ok := helps.ParseCodexImageToolUsage(completedData)
	if !ok {
		return
	}
	reporter.EnsurePublished(ctx)
	reporter.PublishAdditionalModel(ctx, codexImageGenerationToolModel(body), detail)
}

func codexImageGenerationToolModel(body []byte) string {
	tools := gjson.GetBytes(body, "tools")
	if tools.IsArray() {
		for _, tool := range tools.Array() {
			if tool.Get("type").String() != "image_generation" {
				continue
			}
			if model := strings.TrimSpace(tool.Get("model").String()); model != "" {
				return model
			}
			break
		}
	}
	return codexDefaultImageToolModel
}
