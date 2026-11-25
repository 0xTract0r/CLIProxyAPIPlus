package executor

import (
	"bufio"
	"context"
	"errors"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MoeForge/moego/ojson"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

func TestOrdinaryCodexDuplexAppendIDsAndClose(t *testing.T) {
	serverDone := make(chan error, 1)
	dial := directFixtureDial(func(_ context.Context, _, _ string) (net.Conn, error) {
		client, server := net.Pipe()
		go func() {
			defer server.Close()
			run := func() error {
				_ = server.SetDeadline(time.Now().Add(4 * time.Second))
				rw := bufio.NewReadWriter(bufio.NewReader(server), bufio.NewWriter(server))
				request, err := http.ReadRequest(rw.Reader)
				if err != nil {
					return err
				}
				if request.Header.Get("X-Client-Request-Id") != "known-request-id" || request.Header.Get("Authorization") != "Bearer fixture" {
					return errors.New("ordinary request id or selected bearer missing")
				}
				up := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
				socket, err := up.Upgrade(&wsPipeHTTPWriter{conn: server, buf: rw, header: make(http.Header)}, request, nil)
				if err != nil {
					return err
				}
				defer socket.Close()
				for i, id := range []string{"resp-first", "resp-second"} {
					_, raw, err := socket.ReadMessage()
					if err != nil {
						return err
					}
					d := ojson.NewDoc(raw)
					eventID := d.Get("event_id").String()
					if i == 0 && eventID != "event-first" || i == 1 && eventID != "event-second" {
						return errors.New("ordinary event id was lost")
					}
					if i == 1 && (d.Get("previous_response_id").String() != "resp-first" || d.Get("instructions").String() != "retain instructions" || d.Get("model").String() != "gpt-5") {
						return errors.New("append lost parent, instructions or model: " + string(raw))
					}
					for _, kind := range []string{"response.created", "response.completed"} {
						result := `{"type":"` + kind + `","response":{"id":"` + id + `","model":"gpt-5","output":[]}}`
						if err := socket.WriteMessage(websocket.TextMessage, []byte(result)); err != nil {
							return err
						}
					}
				}
				if _, _, err = socket.ReadMessage(); err == nil {
					return errors.New("cancel sent an extra request")
				}
				if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
					return errors.New("cancel did not close socket")
				}
				return nil
			}
			serverDone <- run()
		}()
		return client, nil
	})
	g, _ := gin.CreateTestContext(nil)
	g.Set("userApiKey", "tenant-fixture")
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	ctx = context.WithValue(ctx, "gin", g)
	input := make(chan cliproxyexecutor.WebsocketInput, 1)
	ctx = cliproxyexecutor.WithWebsocketInput(cliproxyexecutor.WithDownstreamWebsocket(ctx), input)
	e := NewCodexWebsocketsExecutor(&config.Config{Codex: config.CodexConfig{ResponseSteering: true}})
	e.directDialContext = dial
	e.store = &codexWebsocketSessionStore{sessions: make(map[string]*codexWebsocketSession)}
	auth := &cliproxyauth.Auth{ID: "duplex-fixture", ProxyURL: "direct", Provider: "codex", Attributes: map[string]string{"api_key": "fixture", "base_url": "http://ws-fixture.invalid"}}
	var observed atomic.Int32
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatCodex, Headers: http.Header{"X-Client-Request-Id": {"known-request-id"}}, Metadata: map[string]any{cliproxyexecutor.ExecutionSessionMetadataKey: "duplex-fixture"}}
	opts.WebSocketResponseObserver = func(_ context.Context, event cliproxyexecutor.WebSocketResponseEvent) {
		if event.EventType == "response.completed" {
			observed.Add(1)
		}
	}

	req := cliproxyexecutor.Request{Model: "gpt-5", Payload: []byte(`{"type":"response.create","event_id":"event-first","model":"gpt-5","instructions":"retain instructions","input":[{"role":"user","content":"first"}]}`)}
	defer e.CloseExecutionSession(executionSessionIDFromOptions(opts))
	started, err := e.ExecuteStream(ctx, auth, req, opts)
	if err != nil {
		t.Fatal(err)
	}
	done := 0
	for chunk := range started.Chunks {
		if chunk.Err != nil {
			t.Fatal(chunk.Err)
		}
		if !strings.Contains(string(chunk.Payload), "response.completed") {
			continue
		}
		done++
		if done == 1 {
			input <- cliproxyexecutor.WebsocketInput{Payload: []byte(`{"type":"response.append","event_id":"event-second","input":[{"role":"user","content":"second"}]}`)}
		}
		if done == 2 {
			cancel()
		}
	}
	if done != 2 {
		t.Fatalf("completed turns=%d", done)
	}
	select {
	case err := <-serverDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("server did not close")
	}
	if count := observed.Load(); count != 2 {
		t.Fatalf("duplex observer received %d completions, want 2", count)
	}

}

