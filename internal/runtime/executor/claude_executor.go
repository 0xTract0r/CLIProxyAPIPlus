package executor

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	sigcompat "github.com/router-for-me/CLIProxyAPI/v8/internal/signature"
	responsesclaude "github.com/router-for-me/CLIProxyAPI/v8/internal/translator/claude/openai/responses"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// ClaudeExecutor handles Anthropic Claude over the messages API and keeps bounded
// in-memory thread alias state for OAuth continuations.
// If api_key is unavailable on auth, it falls back to legacy via ClientAdapter.
type ClaudeExecutor struct {
	cfg                     *config.Config
	requestLogProvider      string
	upstreamModelNormalizer func(string) string
	// authManager wires the auth manager so the per-account claude client
	// device-profile high-water can be persisted into the auth record
	// (auth.Metadata[claude_device_high_water]). Without a manager the high-water
	// write-back degrades to a no-op.
	authManager         *cliproxyauth.Manager
	oauthProfileFetcher claudeOAuthProfileFetcher
	// oauthToolAliases is shared by every ForAPIKey copy of this executor;
	// constructors allocate it so copies made per request reuse one store.
	oauthToolAliases *claudeOAuthToolAliasStore
}

type claudeOAuthCancellationError struct {
	cause error
}

func (e *claudeOAuthCancellationError) Error() string {
	if e == nil || e.cause == nil {
		return ""
	}
	return e.cause.Error()
}

