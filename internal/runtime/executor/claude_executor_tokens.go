package executor

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/thinking"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func (e *ClaudeExecutor) CountTokens(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	ctx = contextWithClaudeInboundHeaders(ctx, opts.Headers)
	apiKey, baseURL := claudeCreds(auth)
	// A configured upstream keeps the fork's count endpoint behavior. Local
	// estimation is explicit; unauthenticated SDK calls may still count locally.
	localCount := e.cfg != nil && e.cfg.Claude.LocalTokenCount
	if !localCount && (strings.TrimSpace(apiKey) != "" || strings.TrimSpace(baseURL) != "") {
		return e.countTokensUpstream(ctx, auth, req, opts)
	}

	baseModel := thinking.ParseSuffix(req.Model).ModelName
	from := opts.SourceFormat
	ctx, req, opts = e.applyNativeToolSearchFallback(ctx, auth, from, e.upstreamModel(baseModel), req, opts)
	responseFormat := cliproxyexecutor.ResponseFormatOrSource(opts)
	to := sdktranslator.FromString("claude")

	// Use streaming translation to preserve function calling, except for claude.
	stream := from != to
	body, err := helps.TranslateRequestReturningError(ctx, opts.Headers, e.cfg, from, to, baseModel, req.Payload, stream, helps.APIKeyModelIsCompat(req))
	if err != nil {
		return cliproxyexecutor.Response{}, err
	}
	originalTranslatedForPayload := append([]byte(nil), body...)
	if len(opts.OriginalRequest) > 0 {
		originalTranslatedForPayload = helps.TranslateRequestWithAPIKeyModelCompatibility(ctx, opts.Headers, e.cfg, from, to, baseModel, opts.OriginalRequest, stream, helps.APIKeyModelIsCompat(req))
	}
	var errThinking error
	body, errThinking = helps.ApplyRequestThinking(body, req, opts, from.String(), to.String(), e.Identifier())
	if errThinking != nil {
		return cliproxyexecutor.Response{}, errThinking
	}
	originalPayload := req.Payload
	if len(opts.OriginalRequest) > 0 {
		originalPayload = opts.OriginalRequest
	}
	webSearchSessionKey, webSearchTTL, _ := e.claudeWebSearchResultCacheScope(ctx, auth, from, req, opts, originalPayload)
	body = helps.RestoreClaudeWebSearchResultsContext(ctx, body, webSearchSessionKey, webSearchTTL)
	if rebuildMidSystemMessageEnabled(e.cfg, auth) {
		body = rebuildMidSystemMessagesToTopLevel(body)
	}
	body = sanitizeClaudeMessagesForClaudeUpstreamWithDebug(ctx, body, baseModel, helps.APIKeyModelIsCompat(req))
	body = applyClaudePreservedThinkingBinding(body)
	body = helps.NewPayloadFinalizer(e.cfg, e.Identifier(), baseModel, to.String(), "", originalTranslatedForPayload, req, opts)(body)
	body = stripPromptCacheOptions(body)
	if errValidate := validateClaudeTokenCountRequest(body); errValidate != nil {
		return cliproxyexecutor.Response{}, errValidate
	}

	// Custom API-key gateways without a native count_tokens contract continue to
	// use the local estimator without injecting generation-only CLI instructions.
	count, err := helps.CountClaudeInputTokens(body)
	if err != nil {
		return cliproxyexecutor.Response{}, fmt.Errorf("claude executor: token counting failed: %w", err)
	}

	usageJSON := []byte(fmt.Sprintf(`{"input_tokens":%d}`, count))
	out := sdktranslator.TranslateTokenCount(ctx, to, responseFormat, count, usageJSON)
	return cliproxyexecutor.Response{Payload: out}, nil
}

type claudeTokenCountValidationError struct {
	statusErr
}

func (claudeTokenCountValidationError) IsRequestScoped() bool {
	return true
}

func newClaudeTokenCountValidationError(message string) error {
	return claudeTokenCountValidationError{statusErr{code: http.StatusBadRequest, msg: message}}
}

