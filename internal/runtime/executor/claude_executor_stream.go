package executor

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	internalsignature "github.com/router-for-me/CLIProxyAPI/v8/internal/signature"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/thinking"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

type ordinaryClaudeStreamCancellation struct{ error }

func (c ordinaryClaudeStreamCancellation) Unwrap() error       { return c.error }
func (ordinaryClaudeStreamCancellation) IsRequestScoped() bool { return true }

func scopeOrdinaryClaudeStreamCancellation(err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return ordinaryClaudeStreamCancellation{err}
	}
	return err
}

// Keep cancellation in the error chain while exposing a stable HTTP error for malformed upstream bodies.
func ordinaryClaudeResponseReadError(err error, message string) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return statusErr{code: http.StatusBadGateway, msg: message}
}

func (e *ClaudeExecutor) ExecuteStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (_ *cliproxyexecutor.StreamResult, err error) {
	ctx = pinOrdinaryClaudeToolSearch(ctx, opts.SourceFormat)
	defer func() { err = scopeOrdinaryClaudeStreamCancellation(err) }()
	if err := expandClaudeResponsesCompaction(&req, &opts); err != nil {
		return nil, statusErr{code: http.StatusBadRequest, msg: err.Error()}
	}
	if claudeResponsesCompactionRequested(req, opts) {
		return e.executeClaudeCompactionStream(ctx, auth, req, opts)
	}
	baseModel := thinking.ParseSuffix(req.Model).ModelName
	upstreamModel := e.upstreamModel(baseModel)
	claude := sdktranslator.FromString("claude")
	responseFormat := cliproxyexecutor.ResponseFormatOrSource(opts)
	body, err := e.prepareOrdinaryClaudeMessages(ctx, auth, req, opts, true, true, baseModel, upstreamModel)
	if err != nil {
		return nil, err
	}

	reporter := helps.NewExecutorUsageReporter(ctx, e, baseModel, auth)
	reporter.UseDecodedContentTelemetry()
	defer reporter.TrackFailure(ctx, &err)
	if upstreamModel != baseModel {
		reporter.SetUpstreamModel(upstreamModel)
	}
	resp, err := e.sendOrdinaryClaudeRequest(ctx, auth, "/v1/messages", body, true, opts.Headers, reporter)
	if err != nil {
		return nil, err
	}

	helps.RecordAPIResponseMetadata(ctx, e.cfg, resp.StatusCode, resp.Header.Clone())
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, ordinaryClaudeHTTPError(resp)
	}
	decoded, err := decodeResponseBody(resp.Body, claudeResponseContentEncoding(resp.Header))
	if err != nil {
		_ = resp.Body.Close()
		return nil, ordinaryClaudeResponseReadError(err, "invalid Claude response encoding")
	}

	out := make(chan cliproxyexecutor.StreamChunk, 1)
	go func() {
		defer close(out)
		defer decoded.Close()
		var usage helps.StreamUsageBuffer
		completed := false
		defer func() {
			if cause := ctx.Err(); cause != nil && !completed {
				err := scopeOrdinaryClaudeStreamCancellation(cause)
				usage.PublishFailure(ctx, reporter, err)
				// A downstream that has left must not block termination. If
				// the slot is full, replace its unsent payload with the error.
				select {
				case out <- cliproxyexecutor.StreamChunk{Err: err}:
				default:
					select {
					case <-out:
					default:
					}
					select {
					case out <- cliproxyexecutor.StreamChunk{Err: err}:
					default:
					}
				}
				return
			}
			usage.Publish(ctx, reporter)
		}()
		send := func(chunk cliproxyexecutor.StreamChunk) bool {
			select {
			case <-ctx.Done():
				return false
			case out <- chunk:
				return true
			}
		}
		fail := func(err error) {
			if ctx.Err() != nil {
				return
			}
			err = scopeOrdinaryClaudeStreamCancellation(err)
			usage.PublishFailure(ctx, reporter, err)
			send(cliproxyexecutor.StreamChunk{Err: err})
		}
		scanner := bufio.NewScanner(decoded)
		scanner.Buffer(nil, 52_428_800)
		var event bytes.Buffer
		var translation any
		done := false
		for scanner.Scan() {
			line := bytes.Clone(scanner.Bytes())
			data, isData := bytes.CutPrefix(line, []byte("data:"))
			if isData {
				payload := bytes.TrimSpace(data)
				if !ojson.ValidBytes(payload) {
					fail(statusErr{code: http.StatusBadGateway, msg: "upstream returned malformed stream data"})
					return
				}
				typ := ojson.GetBytesNoCopy(payload, "type").String()
				if typ == "error" {
					fail(ordinaryClaudeSSEError(payload, resp.Header))
					return
				}
				done = done || typ == "message_stop"
				usage.ObserveClaudeStream(line)
				reporter.ObserveContentEvent(payload)
				helps.ObserveClaudeTokenEvent(reporter, payload)
				reporter.ObserveResponseModel(line)
				line = e.restoreResponseModel(line, req.Model)
			}
			if responseFormat == claude {
				event.Write(line)
				event.WriteByte('\n')
				if len(line) == 0 {
					if !send(cliproxyexecutor.StreamChunk{Payload: bytes.Clone(event.Bytes())}) {
						return
					}
					event.Reset()
					if done {
						completed = true
						return
					}
				}
			} else {
				for _, chunk := range sdktranslator.TranslateStream(ctx, claude, responseFormat, req.Model, opts.OriginalRequest, body, line, &translation) {
					if !send(cliproxyexecutor.StreamChunk{Payload: chunk}) {
						return
					}
				}
				if done {
					completed = true
					return
				}
			}
		}
		if ctx.Err() != nil {
			return
		}
		if scanErr := scanner.Err(); scanErr != nil {
			fail(scanErr)
			return
		}
		if !done {
			fail(statusErr{code: http.StatusBadGateway, msg: "upstream stream closed before message_stop"})
			return
		}
		if event.Len() != 0 {
			event.WriteByte('\n')
			if !send(cliproxyexecutor.StreamChunk{Payload: event.Bytes()}) {
				return
			}
		}
		completed = true
	}()
	return &cliproxyexecutor.StreamResult{Chunks: out, Headers: resp.Header.Clone()}, nil
}

