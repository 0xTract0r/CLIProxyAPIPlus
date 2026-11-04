package openai

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

func TestForwardResponsesStreamTerminalErrorUsesResponsesErrorChunk(t *testing.T) {
	gin.SetMode(gin.TestMode)
	base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, nil)
	h := NewOpenAIResponsesAPIHandler(base)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		t.Fatalf("expected gin writer to implement http.Flusher")
	}

	data := make(chan []byte)
	errs := make(chan *interfaces.ErrorMessage, 1)
	errs <- &interfaces.ErrorMessage{StatusCode: http.StatusInternalServerError, Error: errors.New("unexpected EOF")}
	close(errs)

	h.forwardResponsesStream(c, flusher, func(error) {}, data, errs, nil)
	body := recorder.Body.String()
	if !strings.Contains(body, `"type":"error"`) {
		t.Fatalf("expected responses error chunk, got: %q", body)
	}
	if strings.Contains(body, `"error":{`) {
		t.Fatalf("expected streaming error chunk (top-level type), got HTTP error body: %q", body)
	}
}

func TestForwardResponsesStreamTimeoutAfterDataIsFlushedOnce(t *testing.T) {
	base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, nil)
	h := NewOpenAIResponsesAPIHandler(base)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	data := make(chan []byte)
	errs := make(chan *interfaces.ErrorMessage, 1)
	go func() {
		data <- []byte("event: response.created\ndata: {\"type\":\"response.created\"}\n\n")
		errs <- &interfaces.ErrorMessage{StatusCode: http.StatusInternalServerError, Error: errors.New(`{"error":{"message":"upstream read timeout","type":"server_error","code":"upstream_read_timeout"}}`)}
		close(errs)
		close(data)
	}()
	cancels := 0
	h.forwardResponsesStream(c, c.Writer, func(err error) {
		cancels++
		if err == nil || !recorder.Flushed || !strings.Contains(recorder.Body.String(), `"code":"upstream_read_timeout"`) {
			t.Error("terminal error was not flushed before cancellation")
		}
	}, data, errs, nil)
	body := recorder.Body.String()
	if !strings.Contains(body, "response.created") || strings.Count(body, "event: error") != 1 || strings.Contains(body, "response.completed") || cancels != 1 {
		t.Fatalf("stream terminal contract changed: %s", body)
	}
}