func validateClaudeTokenCountRequest(body []byte) error {
	if !gjson.ValidBytes(body) {
		return newClaudeTokenCountValidationError("invalid Claude token count request JSON")
	}
	root := gjson.ParseBytes(body)
	if !root.IsObject() {
		return newClaudeTokenCountValidationError("Claude token count request must be a JSON object")
	}
	messages := root.Get("messages")
	if !messages.IsArray() || len(messages.Array()) == 0 {
		return newClaudeTokenCountValidationError("Claude token count request messages must be a non-empty array")
	}
	for _, message := range messages.Array() {
		if !message.IsObject() {
			return newClaudeTokenCountValidationError("Claude token count request messages must contain objects")
		}
		role := message.Get("role").String()
		if role != "user" && role != "assistant" {
			return newClaudeTokenCountValidationError("Claude token count request message role must be user or assistant")
		}
		content := message.Get("content")
		if content.Type == gjson.String {
			continue
		}
		if !content.IsArray() {
			return newClaudeTokenCountValidationError("Claude token count request message content must be a string or array")
		}
		for _, block := range content.Array() {
			if !block.IsObject() || block.Get("type").Type != gjson.String || block.Get("type").String() == "" {
				return newClaudeTokenCountValidationError("Claude token count request content blocks must be typed objects")
			}
		}
	}
	return nil
}

// claudeLegacyCountPayloadCCH复现F非OAuth摘要的preimage：caller system转发后、
// 注入billing/system模板前的payload。仅供custom Count旧SHA兼容，不是最终正文签名。
func claudeLegacyCountPayloadCCH(payload []byte, strictMode bool) string {
	if !strictMode {
		var parts []string
		system := gjson.GetBytes(payload, "system")
		if system.IsArray() {
			for _, part := range system.Array() {
				if part.Get("type").String() == "text" {
					if text := strings.TrimSpace(part.Get("text").String()); text != "" {
						parts = append(parts, text)
					}
				}
			}
		} else if system.Type == gjson.String && strings.TrimSpace(system.String()) != "" {
			parts = append(parts, strings.TrimSpace(system.String()))
		}
		if len(parts) > 0 {
			payload = prependToFirstUserMessage(payload, strings.Join(parts, "\n\n"))
		}
	}
	sum := sha256.Sum256(payload)
	return fmt.Sprintf("%x", sum[:])[:5]
}

func shouldUseClaudeUpstreamTokenCount(apiKey, baseURL string) bool {
	return strings.TrimSpace(apiKey) != "" && isAnthropicUpstreamBase(baseURL)
}

