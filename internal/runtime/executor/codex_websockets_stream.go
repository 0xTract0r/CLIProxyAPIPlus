package executor

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/thinking"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
)

func (e *CodexWebsocketsExecutor) ExecuteStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (result *cliproxyexecutor.StreamResult, err error) {
	log.Debugf("Executing Codex Websockets stream request with auth ID: %s, model: %s", auth.ID, req.Model)
	if ctx == nil {
		ctx = context.Background()
	}
	if opts.Alt == "responses/compact" {
		return nil, statusErr{code: http.StatusBadRequest, msg: "streaming not supported for /responses/compact"}
	}

	baseModel := thinking.ParseSuffix(req.Model).ModelName
	fastEnabled := codexFastEnabled(auth, baseModel)
	apiKey, baseURL := codexCreds(auth)
	if baseURL == "" {
		baseURL = "https://chatgpt.com/backend-api/codex"
	}

	reporter := helps.NewExecutorUsageReporter(ctx, e, baseModel, auth)
	reporter.SetWebsocketTelemetry()
	httpFallbackAllowed, httpFallbackStarted := false, false
	wsBudget, wsMessageBytes, wsPhase := codexFastWebsocketMessageBytes(), 0, "prepare"
	defer func() {
		if !httpFallbackStarted {
			reporter.TrackFailure(ctx, &err)
		}
	}()
	defer func() {
		if httpFallbackAllowed && ctx.Err() == nil && isCodexWebsocketMessageTooBig(err) {
			httpFallbackStarted = true
			logCodexFastTransport(ctx, wsPhase, "http_upstream_1009", wsMessageBytes, wsBudget)
			result, err = e.executeHTTPFallbackStream(ctx, auth, req, opts, true)
		}
	}()

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
	body = helps.SetStringIfDifferent(body, "model", baseModel)
	body = normalizeCodexInstructions(body)
	if e.cfg == nil || e.cfg.DisableImageGeneration == config.DisableImageGenerationOff {
		body = ensureImageGenerationTool(body, baseModel, auth, opts.Headers)
	}
	body = sanitizeOpenAIResponsesReasoningEncryptedContent(ctx, "codex websockets executor", body)
	body = normalizeCodexWebsocketParallelToolCalls(body, opts.Headers)
	body, optimizeMultiAgentV2 := helps.OptimizeCodexMultiAgentV2Request(ctx, opts.Headers, body, e.cfg)
	body, replayScope, errReplay := applyCodexReasoningReplayCacheRequired(ctx, from, req, opts, body)
	if errReplay != nil {
		return nil, errReplay
	}

	httpURL := strings.TrimSuffix(baseURL, "/") + "/responses"
	wsURL, err := buildCodexResponsesWebsocketURL(httpURL)
	if err != nil {
		return nil, err
	}

	body, wsHeaders, errPromptCache := applyCodexPromptCacheHeadersWithContext(ctx, from, req, body, opts.Headers)
	if errPromptCache != nil {
		return nil, errPromptCache
	}
	clientBody := body
	var identityState codexIdentityConfuseState
	upstreamBody, identityState := applyCodexIdentityConfuseBody(e.cfg, auth, originalPayloadSource, body)
	upstreamBody = applyCodexServiceTierPolicy(upstreamBody, fastEnabled)
	reporter.SetCodexFastContext(originalPayloadSource, upstreamBody, fastEnabled)
	reporter.SetTranslatedReasoningEffort(clientBody, to.String())
	wsHeaders = applyCodexWebsocketHeaders(ctx, wsHeaders, auth, apiKey, e.cfg)
	// codex 版本高水位持久化（真实 serving 路径：WS ExecuteStream 出站）。见 WS Execute 注释。
	e.persistCodexDeviceHighWater(ctx, auth)
	applyModelHeaderOverrides(wsHeaders, baseModel)
	applyCodexIdentityConfuseHeaders(wsHeaders, &identityState)
	httpFallbackAllowed = fastEnabled && codexHTTPFallbackAllowed(ctx, req, opts, upstreamBody)
	if httpFallbackAllowed {
		mainBody := buildCodexWebsocketFastMainBody(upstreamBody, "")
		prewarmBody := buildCodexWebsocketPrewarmBody(upstreamBody)
		if len(prewarmBody) > len(mainBody) {
			mainBody = prewarmBody
		}
		if codexFastWebsocketSizeGate(ctx, mainBody, wsBudget, "prepare") {
			httpFallbackStarted = true
			return e.executeHTTPFallbackStream(ctx, auth, req, opts, false)
		}
	}

	var authID, authLabel, authType, authValue string
	authID = auth.ID
	authLabel = auth.Label
	authType, authValue = auth.AccountInfo()

	executionSessionID := executionSessionIDFromOptions(opts)
	if executionSessionID == "" && fastEnabled {
		// See codex_websockets_execute.go: fast HTTP downstream needs a stable session
		// id to reuse the warm connection and run prewarm -> main. Fast path only.
		executionSessionID = codexFastSessionFallbackID(opts, req)
	}
	lease, errLease := e.acquireSessionLease(ctx, executionSessionID, auth, wsURL, codexFastIndependentRequest(ctx, req, opts, upstreamBody, fastEnabled))
	if errLease != nil {
		return nil, errLease
	}
	var sess *codexWebsocketSession
	if lease != nil {
		sess = lease.session
	}
	unlockStreamSession := func() { lease.release() }
	streamOwnsConnection := false
	defer func() {
		if !streamOwnsConnection {
			unlockStreamSession()
		}
	}()

	wsReqBody := buildCodexWebsocketRequestBody(upstreamBody)
	wsReqLog := helps.UpstreamRequestLog{
		URL:       wsURL,
		Method:    "WEBSOCKET",
		Headers:   wsHeaders.Clone(),
		Body:      wsReqBody,
		Provider:  e.Identifier(),
		AuthID:    authID,
		AuthLabel: authLabel,
		AuthType:  authType,
		AuthValue: authValue,
	}
	helps.RecordAPIWebsocketRequest(ctx, e.cfg, wsReqLog)

	var conn *websocket.Conn
	var closer *websocketConnectionCloser
	var respHS *http.Response
	var errDial error
	if cliproxyexecutor.RequiredUpstreamWebsocket(ctx) {
		conn, closer = existingWebsocketSessionConn(sess, authID, wsURL)
		if conn == nil {
			if sess != nil {
				unlockStreamSession()
			}
			return nil, cliproxyexecutor.NewUpstreamWebsocketReplayRequiredError()
		}
	} else {
		conn, closer, respHS, errDial = e.ensureUpstreamConn(ctx, auth, sess, authID, wsURL, wsHeaders)
	}
	var upstreamHeaders http.Header
	if respHS != nil {
		upstreamHeaders = respHS.Header.Clone()
	}
	if errDial != nil {
		bodyErr := websocketHandshakeBody(respHS)
		if respHS != nil {
			helps.RecordAPIWebsocketUpgradeRejection(ctx, e.cfg, websocketUpgradeRequestLog(wsReqLog), respHS.StatusCode, respHS.Header.Clone(), bodyErr)
		}
		if respHS != nil && respHS.StatusCode == http.StatusUpgradeRequired {
			if sess != nil {
				unlockStreamSession()
			}
			if opts.ExecutionLifecycle != nil || cliproxyexecutor.DownstreamWebsocket(ctx) {
				return nil, statusErr{code: respHS.StatusCode, msg: string(bodyErr)}
			}
			return e.CodexExecutor.ExecuteStream(ctx, auth, req, opts)
		}
		if respHS != nil && respHS.StatusCode > 0 {
			if sess != nil {
				unlockStreamSession()
			}
			return nil, statusErr{code: respHS.StatusCode, msg: string(bodyErr)}
		}
		helps.RecordAPIWebsocketError(ctx, e.cfg, "dial", errDial)
		if sess != nil {
			unlockStreamSession()
		}
		return nil, errDial
	}
	if errBind := sess.bindExecutionLifecycle(opts, conn, closer, req.Model); errBind != nil {
		closeWebsocketAfterBindFailure(sess, conn, closer)
		unlockStreamSession()
		return nil, errBind
	}
	recordAPIWebsocketHandshake(ctx, e.cfg, respHS)
	reporter.StartResponseTTFT()

	if sess == nil {
		logCodexWebsocketConnected(executionSessionID, authID, wsURL)
	}

	var readCh chan codexWebsocketRead
	if sess != nil {
		readCh = sess.activate(conn)
	}
	var stopFallbackCancel func() bool
	if httpFallbackAllowed {
		recoveryCloser := closer
		stopFallbackCancel = context.AfterFunc(ctx, func() { _ = recoveryCloser.Close() })
	}
	defer func() {
		if !streamOwnsConnection && stopFallbackCancel != nil {
			stopFallbackCancel()
		}
	}()

	// Codex fast: run the generate:false prewarm -> main turn link on this same
	// connection before the main send. Priority policy also applies over HTTP.
	if fastEnabled {
		wsPhase, wsMessageBytes = "prewarm", len(buildCodexWebsocketPrewarmBody(upstreamBody))
		prewarmID, errPrewarm := e.runCodexFastPrewarm(ctx, sess, conn, readCh, upstreamBody, identityState)
		if errPrewarm != nil {
			if ctx.Err() != nil {
				errPrewarm = ctx.Err()
			}
			helps.RecordAPIWebsocketError(ctx, e.cfg, "fast_prewarm", errPrewarm)
			if sess != nil {
				e.invalidateUpstreamConn(sess, conn, "fast_prewarm_error", errPrewarm)
				sess.clearActive(conn, readCh)
				unlockStreamSession()
			} else {
				logCodexWebsocketDisconnected(executionSessionID, authID, wsURL, "fast_prewarm_error", errPrewarm)
				if errClose := closer.Close(); errClose != nil {
					log.Errorf("codex websockets executor: close websocket error: %v", errClose)
				}
			}
			return nil, errPrewarm
		}
		wsReqBody = buildCodexWebsocketFastMainBody(upstreamBody, prewarmID)
		if httpFallbackAllowed && codexFastWebsocketSizeGate(ctx, wsReqBody, wsBudget, "main") {
			httpFallbackStarted = true
			if sess != nil {
				e.invalidateUpstreamConn(sess, conn, "http_size_gate", nil)
				sess.clearActive(conn, readCh)
				unlockStreamSession()
			} else {
				_ = closer.Close()
			}
			return e.executeHTTPFallbackStream(ctx, auth, req, opts, false)
		}
		helps.RecordAPIWebsocketRequest(ctx, e.cfg, helps.UpstreamRequestLog{
			URL:       wsURL,
			Method:    "WEBSOCKET",
			Headers:   wsHeaders.Clone(),
			Body:      wsReqBody,
			Provider:  e.Identifier(),
			AuthID:    authID,
			AuthLabel: authLabel,
			AuthType:  authType,
			AuthValue: authValue,
		})
	}

	wsPhase, wsMessageBytes = "main", len(wsReqBody)
	if errSend := writeCodexWebsocketMessage(sess, conn, wsReqBody); errSend != nil {
		errSend = mapCodexWebsocketWriteError(sess, conn, errSend)
		helps.RecordAPIWebsocketError(ctx, e.cfg, "send", errSend)
		if sess != nil {
			if cliproxyexecutor.RequiredUpstreamWebsocket(ctx) {
				e.invalidateUpstreamConnWithoutDisconnectNotify(sess, conn, "send_error", errSend)
				sess.clearActive(conn, readCh)
				unlockStreamSession()
				if !shouldRetryCodexWebsocketSend(errSend) {
					return nil, errSend
				}
				return nil, cliproxyexecutor.NewUpstreamWebsocketReplayRequiredError()
			}
			e.invalidateUpstreamConn(sess, conn, "send_error", errSend)
			if !shouldRetryCodexWebsocketSend(errSend) {
				sess.clearActive(conn, readCh)
				unlockStreamSession()
				return nil, errSend
			}

			// Retry once with a new websocket connection for the same execution session.
			connRetry, closerRetry, respHSRetry, errDialRetry := e.ensureUpstreamConn(ctx, auth, sess, authID, wsURL, wsHeaders)
			if errDialRetry != nil || connRetry == nil {
				closeHTTPResponseBody(respHSRetry, "codex websockets executor: close handshake response body error")
				helps.RecordAPIWebsocketError(ctx, e.cfg, "dial_retry", errDialRetry)
				sess.clearActive(conn, readCh)
				unlockStreamSession()
				return nil, errDialRetry
			}
			previousConn, previousReadCh := conn, readCh
			conn = connRetry
			closer = closerRetry
			if errBind := sess.bindExecutionLifecycle(opts, conn, closer, req.Model); errBind != nil {
				clearRetryActiveState(sess, previousConn, previousReadCh)
				closeWebsocketAfterBindFailure(sess, conn, closer)
				unlockStreamSession()
				return nil, errBind
			}
			readCh = sess.activate(conn)
			wsReqBodyRetry := buildCodexWebsocketRequestBody(upstreamBody)
			helps.RecordAPIWebsocketRequest(ctx, e.cfg, helps.UpstreamRequestLog{
				URL:       wsURL,
				Method:    "WEBSOCKET",
				Headers:   wsHeaders.Clone(),
				Body:      wsReqBodyRetry,
				Provider:  e.Identifier(),
				AuthID:    authID,
				AuthLabel: authLabel,
				AuthType:  authType,
				AuthValue: authValue,
			})
			recordAPIWebsocketHandshake(ctx, e.cfg, respHSRetry)
			reporter.StartResponseTTFT()
			if errSendRetry := writeCodexWebsocketMessage(sess, conn, wsReqBodyRetry); errSendRetry != nil {
				errSendRetry = mapCodexWebsocketWriteError(sess, conn, errSendRetry)
				helps.RecordAPIWebsocketError(ctx, e.cfg, "send_retry", errSendRetry)
				e.invalidateUpstreamConn(sess, conn, "send_error", errSendRetry)
				sess.clearActive(conn, readCh)
				unlockStreamSession()
				return nil, errSendRetry
			}
			wsReqBody = wsReqBodyRetry
		} else {
			logCodexWebsocketDisconnected(executionSessionID, authID, wsURL, "send_error", errSend)
			if errClose := closer.Close(); errClose != nil {
				log.Errorf("codex websockets executor: close websocket error: %v", errClose)
			}
			return nil, errSend
		}
	}

	var firstRead *codexWebsocketRead
	if httpFallbackAllowed {
		msgType, payload, errRead := readCodexWebsocketMessage(ctx, sess, conn, readCh)
		if errRead != nil {
			mappedErr := mapCodexWebsocketReadError(errRead)
			if ctx.Err() != nil {
				mappedErr = ctx.Err()
			}
			helps.RecordAPIWebsocketError(ctx, e.cfg, "read", mappedErr)
			if sess != nil {
				e.invalidateUpstreamConn(sess, conn, "read_error", mappedErr)
				sess.clearActive(conn, readCh)
				unlockStreamSession()
			} else {
				_ = closer.Close()
			}
			return nil, mappedErr
		}
		firstRead = &codexWebsocketRead{msgType: msgType, payload: payload}
		httpFallbackAllowed = false
	}
	out := make(chan cliproxyexecutor.StreamChunk)
	streamOwnsConnection = true
	go func() {
		terminateReason := "completed"
		var terminateErr error
		mainResponseCompleted := false

		defer close(out)
		defer func() {
			if stopFallbackCancel != nil {
				stopFallbackCancel()
			}
			if sess != nil {
				// Clearing the consumer alone leaves upstream generation running.
				// Discard that connection before another turn can activate it.
				if fastEnabled && !mainResponseCompleted {
					e.invalidateUpstreamConn(sess, conn, "fast_turn_incomplete", terminateErr)
				}
				sess.clearActive(conn, readCh)
				unlockStreamSession()
				return
			}
			logCodexWebsocketDisconnected(executionSessionID, authID, wsURL, terminateReason, terminateErr)
			if errClose := closer.Close(); errClose != nil {
				log.Errorf("codex websockets executor: close websocket error: %v", errClose)
			}
		}()

		send := func(chunk cliproxyexecutor.StreamChunk) bool {
			if ctx == nil {
				out <- chunk
				return true
			}
			select {
			case out <- chunk:
				return true
			case <-ctx.Done():
				return false
			}
		}

		claudeInputTokens := helps.NewClaudeInputTokenState(from, to, responseFormat, originalPayload)
		var param any
		outputItemsByIndex := make(map[int64][]byte)
		var outputItemsFallback [][]byte
		for {
			if ctx != nil && ctx.Err() != nil {
				terminateReason = "context_done"
				terminateErr = ctx.Err()
				_ = send(cliproxyexecutor.StreamChunk{Err: ctx.Err()})
				return
			}
			var msgType int
			var payload []byte
			var errRead error
			if firstRead != nil {
				msgType, payload = firstRead.msgType, firstRead.payload
				firstRead = nil
			} else {
				msgType, payload, errRead = readCodexWebsocketMessage(ctx, sess, conn, readCh)
			}
			if errRead != nil {
				if ctx != nil && ctx.Err() != nil {
					terminateReason = "context_done"
					terminateErr = ctx.Err()
					_ = send(cliproxyexecutor.StreamChunk{Err: ctx.Err()})
					return
				}
				mappedErr := mapCodexWebsocketReadError(errRead)
				terminateReason = "read_error"
				terminateErr = mappedErr
				helps.RecordAPIWebsocketError(ctx, e.cfg, "read", mappedErr)
				reporter.PublishFailure(ctx, mappedErr)
				_ = send(cliproxyexecutor.StreamChunk{Err: mappedErr})
				return
			}
			if msgType != websocket.TextMessage {
				if msgType == websocket.BinaryMessage {
					err = fmt.Errorf("codex websockets executor: unexpected binary message")
					terminateReason = "unexpected_binary"
					terminateErr = err
					helps.RecordAPIWebsocketError(ctx, e.cfg, "unexpected_binary", err)
					reporter.PublishFailure(ctx, err)
					if sess != nil {
						e.invalidateUpstreamConn(sess, conn, "unexpected_binary", err)
					}
					_ = send(cliproxyexecutor.StreamChunk{Err: err})
					return
				}
				continue
			}

			payload = bytes.TrimSpace(payload)
			if len(payload) == 0 {
				continue
			}
			reporter.MarkFirstResponseByte()
			reporter.ObserveContentEvent(payload)
			payload = applyCodexIdentityConfuseResponsePayload(payload, identityState)
			helps.AppendAPIWebsocketResponse(ctx, e.cfg, payload)
			payload = helps.RestoreCodexMultiAgentV2Response(payload, optimizeMultiAgentV2)

			if wsErr, ok := parseCodexWebsocketError(payload); ok {
				terminateReason = "upstream_error"
				terminateErr = wsErr
				if sess != nil {
					e.invalidateUpstreamConn(sess, conn, "upstream_error", wsErr)
				}
				if errClearReplay := clearCodexReasoningReplayOnWebsocketError(ctx, replayScope, payload); errClearReplay != nil {
					terminateErr = errClearReplay
					helps.RecordAPIWebsocketError(ctx, e.cfg, "replay_clear_error", errClearReplay)
					reporter.PublishFailure(ctx, errClearReplay)
					_ = send(cliproxyexecutor.StreamChunk{Err: errClearReplay})
					return
				}
				helps.RecordAPIWebsocketError(ctx, e.cfg, "upstream_error", wsErr)
				reporter.PublishFailure(ctx, wsErr)
				_ = send(cliproxyexecutor.StreamChunk{Err: wsErr})
				return
			}
			if streamErr, terminalBody, ok := codexTerminalFailureErr(payload); ok {
				terminateReason = "upstream_error"
				terminateErr = streamErr
				if sess != nil {
					e.invalidateUpstreamConn(sess, conn, "terminal_failure", streamErr)
					unlockStreamSession()
				}
				if errClearReplay := clearCodexReasoningReplayOnInvalidSignature(ctx, replayScope, streamErr.StatusCode(), terminalBody); errClearReplay != nil {
					terminateErr = errClearReplay
					helps.RecordAPIWebsocketError(ctx, e.cfg, "replay_clear_error", errClearReplay)
					reporter.PublishFailure(ctx, errClearReplay)
					_ = send(cliproxyexecutor.StreamChunk{Err: errClearReplay})
					return
				}
				helps.RecordAPIWebsocketError(ctx, e.cfg, "upstream_error", streamErr)
				reporter.PublishFailure(ctx, streamErr)
				_ = send(cliproxyexecutor.StreamChunk{Err: streamErr})
				return
			}

			eventType := gjson.GetBytes(payload, "type").String()
			isTerminalEvent := eventType == "response.completed" || eventType == "response.done" || eventType == "error"
			if eventType == "response.output_item.done" {
				collectCodexOutputItemDone(payload, outputItemsByIndex, &outputItemsFallback)
			}
			completedPayload := payload
			if eventType == "response.completed" || eventType == "response.done" {
				mainResponseCompleted = true
				completedPayload = normalizeCodexWebsocketCompletion(completedPayload)
				completedPayload = patchCodexCompletedOutput(completedPayload, outputItemsByIndex, outputItemsFallback)
				cacheCodexReasoningReplayFromCompleted(replayScope, completedPayload)
				if detail, ok := helps.ParseCodexUsage(completedPayload); ok {
					reporter.Publish(ctx, detail)
				}
			}

			clientPayload := applyCodexIdentityExposeResponsePayload(payload, identityState)
			if cliproxyexecutor.DownstreamWebsocket(ctx) {
				if !send(cliproxyexecutor.StreamChunk{Payload: clientPayload}) {
					terminateReason = "context_done"
					terminateErr = ctx.Err()
					return
				}
				if isTerminalEvent {
					return
				}
				continue
			}

			payload = normalizeCodexWebsocketCompletion(payload)
			if eventType == "response.completed" || eventType == "response.done" {
				payload = completedPayload
			}
			eventType = gjson.GetBytes(payload, "type").String()
			clientPayload = applyCodexIdentityExposeResponsePayload(payload, identityState)
			line := encodeCodexWebsocketAsSSE(clientPayload)
			chunks := helps.TranslateStreamWithClaudeInputTokens(ctx, to, responseFormat, req.Model, originalPayload, clientBody, line, &param, claudeInputTokens)
			for i := range chunks {
				if !send(cliproxyexecutor.StreamChunk{Payload: chunks[i]}) {
					terminateReason = "context_done"
					terminateErr = ctx.Err()
					return
				}
			}
			if eventType == "response.completed" || eventType == "response.done" {
				return
			}
		}
	}()

	return &cliproxyexecutor.StreamResult{Headers: upstreamHeaders, Chunks: out}, nil
}
