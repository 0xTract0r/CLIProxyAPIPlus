package executor

import (
	"context"
	"errors"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
)

// A local transport policy with headroom, not an asserted upstream limit.
const codexFastWebsocketDefaultMessageBytes = 16*1024*1024 - 64*1024
const codexFastWebsocketMessageBytesEnv = "CPA_CODEX_FAST_WS_MAX_MESSAGE_BYTES"

func codexFastWebsocketMessageBytes() int {
	value, err := strconv.Atoi(strings.TrimSpace(os.Getenv(codexFastWebsocketMessageBytesEnv)))
	if err != nil || value <= 0 {
		return codexFastWebsocketDefaultMessageBytes
	}
	return value
}

func codexHTTPFallbackAllowed(ctx context.Context, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, body []byte) bool {
	if opts.ExecutionLifecycle != nil || cliproxyexecutor.DownstreamWebsocket(ctx) || cliproxyexecutor.RequiredUpstreamWebsocket(ctx) {
		return false
	}
	for _, payload := range [][]byte{req.Payload, opts.OriginalRequest, body} {
		if gjson.GetBytes(payload, "previous_response_id").Exists() {
			return false
		}
		if generate := gjson.GetBytes(payload, "generate"); generate.Exists() && !generate.Bool() {
			return false
		}
	}
	return true
}

func codexFastWebsocketSizeGate(ctx context.Context, body []byte, budget int, phase string) bool {
	decision := "websocket"
	if len(body) > budget {
		decision = "http_size_gate"
	}
	logCodexFastTransport(ctx, phase, decision, len(body), budget)
	return decision == "http_size_gate"
}

func logCodexFastTransport(ctx context.Context, phase, decision string, messageBytes, budget int) {
	// 文本格式化器只输出白名单字段；消息中也保留匿名决策，保证落盘可验收。
	helps.LogWithRequestID(ctx).WithFields(log.Fields{"phase": phase, "message_bytes": messageBytes, "budget_bytes": budget, "decision": decision}).Infof("codex fast transport phase=%s decision=%s message_bytes=%d budget_bytes=%d", phase, decision, messageBytes, budget)
}

func (e *CodexWebsocketsExecutor) httpFallbackExecutor() *CodexExecutor {
	if e.httpFallback != nil {
		return e.httpFallback
	}
	return e.CodexExecutor
}

// Once a WS request has been rejected, a failed HTTP recovery must not restart
// that same payload through another credential's WS path.
type codexHTTPFallbackError struct{ cause error }

func (e codexHTTPFallbackError) Error() string       { return e.cause.Error() }
func (e codexHTTPFallbackError) Unwrap() error       { return e.cause }
func (codexHTTPFallbackError) IsRequestScoped() bool { return true }
func (e codexHTTPFallbackError) StatusCode() int {
	var status cliproxyexecutor.StatusError
	if errors.As(e.cause, &status) {
		return status.StatusCode()
	}
	return http.StatusBadGateway
}
func scopedCodexHTTPFallbackError(err error) error {
	if err == nil {
		return nil
	}
	return codexHTTPFallbackError{cause: err}
}

func (e *CodexWebsocketsExecutor) executeHTTPFallbackStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, scoped bool) (*cliproxyexecutor.StreamResult, error) {
	result, err := e.httpFallbackExecutor().ExecuteStream(ctx, auth, req, opts)
	if !scoped {
		return result, err
	}
	if err != nil {
		return nil, scopedCodexHTTPFallbackError(err)
	}
	out := make(chan cliproxyexecutor.StreamChunk)
	go func() {
		defer close(out)
		for {
			select {
			case <-ctx.Done():
				return
			case chunk, ok := <-result.Chunks:
				if !ok {
					return
				}
				chunk.Err = scopedCodexHTTPFallbackError(chunk.Err)
				select {
				case out <- chunk:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return &cliproxyexecutor.StreamResult{Headers: result.Headers, Chunks: out}, nil
}
