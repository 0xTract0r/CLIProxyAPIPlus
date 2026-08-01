package executor

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/thinking"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func (e *ClaudeExecutor) ExecuteStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (_ *cliproxyexecutor.StreamResult, err error) {
	ctx = contextWithClaudeInboundHeaders(ctx, opts.Headers)
	// Attach a cwd-restore collector when normalization is on so the streamed
	// response can restore tool_use path arguments (requirement ⑦, restore half).
	var cwdRestore *helps.CwdRestoreCollector
	if config.NormalizeAccountEnvEnabled(e.cfg) {
		ctx, cwdRestore = helps.ContextWithCwdRestoreCollector(ctx)
	}
	if opts.Alt == "responses/compact" {
		return nil, statusErr{code: http.StatusNotImplemented, msg: "/responses/compact not supported"}
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
	originalPayloadSource := req.Payload
	if len(opts.OriginalRequest) > 0 {
		originalPayloadSource = opts.OriginalRequest
	}
	originalPayload := originalPayloadSource
	originalTranslated := sdktranslator.TranslateRequest(from, to, baseModel, originalPayload, true)
	body := sdktranslator.TranslateRequest(from, to, baseModel, req.Payload, true)
	body, _ = sjson.SetBytes(body, "model", upstreamModel)

	body, err = thinking.ApplyThinking(body, req.Model, from.String(), to.String(), e.Identifier())
	if err != nil {
		return nil, err
	}
	if rebuildMidSystemMessageEnabled(e.cfg, auth) {
		body = rebuildMidSystemMessagesToTopLevel(body)
	}

	// Apply cloaking (system prompt injection, fake user ID, sensitive word obfuscation)
	// based on client type and configuration. billingVersion (the account
	// high-water version V, floored up in the device profile) is resolved once and
	// reused below for the real-path body cc_version alignment so both the outbound
	// UA and the body draw the same V (no re-resolve).
	billingVersion := resolveClaudeBillingVersion(ctx, e.cfg, auth, apiKey)
	body = applyCloaking(ctx, e.cfg, auth, body, baseModel, apiKey, billingVersion)
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
	body = enforceCacheControlLimit(body, 4)

	// Normalize TTL values to prevent ordering violations under prompt-caching-scope-2026-01-05.
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
	// "cli" on the streaming /v1/messages path, mirroring the outbound UA suffix
	// fold. Real claude-cli clients skip cloak system-block regeneration
	// (ShouldCloak=false in the default auto mode), so their inbound billing
	// header would otherwise reach Anthropic verbatim, leaving the outbound UA
	// suffix (cli) and the body cc_entrypoint (sdk-cli) divergent. Runs before
	// signing so the recomputed cch covers the folded body; gated by the same
	// switch (default on).
	bodyForUpstream, entrypointFolded := normalizeClaudeBillingEntrypoint(e.cfg, bodyForUpstream)
	// REAL serving path (ShouldCloak=false, genuine claude-cli) body cc_version
	// floor: align the body billing-header cc_version <version> segment to the same
	// account high-water V the outbound User-Agent is floored to, so a below-
	// high-water client cannot emit UA=V + body cc_version=<lower> (a
	// one-account-two-versions tell). The <build> segment is passed through
	// verbatim. Runs on the final upstream body (after sanitize / entrypoint fold),
	// mirroring normalizeClaudeBillingEntrypoint, so the single re-sign below covers
	// the rewritten body exactly once. No-op on the cloaked path (cc_version is
	// already V there) and when the switch is off (default) — real path then stays
	// byte-identical to today.
	bodyForUpstream, billingVersionAligned := alignRealPathBillingVersion(e.cfg, bodyForUpstream, billingVersion)
	// Enable cch signing by default for OAuth tokens (not just experimental flag).
	// Also re-sign when the entrypoint was folded or the real-path billing version
	// was aligned so the cch stays consistent with the rewritten body even on
	// non-OAuth paths. Exactly one re-sign covers all body mutations.
	if oauthToken || experimentalCCHSigningEnabled(e.cfg, auth) || entrypointFolded || billingVersionAligned {
		bodyForUpstream = signAnthropicMessagesBody(bodyForUpstream)
	}
	reporter.SetTranslatedReasoningEffort(bodyForUpstream, to.String())

	url := fmt.Sprintf("%s/v1/messages?beta=true", baseURL)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(bodyForUpstream))
	if err != nil {
		return nil, err
	}
	applyClaudeHeaders(httpReq, auth, apiKey, true, extraBetas, e.cfg)
	// claude 版本高水位持久化（真实 serving 流式路径）：同 Execute，applyClaudeHeaders 已记入
	// 内存观测，这里把观测高水位透出给 auth manager 做单调抬升写盘。
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
		return nil, claudeUpstreamTransportError(err)
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
			return nil, newClaudeStatusErr(httpResp.StatusCode, []byte(msg), httpResp.Header, time.Now())
		}
		b, readErr := io.ReadAll(errBody)
		if readErr != nil {
			helps.RecordAPIResponseError(ctx, e.cfg, readErr)
			msg := fmt.Sprintf("failed to read error response body: %v", readErr)
			helps.LogWithRequestID(ctx).Warn(msg)
			b = []byte(msg)
		}
		helps.AppendAPIResponseChunk(ctx, e.cfg, b)
		helps.LogWithRequestID(ctx).Debugf("request error, error status: %d, error message: %s", httpResp.StatusCode, helps.SummarizeErrorBody(httpResp.Header.Get("Content-Type"), b))
		if errClose := errBody.Close(); errClose != nil {
			log.Errorf("response body close error: %v", errClose)
		}
		err = newClaudeStatusErr(httpResp.StatusCode, b, httpResp.Header, time.Now())
		return nil, err
	}
	decodedBody, err := decodeResponseBody(httpResp.Body, httpResp.Header.Get("Content-Encoding"))
	if err != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, err)
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("response body close error: %v", errClose)
		}
		return nil, err
	}
	out := make(chan cliproxyexecutor.StreamChunk)
	go func() {
		defer close(out)
		defer func() {
			if errClose := decodedBody.Close(); errClose != nil {
				log.Errorf("response body close error: %v", errClose)
			}
		}()

		// If from == to (Claude → Claude), directly forward the SSE stream without translation
		if from == to {
			invokeRepairer := newClaudeInvokeRepairer(ginHeadersFromContext(ctx), bodyForTranslation)
			// cwd restorer runs BEFORE the invoke repairer so native tool_use blocks
			// (input_json_delta) get their fake→real path arguments restored; the
			// repairer then passes those restored frames through untouched. nil when
			// nothing was captured (transparent pass-through).
			cwdRestorer := newClaudeCwdStreamRestorer(cwdRestore.Pairs())
			emitRepaired := func(line []byte) bool {
				for _, chunk := range invokeRepairer.ProcessLine(line) {
					select {
					case out <- cliproxyexecutor.StreamChunk{Payload: chunk}:
					case <-ctx.Done():
						return false
					}
				}
				return true
			}
			scanner := bufio.NewScanner(decodedBody)
			scanner.Buffer(nil, 52_428_800) // 50MB
			for scanner.Scan() {
				line := scanner.Bytes()
				helps.AppendAPIResponseChunk(ctx, e.cfg, line)
				if detail, ok := helps.ParseClaudeStreamUsage(line); ok {
					reporter.Publish(ctx, detail)
				}
				line = restoreClaudeOAuthToolNamesFromStreamLine(line, claudeToolPrefix, auth.ToolPrefixDisabled(), oauthToolNamesReverseMap)
				line = e.restoreResponseModel(line, req.Model)
				for _, chunk := range cwdRestorer.ProcessLine(line) {
					for _, sub := range claudeChunkLines(chunk) {
						if !emitRepaired(sub) {
							return
						}
					}
				}
			}
			for _, chunk := range cwdRestorer.Flush() {
				for _, sub := range claudeChunkLines(chunk) {
					if !emitRepaired(sub) {
						return
					}
				}
			}
			for _, chunk := range invokeRepairer.Flush() {
				select {
				case out <- cliproxyexecutor.StreamChunk{Payload: chunk}:
				case <-ctx.Done():
					return
				}
			}
			if errScan := scanner.Err(); errScan != nil {
				helps.RecordAPIResponseError(ctx, e.cfg, errScan)
				reporter.PublishFailure(ctx, errScan)
				select {
				case out <- cliproxyexecutor.StreamChunk{Err: errScan}:
				case <-ctx.Done():
				}
			}
			return
		}

		// For other formats, use translation
		// cwd restorer runs on the raw upstream Anthropic lines BEFORE translation
		// so tool_use path arguments are restored while still in Anthropic shape
		// (the translator never needs to know about cwd restoration). nil when
		// nothing was captured.
		cwdRestorer := newClaudeCwdStreamRestorer(cwdRestore.Pairs())
		scanner := bufio.NewScanner(decodedBody)
		scanner.Buffer(nil, 52_428_800) // 50MB
		var param any
		translateAndEmit := func(line []byte) bool {
			chunks := sdktranslator.TranslateStream(
				ctx,
				to,
				from,
				req.Model,
				opts.OriginalRequest,
				bodyForTranslation,
				bytes.Clone(line),
				&param,
			)
			for i := range chunks {
				select {
				case out <- cliproxyexecutor.StreamChunk{Payload: chunks[i]}:
				case <-ctx.Done():
					return false
				}
			}
			return true
		}
		for scanner.Scan() {
			line := scanner.Bytes()
			helps.AppendAPIResponseChunk(ctx, e.cfg, line)
			if detail, ok := helps.ParseClaudeStreamUsage(line); ok {
				reporter.Publish(ctx, detail)
			}
			line = restoreClaudeOAuthToolNamesFromStreamLine(line, claudeToolPrefix, auth.ToolPrefixDisabled(), oauthToolNamesReverseMap)
			line = e.restoreResponseModel(line, req.Model)
			for _, chunk := range cwdRestorer.ProcessLine(line) {
				for _, sub := range claudeChunkLines(chunk) {
					if !translateAndEmit(sub) {
						return
					}
				}
			}
		}
		for _, chunk := range cwdRestorer.Flush() {
			for _, sub := range claudeChunkLines(chunk) {
				if !translateAndEmit(sub) {
					return
				}
			}
		}
		if errScan := scanner.Err(); errScan != nil {
			helps.RecordAPIResponseError(ctx, e.cfg, errScan)
			reporter.PublishFailure(ctx, errScan)
			select {
			case out <- cliproxyexecutor.StreamChunk{Err: errScan}:
			case <-ctx.Done():
			}
		}
	}()
	return &cliproxyexecutor.StreamResult{Headers: httpResp.Header.Clone(), Chunks: out}, nil
}

