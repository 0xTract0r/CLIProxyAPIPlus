package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// These invented values reproduce only the observed JSON shape, never live content.
func codexSyntheticHistoryItem(kind string) map[string]any {
	switch kind {
	case "additional_tools":
		return map[string]any{"type": kind, "id": "tools_synthetic", "role": "developer", "tools": []any{map[string]any{"type": "namespace", "name": "synthetic_tools", "description": "Synthetic namespace", "tools": []any{map[string]any{"type": "function", "name": "lookup", "description": "Synthetic lookup", "parameters": map[string]any{"type": "object", "properties": map[string]any{}}}}}}}
	case "compaction":
		return map[string]any{"type": kind, "id": "compact_synthetic", "encrypted_content": "synthetic-opaque-compaction", "internal_chat_message_metadata_passthrough": map[string]any{"turn_id": "turn_synthetic"}}
	case "agent_message":
		return map[string]any{"type": kind, "id": "agent_synthetic", "author": "assistant", "recipient": "all", "content": []any{map[string]any{"type": "input_text", "text": "Synthetic agent history"}, map[string]any{"type": "encrypted_content", "encrypted_content": "synthetic-opaque-agent"}}, "internal_chat_message_metadata_passthrough": map[string]any{"turn_id": "turn_synthetic", "create_time": 123.0}}
	}
	return nil
}

