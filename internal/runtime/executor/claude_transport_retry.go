package executor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

var claudeProxyTransportRetryBackoffs = []time.Duration{
	200 * time.Millisecond,
	800 * time.Millisecond,
}

func doClaudeHTTPWithTransportRetry(ctx context.Context, client *http.Client, req *http.Request) (*http.Response, error) {
	if client == nil {
		client = http.DefaultClient
	}
	if req == nil {
		return nil, fmt.Errorf("claude executor: request is nil")
	}
	if ctx == nil {
		ctx = req.Context()
	}

	var lastErr error
	for attempt := 0; ; attempt++ {
		attemptReq := req
		if attempt > 0 {
			cloned, errClone := cloneClaudeRequestForRetry(ctx, req)
			if errClone != nil {
				return nil, lastErr
			}
			attemptReq = cloned
		}

		resp, err := client.Do(attemptReq)
		if err == nil {
			return resp, nil
		}
		lastErr = err
		if !shouldRetryClaudeTransportError(ctx, req, err, attempt) {
			return nil, err
		}
		wait := claudeProxyTransportRetryBackoffs[attempt]
		logWithRequestID(ctx).Debugf("claude executor: retrying transient proxy transport error after %s: %v", wait, err)
		if errWait := waitBeforeClaudeTransportRetry(ctx, wait); errWait != nil {
			return nil, errWait
		}
	}
}

func cloneClaudeRequestForRetry(ctx context.Context, req *http.Request) (*http.Request, error) {
	cloned := req.Clone(ctx)
	if req.GetBody != nil {
		body, err := req.GetBody()
		if err != nil {
			return nil, err
		}
		cloned.Body = body
		return cloned, nil
	}
	if req.Body == nil || req.Body == http.NoBody {
		cloned.Body = req.Body
		return cloned, nil
	}
	return nil, fmt.Errorf("claude executor: request body cannot be replayed")
}

func shouldRetryClaudeTransportError(ctx context.Context, req *http.Request, err error, attempt int) bool {
	if err == nil || attempt < 0 || attempt >= len(claudeProxyTransportRetryBackoffs) {
		return false
	}
	if ctx != nil && ctx.Err() != nil {
		return false
	}
	if req != nil && req.Body != nil && req.Body != http.NoBody && req.GetBody == nil {
		return false
	}
	return isRetryableClaudeProxyTransportError(err)
}

func isRetryableClaudeProxyTransportError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, io.EOF) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	msg := strings.ToLower(err.Error())
	retryableFragments := []string{
		"socks connect",
		"proxyconnect",
		"connection not allowed by ruleset",
		"connection reset",
		"connection refused",
		"connect: operation timed out",
		"i/o timeout",
		"timeout awaiting response headers",
		"temporary failure",
		"unexpected eof",
	}
	for _, fragment := range retryableFragments {
		if strings.Contains(msg, fragment) {
			return true
		}
	}
	return false
}

func waitBeforeClaudeTransportRetry(ctx context.Context, wait time.Duration) error {
	if wait <= 0 {
		return nil
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
