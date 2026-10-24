package openai

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cache"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	runtimeexecutor "github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/httptransport"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"github.com/tidwall/sjson"
)

const issue6332ReviewModel = "gemini-3.7-flash"

func issue6332ToolFrame() string {
	return `data: {"response":{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"id":"native-call","name":"lookup","args":{"key":"value"}},"thoughtSignature":"` + strings.Repeat("s", 128) + `"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":11,"candidatesTokenCount":22,"totalTokenCount":33}}}` + "\n\n"
}

func issue6332ReviewBase(t *testing.T, url string) (*handlers.BaseAPIHandler, *cliproxyauth.Manager) {
	t.Helper()
	manager := cliproxyauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(runtimeexecutor.NewAntigravityExecutor(&config.Config{}))
	auth := &cliproxyauth.Auth{ID: t.Name(), Provider: "antigravity", Status: cliproxyauth.StatusActive, ProxyURL: "direct", Metadata: map[string]any{"access_token": "token", "expired": time.Now().Add(time.Hour).Format(time.RFC3339), "project_id": "project"}, Attributes: map[string]string{"base_url": url}}
	if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
		t.Fatal(errRegister)
	}
	registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: issue6332ReviewModel}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
	return handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager), manager
}

func issue6332ReviewBody(t *testing.T) []byte {
	return []byte(`{"model":"` + issue6332ReviewModel + `","stream":true,"metadata":{"session_id":"` + t.Name() + `"},"input":[{"role":"user","content":"hello"}]}`)
}

func issue6332ReviewNested(ctx context.Context, base *handlers.BaseAPIHandler, body []byte) error {
	stream, errExecute := base.ExecuteModelStream(ctx, handlers.ModelExecutionRequest{EntryProtocol: "openai-response", ExitProtocol: "openai-response", Model: issue6332ReviewModel, Stream: true, Body: body})
	if errExecute != nil {
		return errExecute.Error
	}
	for chunk := range stream.Chunks {
		if chunk.Err != nil {
			return chunk.Err
		}
	}
	return ctx.Err()
}

type issue6332PeerHost struct {
	handlers.PluginInterceptorHost
	base     *handlers.BaseAPIHandler
	arrived  sync.WaitGroup
	finished sync.WaitGroup
	results  chan error
}

func (*issue6332PeerHost) HasRequestInterceptors() bool                                 { return false }
func (*issue6332PeerHost) HasStreamInterceptors() bool                                  { return true }
func (*issue6332PeerHost) CompleteRequest(context.Context, pluginapi.RequestCompletion) {}
func (h *issue6332PeerHost) InterceptStreamChunk(ctx context.Context, req pluginapi.StreamChunkInterceptRequest) pluginapi.StreamChunkInterceptResponse {
	if !strings.Contains(string(req.Body), "response.completed") || req.Metadata["source"] == "plugin_host_model_callback" {
		return pluginapi.StreamChunkInterceptResponse{}
	}
	h.arrived.Done()
	h.arrived.Wait()
	// Both independent terminal interceptors are now active, before either delivery.
	nested, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	h.results <- issue6332ReviewNested(nested, h.base, req.RequestBody)
	h.finished.Done()
	h.finished.Wait()
	return pluginapi.StreamChunkInterceptResponse{}
}

func TestIssue6332Review7ConcurrentTerminalInterceptors(t *testing.T) {
	var requests atomic.Int32
	var initial sync.WaitGroup
	initial.Add(2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1) <= 2 {
			// Both outer requests must pass their initial replay read before responding.
			initial.Done()
			initial.Wait()
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, issue6332ToolFrame())
	}))
	defer server.Close()
	base, _ := issue6332ReviewBase(t, server.URL)
	host := &issue6332PeerHost{base: base, results: make(chan error, 2)}
	host.arrived.Add(2)
	host.finished.Add(2)
	base.SetPluginHost(host)
	router := gin.New()
	router.POST("/v1/responses", NewOpenAIResponsesAPIHandler(base).Responses)
	var done sync.WaitGroup
	for range 2 {
		done.Add(1)
		go func() {
			defer done.Done()
			writer := httptest.NewRecorder()
			router.ServeHTTP(writer, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(string(issue6332ReviewBody(t)))))
			if !strings.Contains(writer.Body.String(), "response.completed") {
				t.Errorf("outer completion missing: %s", writer.Body.String())
			}
		}()
	}
	done.Wait()
	for range 2 {
		if errNested := <-host.results; errNested != nil {
			t.Errorf("same-session peer terminal interceptor deadlocked: %v", errNested)
		}
	}
}

