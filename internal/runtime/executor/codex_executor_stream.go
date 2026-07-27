package executor

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"

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

func (e *CodexExecutor) ExecuteStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (_ *cliproxyexecutor.StreamResult, err error) {
	if opts.Alt == "responses/compact" {
		return nil, statusErr{code: http.StatusBadRequest, msg: "streaming not supported for /responses/compact"}
	}
	if isCodexOpenAIImageRequest(opts) {
		return e.executeOpenAIImageStream(ctx, auth, req, opts)
	}
	// fork(anticorr ⑦-codex): attach a cwd-restore collector when normalization is on
	// so the streamed response can restore fake→real tool-call paths.
	if config.NormalizeAccountEnvEnabled(e.cfg) {
		ctx, _ = helps.ContextWithCwdRestoreCollector(ctx)
	}
	baseModel := thinking.ParseSuffix(req.Model).ModelName

	apiKey, baseURL := codexCreds(auth)
	if baseURL == "" {
		baseURL = "https://chatgpt.com/backend-api/codex"
	}

	reporter := helps.NewExecutorUsageReporter(ctx, e, baseModel, auth)
	defer reporter.TrackFailure(ctx, &err)

	from := opts.SourceFormat
	responseFormat := cliproxyexecutor.ResponseFormatOrSource(opts)
	to := sdktranslator.FromString("codex")
	originalPayloadSource := req.Payload
	if len(opts.OriginalRequest) > 0 {
		originalPayloadSource = opts.OriginalRequest
	}
	originalPayload := originalPayloadSource
	originalTranslated, body := translateCodexRequestPair(from, to, baseModel, originalPayload, req.Payload, true)

	body, err = thinking.ApplyThinking(body, req.Model, from.String(), to.String(), e.Identifier())
	if err != nil {
		return nil, err
	}

	requestedModel := helps.PayloadRequestedModel(opts, req.Model)
	requestPath := helps.PayloadRequestPath(opts)
	body = helps.ApplyPayloadConfigWithRequest(e.cfg, baseModel, to.String(), from.String(), "", body, originalTranslated, requestedModel, requestPath, opts.Headers)
	body, _ = sjson.DeleteBytes(body, "previous_response_id")
	body, _ = sjson.DeleteBytes(body, "generate")
	body, _ = sjson.DeleteBytes(body, "prompt_cache_retention")
	body, _ = sjson.DeleteBytes(body, "safety_identifier")
	body, _ = sjson.DeleteBytes(body, "stream_options")
	body = helps.SetStringIfDifferent(body, "model", baseModel)
	body = normalizeCodexInstructions(body)
	// fork(anticorr ⑦-codex): normalize the real cwd/git/CODEX_HOME paths in the
	// outbound streamed body (capturing the fake→real mapping for response-side restore).
	body = e.normalizeCodexPaths(ctx, body, auth, apiKey)
	// fork: 图像策略统一走 applyImageGenerationPolicy（DisableImageGenerationOff 注入、
	// 其余档位含 nil cfg 剥离 image_generation）。opts.Headers 透传用于 responses-lite 判定。
	body = applyImageGenerationPolicy(e.cfg, body, baseModel, auth, opts.Headers)
	body = sanitizeOpenAIResponsesReasoningEncryptedContent(ctx, "codex executor", body)
	body = normalizeCodexParallelToolCalls(body, opts.Headers)
	body, optimizeMultiAgentV2 := helps.OptimizeCodexMultiAgentV2Request(ctx, opts.Headers, body, e.cfg)
	body, replayScope, errReplay := applyCodexReasoningReplayCacheRequired(ctx, from, req, opts, body)
	if errReplay != nil {
		return nil, errReplay
	}
	reporter.SetTranslatedReasoningEffort(body, to.String())

	url := strings.TrimSuffix(baseURL, "/") + "/responses"
	var identityState codexIdentityConfuseState
	httpReq, upstreamBody, identityState, err := e.cacheHelper(ctx, from, url, auth, req, originalPayloadSource, body, opts.Headers)
	if err != nil {
		return nil, err
	}
	applyCodexHeaders(httpReq, auth, apiKey, true, e.cfg)
	applyModelHeaderOverrides(httpReq.Header, baseModel)
	// fork(anticorr): codex 版本高水位持久化（真实 serving 出站点：ExecuteStream 主对话流）。
	e.persistCodexDeviceHighWater(httpReq.Context(), auth)
	applyCodexIdentityConfuseHeaders(httpReq.Header, &identityState)
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
		Body:      upstreamBody,
		Provider:  e.Identifier(),
		AuthID:    authID,
		AuthLabel: authLabel,
		AuthType:  authType,
		AuthValue: authValue,
	})

	httpClient := helps.NewUtlsHTTPClient(ctx, e.cfg, auth, 0)
	httpClient = reporter.TrackHTTPClient(httpClient)
	httpResp, err := httpClient.Do(httpReq)
	if err != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, err)
		return nil, err
	}
	helps.RecordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		data, readErr := io.ReadAll(httpResp.Body)
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("codex executor: close response body error: %v", errClose)
		}
		if readErr != nil {
			helps.RecordAPIResponseError(ctx, e.cfg, readErr)
			return nil, readErr
		}
		data = applyCodexIdentityConfuseResponsePayload(data, identityState)
		if errClearReplay := clearCodexReasoningReplayOnInvalidSignature(ctx, replayScope, httpResp.StatusCode, data); errClearReplay != nil {
			return nil, errClearReplay
		}
		helps.AppendAPIResponseChunk(ctx, e.cfg, data)
		helps.LogWithRequestID(ctx).Debugf("request error, error status: %d, error message: %s", httpResp.StatusCode, helps.SummarizeErrorBody(httpResp.Header.Get("Content-Type"), data))
		err = newCodexStatusErr(httpResp.StatusCode, data, httpResp.Header)
		return nil, err
	}
	out := make(chan cliproxyexecutor.StreamChunk)
	go func() {
		defer close(out)
		defer func() {
			if errClose := httpResp.Body.Close(); errClose != nil {
				log.Errorf("codex executor: close response body error: %v", errClose)
			}
		}()
		scanner := bufio.NewScanner(httpResp.Body)
		scanner.Buffer(nil, 52_428_800) // 50MB
		claudeInputTokens := helps.NewClaudeInputTokenState(from, to, responseFormat, originalPayload)
		var param any
		outputItemsByIndex := make(map[int64][]byte)
		var outputItemsFallback [][]byte
		// cyberPolicyRecorded 保证同一 ExecuteStream 调用内只对 cyber_policy 计数一次
		// （response 端常见会先发 type=error，紧接着 type=response.failed）。
		var cyberPolicyRecorded bool
		for scanner.Scan() {
			line := applyCodexIdentityConfuseResponsePayload(scanner.Bytes(), identityState)
			helps.AppendAPIResponseChunk(ctx, e.cfg, line)
			translatedLine := bytes.Clone(line)
			terminalSuccess := false

			if bytes.HasPrefix(line, dataTag) {
				data := bytes.TrimSpace(line[5:])
				data = helps.RestoreCodexMultiAgentV2Response(data, optimizeMultiAgentV2)
				translatedLine = append([]byte("data: "), data...)
				eventType := gjson.GetBytes(data, "type").String()
				// fork: cyber_policy 侧信道告警。先于 terminal err 检测，确保即使该事件随后
				// 被当作 terminal error 提前 return，也能记录 cyber_policy 计数（authManager 写回
				// CyberPolicyFlagCount / LastCyberPolicyAt）。
				if (eventType == "error" || eventType == "response.failed") &&
					!cyberPolicyRecorded && cyberPolicyHitFromData(data, eventType) {
					cyberPolicyRecorded = true
					e.recordCyberPolicy(ctx, auth, req.Model)
				}
				if streamErr, terminalBody, ok := codexTerminalFailureErr(data); ok {
					if errClearReplay := clearCodexReasoningReplayOnInvalidSignature(ctx, replayScope, streamErr.StatusCode(), terminalBody); errClearReplay != nil {
						helps.RecordAPIResponseError(ctx, e.cfg, errClearReplay)
						reporter.PublishFailure(ctx, errClearReplay)
						select {
						case out <- cliproxyexecutor.StreamChunk{Err: errClearReplay}:
						case <-ctx.Done():
						}
						return
					}
					helps.RecordAPIResponseError(ctx, e.cfg, streamErr)
					reporter.PublishFailure(ctx, streamErr)
					select {
					case out <- cliproxyexecutor.StreamChunk{Err: streamErr}:
					case <-ctx.Done():
					}
					return
				}
				switch eventType {
				case "response.output_item.done":
					collectCodexOutputItemDone(data, outputItemsByIndex, &outputItemsFallback)
				case "response.completed", "response.incomplete":
					terminalSuccess = true
					if detail, ok := helps.ParseCodexUsage(data); ok {
						reporter.Publish(ctx, detail)
					}
					publishCodexImageToolUsage(ctx, reporter, body, data)
					data = patchCodexCompletedOutput(data, outputItemsByIndex, outputItemsFallback)
					if eventType == "response.completed" {
						cacheCodexReasoningReplayFromCompleted(replayScope, data)
					}
					translatedLine = append([]byte("data: "), data...)
				}
			}

			translatedLine = applyCodexIdentityExposeResponsePayload(translatedLine, identityState)
			// fork(anticorr ⑦-codex): restore fake→real cwd in tool-call arguments per line.
			// The .done/.completed events carry the complete arguments, so the fixed-literal
			// fake root is whole there; per-line restoration applies.
			translatedLine = restoreCodexResponseCwd(ctx, translatedLine)
			chunks := helps.TranslateStreamWithClaudeInputTokens(ctx, to, responseFormat, req.Model, originalPayload, body, translatedLine, &param, claudeInputTokens)
			for i := range chunks {
				select {
				case out <- cliproxyexecutor.StreamChunk{Payload: chunks[i]}:
				case <-ctx.Done():
					return
				}
			}
			if terminalSuccess {
				return
			}
		}
		if errScan := scanner.Err(); errScan != nil {
			if ctx.Err() != nil {
				return
			}
			helps.RecordAPIResponseError(ctx, e.cfg, errScan)
		}
		streamErr := newCodexIncompleteStreamError()
		helps.RecordAPIResponseError(ctx, e.cfg, streamErr)
		reporter.PublishFailure(ctx, streamErr)
		select {
		case out <- cliproxyexecutor.StreamChunk{Err: streamErr}:
		case <-ctx.Done():
		}
	}()
	return &cliproxyexecutor.StreamResult{Headers: httpResp.Header.Clone(), Chunks: out}, nil
}
