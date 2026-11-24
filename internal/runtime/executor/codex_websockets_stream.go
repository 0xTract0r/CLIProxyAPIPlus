package executor

import (
	"context"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

func (e *CodexWebsocketsExecutor) ExecuteStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	if opts.Alt == "responses/compact" || isCodexOpenAIImageRequest(opts) {
		return e.CodexExecutor.ExecuteStream(ctx, auth, req, opts)
	}
	turn, err := e.startOrdinaryCodexWebsocket(ctx, auth, req, opts)
	if err != nil {
		return nil, err
	}
	if input := cliproxyexecutor.WebsocketInputFromContext(ctx); input != nil && e.cfg != nil && (e.cfg.Codex.ResponseSteering || e.cfg.CodexResponseSteering) {
		return e.streamCodexDuplex(ctx, auth, req, opts, turn.session, turn.connection, turn.read, input, turn.prepared, turn.reporter, turn.headers, turn.release), nil
	}

	out := make(chan cliproxyexecutor.StreamChunk)
	go func() {
		defer close(out)
		var terminalErr error
		defer func() { e.finishOrdinaryCodexWebsocket(turn, terminalErr) }()
		send := func(ch cliproxyexecutor.StreamChunk) bool {
			select {
			case <-ctx.Done():
				terminalErr = ctx.Err()
				return false
			case out <- ch:
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
				fail(mapCodexWebsocketReadError(err))
				return
			}
			helps.EmitWebSocketResponseEvent(ctx, opts, auth, e.Identifier(), req.Model, payload)
			if status, _, failed := codexTerminalFailureErrWithCooling(payload, e.modelLevelCooling()); failed {
				fail(status)
				return
			}
			kind := helps.CodexResponseEventType(payload)
			turn.reporter.ObserveResponseModel(payload)
			observeCodexTokenEvent(turn.reporter, payload)
			output = output || helps.HasMeaningfulCodexOutputDelta(payload)
			if kind == "response.output_item.done" {
				collectCodexOutputItemDone(payload, items, &fallback)
			}
			done := kind == "response.completed" || kind == "response.done" || kind == "response.incomplete"
			if done {
				if helps.IsCodexTerminalEmptyIncomplete(payload, len(items)+len(fallback), output) {
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
				return
			}
		}
	}()
	return &cliproxyexecutor.StreamResult{Headers: turn.headers, Chunks: out}, nil
}
