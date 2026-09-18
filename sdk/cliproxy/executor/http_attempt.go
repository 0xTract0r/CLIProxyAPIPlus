package executor

import (
	"context"
	"errors"
	"fmt"
)

// HTTPAttemptInfo describes the final prepared request without retaining its
// body, headers, credentials or session identity. Tokens use scheduler units.
type HTTPAttemptInfo struct {
	Provider, Model string
	CountOnly       bool
	EstimatedTokens int64
	EstimateKnown   bool
}

type HTTPAttemptResult struct {
	Complete        bool
	SchedulerTokens int64
	StatusCode      int
	Cause           error
}

// HTTPAttemptGate is optional and carries no auth-package dependency. Before
// must return a fresh permit for each application-level HTTP attempt.
type HTTPAttemptGate interface {
	Before(context.Context, HTTPAttemptInfo) (HTTPAttemptPermit, error)
}

type HTTPAttemptPermit interface {
	MarkSent(context.Context) error
	CancelUnsent(context.Context) error
	Finish(context.Context, HTTPAttemptResult) error
}

type httpAttemptGateKey struct{}
type httpAttemptGateValue struct{ gate HTTPAttemptGate }

func WithHTTPAttemptGate(ctx context.Context, gate HTTPAttemptGate) context.Context {
	return context.WithValue(ctx, httpAttemptGateKey{}, httpAttemptGateValue{gate: gate})
}

func HTTPAttemptGateFromContext(ctx context.Context) HTTPAttemptGate {
	if ctx == nil {
		return nil
	}
	value, _ := ctx.Value(httpAttemptGateKey{}).(httpAttemptGateValue)
	return value.gate
}

// HTTPAttemptGateError preserves local admission/accounting errors separately
// from a real earlier transport failure. Callers can errors.As through Cause;
// Previous must not be erased when a later retry cannot obtain admission.
type HTTPAttemptGateError struct {
	Cause    error
	Previous error
	Sent     bool
}

func (e *HTTPAttemptGateError) Error() string { return fmt.Sprintf("HTTP attempt gate: %v", e.Cause) }
func (e *HTTPAttemptGateError) Unwrap() error { return e.Cause }

func IsHTTPAttemptGateError(err error) bool {
	var gateErr *HTTPAttemptGateError
	return errors.As(err, &gateErr)
}
