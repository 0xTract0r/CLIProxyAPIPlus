package auth

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	executor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

type ordinaryClaudeContextExecutor struct {
	models []string
	counts []string
}

func (*ordinaryClaudeContextExecutor) Identifier() string { return "claude" }
func (e *ordinaryClaudeContextExecutor) Execute(_ context.Context, _ *Auth, r executor.Request, _ executor.Options) (executor.Response, error) {
	e.models = append(e.models, r.Model)
	return executor.Response{Payload: []byte("ok")}, nil
}
func (e *ordinaryClaudeContextExecutor) ExecuteStream(_ context.Context, _ *Auth, r executor.Request, _ executor.Options) (*executor.StreamResult, error) {
	e.models = append(e.models, r.Model)
	out := make(chan executor.StreamChunk, 1)
	out <- executor.StreamChunk{Payload: []byte("ok")}
	close(out)
	return &executor.StreamResult{Chunks: out}, nil
}
func (e *ordinaryClaudeContextExecutor) CountTokens(_ context.Context, _ *Auth, r executor.Request, _ executor.Options) (executor.Response, error) {
	e.counts = append(e.counts, r.Model)
	return executor.Response{Payload: []byte("1")}, nil
}
func (*ordinaryClaudeContextExecutor) Refresh(_ context.Context, a *Auth) (*Auth, error) {
	return a, nil
}
func (*ordinaryClaudeContextExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	panic("the fixture must not send an HTTP request")
}

func ordinaryClaudeContextManager(t *testing.T, plan string, models ...string) (*Manager, *ordinaryClaudeContextExecutor) {
	t.Helper()
	m := NewManager(nil, &RoundRobinSelector{}, nil)
	e := &ordinaryClaudeContextExecutor{}
	m.RegisterExecutor(e)
	a := &Auth{ID: "fixture-" + plan, Provider: "claude", Attributes: map[string]string{AttributeAuthKind: AuthKindOAuth}, Metadata: map[string]any{"plan_type": plan}}
	r := registry.GetGlobalRegistry()
	mm := make([]*registry.ModelInfo, 0, len(models))
	for _, name := range models {
		mm = append(mm, &registry.ModelInfo{ID: name})
	}
	r.RegisterClient(a.ID, "claude", mm)
	t.Cleanup(func() { r.UnregisterClient(a.ID) })
	if _, err := m.Register(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	return m, e
}

func TestOrdinaryClaudeLongContextNeverRewritesRequestedModel(t *testing.T) {
	const sonnet = "claude-sonnet-4-6"
	const native = "claude-sonnet-5"
	m, e := ordinaryClaudeContextManager(t, "pro", sonnet, native, "fixture-context-alias")
	m.SetConfig(&config.Config{Claude: config.ClaudeConfig{SonnetLongContextPolicy: config.ClaudeSonnetLongContextPolicyRouteToOpus1M}})
	ctx := context.Background()
	p := []string{"claude"}
	long := []byte(`{"messages":[{"role":"user","content":"` + strings.Repeat("x", 810000) + `"}]}`)
	req := executor.Request{Model: sonnet, Payload: long}
	if _, err := m.Execute(ctx, p, req, executor.Options{}); statusCodeFromError(err) != 400 {
		t.Fatalf("execute: %v", err)
	}
	if _, err := m.ExecuteStream(ctx, p, req, executor.Options{}); statusCodeFromError(err) != 400 {
		t.Fatalf("stream: %v", err)
	}
	m.SetOAuthModelAlias(map[string][]config.OAuthModelAlias{"claude": {{Name: sonnet, Alias: "fixture-context-alias"}}})
	aliased := executor.Request{Model: "fixture-context-alias", Payload: long}
	if _, err := m.Execute(ctx, p, aliased, executor.Options{}); statusCodeFromError(err) != 400 {
		t.Fatalf("aliased execute: %v", err)
	}
	if _, err := m.ExecuteStream(ctx, p, aliased, executor.Options{}); statusCodeFromError(err) != 400 {
		t.Fatalf("aliased stream: %v", err)
	}
	if len(e.models) != 0 {
		t.Fatalf("oversize request sent upstream: %v", e.models)
	}
	if _, err := m.ExecuteCount(ctx, p, req, executor.Options{}); err != nil || len(e.counts) != 1 || e.counts[0] != sonnet {
		t.Fatalf("count exemption: %v %v", err, e.counts)
	}
	if _, err := m.Execute(ctx, p, executor.Request{Model: native, Payload: long}, executor.Options{}); err != nil || len(e.models) != 1 || e.models[0] != native {
		t.Fatalf("native 1M: %v %v", err, e.models)
	}
	huge := []byte(`{"messages":[{"role":"user","content":"` + strings.Repeat("x", 4000004) + `"}]}`)
	if _, err := m.Execute(ctx, p, executor.Request{Model: native, Payload: huge}, executor.Options{}); statusCodeFromError(err) != 413 || len(e.models) != 1 {
		t.Fatalf("absolute cap: %v %v", err, e.models)
	}
}

func TestOrdinaryClaudeAliasedOneMillionRequiresSubscription(t *testing.T) {
	const opus = "claude-opus-4-8"
	const alias = "fixture-reasoner"
	ctx := context.Background()
	p := []string{"claude"}
	opts := executor.Options{Headers: http.Header{"Anthropic-Beta": {"context-1m-2025-08-07"}}}
	for _, plan := range []string{"pro", "max"} {
		t.Run(plan, func(t *testing.T) {
			m, e := ordinaryClaudeContextManager(t, plan, opus, alias)
			m.SetOAuthModelAlias(map[string][]config.OAuthModelAlias{"claude": {{Name: opus, Alias: alias}}})
			_, err := m.Execute(ctx, p, executor.Request{Model: alias}, opts)
			stream, errStream := m.ExecuteStream(ctx, p, executor.Request{Model: alias}, opts)
			if plan == "pro" {
				if err == nil || errStream == nil || len(e.models) != 0 {
					t.Fatalf("unentitled alias reached executor: %v %v %v", err, errStream, e.models)
				}
				if _, err := m.Execute(ctx, p, executor.Request{Model: alias}, executor.Options{}); err != nil || len(e.models) != 1 || e.models[0] != opus {
					t.Fatalf("ordinary Opus alias: %v %v", err, e.models)
				}
				return
			}
			if err != nil || errStream != nil || len(e.models) != 2 || e.models[0] != opus || e.models[1] != opus {
				t.Fatalf("entitled ordinary alias: %v %v %v", err, errStream, e.models)
			}
			for range stream.Chunks {
			}
		})
	}
}