func codexSyntheticHistoryRequest(marker string, kinds ...string) cliproxyexecutor.Request {
	input := make([]any, 0, len(kinds)+5)
	for _, kind := range kinds {
		input = append(input, codexSyntheticHistoryItem(kind))
	}
	if len(kinds) > 0 {
		input = append(input, map[string]any{"type": "function_call", "call_id": "call_function", "name": "lookup", "arguments": "{}"}, map[string]any{"type": "custom_tool_call", "call_id": "call_custom", "name": "read", "input": "synthetic"}, map[string]any{"type": "custom_tool_call_output", "call_id": "call_custom", "output": "synthetic custom result"}, map[string]any{"type": "function_call_output", "call_id": "call_function", "output": "synthetic function result"})
	}
	input = append(input, map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": marker}}})
	body, _ := json.Marshal(map[string]any{"model": "gpt-5-codex", "prompt_cache_key": "synthetic-shared", "input": input})
	return cliproxyexecutor.Request{Model: "gpt-5-codex", Payload: body}
}

func TestCodexFastIndependentCompleteHistoryItems(t *testing.T) {
	for _, kind := range []string{"additional_tools", "compaction", "agent_message", "mixed"} {
		t.Run(kind, func(t *testing.T) {
			kinds := []string{kind}
			if kind == "mixed" {
				kinds = []string{"additional_tools", "compaction", "agent_message"}
			}
			req := codexSyntheticHistoryRequest("REQUEST_B", kinds...)
			opts := cliproxyexecutor.Options{OriginalRequest: append([]byte(nil), req.Payload...)}
			if !codexFastIndependentRequest(context.Background(), req, opts, req.Payload, true) {
				t.Fatal("complete self-contained history was classified dependent")
			}
		})
	}
}

func TestCodexFastExtendedHistoryConcurrentRequests(t *testing.T) {
	for _, streamA := range []bool{false, true} {
		for _, streamB := range []bool{false, true} {
			t.Run(fmt.Sprintf("A=%t/B=%t", streamA, streamB), func(t *testing.T) {
				startedA := make(chan struct{})
				releaseA := make(chan struct{})
				var once sync.Once
				release := func() { once.Do(func() { close(releaseA) }) }
				defer release()
				var connections, httpCalls atomic.Int32
				capturedB := make(chan []byte, 1)
				upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if !websocket.IsWebSocketUpgrade(r) {
						httpCalls.Add(1)
						http.Error(w, "unexpected HTTP", 500)
						return
					}
					conn, err := upgrader.Upgrade(w, r, nil)
					if err != nil {
						return
					}
					connections.Add(1)
					defer func() { _ = conn.Close() }()
					for {
						_, body, err := conn.ReadMessage()
						if err != nil {
							return
						}
						if gjson.GetBytes(body, "service_tier").String() != "priority" {
							t.Error("history request lost priority")
						}
						if generate := gjson.GetBytes(body, "generate"); generate.Exists() && !generate.Bool() {
							_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"id":"resp_warm","status":"completed","output":[]}}`))
							continue
						}
						items := gjson.GetBytes(body, "input").Array()
						marker := items[len(items)-1].Get("content.0.text").String()
						if gjson.GetBytes(body, "previous_response_id").String() != "resp_warm" {
							t.Error("history request lost prewarm link")
						}
						if marker == "REQUEST_A" {
							_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.created","response":{"id":"resp_A","status":"in_progress","output":[]}}`))
							close(startedA)
							<-releaseA
						} else {
							capturedB <- body
						}
						_ = conn.WriteJSON(map[string]any{"type": "response.completed", "response": map[string]any{"id": "resp_" + marker, "status": "completed", "output": []any{map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "result_" + marker}}}}, "usage": map[string]any{"input_tokens": 3, "output_tokens": 1, "total_tokens": 4}}})
					}
				}))
				defer server.Close()
				exec := NewCodexAutoExecutor(&config.Config{SDKConfig: config.SDKConfig{DisableImageGeneration: config.DisableImageGenerationAll}})
				exec.wsExec.store = &codexWebsocketSessionStore{sessions: make(map[string]*codexWebsocketSession)}
				auth := &cliproxyauth.Auth{ID: t.Name(), Provider: "codex", ProxyURL: "direct", Attributes: map[string]string{"api_key": "test", "base_url": server.URL, "fast_models": "*"}}
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				defer exec.CloseExecutionSession("codex-fast:pck:synthetic-shared")
				opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("codex"), ResponseFormat: sdktranslator.FromString("codex")}
				run := func(req cliproxyexecutor.Request, stream bool) <-chan error {
					done := make(chan error, 1)
					go func() {
						body, _, err := runCodexFallbackRequest(ctx, exec, auth, req, opts, stream)
						items := gjson.GetBytes(req.Payload, "input").Array()
						marker := items[len(items)-1].Get("content.0.text").String()
						if err == nil && !strings.Contains(string(body), "result_"+marker) {
							err = fmt.Errorf("response marker mismatch")
						}
						done <- err
					}()
					return done
				}
				doneA := run(codexSyntheticHistoryRequest("REQUEST_A"), streamA)
				select {
				case <-startedA:
				case <-time.After(time.Second):
					t.Fatal("A did not reach long-response barrier")
				}
				reqB := codexSyntheticHistoryRequest("REQUEST_B", "additional_tools", "compaction", "agent_message")
				doneB := run(reqB, streamB)
				select {
				case err := <-doneB:
					if err != nil {
						t.Fatalf("B: %v", err)
					}
				case <-time.After(time.Second):
					t.Fatal("complete history B queued behind long A")
				}
				if connections.Load() != 2 || httpCalls.Load() != 0 {
					t.Fatalf("WS=%d HTTP=%d", connections.Load(), httpCalls.Load())
				}
				if body := <-capturedB; gjson.GetBytes(body, "input").Raw != gjson.GetBytes(reqB.Payload, "input").Raw {
					t.Fatal("history input changed on extra connection")
				}
				release()
				select {
				case err := <-doneA:
					if err != nil {
						t.Fatalf("A: %v", err)
					}
				case <-time.After(time.Second):
					t.Fatal("A did not finish")
				}
			})
		}
	}
}

