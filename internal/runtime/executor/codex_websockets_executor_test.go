package executor

import (
	"bytes"
	"context"
	"errors"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	log "github.com/sirupsen/logrus"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MoeForge/moego/ojson"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

func TestBuildCodexWebsocketRequestBodyPreservesPreviousResponseID(t *testing.T) {
	body := []byte(`{"model":"gpt-5-codex","previous_response_id":"resp-1","input":[{"type":"message","id":"msg-1"}]}`)

	wsReqBody := buildCodexWebsocketRequestBody(body)

	if got := ojson.GetBytes(wsReqBody, "type").String(); got != "response.create" {
		t.Fatalf("type = %s, want response.create", got)
	}
	if got := ojson.GetBytes(wsReqBody, "previous_response_id").String(); got != "resp-1" {
		t.Fatalf("previous_response_id = %s, want resp-1", got)
	}
	if ojson.GetBytes(wsReqBody, "input.0.id").String() != "msg-1" {
		t.Fatalf("input item id mismatch")
	}
	if got := ojson.GetBytes(wsReqBody, "type").String(); got == "response.append" {
		t.Fatalf("unexpected websocket request type: %s", got)
	}
}

func TestBuildCodexWebsocketRequestBodySanitizesOverlongInputItemIDs(t *testing.T) {
	longReasoningItemID := "rs_" + strings.Repeat("a", 64)
	longCallItemID := strings.Repeat("grok-call-item-", 6)
	longOutputItemID := strings.Repeat("grok-output-item-", 6)
	body := []byte(`{"model":"gpt-5-codex","input":[{"type":"reasoning","id":"` + longReasoningItemID + `","encrypted_content":"gAAAA-encrypted","summary":[]},{"type":"function_call","id":"` + longCallItemID + `","call_id":"call-1","name":"lookup"},{"type":"function_call_output","id":"` + longOutputItemID + `","call_id":"call-1","output":"ok"},{"type":"message","id":"item_74ec40c883248ebb4885ec84"}]}`)

	first := buildCodexWebsocketRequestBody(body)
	second := buildCodexWebsocketRequestBody(body)

	if input := ojson.GetBytes(first, "input").Array(); len(input) != 3 {
		t.Fatalf("input length = %d, want 3: %s", len(input), first)
	}
	if gotType := ojson.GetBytes(first, "input.0.type").String(); gotType != "function_call" {
		t.Fatalf("input.0.type = %q, want function_call: %s", gotType, first)
	}

	shortCallItemID := ojson.GetBytes(first, "input.0.id").String()
	shortOutputItemID := ojson.GetBytes(first, "input.1.id").String()
	if len([]rune(shortCallItemID)) > 64 || shortCallItemID == longCallItemID {
		t.Fatalf("input.0.id was not shortened to at most 64 characters: %q", shortCallItemID)
	}
	if len([]rune(shortOutputItemID)) > 64 || shortOutputItemID == longOutputItemID {
		t.Fatalf("input.1.id was not shortened to at most 64 characters: %q", shortOutputItemID)
	}
	if shortCallItemID == shortOutputItemID {
		t.Fatalf("distinct long IDs produced the same shortened ID: %q", shortCallItemID)
	}
	if got := ojson.GetBytes(second, "input.0.id").String(); got != shortCallItemID {
		t.Fatalf("input item ID shortening is not deterministic: first=%q second=%q", shortCallItemID, got)
	}
	if got := ojson.GetBytes(first, "input.0.call_id").String(); got != "call-1" {
		t.Fatalf("function call_id = %q, want call-1", got)
	}
	if got := ojson.GetBytes(first, "input.1.call_id").String(); got != "call-1" {
		t.Fatalf("function call output call_id = %q, want call-1", got)
	}
	if got := ojson.GetBytes(first, "input.2.id").String(); got != "msg_item_74ec40c883248ebb4885ec84" {
		t.Fatalf("message input item ID was not normalized: %q", got)
	}
}

func TestExistingWebsocketSessionConnRequiresMatchingHealthyConnection(t *testing.T) {
	conn := &websocket.Conn{}
	closer := newWebsocketConnectionCloser(conn)
	sess := &codexWebsocketSession{
		conn:       conn,
		connCloser: closer,
		authID:     "auth-a",
		wsURL:      "ws://example.test/responses",
	}
	sess.resetUpstreamDisconnectError(conn)
	if gotConn, gotCloser := existingWebsocketSessionConn(sess, "auth-a", "ws://example.test/responses", ""); gotConn != conn || gotCloser != closer {
		t.Fatal("matching healthy websocket session was not reusable")
	}
	if got, _ := existingWebsocketSessionConn(sess, "auth-b", "ws://example.test/responses", ""); got != nil {
		t.Fatal("websocket session matched a different auth")
	}
	if got, _ := existingWebsocketSessionConn(sess, "auth-a", "ws://other.test/responses", ""); got != nil {
		t.Fatal("websocket session matched a different URL")
	}
	sess.setUpstreamDisconnectError(conn, errors.New("upstream disconnected"))
	if got, _ := existingWebsocketSessionConn(sess, "auth-a", "ws://example.test/responses", ""); got != nil {
		t.Fatal("disconnected websocket session remained reusable")
	}
}

func TestSendTerminalWebsocketReadInvalidatesBeforeWaitingForCapacity(t *testing.T) {
	terminalErr := &websocket.CloseError{Code: websocket.CloseMessageTooBig}

	t.Run("available channel keeps fast path ordering", func(t *testing.T) {
		ch := make(chan codexWebsocketRead, 1)
		done := make(chan struct{})
		invalidateCalls := 0
		invalidated := sendTerminalWebsocketRead(ch, done, codexWebsocketRead{err: terminalErr}, func() {
			invalidateCalls++
		})
		if invalidated {
			t.Fatal("available channel should not invalidate before delivery")
		}
		if invalidateCalls != 0 {
			t.Fatalf("invalidate calls = %d, want 0", invalidateCalls)
		}
		event := <-ch
		if !errors.Is(event.err, terminalErr) {
			t.Fatalf("terminal error = %v, want %v", event.err, terminalErr)
		}
	})

	t.Run("full channel invalidates before waiting", func(t *testing.T) {
		ch := make(chan codexWebsocketRead, 1)
		ch <- codexWebsocketRead{payload: []byte("queued")}
		done := make(chan struct{})
		invalidateCalled := make(chan struct{})
		result := make(chan bool, 1)

		go func() {
			result <- sendTerminalWebsocketRead(ch, done, codexWebsocketRead{err: terminalErr}, func() {
				close(invalidateCalled)
			})
		}()

		select {
		case <-invalidateCalled:
		case <-time.After(time.Second):
			t.Fatal("invalidation did not happen before waiting for channel capacity")
		}
		select {
		case <-result:
			t.Fatal("terminal sender returned before capacity was released")
		default:
		}

		<-ch
		select {
		case event := <-ch:
			if !errors.Is(event.err, terminalErr) {
				t.Fatalf("terminal error = %v, want %v", event.err, terminalErr)
			}
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for terminal read")
		}
		select {
		case invalidated := <-result:
			if !invalidated {
				t.Fatal("full channel should report early invalidation")
			}
		case <-time.After(time.Second):
			t.Fatal("terminal sender did not finish")
		}
	})

	t.Run("full channel stops when invalidation cancels active read", func(t *testing.T) {
		ch := make(chan codexWebsocketRead, 1)
		ch <- codexWebsocketRead{payload: []byte("queued")}
		done := make(chan struct{})
		invalidated := sendTerminalWebsocketRead(ch, done, codexWebsocketRead{err: terminalErr}, func() {
			close(done)
		})
		if !invalidated {
			t.Fatal("full channel should report early invalidation")
		}
		if len(ch) != 1 {
			t.Fatalf("channel length = %d, want queued payload only", len(ch))
		}
	})
}

func TestMapCodexWebsocketWriteErrorStopsRetryForMessageTooBig(t *testing.T) {
	networkWriteErr := errors.New("write: broken pipe")
	tests := []struct {
		name       string
		closeCode  int
		writeErr   error
		wantStatus int
		wantRetry  bool
	}{
		{
			name:       "close sent after message too big is request scoped",
			closeCode:  websocket.CloseMessageTooBig,
			writeErr:   websocket.ErrCloseSent,
			wantStatus: http.StatusRequestEntityTooLarge,
			wantRetry:  false,
		},
		{
			name:       "network write error after message too big is request scoped",
			closeCode:  websocket.CloseMessageTooBig,
			writeErr:   networkWriteErr,
			wantStatus: http.StatusRequestEntityTooLarge,
			wantRetry:  false,
		},
		{
			name:      "other close keeps stale connection retry",
			closeCode: websocket.CloseNormalClosure,
			writeErr:  websocket.ErrCloseSent,
			wantRetry: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sess := &codexWebsocketSession{}
			conn := &websocket.Conn{}
			sess.resetUpstreamDisconnectError(conn)
			sess.setUpstreamDisconnectError(conn, &websocket.CloseError{Code: tt.closeCode})

			mappedErr := mapCodexWebsocketWriteError(sess, conn, tt.writeErr)
			if got := shouldRetryCodexWebsocketSend(mappedErr); got != tt.wantRetry {
				t.Fatalf("shouldRetryCodexWebsocketSend() = %v, want %v; err=%v", got, tt.wantRetry, mappedErr)
			}
			if tt.wantStatus == 0 {
				if !errors.Is(mappedErr, tt.writeErr) {
					t.Fatalf("mapped error = %v, want %v", mappedErr, tt.writeErr)
				}
				return
			}
			statusErr, ok := mappedErr.(interface{ StatusCode() int })
			if !ok || statusErr.StatusCode() != tt.wantStatus {
				t.Fatalf("mapped status = %v, want %d; err=%v", statusErr, tt.wantStatus, mappedErr)
			}
			requestErr, ok := mappedErr.(interface{ IsRequestScoped() bool })
			if !ok || !requestErr.IsRequestScoped() {
				t.Fatalf("mapped error should be request scoped, got %T", mappedErr)
			}
		})
	}
}

func TestMapCodexWebsocketWriteErrorDoesNotReusePriorConnectionClose(t *testing.T) {
	sess := &codexWebsocketSession{}
	priorConn := &websocket.Conn{}
	replacementConn := &websocket.Conn{}

	sess.resetUpstreamDisconnectError(priorConn)
	sess.setUpstreamDisconnectError(priorConn, &websocket.CloseError{Code: websocket.CloseMessageTooBig})
	priorErr := mapCodexWebsocketWriteError(sess, priorConn, websocket.ErrCloseSent)
	if shouldRetryCodexWebsocketSend(priorErr) {
		t.Fatalf("prior connection 1009 should not retry, got %v", priorErr)
	}

	sess.resetUpstreamDisconnectError(replacementConn)
	// A late close callback from the prior connection must not overwrite the
	// replacement connection's close state.
	sess.setUpstreamDisconnectError(priorConn, &websocket.CloseError{Code: websocket.CloseMessageTooBig})
	sess.setUpstreamDisconnectError(replacementConn, &websocket.CloseError{Code: websocket.CloseNormalClosure})
	replacementErr := mapCodexWebsocketWriteError(sess, replacementConn, websocket.ErrCloseSent)
	if !errors.Is(replacementErr, websocket.ErrCloseSent) {
		t.Fatalf("replacement connection error = %v, want %v", replacementErr, websocket.ErrCloseSent)
	}
	if !shouldRetryCodexWebsocketSend(replacementErr) {
		t.Fatalf("replacement connection should keep stale-connection retry, got %v", replacementErr)
	}
}

func TestBuildCodexResponsesWebsocketURLRequiresHTTPURL(t *testing.T) {
	if got, err := buildCodexResponsesWebsocketURL("https://example.com/backend/responses"); err != nil || got != "wss://example.com/backend/responses" {
		t.Fatalf("https URL = %q, %v; want wss URL", got, err)
	}
	if _, err := buildCodexResponsesWebsocketURL("ftp://example.com/responses"); err == nil {
		t.Fatalf("expected unsupported scheme error")
	}
	if _, err := buildCodexResponsesWebsocketURL("https:///responses"); err == nil {
		t.Fatalf("expected empty host error")
	}
}

func TestParseCodexWebsocketErrorMarksConnectionLimitRetryable(t *testing.T) {
	err, ok := parseCodexWebsocketError([]byte(`{"type":"error","status":429,"error":{"code":"websocket_connection_limit_reached","message":"too many websockets"},"headers":{"retry-after":"1"}}`))
	if !ok {
		t.Fatalf("expected websocket error")
	}
	status, ok := err.(interface{ StatusCode() int })
	if !ok || status.StatusCode() != http.StatusTooManyRequests {
		t.Fatalf("status = %#v, want 429", err)
	}
	retryable, ok := err.(interface{ RetryAfter() *time.Duration })
	if !ok || retryable.RetryAfter() == nil {
		t.Fatalf("expected retryable websocket connection limit error")
	}
	if got := *retryable.RetryAfter(); got != time.Second {
		t.Fatalf("retryAfter = %v, want explicit provider 1s", got)
	}
	withHeaders, ok := err.(interface{ Headers() http.Header })
	if !ok || withHeaders.Headers().Get("retry-after") != "1" {
		t.Fatalf("headers = %#v, want retry-after", err)
	}
}

