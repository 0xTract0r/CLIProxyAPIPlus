package test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/clienterror"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor"
	_ "github.com/router-for-me/CLIProxyAPI/v8/internal/translator"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers/claude"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers/gemini"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers/openai"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

const (
	ordinaryOutageModel = "gemini-2.5-flash"
	ordinaryOutageKey   = "single-outage-fixture-key"
)

func TestOrdinarySingleGrantUpstream502AcrossPublicRoutes(t *testing.T) {
	for _, api := range []string{"claude", "chat", "responses", "gemini"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", api, stream), func(t *testing.T) {
				router, calls := ordinaryOutageRouter(t, func(w http.ResponseWriter, _ *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusBadGateway)
					_, _ = io.WriteString(w, `{"error":{"code":502,"message":"fixture backend unavailable","status":"UNAVAILABLE"}}`)
				})
				response := ordinaryOutageRequest(router, api, stream, false)
				if response.Code != http.StatusBadGateway || calls.Load() != 1 {
					t.Fatalf("want one backend call and HTTP 502, got %d calls, %d %s", calls.Load(), response.Code, response.Body)
				}
				if strings.HasPrefix(response.Header().Get("Content-Type"), "text/event-stream") || !strings.Contains(response.Body.String(), `"error"`) {
					t.Fatalf("failure before output must be an HTTP error, got %q %s", response.Header().Get("Content-Type"), response.Body)
				}
			})
		}
	}
}

