package cliproxy

import (
	"context"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

type ordinaryManagerContextKey struct{}

type ordinaryManagerExecutor struct{}

func (ordinaryManagerExecutor) Identifier() string { return "fixture-injected" }
func (e ordinaryManagerExecutor) Execute(ctx context.Context, a *coreauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	value, _ := ctx.Value(ordinaryManagerContextKey{}).(string)
	return cliproxyexecutor.Response{Payload: []byte(value)}, nil
}
func (e ordinaryManagerExecutor) CountTokens(ctx context.Context, a *coreauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return e.Execute(ctx, a, req, opts)
}
func (e ordinaryManagerExecutor) ExecuteStream(ctx context.Context, a *coreauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	resp, err := e.Execute(ctx, a, req, opts)
	if err != nil {
		return nil, err
	}
	chunks := make(chan cliproxyexecutor.StreamChunk, 1)
	chunks <- cliproxyexecutor.StreamChunk{Payload: resp.Payload}
	close(chunks)
	return &cliproxyexecutor.StreamResult{Chunks: chunks}, nil
}
func (ordinaryManagerExecutor) Refresh(_ context.Context, a *coreauth.Auth) (*coreauth.Auth, error) {
	return a, nil
}
func (ordinaryManagerExecutor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	panic("test must not perform HTTP transport")
}

func TestOrdinaryInjectedManagerPreservesRequestContext(t *testing.T) {
	owner := coreauth.NewManager(nil, nil, nil)
	e := ordinaryManagerExecutor{}
	dir := t.TempDir()
	cfg := &config.Config{AuthDir: dir}
	s, err := NewBuilder().WithConfig(cfg).WithConfigPath(filepath.Join(dir, "config.yaml")).WithCoreAuthManager(owner).Build()
	if err != nil || s.coreManager != owner {
		t.Fatalf("external manager was replaced: %v", err)
	}
	owner.RegisterExecutor(e)
	const id = "synthetic-injected-auth"
	const model = "fixture-model"
	r := registry.GetGlobalRegistry()
	r.RegisterClient(id, e.Identifier(), []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() { r.UnregisterClient(id) })
	ctx := context.WithValue(context.Background(), ordinaryManagerContextKey{}, "fixture-context")
	if _, err := owner.Register(ctx, &coreauth.Auth{ID: id, Provider: e.Identifier()}); err != nil {
		t.Fatal(err)
	}
	req := cliproxyexecutor.Request{Model: model}
	for _, call := range []func(context.Context, []string, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error){s.coreManager.Execute, s.coreManager.ExecuteCount} {
		res, err := call(ctx, []string{e.Identifier()}, req, cliproxyexecutor.Options{})
		if err != nil || string(res.Payload) != "fixture-context" {
			t.Fatalf("context not passed: %q %v", res.Payload, err)
		}
	}
	stream, err := s.coreManager.ExecuteStream(ctx, []string{e.Identifier()}, req, cliproxyexecutor.Options{})
	if err != nil {
		t.Fatal(err)
	}
	var got string
	for c := range stream.Chunks {
		if c.Err != nil {
			t.Fatal(c.Err)
		}
		got += string(c.Payload)
	}
	if got != "fixture-context" {
		t.Fatalf("stream context got %q", got)
	}
}
