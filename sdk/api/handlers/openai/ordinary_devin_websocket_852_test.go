package openai

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/MoeForge/moego/ojson"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

type ordinaryPipeListener struct {
	conn net.Conn
	stop chan struct{}
	once sync.Once
}

func (l *ordinaryPipeListener) Accept() (net.Conn, error) {
	if l.conn != nil {
		conn := l.conn
		l.conn = nil
		return conn, nil
	}
	<-l.stop
	return nil, net.ErrClosed
}

func (l *ordinaryPipeListener) Addr() net.Addr { return &net.TCPAddr{} }
func (l *ordinaryPipeListener) Close() error {
	l.once.Do(func() { close(l.stop) })
	return nil
}

func TestOrdinaryDevinWebsocketRealTurnsStayOnSelectedCredential(t *testing.T) {
	const native = "devin/swe-1-6-slow"
	const alias = "ordinary-devin-live(high)"
	const selected = "devin-live-selected"
	const other = "devin-live-other"
	selector := &orderedWebsocketSelector{order: []string{selected, other}}
	m := coreauth.NewManager(nil, selector, nil)
	r := registry.GetGlobalRegistry()
	for _, id := range []string{selected, other} {
		a := &coreauth.Auth{ID: id, Provider: "devin", Status: coreauth.StatusActive}
		if _, err := m.Register(t.Context(), a); err != nil {
			t.Fatal(err)
		}
		r.RegisterClient(id, "devin", []*registry.ModelInfo{{ID: alias, MetadataModelID: native, OAuthAliasSource: native}})
		t.Cleanup(func() { r.UnregisterClient(id) })
	}
	var mu sync.Mutex
	var attempts []string
	m.RegisterExecutor(ordinaryDevinExecutor{call: func(a *coreauth.Auth, req coreexecutor.Request) (*coreexecutor.StreamResult, error) {
		mu.Lock()
		attempts = append(attempts, a.ID+":"+req.Model)
		n := len(attempts)
		mu.Unlock()
		if n >= 3 {
			return nil, websocketPinnedFailoverStatusError{status: http.StatusForbidden, msg: "ordinary provider denied turn"}
		}
		ch := make(chan coreexecutor.StreamChunk, 1)
		ch <- coreexecutor.StreamChunk{Payload: []byte(fmt.Sprintf(`{"type":"response.completed","response":{"id":"resp-%d","output":[]}}`, n))}
		close(ch)
		return &coreexecutor.StreamResult{Chunks: ch}, nil
	}})

	router := gin.New()
	router.GET("/v1/responses/ws", NewOpenAIResponsesAPIHandler(handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, m)).ResponsesWebsocket)
	clientPipe, serverPipe := net.Pipe()
	defer clientPipe.Close()
	defer serverPipe.Close()
	l := &ordinaryPipeListener{conn: serverPipe, stop: make(chan struct{})}
	server := &http.Server{Handler: router, ReadHeaderTimeout: time.Second}
	serverDone := make(chan struct{})
	go func() { defer close(serverDone); _ = server.Serve(l) }()
	t.Cleanup(func() { _ = l.Close(); <-serverDone })
	dialer := websocket.Dialer{HandshakeTimeout: 3 * time.Second,
		NetDialContext: func(context.Context, string, string) (net.Conn, error) { return clientPipe, nil }}
	conn, _, err := dialer.DialContext(t.Context(), "ws://in-memory.invalid/v1/responses/ws", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(4 * time.Second))
	_ = conn.SetWriteDeadline(time.Now().Add(4 * time.Second))
	for i := 1; i <= 3; i++ {
		msg := fmt.Sprintf(`{"type":"response.create","model":%q,"input":[{"type":"message","role":"user","content":"ordinary turn %d"}]}`, alias, i)
		if err := conn.WriteMessage(websocket.TextMessage, []byte(msg)); err != nil {
			t.Fatal(err)
		}
		_, payload, err := conn.ReadMessage()
		if err != nil {
			t.Fatal(err)
		}
		event := ojson.GetBytesNoCopy(payload, "type").String()
		if i < 3 && event != "response.completed" {
			t.Fatalf("turn %d did not complete: %s", i, payload)
		}
		if i == 3 && (event != "error" || ojson.GetBytesNoCopy(payload, "status").Int() != http.StatusForbidden) {
			t.Fatalf("expected original refusal: %s", payload)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	want := selected + ":" + native + "(high)"
	if !slices.Equal(attempts, []string{want, want, want}) {
		t.Errorf("selected credential/model changed or was retried: %v", attempts)
	}
}