func (e *ClaudeExecutor) prepareOrdinaryClaudeMessages(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, stream, insertCache bool, baseModel, upstreamModel string) ([]byte, error) {
	if err := e.rejectUnsupportedClaudeRequest(auth); err != nil {
		return nil, err
	}
	if len(req.Payload) > ordinaryClaudeMaxRequestBytes || len(opts.OriginalRequest) > ordinaryClaudeMaxRequestBytes {
		return nil, statusErr{code: http.StatusRequestEntityTooLarge, msg: "Claude request exceeds size limit", requestScoped: true}
	}

	if err := rejectOrdinaryClaudeDuplicateMetadata(req.Payload); err != nil {
		return nil, err
	}

	from := opts.SourceFormat
	claude := sdktranslator.FromString("claude")
	ctx, req, opts = e.applyNativeToolSearchFallback(ctx, auth, from, upstreamModel, req, opts)
	original := opts.OriginalRequest
	if len(original) == 0 {
		original = req.Payload
	}
	originalTranslated, body, err := helps.TranslateRequestPairReturningError(ctx, opts.Headers, e.cfg, from, claude, baseModel, original, req.Payload, stream, helps.APIKeyModelIsCompat(req))
	if err != nil {
		return nil, err
	}
	if !ojson.ValidBytes(body) || !ojson.GetBytesNoCopy(body, "messages").IsArray() {
		return nil, statusErr{code: http.StatusBadRequest, msg: "invalid Messages payload", requestScoped: true}
	}
	doc := ojson.NewDoc(body)
	helps.SetStringIfDifferentDoc(doc, "model", upstreamModel)
	helps.SetBoolIfDifferentDoc(doc, "stream", stream)
	if err = helps.ApplyRequestThinkingDoc(doc, req, opts, from.String(), claude.String(), e.Identifier()); err != nil {
		return nil, err
	}
	ensureModelMaxTokensDoc(doc, baseModel)
	sanitizeClaudeAdvisorToolModelsDoc(doc)
	disableThinkingIfToolChoiceForcedDoc(doc)
	helps.ApplyPayloadConfigWithTrackedPathsDoc(
		e.cfg, baseModel, claude.String(), from.String(), "", doc,
		stripTranslatedDefaultMaxTokens(from, original, originalTranslated),
		helps.PayloadRequestedModel(opts, req.Model), helps.PayloadRequestPath(opts), opts.Headers,
	)
	system := doc.Get("system")
	if system.Exists() && system.Type != ojson.String {
		if !system.IsArray() {
			return nil, statusErr{code: http.StatusBadRequest, msg: "Claude system must contain only text", requestScoped: true}
		}
		for _, block := range system.Array() {
			if block.Get("type").String() != "text" || block.Get("text").Type != ojson.String {
				return nil, statusErr{code: http.StatusBadRequest, msg: "Claude system must contain only text", requestScoped: true}
			}
		}
	}
	if err := rejectForeignOrdinaryClaudeThinking(doc); err != nil {
		return nil, err
	}
	normalizeOrdinaryClaudeSampling(doc)
	explicitCache := strings.EqualFold(doc.Get("prompt_cache_options.mode").String(), "explicit") ||
		isExplicitPromptCacheMode(original, req.Payload)
	if insertCache && !explicitCache && shouldEnsureCacheControlDoc(doc, false, false) {
		ensureCacheControlDoc(doc)
	}
	normalizeOrdinaryClaudeCache(doc, explicitCache)
	_ = doc.Delete("prompt_cache_options")
	_, base, _ := ordinaryClaudeCredential(auth)
	if err := validateOrdinaryClaudeModel(doc, base); err != nil {
		return nil, err
	}
	// Keep the native transport response mode consistent with the caller.
	helps.SetBoolIfDifferentDoc(doc, "stream", stream)
	return doc.Bytes(), nil

}

