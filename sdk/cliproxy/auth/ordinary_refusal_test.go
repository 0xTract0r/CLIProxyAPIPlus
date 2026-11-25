package auth

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

type ordinaryRefusalFixture struct {
	status int
	hint   time.Duration
}

func (e ordinaryRefusalFixture) Error() string              { return "synthetic provider refusal" }
func (e ordinaryRefusalFixture) StatusCode() int            { return e.status }
func (e ordinaryRefusalFixture) RetryAfter() *time.Duration { return &e.hint }
func (e ordinaryRefusalFixture) Headers() http.Header       { return http.Header{"Retry-After": {"75"}} }

func TestOrdinarySelectedCredentialRefusalIsTerminal(t *testing.T) {
	for _, operation := range []string{"execute", "count", "stream"} {
		for _, status := range []int{400, 401, 403, 429, 503} {
			t.Run(operation+"/"+strconv.Itoa(status), func(t *testing.T) {
				m, e := issue7017Setup(t, 2, time.Second, ordinaryRefusalFixture{status, 75 * time.Second}, "a-fixture", "z-fixture")
				ctx := context.Background()
				providers := []string{e.Identifier()}
				request := cliproxyexecutor.Request{Model: "gpt-6.1-sol"}
				var err error
				switch operation {
				case "execute":
					_, err = m.Execute(ctx, providers, request, cliproxyexecutor.Options{})
				case "count":
					_, err = m.ExecuteCount(ctx, providers, request, cliproxyexecutor.Options{})
				case "stream":
					_, err = m.ExecuteStream(ctx, providers, request, cliproxyexecutor.Options{})
				}
				if statusCodeFromError(err) != status || SafeResponseHeaders(err).Get("Retry-After") != "75" || len(e.callIDs()) != 1 || e.callIDs()[0] != "a-fixture" {
					t.Fatalf("wanted one attempt with HTTP %d and original Retry-After, got err=%v calls=%v", status, err, e.callIDs())
				}
			})
		}
	}
}

type ordinarySuccessFixture struct {
	*issue7017Executor
}

func (e ordinarySuccessFixture) Execute(ctx context.Context, a *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	if err := ctx.Err(); err != nil {
		return cliproxyexecutor.Response{}, err
	}
	e.record(a)
	return cliproxyexecutor.Response{Payload: []byte("ok")}, nil
}

func (e ordinarySuccessFixture) CountTokens(ctx context.Context, a *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return e.Execute(ctx, a, req, opts)
}

func (e ordinarySuccessFixture) ExecuteStream(ctx context.Context, a *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	e.record(a)
	chunks := make(chan cliproxyexecutor.StreamChunk, 1)
	chunks <- cliproxyexecutor.StreamChunk{Payload: []byte("ok")}
	close(chunks)
	return &cliproxyexecutor.StreamResult{Chunks: chunks}, nil
}

func TestOrdinarySelectedCredentialSuccessAndCanceledStream(t *testing.T) {
	m, e := issue7017Setup(t, 2, time.Second, nil, "fixture")
	m.RegisterExecutor(ordinarySuccessFixture{e})
	ctx := context.Background()
	providers := []string{e.Identifier()}
	req := cliproxyexecutor.Request{Model: "gpt-6.1-sol"}
	opts := cliproxyexecutor.Options{}
	if resp, err := m.Execute(ctx, providers, req, opts); err != nil || string(resp.Payload) != "ok" {
		t.Fatalf("execute = %q, %v", resp.Payload, err)
	}
	if resp, err := m.ExecuteCount(ctx, providers, req, opts); err != nil || string(resp.Payload) != "ok" {
		t.Fatalf("count = %q, %v", resp.Payload, err)
	}
	stream, err := m.ExecuteStream(ctx, providers, req, opts)
	if err != nil {
		t.Fatal(err)
	}
	var payload string
	for c := range stream.Chunks {
		if c.Err != nil {
			t.Fatal(c.Err)
		}
		payload += string(c.Payload)
	}
	if payload != "ok" {
		t.Fatalf("stream = %q", payload)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := m.ExecuteStream(canceled, providers, req, opts); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled stream = %v", err)
	}
	if calls := e.callIDs(); len(calls) != 3 {
		t.Fatalf("calls = %v", calls)
	}
}

func TestOrdinaryRegistryReconciliationCannotClearTerminalState(t *testing.T) {
	now := time.Now()
	for _, candidate := range []*Auth{
		{AutoQuarantined: true, Unavailable: true, Status: StatusError, StatusMessage: "quarantined"},
		{Metadata: map[string]any{"reauth_required": true}, Unavailable: true, Status: StatusError, StatusMessage: "reauth_required"},
	} {
		prior := candidate.StatusMessage
		reconcileAggregatedAuthState(candidate, now)
		if !candidate.Unavailable || candidate.Status != StatusError || candidate.StatusMessage != prior {
			t.Fatalf("model reconciliation revived terminal status %q: %+v", prior, candidate)
		}
	}
}