func TestOrdinaryCodexWebsocketAuthorizedRouteBeforeDial(t *testing.T) {
	for _, row := range []struct {
		name, credential, global, override, wantRoute string
		disable, cancel                               bool
	}{
		{name: "no route"},
		{name: "invalid credential", credential: "unknown://bad.invalid", global: "direct"},
		{name: "invalid request", credential: "direct", override: "unknown://bad.invalid"},
		{name: "service route", global: "direct", wantRoute: "direct"},
		{name: "credential route", credential: "direct", global: "unknown://bad.invalid", wantRoute: "direct"},
		{name: "request override", credential: "unknown://bad.invalid", override: "direct", wantRoute: "direct"},
		{name: "disabled", credential: "direct", disable: true},
		{name: "cancel", credential: "direct", cancel: true},
	} {
		t.Run(row.name, func(t *testing.T) {
			seen := make(chan string, 1)
			dial := directFixtureDial(func(ctx context.Context, _, _ string) (net.Conn, error) {
				seen <- cliproxyexecutor.RequestProxyURL(ctx)
				return nil, errors.New("fixture stop at dial")
			})
			ctx := t.Context()
			if row.override != "" {
				ctx = cliproxyexecutor.WithRequestProxyURL(ctx, row.override)
			}
			if row.cancel {
				live, cancel := context.WithCancel(ctx)
				cancel()
				ctx = live
			}
			e := NewCodexWebsocketsExecutor(&config.Config{SDKConfig: config.SDKConfig{ProxyURL: row.global}})
			e.directDialContext = dial
			a := &cliproxyauth.Auth{ID: "fixture", ProxyURL: row.credential, Disabled: row.disable, Attributes: map[string]string{"api_key": "fixture", "base_url": "http://ws-fixture.invalid"}}
			req := cliproxyexecutor.Request{Model: "gpt-5", Payload: []byte(`{"type":"response.create","model":"gpt-5","input":[]}`)}
			got, err := e.startOrdinaryCodexWebsocket(ctx, a, req, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatCodex})
			if got != nil || err == nil {
				t.Fatalf("unexpected turn %v error %v", got, err)
			}
			select {
			case route := <-seen:
				if row.wantRoute == "" || route != row.wantRoute {
					t.Fatalf("unexpected route %q", route)
				}
			default:
				if row.wantRoute != "" {
					t.Fatalf("authorized route was not delegated: %v", err)
				}
			}
		})
	}
}

func TestOrdinaryCodexCloseDuringDial(t *testing.T) {
	for _, how := range []string{"execution", "credential", "request-cancel"} {
		t.Run(how, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			e := NewCodexWebsocketsExecutor(&config.Config{})
			e.store = &codexWebsocketSessionStore{}
			g, _ := gin.CreateTestContext(nil)
			g.Set("userApiKey", "tenant")
			const id = "pending"
			ch := e.UpstreamDisconnectChan(id)
			entered := make(chan struct{})
			dial := directFixtureDial(func(dialCtx context.Context, _, _ string) (net.Conn, error) {
				close(entered)
				<-dialCtx.Done()
				return nil, dialCtx.Err()
			})
			e.directDialContext = dial
			ctx = context.WithValue(ctx, "gin", g)
			a := &cliproxyauth.Auth{ID: "fixture-auth", ProxyURL: "direct", Attributes: map[string]string{"api_key": "fixture", "base_url": "http://fixture.invalid"}}
			req := cliproxyexecutor.Request{Model: "gpt-5", Payload: []byte(`{"type":"response.create","model":"gpt-5","input":[]}`)}
			opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatCodex, Metadata: map[string]any{cliproxyexecutor.ExecutionSessionMetadataKey: id}}
			done := make(chan error, 1)
			go func() {
				turn, err := e.startOrdinaryCodexWebsocket(ctx, a, req, opts)
				if turn != nil {
					err = errors.New("closed dial installed a live connection")
				}
				done <- err
			}()
			select {
			case <-entered:
			case <-ctx.Done():
				t.Fatal("dial was not entered")
			}
			switch how {
			case "execution":
				e.CloseExecutionSession(id)
			case "credential":
				e.CloseAuthExecutionSessions(a.ID)
			default:
				cancel()
			}
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) && !errors.Is(err, errCodexWebsocketSessionClosed) {
					t.Fatalf("unexpected dial end: %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("pending dial was not canceled")
			}
			if how != "request-cancel" {
				select {
				case err := <-ch:
					if !errors.Is(err, errCodexWebsocketSessionClosed) {
						t.Fatal(err)
					}
				default:
					t.Fatal("bare observer still open")
				}
			}
		})
	}
}