func validateClaudeStreamingResponse(data []byte) error {
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(nil, 52_428_800)

	hasData := false
	hasMessageStart := false
	hasMessageDelta := false

	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 || !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		payload := bytes.TrimSpace(line[len("data:"):])
		if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
			continue
		}
		hasData = true
		if !gjson.ValidBytes(payload) {
			return statusErr{code: http.StatusBadGateway, msg: "claude executor: upstream returned malformed stream data"}
		}

		root := gjson.ParseBytes(payload)
		switch root.Get("type").String() {
		case "error":
			message := strings.TrimSpace(root.Get("error.message").String())
			if message == "" {
				message = strings.TrimSpace(root.Get("error.type").String())
			}
			if message == "" {
				message = "unknown upstream error"
			}
			return statusErr{code: http.StatusBadGateway, msg: "claude executor: upstream returned error event: " + message}
		case "message_start":
			message := root.Get("message")
			if strings.TrimSpace(message.Get("id").String()) == "" || strings.TrimSpace(message.Get("model").String()) == "" {
				return statusErr{code: http.StatusBadGateway, msg: "claude executor: upstream stream message_start is missing id or model"}
			}
			hasMessageStart = true
		case "message_delta":
			hasMessageDelta = true
		}
	}
	if errScan := scanner.Err(); errScan != nil {
		return errScan
	}
	if !hasData {
		return statusErr{code: http.StatusBadGateway, msg: "claude executor: upstream returned empty stream response"}
	}
	if !hasMessageStart {
		return statusErr{code: http.StatusBadGateway, msg: "claude executor: upstream stream response is missing message_start"}
	}
	if !hasMessageDelta {
		return statusErr{code: http.StatusBadGateway, msg: "claude executor: upstream stream response ended before message completion"}
	}
	return nil
}