func TestParseCodexWebsocketErrorUsesUsageLimitRetryMetadata(t *testing.T) {
	err, ok := parseCodexWebsocketError([]byte(`{"type":"error","status":429,"body":{"error":{"type":"usage_limit_reached","message":"usage limit reached","resets_in_seconds":7}}}`))
	if !ok {
		t.Fatalf("expected websocket error")
	}

	retryable, ok := err.(interface{ RetryAfter() *time.Duration })
	if !ok || retryable.RetryAfter() == nil {
		t.Fatalf("expected retryable usage limit websocket error")
	}
	if got := *retryable.RetryAfter(); got != 7*time.Second {
		t.Fatalf("retryAfter = %v, want 7s", got)
	}
}

func TestParseCodexWebsocketErrorPreservesWrappedBodyAndHeaders(t *testing.T) {
	err, ok := parseCodexWebsocketError([]byte(`{"type":"error","status":429,"body":{"error":{"code":"websocket_connection_limit_reached","type":"server_error","message":"too many websocket connections"}},"headers":{"x-request-id":"req-1"}}`))
	if !ok {
		t.Fatalf("expected websocket error")
	}

	parsed := ojson.Parse(err.Error())
	if got := parsed.Get("status").Int(); got != http.StatusTooManyRequests {
		t.Fatalf("wrapped status = %d, want 429; payload=%s", got, err.Error())
	}
	if got := parsed.Get("body.error.code").String(); got != "websocket_connection_limit_reached" {
		t.Fatalf("wrapped body error code = %s, want websocket_connection_limit_reached; payload=%s", got, err.Error())
	}
	if got := parsed.Get("error.code").String(); got != "websocket_connection_limit_reached" {
		t.Fatalf("surface error code = %s, want websocket_connection_limit_reached; payload=%s", got, err.Error())
	}
	retryable, ok := err.(interface{ RetryAfter() *time.Duration })
	if !ok || retryable.RetryAfter() == nil {
		t.Fatalf("expected body.error.code websocket connection limit to be retryable")
	}
	withHeaders, ok := err.(interface{ Headers() http.Header })
	if !ok || withHeaders.Headers().Get("x-request-id") != "req-1" {
		t.Fatalf("headers = %#v, want x-request-id", err)
	}
}

func TestNewProxyAwareWebsocketDialerDirectDisablesProxy(t *testing.T) {
	t.Parallel()

	dialer := newProxyAwareWebsocketDialer(
		context.Background(),
		&config.Config{SDKConfig: sdkconfig.SDKConfig{ProxyURL: "http://global-proxy.example.com:8080"}},
		&cliproxyauth.Auth{ProxyURL: "direct"},
	)

	if dialer.Proxy != nil {
		t.Fatal("expected websocket proxy function to be nil for direct mode")
	}
}

func TestCodexWebsockets_LastEventAndTerminalTracking(t *testing.T) {
	conn1 := &websocket.Conn{}
	conn2 := &websocket.Conn{}
	sess := &codexWebsocketSession{sessionID: "track-session"}

	// Initially empty
	sess.resetUpstreamDisconnectError(conn1)
	if got := sess.getLastEventType(conn1); got != "" {
		t.Fatalf("initial lastEventType = %q, want empty", got)
	}

	// Non-terminal event
	sess.setLastEventType(conn1, "response.output_item.added")
	if got := sess.getLastEventType(conn1); got != "response.output_item.added" {
		t.Fatalf("lastEventType = %q, want response.output_item.added", got)
	}
	if isTerminalEvent(sess.getLastEventType(conn1)) {
		t.Fatalf("output_item.added should not be terminal")
	}

	// Terminal event
	sess.setLastEventType(conn1, "response.completed")
	if got := sess.getLastEventType(conn1); got != "response.completed" {
		t.Fatalf("lastEventType = %q, want response.completed", got)
	}
	if !isTerminalEvent(sess.getLastEventType(conn1)) {
		t.Fatalf("response.completed must be terminal")
	}

	// Reset for new connection resets tracking
	sess.resetUpstreamDisconnectError(conn2)
	if got := sess.getLastEventType(conn2); got != "" {
		t.Fatalf("reconnected lastEventType = %q, want empty", got)
	}
	// Old conn should not match
	if got := sess.getLastEventType(conn1); got != "" {
		t.Fatalf("stale conn lastEventType = %q, want empty", got)
	}
}

func TestOrdinaryCodexSameSessionGrantAndGeneration(t *testing.T) {
	e := NewCodexWebsocketsExecutor(&config.Config{})
	e.store = &codexWebsocketSessionStore{}
	const id = "public-execution-id"
	disconnect := e.UpstreamDisconnectChan(id)
	original := e.getOrCreateSession(id)
	scope := ordinaryWebsocketScope("tenant-A", "credential-A", "bearer-1", "grant-A", "https://one.invalid", "direct")
	release, err := acquireOrdinaryCodexTurn(t.Context(), original, scope, "credential-A")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	for _, other := range []string{
		ordinaryWebsocketScope("tenant-B", "credential-A", "bearer-1", "grant-A", "https://one.invalid", "direct"),
		ordinaryWebsocketScope("tenant-A", "credential-A", "bearer-1", "grant-B", "https://one.invalid", "direct"),
		ordinaryWebsocketScope("tenant-A", "credential-A", "bearer-1", "grant-A", "https://two.invalid", "direct"),
		ordinaryWebsocketScope("tenant-A", "credential-A", "bearer-1", "grant-A", "https://one.invalid", "http://fixture.invalid:8080"),
	} {
		if r, err := acquireOrdinaryCodexTurn(t.Context(), original, other, "credential-A"); err == nil {
			r()
			t.Fatal("different scope claimed the existing generation")
		}
	}
	untouched := e.UpstreamDisconnectChan("separate-execution-id")
	e.CloseAuthExecutionSessions("credential-A")
	select {
	case err := <-disconnect:
		if !errors.Is(err, errCodexWebsocketSessionClosed) {
			t.Fatalf("unexpected disconnect event: %v", err)
		}
	case <-t.Context().Done():
		t.Fatal("bare disconnect subscriber was not notified")
	}
	select {
	case err := <-untouched:
		t.Fatalf("unrelated execution closed: %v", err)
	default:
	}
	next := e.getOrCreateSession(id)
	if next == original || e.UpstreamDisconnectChan(id) == disconnect {
		t.Fatal("old generation was reused")
	}
	if _, err := acquireOrdinaryCodexTurn(t.Context(), original, scope, "credential-A"); !errors.Is(err, errCodexWebsocketSessionClosed) {
		t.Fatal(err)
	}
	nextCh := e.UpstreamDisconnectChan(id)
	e.CloseExecutionSession(id)
	select {
	case err := <-nextCh:
		if !errors.Is(err, errCodexWebsocketSessionClosed) {
			t.Fatal(err)
		}
	case <-t.Context().Done():
		t.Fatal("new generation was not closed")
	}
}

func contextWithGinHeaders(headers map[string]string) context.Context {
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Request = httptest.NewRequest(http.MethodPost, "/", nil)
	ginCtx.Request.Header = make(http.Header, len(headers))
	for key, value := range headers {
		ginCtx.Request.Header.Set(key, value)
	}
	return context.WithValue(context.Background(), "gin", ginCtx)
}

func TestCodexAutoExecutorRequiredUpstreamWebsocketRejectsHTTPFallback(t *testing.T) {
	exec := NewCodexAutoExecutor(&config.Config{SDKConfig: config.SDKConfig{DisableImageGeneration: config.DisableImageGenerationAll}})
	auth := &cliproxyauth.Auth{
		ID:       "codex-http-only",
		Provider: "codex",
		Attributes: map[string]string{
			"api_key": "sk-test",
		},
	}
	ctx := cliproxyexecutor.WithRequiredUpstreamWebsocket(
		cliproxyexecutor.WithDownstreamWebsocket(context.Background()),
	)
	_, errExecute := exec.ExecuteStream(ctx, auth, cliproxyexecutor.Request{
		Model:   "gpt-5.4",
		Payload: []byte(`{"model":"gpt-5.4","previous_response_id":"resp-1","input":[{"type":"message","id":"msg-2"}]}`),
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai-response")})
	if errExecute == nil {
		t.Fatal("ExecuteStream() error = nil, want replay-required error")
	}
	statusErr, ok := errExecute.(interface{ StatusCode() int })
	if !ok || statusErr.StatusCode() != http.StatusUpgradeRequired {
		t.Fatalf("ExecuteStream() error = %T %v, want status 426", errExecute, errExecute)
	}
	if got := ojson.Get(errExecute.Error(), "error.code").String(); got != "upstream_http_replay_required" {
		t.Fatalf("ExecuteStream() error code = %q, want upstream_http_replay_required", got)
	}
	requestScoped, ok := errExecute.(cliproxyexecutor.RequestScopedError)
	if !ok || !requestScoped.IsRequestScoped() {
		t.Fatalf("ExecuteStream() error = %T, want request-scoped replay signal", errExecute)
	}
}

func TestCodexWebsocketMissingRequiredSessionDoesNotMarkUpstreamAttempt(t *testing.T) {
	exec := NewCodexWebsocketsExecutor(&config.Config{SDKConfig: config.SDKConfig{DisableImageGeneration: config.DisableImageGenerationAll}})
	exec.store = &codexWebsocketSessionStore{sessions: make(map[string]*codexWebsocketSession)}
	auth := &cliproxyauth.Auth{
		ID:       "codex-required-session",
		ProxyURL: "direct",
		Provider: "codex",
		Attributes: map[string]string{
			"api_key":  "sk-test",
			"base_url": "https://fixture.invalid",
		},
	}
	ctx := cliproxyexecutor.WithUpstreamAttemptTracker(
		cliproxyexecutor.WithRequiredUpstreamWebsocket(context.Background()),
	)
	_, errExecute := exec.Execute(ctx, auth, cliproxyexecutor.Request{
		Model:   "gpt-5.4",
		Payload: []byte(`{"model":"gpt-5.4","previous_response_id":"resp-1","input":[{"type":"message","role":"user","content":"hello"}]}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FormatOpenAIResponse,
		Metadata: map[string]any{
			cliproxyexecutor.ExecutionSessionMetadataKey: "missing-codex-session",
		},
	})
	if !cliproxyexecutor.IsUpstreamWebsocketReplayRequired(errExecute) {
		t.Fatalf("Execute() error = %T %v, want replay-required", errExecute, errExecute)
	}
	if cliproxyexecutor.UpstreamAttempted(ctx) {
		t.Fatal("missing retained websocket connection was marked as an upstream attempt")
	}
}

type terminalFailureLifecycle struct {
	active atomic.Bool
	ends   atomic.Int32
}

func newTerminalFailureLifecycle() *terminalFailureLifecycle {
	lifecycle := &terminalFailureLifecycle{}
	lifecycle.active.Store(true)
	return lifecycle
}

func (*terminalFailureLifecycle) Bind(func() error) error { return nil }
func (l *terminalFailureLifecycle) End(string) {
	l.ends.Add(1)
	l.active.Store(false)
}
func (*terminalFailureLifecycle) Retain() {}

type rejectingExecutionLifecycle struct{}

func (rejectingExecutionLifecycle) Bind(func() error) error {
	return errors.New("lifecycle bind rejected")
}
func (rejectingExecutionLifecycle) End(string) {}

func TestCodexWebsocketsExecuteStreamHandshakeErrorReturnsWithoutLockingSession(t *testing.T) {
	server := newCodexPipeServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"unauthorized"}}`))
	}))
	defer server.Close()

	exec := NewCodexWebsocketsExecutor(&config.Config{SDKConfig: config.SDKConfig{DisableImageGeneration: config.DisableImageGenerationAll}})
	exec.directDialContext = server.dial
	const executionSessionID = "ws-handshake-error-session"
	t.Cleanup(func() { exec.CloseExecutionSession(executionSessionID) })
	auth := &cliproxyauth.Auth{ProxyURL: "direct",
		ID:       "codex-test",
		Provider: "codex",
		Attributes: map[string]string{
			"api_key":  "sk-test",
			"base_url": server.URL,
		},
	}
	opts := cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai-response"),
		Metadata: map[string]any{
			cliproxyexecutor.ExecutionSessionMetadataKey: executionSessionID,
		},
	}

	for i := 0; i < 2; i++ {
		attemptCtx := cliproxyexecutor.WithUpstreamAttemptTracker(fixtureCodexTenantContext(t.Context()))
		done := make(chan error, 1)
		go func(ctx context.Context) {
			_, errExecute := exec.ExecuteStream(ctx, auth, cliproxyexecutor.Request{
				Model:   "gpt-5.4",
				Payload: []byte(`{"model":"gpt-5.4","input":[{"type":"message","id":"msg-1"}]}`),
			}, opts)
			done <- errExecute
		}(attemptCtx)
		select {
		case errExecute := <-done:
			statusErr, ok := errExecute.(interface{ StatusCode() int })
			if !ok || statusErr.StatusCode() != http.StatusUnauthorized {
				t.Fatalf("attempt %d error = %T %v, want status 401", i+1, errExecute, errExecute)
			}
			if !cliproxyexecutor.UpstreamAttempted(attemptCtx) {
				t.Fatalf("attempt %d websocket handshake was not marked as upstream", i+1)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("attempt %d timed out; execution session remained locked", i+1)
		}
	}
}