type issue6332GatedBody struct {
	io.ReadCloser
	release    <-chan struct{}
	eofReached chan struct{}
	closed     chan struct{}
	eofOnce    sync.Once
	closeOnce  sync.Once
}

func (b *issue6332GatedBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	// net/http may return the last frame together with EOF. Deliver those bytes
	// before gating the subsequent zero-byte EOF, as strings.Reader did.
	if n > 0 && err == io.EOF {
		return n, nil
	}
	if err == io.EOF {
		b.eofOnce.Do(func() { close(b.eofReached) })
		<-b.release
	}
	return n, err
}
func (b *issue6332GatedBody) Close() error {
	err := b.ReadCloser.Close()
	b.closeOnce.Do(func() { close(b.closed) })
	return err
}

type issue6332LifecycleTransport struct {
	calls      atomic.Int32
	release    <-chan struct{}
	eofReached chan struct{}
	closed     chan struct{}
	requests   chan []byte
	firstFrame string
}

type issue6332LifecycleObservedTransport struct {
	next     http.RoundTripper
	observer *issue6332LifecycleTransport
}

func (rt *issue6332LifecycleTransport) decorate(next http.RoundTripper) http.RoundTripper {
	return &issue6332LifecycleObservedTransport{next: next, observer: rt}
}
func (rt *issue6332LifecycleObservedTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	// Delegate the exact transport chosen for this credential/direct route.
	response, err := rt.next.RoundTrip(r)
	if err != nil {
		return response, err
	}
	if rt.observer.calls.Add(1) == 1 {
		response.Body = &issue6332GatedBody{ReadCloser: response.Body, release: rt.observer.release, eofReached: rt.observer.eofReached, closed: rt.observer.closed}
	}
	return response, nil
}

type issue6332CallbackHost struct {
	handlers.PluginInterceptorHost
	base   *handlers.BaseAPIHandler
	body   []byte
	result chan error
}

func (*issue6332CallbackHost) HasRequestInterceptors() bool { return false }
func (*issue6332CallbackHost) HasStreamInterceptors() bool  { return false }
func (h *issue6332CallbackHost) CompleteRequest(ctx context.Context, c pluginapi.RequestCompletion) {
	if c.Metadata["source"] == "plugin_host_model_callback" {
		return
	}
	if c.Outcome != pluginapi.RequestCompletionSucceeded {
		h.result <- fmt.Errorf("outer lifecycle failed: %+v", c)
		return
	}
	// A lifecycle callback starts a new turn without waiting for usage or upstream cleanup.
	nested, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	h.result <- issue6332ReviewNested(nested, h.base, h.body)
}

func TestIssue6332Review7LifecycleNextTurn(t *testing.T) {
	for _, split := range []bool{false, true} {
		t.Run(fmt.Sprintf("split_signature_%v", split), func(t *testing.T) {
			testIssue6332LifecycleNextTurn(t, split)
		})
	}
}

