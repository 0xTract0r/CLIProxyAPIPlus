package executor

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/httptransport"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

type publicProviderFixtureTransport func(*http.Request) (*http.Response, error)

func (f publicProviderFixtureTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func publicProviderTestStatus(t *testing.T, err error, want int) {
	t.Helper()
	var status cliproxyexecutor.StatusError
	if !errors.As(err, &status) || status.StatusCode() != want || !coreauth.IsRequestStopError(err) {
		t.Fatalf("error = %v, want terminal HTTP %d", err, want)
	}
}

func TestOrdinaryGitLabCodeSuggestionsRuntime(t *testing.T) {
	t.Setenv("GITLAB_BASE_URL", "https://gitlab.example.invalid")
	auth := &coreauth.Auth{ID: "synthetic-gitlab", Provider: "gitlab", Metadata: map[string]any{
		"auth_method": "pat", "personal_access_token": "fixture-gitlab-pat",
		"base_url": "https://gitlab.example.invalid",
	}}
	exec := NewGitLabExecutor(&config.Config{})
	status, calls := http.StatusOK, 0
	ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", publicProviderFixtureTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.String() != "https://gitlab.example.invalid/api/v4/code_suggestions/completions" ||
			r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer fixture-gitlab-pat" || r.Header.Get("Cookie") != "" {
			t.Fatalf("GitLab request violates documented endpoint or authorization")
		}
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), `"current_file"`) {
			t.Fatalf("GitLab request lost current_file")
		}
		headers := http.Header{"Retry-After": []string{"9"}, "Location": []string{"https://other.invalid"}}
		return &http.Response{StatusCode: status, Header: headers, Body: io.NopCloser(strings.NewReader(`{"choices":[{"text":"synthetic completion","index":0}]}`))}, nil
	}))
	req := cliproxyexecutor.Request{Model: GitLabCodeSuggestionsModel,
		Payload: []byte(`{"current_file":{"file_name":"example.go","content_above_cursor":"package example"}}`)}
	if resp, err := exec.Execute(ctx, auth, req, cliproxyexecutor.Options{}); err != nil || !strings.Contains(string(resp.Payload), "synthetic completion") {
		t.Fatalf("GitLab success: %s, %v", resp.Payload, err)
	}
	for _, want := range []int{http.StatusForbidden, http.StatusTooManyRequests, http.StatusTemporaryRedirect} {
		status = want
		_, err := exec.Execute(ctx, auth, req, cliproxyexecutor.Options{})
		publicProviderTestStatus(t, err, want)
		if want == http.StatusTooManyRequests {
			var retry interface{ RetryAfter() *time.Duration }
			if !errors.As(err, &retry) || retry.RetryAfter() == nil || *retry.RetryAfter() != 9*time.Second {
				t.Fatal("GitLab Retry-After not preserved")
			}
		}
	}
	if _, err := exec.ExecuteStream(ctx, auth, req, cliproxyexecutor.Options{}); err == nil {
		t.Fatal("unsupported GitLab completion stream unexpectedly enabled")
	}
	req.Payload = []byte(`{"messages":[{"role":"user","content":"do not send"}]}`)
	_, err := exec.Execute(ctx, auth, req, cliproxyexecutor.Options{})
	publicProviderTestStatus(t, err, http.StatusBadRequest)
	if calls != 4 {
		t.Fatalf("GitLab retried, redirected or forwarded incompatible payload: %d", calls)
	}
}

func TestOrdinaryGitLabOtherFormatsTerminateBeforeSend(t *testing.T) {
	t.Setenv("GITLAB_BASE_URL", "https://gitlab.example.invalid")
	auth := &coreauth.Auth{ID: "synthetic-gitlab", Provider: "gitlab", Metadata: map[string]any{
		"base_url": "https://gitlab.example.invalid", "access_token": "fixture",
	}}
	ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", publicProviderFixtureTransport(func(*http.Request) (*http.Response, error) {
		t.Fatal("unsupported GitLab operation attempted a request")
		return nil, nil
	}))
	exec := NewGitLabExecutor(&config.Config{})
	for _, model := range []string{"claude-sonnet-4-6", "gpt-5", "chat"} {
		req := cliproxyexecutor.Request{Model: model,
			Payload: []byte(`{"messages":[{"role":"user","content":"test"}]}`)}
		_, err := exec.Execute(ctx, auth, req, cliproxyexecutor.Options{})
		publicProviderTestStatus(t, err, http.StatusNotImplemented)
	}
	req := cliproxyexecutor.Request{Model: GitLabCodeSuggestionsModel}
	_, err := exec.CountTokens(ctx, auth, req, cliproxyexecutor.Options{})
	publicProviderTestStatus(t, err, http.StatusNotImplemented)
	_, err = exec.ExecuteStream(ctx, auth, req, cliproxyexecutor.Options{})
	publicProviderTestStatus(t, err, http.StatusNotImplemented)
}

