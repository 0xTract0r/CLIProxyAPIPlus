package executor

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/thinking"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/sjson"
)

func (e *ClaudeExecutor) Execute(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (resp cliproxyexecutor.Response, err error) {
	ctx = contextWithClaudeInboundHeaders(ctx, opts.Headers)
	// Account cwd normalization (requirement ⑦) is response-restorable: attach a
	// collector so applyCloaking's NormalizeAccountEnvWithRestore can record the
	// fake→real cwd mapping this request applies, and restore it in tool_use path
	// arguments on the response. Only attached when the switch is on.
	var cwdRestore *helps.CwdRestoreCollector
	if config.NormalizeAccountEnvEnabled(e.cfg) {
		ctx, cwdRestore = helps.ContextWithCwdRestoreCollector(ctx)
	}
	if opts.Alt == "responses/compact" {
		return resp, statusErr{code: http.StatusNotImplemented, msg: "/responses/compact not supported"}
	}
	baseModel := thinking.ParseSuffix(req.Model).ModelName
	upstreamModel := e.upstreamModel(baseModel)

	apiKey, baseURL := claudeCreds(auth)
	if baseURL == "" {
		baseURL = "https://api.anthropic.com"
	}

	reporter := helps.NewExecutorUsageReporter(ctx, e, baseModel, auth)
	defer reporter.TrackFailure(ctx, &err)
	from := opts.SourceFormat
	to := sdktranslator.FromString("claude")
	// Use streaming translation to preserve function calling, except for claude.
	stream := from != to
	originalPayloadSource := req.Payload
	if len(opts.OriginalRequest) > 0 {
		originalPayloadSource = opts.OriginalRequest
	}
	originalPayload := originalPayloadSource
	originalTranslated := sdktranslator.TranslateRequest(from, to, baseModel, originalPayload, stream)
	body := sdktranslator.TranslateRequest(from, to, baseModel, req.Payload, stream)
	body, _ = sjson.SetBytes(body, "model", upstreamModel)

	body, err = thinking.ApplyThinking(body, req.Model, from.String(), to.String(), e.Identifier())
	if err != nil {
		return resp, err
	}
	if rebuildMidSystemMessageEnabled(e.cfg, auth) {
		body = rebuildMidSystemMessagesToTopLevel(body)
	}

	// Apply cloaking (system prompt injection, fake user ID, sensitive word obfuscation)
	// based on client type and configuration.
	body = applyCloaking(ctx, e.cfg, auth, body, baseModel, apiKey, resolveClaudeBillingVersion(ctx, e.cfg, auth, apiKey))
	body = ensureModelMaxTokens(body, baseModel)

	requestedModel := helps.PayloadRequestedModel(opts, req.Model)
	requestPath := helps.PayloadRequestPath(opts)
	body = helps.ApplyPayloadConfigWithRequest(e.cfg, baseModel, to.String(), from.String(), "", body, originalTranslated, requestedModel, requestPath, opts.Headers)
	body = ensureModelMaxTokens(body, baseModel)

	// Disable thinking if tool_choice forces tool use (Anthropic API constraint)
	body = disableThinkingIfToolChoiceForced(body)
	body = normalizeClaudeTemperatureForThinking(body)
	// Claude OAuth (and this executor's redact-thinking beta) returns signature-only
	// thinking blocks unless display is set to "summarized".
	body = ensureClaudeThinkingDisplay(body)

	// Auto-inject cache_control if missing (optimization for ClawdBot/clients without caching support)
	if countCacheControls(body) == 0 {
		body = ensureCacheControl(body)
	}

	// Enforce Anthropic's cache_control block limit (max 4 breakpoints per request).
	// Cloaking and ensureCacheControl may push the total over 4 when the client
	// (e.g. Amp CLI) already sends multiple cache_control blocks.
	body = enforceCacheControlLimit(body, 4)

	// Normalize TTL values to prevent ordering violations under prompt-caching-scope-2026-01-05.
	// A 1h-TTL block must not appear after a 5m-TTL block in evaluation order (tools→system→messages).
	body = normalizeCacheControlTTL(body)

	// Extract betas from body and convert to header
	var extraBetas []string
	extraBetas, body = extractAndRemoveBetas(body)
	bodyForTranslation := body
	bodyForUpstream := body
	oauthToken := isClaudeOAuthToken(apiKey)
	var oauthToolNamesReverseMap map[string]string
	if oauthToken {
		bodyForUpstream, oauthToolNamesReverseMap = prepareClaudeOAuthToolNamesForUpstream(bodyForUpstream, claudeToolPrefix, auth.ToolPrefixDisabled())
	}
	bodyForUpstream = sanitizeClaudeMessagesForClaudeUpstreamWithDebug(ctx, bodyForUpstream, baseModel)
	// Fold a self-tagged "sdk-cli" cc_entrypoint in the body billing header into
	// "cli" on the /v1/messages path, mirroring the outbound UA suffix fold. Real
	// claude-cli clients skip cloak system-block regeneration (ShouldCloak=false
	// in the default auto mode), so their inbound billing header would otherwise
	// reach Anthropic verbatim, leaving the outbound UA suffix (cli) and the body
	// cc_entrypoint (sdk-cli) divergent. Runs before signing so the recomputed cch
	// covers the folded body; gated by the same switch (default on).
	bodyForUpstream, entrypointFolded := normalizeClaudeBillingEntrypoint(e.cfg, bodyForUpstream)
	// Enable cch signing by default for OAuth tokens (not just experimental flag).
	// Claude Code always computes cch; missing or invalid cch is a detectable
	// fingerprint. Also re-sign when the entrypoint was folded so the cch stays
	// consistent with the rewritten body even on non-OAuth paths.
	if oauthToken || experimentalCCHSigningEnabled(e.cfg, auth) || entrypointFolded {
		bodyForUpstream = signAnthropicMessagesBody(bodyForUpstream)
	}
	reporter.SetTranslatedReasoningEffort(bodyForUpstream, to.String())

	url := fmt.Sprintf("%s/v1/messages?beta=true", baseURL)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(bodyForUpstream))
	if err != nil {
		return resp, err
	}
	applyClaudeHeaders(httpReq, auth, apiKey, false, extraBetas, e.cfg)
	// claude 版本高水位持久化（真实 serving 路径）：applyClaudeHeaders 内部的
	// resolveClaudeDeviceProfileForRequest -> ResolveClaudeDeviceProfile 已把本次真实
	// 请求的合法高水位候选记入内存观测。这里把当前账号的观测高水位透出给 auth
	// manager，由 RaiseClaudeDeviceHighWater 做"仅单调抬升才写盘"。PrepareRequest 只服务
	// HttpRequest adapter 旁路，真实 /v1/messages 服务走 Execute/ExecuteStream/CountTokens，
	// 因此写回必须挂在这些 serving 方法上，否则持久化永不触发（重启回落到 floor）。
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
		Body:      bodyForUpstream,
		Provider:  e.upstreamRequestLogProvider(),
		AuthID:    authID,
		AuthLabel: authLabel,
		AuthType:  authType,
		AuthValue: authValue,
	})

	ctx = helps.WithRuntimeTransportHostFromRequest(ctx, httpReq)
	httpClient := newProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	httpClient = reporter.TrackHTTPClient(httpClient)
	httpResp, err := doClaudeHTTPWithTransportRetry(ctx, httpClient, httpReq)
	if err != nil {
		recordAPIResponseError(ctx, e.cfg, err)
		return resp, claudeUpstreamTransportError(err)
	}
	helps.RecordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		// Decompress error responses — pass the Content-Encoding value (may be empty)
		// and let decodeResponseBody handle both header-declared and magic-byte-detected
		// compression.  This keeps error-path behaviour consistent with the success path.
		errBody, decErr := decodeResponseBody(httpResp.Body, httpResp.Header.Get("Content-Encoding"))
		if decErr != nil {
			helps.RecordAPIResponseError(ctx, e.cfg, decErr)
			msg := fmt.Sprintf("failed to decode error response body: %v", decErr)
			logWithRequestID(ctx).Warn(msg)
			return resp, newClaudeStatusErr(httpResp.StatusCode, []byte(msg), httpResp.Header, time.Now())
		}
		b, readErr := io.ReadAll(errBody)
		if readErr != nil {
			helps.RecordAPIResponseError(ctx, e.cfg, readErr)
			msg := fmt.Sprintf("failed to read error response body: %v", readErr)
			helps.LogWithRequestID(ctx).Warn(msg)
			b = []byte(msg)
		}
		appendAPIResponseChunk(ctx, e.cfg, b)
		logWithRequestID(ctx).Debugf("request error, error status: %d, error message: %s", httpResp.StatusCode, summarizeErrorBody(httpResp.Header.Get("Content-Type"), b))
		err = newClaudeStatusErr(httpResp.StatusCode, b, httpResp.Header, time.Now())
		if errClose := errBody.Close(); errClose != nil {
			log.Errorf("response body close error: %v", errClose)
		}
		return resp, err
	}
	decodedBody, err := decodeResponseBody(httpResp.Body, httpResp.Header.Get("Content-Encoding"))
	if err != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, err)
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("response body close error: %v", errClose)
		}
		return resp, err
	}
	defer func() {
		if errClose := decodedBody.Close(); errClose != nil {
			log.Errorf("response body close error: %v", errClose)
		}
	}()
	data, err := io.ReadAll(decodedBody)
	if err != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, err)
		return resp, err
	}
	helps.AppendAPIResponseChunk(ctx, e.cfg, data)
	if stream {
		if errValidate := validateClaudeStreamingResponse(data); errValidate != nil {
			helps.RecordAPIResponseError(ctx, e.cfg, errValidate)
			return resp, errValidate
		}
		lines := bytes.Split(data, []byte("\n"))
		for _, line := range lines {
			if detail, ok := helps.ParseClaudeStreamUsage(line); ok {
				reporter.Publish(ctx, detail)
			}
		}
	} else {
		reporter.Publish(ctx, helps.ParseClaudeUsage(data))
	}
	data = restoreClaudeOAuthToolNamesFromResponse(data, claudeToolPrefix, auth.ToolPrefixDisabled(), oauthToolNamesReverseMap)
	data = e.restoreResponseModel(data, req.Model)
	// Restore the fake→real cwd inside tool_use path arguments before translation,
	// the response-side half of account cwd normalization (requirement ⑦). The
	// non-stream JSON form uses the tool_use input walker; a buffered upstream
	// stream blob (from != to) is restored line-by-line. Conversational text is
	// never touched.
	if pairs := cwdRestore.Pairs(); len(pairs) > 0 {
		if stream {
			data = restoreClaudeStreamCwdBlob(pairs, data)
		} else {
			data = helps.RestoreClaudeToolUseCwdInResponse(pairs, data)
		}
	}
	var param any
	out := sdktranslator.TranslateNonStream(
		ctx,
		to,
		from,
		req.Model,
		opts.OriginalRequest,
		bodyForTranslation,
		data,
		&param,
	)
	resp = cliproxyexecutor.Response{Payload: out, Headers: httpResp.Header.Clone()}
	return resp, nil
}
