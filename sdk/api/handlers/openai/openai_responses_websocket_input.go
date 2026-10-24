package openai

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/interfaces"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/tidwall/gjson"
)

// responsesLocalInterrupt cancels an in-flight HTTP turn. A websocket upstream
// does not use it; that interrupt is written to the existing socket instead.
type responsesLocalInterrupt struct {
	mu      sync.Mutex
	current *responsesLocalInterruptTurn
}
type responsesLocalInterruptTurn struct {
	responseID string
	frames     chan []byte
}

func newResponsesLocalInterrupt() *responsesLocalInterrupt { return &responsesLocalInterrupt{} }

// 每代拥有自己的队列；迟到的 end/事件永远不能修改下一代。
func (s *responsesLocalInterrupt) begin() *responsesLocalInterruptTurn {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	turn := &responsesLocalInterruptTurn{frames: make(chan []byte, 1)}
	s.current = turn
	return turn
}
func (s *responsesLocalInterrupt) bind(turn *responsesLocalInterruptTurn, id string) {
	if s == nil || turn == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.current == turn {
		turn.responseID = id
	}
}
func (s *responsesLocalInterrupt) end(turn *responsesLocalInterruptTurn) {
	if s == nil || turn == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.current == turn {
		s.current = nil
	}
}
func (s *responsesLocalInterrupt) deliver(payload []byte) bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	turn := s.current
	if turn == nil {
		return false
	}
	id := strings.TrimSpace(gjson.GetBytes(payload, "response_id").String())
	if id == "" || id != turn.responseID {
		return false
	}
	select {
	case turn.frames <- bytes.Clone(payload):
	default:
	}
	return true
}

// readResponsesWebsocketInput is the only downstream reader. Interrupts are
// handled immediately so they are not queued behind an active response.
// The bounded queue backpressures clients instead of retaining unlimited input.
func readResponsesWebsocketInput(ctx context.Context, cancel context.CancelCauseFunc, conn *websocket.Conn, interrupt func([]byte) error, local *responsesLocalInterrupt, writer *responsesWebsocketWriter, timeline websocketTimelineAppender) (<-chan cliproxyexecutor.WebsocketInput, <-chan struct{}) {
	input := make(chan cliproxyexecutor.WebsocketInput, 16)
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer close(input)
		for {
			kind, payload, errRead := conn.ReadMessage()
			if errRead != nil {
				cancel(errRead)
				return
			}
			if kind != websocket.TextMessage && kind != websocket.BinaryMessage {
				continue
			}
			// Control frames bypass response.create normalization. The original
			// response_id, mode, and extension fields must reach upstream unchanged.
			if json.Valid(payload) && gjson.GetBytes(payload, "type").String() == "response.interrupt" {
				appendResponsesInterruptDiagnostic(timeline, payload, "")
				errInterrupt := interrupt(payload)
				switch {
				case errInterrupt == nil:
					appendResponsesInterruptDiagnostic(timeline, payload, "handled")
					continue
				case errors.Is(errInterrupt, cliproxyexecutor.ErrNoActiveUpstreamWebsocket) && local.deliver(payload):
					appendResponsesInterruptDiagnostic(timeline, payload, "local_http")
					continue
				default:
					appendResponsesInterruptDiagnostic(timeline, payload, "rejected")
					// The sanitized outcome replaces raw error logging here: errors
					// from transport dependencies may contain credentials or URLs.
					_, errWrite := writeResponsesWebsocketError(writer, nil, &interfaces.ErrorMessage{
						StatusCode: http.StatusBadRequest,
						Error:      errInterrupt,
					})
					if errWrite != nil {
						cancel(errWrite)
						return
					}
					continue
				}
			}
			select {
			case input <- cliproxyexecutor.WebsocketInput{Payload: payload}:
			case <-ctx.Done():
				return
			}
		}
	}()
	return input, done
}

func appendResponsesInterruptDiagnostic(timeline websocketTimelineAppender, payload []byte, outcome string) {
	if timeline == nil {
		return
	}
	// Hash client-controlled IDs and omit all arbitrary fields, including mode.
	// Only correlation metadata is needed to diagnose control-frame timing.
	idHash := sha256.Sum256([]byte(gjson.GetBytes(payload, "response_id").String()))
	diagnostic := struct {
		Type           string `json:"type"`
		ResponseIDHash string `json:"response_id_hash"`
		Outcome        string `json:"outcome,omitempty"`
	}{Type: "response.interrupt", ResponseIDHash: fmt.Sprintf("%x", idHash[:8]), Outcome: outcome}
	data, errMarshal := json.Marshal(diagnostic)
	if errMarshal != nil {
		return
	}
	event := "interrupt"
	if outcome != "" {
		event = "interrupt.outcome"
	}
	timeline.Append(event, data, time.Now())
}

func (h *OpenAIResponsesAPIHandler) forwardResponsesWebsocketInterrupt(ctx context.Context, sessionID string, payload []byte) error {
	responseID := gjson.GetBytes(payload, "response_id")
	if responseID.Type != gjson.String || strings.TrimSpace(responseID.String()) == "" {
		return fmt.Errorf("response.interrupt requires response_id")
	}
	if h == nil || h.AuthManager == nil {
		return cliproxyexecutor.ErrNoActiveUpstreamWebsocket
	}
	exec, ok := h.AuthManager.Executor("codex")
	if !ok || exec == nil {
		return cliproxyexecutor.ErrNoActiveUpstreamWebsocket
	}
	sender, ok := exec.(interface {
		InterruptExecutionSession(context.Context, string, []byte) error
	})
	if !ok || sender == nil {
		return fmt.Errorf("upstream executor does not support response.interrupt")
	}
	ctx = cliproxyexecutor.WithWebsocketAuthCheck(ctx, func(authID string) bool {
		// Home runtime credentials live on the execution session, not in the global map.
		auth, found := h.AuthManager.GetByID(authID)
		if !found || auth == nil {
			auth, found = h.AuthManager.GetExecutionSessionAuthByID(sessionID, authID)
		}
		return found && auth != nil && !auth.Disabled && auth.Status != coreauth.StatusDisabled
	})
	return sender.InterruptExecutionSession(ctx, sessionID, payload)
}