func TestOrdinarySingleGrantGeminiOutputBoundary(t *testing.T) {
	const malformed = `{"candidates":[{"content":{"role":"model","parts":[]},"finishReason":"MALFORMED_FUNCTION_CALL"}]}`
	const healthy = `{"candidates":[{"content":{"role":"model","parts":[{"text":"new healthy answer"}]},"finishReason":"STOP"}]}`
	const visible = `{"candidates":[{"content":{"role":"model","parts":[{"text":"visible partial answer"}]}}]}`
	const visibleWithMalformedFinish = `{"candidates":[{"content":{"role":"model","parts":[{"text":"visible partial answer"}]},"finishReason":"MALFORMED_FUNCTION_CALL"}]}`

	t.Run("before_visible_output", func(t *testing.T) {
		const empty = ": connection closed before the first event\n\n"
		for _, tc := range []struct {
			name, api, frame string
		}{
			{"chat_empty_sse_eof", "chat", empty},
			{"responses_empty_sse_eof", "responses", empty},
			{"responses_apply_patch_malformed_eof_without_usage", "responses", "data: " + malformed + "\n\n"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				router, calls := ordinaryOutageRouter(t, func(w http.ResponseWriter, _ *http.Request) {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, tc.frame)
				})
				rec := ordinaryOutageRequest(router, tc.api, true, false)
				if rec.Code != http.StatusBadGateway || calls.Load() != 1 {
					t.Fatalf("empty upstream stream = %d %s, calls=%d, want HTTP 502 and one call", rec.Code, rec.Body, calls.Load())
				}
				for _, completion := range []string{`"finish_reason":"stop"`, "response.completed", "response.failed", "[DONE]"} {
					if strings.Contains(rec.Body.String(), completion) {
						t.Errorf("pre-output EOF unexpectedly committed SSE (%s): %s", completion, rec.Body)
					}
				}
			})
		}
	})

	t.Run("valid_empty_response_completes", func(t *testing.T) {
		const stopped = `{"candidates":[{"content":{"role":"model","parts":[]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":0,"totalTokenCount":3}}`
		router, calls := ordinaryOutageRouter(t, func(w http.ResponseWriter, r *http.Request) {
			writeOrdinaryOutageGemini(w, r, stopped)
		})
		rec := ordinaryOutageRequest(router, "responses", true, false)
		body := rec.Body.String()
		if rec.Code != http.StatusOK || calls.Load() != 1 ||
			strings.Count(body, "event: response.completed\n") != 1 ||
			strings.Contains(body, "event: response.failed") || strings.Contains(body, "response.output_text.delta") {
			t.Fatalf("valid empty STOP = %d %s, calls=%d, want one completion and no output or error", rec.Code, rec.Body, calls.Load())
		}
	})

	t.Run("caller_cancels_before_first_frame", func(t *testing.T) {
		deadline, stop := context.WithTimeout(t.Context(), 5*time.Second)
		defer stop()
		ctx, cancel := context.WithCancel(deadline)
		defer cancel()
		upstreamReady := make(chan struct{}, 1)
		upstreamCanceled := make(chan error, 1)
		router, calls := ordinaryOutageRouter(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.(http.Flusher).Flush()
			select {
			case upstreamReady <- struct{}{}:
			default:
			}
			select {
			case <-r.Context().Done():
				select {
				case upstreamCanceled <- r.Context().Err():
				default:
				}
			case <-deadline.Done():
			}
		})

		rec := httptest.NewRecorder()
		gatewayDone := make(chan struct{})
		go func() {
			defer close(gatewayDone)
			req := newOrdinaryOutageRequest("responses", true, false).WithContext(ctx)
			router.ServeHTTP(rec, req)
		}()
		wait := func(stage string, done <-chan struct{}) {
			t.Helper()
			select {
			case <-done:
			case <-deadline.Done():
				t.Fatalf("timed out waiting for %s", stage)
			}
		}
		wait("loopback flush", upstreamReady)
		cancel()
		select {
		case err := <-upstreamCanceled:
			if err != context.Canceled {
				t.Fatalf("upstream context error = %v, want context.Canceled", err)
			}
		case <-deadline.Done():
			t.Fatal("timed out waiting for upstream cancellation")
		}
		wait("gateway return", gatewayDone)
		contentType := rec.Header().Get("Content-Type")
		if calls.Load() != 1 || rec.Flushed {
			t.Fatalf("cancellation must not retry or commit streaming: calls=%d, flushed=%t", calls.Load(), rec.Flushed)
		}
		switch rec.Code {
		case http.StatusOK:
			// Gin finalizes the recorder's default status even if the canceled request wrote nothing.
			if rec.Body.Len() != 0 || contentType != "" {
				t.Fatalf("canceled request returned a success payload: type=%q, body=%q", contentType, rec.Body)
			}
		case clienterror.StatusClientClosedRequest:
			const wantBody = `{"error":{"message":"context canceled","type":"invalid_request_error"}}`
			if contentType != "application/json" || rec.Body.String() != wantBody {
				t.Fatalf("unexpected HTTP 499: type=%q, body=%q", contentType, rec.Body)
			}
		default:
			t.Fatalf("unexpected cancellation status %d: %s", rec.Code, rec.Body)
		}
	})

	t.Run("transport_truncated_after_visible_text", func(t *testing.T) {
		router, calls := ordinaryOutageRouter(t, func(w http.ResponseWriter, _ *http.Request) {
			frame := "data: " + visible + "\n\n"
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Content-Length", fmt.Sprint(len(frame)+32))
			_, _ = io.WriteString(w, frame)
		})
		rec := ordinaryOutageRequest(router, "responses", true, false)
		body := rec.Body.String()
		if rec.Code != http.StatusOK || calls.Load() != 1 ||
			!strings.Contains(body, "visible partial answer") || !strings.Contains(body, "response.failed") ||
			strings.Contains(body, "response.completed") {
			t.Fatalf("truncated response = %d %s, calls=%d, want preserved visible text and a terminal failure", rec.Code, rec.Body, calls.Load())
		}
	})

	t.Run("malformed_then_independent_healthy_request", func(t *testing.T) {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
				router, calls := ordinaryOutageRouter(t, func(w http.ResponseWriter, r *http.Request) {
					body, err := io.ReadAll(r.Body)
					if err != nil {
						http.Error(w, "fixture cannot read request", http.StatusInternalServerError)
						return
					}
					reply := malformed
					if strings.Contains(string(body), "independent healthy request") {
						reply = healthy
					}
					writeOrdinaryOutageGemini(w, r, reply)
				})
				bad := ordinaryOutageRequest(router, "gemini", stream, false)
				if bad.Code != http.StatusBadGateway || calls.Load() != 1 {
					t.Errorf("malformed output = %d %s, calls=%d, want HTTP 502 and one call", bad.Code, bad.Body, calls.Load())
				}
				good := ordinaryOutageRequest(router, "gemini", !stream, true)
				if good.Code != http.StatusOK || !strings.Contains(good.Body.String(), "new healthy answer") || calls.Load() != 2 {
					t.Fatalf("independent request on the same grant = %d %s, calls=%d, want HTTP 200 and one additional call", good.Code, good.Body, calls.Load())
				}
			})
		}
	})

	t.Run("claude_preserves_partial_text", func(t *testing.T) {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
				router, calls := ordinaryOutageRouter(t, func(w http.ResponseWriter, r *http.Request) {
					if strings.HasSuffix(r.URL.Path, ":streamGenerateContent") {
						writeOrdinaryOutageGemini(w, r, visible, malformed)
						return
					}
					writeOrdinaryOutageGemini(w, r, visibleWithMalformedFinish)
				})
				rec := ordinaryOutageRequest(router, "claude", stream, false)
				if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "visible partial answer") || calls.Load() != 1 {
					t.Fatalf("partial response = %d %s, calls=%d, want HTTP 200 preserving visible text on one call", rec.Code, rec.Body, calls.Load())
				}
				if strings.Contains(rec.Body.String(), "event: error\n") {
					t.Errorf("provider's malformed tool tail turned preserved text into a terminal error: %s", rec.Body)
				}
			})
		}
	})
}

