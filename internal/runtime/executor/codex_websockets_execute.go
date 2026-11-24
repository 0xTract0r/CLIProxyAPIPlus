package executor

import (
	"context"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func (e *CodexWebsocketsExecutor) Execute(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (_ cliproxyexecutor.Response, err error) {
	if opts.Alt == "responses/compact" || isCodexOpenAIImageRequest(opts) {
		return e.CodexExecutor.Execute(ctx, auth, req, opts)
	}
	turn, err := e.startOrdinaryCodexWebsocket(ctx, auth, req, opts)
	if err != nil {
		return cliproxyexecutor.Response{}, err
	}
	defer func() { e.finishOrdinaryCodexWebsocket(turn, err) }()
	defer turn.reporter.TrackFailure(ctx, &err)
	items := make(map[int64][]byte)
	var fallback [][]byte
	output := false
	for {
		payload, err := nextOrdinaryCodexWebsocketPayload(ctx, turn)
		if err != nil {
			return cliproxyexecutor.Response{}, mapCodexWebsocketReadError(err)
		}
		helps.EmitWebSocketResponseEvent(ctx, opts, auth, e.Identifier(), req.Model, payload)
		if status, _, failed := codexTerminalFailureErrWithCooling(payload, e.modelLevelCooling()); failed {
			return cliproxyexecutor.Response{}, status
		}
		kind := helps.CodexResponseEventType(payload)
		turn.reporter.ObserveResponseModel(payload)
		observeCodexTokenEvent(turn.reporter, payload)
		output = output || helps.HasMeaningfulCodexOutputDelta(payload)
		if kind == "response.output_item.done" {
			collectCodexOutputItemDone(payload, items, &fallback)
		}
		if kind != "response.completed" && kind != "response.done" && kind != "response.incomplete" {
			continue
		}
		if helps.IsCodexTerminalEmptyIncomplete(payload, len(items)+len(fallback), output) {
			return cliproxyexecutor.Response{}, newCodexEmptyIncompleteStreamError()
		}
		complete := patchCodexCompletedOutput(payload, items, fallback)
		if detail, ok := helps.ParseCodexUsageWithOutputItems(complete, items, fallback); ok {
			turn.reporter.Publish(ctx, detail)
		} else {
			turn.reporter.EnsurePublished(ctx)
		}
		publishCodexImageToolUsageWithOutputItems(ctx, turn.reporter, turn.body, complete, items, fallback)
		return translateOrdinaryCodexResponse(ctx, req, opts, turn.body, complete, turn.headers, kind == "response.incomplete")
	}
}