func TestCodexFastExtendedHistoryExistingGuards(t *testing.T) {
	for _, guard := range []string{"previous-request", "previous-original", "previous-body", "previous-null", "generate-false", "execution-session", "native", "required", "lifecycle", "unknown", "reference", "pending"} {
		t.Run(guard, func(t *testing.T) {
			req := codexSyntheticHistoryRequest("REQUEST_B", "additional_tools", "compaction", "agent_message")
			body := append([]byte(nil), req.Payload...)
			opts := cliproxyexecutor.Options{OriginalRequest: append([]byte(nil), req.Payload...)}
			ctx := context.Background()
			switch guard {
			case "previous-request":
				req.Payload, _ = sjson.SetBytes(req.Payload, "previous_response_id", "resp_old")
			case "previous-original":
				opts.OriginalRequest, _ = sjson.SetBytes(opts.OriginalRequest, "previous_response_id", "resp_old")
			case "previous-body":
				body, _ = sjson.SetBytes(body, "previous_response_id", "resp_old")
			case "previous-null":
				req.Payload, _ = sjson.SetRawBytes(req.Payload, "previous_response_id", []byte("null"))
			case "generate-false":
				req.Payload, _ = sjson.SetBytes(req.Payload, "generate", false)
			case "execution-session":
				opts.Metadata = map[string]any{cliproxyexecutor.ExecutionSessionMetadataKey: "real_session"}
			case "native":
				ctx = cliproxyexecutor.WithDownstreamWebsocket(ctx)
			case "required":
				ctx = cliproxyexecutor.WithRequiredUpstreamWebsocket(ctx)
			case "lifecycle":
				opts.ExecutionLifecycle = newTerminalFailureLifecycle()
			case "unknown":
				body, _ = sjson.SetBytes(body, "input.-1", map[string]any{"type": "unverified_tool", "call_id": "old"})
			case "reference":
				body, _ = sjson.SetBytes(body, "input.-1", map[string]any{"type": "item_reference", "id": "old"})
			case "pending":
				body, _ = sjson.SetBytes(body, "input.-1", map[string]any{"type": "function_call", "call_id": "pending", "name": "read", "arguments": "{}"})
			}
			if codexFastIndependentRequest(ctx, req, opts, body, true) {
				t.Fatal("history item bypassed existing dependent-request guard")
			}
		})
	}
}

func TestCodexFastExtendedHistoryMalformedItems(t *testing.T) {
	cases := []struct {
		kind, path, value string
		remove            bool
	}{
		{"additional_tools", "role", `false`, false},
		{"additional_tools", "tools", `{}`, false},
		{"additional_tools", "tools.0.name", `false`, false},
		{"additional_tools", "tools.0.name", "", true},
		{"additional_tools", "tools.0.tools", `{}`, false},
		{"additional_tools", "role", `"user"`, false},
		{"additional_tools", "tools", `[]`, false},
		{"additional_tools", "tools.0.name", `""`, false},
		{"additional_tools", "tools.0.tools.0.parameters", "", true},
		{"additional_tools", "tools.0.tools.0.type", `"tool_reference"`, false},
		{"additional_tools", "tools.0.tools.0", `{"type":"custom","name":"read"}`, false},
		{"additional_tools", "tools.0.tools.0", `{"type":"custom","name":"read","format":{"type":"grammar","syntax":"lark","definition":""}}`, false},
		{"compaction", "encrypted_content", "", true},
		{"compaction", "encrypted_content", `null`, false},
		{"compaction", "encrypted_content", `""`, false},
		{"agent_message", "author", `false`, false},
		{"agent_message", "author", `""`, false},
		{"agent_message", "recipient", `false`, false},
		{"agent_message", "recipient", `""`, false},
		{"agent_message", "content", "", true},
		{"agent_message", "content", `{}`, false},
		{"agent_message", "content.0.text", `false`, false},
		{"agent_message", "content.1.encrypted_content", `null`, false},
		{"agent_message", "content.1.type", `"unverified_reference"`, false},
	}
	for i, tc := range cases {
		t.Run(fmt.Sprintf("%s/%s/%d", tc.kind, tc.path, i), func(t *testing.T) {
			req := codexSyntheticHistoryRequest("REQUEST_B", tc.kind)
			if tc.remove {
				req.Payload, _ = sjson.DeleteBytes(req.Payload, "input.0."+tc.path)
			} else {
				req.Payload, _ = sjson.SetRawBytes(req.Payload, "input.0."+tc.path, []byte(tc.value))
			}
			opts := cliproxyexecutor.Options{OriginalRequest: append([]byte(nil), req.Payload...)}
			if codexFastIndependentRequest(context.Background(), req, opts, req.Payload, true) {
				t.Fatal("malformed history item classified independent")
			}
		})
	}
}

