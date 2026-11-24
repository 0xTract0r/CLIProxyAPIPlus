package executor

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"

	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/thinking"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

func (e *CodexExecutor) Execute(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (_ cliproxyexecutor.Response, err error) {
	baseModel := thinking.ParseSuffix(req.Model).ModelName
	compat, err := e.expandCodexResponsesCompaction(auth, &req, &opts, baseModel)
	if err != nil {
		return cliproxyexecutor.Response{}, statusErr{code: 400, msg: err.Error(), requestScoped: true}
	}
	if isCodexOpenAIImageRequest(opts) {
		return e.executeOpenAIImage(ctx, auth, req, opts)
	}
	if err = rejectSyntheticCompactionPreviousResponseID(ctx, req.Payload); err != nil {
		return cliproxyexecutor.Response{}, err
	}
	if !codexCompactionSummaryFromContext(ctx) && e.shouldCompactCompatResponsesTrigger(compat, req, opts) {
		return e.executeCodexResponsesCompaction(ctx, auth, req, opts)
	}

	reporter := helps.NewExecutorUsageReporter(ctx, e, baseModel, auth)
	defer reporter.TrackFailure(ctx, &err)
	endpoint := "/responses"
	directCompact := opts.Alt == "responses/compact"
	if directCompact {
		endpoint += "/compact"
	}
	if !directCompact {
		reporter.UseDecodedContentTelemetry()
	}
	resp, requestBody, err := e.sendOrdinaryCodex(ctx, auth, req, opts, endpoint, !directCompact, compat, reporter)
	if err != nil {
		return cliproxyexecutor.Response{}, err
	}
	defer resp.Body.Close()
	if directCompact {
		const maxResponseBytes = 32 << 20
		raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
		if err != nil {
			return cliproxyexecutor.Response{}, err
		}
		if len(raw) > maxResponseBytes || !ojson.ValidBytes(raw) {
			return cliproxyexecutor.Response{}, statusErr{code: 502, msg: "invalid or oversized Codex response"}
		}
		reporter.Publish(ctx, helps.ParseOpenAIUsage(raw))
		return translateOrdinaryCodexResponse(ctx, req, opts, requestBody, raw, resp.Header, false)
	}
	items := make(map[int64][]byte)
	var fallback [][]byte
	sawOutput := false
	scanner := helps.NewCodexSSEScanner(ctx, resp.Body, 16<<20)
	for scanner.Scan() {
		b, ok := bytes.CutPrefix(scanner.Bytes(), []byte("data:"))
		if !ok {
			continue
		}
		b = bytes.TrimSpace(b)
		reporter.ObserveContentEvent(b)
		if terminal, _, failed := codexTerminalFailureErrWithCooling(b, e.modelLevelCooling()); failed {
			e.observeOrdinaryPolicyRefusal(ctx, auth, req.Model, b)
			return cliproxyexecutor.Response{}, terminal
		}
		reporter.ObserveResponseModel(b)
		event := helps.CodexResponseEventType(b)
		sawOutput = sawOutput || helps.HasMeaningfulCodexOutputDelta(b)
		if event == "response.output_item.done" {
			collectCodexOutputItemDone(b, items, &fallback)
		}
		incomplete := event == "response.incomplete"
		if event != "response.completed" && event != "response.done" && !incomplete {
			continue
		}
		if helps.IsCodexTerminalEmptyIncomplete(b, len(items)+len(fallback), sawOutput) {
			return cliproxyexecutor.Response{}, newCodexEmptyIncompleteStreamError()
		}
		final := patchCodexCompletedOutput(b, items, fallback)
		if usage, ok := helps.ParseCodexUsageWithOutputItems(final, items, fallback); ok {
			reporter.Publish(ctx, usage)
		} else {
			reporter.EnsurePublished(ctx)
		}
		publishCodexImageToolUsageWithOutputItems(ctx, reporter, requestBody, final, items, fallback)
		out, err := translateOrdinaryCodexResponse(ctx, req, opts, requestBody, final, resp.Header, incomplete)
		if err != nil {
			return cliproxyexecutor.Response{}, err
		}
		if summary := codexCompactionSummaryStateFromContext(ctx); summary != nil {
			if incomplete {
				return cliproxyexecutor.Response{}, newCodexEmptyIncompleteStreamError()
			}
			out.Payload, err = helps.BuildResponsesCompactionResponseFromSummary(summary.baseModel, out.Payload)
			if err != nil {
				return cliproxyexecutor.Response{}, statusErr{code: 502, msg: err.Error()}
			}
		}
		return out, nil
	}
	if err := ctx.Err(); err != nil {
		return cliproxyexecutor.Response{}, err
	}
	if err := scanner.Err(); err != nil {
		return cliproxyexecutor.Response{}, err
	}
	return cliproxyexecutor.Response{}, newCodexIncompleteStreamError()
}

func translateOrdinaryCodexResponse(ctx context.Context, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, body, raw []byte, headers http.Header, incomplete bool) (cliproxyexecutor.Response, error) {
	var param any
	target := cliproxyexecutor.ResponseFormatOrSource(opts)
	out := sdktranslator.TranslateNonStream(ctx, sdktranslator.FromString("codex"), target, req.Model, opts.OriginalRequest, body, raw, &param)
	if len(out) == 0 || helps.ApplyPatchTranslationError(param) != nil {
		return cliproxyexecutor.Response{}, statusErr{code: 502, msg: "invalid Codex translated response"}
	}
	if target == sdktranslator.FormatOpenAIResponse {
		out = helps.EnsureResponsesUsageDetails(out)
	}
	return cliproxyexecutor.Response{Payload: out, Headers: headers.Clone(), Incomplete: incomplete}, nil
}

func (e *CodexExecutor) executeCompact(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	opts.Alt = "responses/compact"
	return e.Execute(ctx, auth, req, opts)
}

type codexCompactionSummaryContextKey struct{}

type codexCompactionSummaryState struct {
	baseModel string
}

func withCodexCompactionSummary(ctx context.Context, baseModel string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, codexCompactionSummaryContextKey{}, &codexCompactionSummaryState{baseModel: baseModel})
}

