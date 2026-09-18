package auth

import (
	"context"
	"errors"
	"net/http"
	"strings"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
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
	return &streamBootstrapError{
		cause:   err,
		headers: cloneHTTPHeader(headers),
	}
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
			select {
			case <-ctx.Done():
				return nil, false, ctx.Err()
			case chunk, ok = <-ch:
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

func (m *Manager) wrapStreamResult(ctx context.Context, auth *Auth, provider, resultModel string, headers http.Header, buffered []cliproxyexecutor.StreamChunk, remaining <-chan cliproxyexecutor.StreamChunk, aliasResult OAuthModelAliasResult, ephemeralResult bool, onComplete ...func()) *cliproxyexecutor.StreamResult {
	out := make(chan cliproxyexecutor.StreamChunk)
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
				rerr := resultErrorFromError(chunk.Err)
				result := Result{AuthID: auth.ID, Provider: provider, Model: resultModel, Success: false, Error: rerr}
				// Fork plan-quota cooldown: carry any Retry-After from the mid-stream
				// error so recordExecutionResult can mark the model's quota exhausted
				// and schedule the cooldown instead of retrying an exhausted account.
				result.RetryAfter = retryAfterFromError(chunk.Err)
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
		if !failed {
			result := Result{AuthID: auth.ID, Provider: provider, Model: resultModel, Success: true}
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

func (m *Manager) executeStreamWithModelPool(ctx context.Context, executor ProviderExecutor, auth *Auth, provider string, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, routeModel, executionModel string, execModels []string, pooled bool, aliasResult OAuthModelAliasResult, allowRetry bool, ephemeralResult bool) (*cliproxyexecutor.StreamResult, error) {
	if executor == nil {
		return nil, &Error{Code: "executor_not_found", Message: "executor not registered"}
	}
	ctx = contextWithRequestedModelAlias(ctx, opts, routeModel)
	var lastErr error
	didRefreshOnUnauthorized := false
	for idx, execModel := range execModels {
		resultModel := m.stateModelForExecution(auth, routeModel, execModel, pooled)
		execReq := req
		execReq.Model = execModel
		if executionModel != "" {
			execReq.Model = executionModel
		}
		execOpts := opts
		execReq, execOpts = applyRequestAfterAuthInterceptor(ctx, executor, provider, execReq, execOpts, requestedModelAliasFromOptions(execOpts, routeModel))
		if !authAllowsClaudeContext(auth, execReq.Model, execOpts) {
			return nil, claudeContextEntitlementError()
		}
		if errCtx := ctx.Err(); errCtx != nil {
			return nil, errCtx
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
			return response, pacingExecutionError(err)
		}
		discard := func(ch <-chan cliproxyexecutor.StreamChunk) {
			if target {
				cancelAttempt()
			} else {
				discardStreamChunks(ch)
			}
		}
		streamResult, errStream := send()
		if errStream != nil {
			if errCtx := ctx.Err(); errCtx != nil {
				return nil, errCtx
			}
			if allowRetry {
				if refreshed, okRefresh := m.tryRefreshAfterUnauthorized(ctx, auth, errStream, didRefreshOnUnauthorized); okRefresh {
					priorErr := errStream
					auth = refreshed
					didRefreshOnUnauthorized = true
					streamResult, errStream = send()
					errStream = pacingKeepPrevious(errStream, priorErr)
					if errStream != nil {
						if errCtx := ctx.Err(); errCtx != nil {
							return nil, errCtx
						}
					}
				}
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
		if errStream == nil && (streamResult == nil || streamResult.Chunks == nil) {
			errStream = &Error{Code: "empty_stream", Message: "upstream stream has no source", Retryable: true}
		}
		if errStream != nil {
			rerr := resultErrorFromError(errStream)
			result := Result{AuthID: auth.ID, Provider: provider, Model: resultModel, Success: false, Error: rerr}
			result.RetryAfter = retryAfterFromError(errStream)
			m.recordExecutionResult(modelCtx, result, auth, ephemeralResult)
			if target {
				cancelAttempt()
			}
			slot.close(modelCtx)
			if isRequestInvalidError(errStream) {
				return nil, errStream
			}
			lastErr = errStream
			continue
		}

		buffered, closed, bootstrapErr := readStreamBootstrap(ctx, streamResult.Chunks)
		if bootstrapErr != nil {
			if errCtx := ctx.Err(); errCtx != nil {
				discard(streamResult.Chunks)
				return nil, errCtx
			}
			if allowRetry {
				if refreshed, okRefresh := m.tryRefreshAfterUnauthorized(ctx, auth, bootstrapErr, didRefreshOnUnauthorized); okRefresh {
					discard(streamResult.Chunks)
					auth = refreshed
					didRefreshOnUnauthorized = true
					retryStream, retryErr := send()
					retryErr = pacingKeepPrevious(retryErr, bootstrapErr)
					if retryErr == nil && (retryStream == nil || retryStream.Chunks == nil) {
						retryErr = &Error{Code: "empty_stream", Message: "upstream retry stream has no source", Retryable: true}
					}
					if retryErr != nil {
						if errCtx := ctx.Err(); errCtx != nil {
							return nil, errCtx
						}
						bootstrapErr = retryErr
						streamResult = &cliproxyexecutor.StreamResult{}
					} else {
						streamResult = retryStream
						buffered, closed, bootstrapErr = readStreamBootstrap(ctx, streamResult.Chunks)
					}
				}
			}
		}
		if bootstrapErr != nil {
			if isRequestInvalidError(bootstrapErr) {
				rerr := resultErrorFromError(bootstrapErr)
				result := Result{AuthID: auth.ID, Provider: provider, Model: resultModel, Success: false, Error: rerr}
				result.RetryAfter = retryAfterFromError(bootstrapErr)
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
				result := Result{AuthID: auth.ID, Provider: provider, Model: resultModel, Success: false, Error: rerr}
				result.RetryAfter = retryAfterFromError(bootstrapErr)
				m.recordExecutionResult(modelCtx, result, auth, ephemeralResult)
				if target {
					cancelAttempt()
				}
				slot.close(modelCtx)
				discard(streamResult.Chunks)
				lastErr = bootstrapErr
				continue
			}
			rerr := resultErrorFromError(bootstrapErr)
			result := Result{AuthID: auth.ID, Provider: provider, Model: resultModel, Success: false, Error: rerr}
			result.RetryAfter = retryAfterFromError(bootstrapErr)
			m.recordExecutionResult(modelCtx, result, auth, ephemeralResult)
			if target {
				cancelAttempt()
			}
			slot.close(modelCtx)
			discard(streamResult.Chunks)
			return nil, newStreamBootstrapError(bootstrapErr, streamResult.Headers)
		}

		if closed && len(buffered) == 0 {
			emptyErr := &Error{Code: "empty_stream", Message: "upstream stream closed before first payload", Retryable: true}
			result := Result{AuthID: auth.ID, Provider: provider, Model: resultModel, Success: false, Error: emptyErr}
			m.recordExecutionResult(modelCtx, result, auth, ephemeralResult)
			if target {
				cancelAttempt()
			}
			slot.close(modelCtx)
			if idx < len(execModels)-1 {
				lastErr = emptyErr
				continue
			}
			return nil, newStreamBootstrapError(emptyErr, streamResult.Headers)
		}

		remaining := streamResult.Chunks
		if closed {
			closedCh := make(chan cliproxyexecutor.StreamChunk)
			close(closedCh)
			remaining = closedCh
		}
		// Home owns its lifecycle. Opt-in slots transfer their existing admission
		// to the wrapper; other streams retain legacy post-bootstrap accounting.
		if ephemeralResult {
			return m.wrapStreamResult(ctx, auth.Clone(), provider, resultModel, streamResult.Headers, buffered, remaining, aliasResult, ephemeralResult), nil
		}
		if target {
			transferred = true
			return m.wrapStreamResult(modelCtx, auth.Clone(), provider, resultModel, streamResult.Headers, buffered, remaining, aliasResult, false, func() {
				cancelAttempt()
				slot.close(modelCtx)
				m.finishPacingCall(modelCtx)
			}), nil
		}
		legacySlot, _ := m.beginAccountExecution(auth)
		transferred = true
		return m.wrapStreamResult(modelCtx, auth.Clone(), provider, resultModel, streamResult.Headers, buffered, remaining, aliasResult, ephemeralResult, func() { legacySlot.release(); m.finishPacingCall(modelCtx) }), nil
	}
	if lastErr == nil {
		lastErr = &Error{Code: "auth_not_found", Message: "no upstream model available"}
	}
	return nil, lastErr
}