// countTokensUpstream preserves Anthropic's native token-counting contract.
func (e *ClaudeExecutor) countTokensUpstream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	baseModel := thinking.ParseSuffix(req.Model).ModelName
	upstreamModel := e.upstreamModel(baseModel)

	apiKey, baseURL := claudeCreds(auth)
	baseURL = strings.TrimSpace(baseURL)
	if baseURL == "" {
		baseURL = "https://api.anthropic.com"
	}
	url := fmt.Sprintf("%s/v1/messages/count_tokens?beta=true", baseURL)
	fp := resolveClaudeFingerprintPolicy(e.cfg, auth, apiKey)

	from := opts.SourceFormat
	ctx, req, opts = e.applyNativeToolSearchFallback(ctx, auth, from, upstreamModel, req, opts)
	responseFormat := cliproxyexecutor.ResponseFormatOrSource(opts)
	to := sdktranslator.FromString("claude")
	originalPayload := req.Payload
	if len(opts.OriginalRequest) > 0 {
		originalPayload = opts.OriginalRequest
	}
	incomingHeaders, claudeCodeDetection := detectIncomingClaudeCodeRequest(ctx, opts.Headers, originalPayload, true, e.cfg)
	initialPolicy, _ := resolveClaudeWirePolicy(e.cfg, auth, apiKey, claudeCodeDetection.Confirmed, helps.HeaderValueCaseInsensitive(incomingHeaders, "User-Agent"))
	confirmedClaudeCode := isEffectiveConfirmedClaudeCode(claudeCodeDetection, initialPolicy)
	claudeSessionID := ""
	if fp.ProfileClaudeCodeCLI {
		claudeSessionID = helps.ClaudeAgentSessionUUIDForRequest(incomingHeaders, originalPayload, req.Payload, confirmedClaudeCode, opts.Metadata, req.Metadata)
	}
	// Use streaming translation to preserve function calling, except for claude.
	stream := from != to
	body, err := helps.TranslateRequestReturningError(ctx, opts.Headers, e.cfg, from, to, baseModel, req.Payload, stream, helps.APIKeyModelIsCompat(req))
	if err != nil {
		return cliproxyexecutor.Response{}, err
	}
	originalTranslatedForPayload := append([]byte(nil), body...)
	if len(opts.OriginalRequest) > 0 {
		originalTranslatedForPayload = helps.TranslateRequestWithAPIKeyModelCompatibility(ctx, opts.Headers, e.cfg, from, to, baseModel, opts.OriginalRequest, stream, helps.APIKeyModelIsCompat(req))
	}
	body = helps.SetStringIfDifferent(body, "model", upstreamModel)
	var errThinking error
	body, errThinking = helps.ApplyRequestThinking(body, req, opts, from.String(), to.String(), e.Identifier())
	if errThinking != nil {
		return cliproxyexecutor.Response{}, errThinking
	}
	webSearchSessionKey, webSearchTTL, _ := e.claudeWebSearchResultCacheScope(ctx, auth, from, req, opts, originalPayload)
	body = helps.RestoreClaudeWebSearchResultsContext(ctx, body, webSearchSessionKey, webSearchTTL)
	if rebuildMidSystemMessageEnabled(e.cfg, auth) {
		body = rebuildMidSystemMessagesToTopLevel(body)
	}

	directAnthropic := isAnthropicUpstreamBase(baseURL)
	// 官方/显式原生profile与其他provider沿用native Count形状；普通Claude gateway保fork职责。
	customClaudeCount := !directAnthropic && (auth == nil || strings.TrimSpace(auth.Provider) == "" || strings.EqualFold(strings.TrimSpace(auth.Provider), "claude")) &&
		claudeFingerprintProfileFromConfig(e.cfg, auth) == claudeFingerprintProfileDefault
	policy, settings := resolveClaudeWirePolicy(e.cfg, auth, apiKey, confirmedClaudeCode, helps.HeaderValueCaseInsensitive(incomingHeaders, "User-Agent"))
	cloaked := policy.Cloak
	explicitCacheMode := isExplicitPromptCacheMode(originalPayload, req.Payload, body)
	useCCHSigning := customClaudeCount && (policy.OAuth || experimentalCCHSigningEnabled(e.cfg, auth))
	if customClaudeCount {
		legacyCCH := ""
		var legacyCCHOutput *string
		if cloaked && !useCCHSigning && !strings.HasPrefix(baseModel, "claude-3-5-haiku") {
			legacyCCHOutput = &legacyCCH
		}
		ctx = helps.WithIncomingHeaders(ctx, incomingHeaders)
		body, cloaked, err = applyCloakingWithDeviceIdentityPolicy(ctx, e.cfg, auth, body, apiKey, confirmedClaudeCode, useCCHSigning, true, false, legacyCCHOutput, explicitCacheMode)
		if err != nil {
			return cliproxyexecutor.Response{}, err
		}
		if legacyCCH != "" {
			billing := gjson.GetBytes(body, "system.0.text").String()
			if strings.HasPrefix(billing, "x-anthropic-billing-header:") {
				billing = claudeBillingHeaderEntrypointPattern.ReplaceAllStringFunc(billing, func(entrypoint string) string { return entrypoint + " cch=" + legacyCCH + ";" })
				body, _ = sjson.SetBytes(body, "system.0.text", billing)
			}
		}
	} else {
		if cloaked {
			if !settings.strictMode {
				if errSystem := validateClaudeCallerSystemBlocks(gjson.GetBytes(body, "system")); errSystem != nil {
					return cliproxyexecutor.Response{}, errSystem
				}
			}
			if !strings.HasPrefix(baseModel, "claude-3-5-haiku") {
				if policy.OAuth && !settings.strictMode {
					body = neutralizeClaudeOAuthCallerSystem(body)
				}
				body = relocateClaudeSystemPromptForCountTokensWithPolicy(body, settings.strictMode, explicitCacheMode, !policy.OAuth)
			}
			if len(settings.sensitiveWords) > 0 {
				body = helps.ObfuscateSensitiveWords(body, helps.BuildSensitiveWordMatcher(settings.sensitiveWords))
			}
		} else if policy.Uncloak {
			body = uncloakClaudeCodePayload(body, confirmedClaudeCode)
		}
	}
	// Keep count_tokens requests compatible with Anthropic cache-control constraints too.
	body = enforceCacheControlLimit(body, 4)
	if !explicitCacheMode {
		body = normalizeCacheControlTTL(body)
	}

	// Extract betas from body and convert to header (for count_tokens too)
	var extraBetas []string
	extraBetas, body = extractAndRemoveBetas(body)
	// Claude Code 2.1.220's beta.messages.countTokens() always appends this beta.
	extraBetas = append(extraBetas, claudeTokenCountingBeta)
	if fp.MCPAlias && cloaked {
		mcpAliases := resolveClaudeMCPAliasOptions(ctx)
		body, _ = prepareClaudeOAuthToolNamesForUpstream(body, mcpAliases)
	}
	body = sanitizeClaudeMessagesForClaudeUpstreamWithDebug(ctx, body, baseModel, helps.APIKeyModelIsCompat(req))
	body = applyClaudePreservedThinkingBinding(body)
	body = stripPromptCacheOptions(body)
	// Two different reasons converge on the same deletions, and they must stay
	// separable.
	//
	// Match the fixed upstream's first-party native count profile. This is a
	// client wire-shape choice, not a claim that Anthropic rejects system prompts;
	// the public count API supports system. Caller semantics were handled above.
	//
	// Elsewhere (Kimi, delegated Anthropic Messages providers) the caller owns its
	// body by default: a caller that deliberately sends context_management expects
	// the token count to reflect it, so CPA must not silently rewrite the request.
	// Only an explicit claude-code-cli profile aligns the shape, and then it aligns
	// to the measured one: Claude Code 2.1.220 count_tokens carries exactly model,
	// messages and tools, never a system block.
	alignCLICountTokensShape := fp.ProfileClaudeCodeCLI && !customClaudeCount
	if directAnthropic || alignCLICountTokensShape {
		body, _ = sjson.DeleteBytes(body, "metadata")
		body, _ = sjson.DeleteBytes(body, "context_management")
		body, _ = sjson.DeleteBytes(body, "diagnostics")
	}
	if alignCLICountTokensShape {
		body = util.StripClaudeCodeAttributionSystem(body)
	}
	// Runs on the finished body: payload rules can rewrite model and messages
	// long after translation, so an earlier check would not describe the request
	// that is about to be sent.
	body = helps.NewPayloadFinalizer(e.cfg, e.Identifier(), baseModel, to.String(), "", originalTranslatedForPayload, req, opts)(body)
	body = stripPromptCacheOptions(body)
	if errMidSystem := validateClaudeMidSystemMessageModel(body, confirmedClaudeCode, directAnthropic); errMidSystem != nil {
		return cliproxyexecutor.Response{}, errMidSystem
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return cliproxyexecutor.Response{}, err
	}
	if errHeaders := applyClaudeHeaders(httpReq, auth, apiKey, false, extraBetas, body, e.cfg, incomingHeaders, confirmedClaudeCode && !cloaked, claudeSessionID); errHeaders != nil {
		return cliproxyexecutor.Response{}, errHeaders
	}
	// 最后payload规则与出站UA确定后签名；request Body/GetBody/长度及日志共用最终字节。
	if useCCHSigning {
		body, err = signClaudeHTTPRequestCCH(httpReq, body)
		if err != nil {
			return cliproxyexecutor.Response{}, fmt.Errorf("sign Claude count CCH: %w", err)
		}
	}
	e.persistClaudeDeviceHighWater(auth, apiKey)
	var authID, authLabel, authType, authValue string
	if auth != nil {
		authID = auth.ID
		authLabel = auth.Label
		authType, authValue = auth.AccountInfo()
	}
	helps.RecordAPIRequest(ctx, e.cfg, helps.UpstreamRequestLog{
		URL:       url,
		Method:    http.MethodPost,
		Headers:   httpReq.Header.Clone(),
		Body:      body,
		Provider:  e.upstreamRequestLogProvider(),
		AuthID:    authID,
		AuthLabel: authLabel,
		AuthType:  authType,
		AuthValue: authValue,
	})

	ctx = helps.WithRuntimeTransportHostFromRequest(ctx, httpReq)
	httpClient := newProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	resp, err := doClaudeHTTPWithTransportRetry(ctx, httpClient, httpReq)
	if err != nil {
		recordAPIResponseError(ctx, e.cfg, err)
		if cliproxyexecutor.IsHTTPAttemptGateError(err) {
			return cliproxyexecutor.Response{}, err
		}
		return cliproxyexecutor.Response{}, claudeUpstreamTransportError(err)
	}
	attempt := helps.ClaimClaudeAttemptResponse(resp)
	defer attempt.Abandon()
	helps.RecordAPIResponseMetadata(ctx, e.cfg, resp.StatusCode, resp.Header.Clone())
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Decompress error responses — pass the Content-Encoding value (may be empty)
		// and let decodeResponseBody handle both header-declared and magic-byte-detected
		// compression.  This keeps error-path behaviour consistent with the success path.
		errBody, decErr := decodeResponseBody(resp.Body, claudeResponseContentEncoding(resp.Header))
		if decErr != nil {
			helps.RecordAPIResponseError(ctx, e.cfg, decErr)
			msg := fmt.Sprintf("failed to decode error response body: %v", decErr)
			helps.LogWithRequestID(ctx).Warn(msg)
			return cliproxyexecutor.Response{}, classifyClaudeUpstreamErrorWithCooling(resp.StatusCode, resp.Header, []byte(msg), e.modelLevelCooling())
		}
		errBody = attempt.ObserveDecoded(errBody)
		b, readErr := io.ReadAll(errBody)
		if readErr != nil {
			helps.RecordAPIResponseError(ctx, e.cfg, readErr)
			msg := fmt.Sprintf("failed to read error response body: %v", readErr)
			helps.LogWithRequestID(ctx).Warn(msg)
			b = []byte(msg)
		}
		helps.AppendAPIResponseChunk(ctx, e.cfg, b)
		if errClose := errBody.Close(); errClose != nil {
			log.Errorf("response body close error: %v", errClose)
		}
		return cliproxyexecutor.Response{}, classifyClaudeUpstreamErrorWithCooling(resp.StatusCode, resp.Header, b, e.modelLevelCooling())
	}
	decodedBody, err := decodeResponseBody(resp.Body, claudeResponseContentEncoding(resp.Header))
	if err != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, err)
		if errClose := resp.Body.Close(); errClose != nil {
			log.Errorf("response body close error: %v", errClose)
		}
		return cliproxyexecutor.Response{}, err
	}
	decodedBody = attempt.ObserveDecoded(decodedBody)
	defer func() {
		if errClose := decodedBody.Close(); errClose != nil {
			log.Errorf("response body close error: %v", errClose)
		}
	}()
	data, err := io.ReadAll(decodedBody)
	if err != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, err)
		return cliproxyexecutor.Response{}, err
	}
	helps.AppendAPIResponseChunk(ctx, e.cfg, data)
	count := gjson.GetBytes(data, "input_tokens").Int()
	out := sdktranslator.TranslateTokenCount(ctx, to, responseFormat, count, data)
	return cliproxyexecutor.Response{Payload: out, Headers: resp.Header.Clone()}, nil
}