func TestCodexWebsocketsExecuteStreamPassesThroughUpstreamWebsocketPayloadForDownstreamWebsocket(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	capturedPayload := make(chan []byte, 1)
	delta := []byte(`{"type":"response.output_text.delta","delta":"hello"}`)
	completed := []byte(`{"type":"response.completed","response":{"id":"resp-1","output":[],"usage":{"input_tokens":0,"output_tokens":0,"total_tokens":0}}}`)
	server := newCodexPipeServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade websocket: %v", err)
			return
		}
		defer func() { _ = conn.Close() }()

		_, payload, errRead := conn.ReadMessage()
		if errRead != nil {
			t.Errorf("read upstream websocket message: %v", errRead)
			return
		}
		capturedPayload <- bytes.Clone(payload)
		if errWrite := conn.WriteMessage(websocket.TextMessage, delta); errWrite != nil {
			t.Errorf("write delta websocket message: %v", errWrite)
			return
		}
		if errWrite := conn.WriteMessage(websocket.TextMessage, completed); errWrite != nil {
			t.Errorf("write completed websocket message: %v", errWrite)
			return
		}
	}))
	defer server.Close()

	exec := NewCodexWebsocketsExecutor(&config.Config{SDKConfig: config.SDKConfig{DisableImageGeneration: config.DisableImageGenerationAll}})
	exec.directDialContext = server.dial
	auth := &cliproxyauth.Auth{ProxyURL: "direct", Attributes: map[string]string{"api_key": "sk-test", "base_url": server.URL}}
	req := cliproxyexecutor.Request{
		Model:   "gpt-5-codex",
		Payload: []byte(`{"model":"prolite/gpt-5-codex","input":[{"type":"additional_tools","role":"developer","tools":[{"type":"custom","name":"exec"}]},{"type":"message","role":"user","content":"hello"}],"parallel_tool_calls":true}`),
	}
	opts := cliproxyexecutor.Options{
		SourceFormat:   sdktranslator.FromString("openai-response"),
		ResponseFormat: sdktranslator.FromString("openai-response"),
	}
	ctx := cliproxyexecutor.WithUpstreamAttemptTracker(
		cliproxyexecutor.WithDownstreamWebsocket(fixtureCodexTenantContext(t.Context())),
	)

	result, err := exec.ExecuteStream(ctx, auth, req, opts)
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}
	if !cliproxyexecutor.UpstreamAttempted(ctx) {
		t.Fatal("websocket write did not mark an upstream attempt")
	}

	select {
	case chunk, ok := <-result.Chunks:
		if !ok {
			t.Fatal("stream closed before first chunk")
		}
		if chunk.Err != nil {
			t.Fatalf("first chunk error = %v", chunk.Err)
		}
		if !bytes.Equal(bytes.TrimSpace(chunk.Payload), delta) {
			t.Fatalf("first chunk = %q, want raw upstream websocket payload %q", chunk.Payload, delta)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for first stream chunk")
	}

	select {
	case payload := <-capturedPayload:
		if got := ojson.GetBytes(payload, "model").String(); got != "gpt-5-codex" {
			t.Fatalf("upstream model = %s, want gpt-5-codex; payload=%s", got, payload)
		}
		parallelToolCalls := ojson.GetBytes(payload, "parallel_tool_calls")
		if !parallelToolCalls.Exists() || !parallelToolCalls.Bool() {
			t.Fatalf("non-lite parallel_tool_calls should be preserved: %s", payload)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for upstream websocket payload")
	}
}

func TestCodexWebsocketsExecuteStreamPropagatesUpstreamErrorForDownstreamWebsocket(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	errorPayload := []byte(`{"type":"error","status":429,"error":{"code":"websocket_connection_limit_reached","message":"too many websockets"}}`)
	server := newCodexPipeServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade websocket: %v", err)
			return
		}
		defer func() { _ = conn.Close() }()

		if _, _, errRead := conn.ReadMessage(); errRead != nil {
			t.Errorf("read upstream websocket message: %v", errRead)
			return
		}
		if errWrite := conn.WriteMessage(websocket.TextMessage, errorPayload); errWrite != nil {
			t.Errorf("write error websocket message: %v", errWrite)
			return
		}
	}))
	defer server.Close()

	exec := NewCodexWebsocketsExecutor(&config.Config{SDKConfig: config.SDKConfig{DisableImageGeneration: config.DisableImageGenerationAll}})
	exec.directDialContext = server.dial
	auth := &cliproxyauth.Auth{ProxyURL: "direct", Attributes: map[string]string{"api_key": "sk-test", "base_url": server.URL}}
	req := cliproxyexecutor.Request{
		Model:   "gpt-5-codex",
		Payload: []byte(`{"model":"gpt-5-codex","input":[{"type":"message","role":"user","content":"hello"}]}`),
	}
	opts := cliproxyexecutor.Options{
		SourceFormat:   sdktranslator.FromString("openai-response"),
		ResponseFormat: sdktranslator.FromString("openai-response"),
	}
	ctx := cliproxyexecutor.WithDownstreamWebsocket(fixtureCodexTenantContext(t.Context()))

	result, err := exec.ExecuteStream(ctx, auth, req, opts)
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}

	select {
	case chunk, ok := <-result.Chunks:
		if !ok {
			t.Fatal("stream closed before error chunk")
		}
		if len(bytes.TrimSpace(chunk.Payload)) != 0 {
			t.Fatalf("error chunk payload = %q, want empty", chunk.Payload)
		}
		if chunk.Err == nil {
			t.Fatal("error chunk Err = nil, want upstream error")
		}
		statusErr, ok := chunk.Err.(interface{ StatusCode() int })
		if !ok {
			t.Fatalf("error type %T does not expose StatusCode", chunk.Err)
		}
		if got := statusErr.StatusCode(); got != http.StatusTooManyRequests {
			t.Fatalf("status = %d, want %d", got, http.StatusTooManyRequests)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for error stream chunk")
	}
}

func TestCodexWebsocketsExecuteStreamMapsMessageTooBigClose(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	server := newCodexPipeServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade websocket: %v", err)
			return
		}
		defer func() { _ = conn.Close() }()

		if _, _, errRead := conn.ReadMessage(); errRead != nil {
			t.Errorf("read upstream websocket message: %v", errRead)
			return
		}
		deadline := time.Now().Add(time.Second)
		closeMessage := websocket.FormatCloseMessage(websocket.CloseMessageTooBig, "message too big")
		if errWrite := conn.WriteControl(websocket.CloseMessage, closeMessage, deadline); errWrite != nil {
			t.Errorf("write close websocket message: %v", errWrite)
			return
		}
	}))
	defer server.Close()

	exec := NewCodexWebsocketsExecutor(&config.Config{SDKConfig: config.SDKConfig{DisableImageGeneration: config.DisableImageGenerationAll}})
	exec.directDialContext = server.dial
	auth := &cliproxyauth.Auth{ProxyURL: "direct", Attributes: map[string]string{"api_key": "sk-test", "base_url": server.URL}}
	req := cliproxyexecutor.Request{
		Model:   "gpt-5-codex",
		Payload: []byte(`{"model":"gpt-5-codex","input":[{"type":"message","role":"user","content":"hello"}]}`),
	}
	opts := cliproxyexecutor.Options{
		SourceFormat:   sdktranslator.FromString("openai-response"),
		ResponseFormat: sdktranslator.FromString("openai-response"),
	}

	result, err := exec.ExecuteStream(fixtureCodexTenantContext(t.Context()), auth, req, opts)
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}

	select {
	case chunk, ok := <-result.Chunks:
		if !ok {
			t.Fatal("stream closed before error chunk")
		}
		if chunk.Err == nil {
			t.Fatal("error chunk Err = nil, want message-too-big error")
		}
		statusErr, ok := chunk.Err.(interface{ StatusCode() int })
		if !ok {
			t.Fatalf("error type %T does not expose StatusCode", chunk.Err)
		}
		if got := statusErr.StatusCode(); got != http.StatusRequestEntityTooLarge {
			t.Fatalf("status = %d, want %d", got, http.StatusRequestEntityTooLarge)
		}
		if got := ojson.Get(chunk.Err.Error(), "error.code").String(); got != "message_too_big" {
			t.Fatalf("error code = %q, want message_too_big; err=%v", got, chunk.Err)
		}
		requestErr, ok := chunk.Err.(interface{ IsRequestScoped() bool })
		if !ok || !requestErr.IsRequestScoped() {
			t.Fatalf("message-too-big error should be request scoped, got %T", chunk.Err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for error stream chunk")
	}
}

func TestCodexWebsocketsUpstreamDisconnectChanSignalsOnInvalidate(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	server := newCodexPipeServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade websocket: %v", err)
			return
		}
		defer func() { _ = conn.Close() }()
		for {
			if _, _, errRead := conn.ReadMessage(); errRead != nil {
				return
			}
		}
	}))
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	conn, _, err := (&websocket.Dialer{NetDialContext: server.dial}).DialContext(t.Context(), wsURL, nil)
	if err != nil {
		t.Fatalf("dial websocket: %v", err)
	}
	defer func() { _ = conn.Close() }()

	exec := NewCodexWebsocketsExecutor(&config.Config{})
	exec.directDialContext = server.dial
	exec.store = &codexWebsocketSessionStore{sessions: make(map[string]*codexWebsocketSession)}
	sessionID := "sess-1"
	disconnectCh := exec.UpstreamDisconnectChan(sessionID)
	if disconnectCh == nil {
		t.Fatal("expected disconnect channel")
	}

	sess := exec.getOrCreateSession(sessionID)
	if sess == nil {
		t.Fatal("expected session")
	}
	sess.connMu.Lock()
	sess.conn = conn
	sess.authID = "auth-1"
	sess.wsURL = "ws://example.test/responses"
	sess.readerConn = conn
	sess.connMu.Unlock()

	upstreamErr := errors.New("upstream gone")
	exec.invalidateUpstreamConn(sess, conn, "test_invalidate", upstreamErr)

	select {
	case errRead, ok := <-disconnectCh:
		if !ok {
			t.Fatal("expected disconnect channel to deliver error before closing")
		}
		if !errors.Is(errRead, errCodexWebsocketSessionClosed) || strings.Contains(errRead.Error(), upstreamErr.Error()) {
			t.Fatalf("disconnect observer leaked or lost the public closure signal: %v", errRead)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for disconnect signal")
	}
}

func TestCodexWebsocketUpgradeRequiredDoesNotFallbackToHTTPWithLifecycle(t *testing.T) {
	var httpFallbackCalls atomic.Int32
	server := newCodexPipeServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			httpFallbackCalls.Add(1)
			http.Error(w, "unexpected HTTP fallback", http.StatusInternalServerError)
			return
		}
		http.Error(w, "websocket upgrade required", http.StatusUpgradeRequired)
	}))
	defer server.Close()

	exec := NewCodexWebsocketsExecutor(&config.Config{SDKConfig: config.SDKConfig{DisableImageGeneration: config.DisableImageGenerationAll}})
	exec.directDialContext = server.dial
	auth := &cliproxyauth.Auth{ProxyURL: "direct", ID: "auth-a", Provider: "codex", Attributes: map[string]string{"api_key": "sk-test", "base_url": server.URL}}
	req := cliproxyexecutor.Request{Model: "gpt-5-codex", Payload: []byte(`{"model":"gpt-5-codex","input":[{"type":"message","role":"user","content":"hello"}]}`)}
	opts := cliproxyexecutor.Options{
		SourceFormat:       sdktranslator.FromString("openai-response"),
		ResponseFormat:     sdktranslator.FromString("openai-response"),
		ExecutionLifecycle: newTerminalFailureLifecycle(),
	}

	if _, errExecute := exec.ExecuteStream(fixtureCodexTenantContext(t.Context()), auth, req, opts); errExecute == nil {
		t.Fatal("ExecuteStream() error = nil, want failed Home lifecycle attempt")
	}
	if got := httpFallbackCalls.Load(); got != 0 {
		t.Fatalf("HTTP fallback calls = %d, want 0 with an execution lifecycle", got)
	}
}

func TestCodexWebsocketHandshakeFailureReleasesSessionRequestLock(t *testing.T) {
	for _, statusCode := range []int{http.StatusUpgradeRequired, http.StatusBadGateway} {
		t.Run(http.StatusText(statusCode), func(t *testing.T) {
			server := newCodexPipeServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				http.Error(w, "upstream rejected websocket", statusCode)
			}))
			defer server.Close()

			exec := NewCodexWebsocketsExecutor(&config.Config{SDKConfig: config.SDKConfig{DisableImageGeneration: config.DisableImageGenerationAll}})
			exec.directDialContext = server.dial
			exec.store = &codexWebsocketSessionStore{sessions: make(map[string]*codexWebsocketSession)}
			auth := &cliproxyauth.Auth{ProxyURL: "direct", ID: "auth-a", Provider: "codex", Attributes: map[string]string{"api_key": "sk-test", "base_url": server.URL}}
			req := cliproxyexecutor.Request{Model: "gpt-5-codex", Payload: []byte(`{"model":"gpt-5-codex","input":[{"type":"message","role":"user","content":"hello"}]}`)}
			opts := cliproxyexecutor.Options{
				SourceFormat:   sdktranslator.FromString("openai-response"),
				ResponseFormat: sdktranslator.FromString("openai-response"),
				Metadata: map[string]any{
					cliproxyexecutor.ExecutionSessionMetadataKey: "failed-handshake",
				},
			}

			_, _ = exec.ExecuteStream(fixtureCodexTenantContext(t.Context()), auth, req, opts)
			sess := exec.getOrCreateSession("failed-handshake")
			acquired := make(chan struct{})
			go func() {
				sess.reqMu.Lock()
				close(acquired)
				sess.reqMu.Unlock()
			}()
			select {
			case <-acquired:
			case <-time.After(time.Second):
				t.Fatal("websocket handshake failure left the session request lock held")
			}
		})
	}
}

