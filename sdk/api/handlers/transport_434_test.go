package handlers

import (
	"context"
	"fmt"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	auth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	exec "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	cfg "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	"net/http"
	"testing"
	"time"
)

type transport434Executor struct {
	failOnceStreamExecutor
	oldContext context.Context
	provider   string
	final      exec.StreamInterruptOwner
	late       chan exec.StreamChunk
}

func (e *transport434Executor) Identifier() string {
	if e.provider != "" {
		return e.provider
	}
	return "codex"
}
func (e *transport434Executor) ExecuteStream(ctx context.Context, _ *auth.Auth, _ exec.Request, _ exec.Options) (*exec.StreamResult, error) {
	e.calls++
	if e.calls == 1 {
		e.oldContext = ctx
		e.late = make(chan exec.StreamChunk, 2)
		e.late <- exec.StreamChunk{Err: &auth.Error{Code: "unauthorized", Message: "failed HTTP", HTTPStatus: 401}}
		return &exec.StreamResult{InterruptOwner: exec.StreamInterruptLocal, Chunks: e.late}, nil
	}
	ch := make(chan exec.StreamChunk, 1)
	ch <- exec.StreamChunk{Payload: []byte(`{"type":"response.created","response":{"id":"same-id"}}`)}
	close(ch)
	return &exec.StreamResult{InterruptOwner: e.final, Headers: http.Header{"X-Upstream-Transport": {"http"}}, Chunks: ch}, nil
}
func Test434FinalAttemptTransportAndLateDiscard(t *testing.T) {
	for _, source := range []exec.StreamInterruptOwner{exec.StreamInterruptLocal, exec.StreamInterruptNativeWebsocket} {
		t.Run(fmt.Sprint(source), func(t *testing.T) {
			e := &transport434Executor{final: source}
			m := auth.NewManager(nil, nil, nil)
			m.RegisterExecutor(e)
			model := "transport434"
			for n := 0; n < 2; n++ {
				id := fmt.Sprintf("transport434-%d-%d", source, n)
				_, err := m.Register(context.Background(), &auth.Auth{ID: id, Provider: "codex", ProxyURL: "direct", Status: auth.StatusActive})
				if err != nil {
					t.Fatal(err)
				}
				registry.GetGlobalRegistry().RegisterClient(id, "codex", []*registry.ModelInfo{{ID: model}})
				defer registry.GetGlobalRegistry().UnregisterClient(id)
			}
			h := NewBaseAPIHandlers(&cfg.SDKConfig{Streaming: cfg.StreamingConfig{BootstrapRetries: 1}}, m)
			ctx := exec.WithLocalStreamInterruptTracker(context.Background())
			data, _, errs := h.ExecuteStreamWithAuthManager(ctx, "openai", model, []byte(`{"model":"transport434"}`), "")
			for range data {
			}
			for err := range errs {
				if err != nil {
					t.Fatal(err)
				}
			}
			want := source == exec.StreamInterruptLocal
			if exec.LocalStreamInterruptible(ctx) != want {
				t.Fatalf("selected source=%v lost", source)
			}
			if e.calls != 2 {
				t.Fatalf("attempts=%d", e.calls)
			}
			// 旧 HTTP attempt 在最终 WS/HTTP 同ID响应已选中后才发送事件；只进入discard。
			drained := make(chan struct{})
			go func() {
				e.late <- exec.StreamChunk{Payload: []byte(`{"type":"response.created","response":{"id":"same-id"}}`)}
				close(e.late)
				close(drained)
			}()
			select {
			case <-drained:
			case <-time.After(time.Second):
				t.Fatal("old source stuck")
			}
			exec.SetLocalStreamInterruptible(e.oldContext, true) // late old/nested handler cannot alter the current selected owner
			next := exec.WithLocalStreamInterruptTracker(ctx)
			if exec.LocalStreamInterruptible(ctx) != want || exec.LocalStreamInterruptible(next) {
				t.Fatal("late source contaminated transport generation")
			}
		})
	}
}