// Normalize proxy-only model suffixes on provider-native advisor declarations.
// Operator payload rules still run last, and ordinary tool fields stay untouched.
func sanitizeClaudeAdvisorToolModelsDoc(doc *ojson.Doc) bool {
	if doc == nil {
		return false
	}
	changed := false
	for i, tool := range doc.Get("tools").Array() {
		if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(tool.Get("type").String())), "advisor_") {
			continue
		}
		model := tool.Get("model")
		if model.Type != ojson.String {
			continue
		}
		original := model.String()
		trimmed := strings.TrimSpace(original)
		if trimmed == "" {
			continue
		}
		base := strings.TrimSpace(thinking.ParseSuffix(trimmed).ModelName)
		if base == "" || base == original {
			continue
		}
		if doc.Set("tools."+strconv.Itoa(i)+".model", base) == nil {
			changed = true
		}
	}
	return changed
}

func ordinaryClaudeHTTPError(resp *http.Response) error {
	decoded, err := decodeResponseBody(resp.Body, claudeResponseContentEncoding(resp.Header))
	if err != nil {
		_ = resp.Body.Close()
		return statusErr{code: resp.StatusCode, msg: "failed to decode error response body", retryAfter: ordinaryClaudeRetryAfter(resp.Header, time.Now())}
	}
	defer decoded.Close()
	body, _ := io.ReadAll(io.LimitReader(decoded, 64<<10))
	msg := strings.TrimSpace(string(body))
	if msg == "" {
		msg = http.StatusText(resp.StatusCode)
	}
	return statusErr{code: resp.StatusCode, msg: msg, retryAfter: ordinaryClaudeRetryAfter(resp.Header, time.Now())}
}

func ordinaryClaudeSSEError(payload []byte, headers http.Header) error {
	code := int(ojson.GetBytesNoCopy(payload, "status").Int())
	if code < 400 || code > 599 {
		switch ojson.GetBytesNoCopy(payload, "error.type").String() {
		case "overloaded_error":
			code = 529
		case "rate_limit_error":
			code = http.StatusTooManyRequests
		default:
			code = http.StatusBadGateway
		}
	}
	return statusErr{code: code, msg: fmt.Sprintf("%s", payload), retryAfter: ordinaryClaudeRetryAfter(headers, time.Now())}
}