func (e *claudeOAuthCancellationError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

func (e *claudeOAuthCancellationError) IsRequestScoped() bool {
	return e != nil
}

func newClaudeOAuthCancellationError(ctx context.Context, oauth bool, err error) error {
	if !oauth {
		return nil
	}
	cause := err
	if ctx != nil && ctx.Err() != nil {
		cause = ctx.Err()
	}
	if !errors.Is(cause, context.Canceled) {
		return nil
	}
	return &claudeOAuthCancellationError{cause: cause}
}

func shouldSanitizeClaudeMessagesForUpstream(baseModel string) bool {
	return sigcompat.SignatureProviderFromModelName(baseModel) == sigcompat.SignatureProviderClaude
}

func sanitizeClaudeMessagesForClaudeUpstreamWithDebug(ctx context.Context, body []byte, baseModel string, preserveEmptyThinkingBlocks ...bool) []byte {
	sanitized := body
	preserveEmpty := len(preserveEmptyThinkingBlocks) > 0 && preserveEmptyThinkingBlocks[0]
	if shouldSanitizeClaudeMessagesForUpstream(baseModel) || preserveEmpty {
		var report sigcompat.SignatureSanitizeReport
		sanitized, report = sigcompat.SanitizeClaudeMessagesForClaudeUpstream(body, baseModel, preserveEmptyThinkingBlocks...)
		logClaudeSignatureSanitizeReport(ctx, baseModel, report)
	}
	return helps.NormalizeClaudeToolCallIDs(sanitizeClaudeWebSearchDomains(sanitized))
}

// sanitizeClaudeWebSearchDomains removes empty allowed_domains/blocked_domains
// arrays from built-in web_search tools. Some clients (e.g. litellm) emit an
// empty array instead of omitting the field, and Anthropic rejects it with
// "Empty list of domains is ambiguous. Provide at least one domain or null.".
// Deleting the key is equivalent to leaving it unset.
func sanitizeClaudeWebSearchDomains(body []byte) []byte {
	tools := gjson.GetBytes(body, "tools")
	if !tools.Exists() || !tools.IsArray() {
		return body
	}
	tools.ForEach(func(index, tool gjson.Result) bool {
		if !strings.HasPrefix(tool.Get("type").String(), "web_search_") {
			return true
		}
		for _, field := range []string{"allowed_domains", "blocked_domains"} {
			value := tool.Get(field)
			if value.Exists() && value.IsArray() && len(value.Array()) == 0 {
				path := fmt.Sprintf("tools.%d.%s", index.Int(), field)
				if updated, errDelete := sjson.DeleteBytes(body, path); errDelete == nil {
					body = updated
				}
			}
		}
		return true
	})
	return body
}

func logClaudeSignatureSanitizeReport(ctx context.Context, baseModel string, report sigcompat.SignatureSanitizeReport) {
	if report.DroppedBlocks == 0 && report.DroppedSignatures == 0 && report.ReplacedSignatures == 0 {
		return
	}

	fields := log.Fields{
		"component":           "signature_sanitizer",
		"executor":            "claude",
		"action":              "sanitize_claude_messages",
		"target_provider":     string(report.TargetProvider),
		"target_model":        baseModel,
		"preserved":           report.Preserved,
		"dropped_blocks":      report.DroppedBlocks,
		"dropped_signatures":  report.DroppedSignatures,
		"replaced_signatures": report.ReplacedSignatures,
	}
	if len(report.Decisions) > 0 {
		decision := report.Decisions[0]
		fields["first_block_kind"] = string(decision.BlockKind)
		fields["first_detected_provider"] = string(decision.DetectedProvider)
		fields["first_reason"] = decision.Reason
	}

	helps.LogWithRequestID(ctx).WithFields(fields).Debug("claude executor: sanitized signature history before upstream")
}

// oauthToolRenameMap maps OpenCode-style (lowercase) tool names to Claude Code-style
// (TitleCase) names. Anthropic uses tool name fingerprinting to detect third-party
// clients on OAuth traffic. Renaming to official names avoids extra-usage billing.
// All tools are mapped to TitleCase equivalents to match Claude Code naming patterns.
var oauthToolRenameMap = map[string]string{
	"bash":         "Bash",
	"read":         "Read",
	"write":        "Write",
	"edit":         "Edit",
	"glob":         "Glob",
	"grep":         "Grep",
	"task":         "Task",
	"webfetch":     "WebFetch",
	"todowrite":    "TodoWrite",
	"question":     "Question",
	"skill":        "Skill",
	"ls":           "LS",
	"todoread":     "TodoRead",
	"notebookedit": "NotebookEdit",
}

// The reverse map is now computed per-request in remapOAuthToolNames so that
// only names the client actually caused us to rewrite are restored on the
// response. A global reverse map — as used previously — corrupted responses
// for clients that sent mixed casing (e.g. Amp CLI sends `Bash` TitleCase
// alongside `glob` lowercase; the request flagged renames via `glob→Glob`,
// then the global reverse map incorrectly rewrote every `Bash` in the
// response to `bash`, causing Amp to reject the tool_use as unknown).

// oauthToolsToRemove lists tool names that must be stripped from OAuth requests
// even after remapping. Currently empty — all tools are mapped instead of removed.
var oauthToolsToRemove = map[string]bool{}

// Anthropic-compatible upstreams may reject or even crash when Claude models
// omit max_tokens. Prefer registered model metadata before using a fallback.
const defaultModelMaxTokens = 1024

const claudeClientSafeTransportErrorMessage = "upstream transport error"

// stripTranslatedDefaultMaxTokens removes the translator-synthesized default
// max_tokens from originalTranslated when the caller's original request did not
// explicitly supply an output token limit in the source protocol, allowing
// payload.default rules targeting max_tokens to take effect.
func stripTranslatedDefaultMaxTokens(from sdktranslator.Format, originalPayload, originalTranslated []byte) []byte {
	if len(originalTranslated) == 0 || from == sdktranslator.FormatClaude {
		return originalTranslated
	}
	hasExplicitMaxTokens := func(paths ...string) bool {
		for _, path := range paths {
			if v := gjson.GetBytes(originalPayload, path); v.Exists() && v.Type != gjson.Null {
				return true
			}
		}
		return false
	}
	switch from {
	case sdktranslator.FormatOpenAI:
		if hasExplicitMaxTokens("max_tokens", "max_completion_tokens") {
			return originalTranslated
		}
	case sdktranslator.FormatOpenAIResponse:
		if hasExplicitMaxTokens("max_output_tokens") {
			return originalTranslated
		}
	case sdktranslator.FormatGemini:
		if hasExplicitMaxTokens("generationConfig.maxOutputTokens") {
			return originalTranslated
		}
	default:
		return originalTranslated
	}
	stripped, errDelete := sjson.DeleteBytes(originalTranslated, "max_tokens")
	if errDelete != nil {
		return originalTranslated
	}
	return stripped
}

func NewClaudeExecutor(cfg *config.Config) *ClaudeExecutor {
	return &ClaudeExecutor{cfg: cfg, oauthToolAliases: &claudeOAuthToolAliasStore{}}
}

// NewClaudeExecutorWithManager wires the auth manager so the per-account claude
// client device-profile high-water can be persisted into the auth record
// (auth.Metadata[claude_device_high_water]). Without a manager the high-water
// write-back degrades to a no-op, preserving the prior in-memory-only behavior
// for any caller still using NewClaudeExecutor.
func NewClaudeExecutorWithManager(cfg *config.Config, manager *cliproxyauth.Manager) *ClaudeExecutor {
	return &ClaudeExecutor{cfg: cfg, authManager: manager, oauthToolAliases: &claudeOAuthToolAliasStore{}}
}

func (e *ClaudeExecutor) Identifier() string { return "claude" }

func (e *ClaudeExecutor) SupportsHTTPAttemptGate() bool { return true }
func (e *ClaudeExecutor) claudeWebSearchResultCacheScope(ctx context.Context, auth *cliproxyauth.Auth, from sdktranslator.Format, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, originalPayload []byte) (string, time.Duration, int) {
	if e == nil || e.cfg == nil || !e.cfg.Client.Codex.WebSearchResultCache.Enabled {
		return "", 0, 0
	}
	if !sourceFormatEqual(from, sdktranslator.FormatOpenAIResponse) {
		return "", 0, 0
	}
	sessionKey := xaiReasoningReplayIsolateSessionKey(ctx, codexReasoningReplaySessionKey(ctx, from, req, opts, originalPayload))
	if sessionKey == "" {
		return "", 0, 0
	}
	if credScope := claudeWebSearchResultCacheCredentialScope(auth); credScope != "" {
		sessionKey = credScope + ":" + sessionKey
	}
	cacheCfg := e.cfg.Client.Codex.WebSearchResultCache
	return sessionKey, cacheCfg.EffectiveTTL(), cacheCfg.EffectiveLimit()
}

func claudeWebSearchResultCacheCredentialScope(auth *cliproxyauth.Auth) string {
	if auth == nil {
		return ""
	}
	identity := strings.TrimSpace(auth.ID)
	if identity == "" {
		identity = helps.ClaudeCLIAuthIdentitySeed(auth)
	}
	if identity == "" {
		apiKey, baseURL := claudeCreds(auth)
		identity = strings.TrimSpace(apiKey)
		if identity == "" {
			identity = strings.TrimSpace(baseURL)
		}
	}
	if identity == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(identity))
	return "claude:" + hex.EncodeToString(sum[:8])
}

