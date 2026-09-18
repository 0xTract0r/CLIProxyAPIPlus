package helps

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

// ClaudeHTTPAttempt owns a single permit through response EOF/close/cancellation.
// Claiming decoded observation changes only the observation source, never the
// raw-close fallback that protects decompression and decoration failure paths.
type ClaudeHTTPAttempt struct {
	ctx                    context.Context
	permit                 cliproxyexecutor.HTTPAttemptPermit
	mu                     sync.Mutex
	usage                  claudeAttemptUsage
	claimed, encoded, done bool
	status                 int
	stop                   func() bool
	source                 io.ReadCloser
	finishOnce, closeOnce  sync.Once
	finishErr, closeErr    error
}

func BeginClaudeHTTPAttempt(ctx context.Context, req *http.Request) (*ClaudeHTTPAttempt, error) {
	gate := cliproxyexecutor.HTTPAttemptGateFromContext(ctx)
	if gate == nil {
		return nil, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, &cliproxyexecutor.HTTPAttemptGateError{Cause: err}
	}
	info := claudeAttemptInfo(req)
	permit, err := gate.Before(ctx, info)
	if err != nil {
		if permit != nil {
			err = errors.Join(err, permit.CancelUnsent(context.WithoutCancel(ctx)))
		}
		return nil, &cliproxyexecutor.HTTPAttemptGateError{Cause: err}
	}
	if permit == nil {
		return nil, &cliproxyexecutor.HTTPAttemptGateError{Cause: errors.New("HTTP attempt gate returned no permit")}
	}
	if err = ctx.Err(); err == nil {
		err = permit.MarkSent(ctx)
	}
	if err != nil {
		err = errors.Join(err, permit.CancelUnsent(context.WithoutCancel(ctx)))
		return nil, &cliproxyexecutor.HTTPAttemptGateError{Cause: err}
	}
	a := &ClaudeHTTPAttempt{ctx: ctx, permit: permit, usage: claudeAttemptUsage{count: info.CountOnly}}
	a.mu.Lock()
	a.stop = context.AfterFunc(ctx, func() { _ = a.closeSource(); _ = a.finish(ctx.Err(), false) })
	a.mu.Unlock()
	return a, nil
}

func (a *ClaudeHTTPAttempt) TransportFailed(cause error) error {
	if a == nil {
		return nil
	}
	return a.finish(cause, false)
}

func (a *ClaudeHTTPAttempt) WrapResponse(resp *http.Response) {
	if a == nil {
		return
	}
	if resp == nil || resp.Body == nil {
		_ = a.finish(io.ErrUnexpectedEOF, false)
		return
	}
	a.mu.Lock()
	a.status = resp.StatusCode
	a.source = resp.Body
	a.encoded = strings.TrimSpace(resp.Header.Get("Content-Encoding")) != ""
	a.usage.stream = strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") && !a.usage.count
	done := a.done || a.ctx.Err() != nil
	a.mu.Unlock()
	resp.Body = &claudeAttemptBody{source: resp.Body, attempt: a, raw: true}
	if done {
		_ = a.closeSource()
	}
}

// ClaimClaudeAttemptResponse must precede decompression, which may itself read
// the raw body. Nil preserves the entire legacy response lifecycle.
func ClaimClaudeAttemptResponse(resp *http.Response) *ClaudeHTTPAttempt {
	if resp == nil {
		return nil
	}
	body, ok := resp.Body.(*claudeAttemptBody)
	if !ok {
		return nil
	}
	a := body.attempt
	a.mu.Lock()
	a.claimed = true
	a.mu.Unlock()
	return a
}

func (a *ClaudeHTTPAttempt) ObserveDecoded(body io.ReadCloser) io.ReadCloser {
	if a == nil {
		return body
	}
	return &claudeAttemptBody{source: body, attempt: a}
}

// Abandon is also safe after successful EOF. It closes any response whose
// normal decoder/stream lifecycle was never successfully transferred.
func (a *ClaudeHTTPAttempt) Abandon() {
	if a == nil {
		return
	}
	_ = a.finish(io.ErrUnexpectedEOF, false)
	_ = a.closeSource()
}

func (a *ClaudeHTTPAttempt) closeSource() error {
	a.mu.Lock()
	source := a.source
	a.mu.Unlock()
	// Cancellation can precede client.Do's response. Do not spend closeOnce
	// until a concrete body exists; WrapResponse will close a late response.
	if source == nil {
		return nil
	}
	a.closeOnce.Do(func() { a.closeErr = source.Close() })
	return a.closeErr
}

func (a *ClaudeHTTPAttempt) observe(data []byte, raw bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.done || (raw && (a.claimed || a.encoded)) {
		return
	}
	a.usage.feed(data)
}

func (a *ClaudeHTTPAttempt) finish(cause error, eof bool) error {
	a.finishOnce.Do(func() {
		a.mu.Lock()
		if err := a.ctx.Err(); err != nil {
			cause = err
		}
		complete, tokens := a.usage.result(eof)
		complete = complete && cause == nil && a.status >= 200 && a.status < 300
		a.done = true
		stop, status := a.stop, a.status
		a.mu.Unlock()
		if stop != nil {
			stop()
		}
		err := a.permit.Finish(context.WithoutCancel(a.ctx), cliproxyexecutor.HTTPAttemptResult{Complete: complete, SchedulerTokens: tokens, StatusCode: status, Cause: cause})
		if err != nil {
			a.mu.Lock()
			a.finishErr = &cliproxyexecutor.HTTPAttemptGateError{Cause: err, Previous: cause, Sent: true}
			a.mu.Unlock()
		}
	})
	return a.SettlementError()
}

// SettlementError is diagnostic only. The callback owner must latch storage
// failures and refuse later admission; a served response must not become a new
// client retry merely because post-send accounting failed.
func (a *ClaudeHTTPAttempt) SettlementError() error {
	if a == nil {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.finishErr
}

type claudeAttemptBody struct {
	source  io.ReadCloser
	attempt *ClaudeHTTPAttempt
	raw     bool
}

func (b *claudeAttemptBody) Read(p []byte) (int, error) {
	n, err := b.source.Read(p)
	if n > 0 {
		b.attempt.observe(p[:n], b.raw)
	}
	if err != nil {
		b.attempt.mu.Lock()
		claimed := b.attempt.claimed
		b.attempt.mu.Unlock()
		if !(b.raw && claimed && errors.Is(err, io.EOF)) {
			cause := err
			eof := errors.Is(err, io.EOF)
			if eof {
				cause = nil
			}
			_ = b.attempt.finish(cause, eof)
		}
	}
	return n, err
}

func (b *claudeAttemptBody) Close() error {
	_ = b.attempt.finish(nil, false)
	var closeErr error
	if b.raw {
		closeErr = b.attempt.closeSource()
	} else {
		closeErr = b.source.Close()
	}
	return closeErr
}