func TestCodexWebsocketTerminalFailureInvalidatesRetainedLifecycle(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	var connections atomic.Int32
	firstRelease := make(chan struct{})
	server := newCodexPipeServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, errUpgrade := upgrader.Upgrade(w, r, nil)
		if errUpgrade != nil {
			t.Errorf("upgrade websocket: %v", errUpgrade)
			return
		}
		defer func() { _ = conn.Close() }()
		connection := connections.Add(1)
		if _, _, errRead := conn.ReadMessage(); errRead != nil {
			return
		}
		terminal := []byte(`{"type":"response.failed","response":{"error":{"type":"authentication_error","code":"invalid_api_key","message":"Invalid token."}}}`)
		if errWrite := conn.WriteMessage(websocket.TextMessage, terminal); errWrite != nil {
			t.Errorf("write terminal response: %v", errWrite)
		}
		if connection == 1 {
			<-firstRelease
		}
	}))
	defer server.Close()

	exec := NewCodexWebsocketsExecutor(&config.Config{SDKConfig: config.SDKConfig{DisableImageGeneration: config.DisableImageGenerationAll}})
	exec.directDialContext = server.dial
	exec.store = &codexWebsocketSessionStore{sessions: make(map[string]*codexWebsocketSession)}
	auth := &cliproxyauth.Auth{ProxyURL: "direct", ID: "auth-a", Provider: "codex", Attributes: map[string]string{"api_key": "sk-test", "base_url": server.URL}}
	req := cliproxyexecutor.Request{Model: "gpt-5-codex", Payload: []byte(`{"model":"gpt-5-codex","input":[{"type":"message","role":"user","content":"hello"}]}`)}
	opts := cliproxyexecutor.Options{
		SourceFormat:       sdktranslator.FromString("openai-response"),
		ResponseFormat:     sdktranslator.FromString("openai-response"),
		ExecutionLifecycle: newTerminalFailureLifecycle(),
		Metadata: map[string]any{
			cliproxyexecutor.ExecutionSessionMetadataKey: "terminal-failure",
		},
	}

	result, errExecute := exec.ExecuteStream(fixtureCodexTenantContext(t.Context()), auth, req, opts)
	if errExecute != nil {
		t.Fatalf("first ExecuteStream() error = %v", errExecute)
	}
	for chunk := range result.Chunks {
		if chunk.Err == nil {
			continue
		}
	}
	lifecycle := opts.ExecutionLifecycle.(*terminalFailureLifecycle)
	if lifecycle.active.Load() {
		t.Fatal("terminal failure left the retained lifecycle active")
	}
	if got := lifecycle.ends.Load(); got != 1 {
		t.Fatalf("retained lifecycle End calls = %d, want 1", got)
	}
	sess := exec.getOrCreateSession("terminal-failure")
	sess.connMu.Lock()
	connected := sess.conn != nil
	sess.connMu.Unlock()
	if connected {
		t.Fatal("terminal failure left the upstream session connection cached")
	}
	close(firstRelease)

	opts.ExecutionLifecycle = newTerminalFailureLifecycle()
	result, errExecute = exec.ExecuteStream(fixtureCodexTenantContext(t.Context()), auth, req, opts)
	if errExecute != nil {
		t.Fatalf("second ExecuteStream() error = %v", errExecute)
	}
	for range result.Chunks {
	}
	if got := connections.Load(); got != 2 {
		t.Fatalf("websocket connections = %d, want 2 after terminal invalidation", got)
	}
}

func TestCodexWebsocketNonstreamLifecycleBindFailureDetachesConnection(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	var connections atomic.Int32
	closed := make(chan struct{}, 1)
	server := newCodexPipeServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, errUpgrade := upgrader.Upgrade(w, r, nil)
		if errUpgrade != nil {
			t.Errorf("upgrade websocket: %v", errUpgrade)
			return
		}
		connection := connections.Add(1)
		defer func() {
			_ = conn.Close()
			if connection == 1 {
				closed <- struct{}{}
			}
		}()
		if _, _, errRead := conn.ReadMessage(); errRead != nil {
			return
		}
		completed := []byte(`{"type":"response.completed","response":{"id":"resp-1","output":[],"usage":{"input_tokens":0,"output_tokens":0,"total_tokens":0}}}`)
		if errWrite := conn.WriteMessage(websocket.TextMessage, completed); errWrite != nil {
			t.Errorf("write completed response: %v", errWrite)
		}
	}))
	defer server.Close()

	exec := NewCodexWebsocketsExecutor(&config.Config{SDKConfig: config.SDKConfig{DisableImageGeneration: config.DisableImageGenerationAll}})
	exec.directDialContext = server.dial
	exec.store = &codexWebsocketSessionStore{sessions: make(map[string]*codexWebsocketSession)}
	auth := &cliproxyauth.Auth{ProxyURL: "direct", ID: "auth-a", Provider: "codex", Attributes: map[string]string{"api_key": "sk-test", "base_url": server.URL}}
	req := cliproxyexecutor.Request{Model: "gpt-5-codex", Payload: []byte(`{"model":"gpt-5-codex","input":[{"type":"message","role":"user","content":"hello"}]}`)}
	opts := cliproxyexecutor.Options{
		SourceFormat:       sdktranslator.FromString("openai-response"),
		ResponseFormat:     sdktranslator.FromString("openai-response"),
		ExecutionLifecycle: rejectingExecutionLifecycle{},
		Metadata: map[string]any{
			cliproxyexecutor.ExecutionSessionMetadataKey: "nonstream-bind-failed",
		},
	}
	attemptCtx := cliproxyexecutor.WithUpstreamAttemptTracker(fixtureCodexTenantContext(t.Context()))
	if _, errExecute := exec.Execute(attemptCtx, auth, req, opts); errExecute == nil {
		t.Fatal("Execute() error = nil, want lifecycle bind failure")
	}
	if cliproxyexecutor.UpstreamAttempted(attemptCtx) {
		t.Fatal("successful handshake marked a lifecycle bind failure as an upstream request attempt")
	}
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("nonstream lifecycle bind failure did not close the upstream websocket")
	}
	sess := exec.getOrCreateSession("nonstream-bind-failed")
	sess.connMu.Lock()
	connected := sess.conn != nil
	sess.connMu.Unlock()
	if connected {
		t.Fatal("nonstream lifecycle bind failure left the closed connection attached to the session")
	}

	opts.ExecutionLifecycle = nil
	if _, errExecute := exec.Execute(fixtureCodexTenantContext(t.Context()), auth, req, opts); errExecute != nil {
		t.Fatalf("second Execute() error = %v", errExecute)
	}
	if got := connections.Load(); got != 2 {
		t.Fatalf("websocket connections = %d, want 2 after bind failure", got)
	}
}

func TestCodexWebsocketLifecycleBindFailureReleasesSessionRequestLock(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	closed := make(chan struct{}, 1)
	server := newCodexPipeServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, errUpgrade := upgrader.Upgrade(w, r, nil)
		if errUpgrade != nil {
			t.Errorf("upgrade websocket: %v", errUpgrade)
			return
		}
		defer func() {
			_ = conn.Close()
			closed <- struct{}{}
		}()
		for {
			if _, _, errRead := conn.ReadMessage(); errRead != nil {
				return
			}
		}
	}))
	defer server.Close()

	exec := NewCodexWebsocketsExecutor(&config.Config{SDKConfig: config.SDKConfig{DisableImageGeneration: config.DisableImageGenerationAll}})
	exec.directDialContext = server.dial
	exec.store = &codexWebsocketSessionStore{sessions: make(map[string]*codexWebsocketSession)}
	auth := &cliproxyauth.Auth{ProxyURL: "direct", ID: "auth-a", Provider: "codex", Attributes: map[string]string{"api_key": "sk-test", "base_url": server.URL}}
	req := cliproxyexecutor.Request{Model: "gpt-5-codex", Payload: []byte(`{"model":"gpt-5-codex","input":[{"type":"message","role":"user","content":"hello"}]}`)}
	opts := cliproxyexecutor.Options{
		SourceFormat:       sdktranslator.FromString("openai-response"),
		ResponseFormat:     sdktranslator.FromString("openai-response"),
		ExecutionLifecycle: rejectingExecutionLifecycle{},
		Metadata: map[string]any{
			cliproxyexecutor.ExecutionSessionMetadataKey: "bind-failed",
		},
	}
	attemptCtx := cliproxyexecutor.WithUpstreamAttemptTracker(fixtureCodexTenantContext(t.Context()))
	if _, errExecute := exec.ExecuteStream(attemptCtx, auth, req, opts); errExecute == nil {
		t.Fatal("ExecuteStream() error = nil, want lifecycle bind failure")
	}
	if cliproxyexecutor.UpstreamAttempted(attemptCtx) {
		t.Fatal("successful handshake marked a lifecycle bind failure as an upstream request attempt")
	}
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("lifecycle bind failure did not close the upstream websocket")
	}

	sess := exec.getOrCreateSession("bind-failed")
	acquired := make(chan struct{})
	go func() {
		sess.reqMu.Lock()
		close(acquired)
		sess.reqMu.Unlock()
	}()
	select {
	case <-acquired:
	case <-time.After(time.Second):
		t.Fatal("lifecycle bind failure left the session request lock held")
	}
}

func TestCodexWebsocketsExecuteObservesWebSocketResponseEvents(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	server := newCodexPipeServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Fatalf("upgrade websocket: %v", err)
		}
		defer func() { _ = conn.Close() }()

		if _, _, errRead := conn.ReadMessage(); errRead != nil {
			t.Fatalf("read upstream websocket message: %v", errRead)
		}

		rateLimitMsg := []byte(`{"type":"codex.rate_limits","rate_limits":{"primary":{"used_percent":42}}}`)
		if errWrite := conn.WriteMessage(websocket.TextMessage, rateLimitMsg); errWrite != nil {
			t.Fatalf("write rate limit websocket message: %v", errWrite)
		}

		completed := []byte(`{"type":"response.completed","response":{"id":"resp-1","output":[],"usage":{"input_tokens":0,"output_tokens":0,"total_tokens":0}}}`)
		if errWrite := conn.WriteMessage(websocket.TextMessage, completed); errWrite != nil {
			t.Fatalf("write completed websocket message: %v", errWrite)
		}
	}))
	defer server.Close()

	exec := NewCodexWebsocketsExecutor(&config.Config{})
	exec.directDialContext = server.dial
	auth := &cliproxyauth.Auth{ProxyURL: "direct",
		ID:       "auth-codex-1",
		Label:    "codex-account",
		Provider: "codex",
		Attributes: map[string]string{
			"api_key":   "sk-test",
			"base_url":  server.URL,
			"auth_kind": "oauth",
		},
		Metadata: map[string]any{
			"email": "user@example.com",
		},
	}
	req := cliproxyexecutor.Request{
		Model:   "gpt-5.6-sol",
		Payload: []byte(`{"model":"gpt-5.6-sol","input":[{"role":"user","content":"hello"}]}`),
	}

	var observedEvents []cliproxyexecutor.WebSocketResponseEvent
	opts := cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("codex"),
		Metadata: map[string]any{
			"request_id": "test-req-codex",
		},
		WebSocketResponseObserver: func(_ context.Context, ev cliproxyexecutor.WebSocketResponseEvent) {
			observedEvents = append(observedEvents, ev)
		},
	}

	if _, err := exec.Execute(fixtureCodexTenantContext(t.Context()), auth, req, opts); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	if len(observedEvents) < 2 {
		t.Fatalf("observed %d events, want at least 2", len(observedEvents))
	}

	rateLimitEvent := observedEvents[0]
	if rateLimitEvent.RequestID != "test-req-codex" {
		t.Fatalf("RequestID = %q, want test-req-codex", rateLimitEvent.RequestID)
	}
	if rateLimitEvent.AuthID != "auth-codex-1" || rateLimitEvent.AuthLabel != "codex-account" {
		t.Fatalf("Auth = (%q, %q), want (auth-codex-1, codex-account)", rateLimitEvent.AuthID, rateLimitEvent.AuthLabel)
	}
	if rateLimitEvent.EventType != "codex.rate_limits" {
		t.Fatalf("EventType = %q, want codex.rate_limits", rateLimitEvent.EventType)
	}
	if !bytes.Contains(rateLimitEvent.Payload, []byte(`"used_percent":42`)) {
		t.Fatalf("Payload = %s, want used_percent 42", rateLimitEvent.Payload)
	}
}

