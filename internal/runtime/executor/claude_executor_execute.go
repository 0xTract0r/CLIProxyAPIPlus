package executor

import (
	"bytes"
	"context"
	"io"
	"mime"
	"net/http"

	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/clienterror"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/thinking"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

func (e *ClaudeExecutor) Execute(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	ctx = pinOrdinaryClaudeToolSearch(ctx, opts.SourceFormat)
	if err := expandClaudeResponsesCompaction(&req, &opts); err != nil {
		return cliproxyexecutor.Response{}, statusErr{code: http.StatusBadRequest, msg: err.Error(), requestScoped: true}
	}
	if claudeResponsesCompactionRequested(req, opts) {
		return e.executeClaudeCompaction(ctx, auth, req, opts)
	}
	return e.executeOrdinaryClaudeMessages(ctx, auth, req, opts)
}

func (e *ClaudeExecutor) executeOrdinaryClaudeMessages(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (_ cliproxyexecutor.Response, err error) {
	baseModel := thinking.ParseSuffix(req.Model).ModelName
	upstreamModel := e.upstreamModel(baseModel)
	claude := sdktranslator.FormatClaude
	responseFormat := cliproxyexecutor.ResponseFormatOrSource(opts)
	upstreamStream := opts.SourceFormat != "" && responseFormat != claude && !claudeCompactionSummaryFromContext(ctx)
	body, err := e.prepareOrdinaryClaudeMessages(ctx, auth, req, opts, upstreamStream, true, baseModel, upstreamModel)
	if err != nil {
		return cliproxyexecutor.Response{}, err
	}
	if claudeCompactionSummaryFromContext(ctx) {
		body = finalizeClaudeCompactionSummaryBody(body)
	}
	reporter := helps.NewExecutorUsageReporter(ctx, e, baseModel, auth)
	defer reporter.TrackFailure(ctx, &err)
	if upstreamStream {
		reporter.UseDecodedContentTelemetry()
	}
	if upstreamModel != baseModel {
		reporter.SetUpstreamModel(upstreamModel)
	}
	resp, err := e.sendOrdinaryClaudeRequest(ctx, auth, "/v1/messages", body, upstreamStream, opts.Headers, reporter)
	if err != nil {
		return cliproxyexecutor.Response{}, err
	}
	helps.RecordAPIResponseMetadata(ctx, e.cfg, resp.StatusCode, resp.Header.Clone())
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return cliproxyexecutor.Response{}, ordinaryClaudeHTTPError(resp)
	}
	mediaType, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if upstreamStream && mediaType == "text/event-stream" {
		payload, err := readOrdinaryClaudeResponse(resp, 32<<20, reporter.ContentEventObserver())
		if err != nil {
			return cliproxyexecutor.Response{}, err
		}
		var usage helps.StreamUsageBuffer
		var refusal string
		var stream bytes.Buffer
		stream.Grow(len(payload))
		for line := range bytes.SplitSeq(payload, []byte("\n")) {
			if data, ok := bytes.CutPrefix(line, []byte("data:")); ok {
				data = bytes.TrimSpace(data)
				usage.ObserveClaudeStream(line)
				reporter.ObserveResponseModel(line)
				if message, refused := clienterror.ClaudePolicyRefusalMessage(data); refused {
					refusal = message
				}
				line = e.restoreResponseModel(line, req.Model)
			}
			stream.Write(line)
			stream.WriteByte('\n')
		}
		if err = validateClaudeStreamingResponse(payload); err != nil {
			usage.PublishFailure(ctx, reporter, err)
			return cliproxyexecutor.Response{}, err
		}
		if refusal != "" {
			err := statusErr{code: http.StatusBadRequest, msg: refusal, requestScoped: true}
			usage.PublishFailure(ctx, reporter, err)
			return cliproxyexecutor.Response{}, err
		}
		var translated any
		out := sdktranslator.TranslateNonStream(ctx, claude, responseFormat, req.Model, opts.OriginalRequest, body, stream.Bytes(), &translated)
		if helps.ApplyPatchTranslationError(translated) != nil || len(out) == 0 {
			return cliproxyexecutor.Response{}, statusErr{code: http.StatusBadGateway, msg: helps.ApplyPatchUpstreamErrorMessage}
		}
		if responseFormat == sdktranslator.FormatOpenAIResponse {
			out = helps.EnsureResponsesUsageDetails(out)
		}
		usage.Publish(ctx, reporter)
		headers := resp.Header.Clone()
		headers.Set("Content-Type", "application/json")
		headers.Del("Content-Length")
		headers.Del("Content-Encoding")
		return cliproxyexecutor.Response{Payload: out, Headers: headers}, nil
	}
	payload, err := readOrdinaryClaudeJSON(resp, 32<<20)
	if err != nil {
		return cliproxyexecutor.Response{}, err
	}
	if !ojson.GetBytesNoCopy(payload, "content").IsArray() {
		return cliproxyexecutor.Response{}, statusErr{code: http.StatusBadGateway, msg: "invalid Claude Messages response"}
	}
	usage := helps.ParseClaudeUsage(payload)
	if message, refused := clienterror.ClaudePolicyRefusalMessage(payload); refused {
		rejection := statusErr{code: http.StatusBadRequest, msg: message, requestScoped: true}
		reporter.PublishFailureWithDetail(ctx, usage, rejection)
		return cliproxyexecutor.Response{}, rejection
	}
	reporter.ObserveResponseModel(payload)
	if !claudeCompactionSummaryFromContext(ctx) {
		claude := sdktranslator.FromString("claude")
		responseFormat := cliproxyexecutor.ResponseFormatOrSource(opts)
		payload = e.restoreResponseModel(payload, req.Model)
		var translated any
		payload = sdktranslator.TranslateNonStream(ctx, claude, responseFormat, req.Model, opts.OriginalRequest, body, payload, &translated)
		if helps.ApplyPatchTranslationError(translated) != nil || len(payload) == 0 {
			return cliproxyexecutor.Response{}, statusErr{code: http.StatusBadGateway, msg: helps.ApplyPatchUpstreamErrorMessage}
		}
		if responseFormat == sdktranslator.FormatOpenAIResponse {
			payload = helps.EnsureResponsesUsageDetails(payload)
		}
	}
	reporter.Publish(ctx, usage)
	return cliproxyexecutor.Response{Payload: payload, Headers: resp.Header.Clone()}, nil
}

func readOrdinaryClaudeJSON(resp *http.Response, maxBytes int64) ([]byte, error) {
	payload, err := readOrdinaryClaudeResponse(resp, maxBytes, nil)
	if err != nil {
		return nil, err
	}
	if !ojson.ValidBytes(payload) || !util.ParseGJSONBytesNoCopy(payload).IsObject() {
		return nil, statusErr{code: http.StatusBadGateway, msg: "invalid Claude JSON response"}
	}
	return payload, nil
}

func readOrdinaryClaudeResponse(resp *http.Response, maxBytes int64, observer io.Writer) ([]byte, error) {
	decoded, err := decodeResponseBody(resp.Body, claudeResponseContentEncoding(resp.Header))
	if err != nil {
		_ = resp.Body.Close()
		return nil, ordinaryClaudeResponseReadError(err, "invalid Claude response encoding")
	}
	defer decoded.Close()
	var reader io.Reader = io.LimitReader(decoded, maxBytes+1)
	if observer != nil {
		reader = io.TeeReader(reader, observer)
	}
	payload, err := io.ReadAll(reader)
	if err != nil {
		return nil, ordinaryClaudeResponseReadError(err, "could not read Claude response")
	}
	if int64(len(payload)) > maxBytes {
		return nil, statusErr{code: http.StatusBadGateway, msg: "oversized Claude response"}
	}
	return payload, nil
}