func TestOrdinaryKiloGatewayRuntimeAndRefusal(t *testing.T) {
	auth := &coreauth.Auth{ID: "synthetic-kilo", Provider: "kilo", Metadata: map[string]any{
		"api_key": "fixture-kilo-key", "organization_id": "fixture-org",
	}}
	exec := NewKiloExecutor(&config.Config{})
	status, calls := http.StatusOK, 0
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI, Headers: http.Header{
		"Authorization": []string{"Bearer caller"}, "X-Kilocode-Organizationid": []string{"wrong"},
	}}
	req := cliproxyexecutor.Request{Model: "kilo-auto/efficient",
		Payload: []byte(`{"model":"kilo-auto/efficient","messages":[{"role":"user","content":"fixture"}]}`)}
	ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", publicProviderFixtureTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.String() != kiloGatewayBase+"/chat/completions" || r.Method != http.MethodPost ||
			r.Header.Get("Authorization") != "Bearer fixture-kilo-key" || r.Header.Get("X-KiloCode-OrganizationId") != "fixture-org" {
			t.Fatalf("Kilo sent a request outside the documented gateway or wrong credential scope")
		}
		body := `{"id":"fixture","object":"chat.completion","model":"kilo-auto/efficient","choices":[{"index":0,"message":{"role":"assistant","content":"fixture output"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`
		if status >= 300 {
			body = `{"error":{"message":"fixture rejection"}}`
		}
		headers := http.Header{"Retry-After": []string{"5"}, "Location": []string{"https://other.invalid"}}
		return &http.Response{StatusCode: status, Header: headers, Body: io.NopCloser(strings.NewReader(body))}, nil
	}))
	if resp, err := exec.Execute(ctx, auth, req, opts); err != nil || !strings.Contains(string(resp.Payload), "fixture output") {
		t.Fatalf("Kilo success: %s, %v", resp.Payload, err)
	}
	for _, want := range []int{http.StatusUnauthorized, http.StatusPaymentRequired, http.StatusForbidden, http.StatusTooManyRequests, http.StatusTemporaryRedirect} {
		status = want
		_, err := exec.Execute(ctx, auth, req, opts)
		publicProviderTestStatus(t, err, want)
		if !strings.Contains(err.Error(), "fixture rejection") {
			t.Fatalf("lost upstream error body: %v", err)
		}
	}
	missing := &coreauth.Auth{Provider: "kilo"}
	_, err := exec.Execute(ctx, missing, req, opts)
	publicProviderTestStatus(t, err, http.StatusUnauthorized)
	if calls != 6 {
		t.Fatalf("Kilo retried, followed a redirect, or sent an unauthenticated call: %d calls", calls)
	}
}

type ordinaryProviderCancellableBody struct {
	prefix *strings.Reader
	ctx    context.Context
	closed chan struct{}
}

func (b *ordinaryProviderCancellableBody) Read(p []byte) (int, error) {
	if b.prefix.Len() != 0 {
		return b.prefix.Read(p)
	}
	<-b.ctx.Done()
	return 0, b.ctx.Err()
}

func (b *ordinaryProviderCancellableBody) Close() error {
	close(b.closed)
	return nil
}

func TestOrdinaryKiloGatewayStreamingCancellation(t *testing.T) {
	exec := NewKiloExecutor(&config.Config{})
	auth := &coreauth.Auth{ID: "synthetic-kilo-stream", Provider: "kilo", Metadata: map[string]any{"api_key": "fixture-api-key"}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	closed := make(chan struct{})
	ctx = context.WithValue(ctx, "cliproxy.roundtripper", publicProviderFixtureTransport(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("Accept") != "text/event-stream" {
			t.Fatalf("missing SSE Accept header")
		}
		body := &ordinaryProviderCancellableBody{ctx: r.Context(), closed: closed,
			prefix: strings.NewReader("data: {\"choices\":[{\"delta\":{\"content\":\"live fragment\"}}]}\n\n")}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: body}, nil
	}))
	req := cliproxyexecutor.Request{Model: "kilo-auto/efficient", Payload: []byte(`{"messages":[{"role":"user","content":"fixture"}],"stream":true}`)}
	stream, err := exec.ExecuteStream(ctx, auth, req, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI, Stream: true})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case chunk := <-stream.Chunks:
		if chunk.Err != nil || !strings.Contains(string(chunk.Payload), "live fragment") {
			t.Fatalf("unexpected stream event: %s %v", chunk.Payload, chunk.Err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no streaming payload")
	}
	cancel()
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("cancellation did not close the outgoing stream")
	}
}

