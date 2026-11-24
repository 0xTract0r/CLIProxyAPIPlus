package claude

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/MoeForge/moego/ojson"
	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

type issue7521UpstreamError struct {
	status     int
	body       string
	retryAfter time.Duration
	headers    http.Header
}

func (e *issue7521UpstreamError) Error() string   { return e.body }
func (e *issue7521UpstreamError) StatusCode() int { return e.status }
func (e *issue7521UpstreamError) RetryAfter() *time.Duration {
	d := e.retryAfter
	return &d
}
func (e *issue7521UpstreamError) Headers() http.Header {
	if e.headers == nil {
		return nil
	}
	return e.headers.Clone()
}

type issue7521ClaudeExecutor struct {
	executeFn func(ctx context.Context, auth *coreauth.Auth, req coreexecutor.Request, opts coreexecutor.Options) (coreexecutor.Response, error)
	streamFn  func(ctx context.Context, auth *coreauth.Auth, req coreexecutor.Request, opts coreexecutor.Options) (*coreexecutor.StreamResult, error)
	countFn   func(ctx context.Context, auth *coreauth.Auth, req coreexecutor.Request, opts coreexecutor.Options) (coreexecutor.Response, error)
}

func (e *issue7521ClaudeExecutor) Identifier() string { return "claude" }

func (e *issue7521ClaudeExecutor) Execute(ctx context.Context, auth *coreauth.Auth, req coreexecutor.Request, opts coreexecutor.Options) (coreexecutor.Response, error) {
	coreexecutor.MarkUpstreamAttempt(ctx)
	return e.executeFn(ctx, auth, req, opts)
}

func (e *issue7521ClaudeExecutor) ExecuteStream(ctx context.Context, auth *coreauth.Auth, req coreexecutor.Request, opts coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	coreexecutor.MarkUpstreamAttempt(ctx)
	return e.streamFn(ctx, auth, req, opts)
}

func (e *issue7521ClaudeExecutor) CountTokens(ctx context.Context, auth *coreauth.Auth, req coreexecutor.Request, opts coreexecutor.Options) (coreexecutor.Response, error) {
	coreexecutor.MarkUpstreamAttempt(ctx)
	return e.countFn(ctx, auth, req, opts)
}

func (e *issue7521ClaudeExecutor) Refresh(_ context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	return auth, nil
}

func (e *issue7521ClaudeExecutor) HttpRequest(_ context.Context, _ *coreauth.Auth, _ *http.Request) (*http.Response, error) {
	return nil, nil
}

