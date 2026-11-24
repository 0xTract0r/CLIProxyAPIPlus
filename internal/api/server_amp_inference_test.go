package api

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	sdkaccess "github.com/router-for-me/CLIProxyAPI/v8/sdk/access"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

type ampInferenceObservation struct{ authID, model, format string }

type ampInferenceFixtureExecutor struct {
	coreauth.ProviderExecutor
	calls  []ampInferenceObservation
	refuse bool
}

func (*ampInferenceFixtureExecutor) Identifier() string { return "amp-fixture" }

func (e *ampInferenceFixtureExecutor) Execute(_ context.Context, auth *coreauth.Auth, req coreexecutor.Request, opts coreexecutor.Options) (coreexecutor.Response, error) {
	e.calls = append(e.calls, ampInferenceObservation{auth.ID, req.Model, string(opts.SourceFormat)})
	if e.refuse {
		return coreexecutor.Response{}, &coreauth.Error{HTTPStatus: http.StatusTooManyRequests, Message: "synthetic provider refusal"}
	}
	return coreexecutor.Response{Payload: []byte(`{"id":"fixture","object":"chat.completion","type":"message","choices":[{"index":0,"message":{"role":"assistant","content":"fixture"},"finish_reason":"stop"}],"candidates":[{"content":{"parts":[{"text":"fixture"}]}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)}, nil
}

func (e *ampInferenceFixtureExecutor) CountTokens(ctx context.Context, auth *coreauth.Auth, req coreexecutor.Request, opts coreexecutor.Options) (coreexecutor.Response, error) {
	e.calls = append(e.calls, ampInferenceObservation{auth.ID, req.Model, string(opts.SourceFormat)})
	return coreexecutor.Response{Payload: []byte(`{"input_tokens":3}`)}, nil
}

func (e *ampInferenceFixtureExecutor) ExecuteStream(_ context.Context, auth *coreauth.Auth, req coreexecutor.Request, opts coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	e.calls = append(e.calls, ampInferenceObservation{auth.ID, req.Model, string(opts.SourceFormat)})
	chunks := make(chan coreexecutor.StreamChunk, 1)
	chunks <- coreexecutor.StreamChunk{Payload: []byte(`{"candidates":[{"content":{"parts":[{"text":"amp-stream-fixture"}]}}]}`)}
	close(chunks)
	return &coreexecutor.StreamResult{Chunks: chunks}, nil
}

func TestOrdinaryAmpProviderInferenceUsesSelectedCredential(t *testing.T) {
	const id = "amp-ordinary-credential"
	const localhost = "127.0.0.1:5300"
	s := newAmpForwardingFixture(t, config.AmpCode{
		UpstreamURL: "http://amp-ordinary.example.invalid/fixture-base", UpstreamAPIKey: "synthetic-service-key",
		RestrictManagementToLocalhost: true,
		ModelMappings: []config.AmpModelMapping{
			{From: "client-project/original-chat", To: "client-project/configured-chat"},
			{From: "client-project/original-claude", To: "client-project/configured-claude"},
			{From: "^client-project/original-gemini$", To: "client-project/configured-gemini", Regex: true},
			{From: "client-project/known-claude", To: "client-project/configured-claude"},
		},
	})
	callsForwarded := make(chan string, 8)
	attachAmpPipe(t, s, "amp-ordinary.example.invalid", func(conn net.Conn) {
		req, err := http.ReadRequest(bufio.NewReader(conn))
		if err != nil {
			return
		}
		body, _ := io.ReadAll(req.Body)
		_ = req.Body.Close()
		if req.Header.Get("Authorization") != "Bearer synthetic-service-key" ||
			req.Header.Get("Cookie") != "" || req.URL.Query().Has("key") ||
			!strings.Contains(string(body), "missing-local-catalog") {
			callsForwarded <- "unsafe forwarded data"
		} else {
			callsForwarded <- req.URL.Path
		}
		_, _ = io.WriteString(conn, "HTTP/1.1 409 Conflict\r\nRetry-After: 15\r\nContent-Length: 11\r\nConnection: close\r\n\r\nfixture409!")
	})

	manager := s.handlers.AuthManager
	provider := &ampInferenceFixtureExecutor{}
	manager.RegisterExecutor(provider)
	if _, err := manager.Register(context.Background(), &coreauth.Auth{ID: id, Provider: provider.Identifier(), Status: coreauth.StatusActive}); err != nil {
		t.Fatal(err)
	}
	models := registry.GetGlobalRegistry()
	models.RegisterClient(id, provider.Identifier(), []*registry.ModelInfo{
		{ID: "client-project/configured-chat", Type: "openai", Object: "model", OwnedBy: "fixture"},
		{ID: "client-project/configured-claude", Type: "claude", Object: "model", OwnedBy: "fixture"},
		{ID: "client-project/known-claude", Type: "claude", Object: "model", OwnedBy: "fixture"},
		{ID: "client-project/configured-gemini", Type: "gemini", Object: "model", OwnedBy: "fixture"},
	})
	t.Cleanup(func() { models.UnregisterClient(id) })

	call := func(method, path, body, remote, key string) *httptest.ResponseRecorder {
		req := ampFixtureRequest(method, path, body, remote, key)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Cookie", "caller-cookie=synthetic")
		req.Header.Set("X-Forwarded-For", "127.0.0.1")
		out := httptest.NewRecorder()
		s.Handler().ServeHTTP(out, req)
		return out
	}
	const chatPath = "/api/provider/openai/v1/chat/completions"
	if out := call(http.MethodPost, chatPath, `{"model":"client-project/original-chat"}`, localhost, "wrong-key"); out.Code != 401 ||
		len(provider.calls) != 0 || len(callsForwarded) != 0 {
		t.Fatal("invalid caller key reached a transport")
	}
	list := call(http.MethodGet, "/api/provider/openai/v1/models", "", localhost, ampInboundFixture)
	if list.Code != 200 || !strings.Contains(list.Body.String(), "client-project/configured-chat") {
		t.Fatalf("real standard model catalog omitted the configured prefix: %d %s", list.Code, list.Body.String())
	}
	for _, tc := range []struct{ path, body, want string }{
		{chatPath, `{"model":"client-project/original-chat","messages":[{"role":"user","content":"hello"}]}`, "client-project/configured-chat"},
		{"/api/provider/openai/v1/completions", `{"model":"client-project/original-chat","prompt":"hello"}`, "client-project/configured-chat"},
		{"/api/provider/openai/completions", `{"model":"client-project/original-chat","prompt":"hello"}`, "client-project/configured-chat"},
		{"/api/provider/openai/chat/completions", `{"model":"client-project/original-chat","messages":[{"role":"user","content":"hello"}]}`, "client-project/configured-chat"},
		{"/api/provider/openai/v1/responses", `{"model":"client-project/original-chat","input":"hello"}`, "client-project/configured-chat"},
		{"/api/provider/anthropic/v1/messages", `{"model":"client-project/original-claude","max_tokens":4,"messages":[{"role":"user","content":"hello"}]}`, "client-project/configured-claude"},
		{"/api/provider/anthropic/v1/messages/count_tokens", `{"model":"client-project/original-claude","messages":[{"role":"user","content":"hello"}]}`, "client-project/configured-claude"},
		{"/api/provider/anthropic/v1/messages", `{"model":"client-project/known-claude","max_tokens":4,"messages":[{"role":"user","content":"hello"}]}`, "client-project/known-claude"},
		{"/api/provider/google/v1beta/models/client-project/original-gemini:generateContent", `{"contents":[{"parts":[{"text":"hello"}]}]}`, "client-project/configured-gemini"},
		{"/api/provider/google/v1beta1/projects/example/publishers/google/models/client-project/original-gemini:generateContent", `{"contents":[{"parts":[{"text":"hello"}]}]}`, "client-project/configured-gemini"},
	} {
		before := len(provider.calls)
		out := call(http.MethodPost, tc.path, tc.body, localhost, ampInboundFixture)
		if out.Code != 200 || len(provider.calls) != before+1 {
			t.Fatalf("%s returned %d (%s), observations=%v", tc.path, out.Code, out.Body.String(), provider.calls)
		}
		observation := provider.calls[before]
		if observation.authID != id || observation.model != tc.want {
			t.Fatalf("%s dispatched to %+v, want same credential and model %q", tc.path, observation, tc.want)
		}
	}
	beforeStream := len(provider.calls)
	stream := call(http.MethodPost, "/api/provider/google/v1beta/models/client-project/original-gemini:streamGenerateContent?alt=sse",
		`{"contents":[{"parts":[{"text":"hello"}]}]}`, localhost, ampInboundFixture)
	if stream.Code != 200 || !strings.Contains(stream.Header().Get("Content-Type"), "text/event-stream") ||
		!strings.Contains(stream.Body.String(), "amp-stream-fixture") ||
		len(provider.calls) != beforeStream+1 || provider.calls[beforeStream].authID != id ||
		provider.calls[beforeStream].model != "client-project/configured-gemini" {
		t.Fatal("standard Gemini stream did not use the selected credential and mapped model")
	}
	cfg := *s.getConfig()
	cfg.AmpCode.ForceModelMappings = true
	if !s.UpdateClientsContext(context.Background(), &cfg) {
		t.Fatal("failed to publish forced model mapping")
	}
	beforeForce := len(provider.calls)
	forced := call(http.MethodPost, "/api/provider/anthropic/v1/messages",
		`{"model":"client-project/known-claude","max_tokens":4,"messages":[{"role":"user","content":"hello"}]}`, localhost, ampInboundFixture)
	if forced.Code != 200 || len(provider.calls) != beforeForce+1 ||
		provider.calls[beforeForce].model != "client-project/configured-claude" {
		t.Fatal("forced mapping did not take effect on the live standard route")
	}

	var gzipBody bytes.Buffer
	compressor := gzip.NewWriter(&gzipBody)
	if _, err := io.WriteString(compressor, `{"model":"client-project/original-chat","messages":[{"role":"user","content":"hello"}]}`); err != nil {
		t.Fatal(err)
	}
	if err := compressor.Close(); err != nil {
		t.Fatal(err)
	}
	beforeGzip := len(provider.calls)
	compressed := httptest.NewRequest(http.MethodPost, chatPath, &gzipBody)
	compressed.RemoteAddr = localhost
	compressed.Header.Set("Authorization", "Bearer "+ampInboundFixture)
	compressed.Header.Set("Content-Encoding", "gzip")
	out := httptest.NewRecorder()
	s.Handler().ServeHTTP(out, compressed)
	if out.Code != 200 || len(provider.calls) != beforeGzip+1 || provider.calls[beforeGzip].model != "client-project/configured-chat" {
		t.Fatal("Amp did not preserve the standard compressed request contract")
	}

	knownRequests := len(provider.calls)
	provider.refuse = true
	refused := call(http.MethodPost, chatPath, `{"model":"client-project/original-chat","messages":[{"role":"user","content":"hello"}]}`, localhost, ampInboundFixture)
	if refused.Code != 429 || len(provider.calls) != knownRequests+1 || len(callsForwarded) != 0 {
		t.Fatalf("real provider rejection triggered a new dispatch: status %d", refused.Code)
	}

	if bad := call(http.MethodPost, chatPath, `{"model":`, localhost, ampInboundFixture); bad.Code != 400 || len(callsForwarded) != 0 {
		t.Fatal("malformed request became a fallback upstream request")
	}
	const missing = `{"model":"missing-local-catalog","messages":[{"role":"user","content":"hello"}]}`
	if untrusted := call(http.MethodPost, chatPath, missing, "192.0.2.3:4000", ampInboundFixture); untrusted.Code != 403 || len(callsForwarded) != 0 {
		t.Fatal("local-only fallback trusted a forwarded source address")
	}
	forwarded := call(http.MethodPost, chatPath+"?key=caller-query-value", missing, localhost, ampInboundFixture)
	if forwarded.Code != 409 || forwarded.Body.String() != "fixture409!" || forwarded.Header().Get("Retry-After") != "15" || len(provider.calls) != knownRequests+1 {
		t.Fatalf("ordinary predispatch forwarding did not return the original refusal: %d %s", forwarded.Code, forwarded.Body.String())
	}
	select {
	case path := <-callsForwarded:
		if path != "/fixture-base"+chatPath {
			t.Fatalf("upstream forwarding failed isolation or prefix: %q", path)
		}
	default:
		t.Fatal("expected one configured upstream request")
	}
	if len(callsForwarded) != 0 {
		t.Fatal("a rejected fallback was retried")
	}
}

func TestOrdinaryAmpMappingExactBeatsRegexAndPreservesThinkingSuffix(t *testing.T) {
	configured := []config.AmpModelMapping{
		{From: "^custom/", To: "custom/regex-target", Regex: true},
		{From: "custom/source", To: "custom/exact-target"},
		{From: "custom/source(high)", To: "custom/override-low(low)"},
	}
	if got := ampMappedModel("custom/source(high)", configured); got != "custom/override-low(low)" {
		t.Fatalf("specific mapping was not preferred: %s", got)
	}
	if got := ampMappedModel("custom/source(medium)", configured); got != "custom/exact-target(medium)" {
		t.Fatalf("thinking selection did not round-trip: %s", got)
	}
	if got := ampMappedModel("custom/next", configured); got != "custom/regex-target" {
		t.Fatalf("regular expression routing not used: %s", got)
	}
}

type ampFixtureModelAccess struct {
	checks int
}

func (*ampFixtureModelAccess) Identifier() string { return "fixture-model-access" }

func (p *ampFixtureModelAccess) Authenticate(_ context.Context, request *http.Request) (*sdkaccess.Result, *sdkaccess.AuthError) {
	p.checks++
	if request.Header.Get("Authorization") != "Bearer "+ampInboundFixture {
		return nil, sdkaccess.NewInvalidCredentialError()
	}
	body, err := io.ReadAll(request.Body)
	if err != nil {
		return nil, sdkaccess.NewInternalAuthError("fixture body read failed", err)
	}
	request.Body = io.NopCloser(bytes.NewReader(body))
	if strings.Contains(request.URL.Path, "forbidden-target") || bytes.Contains(body, []byte("forbidden-target")) {
		return nil, sdkaccess.NewDeniedError("not authorized for this model", http.StatusForbidden)
	}
	return &sdkaccess.Result{Provider: p.Identifier(), Principal: ampInboundFixture}, nil
}

func TestOrdinaryAmpMappingDoesNotOverrideTargetAccess(t *testing.T) {
	s := newAmpForwardingFixture(t, config.AmpCode{ModelMappings: []config.AmpModelMapping{
		{From: "test-client/allowed-alias", To: "test-client/forbidden-target"},
	}})
	access := &ampFixtureModelAccess{}
	s.accessManager.SetProviders([]sdkaccess.Provider{access})
	for _, route := range []struct{ path, body string }{
		{"/api/provider/openai/v1/chat/completions", `{"model":"test-client/allowed-alias","messages":[{"role":"user","content":"hello"}]}`},
		{"/api/provider/google/v1beta/models/test-client/allowed-alias:generateContent", `{"contents":[{"parts":[{"text":"hello"}]}]}`},
	} {
		before := access.checks
		req := ampFixtureRequest(http.MethodPost, route.path, route.body, "127.0.0.1:3000", ampInboundFixture)
		req.Header.Set("Content-Type", "application/json")
		out := httptest.NewRecorder()
		s.Handler().ServeHTTP(out, req)
		if out.Code != 403 || access.checks != before+2 || !strings.Contains(out.Body.String(), "not authorized for this model") {
			t.Fatalf("resolved model skipped authorization at %s: %d %s", route.path, out.Code, out.Body.String())
		}
	}
}