var _ cliproxyauth.NativeToolSearchSupport = (*ClaudeExecutor)(nil)

// SupportsNativeToolSearch reports whether a credential can receive Codex
// tool_search as Anthropic native tool search (defer_loading tools and
// tool_reference results) for the given upstream model. Anthropic's own origin
// qualifies; a custom base URL qualifies only when native-tool-search is
// enabled for it. Models that predate tool search never qualify, and nothing
// qualifies while upstream.claude.tool-search is off. That switch is read from
// the translator so the executor and the translator always agree on it.
func (e *ClaudeExecutor) SupportsNativeToolSearch(auth *cliproxyauth.Auth, upstreamModel string) bool {
	return e.supportsNativeToolSearch(responsesclaude.ClientToolSearchEnabled(), auth, upstreamModel)
}

func (e *ClaudeExecutor) supportsNativeToolSearch(toolSearch bool, auth *cliproxyauth.Auth, upstreamModel string) bool {
	if !toolSearch || helps.ClaudeModelLacksToolSearch(upstreamModel) {
		return false
	}
	_, baseURL := claudeCreds(auth)
	if baseURL = strings.TrimSpace(baseURL); baseURL == "" || isAnthropicUpstreamBase(baseURL) {
		return true
	}
	if auth != nil && auth.Attributes != nil && strings.EqualFold(strings.TrimSpace(auth.Attributes["native_tool_search"]), "true") {
		return true
	}
	entry := resolveClaudeKeyConfig(e.cfg, auth)
	return entry != nil && entry.NativeToolSearch
}

