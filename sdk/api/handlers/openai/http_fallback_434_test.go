package openai

import (
	"context"
	"errors"
	"fmt"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	runtime "github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	auth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func Test434NativeDuplex1009DoesNotPretendHTTP(t *testing.T) {
	gin.SetMode(gin.TestMode)
	var sockets atomic.Int32
	cancelled := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if websocket.IsWebSocketUpgrade(r) {
			sockets.Add(1)
			c, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
			if err != nil {
				t.Error(err)
				return
			}
			defer c.Close()
			if _, _, err = c.ReadMessage(); err != nil {
				return
			}
			_ = c.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(1009, "too big"), time.Now().Add(time.Second))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"response.created\",\"response\":{\"id\":\"same-id\"}}\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(cancelled)
	}))
	defer up.Close()
	cfg := &config.Config{}
	m := auth.NewManager(nil, nil, nil)
	m.SetConfig(cfg)
	m.RegisterExecutor(runtime.NewCodexAutoExecutor(cfg))
	id := "434-real-fallback"
	model := "434-real-fallback-model"
	_, err := m.Register(context.Background(), &auth.Auth{ID: id, Provider: "codex", ProxyURL: "direct", Status: auth.StatusActive, Attributes: map[string]string{"api_key": "synthetic", "base_url": up.URL, "websockets": "true", "fast_models": "*"}})
	if err != nil {
		t.Fatal(err)
	}
	registry.GetGlobalRegistry().RegisterClient(id, "codex", []*registry.ModelInfo{{ID: model}})
	defer registry.GetGlobalRegistry().UnregisterClient(id)
	h := NewOpenAIResponsesAPIHandler(handlers.NewBaseAPIHandlers(&cfg.SDKConfig, m))
	router := gin.New()
	router.GET("/", h.ResponsesWebsocket)
	server := httptest.NewServer(router)
	defer server.Close()
	c, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	send := func(s string) {
		t.Helper()
		if err := c.WriteMessage(websocket.TextMessage, []byte(s)); err != nil {
			t.Fatal(err)
		}
	}
	send(fmt.Sprintf(`{"type":"response.create","model":%q,"input":[]}`, model))
	_, _, err = c.ReadMessage()
	var closeErr *websocket.CloseError
	if !errors.As(err, &closeErr) || closeErr.Code != 1009 {
		t.Fatalf("native duplex must preserve1009, got %v", err)
	}
	select {
	case <-cancelled:
		t.Fatal("native WS silently entered HTTP")
	default:
	}
	if sockets.Load() != 1 {
		t.Fatalf("sockets=%d", sockets.Load())
	}
}

func Test434RealHTTPMissingIDCancel(t *testing.T) {
	gin.SetMode(gin.TestMode)
	var sockets atomic.Int32
	cancelled := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if websocket.IsWebSocketUpgrade(r) {
			sockets.Add(1)
			c, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
			if err != nil {
				t.Error(err)
				return
			}
			defer c.Close()
			if _, _, err = c.ReadMessage(); err != nil {
				return
			}
			_ = c.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(1009, "too big"), time.Now().Add(time.Second))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"response.created\",\"response\":{\"id\":\"same-id\"}}\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(cancelled)
	}))
	defer up.Close()
	cfg := &config.Config{}
	m := auth.NewManager(nil, nil, nil)
	m.SetConfig(cfg)
	m.RegisterExecutor(runtime.NewCodexAutoExecutor(cfg))
	id := "434-real-fallback"
	model := "434-real-fallback-model"
	_, err := m.Register(context.Background(), &auth.Auth{ID: id, Provider: "codex", ProxyURL: "direct", Status: auth.StatusActive, Attributes: map[string]string{"api_key": "synthetic", "base_url": up.URL, "websockets": "false", "fast_models": ""}})
	if err != nil {
		t.Fatal(err)
	}
	registry.GetGlobalRegistry().RegisterClient(id, "codex", []*registry.ModelInfo{{ID: model}})
	defer registry.GetGlobalRegistry().UnregisterClient(id)
	h := NewOpenAIResponsesAPIHandler(handlers.NewBaseAPIHandlers(&cfg.SDKConfig, m))
	router := gin.New()
	router.GET("/", h.ResponsesWebsocket)
	server := httptest.NewServer(router)
	defer server.Close()
	c, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	send := func(s string) {
		t.Helper()
		if err := c.WriteMessage(websocket.TextMessage, []byte(s)); err != nil {
			t.Fatal(err)
		}
	}
	send(fmt.Sprintf(`{"type":"response.create","model":%q,"input":[]}`, model))
	read := func(event string) {
		t.Helper()
		_, body, err := c.ReadMessage()
		if err != nil {
			t.Fatal(err)
		}
		if gjson.GetBytes(body, "type").String() != event {
			t.Fatalf("expected %s got %s", event, body)
		}
	}
	read("response.created")
	send(`{"type":"response.interrupt","response_id":"unknown"}`)
	read("error")
	send(`{"type":"response.interrupt"}`)
	read("error")
	send(`{"type":"response.interrupt","response_id":"same-id"}`)
	read("response.incomplete")
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("local HTTP cancellation missing")
	}
	if sockets.Load() != 0 {
		t.Fatal("HTTP cancellation dialed websocket")
	}
}