func TestOrdinaryCodexRetainedReplayNeverDialsReplacement(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Second)
	defer cancel()
	g, _ := gin.CreateTestContext(nil)
	g.Set("userApiKey", "fixture-tenant")
	ctx = context.WithValue(ctx, "gin", g)
	seen := make(chan error, 1)
	drained := make(chan struct{})
	var dials atomic.Int32
	dial := directFixtureDial(func(_ context.Context, _, _ string) (net.Conn, error) {
		if dials.Add(1) != 1 {
			return nil, errors.New("unexpected replacement socket")
		}
		client, server := net.Pipe()
		go func() {
			defer server.Close()
			run := func() error {
				_ = server.SetDeadline(time.Now().Add(3 * time.Second))
				rw := bufio.NewReadWriter(bufio.NewReader(server), bufio.NewWriter(server))
				req, err := http.ReadRequest(rw.Reader)
				if err != nil {
					return err
				}
				u := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
				ws, err := u.Upgrade(&wsPipeHTTPWriter{conn: server, buf: rw, header: make(http.Header)}, req, nil)
				if err != nil {
					return err
				}
				defer ws.Close()
				for i, want := range []string{"response.create", "response.append"} {
					_, data, err := ws.ReadMessage()
					if err != nil {
						return err
					}
					if ojson.GetBytesNoCopy(data, "type").String() != want {
						return errors.New("wrong type")
					}
					if i == 1 && ojson.GetBytesNoCopy(data, "previous_response_id").String() != "response-1" {
						return errors.New("missing retained parent")
					}
					id := []string{"response-1", "response-2"}[i]
					if err := ws.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"id":"`+id+`","output":[]}}`)); err != nil {
						return err
					}
				}
				select {
				case <-drained:
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			seen <- run()
		}()
		return client, nil
	})

	e := NewCodexWebsocketsExecutor(&config.Config{})
	e.directDialContext = dial
	e.store = &codexWebsocketSessionStore{}
	defer e.CloseExecutionSession("fixture-execution")
	a := &cliproxyauth.Auth{ID: "fixture", ProxyURL: "direct", Attributes: map[string]string{"api_key": "fake", "base_url": "http://fixture.invalid"}}
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatCodex, Metadata: map[string]any{cliproxyexecutor.ExecutionSessionMetadataKey: "fixture-execution"}}
	create := cliproxyexecutor.Request{Model: "gpt-5", Payload: []byte(`{"type":"response.create","model":"gpt-5","input":[]}`)}
	appendReq := cliproxyexecutor.Request{Model: "gpt-5", Payload: []byte(`{"type":"response.append","model":"gpt-5","previous_response_id":"response-1","input":[]}`)}
	required := cliproxyexecutor.WithRequiredUpstreamWebsocket(ctx)
	if _, err := e.startOrdinaryCodexWebsocket(required, a, create, opts); !cliproxyexecutor.IsUpstreamWebsocketReplayRequired(err) {
		t.Fatalf("requires retained: %v", err)
	}
	if _, err := e.startOrdinaryCodexWebsocket(ctx, a, appendReq, opts); !cliproxyexecutor.IsUpstreamWebsocketReplayRequired(err) {
		t.Fatalf("append without retained: %v", err)
	}
	if got := dials.Load(); got != 0 {
		t.Fatalf("local replay errors dialed %d sockets", got)
	}
	for i, req := range []cliproxyexecutor.Request{create, appendReq} {
		runCtx := ctx
		if i == 1 {
			runCtx = required
		}
		turn, err := e.startOrdinaryCodexWebsocket(runCtx, a, req, opts)
		if err != nil {
			t.Fatal(err)
		}
		_, err = nextOrdinaryCodexWebsocketPayload(ctx, turn)
		e.finishOrdinaryCodexWebsocket(turn, err)
		if err != nil {
			t.Fatal(err)
		}
	}
	close(drained)
	if got := dials.Load(); got != 1 {
		t.Fatalf("dials=%d, want one retained socket", got)
	}
	select {
	case err := <-seen:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}