func TestCodexWebsocketsExecuteStreamObservesWebSocketResponseEvents(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	server := newCodexPipeServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Fatalf("upgrade websocket: %v", err)
		}
		defer func() { _ = conn.Close() }()

		if _, _, errRead := conn.ReadMessage(); errRead != nil {
			t.Fatalf("read upstream websocket message: %v", errRead)
		}

		rateLimitMsg := []byte(`{"type":"codex.rate_limits","rate_limits":{"primary":{"used_percent":75}}}`)
		if errWrite := conn.WriteMessage(websocket.TextMessage, rateLimitMsg); errWrite != nil {
			t.Fatalf("write rate limit websocket message: %v", errWrite)
		}

		completed := []byte(`{"type":"response.completed","response":{"id":"resp-1","output":[],"usage":{"input_tokens":0,"output_tokens":0,"total_tokens":0}}}`)
		if errWrite := conn.WriteMessage(websocket.TextMessage, completed); errWrite != nil {
			t.Fatalf("write completed websocket message: %v", errWrite)
		}
	}))
	defer server.Close()

	exec := NewCodexWebsocketsExecutor(&config.Config{})
	exec.directDialContext = server.dial
	auth := &cliproxyauth.Auth{ProxyURL: "direct",
		ID:       "auth-stream-1",
		Label:    "codex-stream-account",
		Provider: "codex",
		Attributes: map[string]string{
			"api_key":   "sk-test",
			"base_url":  server.URL,
			"auth_kind": "oauth",
		},
	}
	req := cliproxyexecutor.Request{
		Model:   "gpt-5.6-sol",
		Payload: []byte(`{"model":"gpt-5.6-sol","input":[{"role":"user","content":"hello"}]}`),
	}

	var observedEvents []cliproxyexecutor.WebSocketResponseEvent
	opts := cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("codex"),
		Metadata: map[string]any{
			"request_id": "test-req-stream",
		},
		WebSocketResponseObserver: func(_ context.Context, ev cliproxyexecutor.WebSocketResponseEvent) {
			observedEvents = append(observedEvents, ev)
		},
	}

	streamResult, err := exec.ExecuteStream(fixtureCodexTenantContext(t.Context()), auth, req, opts)
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}

	// Drain stream
	for chunk := range streamResult.Chunks {
		if chunk.Err != nil {
			t.Fatalf("stream chunk error: %v", chunk.Err)
		}
	}

	if len(observedEvents) < 2 {
		t.Fatalf("observed %d events, want at least 2", len(observedEvents))
	}

	rateLimitEvent := observedEvents[0]
	if rateLimitEvent.RequestID != "test-req-stream" {
		t.Fatalf("RequestID = %q, want test-req-stream", rateLimitEvent.RequestID)
	}
	if rateLimitEvent.AuthID != "auth-stream-1" || rateLimitEvent.AuthLabel != "codex-stream-account" {
		t.Fatalf("Auth = (%q, %q), want (auth-stream-1, codex-stream-account)", rateLimitEvent.AuthID, rateLimitEvent.AuthLabel)
	}
	if rateLimitEvent.EventType != "codex.rate_limits" {
		t.Fatalf("EventType = %q, want codex.rate_limits", rateLimitEvent.EventType)
	}
	if !bytes.Contains(rateLimitEvent.Payload, []byte(`"used_percent":75`)) {
		t.Fatalf("Payload = %s, want used_percent 75", rateLimitEvent.Payload)
	}
}

func TestCodexWebsocketsExecuteHandshakeUsageLimitReachedSetsRetryAfter(t *testing.T) {
	body := []byte(`{"error":{"type":"usage_limit_reached","message":"The usage limit has been reached","resets_in_seconds":120}}`)
	server := newCodexPipeServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		if _, errWrite := w.Write(body); errWrite != nil {
			t.Errorf("write handshake rejection: %v", errWrite)
		}
	}))
	defer server.Close()

	exec := NewCodexWebsocketsExecutor(&config.Config{})
	exec.directDialContext = server.dial
	auth := &cliproxyauth.Auth{ProxyURL: "direct",
		ID:       "codex-auth-quota-exhausted",
		Provider: "codex",
		Attributes: map[string]string{
			"base_url":   server.URL,
			"websockets": "true",
			"api_key":    "sk-test",
		},
	}
	req := cliproxyexecutor.Request{
		Model:   "gpt-5.6-luna",
		Payload: []byte(`{"model":"gpt-5.6-luna","input":[{"type":"message","id":"msg-1"}]}`),
	}
	opts := cliproxyexecutor.Options{
		SourceFormat:   sdktranslator.FromString("openai-response"),
		ResponseFormat: sdktranslator.FromString("openai-response"),
	}

	ctx := cliproxyexecutor.WithUpstreamAttemptTracker(fixtureCodexTenantContext(t.Context()))
	_, errExecute := exec.Execute(ctx, auth, req, opts)
	if errExecute == nil {
		t.Fatal("Execute() error = nil, want handshake rejection")
	}
	if !cliproxyexecutor.UpstreamAttempted(ctx) {
		t.Fatal("429 websocket handshake was not marked as an upstream attempt")
	}
	statusErr, ok := errExecute.(interface{ StatusCode() int })
	if !ok || statusErr.StatusCode() != http.StatusTooManyRequests {
		t.Fatalf("status = %#v, want 429", errExecute)
	}
	retryable, ok := errExecute.(interface{ RetryAfter() *time.Duration })
	if !ok || retryable.RetryAfter() == nil {
		t.Fatalf("expected RetryAfter for usage_limit_reached handshake error: %#v", errExecute)
	}
	if got := *retryable.RetryAfter(); got != 120*time.Second {
		t.Fatalf("RetryAfter = %v, want 120s", got)
	}
}

func TestCodexWebsocketsExecuteStreamHandshakeUsageLimitReachedSetsRetryAfter(t *testing.T) {
	body := []byte(`{"error":{"type":"usage_limit_reached","message":"The usage limit has been reached","resets_in_seconds":120}}`)
	server := newCodexPipeServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		if _, errWrite := w.Write(body); errWrite != nil {
			t.Errorf("write handshake rejection: %v", errWrite)
		}
	}))
	defer server.Close()

	exec := NewCodexWebsocketsExecutor(&config.Config{})
	exec.directDialContext = server.dial
	auth := &cliproxyauth.Auth{ProxyURL: "direct",
		ID:       "codex-auth-quota-exhausted-stream",
		Provider: "codex",
		Attributes: map[string]string{
			"base_url":   server.URL,
			"websockets": "true",
			"api_key":    "sk-test",
		},
	}
	req := cliproxyexecutor.Request{
		Model:   "gpt-5.6-luna",
		Payload: []byte(`{"model":"gpt-5.6-luna","input":[{"type":"message","id":"msg-1"}]}`),
	}
	opts := cliproxyexecutor.Options{
		SourceFormat:   sdktranslator.FromString("openai-response"),
		ResponseFormat: sdktranslator.FromString("openai-response"),
	}

	ctx := cliproxyexecutor.WithUpstreamAttemptTracker(fixtureCodexTenantContext(t.Context()))
	_, errExecuteStream := exec.ExecuteStream(ctx, auth, req, opts)
	if errExecuteStream == nil {
		t.Fatal("ExecuteStream() error = nil, want handshake rejection")
	}
	if !cliproxyexecutor.UpstreamAttempted(ctx) {
		t.Fatal("429 streaming websocket handshake was not marked as an upstream attempt")
	}
	statusErr, ok := errExecuteStream.(interface{ StatusCode() int })
	if !ok || statusErr.StatusCode() != http.StatusTooManyRequests {
		t.Fatalf("status = %#v, want 429", errExecuteStream)
	}
	retryable, ok := errExecuteStream.(interface{ RetryAfter() *time.Duration })
	if !ok || retryable.RetryAfter() == nil {
		t.Fatalf("expected RetryAfter for usage_limit_reached handshake error: %#v", errExecuteStream)
	}
	if got := *retryable.RetryAfter(); got != 120*time.Second {
		t.Fatalf("RetryAfter = %v, want 120s", got)
	}
}

func TestCodexWebsocketZeroTokenIncompleteReleasesSessionRequestLock(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	server := newCodexPipeServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, errUpgrade := upgrader.Upgrade(w, r, nil)
		if errUpgrade != nil {
			t.Errorf("upgrade websocket: %v", errUpgrade)
			return
		}
		defer func() { _ = conn.Close() }()
		if _, _, errRead := conn.ReadMessage(); errRead != nil {
			return
		}
		terminal := []byte(`{"type":"response.incomplete","response":{"id":"resp_1","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"output":[],"usage":{"input_tokens":10,"output_tokens":0,"total_tokens":10}}}`)
		_ = conn.WriteMessage(websocket.TextMessage, terminal)
	}))
	defer server.Close()

	exec := NewCodexWebsocketsExecutor(&config.Config{
		Codex: config.CodexConfig{
			StreamBootstrapBuffering: true,
		},
		SDKConfig: config.SDKConfig{
			DisableImageGeneration: config.DisableImageGenerationAll,
		},
	})
	exec.directDialContext = server.dial
	exec.store = &codexWebsocketSessionStore{sessions: make(map[string]*codexWebsocketSession)}
	auth := &cliproxyauth.Auth{ProxyURL: "direct", ID: "auth-a", Provider: "codex", Attributes: map[string]string{"api_key": "sk-test", "base_url": server.URL}}
	req := cliproxyexecutor.Request{Model: "gpt-5-codex", Payload: []byte(`{"model":"gpt-5-codex","input":[{"type":"message","role":"user","content":"hello"}]}`)}
	opts := cliproxyexecutor.Options{
		SourceFormat:   sdktranslator.FromString("openai-response"),
		ResponseFormat: sdktranslator.FromString("openai-response"),
		Metadata: map[string]any{
			cliproxyexecutor.ExecutionSessionMetadataKey: "zero-token-session",
		},
	}

	result, errExecute := exec.ExecuteStream(fixtureCodexTenantContext(t.Context()), auth, req, opts)
	if errExecute == nil && result != nil {
		for chunk := range result.Chunks {
			_ = chunk
		}
	}

	sess := exec.getOrCreateSession("zero-token-session")
	acquired := make(chan struct{})
	go func() {
		sess.reqMu.Lock()
		defer sess.reqMu.Unlock()
		close(acquired)
	}()
	select {
	case <-acquired:
	case <-time.After(time.Second):
		t.Fatal("failed to acquire session request lock after zero-token incomplete failure")
	}
}

func TestCodexWebsockets_KeepalivePingDuringUpload_WithSession(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	serverPongCh := make(chan string, 1)
	inWriteHook := make(chan struct{})
	var inWriteHookOnce sync.Once
	pongDeliveredDuringWrite := make(chan struct{})
	var pongDeliveredOnce sync.Once
	serverDone := make(chan struct{})
	defer close(serverDone)

	testWebsocketWritePayloadHook = func(conn *websocket.Conn) {
		inWriteHookOnce.Do(func() { close(inWriteHook) })
		// Wait until server confirms pong was received before allowing write to finish.
		select {
		case <-pongDeliveredDuringWrite:
		case <-time.After(5 * time.Second):
			t.Error("timed out waiting for pong delivery while payload write was held in hook")
		}
	}
	defer func() { testWebsocketWritePayloadHook = nil }()

	server := newCodexPipeServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade websocket: %v", err)
			return
		}
		defer func() { _ = conn.Close() }()

		conn.SetPongHandler(func(appData string) error {
			serverPongCh <- appData
			return nil
		})

		// Start server reader loop so server processes control frames.
		readErrCh := make(chan error, 1)
		payloadReceived := make(chan struct{})
		go func() {
			signaled := false
			for {
				if _, _, errRead := conn.ReadMessage(); errRead != nil {
					readErrCh <- errRead
					return
				}
				if !signaled {
					signaled = true
					close(payloadReceived)
				}
			}
		}()

		// Wait until client has entered writeMessage and is actively holding writeMu.
		select {
		case <-inWriteHook:
		case <-time.After(5 * time.Second):
			t.Errorf("timed out waiting for client write hook")
			return
		}

		// Upstream sends Ping WHILE client payload write is in progress holding writeMu.
		_ = conn.WriteControl(websocket.PingMessage, []byte("session-ping"), time.Now().Add(5*time.Second))

		// Server asserts Pong arrives while client write is still blocked in the hook.
		select {
		case got := <-serverPongCh:
			if got != "session-ping" {
				t.Errorf("unexpected pong payload: got %q, want session-ping", got)
			}
			pongDeliveredOnce.Do(func() { close(pongDeliveredDuringWrite) })
		case <-time.After(5 * time.Second):
			t.Errorf("pong was not received while payload write was in progress")
			return
		}

		// The held client write is released together with the pong. Wait until the
		// request frame arrives before writing the terminal event and closing.
		select {
		case <-payloadReceived:
		case <-time.After(5 * time.Second):
			t.Errorf("timed out waiting for client payload after pong")
			return
		}

		// Now send terminal response.
		respPayload := []byte(`{"type":"response.completed","response":{"id":"resp-1","status":"completed","output":[]}}`)
		_ = conn.WriteMessage(websocket.TextMessage, respPayload)
		<-serverDone
	}))
	defer server.Close()

	exec := NewCodexWebsocketsExecutor(&config.Config{
		SDKConfig: config.SDKConfig{
			DisableImageGeneration: config.DisableImageGenerationAll,
		},
	})
	exec.directDialContext = server.dial
	auth := &cliproxyauth.Auth{ProxyURL: "direct", ID: "auth-session-ping", Attributes: map[string]string{"api_key": "sk-test", "base_url": server.URL}}
	req := cliproxyexecutor.Request{
		Model:   "gpt-5.6-sol",
		Payload: []byte(`{"model":"gpt-5.6-sol","input":[{"type":"message","role":"user","content":"ping test"}]}`),
	}
	opts := cliproxyexecutor.Options{
		SourceFormat:   sdktranslator.FromString("openai-response"),
		ResponseFormat: sdktranslator.FromString("openai-response"),
		Metadata: map[string]any{
			cliproxyexecutor.ExecutionSessionMetadataKey: "session-ping-test",
		},
	}

	result, errStream := exec.ExecuteStream(fixtureCodexTenantContext(t.Context()), auth, req, opts)
	if errStream != nil {
		t.Fatalf("ExecuteStream() failed: %v", errStream)
	}

	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("chunk error: %v", chunk.Err)
		}
	}
}

