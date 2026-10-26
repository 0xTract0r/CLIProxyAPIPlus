package openai

import (
	"context"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type timeline434 func([]byte)

func (f timeline434) Append(_ string, p []byte, _ time.Time) { f(p) }

// data 故意不关闭：客户端已收到终态，forward 的 defer 仍未运行。
func Test434TerminalBeforeDeferredCleanup(t *testing.T) {
	gin.SetMode(gin.TestMode)
	local := newResponsesLocalInterrupt()
	data := make(chan []byte, 2)
	done := make(chan struct{})
	data <- []byte(`{"type":"response.created","response":{"id":"old"}}`)
	data <- []byte(`{"type":"response.completed","response":{"id":"old","output":[]}}`)
	router := gin.New()
	router.GET("/", func(c *gin.Context) {
		conn, err := (&websocket.Upgrader{}).Upgrade(c.Writer, c.Request, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		defer close(done)
		_, _, _, _, _ = (*OpenAIResponsesAPIHandler)(nil).forwardResponsesWebsocket(c, newResponsesWebsocketWriter(conn), func(...interface{}) {}, data, nil, timeline434(func(p []byte) {
			if gjson.GetBytes(p, "type").String() == "response.completed" && local.deliver([]byte(`{"response_id":"old"}`)) {
				t.Error("terminal exposed before retirement")
			}
		}), "434", responsesWebsocketForwardOptions{localInterrupt: local, localInterruptible: func() bool { return true }})
	})
	server := httptest.NewServer(router)
	defer server.Close()
	client, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	client.SetReadDeadline(time.Now().Add(3 * time.Second))
	for _, event := range []string{"response.created", "response.completed"} {
		_, b, err := client.ReadMessage()
		if err != nil {
			t.Fatal(err)
		}
		if gjson.GetBytes(b, "type").String() != event {
			t.Fatalf("event %s", b)
		}
	}
	select {
	case <-done:
		t.Fatal("barrier invalid: cleanup already ran")
	default:
	}
	if local.deliver([]byte(`{"response_id":"unknown"}`)) {
		t.Error("unknown late interrupt accepted")
	}
	if local.deliver([]byte(`{"response_id":"old"}`)) {
		t.Error("terminal turn still cancellable")
	}
	close(data)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("cleanup blocked")
	}
}
func Test434GenerationAndCorrelation(t *testing.T) {
	local := newResponsesLocalInterrupt()
	old := local.begin()
	local.bind(old, "old")
	if local.deliver([]byte(`{"response_id":"unknown"}`)) {
		t.Fatal("unknown accepted")
	}
	if !local.deliver([]byte(`{"response_id":"old"}`)) {
		t.Fatal("genuine cancel rejected")
	}
	local.end(old)
	next := local.begin()
	local.bind(next, "new")
	local.end(old)
	local.bind(old, "stale")
	select {
	case <-next.frames:
		t.Fatal("old queue leaked")
	default:
	}
	if local.deliver([]byte(`{"response_id":"old"}`)) {
		t.Fatal("old id accepted by new gen")
	}
	if !local.deliver([]byte(`{"response_id":"new"}`)) {
		t.Fatal("late old end retired new gen")
	}
	<-next.frames
	if local.deliver([]byte(`{"type":"response.interrupt"}`)) {
		t.Fatal("missing ID must remain rejected")
	}
	local.end(next)
}
func Test434WebsocketCannotUseLocalFallback(t *testing.T) {
	local := newResponsesLocalInterrupt()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodGet, "/", nil).WithContext(ctx)
	data := make(chan []byte, 1)
	data <- []byte(`{"type":"response.created","response":{"id":"ws"}}`)
	// Nil writer returns after event; local HTTP should never have been opened.
	_, _, _, _, _ = (*OpenAIResponsesAPIHandler)(nil).forwardResponsesWebsocket(c, nil, func(...interface{}) {}, data, nil, nil, "434-ws", responsesWebsocketForwardOptions{localInterrupt: local, localInterruptible: func() bool { return false }})
	if local.deliver([]byte(`{"response_id":"ws"}`)) {
		t.Fatal("WS entered HTTP cancellation")
	}
}