func testIssue6332LifecycleNextTurn(t *testing.T, split bool) {
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	rt := &issue6332LifecycleTransport{release: release, eofReached: make(chan struct{}), closed: make(chan struct{}), requests: make(chan []byte, 2)}
	rt.firstFrame = issue6332ToolFrame()
	if split {
		// A finishReason alone is not the replay boundary: signature and usage follow.
		rt.firstFrame = `data: {"response":{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"id":"native-call","name":"lookup","args":{"key":"value"}}}]},"finishReason":"STOP"}]}}` + "\n\n" +
			`data: {"response":{"candidates":[{"content":{"parts":[{"text":"","thoughtSignature":"` + strings.Repeat("s", 128) + `"}]}}]}}` + "\n\n" +
			`data: {"response":{"usageMetadata":{"promptTokenCount":11,"candidatesTokenCount":22,"totalTokenCount":33}}}` + "\n\n"
	}
	var peerCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read peer request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		rt.requests <- body
		frame := issue6332ToolFrame()
		if peerCalls.Add(1) == 1 {
			frame = rt.firstFrame
		}
		w.Header().Set("Content-Type", "text/event-stream")
		if _, err := io.WriteString(w, frame); err != nil {
			t.Errorf("write peer stream: %v", err)
		}
	}))
	defer server.Close()
	base, _ := issue6332ReviewBase(t, server.URL)
	body := issue6332ReviewBody(t)
	second, _ := sjson.SetBytes(body, "input.-1", map[string]any{"type": "function_call", "call_id": "native-call", "name": "lookup", "arguments": `{"key":"value"}`})
	second, _ = sjson.SetBytes(second, "input.-1", map[string]any{"type": "function_call_output", "call_id": "native-call", "output": "found"})
	host := &issue6332CallbackHost{base: base, body: second, result: make(chan error, 1)}
	base.SetPluginHost(host)
	capture := &issue6332Capture{records: make(chan usage.Record, 4), authID: t.Name()}
	usage.RegisterNamedPlugin(t.Name(), capture)
	t.Cleanup(func() { usage.RegisterNamedPlugin(t.Name(), responsesUsageNop{}) })
	router := gin.New()
	router.POST("/v1/responses", NewOpenAIResponsesAPIHandler(base).Responses)
	writer := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(string(body)))
	request = request.WithContext(httptransport.WithDecorator(request.Context(), rt.decorate))
	router.ServeHTTP(writer, request)
	select {
	case errNested := <-host.result:
		if errNested != nil {
			t.Fatal(errNested)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("lifecycle next turn blocked")
	}
	<-rt.requests
	secondRequest := <-rt.requests
	if !strings.Contains(string(secondRequest), strings.Repeat("s", 128)) {
		t.Errorf("lifecycle next turn read stale replay ledger: %s", secondRequest)
	}
	select {
	case <-rt.eofReached:
	case <-time.After(time.Second):
		t.Fatal("outer producer did not reach gated EOF")
	}
	select {
	case <-rt.closed:
		t.Fatal("upstream cleanup completed before next turn assertion")
	default:
	}
	if peerCalls.Load() != 2 || rt.calls.Load() != 2 {
		t.Fatalf("real peer/delegated calls=%d/%d, want 2/2", peerCalls.Load(), rt.calls.Load())
	}
	unblock()
	select {
	case <-rt.closed:
	case <-time.After(time.Second):
		t.Fatal("underlying body Close did not complete")
	}
	// Join both producers only after the next-turn assertion.
	for range 2 {
		select {
		case record := <-capture.records:
			if record.Failed || record.Detail.InputTokens != 11 || record.Detail.OutputTokens != 22 || record.Detail.TotalTokens != 33 {
				t.Errorf("usage producer=%+v, want success 11/22/33", record)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("usage producers did not join")
		}
	}
	cache.ClearAntigravityReasoningReplayCache()
}

func TestIssue6332Review7PartialEOFDoesNotCommitReplay(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, strings.Replace(issue6332ToolFrame(), `,"finishReason":"STOP"`, "", 1))
	}))
	defer server.Close()
	base, _ := issue6332ReviewBase(t, server.URL)
	router := gin.New()
	router.POST("/v1/responses", NewOpenAIResponsesAPIHandler(base).Responses)
	writer := httptest.NewRecorder()
	router.ServeHTTP(writer, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(string(issue6332ReviewBody(t)))))
	if !strings.Contains(writer.Body.String(), "response.completed") {
		t.Fatalf("expected synthetic EOF completion: %s", writer.Body.String())
	}
	items, ok := cache.GetAntigravityReasoningReplayItems(issue6332ReviewModel, "responses:"+t.Name())
	if ok || len(items) != 0 {
		t.Fatal("synthetic completion committed partial upstream output without finishReason")
	}
}