func TestCodexWebsockets_KeepalivePingDuringUpload_Sessionless(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	serverPongCh := make(chan string, 1)
	inWriteHook := make(chan struct{})
	var inWriteHookOnce sync.Once
	pongDeliveredDuringWrite := make(chan struct{})
	var pongDeliveredOnce sync.Once
	serverDone := make(chan struct{})
	defer close(serverDone)

	testWebsocketWritePayloadHook = func(conn *websocket.Conn) {
		inWriteHookOnce.Do(func() { close(inWriteHook) })
		select {
		case <-pongDeliveredDuringWrite:
		case <-time.After(5 * time.Second):
			t.Error("timed out waiting for pong delivery while payload write was held in hook")
		}
	}
	defer func() { testWebsocketWritePayloadHook = nil }()

	server := newCodexPipeServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade websocket: %v", err)
			return
		}
		defer func() { _ = conn.Close() }()

		conn.SetPongHandler(func(appData string) error {
			serverPongCh <- appData
			return nil
		})

		payloadReceived := make(chan struct{})
		go func() {
			signaled := false
			for {
				if _, _, errRead := conn.ReadMessage(); errRead != nil {
					return
				}
				if !signaled {
					signaled = true
					close(payloadReceived)
				}
			}
		}()

		// Wait until client has entered writeMessage on sessionless path.
		select {
		case <-inWriteHook:
		case <-time.After(5 * time.Second):
			t.Errorf("timed out waiting for client write hook")
			return
		}

		// Upstream sends Ping WHILE client payload write is in progress.
		_ = conn.WriteControl(websocket.PingMessage, []byte("sessionless-ping"), time.Now().Add(5*time.Second))

		// Server asserts Pong arrives while client write is still in progress.
		select {
		case got := <-serverPongCh:
			if got != "sessionless-ping" {
				t.Errorf("unexpected pong payload: got %q, want sessionless-ping", got)
			}
			pongDeliveredOnce.Do(func() { close(pongDeliveredDuringWrite) })
		case <-time.After(5 * time.Second):
			t.Errorf("pong was not received while payload write was in progress on sessionless connection")
			return
		}

		select {
		case <-payloadReceived:
		case <-time.After(5 * time.Second):
			t.Errorf("timed out waiting for client payload after pong on sessionless connection")
			return
		}

		respPayload := []byte(`{"type":"response.completed","response":{"id":"resp-1","status":"completed","output":[]}}`)
		_ = conn.WriteMessage(websocket.TextMessage, respPayload)
		<-serverDone
	}))
	defer server.Close()

	exec := NewCodexWebsocketsExecutor(&config.Config{
		SDKConfig: config.SDKConfig{
			DisableImageGeneration: config.DisableImageGenerationAll,
		},
	})
	exec.directDialContext = server.dial
	auth := &cliproxyauth.Auth{ProxyURL: "direct", ID: "auth-sessionless-ping", Attributes: map[string]string{"api_key": "sk-test", "base_url": server.URL}}
	req := cliproxyexecutor.Request{
		Model:   "gpt-5.6-sol",
		Payload: []byte(`{"model":"gpt-5.6-sol","input":[{"type":"message","role":"user","content":"ping test sessionless"}]}`),
	}
	opts := cliproxyexecutor.Options{
		SourceFormat:   sdktranslator.FromString("openai-response"),
		ResponseFormat: sdktranslator.FromString("openai-response"),
	}

	result, errStream := exec.ExecuteStream(fixtureCodexTenantContext(t.Context()), auth, req, opts)
	if errStream != nil {
		t.Fatalf("ExecuteStream() failed: %v", errStream)
	}

	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("chunk error: %v", chunk.Err)
		}
	}
}

func TestCodexWebsockets_KeepalivePingDuringUpload_NonstreamSessionless(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	serverPongCh := make(chan string, 1)
	inWriteHook := make(chan struct{})
	var inWriteHookOnce sync.Once
	pongDeliveredDuringWrite := make(chan struct{})
	var pongDeliveredOnce sync.Once
	serverDone := make(chan struct{})
	defer close(serverDone)

	testWebsocketWritePayloadHook = func(conn *websocket.Conn) {
		inWriteHookOnce.Do(func() { close(inWriteHook) })
		select {
		case <-pongDeliveredDuringWrite:
		case <-time.After(5 * time.Second):
			t.Error("timed out waiting for pong delivery while nonstream payload write was held in hook")
		}
	}
	defer func() { testWebsocketWritePayloadHook = nil }()

	server := newCodexPipeServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade websocket: %v", err)
			return
		}
		defer func() { _ = conn.Close() }()

		conn.SetPongHandler(func(appData string) error {
			serverPongCh <- appData
			return nil
		})

		payloadReceived := make(chan struct{})
		go func() {
			signaled := false
			for {
				if _, _, errRead := conn.ReadMessage(); errRead != nil {
					return
				}
				if !signaled {
					signaled = true
					close(payloadReceived)
				}
			}
		}()

		// Wait until client has entered writeMessage on nonstream path.
		select {
		case <-inWriteHook:
		case <-time.After(5 * time.Second):
			t.Errorf("timed out waiting for client write hook")
			return
		}

		// Upstream sends Ping WHILE client payload write is in progress.
		_ = conn.WriteControl(websocket.PingMessage, []byte("nonstream-sessionless-ping"), time.Now().Add(5*time.Second))

		// Server asserts Pong arrives while client write is still in progress.
		select {
		case got := <-serverPongCh:
			if got != "nonstream-sessionless-ping" {
				t.Errorf("unexpected pong payload: got %q, want nonstream-sessionless-ping", got)
			}
			pongDeliveredOnce.Do(func() { close(pongDeliveredDuringWrite) })
		case <-time.After(5 * time.Second):
			t.Errorf("pong was not received while payload write was in progress on nonstream sessionless connection")
			return
		}

		select {
		case <-payloadReceived:
		case <-time.After(5 * time.Second):
			t.Errorf("timed out waiting for client payload after pong on nonstream sessionless connection")
			return
		}

		respPayload := []byte(`{"type":"response.completed","response":{"id":"resp-1","status":"completed","output":[]}}`)
		_ = conn.WriteMessage(websocket.TextMessage, respPayload)
		<-serverDone
	}))
	defer server.Close()

	exec := NewCodexWebsocketsExecutor(&config.Config{
		SDKConfig: config.SDKConfig{
			DisableImageGeneration: config.DisableImageGenerationAll,
		},
	})
	exec.directDialContext = server.dial
	auth := &cliproxyauth.Auth{ProxyURL: "direct", ID: "auth-nonstream-ping", Attributes: map[string]string{"api_key": "sk-test", "base_url": server.URL}}
	req := cliproxyexecutor.Request{
		Model:   "gpt-5.6-sol",
		Payload: []byte(`{"model":"gpt-5.6-sol","input":[{"type":"message","role":"user","content":"ping test nonstream"}]}`),
	}
	opts := cliproxyexecutor.Options{
		SourceFormat:   sdktranslator.FromString("openai-response"),
		ResponseFormat: sdktranslator.FromString("openai-response"),
	}

	resp, errExec := exec.Execute(fixtureCodexTenantContext(t.Context()), auth, req, opts)
	if errExec != nil {
		t.Fatalf("Execute() failed: %v", errExec)
	}
	if len(resp.Payload) == 0 {
		t.Fatal("Execute() returned empty payload")
	}
}

func TestCodexWebsockets_SessionlessBufferingImmediateTerminalClosesConnection(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	serverClosed := make(chan struct{})

	server := newCodexPipeServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade websocket: %v", err)
			return
		}
		defer func() {
			_ = conn.Close()
			close(serverClosed)
		}()

		// Read client request.
		if _, _, errRead := conn.ReadMessage(); errRead != nil {
			return
		}

		// Send immediate terminal event while buffering is enabled.
		respPayload := []byte(`{"type":"response.completed","response":{"id":"resp-1","status":"completed","output":[]}}`)
		_ = conn.WriteMessage(websocket.TextMessage, respPayload)

		// Wait until client closes connection.
		for {
			if _, _, errRead := conn.ReadMessage(); errRead != nil {
				return
			}
		}
	}))
	defer server.Close()

	exec := NewCodexWebsocketsExecutor(&config.Config{
		Codex: config.CodexConfig{
			StreamBootstrapBuffering: true,
		},
		SDKConfig: config.SDKConfig{
			DisableImageGeneration: config.DisableImageGenerationAll,
		},
	})
	exec.directDialContext = server.dial
	auth := &cliproxyauth.Auth{ProxyURL: "direct", ID: "auth-buffering-close", Attributes: map[string]string{"api_key": "sk-test", "base_url": server.URL}}
	req := cliproxyexecutor.Request{
		Model:   "gpt-5.6-sol",
		Payload: []byte(`{"model":"gpt-5.6-sol","input":[{"type":"message","role":"user","content":"buffering close test"}]}`),
	}
	opts := cliproxyexecutor.Options{
		SourceFormat:   sdktranslator.FromString("openai-response"),
		ResponseFormat: sdktranslator.FromString("openai-response"),
		// Sessionless
	}

	result, errStream := exec.ExecuteStream(fixtureCodexTenantContext(t.Context()), auth, req, opts)
	if errStream != nil {
		t.Fatalf("ExecuteStream() failed: %v", errStream)
	}

	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("chunk error: %v", chunk.Err)
		}
	}

	// Server connection must be closed by client immediately upon terminal buffering.
	select {
	case <-serverClosed:
	case <-time.After(2 * time.Second):
		t.Fatal("sessionless connection was not closed after immediate terminal buffering")
	}
}

func TestCodexWebsockets_ChunkedWriteAllowsPongInterleaving(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	serverPongCh := make(chan string, 1)
	pongReceivedBeforeReadComplete := make(chan struct{})

	firstChunkReadOnServer := make(chan struct{})
	allowRemainingChunks := make(chan struct{})

	testWebsocketWriteChunkHook = func(chunkIndex int, totalChunks int) {
		if chunkIndex == 1 {
			// Chunk 0 was sent to the network. Now wait until server confirms it has
			// received chunk 0 and sent a keepalive Ping:
			select {
			case <-firstChunkReadOnServer:
			case <-time.After(5 * time.Second):
			}
			select {
			case <-allowRemainingChunks:
			case <-time.After(5 * time.Second):
			}
		}
	}
	defer func() { testWebsocketWriteChunkHook = nil }()

	// Large message that spans multiple 32KB chunks (128KB total).
	largeContent := strings.Repeat("A", 128*1024)

	server := newCodexPipeServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade websocket: %v", err)
			return
		}
		defer func() { _ = conn.Close() }()

		conn.SetPongHandler(func(appData string) error {
			serverPongCh <- appData
			return nil
		})

		// Read the first chunk of data using NextReader, proving receipt of partial message on the wire.
		msgType, reader, errNext := conn.NextReader()
		if errNext != nil {
			t.Errorf("server NextReader error: %v", errNext)
			return
		}
		if msgType != websocket.TextMessage {
			t.Errorf("unexpected msgType: %d", msgType)
			return
		}

		firstChunk := make([]byte, 8192)
		n, errRead := io.ReadFull(reader, firstChunk)
		if errRead != nil || n < 8192 {
			t.Errorf("failed reading first chunk from wire: n=%d err=%v", n, errRead)
			return
		}

		// Server confirmed reading chunk 0 from the wire!
		close(firstChunkReadOnServer)

		// Server injects keepalive Ping while client is paused between chunks.
		_ = conn.WriteControl(websocket.PingMessage, []byte("chunked-interleaved-ping"), time.Now().Add(time.Second))

		// Goroutine to read reader so Gorilla processes the interleaved Pong frame.
		readDone := make(chan struct{})
		var totalMsg []byte
		var readErr error
		go func() {
			rest, errRest := io.ReadAll(reader)
			readErr = errRest
			totalMsg = append(firstChunk, rest...)
			close(readDone)
		}()

		// Server asserts Pong is received while remaining chunks are still paused.
		select {
		case got := <-serverPongCh:
			if got != "chunked-interleaved-ping" {
				t.Errorf("unexpected pong: got %q, want chunked-interleaved-ping", got)
			}
			close(pongReceivedBeforeReadComplete)
			close(allowRemainingChunks)
		case <-time.After(2 * time.Second):
			t.Errorf("pong was not received while client was paused between chunks")
			close(allowRemainingChunks)
			return
		}

		// Wait for read to finish now that allowRemainingChunks was closed.
		select {
		case <-readDone:
			if readErr != nil {
				t.Errorf("server ReadAll rest error: %v", readErr)
				return
			}
		case <-time.After(2 * time.Second):
			t.Errorf("timed out reading remaining message frames")
			return
		}

		if len(totalMsg) < 128*1024 {
			t.Errorf("total received payload too short: %d bytes", len(totalMsg))
			return
		}

		// Send terminal response.
		respPayload := []byte(`{"type":"response.completed","response":{"id":"resp-1","status":"completed","output":[]}}`)
		_ = conn.WriteMessage(websocket.TextMessage, respPayload)
	}))
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	clientConn, _, errDial := (&websocket.Dialer{NetDialContext: server.dial}).DialContext(t.Context(), wsURL, nil)
	if errDial != nil {
		t.Fatalf("dial error: %v", errDial)
	}
	defer func() { _ = clientConn.Close() }()

	sess := &codexWebsocketSession{sessionID: "session-chunked-test"}
	sess.configureConn(clientConn)
	_ = sess.activate(clientConn)
	exec := NewCodexWebsocketsExecutor(&config.Config{})
	exec.directDialContext = server.dial
	go exec.readUpstreamLoop(sess, clientConn)

	// Write 128KB message through production sess.writeMessage path:
	payload := []byte(largeContent)
	errWrite := sess.writeMessage(clientConn, websocket.TextMessage, payload)
	if errWrite != nil {
		t.Fatalf("writeMessage failed: %v", errWrite)
	}

	select {
	case <-pongReceivedBeforeReadComplete:
	case <-time.After(2 * time.Second):
		t.Fatal("pong was not received before message read completed")
	}
}