func TestCodexFastHistoryValidContentVariants(t *testing.T) {
	for _, variant := range []string{"direct-function", "custom-text", "custom-grammar", "agent-text", "agent-encrypted", "empty-text", "inactive-multi-agent"} {
		t.Run(variant, func(t *testing.T) {
			req := codexSyntheticHistoryRequest("REQUEST_B", "additional_tools", "compaction", "agent_message")
			switch variant {
			case "direct-function":
				tool := gjson.GetBytes(req.Payload, "input.0.tools.0.tools.0")
				req.Payload, _ = sjson.SetRawBytes(req.Payload, "input.0.tools", []byte("["+tool.Raw+"]"))
			case "custom-text":
				req.Payload, _ = sjson.SetBytes(req.Payload, "input.0.tools.0.tools.0", map[string]any{"type": "custom", "name": "read", "format": map[string]any{"type": "text"}})
			case "custom-grammar":
				req.Payload, _ = sjson.SetBytes(req.Payload, "input.0.tools.0.tools.0", map[string]any{"type": "custom", "name": "read", "format": map[string]any{"type": "grammar", "syntax": "lark", "definition": "start: WORD"}})
			case "agent-text":
				req.Payload, _ = sjson.DeleteBytes(req.Payload, "input.2.content.1")
			case "agent-encrypted":
				req.Payload, _ = sjson.DeleteBytes(req.Payload, "input.2.content.0")
			case "empty-text":
				req.Payload, _ = sjson.SetBytes(req.Payload, "input.2.content.0.text", "")
			case "inactive-multi-agent":
				req.Payload, _ = sjson.SetBytes(req.Payload, "multi_agent.enabled", false)
			}
			if !codexFastIndependentRequest(context.Background(), req, cliproxyexecutor.Options{OriginalRequest: req.Payload}, req.Payload, true) {
				t.Fatal("complete history variant classified dependent")
			}
		})
	}
}

func TestCodexFastHistoryActiveConnectionGuards(t *testing.T) {
	for _, source := range []string{"request", "original", "body"} {
		for _, key := range []string{"multi_agent.enabled", "conversation", "conversation_id", "response_id", "stream_id"} {
			t.Run(source+"/"+key, func(t *testing.T) {
				req := codexSyntheticHistoryRequest("REQUEST_B", "additional_tools", "compaction", "agent_message")
				body := req.Payload
				opts := cliproxyexecutor.Options{OriginalRequest: append([]byte(nil), req.Payload...)}
				var value any = "external_anchor"
				if key == "multi_agent.enabled" {
					value = true
				}
				switch source {
				case "request":
					req.Payload, _ = sjson.SetBytes(req.Payload, key, value)
				case "original":
					opts.OriginalRequest, _ = sjson.SetBytes(opts.OriginalRequest, key, value)
				case "body":
					body, _ = sjson.SetBytes(body, key, value)
				}
				if codexFastIndependentRequest(context.Background(), req, opts, body, true) {
					t.Fatal("active connection state classified history")
				}
			})
		}
	}
}

type codexHistoryLogBuffer struct {
	mu     sync.Mutex
	output bytes.Buffer
}

func (b *codexHistoryLogBuffer) Write(data []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.output.Write(data)
}
func (b *codexHistoryLogBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.output.String()
}

func TestCodexFastHistoryEligibilityLogging(t *testing.T) {
	logger := log.StandardLogger()
	formatter, output, level := logger.Formatter, logger.Out, logger.GetLevel()
	t.Cleanup(func() { logger.SetFormatter(formatter); logger.SetOutput(output); logger.SetLevel(level) })
	var captured codexHistoryLogBuffer
	logger.SetFormatter(&logging.LogFormatter{})
	logger.SetOutput(&captured)
	logger.SetLevel(log.InfoLevel)
	ctx := logging.WithRequestID(context.Background(), "history1")
	req := codexSyntheticHistoryRequest("SENSITIVE_SYNTHETIC_MARKER", "additional_tools", "compaction", "agent_message")
	if !codexFastIndependentRequest(ctx, req, cliproxyexecutor.Options{}, req.Payload, true) {
		t.Fatal("complete history refused")
	}
	req.Payload, _ = sjson.SetBytes(req.Payload, "input.-1", map[string]any{"type": "SENSITIVE_SYNTHETIC_TYPE"})
	if codexFastIndependentRequest(ctx, req, cliproxyexecutor.Options{}, req.Payload, true) {
		t.Fatal("unknown history accepted")
	}
	logged := captured.String()
	for _, value := range []string{"history1", "independent=true reason=complete_input", "independent=false reason=unknown_input_item"} {
		if !strings.Contains(logged, value) {
			t.Fatalf("eligibility diagnostic missing %q", value)
		}
	}
	for _, value := range []string{"SENSITIVE_SYNTHETIC", "synthetic-opaque", "synthetic-shared", "compact_synthetic", "agent_synthetic"} {
		if strings.Contains(logged, value) {
			t.Fatal("eligibility log leaked input/identity")
		}
	}
}
