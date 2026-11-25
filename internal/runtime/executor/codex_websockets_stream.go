package executor

import (
	"context"
	"net/http"
	"strings"
	"github.com/MoeForge/moego/ojson"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

func (e *CodexWebsocketsExecutor) ExecuteStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	if opts.Alt == "responses/compact" || isCodexOpenAIImageRequest(opts) {
		return e.CodexExecutor.ExecuteStream(ctx, auth, req, opts)
	}
	started := time.Now()
	turn, err := e.startOrdinaryCodexWebsocket(ctx, auth, req, opts)
	if err != nil {
		return nil, err
	}
	if input := cliproxyexecutor.WebsocketInputFromContext(ctx); input != nil && e.cfg != nil && (e.cfg.Codex.ResponseSteering || e.cfg.CodexResponseSteering) {
		return e.streamCodexDuplex(ctx, auth, req, opts, turn.session, turn.connection, turn.read, input, turn.prepared, turn.reporter, turn.headers, turn.release), nil
	}

	turn.stages = helps.StartCodexMainStream(ctx, started, e.cfg)
	turn.session.connMu.Lock()
	closer := turn.session.connCloser
	turn.session.connMu.Unlock()
	unbindLiveness := func() {}
	if closer != nil {
		unbindLiveness = closer.liveness.Bind(turn.stages)
	}
	out := make(chan cliproxyexecutor.StreamChunk)
	go func() {
		defer close(out)
		var terminalErr error
		outcome := "read_error"
		defer func() {
			unbindLiveness()
			if ctx.Err() != nil && outcome != "completed" && outcome != "incomplete" {
				outcome = "context_done"
				turn.stages.ObserveDownstreamCanceled()
			}
			e.finishOrdinaryCodexWebsocket(turn, terminalErr)
			turn.stages.ExecutorDone(outcome)
		}()
		send := func(ch cliproxyexecutor.StreamChunk) bool {
			started := time.Now()
			select {
			case <-ctx.Done():
				terminalErr = ctx.Err()
				return false
			case out <- ch:
				turn.stages.ObserveSend(started)
				return true
			}
		}
		fail := func(err error) {
			terminalErr = err
			turn.reporter.PublishFailure(ctx, err)
			send(cliproxyexecutor.StreamChunk{Err: err})
		}
		items := make(map[int64][]byte)
		var fallback [][]byte
		var param any
		output := false
		for {
			payload, err := nextOrdinaryCodexWebsocketPayload(ctx, turn)
			if err != nil {
				outcome = "read_error"
				fail(mapCodexWebsocketReadError(err))
				return
			}
			turn.reporter.ObserveContentEvent(payload)
			helps.EmitWebSocketResponseEvent(ctx, opts, auth, e.Identifier(), req.Model, payload)
			if status, _, failed := codexTerminalFailureErrWithCooling(payload, e.modelLevelCooling()); failed {
				outcome = "terminal_failure"
				if status.StatusCode() == http.StatusForbidden || status.StatusCode() == http.StatusTooManyRequests {
					fail(newOrdinaryWebsocketRefusal(status, payload, time.Now()))
				} else {
					fail(status)
				}
				return
			}
			kind := helps.CodexResponseEventType(payload)
			if (kind == "response.output_text.delta" || kind == "response.refusal.delta") &&
				ojson.GetBytesNoCopy(payload, "delta").String() != "" {
				turn.stages.ObserveFirstVisible()
			}
			turn.reporter.ObserveResponseModel(payload)
			observeCodexTokenEvent(turn.reporter, payload)
			output = output || helps.HasMeaningfulCodexOutputDelta(payload)
			if kind == "response.output_item.done" {
				collectCodexOutputItemDone(payload, items, &fallback)
			}
			done := kind == "response.completed" || kind == "response.done" || kind == "response.incomplete"
			if done {
				if helps.IsCodexTerminalEmptyIncomplete(payload, len(items)+len(fallback), output) {
					outcome = "incomplete"
					fail(newCodexEmptyIncompleteStreamError())
					return
				}
				payload = patchCodexCompletedOutput(payload, items, fallback)
				if detail, ok := helps.ParseCodexUsageWithOutputItems(payload, items, fallback); ok {
					turn.reporter.Publish(ctx, detail)
				} else {
					turn.reporter.EnsurePublished(ctx)
				}
				publishCodexImageToolUsageWithOutputItems(ctx, turn.reporter, turn.body, payload, items, fallback)
			}
			if cliproxyexecutor.DownstreamWebsocket(ctx) {
				if !send(cliproxyexecutor.StreamChunk{Payload: helps.EnsureResponsesUsageDetails(payload)}) {
					return
				}
			} else {
				for _, chunk := range sdktranslator.TranslateStream(ctx, sdktranslator.FromString("codex"), cliproxyexecutor.ResponseFormatOrSource(opts), req.Model, opts.OriginalRequest, turn.body, append([]byte("data: "), payload...), &param) {
					if len(chunk) != 0 && !send(cliproxyexecutor.StreamChunk{Payload: chunk}) {
						return
					}
				}
			}
			if done {
				outcome = "completed"
				if kind == "response.incomplete" {
					outcome = "incomplete"
				}
				return
			}
		}
	}()
	return &cliproxyexecutor.StreamResult{Headers: turn.headers, Chunks: out}, nil
}

type ordinaryWebsocketRefusal struct {
	statusErr
	retryAfterHeader string
}

func (ordinaryWebsocketRefusal) IsUpstreamWebsocketRefusal() bool { return true }

func (e ordinaryWebsocketRefusal) SafeResponseHeaders() http.Header {
	if e.retryAfterHeader == "" {
		return nil
	}
	return http.Header{"Retry-After": {e.retryAfterHeader}}
}

func newOrdinaryWebsocketRefusal(status statusErr, payload []byte, now time.Time) ordinaryWebsocketRefusal {
	refusal := ordinaryWebsocketRefusal{statusErr: status}
	headers := parseCodexWebsocketErrorHeaders(payload)
	values := headers.Values("Retry-After")
	if len(values) != 1 {
		return refusal
	}
	raw := strings.TrimSpace(values[0])
	if strings.IndexFunc(raw, func(r rune) bool { return r < '0' || r > '9' }) >= 0 {
		if _, err := http.ParseTime(raw); err != nil {
			return refusal
		}
	}
	delay := parseRetryAfterHeader(headers, now)
	if delay == nil {
		return refusal
	}
	refusal.retryAfterHeader = raw
	if refusal.retryAfter == nil || *delay > *refusal.retryAfter {
		refusal.retryAfter = delay
	}
	return refusal
}
