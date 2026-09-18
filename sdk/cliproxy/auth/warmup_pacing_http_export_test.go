package auth

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

// CheckWarmupPacingHTTPPoolReselect is exported only in the test build so the
// external test can exercise the real HTTP helper without an auth/import cycle.
// The two-model pool is a conductor fixture, not a standard Claude model mapping.
func CheckWarmupPacingHTTPPoolReselect(t *testing.T, begin func(context.Context, *http.Request, func()) (func(error) error, error)) {
	t.Helper()
	m, s, a, e, now, model := pacingExecutionFixture(t)
	*now = now.Add(time.Hour)
	opts := servingOptions("wrapped-pool-root", "", "system", "Review retry cleanup.")
	ctx, _ := virtualWarmupWait(now)
	if _, err := s.Pick(ctx, "claude", model, opts, []*Auth{a}); err != nil {
		t.Fatal(err)
	}
	firstErr := &Error{Code: "synthetic", Message: "synthetic transport failure", HTTPStatus: 503}
	var models []string
	var markedChanged bool
	e.stream = func(ctx context.Context, current *Auth, req cliproxyexecutor.Request, _ cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
		models = append(models, req.Model)
		raw, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://localhost/v1/messages", strings.NewReader(`{"model":"synthetic","max_tokens":100,"messages":[{"role":"user","content":"Review retry cleanup."}]}`))
		if err != nil {
			t.Fatal(err)
		}
		finish, err := begin(ctx, raw, func() {
			if req.Model == "up-b" {
				markedChanged = true
				updated := current.Clone()
				updated.ProxyURL = "http://changed-proxy.invalid:8080"
				if _, err := m.Update(ctx, updated); err != nil {
					t.Fatal(err)
				}
			}
		})
		if err != nil {
			return nil, err
		}
		if err = finish(firstErr); err != nil {
			t.Fatal(err)
		}
		*now = now.Add(21 * time.Second)
		return nil, firstErr
	}
	_, err := m.executeStreamWithModelPool(ctx, e, a, "claude", cliproxyexecutor.Request{Model: model, Payload: opts.OriginalRequest}, opts, model, "", []string{"up-a", "up-b"}, true, OAuthModelAliasResult{}, true, false)
	var local *warmupAdmissionReselect
	if !markedChanged || !errors.As(err, &local) || local.previous != firstErr || len(models) != 2 || models[1] != "up-b" {
		t.Fatalf("real helper wrapper lost pool continuation: marked=%v models=%v err=%v", markedChanged, models, err)
	}
	if !warmupCanResumeCredential(ctx, map[string]struct{}{a.ID: {}}) {
		t.Fatal("real earlier attempt consumed resumed credential allowance")
	}
	remaining, resumeErr := warmupResumeModels(ctx, a.ID, []string{"up-a", "up-b"})
	if resumeErr != nil || len(remaining) != 1 || remaining[0] != "up-b" {
		t.Fatal("helper-wrapped refusal replayed first model", remaining, resumeErr)
	}
	d, peekErr := m.pacing.pacer.PeekConfigured(a.ID, WarmupPacingRequest{CountOnly: true, EstimateKnown: true})
	if peekErr != nil || d.DayRequests != 1 || d.InFlight != 0 || d.PendingTokens != 0 {
		t.Fatalf("MarkSent rejection leaked reservation: %+v %v", d, peekErr)
	}
}