func Test434PluginLocalOwnershipAndCancel(t *testing.T) {
	for _, owner := range []exec.StreamInterruptOwner{exec.StreamInterruptLocal, exec.StreamInterruptNativeWebsocket} {
		t.Run(fmt.Sprint(owner), func(t *testing.T) {
			stopped := make(chan struct{})
			host := &handlerDirectExecutorRouteHost{stream: func(ctx context.Context, _ string, _ exec.Request, _ exec.Options) (*exec.StreamResult, error) {
				chunks := make(chan exec.StreamChunk, 1)
				chunks <- exec.StreamChunk{Payload: []byte(`{"type":"response.created","response":{"id":"plugin-id"}}`)}
				go func() { <-ctx.Done(); close(chunks); close(stopped) }()
				return &exec.StreamResult{InterruptOwner: owner, Headers: http.Header{"X-Upstream-Transport": {"http"}}, Chunks: chunks}, nil
			}}
			h := NewBaseAPIHandlers(&cfg.SDKConfig{}, nil)
			h.SetModelRouterHost(host)
			ctx, cancel := context.WithCancel(exec.WithLocalStreamInterruptTracker(context.Background()))
			defer cancel()
			data, _, errs := h.streamWithPluginExecutor(ctx, "openai", "openai", "plugin-model", "plugin-model", []byte(`{}`), "", "plugin434", modelExecutionOptions{})
			select {
			case <-data:
			case err := <-errs:
				t.Fatalf("plugin error %v", err)
			case <-time.After(time.Second):
				t.Fatal("plugin blocked")
			}
			if exec.LocalStreamInterruptible(ctx) != (owner == exec.StreamInterruptLocal) {
				t.Fatal("plugin owner/header mismatch")
			}
			cancel()
			select {
			case <-stopped:
			case <-time.After(time.Second):
				t.Fatal("plugin context not cancelled")
			}
			for range data {
			}
			for range errs {
			}
		})
	}
}
func Test434NonCodexOwnershipAndLateDiscard(t *testing.T) {
	for _, source := range []exec.StreamInterruptOwner{exec.StreamInterruptLocal, exec.StreamInterruptNativeWebsocket} {
		t.Run(fmt.Sprint(source), func(t *testing.T) {
			e := &transport434Executor{final: source, provider: "http-custom"}
			m := auth.NewManager(nil, nil, nil)
			m.RegisterExecutor(e)
			model := "transport434"
			for n := 0; n < 2; n++ {
				id := fmt.Sprintf("transport434-%d-%d", source, n)
				_, err := m.Register(context.Background(), &auth.Auth{ID: id, Provider: "http-custom", ProxyURL: "direct", Status: auth.StatusActive})
				if err != nil {
					t.Fatal(err)
				}
				registry.GetGlobalRegistry().RegisterClient(id, "http-custom", []*registry.ModelInfo{{ID: model}})
				defer registry.GetGlobalRegistry().UnregisterClient(id)
			}
			h := NewBaseAPIHandlers(&cfg.SDKConfig{Streaming: cfg.StreamingConfig{BootstrapRetries: 1}}, m)
			ctx := exec.WithLocalStreamInterruptTracker(context.Background())
			data, _, errs := h.ExecuteStreamWithAuthManager(ctx, "openai", model, []byte(`{"model":"transport434"}`), "")
			for range data {
			}
			for err := range errs {
				if err != nil {
					t.Fatal(err)
				}
			}
			want := source == exec.StreamInterruptLocal
			if exec.LocalStreamInterruptible(ctx) != want {
				t.Fatalf("selected source=%v lost", source)
			}
			if e.calls != 2 {
				t.Fatalf("attempts=%d", e.calls)
			}
			// 旧 HTTP attempt 在最终 WS/HTTP 同ID响应已选中后才发送事件；只进入discard。
			drained := make(chan struct{})
			go func() {
				e.late <- exec.StreamChunk{Payload: []byte(`{"type":"response.created","response":{"id":"same-id"}}`)}
				close(e.late)
				close(drained)
			}()
			select {
			case <-drained:
			case <-time.After(time.Second):
				t.Fatal("old source stuck")
			}
			exec.SetLocalStreamInterruptible(e.oldContext, true) // late old/nested handler cannot alter the current selected owner
			next := exec.WithLocalStreamInterruptTracker(ctx)
			if exec.LocalStreamInterruptible(ctx) != want || exec.LocalStreamInterruptible(next) {
				t.Fatal("late source contaminated transport generation")
			}
		})
	}
}