func TestCodexWebsockets_SendErrorLogsSessionObject(t *testing.T) {
	origOut := log.StandardLogger().Out
	origLevel := log.GetLevel()
	var buf bytes.Buffer
	log.SetOutput(&buf)
	log.SetLevel(log.DebugLevel)
	defer func() {
		log.SetOutput(origOut)
		log.SetLevel(origLevel)
	}()

	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	server := newCodexPipeServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade websocket: %v", err)
			return
		}
		defer func() { _ = conn.Close() }()
		for {
			if _, _, errRead := conn.ReadMessage(); errRead != nil {
				return
			}
		}
	}))
	defer server.Close()

	// Deterministically cause send error by expiring write deadline right before writing.
	testWebsocketWritePayloadHook = func(conn *websocket.Conn) {
		_ = conn.SetWriteDeadline(time.Now().Add(-time.Second))
	}
	defer func() { testWebsocketWritePayloadHook = nil }()

	exec := NewCodexWebsocketsExecutor(&config.Config{
		SDKConfig: config.SDKConfig{
			DisableImageGeneration: config.DisableImageGenerationAll,
		},
	})
	exec.directDialContext = server.dial
	auth := &cliproxyauth.Auth{ProxyURL: "direct", ID: "auth-send-fail", Attributes: map[string]string{"api_key": "sk-test", "base_url": server.URL}}
	req := cliproxyexecutor.Request{
		Model:   "gpt-5.6-sol",
		Payload: []byte(`{"model":"gpt-5.6-sol","input":[{"type":"message","role":"user","content":"send fail"}]}`),
	}
	opts := cliproxyexecutor.Options{
		SourceFormat:   sdktranslator.FromString("openai-response"),
		ResponseFormat: sdktranslator.FromString("openai-response"),
		// Sessionless -> ephemeral
	}

	result, err := exec.ExecuteStream(fixtureCodexTenantContext(t.Context()), auth, req, opts)
	if err == nil && result != nil {
		for chunk := range result.Chunks {
			if chunk.Err != nil {
				err = chunk.Err
			}
		}
	}
	if err == nil {
		t.Fatal("expected ExecuteStream to fail when connection is closed before send")
	}

	logOutput := buf.String()
	if !strings.Contains(logOutput, "session_object=ephemeral") {
		t.Fatalf("expected session_object=ephemeral in log output, got: %s", logOutput)
	}
	if !strings.Contains(logOutput, "reason=send_error") {
		t.Fatalf("expected reason=send_error in log output, got: %s", logOutput)
	}
}

func TestCodexWebsocketReconnectUpgradeRejectionRecordedWhenRequestLogDisabled(t *testing.T) {
	gin.SetMode(gin.TestMode)
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}

	for _, stream := range []bool{false, true} {
		name := "execute"
		if stream {
			name = "stream"
		}
		t.Run(name, func(t *testing.T) {
			var dials atomic.Int32
			server := newCodexPipeServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if dials.Add(1) == 1 {
					conn, errUpgrade := upgrader.Upgrade(w, r, nil)
					if errUpgrade != nil {
						return
					}
					defer func() { _ = conn.Close() }()
					for {
						if _, _, errRead := conn.ReadMessage(); errRead != nil {
							return
						}
					}
				}
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Retry-After", "11")
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(`{"error":{"message":"codex websocket reconnect forbidden"}}`))
			}))
			defer server.Close()

			ginCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
			ginCtx.Set("userApiKey", "fixture-tenant")
			ctx := context.WithValue(t.Context(), "gin", ginCtx)
			exec := NewCodexWebsocketsExecutor(&config.Config{
				SDKConfig: config.SDKConfig{
					DisableImageGeneration: config.DisableImageGenerationAll,
				},
			})
			exec.directDialContext = server.dial
			auth := &cliproxyauth.Auth{ProxyURL: "direct",
				ID:         "codex-ws-reconnect",
				Attributes: map[string]string{"api_key": "sk-test", "base_url": server.URL},
			}
			req := cliproxyexecutor.Request{
				Model:   "gpt-5.6-sol",
				Payload: []byte(`{"model":"gpt-5.6-sol","input":[{"type":"message","role":"user","content":"reconnect"}]}`),
			}
			opts := cliproxyexecutor.Options{
				SourceFormat:   sdktranslator.FormatOpenAIResponse,
				ResponseFormat: sdktranslator.FormatOpenAIResponse,
				Metadata: map[string]any{
					cliproxyexecutor.ExecutionSessionMetadataKey: "sess-codex-reconnect-" + name,
				},
			}

			turn, err := exec.startOrdinaryCodexWebsocket(ctx, auth, req, opts)
			if err != nil {
				t.Fatalf("first ordinary upgrade: %v", err)
			}
			exec.finishOrdinaryCodexWebsocket(turn, nil)
			exec.CloseExecutionSession(executionSessionIDFromOptions(opts))
			// The caller explicitly starts a fresh turn after closing the prior
			// session. A failed send never triggers automatic replay.
			var errExec error
			if stream {
				_, errExec = exec.ExecuteStream(ctx, auth, req, opts)
			} else {
				_, errExec = exec.Execute(ctx, auth, req, opts)
			}
			if errExec == nil {
				t.Fatal("expected reconnect upgrade rejection to fail")
			}

			value, exists := ginCtx.Get(logging.DeferredAPIResponseContextKey)
			if !exists {
				t.Fatal("deferred API response was not recorded on codex websocket reconnect rejection")
			}
			deferred, ok := value.(logging.DeferredAPIResponse)
			if !ok || deferred == nil {
				t.Fatalf("deferred API response = %#v, want logging.DeferredAPIResponse", value)
			}
			rendered := string(deferred.RenderAPIResponse())
			for _, want := range []string{
				"=== API RESPONSE 1 ===",
				"Status: 101",
				"=== API RESPONSE 2 ===",
				"Status: 403",
				"Retry-After: 11",
				`{"error":{"message":"codex websocket reconnect forbidden"}}`,
			} {
				if !strings.Contains(rendered, want) {
					t.Fatalf("deferred API response missing %q:\n%s", want, rendered)
				}
			}

			var transportDials atomic.Int32
			transportServer := newCodexPipeServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if transportDials.Add(1) == 1 {
					conn, errUpgrade := upgrader.Upgrade(w, r, nil)
					if errUpgrade != nil {
						return
					}
					defer func() { _ = conn.Close() }()
					for {
						if _, _, errRead := conn.ReadMessage(); errRead != nil {
							return
						}
					}
				}
				if hijacker, ok := w.(http.Hijacker); ok {
					if rawConn, _, errHijack := hijacker.Hijack(); errHijack == nil {
						_ = rawConn.Close()
					}
				}
			}))
			defer transportServer.Close()

			subGinCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
			subGinCtx.Set("userApiKey", "fixture-tenant")
			subCtx := context.WithValue(t.Context(), "gin", subGinCtx)
			subAuth := &cliproxyauth.Auth{ProxyURL: "direct",
				ID:         "codex-ws-transport-" + name,
				Attributes: map[string]string{"api_key": "sk-test", "base_url": transportServer.URL},
			}
			subOpts := cliproxyexecutor.Options{
				SourceFormat:   sdktranslator.FormatOpenAIResponse,
				ResponseFormat: sdktranslator.FormatOpenAIResponse,
				Metadata: map[string]any{
					cliproxyexecutor.ExecutionSessionMetadataKey: "sess-codex-transport-" + name,
				},
			}
			exec.directDialContext = transportServer.dial
			subTurn, err := exec.startOrdinaryCodexWebsocket(subCtx, subAuth, req, subOpts)
			if err != nil {
				t.Fatal(err)
			}
			exec.finishOrdinaryCodexWebsocket(subTurn, nil)
			exec.CloseExecutionSession(executionSessionIDFromOptions(subOpts))
			if stream {
				_, errExec = exec.ExecuteStream(subCtx, subAuth, req, subOpts)
			} else {
				_, errExec = exec.Execute(subCtx, subAuth, req, subOpts)
			}
			if errExec == nil {
				t.Fatal("expected transport reconnect failure")
			}
			subVal, ok := subGinCtx.Get(logging.DeferredAPIResponseContextKey)
			if !ok {
				t.Fatal("deferred API response missing on transport reconnect failure")
			}
			subRendered := string(subVal.(logging.DeferredAPIResponse).RenderAPIResponse())
			for _, want := range []string{
				"=== API RESPONSE 1 ===",
			} {
				if !strings.Contains(subRendered, want) {
					t.Fatalf("transport reconnect response missing %q:\n%s", want, subRendered)
				}
			}

			initServer := newCodexPipeServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				hijacker, ok := w.(http.Hijacker)
				if !ok {
					t.Error("hijacker unsupported")
					return
				}
				rawConn, rawBuf, errHijack := hijacker.Hijack()
				if errHijack != nil {
					t.Errorf("hijack: %v", errHijack)
					return
				}
				_, _ = rawBuf.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: invalid\r\n\r\n")
				_ = rawBuf.Flush()
				_ = rawConn.Close()
			}))
			defer initServer.Close()

			initGinCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
			initCtx := context.WithValue(fixtureCodexTenantContext(t.Context()), "gin", initGinCtx)
			initAuth := &cliproxyauth.Auth{ProxyURL: "direct",
				ID:         "codex-ws-init-" + name,
				Attributes: map[string]string{"api_key": "sk-test", "base_url": initServer.URL},
			}
			initOpts := cliproxyexecutor.Options{
				SourceFormat:   sdktranslator.FormatOpenAIResponse,
				ResponseFormat: sdktranslator.FormatOpenAIResponse,
			}
			exec.directDialContext = initServer.dial
			if stream {
				_, errExec = exec.ExecuteStream(initCtx, initAuth, req, initOpts)
			} else {
				_, errExec = exec.Execute(initCtx, initAuth, req, initOpts)
			}
			if errExec == nil {
				t.Fatal("expected initial invalid-101 handshake to fail")
			}
			initVal, ok := initGinCtx.Get(logging.DeferredAPIResponseContextKey)
			if !ok {
				t.Fatal("deferred API response missing on initial invalid-101 handshake")
			}
			initRendered := string(initVal.(logging.DeferredAPIResponse).RenderAPIResponse())
			if !strings.Contains(initRendered, "Status: 101") || strings.Contains(initRendered, "Error: dial:") {
				t.Fatalf("initial handshake response must contain only upstream details:\n%s", initRendered)
			}
		})
	}
}

func TestOrdinaryCodexWebsocketHeadersUseSelectedBearerAndOrigin(t *testing.T) {
	e := NewCodexWebsocketsExecutor(&config.Config{})
	for _, tc := range []struct {
		name, base, key, account, wantAccount string
		wantStatus                            int
	}{
		{"ordinary OAuth", "", "fixture-token", "fixture-account", "fixture-account", 0},
		{"custom same-origin OAuth", "https://private.invalid", "fixture-token", "fixture-account", "", 0},
		{"no credential", "https://private.invalid", "", "fixture-account", "", http.StatusUnauthorized},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := cliproxyexecutor.Request{Model: "gpt-5", Payload: []byte(`{"model":"gpt-5","input":[]}`)}
			opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatCodex, Headers: http.Header{
				"Authorization":       {"Bearer caller-value"},
				"Chatgpt-Account-Id":  {"origin-other-account"},
				"X-Client-Request-Id": {"client-request"},
				"Session_id":          {"older-client-session"},
			}}
			auth := &cliproxyauth.Auth{Attributes: map[string]string{"base_url": tc.base}, Metadata: map[string]any{"access_token": tc.key, "account_id": tc.account}}
			wire, err := e.prepareCodexWebsocketStream(t.Context(), auth, req, opts)
			if tc.wantStatus != 0 {
				var got statusErr
				if !errors.As(err, &got) || got.StatusCode() != tc.wantStatus || wire != nil {
					t.Fatalf("missing credential preflight = %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			h := wire.wsHeaders
			if h.Get("Authorization") != "Bearer "+tc.key || h.Get("ChatGPT-Account-ID") != tc.wantAccount || h.Get("X-Client-Request-Id") != "client-request" || h.Get("Session_id") != "" || h.Get("Originator") != "" || h.Get("User-Agent") != "" {
				t.Fatal("credential or public header boundary changed")
			}
		})
	}
}

func TestCodexWebsockets_PingHandlerDoesNotBlockOnWriteMu(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	serverConnCh := make(chan *websocket.Conn, 1)
	pongReceived := make(chan string, 1)

	server := newCodexPipeServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade websocket: %v", err)
			return
		}
		conn.SetPongHandler(func(appData string) error {
			pongReceived <- appData
			return nil
		})
		serverConnCh <- conn
		for {
			if _, _, errRead := conn.ReadMessage(); errRead != nil {
				return
			}
		}
	}))
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	clientConn, _, errDial := (&websocket.Dialer{NetDialContext: server.dial}).DialContext(t.Context(), wsURL, nil)
	if errDial != nil {
		t.Fatalf("dial websocket failed: %v", errDial)
	}
	defer func() { _ = clientConn.Close() }()

	serverConn := <-serverConnCh
	defer func() { _ = serverConn.Close() }()

	sess := &codexWebsocketSession{sessionID: "test-keepalive"}
	sess.configureConn(clientConn)

	// Start client read loop so it processes control frames.
	go func() {
		for {
			if _, _, errRead := clientConn.ReadMessage(); errRead != nil {
				return
			}
		}
	}()

	// Simulate an active application message write holding writeMu.
	sess.writeMu.Lock()
	defer sess.writeMu.Unlock()

	// Upstream sends a keepalive ping while writeMu is held.
	errPing := serverConn.WriteControl(websocket.PingMessage, []byte("keepalive-ping"), time.Now().Add(time.Second))
	if errPing != nil {
		t.Fatalf("failed to send ping: %v", errPing)
	}

	// Pong must be received promptly without being starved by writeMu.
	select {
	case got := <-pongReceived:
		if got != "keepalive-ping" {
			t.Fatalf("unexpected pong payload: got %q, want keepalive-ping", got)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("pong response was blocked/starved while writeMu was held")
	}
}