func TestClaudeSelectedCredential429PreservesRetryAfter(t *testing.T) {
	gin.SetMode(gin.TestMode)

	const model = "issue-7521-claude-route-model"
	hint := 95 * time.Minute
	fixtureMessage := "First account rate limited."

	for _, tc := range []struct {
		name               string
		path               string
		streaming          bool
		disablePassthrough bool
		nominateTrace      bool
		forbidden          bool
	}{
		{name: "messages_non_stream", path: "/v1/messages", streaming: false},
		{name: "messages_stream", path: "/v1/messages", streaming: true},
		{name: "count_tokens", path: "/v1/messages/count_tokens", streaming: false},
		{name: "disabled", path: "/v1/messages", disablePassthrough: true},
		{name: "connection_nominated", path: "/v1/messages", nominateTrace: true},
		{name: "forbidden", path: "/v1/messages", forbidden: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var selected []string
			makeErr := func(id string) error {
				selected = append(selected, id)
				status := http.StatusTooManyRequests
				if tc.forbidden {
					status = http.StatusForbidden
				}
				headers := http.Header{"Retry-After": {"5700"}, "X-Upstream-Trace": {"trace-selected"}, "Set-Cookie": {"fixture=secret"}, "Cookie": {"fixture=secret"}, "Authorization": {"secret"}, "X-Private-Fixture": {"secret"}}
				if tc.nominateTrace {
					headers["connection"] = []string{"x-upstream-trace"}
				}
				return &issue7521UpstreamError{
					status:     status,
					body:       `{"type":"error","error":{"type":"rate_limit_error","message":"First account rate limited."}}`,
					retryAfter: hint,
					headers:    headers,
				}
			}
			manager := coreauth.NewManager(nil, nil, nil)
			manager.RegisterExecutor(&issue7521ClaudeExecutor{
				executeFn: func(_ context.Context, auth *coreauth.Auth, _ coreexecutor.Request, _ coreexecutor.Options) (coreexecutor.Response, error) {
					return coreexecutor.Response{}, makeErr(auth.ID)
				},
				streamFn: func(_ context.Context, auth *coreauth.Auth, _ coreexecutor.Request, _ coreexecutor.Options) (*coreexecutor.StreamResult, error) {
					chunks := make(chan coreexecutor.StreamChunk, 1)
					chunks <- coreexecutor.StreamChunk{Err: makeErr(auth.ID)}
					close(chunks)
					return &coreexecutor.StreamResult{Chunks: chunks}, nil
				},
				countFn: func(_ context.Context, auth *coreauth.Auth, _ coreexecutor.Request, _ coreexecutor.Options) (coreexecutor.Response, error) {
					return coreexecutor.Response{}, makeErr(auth.ID)
				},
			})

			reg := registry.GetGlobalRegistry()
			for _, suffix := range []string{"-a", "-b"} {
				authID := "issue-7521-route-" + tc.name + suffix
				auth := &coreauth.Auth{
					ID:         authID,
					Provider:   "claude",
					Status:     coreauth.StatusActive,
					Attributes: map[string]string{"api_key": "key" + suffix},
				}
				if _, errReg := manager.Register(context.Background(), auth); errReg != nil {
					t.Fatalf("register auth: %v", errReg)
				}
				reg.RegisterClient(authID, "claude", []*registry.ModelInfo{{ID: model}})
				t.Cleanup(func() {
					reg.UnregisterClient(authID)
				})
			}

			handler := NewClaudeCodeAPIHandler(handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{PassthroughHeaders: !tc.disablePassthrough}, manager))
			router := gin.New()
			router.POST("/v1/messages", handler.ClaudeMessages)
			router.POST("/v1/messages/count_tokens", handler.ClaudeCountTokens)

			body := `{"model":"` + model + `","messages":[{"role":"user","content":"hi"}],"stream":` + strconv.FormatBool(tc.streaming) + `}`
			req := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)

			wantStatus := http.StatusTooManyRequests
			if tc.forbidden {
				wantStatus = http.StatusForbidden
			}
			if rec.Code != wantStatus {
				t.Fatalf("status = %d, want %d; body=%s", rec.Code, wantStatus, rec.Body.String())
			}
			seconds, errParse := strconv.ParseInt(rec.Header().Get("Retry-After"), 10, 64)
			if errParse != nil || seconds <= 0 {
				t.Fatalf("Retry-After = %q, want positive integer seconds", rec.Header().Get("Retry-After"))
			}
			if seconds < 5690 || seconds > 5700 || len(selected) != 1 || !strings.HasSuffix(selected[0], "-a") {
				t.Fatalf("selected hint=%d calls=%v", seconds, selected)
			}
			wantTrace := "trace-selected"
			if tc.nominateTrace || tc.disablePassthrough {
				wantTrace = ""
			}
			if got := rec.Header().Get("X-Upstream-Trace"); got != wantTrace {
				t.Fatalf("selected upstream header was lost: %q", got)
			}
			for _, key := range []string{"Connection", "Set-Cookie", "Cookie", "Authorization", "X-Private-Fixture"} {
				if rec.Header().Get(key) != "" {
					t.Fatalf("unexpected response header %s", key)
				}
			}
			if tc.forbidden {
				return
			}
			if gotType := ojson.GetBytesNoCopy(rec.Body.Bytes(), "error.type").String(); gotType != "rate_limit_error" {
				t.Fatalf("error.type = %q, want rate_limit_error", gotType)
			}
			wantMessage := fixtureMessage
			if gotMsg := ojson.GetBytesNoCopy(rec.Body.Bytes(), "error.message").String(); gotMsg != wantMessage {
				t.Fatalf("error.message = %q, want %q", gotMsg, wantMessage)
			}
		})
	}
}

func TestClaudeHTTPTraceDoesNotTrustLocalError(t *testing.T) {
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	h := NewClaudeCodeAPIHandler(handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{PassthroughHeaders: true}, nil))
	h.WriteErrorResponse(c, &interfaces.ErrorMessage{StatusCode: http.StatusForbidden, Error: &issue7521UpstreamError{status: http.StatusForbidden, body: "local refusal", headers: http.Header{"X-Upstream-Trace": {"local-fixture"}}}})
	if got := rec.Header().Get("X-Upstream-Trace"); got != "" {
		t.Fatalf("local error without a physical attempt exposed trace: %q", got)
	}
}