func TestOrdinaryGitLabCancellationAndPinnedOrigin(t *testing.T) {
	t.Setenv("GITLAB_BASE_URL", "https://gitlab.example.invalid")
	auth := &coreauth.Auth{Provider: "gitlab", Metadata: map[string]any{
		"base_url": "https://gitlab.example.invalid", "access_token": "fixture-key",
	}}
	calls := 0
	ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", publicProviderFixtureTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		if !errors.Is(r.Context().Err(), context.Canceled) {
			t.Fatal("request did not receive caller cancellation")
		}
		return nil, r.Context().Err()
	}))
	exec := NewGitLabExecutor(&config.Config{})
	foreign, err := http.NewRequest(http.MethodPost, "https://other.invalid/api/v4/code_suggestions/completions", nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = exec.HttpRequest(ctx, auth, foreign)
	publicProviderTestStatus(t, err, http.StatusBadRequest)
	if calls != 0 {
		t.Fatalf("foreign origin received %d transport calls", calls)
	}
	ctx, cancel := context.WithCancel(ctx)
	cancel()
	req := cliproxyexecutor.Request{Model: GitLabCodeSuggestionsModel,
		Payload: []byte(`{"current_file":{"file_name":"example.go","content_above_cursor":"package example"}}`)}
	_, err = exec.Execute(ctx, auth, req, cliproxyexecutor.Options{})
	if !errors.Is(err, context.Canceled) || calls != 1 {
		t.Fatalf("GitLab cancellation = %v; transport calls = %d", err, calls)
	}
}

func TestOrdinaryGitLabOAuthRefreshUsesBoundAccountProxy(t *testing.T) {
	t.Setenv("GITLAB_BASE_URL", "https://gitlab.example.invalid")
	auth := &coreauth.Auth{Provider: "gitlab", ProxyURL: "http://fixture-proxy.invalid:8080", Metadata: map[string]any{
		"base_url": "https://gitlab.example.invalid", "auth_method": "oauth", "oauth_client_id": "fixture-app",
		"access_token": "fixture-old-access", "refresh_token": "fixture-old-refresh",
	}}
	var calls int
	ctx := httptransport.WithDecorator(context.Background(), func(next http.RoundTripper) http.RoundTripper {
		routed, ok := next.(*http.Transport)
		if !ok || routed.Proxy == nil {
			t.Fatal("GitLab refresh did not select the configured account proxy")
		}
		return publicProviderFixtureTransport(func(r *http.Request) (*http.Response, error) {
			calls++
			proxy, err := routed.Proxy(r)
			if err != nil || proxy.String() != auth.ProxyURL ||
				r.URL.String() != "https://gitlab.example.invalid/oauth/token" || r.Method != http.MethodPost {
				t.Fatal("GitLab refresh left its bound proxy or issuer")
			}
			if err := r.ParseForm(); err != nil || r.Form.Get("grant_type") != "refresh_token" ||
				r.Form.Get("refresh_token") != "fixture-old-refresh" || r.Form.Get("client_id") != "fixture-app" {
				t.Fatal("GitLab refresh did not use the ordinary OAuth grant")
			}
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header),
				Body: io.NopCloser(strings.NewReader(`{"access_token":"fixture-new-access","refresh_token":"fixture-new-refresh","expires_in":60}`))}, nil
		})
	})
	refreshed, err := NewGitLabExecutor(&config.Config{}).Refresh(ctx, auth)
	if err != nil || refreshed == nil || calls != 1 ||
		refreshed.Metadata["access_token"] != "fixture-new-access" ||
		refreshed.Metadata["refresh_token"] != "fixture-new-refresh" ||
		refreshed.Metadata["oauth_expires_at"] == "" || auth.Metadata["access_token"] != "fixture-old-access" {
		t.Fatalf("GitLab OAuth refresh: calls=%d, err=%v", calls, err)
	}
}

func TestOrdinaryKiloDeviceGrantUsesOnlyBoundGateway(t *testing.T) {
	auth := &coreauth.Auth{Provider: "kilo", Metadata: map[string]any{
		"kilocodeToken": "fixture-device-grant", "kilocodeOrganizationId": "fixture-bound-org",
	}}
	exec := NewKiloExecutor(&config.Config{})
	calls := 0
	ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", publicProviderFixtureTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.String() != kiloDeviceGatewayBase+"/chat/completions" ||
			r.Header.Get("Authorization") != "Bearer fixture-device-grant" ||
			r.Header.Get("X-KiloCode-OrganizationId") != "fixture-bound-org" || r.Header.Get("X-KiloCode-EditorName") != "" {
			t.Fatal("device grant escaped its gateway or organization")
		}
		return &http.Response{StatusCode: http.StatusForbidden, Header: make(http.Header),
			Body: io.NopCloser(strings.NewReader(`{"error":{"message":"fixture organization denied"}}`))}, nil
	}))
	req := cliproxyexecutor.Request{Model: "kilo-auto/efficient",
		Payload: []byte(`{"messages":[{"role":"user","content":"fixture"}]}`)}
	_, err := exec.Execute(ctx, auth, req, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI})
	publicProviderTestStatus(t, err, http.StatusForbidden)
	foreign, err := http.NewRequest(http.MethodPost, kiloGatewayBase+"/chat/completions", nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = exec.HttpRequest(ctx, auth, foreign)
	publicProviderTestStatus(t, err, http.StatusForbidden)
	if calls != 1 {
		t.Fatalf("device flow used %d suppliers rather than one", calls)
	}
}