func codexCompactionSummaryStateFromContext(ctx context.Context) *codexCompactionSummaryState {
	if ctx == nil {
		return nil
	}
	state, _ := ctx.Value(codexCompactionSummaryContextKey{}).(*codexCompactionSummaryState)
	return state
}

func codexCompactionSummaryFromContext(ctx context.Context) bool {
	return codexCompactionSummaryStateFromContext(ctx) != nil
}

func rejectSyntheticCompactionPreviousResponseID(ctx context.Context, payload []byte) error {
	prevID := strings.TrimSpace(ojson.GetBytesNoCopy(payload, "previous_response_id").String())
	if !helps.RecognizedResponsesCompactionResponseID(prevID) {
		return nil
	}
	if cliproxyexecutor.DownstreamWebsocket(ctx) {
		return &cliproxyexecutor.UpstreamWebsocketTurnError{
			FullContextRequired: true,
			EventID:             strings.Clone(strings.TrimSpace(ojson.GetBytesNoCopy(payload, "event_id").String())),
		}
	}
	return statusErr{
		code: http.StatusBadRequest,
		msg:  "previous_response_id references a local compaction response; resend full conversation input without previous_response_id",
	}
}

func validateCodexLocalCompactionSource(ctx context.Context, payload []byte) error {
	prevID := strings.TrimSpace(ojson.GetBytesNoCopy(payload, "previous_response_id").String())
	reqType := strings.TrimSpace(ojson.GetBytesNoCopy(payload, "type").String())
	if prevID != "" || reqType == "response.append" {
		if cliproxyexecutor.DownstreamWebsocket(ctx) {
			return &cliproxyexecutor.UpstreamWebsocketTurnError{
				FullContextRequired: true,
				EventID:             strings.Clone(strings.TrimSpace(ojson.GetBytesNoCopy(payload, "event_id").String())),
			}
		}
		return statusErr{
			code: http.StatusBadRequest,
			msg:  "local summary compaction requires full conversation input without previous_response_id",
		}
	}
	if !helps.HasResponsesCompactionConversationInput(payload) {
		return statusErr{
			code: http.StatusBadRequest,
			msg:  "local summary compaction requires non-empty conversation input",
		}
	}
	return nil
}

func (e *CodexExecutor) expandCodexResponsesCompaction(auth *cliproxyauth.Auth, req *cliproxyexecutor.Request, opts *cliproxyexecutor.Options, baseModel string) (bool, error) {
	if req == nil || opts == nil {
		return false, nil
	}
	isCompat := e.resolveCodexModelIsCompat(auth, *req, baseModel)
	if isCompat && opts.SourceFormat == sdktranslator.FormatOpenAIResponse && opts.Alt != "responses/compact" {
		return true, helps.ExpandResponsesCompactionCapsules(req, opts, e.Identifier())
	}
	return isCompat, helps.ExpandRecognizedResponsesCompactionCapsules(req, opts)
}

func (e *CodexExecutor) shouldCompactCompatResponsesTrigger(isCompat bool, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) bool {
	return isCompat && opts.SourceFormat == sdktranslator.FormatOpenAIResponse && opts.Alt != "responses/compact" && helps.HasResponsesCompactionTriggerRequest(req, opts)
}

func (e *CodexExecutor) executeCodexResponsesCompaction(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	var resp cliproxyexecutor.Response
	sourcePayload := helps.ResponsesCompactionSourcePayload(req, opts)
	if errValidate := validateCodexLocalCompactionSource(ctx, sourcePayload); errValidate != nil {
		return resp, errValidate
	}
	baseModel := thinking.ParseSuffix(req.Model).ModelName
	summaryPayload := helps.PrepareAntigravityCompactionSummaryPayload(sourcePayload, baseModel)
	summaryReq := req
	summaryReq.Payload = summaryPayload
	summaryOpts := opts
	summaryOpts.Alt = ""
	summaryOpts.Stream = false
	summaryOpts.OriginalRequest = nil
	summaryOpts.SourceFormat = sdktranslator.FormatOpenAIResponse
	summaryOpts.ResponseFormat = sdktranslator.FormatOpenAIResponse

	return e.Execute(withCodexCompactionSummary(ctx, baseModel), auth, summaryReq, summaryOpts)
}

func (e *CodexExecutor) executeCodexResponsesCompactionStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	cliproxyexecutor.MarkLocalWebsocketTurn(ctx)
	baseModel := thinking.ParseSuffix(req.Model).ModelName
	summaryResp, errSummary := e.executeCodexResponsesCompaction(ctx, auth, req, opts)
	if errSummary != nil {
		return nil, errSummary
	}
	chunks, errChunks := helps.BuildResponsesCompactionStreamChunksFromResponse(baseModel, summaryResp.Payload)
	if errChunks != nil {
		return nil, statusErr{code: http.StatusBadGateway, msg: errChunks.Error()}
	}
	out := make(chan cliproxyexecutor.StreamChunk, len(chunks))
	for _, chunk := range chunks {
		out <- cliproxyexecutor.StreamChunk{Payload: chunk}
	}
	close(out)

	headers := summaryResp.Headers.Clone()
	if headers == nil {
		headers = make(http.Header)
	}
	headers.Set("Content-Type", "text/event-stream")
	return &cliproxyexecutor.StreamResult{Headers: headers, Chunks: out}, nil
}