func validateClaudeStreamingResponse(data []byte) error {
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(nil, 52_428_800)

	hasData := false
	hasMessageStart := false
	hasMessageDelta := false
	hasMessageStop := false

	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 || !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		payload := bytes.TrimSpace(line[len("data:"):])
		if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
			continue
		}
		hasData = true
		if !ojson.ValidBytes(payload) {
			return statusErr{code: http.StatusBadGateway, msg: "claude executor: upstream returned malformed stream data"}
		}

		root := util.ParseGJSONBytesNoCopy(payload)
		switch root.Get("type").String() {
		case "error":
			message := strings.TrimSpace(root.Get("error.message").String())
			if message == "" {
				message = strings.TrimSpace(root.Get("error.type").String())
			}
			if message == "" {
				message = "unknown upstream error"
			}
			return statusErr{code: http.StatusBadGateway, msg: "claude executor: upstream returned error event: " + message}
		case "message_start":
			message := root.Get("message")
			if strings.TrimSpace(message.Get("id").String()) == "" || strings.TrimSpace(message.Get("model").String()) == "" {
				return statusErr{code: http.StatusBadGateway, msg: "claude executor: upstream stream message_start is missing id or model"}
			}
			hasMessageStart = true
		case "message_delta":
			hasMessageDelta = true
		case "message_stop":
			hasMessageStop = hasMessageStart && hasMessageDelta
		}
	}
	if errScan := scanner.Err(); errScan != nil {
		return errScan
	}
	if !hasData {
		return statusErr{code: http.StatusBadGateway, msg: "claude executor: upstream returned empty stream response"}
	}
	if !hasMessageStart {
		return statusErr{code: http.StatusBadGateway, msg: "claude executor: upstream stream response is missing message_start"}
	}
	if !hasMessageDelta || !hasMessageStop {
		return statusErr{code: http.StatusBadGateway, msg: "claude executor: upstream stream response ended before message completion"}
	}
	return nil
}

// Use the public non-native Messages sampling rules after applying payload overrides.
func normalizeOrdinaryClaudeSampling(doc *ojson.Doc) {
	_ = doc.Delete("temperature")
	_ = doc.Delete("top_p")
	switch strings.ToLower(strings.TrimSpace(doc.Get("thinking.type").String())) {
	case "enabled", "adaptive", "auto":
		_ = doc.Delete("top_k")
	}
}

// Reject known cross-provider state without rewriting unknown native opaque values.
func rejectForeignOrdinaryClaudeThinking(doc *ojson.Doc) error {
	for _, message := range doc.Get("messages").Array() {
		for _, part := range message.Get("content").Array() {
			switch part.Get("type").String() {
			case "thinking":
				if signature := part.Get("signature"); signature.Exists() {
					if !ordinaryClaudeSignatureShape(signature) || foreignOrdinaryClaudeSignature(signature) || malformedOrdinaryClaudeEnvelope(signature.String()) {
						return statusErr{code: http.StatusBadRequest, msg: "thinking signature is incompatible with Claude Messages", requestScoped: true}
					}
				}
			case "tool_use":
				for _, path := range []string{"signature", "thought_signature", "extra_content.google.thought_signature"} {
					if foreignOrdinaryClaudeSignature(part.Get(path)) {
						return statusErr{code: http.StatusBadRequest, msg: "tool state is incompatible with Claude Messages", requestScoped: true}
					}
				}
			}
		}
	}
	return nil
}

func foreignOrdinaryClaudeSignature(value ojson.Result) bool {
	if !value.Exists() || value.Type != ojson.String {
		return false
	}
	provider := internalsignature.DetectSignatureProviderForBlock(value.String(), internalsignature.SignatureBlockKindClaudeThinking)
	return provider != internalsignature.SignatureProviderUnknown && provider != internalsignature.SignatureProviderClaude
}

// Check only a nonempty base64-shaped value; the service authenticates it.
func ordinaryClaudeSignatureShape(value ojson.Result) bool {
	if value.Type != ojson.String {
		return false
	}
	raw := value.String()
	core := strings.TrimRight(raw, "=")
	padding := len(raw) - len(core)
	if core == "" || padding > 2 || len(core)%4 == 1 || (padding != 0 && len(raw)%4 != 0) {
		return false
	}
	for i := 0; i < len(core); i++ {
		c := core[i]
		if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '+' || c == '/' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

// A recognized Claude envelope must parse, but unrecognized opaque formats
// remain untouched for the service to authenticate.
func malformedOrdinaryClaudeEnvelope(raw string) bool {
	return internalsignature.HasClaudeThinkingSignaturePrefix(raw) &&
		!internalsignature.IsValidClaudeThinkingSignature(raw, internalsignature.ClaudeSignatureValidationOptions{Strict: true}) &&
		!internalsignature.IsValidClaudeCAISSignature(raw)
}