// applyNativeToolSearchFallback removes Codex tool_search from a Responses
// request before translation when the credential cannot use native tool
// search, so the translated body matches the pre-tool-search translation.
// While upstream.claude.tool-search is off the translator already produces
// that translation from the unmodified request, so nothing is removed.
//
// The returned context pins upstream.claude.tool-search for the rest of the
// request, so this decision and the request and response translations agree
// even if the configuration is reloaded mid-request.
func (e *ClaudeExecutor) applyNativeToolSearchFallback(ctx context.Context, auth *cliproxyauth.Auth, from sdktranslator.Format, upstreamModel string, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (context.Context, cliproxyexecutor.Request, cliproxyexecutor.Options) {
	if from != sdktranslator.FormatOpenAIResponse {
		return ctx, req, opts
	}
	toolSearch := responsesclaude.ClientToolSearchEnabledFor(ctx)
	ctx = responsesclaude.WithClientToolSearch(ctx, toolSearch)
	if !toolSearch {
		return ctx, req, opts
	}
	if !helps.ResponsesHasToolSearch(req.Payload) && !helps.ResponsesHasToolSearch(opts.OriginalRequest) {
		return ctx, req, opts
	}
	if e.supportsNativeToolSearch(toolSearch, auth, upstreamModel) {
		return ctx, req, opts
	}
	req.Payload = helps.StripResponsesToolSearch(req.Payload)
	if len(opts.OriginalRequest) > 0 {
		opts.OriginalRequest = helps.StripResponsesToolSearch(opts.OriginalRequest)
	}
	log.Debugf("claude executor: native tool search unavailable, removed tool_search from request (model=%s)", upstreamModel)
	return ctx, req, opts
}

func (e *ClaudeExecutor) modelLevelCooling() bool {
	return e != nil && e.cfg != nil && e.cfg.Claude.ModelLevelCooling
}

func (e *ClaudeExecutor) upstreamRequestLogProvider() string {
	if provider := strings.TrimSpace(e.requestLogProvider); provider != "" {
		return provider
	}
	return e.Identifier()
}

func (e *ClaudeExecutor) upstreamModel(baseModel string) string {
	if e.upstreamModelNormalizer != nil {
		return e.upstreamModelNormalizer(baseModel)
	}
	return baseModel
}

func (e *ClaudeExecutor) restoreResponseModel(payload []byte, model string) []byte {
	if e.upstreamModelNormalizer == nil || strings.TrimSpace(model) == "" {
		return payload
	}
	return restoreClaudeResponseModel(payload, model)
}

func restoreClaudeResponseModel(payload []byte, model string) []byte {
	if updated, changed := setClaudeResponseModel(payload, model); changed {
		return updated
	}

	trimmed := bytes.TrimSpace(payload)
	if !bytes.HasPrefix(trimmed, []byte("data:")) {
		return payload
	}
	dataIndex := bytes.Index(payload, []byte("data:"))
	if dataIndex < 0 {
		return payload
	}
	rawJSON := bytes.TrimSpace(payload[dataIndex+len("data:"):])
	updated, changed := setClaudeResponseModel(rawJSON, model)
	if !changed {
		return payload
	}
	rebuilt := make([]byte, 0, dataIndex+len("data: ")+len(updated))
	rebuilt = append(rebuilt, payload[:dataIndex]...)
	rebuilt = append(rebuilt, []byte("data: ")...)
	rebuilt = append(rebuilt, updated...)
	return rebuilt
}

func setClaudeResponseModel(payload []byte, model string) ([]byte, bool) {
	if !gjson.ValidBytes(payload) {
		return payload, false
	}
	updated := payload
	changed := false
	for _, path := range []string{"model", "message.model"} {
		if !gjson.GetBytes(updated, path).Exists() {
			continue
		}
		next, errSet := sjson.SetBytes(updated, path, model)
		if errSet != nil {
			continue
		}
		updated = next
		changed = true
	}
	return updated, changed
}

func claudeUpstreamTransportError(err error) error {
	if err == nil {
		return nil
	}
	// 只保留已验证的内部 TLS 截止时间分类，丢弃原始 URL/代理/账号信息。
	if cliproxyexecutor.IsConfiguredTLSHandshakeTimeoutError(err) {
		return &claudeSafeConfiguredTLSHandshakeTimeoutError{}
	}
	return statusErr{code: http.StatusBadGateway, msg: claudeClientSafeTransportErrorMessage}
}

type claudeSafeConfiguredTLSHandshakeTimeoutError struct{}

func (*claudeSafeConfiguredTLSHandshakeTimeoutError) Error() string {
	return claudeClientSafeTransportErrorMessage
}
func (*claudeSafeConfiguredTLSHandshakeTimeoutError) StatusCode() int { return 0 }
func (*claudeSafeConfiguredTLSHandshakeTimeoutError) Unwrap() error {
	return cliproxyexecutor.NewConfiguredTLSHandshakeTimeoutError(context.DeadlineExceeded)
}

func newClaudeStatusErr(statusCode int, body []byte, headers http.Header, now time.Time) statusErr {
	err := statusErr{code: statusCode, msg: string(body)}
	if retryAfter := parseUsageLimitRetryAfter(statusCode, body, headers, now); retryAfter != nil {
		err.retryAfter = retryAfter
	}
	return err
}

// PrepareRequest injects Claude credentials into the outgoing HTTP request.
func (e *ClaudeExecutor) PrepareRequest(req *http.Request, auth *cliproxyauth.Auth) error {
	if req == nil {
		return nil
	}
	apiKey, _ := claudeCreds(auth)
	if strings.TrimSpace(apiKey) != "" {
		deviceProfile := helps.ResolveClaudeDeviceProfile(auth, apiKey, req.Header, e.cfg)
		if helps.ClaudeDeviceProfileStabilizationEnabled(e.cfg) {
			helps.ApplyClaudeDeviceProfileHeaders(req, deviceProfile)
			// Fold the applied device-profile high-water User-Agent suffix entrypoint
			// (sdk-cli -> cli, gated by config.NormalizeSdkCliEntrypointEnabled) so this
			// background quota/oauth token-endpoint egress — the GET /api/oauth/profile
			// and /api/oauth/usage snapshot lookups reaching here via
			// quota_snapshots.go fetchQuotaJSON -> exec.HttpRequest — presents the same
			// "(external, cli)" suffix as the serving path
			// (helps.AlignClaudeDeviceProfileUserAgentSuffix) and the refresh/reauth
			// token-endpoint paths (helps.NormalizeClaudeUserAgentEntrypoint). Unlike
			// serving there is no inbound client UA to mirror here (this is a sessionless
			// background call; ginHeaders is nil below), so the frozen high-water suffix
			// — which a single `claude --print` can seed to "sdk-cli" — is folded in
			// place, exactly as the refresh/reauth high-water is folded before injection.
			// Only the parenthetical entrypoint field is rewritten; the high-water
			// "claude-cli/<version>" prefix is preserved. The fold is a no-op when the
			// gate is disabled, the suffix entrypoint is not sdk-cli, or the applied UA
			// has no parenthetical block.
			if ua := req.Header.Get("User-Agent"); ua != "" {
				if folded := helps.NormalizeClaudeUserAgentEntrypoint(e.cfg, ua); folded != ua {
					req.Header.Set("User-Agent", folded)
				}
			}
		}
		// claude 版本高水位持久化：ResolveClaudeDeviceProfile 已把本次合法候选记入内存观测
		// （伪造超高版本在 sanity-ceiling gate 已被丢弃，不会进入观测）。这里把当前账号的
		// 观测高水位三元组透出给 auth manager，由 RaiseClaudeDeviceHighWater 做"仅单调抬升才
		// 写盘"。无 manager 时降级为 no-op，不破坏 NewClaudeExecutor 调用方。
		e.persistClaudeDeviceHighWater(auth, apiKey)
	}
	useAPIKey := auth != nil && (auth.AuthKind() == cliproxyauth.AuthKindAPIKey || (auth.Attributes != nil && strings.TrimSpace(auth.Attributes["api_key"]) != ""))
	isAnthropicBase := isAnthropicUpstreamURL(req.URL)
	if strings.TrimSpace(apiKey) != "" {
		if isAnthropicBase && useAPIKey {
			req.Header.Del("Authorization")
			req.Header.Set("x-api-key", apiKey)
		} else {
			req.Header.Del("x-api-key")
			req.Header.Set("Authorization", "Bearer "+apiKey)
		}
	} else {
		req.Header.Del("Authorization")
		req.Header.Del("x-api-key")
	}
	// Fill in the rest of the managed Anthropic/stainless protocol header set that
	// real serving (applyClaudeHeaders) always sends, so this adapter's callers —
	// currently only the background quota/oauth snapshot lookups in
	// quota_snapshots.go (GET /api/oauth/profile, /api/oauth/usage via
	// exec.HttpRequest) — stop egressing a half-managed subset (previously just
	// the 5 device-profile headers above) that stands out as a distinguishable
	// fingerprint gap versus real claude-cli. Gate on isAnthropicBase: this method
	// also implements the generic RequestPreparer hook reachable via
	// Manager.InjectCredentials/PrepareHttpRequest for arbitrary requests an SDK
	// embedder may build, so only inject Anthropic-specific protocol headers when
	// the request actually targets api.anthropic.com, never a non-Anthropic
	// base_url/proxy target. ginHeaders is intentionally nil here (unlike real
	// serving, which sources overrides from the inbound client request): this is
	// a sessionless background call with no legitimate inbound client headers to
	// inherit from. includeSessionID=false for the same reason — quota/oauth is a
	// backend lookup with no client session context, and attaching a session id
	// would itself become a new cross-account correlation anchor.
	if isAnthropicBase {
		applyClaudeManagedProtocolHeaders(req, nil, e.cfg, apiKey, isAnthropicBase, false)
	}
	managedHeaderSnapshot := captureManagedHeaderSnapshot(req.Header, claudeManagedHeaderNames)
	util.ApplyCustomHeadersFromAttrs(req, authAttrs(auth))
	applyClaudeManagedHeaders(req, auth, managedHeaderSnapshot)
	return nil
}

// persistClaudeDeviceHighWater reads the account's current observed claude client
// device-profile high-water and asks the auth manager to monotonically raise the
// persisted high-water (auth.Metadata[claude_device_high_water]). It is a no-op
// when no manager is wired, when the auth has no ID, or when no real observation
// exists yet. The manager performs the strict-increase comparison, so calling
// this every request is safe and writes to disk only on an actual raise.
func (e *ClaudeExecutor) persistClaudeDeviceHighWater(auth *cliproxyauth.Auth, apiKey string) {
	if e == nil || e.authManager == nil || auth == nil {
		return
	}
	// 反关联 A1：农场号出站版本跟随容器实际版本、不走单调高水位，故不把农场号观测持久化
	// 进 auth.Metadata 高水位——否则重启后会经 SeedClaudeObservedHighWaterFromAuth 回流全局
	// 观测池、把农场容器版本带进普通号 floor。农场号出站版本无需持久化：容器每次入站都带
	// 真实版本，冷启动首帧即可直出。
	if _, farmBound := cliproxyauth.ClaudeDeviceIDSource(auth); farmBound {
		return
	}
	authID := strings.TrimSpace(auth.ID)
	if authID == "" {
		return
	}
	highWater, ok := helps.ClaudeObservedHighWaterForAuth(auth, apiKey)
	if !ok {
		return
	}
	if _, err := e.authManager.RaiseClaudeDeviceHighWater(context.Background(), authID, highWater); err != nil {
		log.WithError(err).WithFields(log.Fields{
			"component": "claude_device_high_water",
			"auth_id":   authID,
		}).Warn("claude executor: failed to persist device-profile high-water")
	}
}

// HttpRequest injects Claude credentials into the request and executes it.
func (e *ClaudeExecutor) HttpRequest(ctx context.Context, auth *cliproxyauth.Auth, req *http.Request) (*http.Response, error) {
	if req == nil {
		return nil, fmt.Errorf("claude executor: request is nil")
	}
	if ctx == nil {
		ctx = req.Context()
	}
	httpReq := req.WithContext(ctx)
	if err := e.PrepareRequest(httpReq, auth); err != nil {
		return nil, err
	}
	ctx = helps.WithRuntimeTransportHostFromRequest(ctx, httpReq)
	httpClient := newProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	return doClaudeHTTPWithTransportRetry(ctx, httpClient, httpReq)
}

// SupportsApplyPatch reports the actual executor contract, independent of its provider name.
func (e *ClaudeExecutor) SupportsApplyPatch() bool { return e != nil }