func TestCodexWebsockets_PingLoggingRedacted(t *testing.T) {
	origOut := log.StandardLogger().Out
	origLevel := log.GetLevel()
	var buf bytes.Buffer
	log.SetOutput(&buf)
	log.SetLevel(log.DebugLevel)
	defer func() {
		log.SetOutput(origOut)
		log.SetLevel(origLevel)
	}()

	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	server := newCodexPipeServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade websocket: %v", err)
			return
		}
		defer func() { _ = conn.Close() }()
		for {
			if _, _, errRead := conn.ReadMessage(); errRead != nil {
				return
			}
		}
	}))
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	clientConn, _, errDial := (&websocket.Dialer{NetDialContext: server.dial}).DialContext(t.Context(), wsURL, nil)
	if errDial != nil {
		t.Fatalf("dial websocket failed: %v", errDial)
	}
	defer func() { _ = clientConn.Close() }()

	sess := &codexWebsocketSession{sessionID: "redact-session"}
	sess.configureConn(clientConn)

	sensitiveData := "SUPER-SECRET-PAYLOAD-12345"
	pingHandler := clientConn.PingHandler()
	if pingHandler == nil {
		t.Fatal("pingHandler is nil")
	}
	_ = pingHandler(sensitiveData)

	logOutput := buf.String()
	if strings.Contains(logOutput, sensitiveData) {
		t.Fatalf("log output leaked sensitive ping payload: %s", logOutput)
	}
	if !strings.Contains(logOutput, "ping_bytes=") {
		t.Fatalf("log output missing ping_bytes: %s", logOutput)
	}
}

func TestCodexAutoRouteWebsocketAccountPolicy(t *testing.T) {
	for _, tc := range []struct {
		name       string
		downstream bool
		attributes map[string]string
		metadata   map[string]any
		want       bool
	}{
		{"disabled HTTP", false, nil, nil, false},
		{"disabled WS", true, nil, nil, false},
		{"enabled HTTP", false, map[string]string{"websockets": "true"}, nil, true},
		{"enabled WS", true, map[string]string{"websockets": "true"}, nil, true},
		{"metadata HTTP", false, nil, map[string]any{"websockets": true}, true},
		{"metadata string HTTP", false, nil, map[string]any{"websockets": "true"}, true},
		{"attribute overrides metadata", false, map[string]string{"websockets": "false"}, map[string]any{"websockets": true}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			auth := &cliproxyauth.Auth{Attributes: tc.attributes, Metadata: tc.metadata}
			if got := codexWebsocketsEnabled(auth); got != tc.want {
				t.Fatalf("websocket route = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestCodexWebsocketsExecuteResponsesLiteDoesNotInjectImageGenerationTool(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	capturedPayload := make(chan []byte, 1)
	server := newCodexPipeServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Fatalf("upgrade websocket: %v", err)
		}
		defer func() { _ = conn.Close() }()

		_, payload, errRead := conn.ReadMessage()
		if errRead != nil {
			t.Fatalf("read upstream websocket message: %v", errRead)
		}
		capturedPayload <- bytes.Clone(payload)

		completed := []byte(`{"type":"response.completed","response":{"id":"resp-1","output":[],"usage":{"input_tokens":0,"output_tokens":0,"total_tokens":0}}}`)
		if errWrite := conn.WriteMessage(websocket.TextMessage, completed); errWrite != nil {
			t.Fatalf("write completed websocket message: %v", errWrite)
		}
	}))
	defer server.Close()

	exec := NewCodexWebsocketsExecutor(&config.Config{})
	exec.directDialContext = server.dial
	auth := &cliproxyauth.Auth{ProxyURL: "direct",
		Provider: "codex",
		Attributes: map[string]string{
			"api_key":   "sk-test",
			"base_url":  server.URL,
			"plan_type": "pro",
		},
	}
	req := cliproxyexecutor.Request{
		Model:   "gpt-5.6-sol",
		Payload: []byte(`{"model":"gpt-5.6-sol","input":[{"type":"additional_tools","role":"developer","tools":[{"type":"custom","name":"exec"}]},{"role":"user","content":"hello"}],"parallel_tool_calls":true,"client_metadata":{"ws_request_header_x_openai_internal_codex_responses_lite":"true"}}`),
	}
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("codex")}

	if _, err := exec.Execute(context.Background(), auth, req, opts); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	select {
	case payload := <-capturedPayload:
		if instructions := ojson.GetBytes(payload, "instructions"); instructions.Exists() {
			t.Errorf("unexpected instructions in responses-lite upstream payload: %s", payload)
		}
		if tools := ojson.GetBytes(payload, "tools"); tools.Exists() {
			t.Fatalf("unexpected tools in responses-lite upstream payload: %s", tools.Raw)
		}
		if got := ojson.GetBytes(payload, "input.0.type").String(); got != "additional_tools" {
			t.Fatalf("input.0.type = %q, want additional_tools; payload=%s", got, payload)
		}
		if got := ojson.GetBytes(payload, "client_metadata.ws_request_header_x_openai_internal_codex_responses_lite").String(); got != "true" {
			t.Fatalf("responses-lite metadata = %q, want true; payload=%s", got, payload)
		}
		parallelToolCalls := ojson.GetBytes(payload, "parallel_tool_calls")
		if !parallelToolCalls.Exists() || parallelToolCalls.Bool() {
			t.Fatalf("responses-lite parallel_tool_calls should be false: %s", payload)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for upstream websocket payload")
	}
}

func TestCodexWebsocketsExecuteStreamResponsesLiteForcesParallelToolCallsFalse(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	capturedPayload := make(chan []byte, 1)
	server := newCodexPipeServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, errUpgrade := upgrader.Upgrade(w, r, nil)
		if errUpgrade != nil {
			t.Errorf("upgrade websocket: %v", errUpgrade)
			return
		}
		defer func() { _ = conn.Close() }()

		_, payload, errRead := conn.ReadMessage()
		if errRead != nil {
			t.Errorf("read upstream websocket message: %v", errRead)
			return
		}
		capturedPayload <- bytes.Clone(payload)

		completed := []byte(`{"type":"response.completed","response":{"id":"resp-1","output":[],"usage":{"input_tokens":0,"output_tokens":0,"total_tokens":0}}}`)
		if errWrite := conn.WriteMessage(websocket.TextMessage, completed); errWrite != nil {
			t.Errorf("write completed websocket message: %v", errWrite)
		}
	}))
	defer server.Close()

	exec := NewCodexWebsocketsExecutor(&config.Config{})
	exec.directDialContext = server.dial
	auth := &cliproxyauth.Auth{ProxyURL: "direct",
		Provider: "codex",
		Attributes: map[string]string{
			"api_key":   "sk-test",
			"base_url":  server.URL,
			"plan_type": "pro",
		},
	}
	req := cliproxyexecutor.Request{
		Model:   "gpt-5.6-luna",
		Payload: []byte(`{"model":"gpt-5.6-luna","input":[{"type":"additional_tools","role":"developer","tools":[{"type":"custom","name":"exec"}]},{"role":"user","content":"hello"}],"parallel_tool_calls":true,"client_metadata":{"ws_request_header_x_openai_internal_codex_responses_lite":"true"}}`),
	}
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("codex")}

	result, errExecute := exec.ExecuteStream(context.Background(), auth, req, opts)
	if errExecute != nil {
		t.Fatalf("ExecuteStream() error = %v", errExecute)
	}
	streamComplete := false
	for !streamComplete {
		select {
		case chunk, ok := <-result.Chunks:
			if !ok {
				streamComplete = true
				continue
			}
			if chunk.Err != nil {
				t.Fatalf("stream chunk error = %v", chunk.Err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for websocket stream completion")
		}
	}

	select {
	case payload := <-capturedPayload:
		parallelToolCalls := ojson.GetBytes(payload, "parallel_tool_calls")
		if !parallelToolCalls.Exists() || parallelToolCalls.Bool() {
			t.Fatalf("responses-lite parallel_tool_calls should be false: %s", payload)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for upstream websocket payload")
	}
}

func TestOrdinaryCodexStandardWebsocketDoesNotAddPrivateBeta(t *testing.T) {
	e := NewCodexWebsocketsExecutor(&config.Config{})
	a := &cliproxyauth.Auth{Attributes: map[string]string{"api_key": "fixture", "base_url": "https://api.openai.com/v1"}}
	req := cliproxyexecutor.Request{Model: "fixture-model", Payload: []byte(`{"model":"fixture-model","input":[]}`)}
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatCodex}
	wire, err := e.prepareCodexWebsocketStream(t.Context(), a, req, opts)
	if err != nil {
		t.Fatal(err)
	}
	if wire.wsURL != "wss://api.openai.com/v1/responses" || wire.wsHeaders.Get("Authorization") != "Bearer fixture" || wire.wsHeaders.Get("OpenAI-Beta") != "" {
		t.Fatal("standard Responses upgrade did not match the published non-beta handshake")
	}
}

func TestOrdinaryCodexModelHeadersRequireSelectedOriginAndAllowlist(t *testing.T) {
	const idA = "fixture-model-header-credential-a"
	const idB = "fixture-model-header-credential-b"
	const model = "fixture-model-header"
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(idA, "codex", []*registry.ModelInfo{{ID: model, Config: &registry.ModelConfig{OverrideHeader: map[string]string{
		"Accept-Language":     "fr-CA",
		"X-Correlation-Id":    "support-case",
		"Authorization":       "Bearer bad",
		"OpenAI-Organization": "other-tenant",
		"Originator":          "untrusted-originator",
		"User-Agent":          "untrusted-agent",
		"OpenAI-Beta":         "private-feature",
		"X-Client-Request-Id": "replace-caller-id",
	}}}})
	reg.RegisterClient(idB, "codex", []*registry.ModelInfo{{ID: model, Config: &registry.ModelConfig{OverrideHeader: map[string]string{"Accept-Language": "en"}}}})
	t.Cleanup(func() { reg.UnregisterClient(idA); reg.UnregisterClient(idB) })
	cfg := &config.Config{CodexKey: []config.CodexKey{{APIKey: "selected", BaseURL: "https://selected.invalid/v1"}}}
	e := NewCodexWebsocketsExecutor(cfg)
	for _, tc := range []struct{ name, id, origin, lang, correlation string }{
		{"selected", idA, "https://selected.invalid/v1", "fr-CA", "support-case"},
		{"different model registration", idB, "https://selected.invalid/v1", "en", ""},
		{"different origin", idA, "https://other.invalid/v1", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := &cliproxyauth.Auth{ID: tc.id, Attributes: map[string]string{"api_key": "selected", "base_url": tc.origin}}
			req := cliproxyexecutor.Request{Model: model, Payload: []byte(`{"model":"fixture-model-header","input":[]}`)}
			opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatCodex, Headers: http.Header{"X-Client-Request-Id": {"caller-id"}}}
			wire, err := e.prepareCodexWebsocketStream(t.Context(), a, req, opts)
			if err != nil {
				t.Fatal(err)
			}
			h := wire.wsHeaders
			if h.Get("Accept-Language") != tc.lang || h.Get("X-Correlation-Id") != tc.correlation || h.Get("Authorization") != "Bearer selected" || h.Get("X-Client-Request-Id") != "caller-id" {
				t.Fatal("wrong model header scope")
			}
			for _, name := range []string{"OpenAI-Organization", "OpenAI-Beta", "Originator", "User-Agent"} {
				if h.Get(name) != "" {
					t.Fatalf("protected header forwarded: %s", name)
				}
			}
		})
	}
}

func TestOrdinaryCodexSameRequestOpaqueReasoningIsPreserved(t *testing.T) {
	e := NewCodexWebsocketsExecutor(&config.Config{})
	a := &cliproxyauth.Auth{Attributes: map[string]string{"api_key": "fixture", "base_url": "https://fixture.invalid"}}
	fixture := "caller-opaque-fixture-T23"
	for _, tc := range []struct{ name, field string }{
		{"caller supplied", "encrypted_content"},
		{"missing", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const body = `{"model":"fixture-model","input":[{"type":"reasoning","id":"rs_fixture","summary":[]}]}`
			doc := ojson.NewDocString(body)
			if tc.field != "" {
				_ = doc.Set("input.0."+tc.field, fixture)
			}
			req := cliproxyexecutor.Request{Model: "fixture-model", Payload: doc.Bytes()}
			wire, err := e.prepareCodexWebsocketStream(t.Context(), a, req, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatCodex})
			if err != nil {
				t.Fatal(err)
			}
			value := ojson.GetBytesNoCopy(wire.clientBody, "input.0.encrypted_content")
			if tc.field != "" && value.String() != fixture || tc.field == "" && value.Exists() {
				t.Fatal("opaque reasoning was lost or added")
			}
		})
	}
}
