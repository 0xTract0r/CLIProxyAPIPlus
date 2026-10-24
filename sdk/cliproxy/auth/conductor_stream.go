package auth

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func discardStreamChunks(ch <-chan cliproxyexecutor.StreamChunk) {
	if ch == nil {
		return
	}
	go func() {
		for range ch {
		}
	}()
}

type streamBootstrapError struct {
	cause   error
	headers http.Header
}

func cloneHTTPHeader(headers http.Header) http.Header {
	if headers == nil {
		return nil
	}
	return headers.Clone()
}

func newStreamBootstrapError(err error, headers http.Header) error {
	if err == nil {
		return nil
	}
	upstreamAttempt := hasUpstreamExecutionAttempt(err)
	err = unwrapUpstreamExecutionAttempt(err)
	bootstrapErr := &streamBootstrapError{
		cause:   err,
		headers: cloneHTTPHeader(headers),
	}
	if upstreamAttempt {
		return markUpstreamExecutionAttempt(bootstrapErr)
	}
	return bootstrapErr
}

func (e *streamBootstrapError) Error() string {
	if e == nil || e.cause == nil {
		return ""
	}
	return e.cause.Error()
}

func (e *streamBootstrapError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

func (e *streamBootstrapError) Headers() http.Header {
	if e == nil {
		return nil
	}
	return cloneHTTPHeader(e.headers)
}

func streamErrorResult(headers http.Header, err error) *cliproxyexecutor.StreamResult {
	ch := make(chan cliproxyexecutor.StreamChunk, 1)
	ch <- cliproxyexecutor.StreamChunk{Err: err}
	close(ch)
	return &cliproxyexecutor.StreamResult{
		Headers: cloneHTTPHeader(headers),
		Chunks:  ch,
	}
}

func validateStreamResult(result *cliproxyexecutor.StreamResult, err error) (*cliproxyexecutor.StreamResult, error) {
	if err != nil {
		return result, err
	}
	if result == nil || result.Chunks == nil {
		return result, &Error{Code: "empty_stream", Message: "upstream stream has no source", Retryable: true}
	}
	return result, nil
}

func readStreamBootstrap(ctx context.Context, ch <-chan cliproxyexecutor.StreamChunk) ([]cliproxyexecutor.StreamChunk, bool, error) {
	if ch == nil {
		return nil, true, nil
	}
	buffered := make([]cliproxyexecutor.StreamChunk, 0, 1)
	for {
		var (
			chunk cliproxyexecutor.StreamChunk
			ok    bool
		)
		if ctx != nil {
			// Prefer a chunk that is already available so an upstream error delivered together
			// with a context cancellation is not discarded.
			select {
			case chunk, ok = <-ch:
			default:
				select {
				case <-ctx.Done():
					return nil, false, ctx.Err()
				case chunk, ok = <-ch:
				}
			}
		} else {
			chunk, ok = <-ch
		}
		if !ok {
			return buffered, true, nil
		}
		if chunk.Err != nil {
			return nil, false, chunk.Err
		}
		buffered = append(buffered, chunk)
		if len(chunk.Payload) > 0 {
			return buffered, false, nil
		}
	}
}

// recordCanceledUpstreamCooldown keeps an upstream-confirmed cooldown (an error carrying
// RetryAfter) when the request context was canceled while the executor was returning, for
// example because the downstream connection was closed in response to the same upstream error.
// Errors without RetryAfter, such as plain client cancellations, are not recorded.
func (m *Manager) recordCanceledUpstreamCooldown(ctx context.Context, auth *Auth, provider, resultModel, routeModel string, opts cliproxyexecutor.Options, err error, ephemeral bool, executionSeq uint64) {
	retryAfter := retryAfterFromError(err)
	if m == nil || auth == nil || retryAfter == nil {
		return
	}
	action, okAction := matchRequestScopedErrorAction(auth, err, m.runtimeConfigSnapshot())
	result := Result{AuthID: auth.ID, Provider: provider, Model: resultModel, RouteModel: routeModel, Success: false, Error: resultErrorFromError(err), Options: opts, RetryAfter: retryAfter, CredentialVersion: auth.CredentialVersion, RegistrationEpoch: auth.RegistrationEpoch, ExecutionSeq: executionSeq}
	result.CredentialScope = isCredentialScopedError(err)
	applyRequestScopedActionToResult(action, okAction, &result)
	m.recordExecutionResult(context.WithoutCancel(ctx), result, auth, ephemeral)
}

func (m *Manager) wrapStreamResult(ctx context.Context, auth *Auth, provider, resultModel, routeModel string, headers http.Header, buffered []cliproxyexecutor.StreamChunk, remaining <-chan cliproxyexecutor.StreamChunk, aliasResult OAuthModelAliasResult, ephemeralResult bool, opts cliproxyexecutor.Options, executionSeq uint64, onComplete ...func()) *cliproxyexecutor.StreamResult {
	out := make(chan cliproxyexecutor.StreamChunk)
	streamStart := time.Now()
	slot := warmupExecutionSlotFromContext(ctx)
	target := slot != nil && slot.target
	go func() {
		defer close(out)
		// Adaptive account scheduling (Phase 2): release the account's in-flight
		// concurrency slot when this forwarding goroutine finishes -- i.e. at
		// true stream completion (all chunks drained, ctx cancelled, or an early
		// return). Deferred so it fires on every exit path including a panic, so
		// the slot is held for the stream's whole lifetime, not just until it
		// started. onComplete is empty for callers that pass no hook (e.g. the
		// existing unit tests and Home dispatch), making this a no-op there.
		for _, done := range onComplete {
			if done != nil {
				defer done()
			}
		}
		var failed bool
		forward := true
		var rewriter *StreamRewriter
		if aliasResult.ForceMapping && strings.TrimSpace(aliasResult.OriginalAlias) != "" {
			rewriter = NewStreamRewriter(StreamRewriteOptions{RewriteModel: aliasResult.OriginalAlias})
		}
		emit := func(chunk cliproxyexecutor.StreamChunk) bool {
			if target && ctx.Err() != nil {
				return false
			}
			if chunk.Err != nil && !failed {
				failed = true
				entry := logEntryWithRequestID(ctx)
				warnLogUpstreamFailure(ctx, entry, provider, resultModel, auth, time.Since(streamStart), chunk.Err)
				rerr := resultErrorFromError(chunk.Err)
				action, okAction := matchRequestScopedErrorAction(auth, chunk.Err, m.runtimeConfigSnapshot())
				result := Result{AuthID: auth.ID, Provider: provider, Model: resultModel, RouteModel: routeModel, Success: false, Error: rerr, Options: opts, CredentialVersion: auth.CredentialVersion, RegistrationEpoch: auth.RegistrationEpoch, ExecutionSeq: executionSeq}
				result.RetryAfter = retryAfterFromError(chunk.Err)
				result.CredentialScope = isCredentialScopedError(chunk.Err)
				applyRequestScopedActionToResult(action, okAction, &result)
				m.recordExecutionResult(ctx, result, auth, ephemeralResult)
			}
			if !forward {
				return false
			}
			if chunk.Err != nil {
				if ctx == nil {
					out <- chunk
					return true
				}
				select {
				case <-ctx.Done():
					forward = false
					return false
				case out <- chunk:
					return true
				}
			}
			if len(chunk.Payload) == 0 {
				return true
			}
			payload := rewriteForceMappedStreamChunk(rewriter, chunk.Payload)
			if len(payload) == 0 {
				return true
			}
			chunk.Payload = payload
			if ctx == nil {
				out <- chunk
				return true
			}
			select {
			case <-ctx.Done():
				forward = false
				return false
			case out <- chunk:
				return true
			}
		}
		for _, chunk := range buffered {
			if ok := emit(chunk); !ok || (target && chunk.Err != nil) {
				if !target {
					discardStreamChunks(remaining)
				}
				return
			}
		}
		if target {
			for remaining != nil {
				select {
				case <-ctx.Done():
					return
				case chunk, ok := <-remaining:
					if ctx.Err() != nil {
						return
					}
					if !ok {
						remaining = nil
						break
					}
					if !emit(chunk) || chunk.Err != nil {
						return
					}
				}
			}
		} else {
			for chunk := range remaining {
				if !emit(chunk) {
					discardStreamChunks(remaining)
					return
				}
			}
		}
		if target && ctx.Err() != nil {
			return
		}
		if tail := finishForceMappedStreamChunks(rewriter); len(tail) > 0 {
			tailChunk := cliproxyexecutor.StreamChunk{Payload: tail}
			if !emit(tailChunk) {
				return
			}
		}
		if !failed && (ephemeralResult || claudeOAuthRequestCancellation(ctx, auth, nil) == nil) {
			result := Result{AuthID: auth.ID, Provider: provider, Model: resultModel, RouteModel: routeModel, Success: true, Options: opts, CredentialVersion: auth.CredentialVersion, RegistrationEpoch: auth.RegistrationEpoch, ExecutionSeq: executionSeq}
			// Fork codex proactive quota cooldown: cool the auth down when the
			// stream's upstream quota headers already report exhaustion.
			if retryAfter := quotaRetryAfterFromHeadersNow(provider, headers); retryAfter != nil {
				result.QuotaExceeded = true
				result.RetryAfter = retryAfter
			}
			m.recordExecutionResult(ctx, result, auth, ephemeralResult)
		}
	}()
	return &cliproxyexecutor.StreamResult{Headers: headers, Chunks: out}
}

func (m *Manager) executeStreamWithModelPool(ctx context.Context, executor ProviderExecutor, auth *Auth, provider string, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, routeModel, executionModel string, execModels []string, pooled bool, aliasResult OAuthModelAliasResult, routing *apiKeyModelRoutingSnapshot, allowRetry bool, ephemeralResult bool) (outResult *cliproxyexecutor.StreamResult, outErr error) {
	if executor == nil {
		return nil, &Error{Code: "executor_not_found", Message: "executor not registered"}
	}
	executor = executorForAuth(executor, auth)
	ctx = contextWithRequestedModelAlias(ctx, opts, routeModel)
	var lastErr error
	var upstreamErr error
	didRefreshOnUnauthorized := false
	for idx, execModel := range execModels {
		ctx = newUpstreamAttemptContext(ctx)
		resultModel := m.stateModelForExecution(auth, routeModel, execModel, pooled)
		execReq := req
		execReq.Model = execModel
		if executionModel != "" {
			execReq.Model = executionModel
		}
		execOpts := opts
		var errIntercept error
		execReq, execOpts, errIntercept = applyRequestAfterAuthInterceptor(ctx, executor, provider, execReq, execOpts, requestedModelAliasFromOptions(execOpts, routeModel))
		if errIntercept != nil {
			return nil, errIntercept
		}
		execReq = attachResolvedExecutionModelInfo(routing, execReq, auth, routeModel, execModel, executionModel != "")
		if errCtx := ctx.Err(); errCtx != nil {
			return nil, errCtx
		}
		entry := logEntryWithRequestID(ctx)
		payload := execOpts.OriginalRequest
		if len(payload) == 0 {
			payload = execReq.Payload
		}
		execOpts.Metadata = ensureCanonicalSessionMetadata(execOpts.Metadata, execOpts.Headers, payload)
		ctx = syncMetadataSessionToContext(ctx, execOpts.Metadata)
		if !authAllowsClaudeContext(auth, execReq.Model, execOpts) {
			return nil, claudeContextEntitlementError()
		}
		var slot *accountExecutionSlot
		target := false
		if !ephemeralResult {
			var admissionErr error
			slot, target, admissionErr = m.admitWarmupExecution(ctx, auth, routeModel, execOpts)
			if admissionErr != nil {
				if local, ok := admissionErr.(*warmupAdmissionReselect); ok {
					local.previous = lastErr
					warmupSetModelResume(ctx, auth.ID, execModel)
				}
				return nil, admissionErr
			}
		}
		modelCtx := m.pacingExecutionContext(withWarmupExecutionSlot(ctx, slot), auth.ID)
		if holder, _ := modelCtx.Value(pacingOwnershipKey{}).(*pacingOwnership); holder != nil {
			holder.stream = true
		}
		transferred := false
		cancelAttempt := func() {}
		defer func() {
			if !transferred {
				m.finishPacingCall(modelCtx)
				cancelAttempt()
				slot.close(modelCtx)
			}
		}()
		send := func() (*cliproxyexecutor.StreamResult, error) {
			sendCtx := modelCtx
			if target {
				cancelAttempt()
				sendCtx, cancelAttempt = context.WithCancel(modelCtx)
			}
			sendCtx, gateErr := m.pacingSendContext(sendCtx, executor, auth, routeModel, execOpts)
			if gateErr != nil {
				return nil, gateErr
			}
			if err := warmupBeforeSend(sendCtx, slot); err != nil {
				return nil, err
			}
			response, err := executor.ExecuteStream(sendCtx, auth, execReq, execOpts)
			return response, markUpstreamExecutionAttemptFromContext(sendCtx, pacingExecutionError(err))
		}
		discard := func(ch <-chan cliproxyexecutor.StreamChunk) {
			if target {
				cancelAttempt()
			} else {
				discardStreamChunks(ch)
			}
		}
		executionSeq := m.nextExecutionSeq()
		startStream := time.Now()
		streamResult, errStream := send()
		errStream = markUpstreamExecutionAttemptFromContext(modelCtx, errStream)
		if hasUpstreamExecutionAttempt(errStream) {
			upstreamErr = errStream
		}
		durationStream := time.Since(startStream)
		if errStream != nil {
			if errCtx := ctx.Err(); errCtx != nil {
				m.recordCanceledUpstreamCooldown(modelCtx, auth, provider, resultModel, routeModel, execOpts, errStream, ephemeralResult, executionSeq)
				return nil, errCtx
			}
			if allowRetry && !ephemeralResult {
				alreadyTried := didRefreshOnUnauthorized
				refreshed, okRefresh := m.tryRefreshAfterUnauthorized(newUpstreamAttemptContext(ctx), auth, errStream, alreadyTried, execOpts.Headers)
				if okRefresh {
					priorErr := errStream
					auth = refreshed
					publishSelectedAuthMetadata(execOpts.Metadata, auth)
					didRefreshOnUnauthorized = true
					modelCtx = newUpstreamAttemptContext(modelCtx)
					ctx = syncMetadataSessionToContext(ctx, execOpts.Metadata)
					executionSeq = m.nextExecutionSeq()
					startRetry := time.Now()
					streamResult, errStream = send()
					errStream = pacingKeepPrevious(errStream, priorErr)
					errStream = markUpstreamExecutionAttemptFromContext(modelCtx, errStream)
					if hasUpstreamExecutionAttempt(errStream) {
						upstreamErr = errStream
					}
					durationRetry := time.Since(startRetry)
					if errStream != nil {
						warnLogUpstreamFailure(ctx, entry, provider, execModel, auth, durationRetry, errStream)
						if errCtx := ctx.Err(); errCtx != nil {
							m.recordCanceledUpstreamCooldown(modelCtx, auth, provider, resultModel, routeModel, execOpts, errStream, ephemeralResult, executionSeq)
							return nil, errCtx
						}
					}
				} else {
					warnLogUpstreamFailure(ctx, entry, provider, execModel, auth, durationStream, errStream)
				}
			} else {
				warnLogUpstreamFailure(ctx, entry, provider, execModel, auth, durationStream, errStream)
			}
		}
		if local, rejected := pacingLocalError(errStream); rejected {
			var reselect *warmupAdmissionReselect
			if errors.As(local, &reselect) {
				reselect.previous = lastErr
				warmupSetModelResume(ctx, auth.ID, execModel)
			}
			return nil, local
		}
		if !ephemeralResult {
			if errCancel := claudeOAuthRequestCancellation(ctx, auth, errStream); errCancel != nil {
				return nil, errCancel
			}
		}
		streamResult, errStream = validateStreamResult(streamResult, errStream)
		errStream = markUpstreamExecutionAttemptFromContext(modelCtx, errStream)
		if errStream != nil {
			rerr := resultErrorFromError(errStream)
			action, okAction := matchRequestScopedErrorAction(auth, errStream, m.runtimeConfigSnapshot())
			result := Result{AuthID: auth.ID, Provider: provider, Model: resultModel, RouteModel: routeModel, Success: false, Error: rerr, Options: execOpts, CredentialVersion: auth.CredentialVersion, RegistrationEpoch: auth.RegistrationEpoch, ExecutionSeq: executionSeq}
			result.RetryAfter = retryAfterFromError(errStream)
			if isCredentialScopedError(errStream) {
				result.CredentialScope = true
			}
			applyRequestScopedActionToResult(action, okAction, &result)
			m.recordExecutionResult(modelCtx, result, auth, ephemeralResult)
			if target {
				cancelAttempt()
			}
			slot.close(modelCtx)
			m.finishPacingCall(modelCtx)

			if okAction {
				if isRequestScopedStop(action, okAction) {
					return nil, wrapRequestStopError(errStream)
				}
				lastErr = errStream
				if result.CredentialScope {
					return nil, preferredExecutionAttemptError(errStream, upstreamErr)
				}
				continue
			}
			if isRequestInvalidError(errStream) {
				return nil, errStream
			}
			lastErr = errStream
			if result.CredentialScope {
				return nil, preferredExecutionAttemptError(errStream, upstreamErr)
			}
			continue
		}

		buffered, closed, bootstrapErr := readStreamBootstrap(ctx, streamResult.Chunks)
		bootstrapErr = markUpstreamExecutionAttemptFromContext(modelCtx, bootstrapErr)
		if hasUpstreamExecutionAttempt(bootstrapErr) {
			upstreamErr = newStreamBootstrapError(bootstrapErr, streamResult.Headers)
		}
		if bootstrapErr != nil {
			if errCtx := ctx.Err(); errCtx != nil {
				m.recordCanceledUpstreamCooldown(modelCtx, auth, provider, resultModel, routeModel, execOpts, bootstrapErr, ephemeralResult, executionSeq)
				discard(streamResult.Chunks)
				return nil, errCtx
			}
			if allowRetry && !ephemeralResult {
				alreadyTried := didRefreshOnUnauthorized
				refreshed, okRefresh := m.tryRefreshAfterUnauthorized(newUpstreamAttemptContext(ctx), auth, bootstrapErr, alreadyTried, execOpts.Headers)
				if okRefresh {
					discard(streamResult.Chunks)
					auth = refreshed
					publishSelectedAuthMetadata(execOpts.Metadata, auth)
					didRefreshOnUnauthorized = true
					modelCtx = newUpstreamAttemptContext(modelCtx)
					executionSeq = m.nextExecutionSeq()
					startRetry := time.Now()
					retryStream, retryErr := send()
					retryErr = pacingKeepPrevious(retryErr, bootstrapErr)
					retryErr = markUpstreamExecutionAttemptFromContext(modelCtx, retryErr)
					retryStream, retryErr = validateStreamResult(retryStream, retryErr)
					retryErr = markUpstreamExecutionAttemptFromContext(modelCtx, retryErr)
					if retryErr != nil {
						if errCtx := ctx.Err(); errCtx != nil {
							m.recordCanceledUpstreamCooldown(modelCtx, auth, provider, resultModel, routeModel, execOpts, retryErr, ephemeralResult, executionSeq)
							return nil, errCtx
						}
						bootstrapErr = retryErr
						warnLogUpstreamFailure(ctx, entry, provider, execModel, auth, time.Since(startRetry), bootstrapErr)
						streamResult = &cliproxyexecutor.StreamResult{}
					} else {
						streamResult = retryStream
						buffered, closed, bootstrapErr = readStreamBootstrap(ctx, streamResult.Chunks)
						bootstrapErr = markUpstreamExecutionAttemptFromContext(modelCtx, bootstrapErr)
						if bootstrapErr != nil {
							warnLogUpstreamFailure(ctx, entry, provider, execModel, auth, time.Since(startRetry), bootstrapErr)
						}
					}
				} else {
					warnLogUpstreamFailure(ctx, entry, provider, execModel, auth, time.Since(startStream), bootstrapErr)
				}
			} else {
				warnLogUpstreamFailure(ctx, entry, provider, execModel, auth, time.Since(startStream), bootstrapErr)
			}
			if hasUpstreamExecutionAttempt(bootstrapErr) {
				upstreamErr = newStreamBootstrapError(bootstrapErr, streamResult.Headers)
			}
		}
		if !ephemeralResult {
			if errCancel := claudeOAuthRequestCancellation(ctx, auth, bootstrapErr); errCancel != nil {
				discardStreamChunks(streamResult.Chunks)
				return nil, errCancel
			}
		}
		if bootstrapErr != nil {
			action, okAction := matchRequestScopedErrorAction(auth, bootstrapErr, m.runtimeConfigSnapshot())
			if okAction {
				rerr := resultErrorFromError(bootstrapErr)
				result := Result{AuthID: auth.ID, Provider: provider, Model: resultModel, RouteModel: routeModel, Success: false, Error: rerr, Options: execOpts, CredentialVersion: auth.CredentialVersion, RegistrationEpoch: auth.RegistrationEpoch, ExecutionSeq: executionSeq}
				result.RetryAfter = retryAfterFromError(bootstrapErr)
				if isCredentialScopedError(bootstrapErr) {
					result.CredentialScope = true
				}
				applyRequestScopedActionToResult(action, okAction, &result)
				m.recordExecutionResult(ctx, result, auth, ephemeralResult)
				discardStreamChunks(streamResult.Chunks)
				if isRequestScopedStop(action, okAction) {
					return nil, wrapRequestStopError(bootstrapErr)
				}
				lastErr = bootstrapErr
				if result.CredentialScope {
					currentErr := newStreamBootstrapError(bootstrapErr, streamResult.Headers)
					return nil, preferredExecutionAttemptError(currentErr, upstreamErr)
				}
				continue
			}
			if isRequestInvalidError(bootstrapErr) {
				rerr := resultErrorFromError(bootstrapErr)
				result := Result{AuthID: auth.ID, Provider: provider, Model: resultModel, RouteModel: routeModel, Success: false, Error: rerr, Options: execOpts, CredentialVersion: auth.CredentialVersion, RegistrationEpoch: auth.RegistrationEpoch, ExecutionSeq: executionSeq}
				result.RetryAfter = retryAfterFromError(bootstrapErr)
				if isCredentialScopedError(bootstrapErr) {
					result.CredentialScope = true
				}
				m.recordExecutionResult(modelCtx, result, auth, ephemeralResult)
				if target {
					cancelAttempt()
				}
				slot.close(modelCtx)
				discard(streamResult.Chunks)
				return nil, bootstrapErr
			}
			if idx < len(execModels)-1 {
				rerr := resultErrorFromError(bootstrapErr)
				result := Result{AuthID: auth.ID, Provider: provider, Model: resultModel, RouteModel: routeModel, Success: false, Error: rerr, Options: execOpts, CredentialVersion: auth.CredentialVersion, RegistrationEpoch: auth.RegistrationEpoch, ExecutionSeq: executionSeq}
				result.RetryAfter = retryAfterFromError(bootstrapErr)
				if isCredentialScopedError(bootstrapErr) {
					result.CredentialScope = true
				}
				m.recordExecutionResult(modelCtx, result, auth, ephemeralResult)
				if target {
					cancelAttempt()
				}
				slot.close(modelCtx)
				discard(streamResult.Chunks)
				lastErr = bootstrapErr
				if result.CredentialScope {
					currentErr := newStreamBootstrapError(bootstrapErr, streamResult.Headers)
					return nil, preferredExecutionAttemptError(currentErr, upstreamErr)
				}
				continue
			}
			rerr := resultErrorFromError(bootstrapErr)
			result := Result{AuthID: auth.ID, Provider: provider, Model: resultModel, RouteModel: routeModel, Success: false, Error: rerr, Options: execOpts, CredentialVersion: auth.CredentialVersion, RegistrationEpoch: auth.RegistrationEpoch, ExecutionSeq: executionSeq}
			result.RetryAfter = retryAfterFromError(bootstrapErr)
			if isCredentialScopedError(bootstrapErr) {
				result.CredentialScope = true
			}
			m.recordExecutionResult(modelCtx, result, auth, ephemeralResult)
			if target {
				cancelAttempt()
			}
			slot.close(modelCtx)
			m.finishPacingCall(modelCtx)

			discard(streamResult.Chunks)
			currentErr := newStreamBootstrapError(bootstrapErr, streamResult.Headers)
			return nil, preferredExecutionAttemptError(currentErr, upstreamErr)
		}

		if closed && len(buffered) == 0 {
			emptyErr := markUpstreamExecutionAttemptFromContext(modelCtx, &Error{Code: "empty_stream", Message: "upstream stream closed before first payload", Retryable: true})
			currentErr := newStreamBootstrapError(emptyErr, streamResult.Headers)
			if hasUpstreamExecutionAttempt(emptyErr) {
				upstreamErr = currentErr
			}
			warnLogUpstreamFailure(ctx, entry, provider, execModel, auth, time.Since(startStream), emptyErr)
			result := Result{AuthID: auth.ID, Provider: provider, Model: resultModel, RouteModel: routeModel, Success: false, Error: resultErrorFromError(emptyErr), Options: execOpts, CredentialVersion: auth.CredentialVersion, RegistrationEpoch: auth.RegistrationEpoch, ExecutionSeq: executionSeq}
			m.recordExecutionResult(modelCtx, result, auth, ephemeralResult)
			if target {
				cancelAttempt()
			}
			slot.close(modelCtx)
			m.finishPacingCall(modelCtx)

			if idx < len(execModels)-1 {
				lastErr = emptyErr
				continue
			}
			return nil, preferredExecutionAttemptError(currentErr, upstreamErr)
		}

		// Bootstrap/retries have selected this immutable attempt; wrapper never switches source.
		selectedTransport := streamResult.InterruptOwner
		defer func() {
			if outResult != nil {
				outResult.InterruptOwner = selectedTransport
			}
		}()
		remaining := streamResult.Chunks
		if closed {
			closedCh := make(chan cliproxyexecutor.StreamChunk)
			close(closedCh)
			remaining = closedCh
		}
		attemptAliasResult := resolveAttemptAliasResult(routing, auth, routeModel, execModel, aliasResult)
		// Home owns its lifecycle. Opt-in slots transfer their existing admission
		// to the wrapper; other streams retain legacy post-bootstrap accounting.
		if ephemeralResult {
			return m.wrapStreamResult(ctx, auth.Clone(), provider, resultModel, routeModel, streamResult.Headers, buffered, remaining, attemptAliasResult, ephemeralResult, execOpts, executionSeq), nil
		}
		if target {
			transferred = true
			return m.wrapStreamResult(modelCtx, auth.Clone(), provider, resultModel, routeModel, streamResult.Headers, buffered, remaining, attemptAliasResult, false, execOpts, executionSeq, func() {
				cancelAttempt()
				slot.close(modelCtx)
				m.finishPacingCall(modelCtx)
			}), nil
		}
		legacySlot, _ := m.beginAccountExecution(auth)
		transferred = true
		return m.wrapStreamResult(modelCtx, auth.Clone(), provider, resultModel, routeModel, streamResult.Headers, buffered, remaining, attemptAliasResult, ephemeralResult, execOpts, executionSeq, func() { legacySlot.release(); m.finishPacingCall(modelCtx) }), nil
	}
	if lastErr == nil {
		lastErr = &Error{Code: "auth_not_found", Message: "no upstream model available"}
	}
	return nil, preferredExecutionAttemptError(lastErr, upstreamErr)
}