func ordinaryOutageRouter(t *testing.T, respond http.HandlerFunc) (*gin.Engine, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		queryKey := r.URL.Query().Get("key")
		headerKey := r.Header.Get("x-goog-api-key")
		if r.Method != http.MethodPost ||
			(queryKey == "" && headerKey == "") ||
			(queryKey != "" && queryKey != ordinaryOutageKey) ||
			(headerKey != "" && headerKey != ordinaryOutageKey) {
			t.Error("unexpected method or credential on loopback backend")
			http.Error(w, "invalid test request", http.StatusUnauthorized)
			return
		}
		respond(w, r)
	}))
	t.Cleanup(upstream.Close)

	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(executor.NewGeminiExecutor(&config.Config{}))
	auth := &coreauth.Auth{
		ID: "ordinary-outage-grant", Provider: "gemini", Status: coreauth.StatusActive, ProxyURL: "direct",
		Attributes: map[string]string{"api_key": ordinaryOutageKey, "base_url": upstream.URL},
	}
	if _, err := manager.Register(t.Context(), auth); err != nil {
		t.Fatal(err)
	}
	registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: ordinaryOutageModel}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })

	gin.SetMode(gin.TestMode)
	router := gin.New()
	base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager)
	router.POST("/v1/messages", claude.NewClaudeCodeAPIHandler(base).ClaudeMessages)
	router.POST("/v1/chat/completions", openai.NewOpenAIAPIHandler(base).ChatCompletions)
	router.POST("/v1/responses", openai.NewOpenAIResponsesAPIHandler(base).Responses)
	router.POST("/v1beta/models/*action", gemini.NewGeminiAPIHandler(base).GeminiHandler)
	return router, &calls
}

func ordinaryOutageRequest(router http.Handler, api string, stream, healthy bool) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, newOrdinaryOutageRequest(api, stream, healthy))
	return rec
}

func newOrdinaryOutageRequest(api string, stream, healthy bool) *http.Request {
	text := "ordinary first request"
	if healthy {
		text = "independent healthy request"
	}
	var path, body string
	switch api {
	case "claude":
		path = "/v1/messages"
		body = fmt.Sprintf(`{"model":%q,"stream":%t,"max_tokens":32,"messages":[{"role":"user","content":%q}]}`, ordinaryOutageModel, stream, text)
	case "chat":
		path = "/v1/chat/completions"
		body = fmt.Sprintf(`{"model":%q,"stream":%t,"messages":[{"role":"user","content":%q}]}`, ordinaryOutageModel, stream, text)
	case "responses":
		path = "/v1/responses"
		body = fmt.Sprintf(`{"model":%q,"stream":%t,"input":%q,"tools":[{"type":"custom","name":"apply_patch"}]}`, ordinaryOutageModel, stream, text)
	case "gemini":
		path = "/v1beta/models/" + ordinaryOutageModel + ":generateContent"
		if stream {
			path = "/v1beta/models/" + ordinaryOutageModel + ":streamGenerateContent?alt=sse"
		}
		body = fmt.Sprintf(`{"contents":[{"role":"user","parts":[{"text":%q}]}]}`, text)
	}

	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	return req
}

func writeOrdinaryOutageGemini(w http.ResponseWriter, r *http.Request, frames ...string) {
	if strings.HasSuffix(r.URL.Path, ":streamGenerateContent") {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, frame := range frames {
			_, _ = fmt.Fprintf(w, "data: %s\n\n", frame)
		}
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, frames[0])
}
