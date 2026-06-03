package auth

import (
	"context"
	"io"
	"net/http"
	"reflect"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func TestExtractCustomHeadersFromMetadata(t *testing.T) {
	meta := map[string]any{
		"headers": map[string]any{
			" X-Test ": " value ",
			"":         "ignored",
			"X-Empty":  "   ",
			"X-Num":    float64(1),
		},
	}

	got := ExtractCustomHeadersFromMetadata(meta)
	want := map[string]string{"X-Test": "value"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ExtractCustomHeadersFromMetadata() = %#v, want %#v", got, want)
	}
}

func TestApplyCustomHeadersFromMetadata(t *testing.T) {
	auth := &Auth{
		Metadata: map[string]any{
			"headers": map[string]string{
				"X-Test":  "new",
				"X-Empty": "   ",
			},
		},
		Attributes: map[string]string{
			"header:X-Test": "old",
			"keep":          "1",
		},
	}

	ApplyCustomHeadersFromMetadata(auth)

	if got := auth.Attributes["header:X-Test"]; got != "new" {
		t.Fatalf("header:X-Test = %q, want %q", got, "new")
	}
	if _, ok := auth.Attributes["header:X-Empty"]; ok {
		t.Fatalf("expected header:X-Empty to be absent, got %#v", auth.Attributes["header:X-Empty"])
	}
	if got := auth.Attributes["keep"]; got != "1" {
		t.Fatalf("keep = %q, want %q", got, "1")
	}
}

func TestApplyRuntimeFieldsFromMetadataRestoresProxyURL(t *testing.T) {
	auth := &Auth{
		Metadata: map[string]any{
			"proxy_url": " socks5://proxy.example:1080 ",
		},
	}

	ApplyRuntimeFieldsFromMetadata(auth)

	if got := auth.ProxyURL; got != "socks5://proxy.example:1080" {
		t.Fatalf("ProxyURL = %q, want %q", got, "socks5://proxy.example:1080")
	}
}

func TestApplyRuntimeFieldsFromMetadataKeepsExplicitProxyURL(t *testing.T) {
	auth := &Auth{
		ProxyURL: "http://explicit.example:8080",
		Metadata: map[string]any{
			"proxy_url": "socks5://metadata.example:1080",
		},
	}

	ApplyRuntimeFieldsFromMetadata(auth)

	if got := auth.ProxyURL; got != "http://explicit.example:8080" {
		t.Fatalf("ProxyURL = %q, want explicit proxy", got)
	}
}

type runtimeFieldCaptureExecutor struct {
	captured *Auth
}

func (e *runtimeFieldCaptureExecutor) Identifier() string { return "codex" }

func (e *runtimeFieldCaptureExecutor) Execute(_ context.Context, auth *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	e.captured = auth
	return cliproxyexecutor.Response{Payload: []byte(`{"choices":[{"message":{"content":"OK"}}]}`)}, nil
}

func (e *runtimeFieldCaptureExecutor) ExecuteStream(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	return nil, nil
}

func (e *runtimeFieldCaptureExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) {
	return auth, nil
}

func (e *runtimeFieldCaptureExecutor) CountTokens(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}

func (e *runtimeFieldCaptureExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, nil
}

func TestManagerRegisterHydratesRuntimeFieldsBeforeExecute(t *testing.T) {
	authID := "codex-runtime-fields-test.json"
	modelID := "runtime-fields-test-model"
	registry.GetGlobalRegistry().RegisterClient(authID, "codex", []*registry.ModelInfo{{ID: modelID, Object: "model", Type: "codex"}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(authID) })

	exec := &runtimeFieldCaptureExecutor{}
	manager := NewManager(nil, nil, nil)
	manager.RegisterExecutor(exec)
	if _, err := manager.Register(context.Background(), &Auth{
		ID:       authID,
		Provider: "codex",
		Metadata: map[string]any{
			"type":      "codex",
			"proxy_url": "socks5://proxy.example:1080",
			"headers": map[string]any{
				"User-Agent": "managed-ua/1.0",
			},
		},
	}); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	_, err := manager.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: modelID}, cliproxyexecutor.Options{})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if exec.captured == nil {
		t.Fatal("executor was not called")
	}
	if got := exec.captured.ProxyURL; got != "socks5://proxy.example:1080" {
		t.Fatalf("captured ProxyURL = %q, want account proxy", got)
	}
	if got := exec.captured.Attributes["header:User-Agent"]; got != "managed-ua/1.0" {
		t.Fatalf("captured header:User-Agent = %q, want managed header", got)
	}
}

type roundTripperProviderFunc func(*Auth) http.RoundTripper

func (f roundTripperProviderFunc) RoundTripperFor(auth *Auth) http.RoundTripper {
	return f(auth)
}

type contextCaptureExecutor struct {
	runtimeFieldCaptureExecutor
	ctxRoundTripper    http.RoundTripper
	stringRoundTripper http.RoundTripper
}

func (e *contextCaptureExecutor) HttpRequest(ctx context.Context, _ *Auth, _ *http.Request) (*http.Response, error) {
	if rt, ok := ctx.Value(roundTripperContextKey{}).(http.RoundTripper); ok {
		e.ctxRoundTripper = rt
	}
	if rt, ok := ctx.Value("cliproxy.roundtripper").(http.RoundTripper); ok {
		e.stringRoundTripper = rt
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(http.NoBody),
	}, nil
}

func TestManagerHttpRequestInjectsPerAuthRoundTripper(t *testing.T) {
	sentinel := http.DefaultTransport
	exec := &contextCaptureExecutor{}
	manager := NewManager(nil, nil, nil)
	manager.RegisterExecutor(exec)
	manager.SetRoundTripperProvider(roundTripperProviderFunc(func(auth *Auth) http.RoundTripper {
		if auth == nil || auth.ID != "codex-http-request-rt-test" {
			t.Fatalf("RoundTripperFor auth ID = %v, want codex-http-request-rt-test", auth)
		}
		return sentinel
	}))

	req, err := http.NewRequest(http.MethodGet, "https://chatgpt.com/backend-api/codex/health", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := manager.HttpRequest(context.Background(), &Auth{
		ID:       "codex-http-request-rt-test",
		Provider: "codex",
	}, req)
	if err != nil {
		t.Fatalf("http request: %v", err)
	}
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if exec.ctxRoundTripper != sentinel {
		t.Fatalf("typed context RoundTripper was not injected")
	}
	if exec.stringRoundTripper != sentinel {
		t.Fatalf("string context RoundTripper was not injected")
	}
}
