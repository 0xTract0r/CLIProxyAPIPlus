package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"maps"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/clienterror"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/thinking"
	antigravitythinking "github.com/router-for-me/CLIProxyAPI/v8/internal/thinking/provider/antigravity"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

const ordinaryAntigravityBodyLimit = 32 << 20

func (e *AntigravityExecutor) validateOrdinaryAntigravity(auth *cliproxyauth.Auth) (*url.URL, string, error) {
	if e.cfg != nil {
		settings := e.cfg.Antigravity
		if len(settings.SensitiveWords)+len(settings.SensitiveWordsReplace)+len(settings.SensitivePatternsReplace) > 0 {
			return nil, "", statusErr{code: 400, msg: "Antigravity content rewriting mode is unsupported", requestScoped: true}
		}
	}
	if auth == nil {
		return nil, "", statusErr{code: 401, msg: "missing Antigravity credential", requestScoped: true}
	}
	if auth.Disabled {
		return nil, "", statusErr{code: 403, msg: "Antigravity credential is disabled", requestScoped: true}
	}
	base := resolveCustomAntigravityBaseURL(auth)
	origin, err := url.Parse(base)
	if err != nil || origin.Scheme != "https" || origin.Hostname() == "" || origin.User != nil || origin.RawQuery != "" || origin.Fragment != "" {
		return nil, "", statusErr{code: 403, msg: "Antigravity requires an explicit authorized HTTPS origin", requestScoped: true}
	}
	token, _ := auth.Metadata["access_token"].(string)
	token = strings.TrimSpace(token)
	if token == "" {
		return nil, "", statusErr{code: 401, msg: "Antigravity requires an existing access token", requestScoped: true}
	}
	return origin, token, nil
}

func (e *AntigravityExecutor) PrepareRequest(req *http.Request, auth *cliproxyauth.Auth) error {
	if req == nil || req.URL == nil {
		return statusErr{code: 400, msg: "invalid Antigravity request", requestScoped: true}
	}
	origin, token, err := e.validateOrdinaryAntigravity(auth)
	if err != nil {
		return err
	}
	if req.URL.User != nil || req.Host != "" && !strings.EqualFold(req.Host, req.URL.Host) || !antigravitySameRedirectOrigin(origin, req.URL) {
		return statusErr{code: 403, msg: "Antigravity credential origin mismatch", requestScoped: true}
	}
	headers := make(http.Header)
	for _, name := range []string{"Content-Type", "Accept"} {
		if value := req.Header.Get(name); value != "" {
			headers.Set(name, value)
		}
	}
	headers.Set("Authorization", "Bearer "+token)
	req.Header = headers
	return nil
}

func (e *AntigravityExecutor) HttpRequest(ctx context.Context, auth *cliproxyauth.Auth, req *http.Request) (*http.Response, error) {
	if req == nil {
		return nil, statusErr{code: 400, msg: "invalid Antigravity request", requestScoped: true}
	}
	if ctx == nil {
		ctx = req.Context()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	request := req.Clone(ctx)
	if err := e.PrepareRequest(request, auth); err != nil {
		return nil, err
	}
	client := *helps.NewProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return client.Do(request)
}

func (e *AntigravityExecutor) ordinaryAntigravitySend(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, path string) (*http.Response, []byte, error) {
	origin, _, err := e.validateOrdinaryAntigravity(auth)
	if err != nil {
		return nil, nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	if len(req.Payload) > ordinaryAntigravityBodyLimit || len(opts.OriginalRequest) > ordinaryAntigravityBodyLimit {
		return nil, nil, statusErr{code: 413, msg: "Antigravity request exceeds size limit", requestScoped: true}
	}
	if opts.Alt == "responses/compact" ||
		(opts.SourceFormat == sdktranslator.FormatOpenAIResponse &&
			(ordinaryAntigravityHasCompactionItem(req.Payload) || ordinaryAntigravityHasCompactionItem(opts.OriginalRequest))) {
		return nil, nil, statusErr{code: http.StatusBadRequest, msg: "Antigravity has no supported upstream compaction protocol", requestScoped: true}
	}
	if opts.Alt != "" && opts.Alt != "sse" {
		return nil, nil, statusErr{code: 400, msg: "unsupported Antigravity request extension", requestScoped: true}
	}
	model := thinking.ParseSuffix(req.Model).ModelName
	to := sdktranslator.FromString("antigravity")
	translatedOriginal, payload, err := helps.TranslateRequestPairReturningError(ctx, opts.Headers, e.cfg, opts.SourceFormat, to, model, opts.OriginalRequest, req.Payload, path == antigravityStreamPath, false)
	if err != nil {
		return nil, nil, err
	}
	if !ojson.ValidBytes(payload) {
		return nil, nil, statusErr{code: 400, msg: "invalid Antigravity request JSON", requestScoped: true}
	}
	doc := ojson.NewDoc(payload)
	if !doc.Get("request").IsObject() {
		doc = ojson.NewDocString(`{}`)
		_ = doc.SetRawBytes("request", payload)
	}
	if helps.PrepareAntigravityClaudeStructuredOutputDoc(model, doc) && ordinaryAntigravityExplicitToolChoice(opts.OriginalRequest, req.Payload) {
		return nil, nil, statusErr{code: http.StatusBadRequest, msg: "this Antigravity model does not support combining structured output with a required tool choice", requestScoped: true}
	}
	if err := normalizeOrdinaryAntigravityLevel(doc); err != nil {
		return nil, nil, err
	}
	if path != antigravityCountTokensPath {
		constrainOrdinaryAntigravityClaudeOutput(doc, model)
	}
	// Prepare the protocol payload before applying the operator's final rules.
	ensureAntigravityGeminiBoundaryUserContentDoc(model, doc)
	normalizeAntigravityGeminiFunctionResponseRolesDoc(doc)
	doc = ojson.NewDoc(repairAntigravityGeminiFunctionResponseNames(doc.Bytes()))
	setup := helps.NewPayloadFinalizer(e.cfg, e.Identifier(), model, to.String(), "request", translatedOriginal, req, opts)
	helps.FinalizePayloadDoc(helps.WithPayloadFinalizer(ctx, setup), doc)
	// Keep route and credential metadata authoritative after ordinary body rules.
	if path != antigravityCountTokensPath {
		project := antigravityProjectIDFromAuth(auth)
		if project == "" {
			return nil, nil, statusErr{code: 400, msg: "Antigravity requires an explicitly assigned project", requestScoped: true}
		}
		_ = doc.Set("project", project)
		_ = doc.Set("model", model)
	} else {
		for _, key := range []string{"project", "model", "request.safetySettings", "request.toolConfig", "request.labels", "request.sessionId", "request.stream"} {
			_ = doc.Delete(key)
		}
		if model != "" {
			_ = doc.Set("request.model", "models/"+strings.TrimPrefix(model, "models/"))
		}
	}
	// Provider-supplied thought signatures are opaque. Do not repair, generate,
	// replace, or replay them from another request.
	payload = doc.Bytes()
	if len(payload) > ordinaryAntigravityBodyLimit {
		return nil, nil, statusErr{code: 413, msg: "Antigravity request exceeds size limit", requestScoped: true}
	}
	if signed := ordinaryAntigravitySignatureValues(req.Payload); !maps.Equal(signed, ordinaryAntigravitySignatureValues(payload)) {
		return nil, nil, statusErr{code: 400, msg: "Antigravity request changed an opaque service signature", requestScoped: true}
	}
	origin.Path = strings.TrimRight(origin.Path, "/") + path
	origin.RawPath = ""
	if path == antigravityStreamPath {
		origin.RawQuery = "alt=sse"
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, origin.String(), bytes.NewReader(payload))
	if err != nil {
		return nil, nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")
	if path == antigravityStreamPath {
		httpReq.Header.Set("Accept", "text/event-stream")
	}
	resp, err := e.HttpRequest(ctx, auth, httpReq)
	if err != nil {
		return nil, nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer resp.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		msg := strings.TrimSpace(string(b))
		if msg == "" {
			msg = http.StatusText(resp.StatusCode)
		}
		return nil, nil, ordinaryAntigravityProviderRefusal(resp.StatusCode, msg, resp.Header, b)
	}
	decoded, err := helps.DecodeResponseBody(resp.Body, resp.Header)
	if err != nil {
		_ = resp.Body.Close()
		return nil, nil, err
	}
	resp.Body = decoded
	return resp, payload, nil
}

func ordinaryAntigravityHasCompactionItem(body []byte) bool {
	for _, item := range ojson.GetBytesNoCopy(body, "input").Array() {
		switch item.Get("type").String() {
		case "compaction", "compaction_trigger":
			return true
		}
	}
	return false
}

func constrainOrdinaryAntigravityClaudeOutput(doc *ojson.Doc, model string) {
	model = strings.ToLower(strings.TrimSpace(model))
	if !strings.HasPrefix(model, "claude-") {
		return
	}
	limit := registry.DefaultClaudeMaxOutputTokens
	if info := registry.LookupModelInfo(model, antigravityAuthType); info != nil && info.MaxCompletionTokens > 0 {
		limit = min(limit, info.MaxCompletionTokens)
	}
	const path = "request.generationConfig.maxOutputTokens"
	if maximum := doc.Get(path); maximum.Type == ojson.Number && maximum.Int() > int64(limit) {
		_ = doc.Set(path, limit)
	}
}

// The SDK preserves the actual caller request separately when a protocol
// adapter generated a wire tool mode. Without that evidence, the supplied
// payload is the caller request.
func ordinaryAntigravityExplicitToolChoice(original, payload []byte) bool {
	if !ojson.ValidBytes(original) || !ojson.GetBytesNoCopy(original, "@this").IsObject() {
		original = payload
	}
	for _, prefix := range []string{"", "request."} {
		for _, field := range []string{"tool_choice", "toolChoice"} {
			choice := ojson.GetBytesNoCopy(original, prefix+field)
			if choice.IsObject() {
				switch strings.ToLower(strings.TrimSpace(choice.Get("type").String())) {
				case "tool", "function":
					return true
				}
				if ordinaryAntigravityRequiredToolMode(choice.Get("type").String()) || ordinaryAntigravityRequiredToolMode(choice.Get("mode").String()) {
					return true
				}
			} else if ordinaryAntigravityRequiredToolMode(choice.String()) {
				return true
			}
		}
		for _, field := range []string{"toolConfig", "tool_config"} {
			for _, config := range []string{"functionCallingConfig", "function_calling_config"} {
				if ordinaryAntigravityRequiredToolMode(ojson.GetBytesNoCopy(original, prefix+field+"."+config+".mode").String()) {
					return true
				}
			}
		}
	}
	return false
}

func ordinaryAntigravityRequiredToolMode(mode string) bool {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "required", "any", "validated":
		return true
	default:
		return false
	}
}

func (e *AntigravityExecutor) Execute(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (_ cliproxyexecutor.Response, err error) {
	reporter := helps.NewExecutorUsageReporter(ctx, e, req.Model, auth)
	defer reporter.TrackFailure(ctx, &err)
	resp, body, err := e.ordinaryAntigravitySend(ctx, auth, req, opts, antigravityGeneratePath)
	if err != nil {
		return cliproxyexecutor.Response{}, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, ordinaryAntigravityBodyLimit+1))
	if err != nil {
		return cliproxyexecutor.Response{}, err
	}
	if len(raw) > ordinaryAntigravityBodyLimit {
		return cliproxyexecutor.Response{}, statusErr{code: 502, msg: "invalid Antigravity response"}
	}
	if mediaType, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type")); mediaType == "text/event-stream" {
		raw, err = collectOrdinaryAntigravityResponse(ctx, raw, resp.Header)
		if err != nil {
			return cliproxyexecutor.Response{}, err
		}
		resp.Header = resp.Header.Clone()
		resp.Header.Set("Content-Type", "application/json")
		resp.Header.Del("Content-Length")
	}
	if !ojson.ValidBytes(raw) {
		return cliproxyexecutor.Response{}, statusErr{code: 502, msg: "invalid Antigravity response"}
	}
	if err := ordinaryAntigravityInBandError(raw, resp.Header); err != nil {
		return cliproxyexecutor.Response{}, err
	}
	if message, blocked := clienterror.GeminiFilterBlockMessage(raw); blocked {
		return cliproxyexecutor.Response{}, statusErr{code: http.StatusBadRequest, msg: message, requestScoped: true}
	}
	var state any
	out := sdktranslator.TranslateNonStream(ctx, sdktranslator.FromString("antigravity"), cliproxyexecutor.ResponseFormatOrSource(opts), req.Model, opts.OriginalRequest, body, raw, &state)
	if err := helps.ApplyPatchTranslationError(state); err != nil {
		return cliproxyexecutor.Response{}, statusErr{code: http.StatusBadGateway, msg: helps.ApplyPatchUpstreamErrorMessage}
	}
	if len(out) == 0 {
		return cliproxyexecutor.Response{}, statusErr{code: 502, msg: "empty Antigravity response"}
	}
	reporter.Publish(ctx, helps.ParseAntigravityUsage(raw))
	return cliproxyexecutor.Response{Payload: out, Headers: resp.Header.Clone()}, nil
}

func (e *AntigravityExecutor) ExecuteStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (_ *cliproxyexecutor.StreamResult, err error) {
	reporter := helps.NewExecutorUsageReporter(ctx, e, req.Model, auth)
	defer reporter.TrackFailure(ctx, &err)
	resp, body, err := e.ordinaryAntigravitySend(ctx, auth, req, opts, antigravityStreamPath)
	if err != nil {
		return nil, err
	}
	out := make(chan cliproxyexecutor.StreamChunk)
	go func() {
		defer close(out)
		defer resp.Body.Close()
		send := func(c cliproxyexecutor.StreamChunk) bool {
			select {
			case <-ctx.Done():
				return false
			case out <- c:
				return true
			}
		}
		var usage helps.StreamUsageBuffer
		fail := func(err error) {
			usage.PublishFailure(ctx, reporter, err)
			send(cliproxyexecutor.StreamChunk{Err: err})
		}
		var pendingFilter string
		scanner := helps.NewCodexSSEScanner(ctx, resp.Body, 16<<20)
		var state any
		completed := false
		var malformedFrame, sawOutput bool
		// A client may close after receiving an upstream terminal event while
		// the upstream HTTP body remains open. Settle observed usage once.
		defer func() {
			if ctx.Err() != nil && completed && pendingFilter == "" {
				usage.Publish(context.WithoutCancel(ctx), reporter)
			}
		}()
		for scanner.Scan() {
			line := bytes.Clone(scanner.Bytes())
			if data, ok := bytes.CutPrefix(line, []byte("data:")); ok {
				data = bytes.TrimSpace(data)
				if bytes.Equal(data, []byte("[DONE]")) {
					if !completed && (malformedFrame || !sawOutput) {
						break
					}
					completed = true
					if pendingFilter == "" {
						chunks := sdktranslator.TranslateStream(ctx, sdktranslator.FromString("antigravity"), cliproxyexecutor.ResponseFormatOrSource(opts), req.Model, opts.OriginalRequest, body, data, &state)
						if helps.ApplyPatchTranslationError(state) != nil {
							fail(statusErr{code: http.StatusBadGateway, msg: helps.ApplyPatchUpstreamErrorMessage})
							return
						}
						for _, chunk := range chunks {
							if len(chunk) > 0 && !send(cliproxyexecutor.StreamChunk{Payload: chunk}) {
								return
							}
						}
					}
					break
				}
				if !ojson.ValidBytes(data) {
					malformedFrame = true
					continue
				}
				if err := ordinaryAntigravityInBandError(data, resp.Header); err != nil {
					fail(err)
					return
				}
				completed = completed || ordinaryAntigravityHasTerminalReason(data)
				usage.Observe(helps.ParseAntigravityStreamUsage(line))
				if message, blocked := clienterror.GeminiFilterBlockMessage(data); blocked {
					pendingFilter = message
					continue
				}
				hasOutput := ordinaryAntigravityHasOutput(data)
				sawOutput = sawOutput || hasOutput
				if pendingFilter != "" {
					if !hasOutput {
						continue
					}
					pendingFilter = ""
				}
			}
			chunks := sdktranslator.TranslateStream(ctx, sdktranslator.FromString("antigravity"), cliproxyexecutor.ResponseFormatOrSource(opts), req.Model, opts.OriginalRequest, body, line, &state)
			if err := helps.ApplyPatchTranslationError(state); err != nil {
				fail(statusErr{code: http.StatusBadGateway, msg: helps.ApplyPatchUpstreamErrorMessage})
				return
			}
			for _, chunk := range chunks {
				if len(chunk) > 0 && !send(cliproxyexecutor.StreamChunk{Payload: chunk}) {
					return
				}
			}
		}
		if ctx.Err() != nil {
			return
		}
		if err := scanner.Err(); err != nil {
			fail(err)
			return
		}
		if pendingFilter != "" {
			fail(statusErr{code: http.StatusBadRequest, msg: pendingFilter, requestScoped: true})
			return
		}
		if malformedFrame && !completed {
			fail(statusErr{code: http.StatusBadGateway, msg: "invalid Antigravity SSE frame"})
			return
		}
		if !completed {
			fail(statusErr{code: 502, msg: "Antigravity stream ended before completion"})
			return
		}
		if !usage.Publish(ctx, reporter) {
			reporter.EnsurePublished(ctx)
		}
	}()
	return &cliproxyexecutor.StreamResult{Headers: resp.Header.Clone(), Chunks: out}, nil
}

func (e *AntigravityExecutor) CountTokens(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	resp, _, err := e.ordinaryAntigravitySend(ctx, auth, req, opts, antigravityCountTokensPath)
	if err != nil {
		return cliproxyexecutor.Response{}, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, (64<<10)+1))
	if err != nil {
		return cliproxyexecutor.Response{}, err
	}
	if len(raw) > 64<<10 || !ojson.ValidBytes(raw) {
		return cliproxyexecutor.Response{}, statusErr{code: 502, msg: "invalid Antigravity token count response"}
	}
	if err := ordinaryAntigravityInBandError(raw, resp.Header); err != nil {
		return cliproxyexecutor.Response{}, err
	}
	total := ojson.GetBytesNoCopy(raw, "totalTokens")
	if total.Type != ojson.Number || total.Int() < 0 || total.Num != float64(total.Int()) {
		return cliproxyexecutor.Response{}, statusErr{code: 502, msg: "invalid Antigravity token count response"}
	}
	return cliproxyexecutor.Response{Payload: sdktranslator.TranslateTokenCount(ctx, sdktranslator.FromString("antigravity"), cliproxyexecutor.ResponseFormatOrSource(opts), total.Int(), raw), Headers: resp.Header.Clone()}, nil
}

func ordinaryAntigravitySignatureValues(raw []byte) map[string]int {
	var obj any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil
	}
	values := make(map[string]int)
	var walk func(any)
	walk = func(value any) {
		switch v := value.(type) {
		case []any:
			for _, item := range v {
				walk(item)
			}
		case map[string]any:
			for key, item := range v {
				switch key {
				case "signature", "thoughtSignature", "thought_signature":
					if s, ok := item.(string); ok && s != "" {
						values[s]++
					}
				default:
					walk(item)
				}
			}
		}
	}
	walk(obj)
	return values
}

// An ordinary request uses the already supplied credential. The SDK preflight
// must enforce the same restrictions before any provider request is attempted.
func (e *AntigravityExecutor) ShouldPrepareRequestAuth(*cliproxyauth.Auth) bool { return true }

func (e *AntigravityExecutor) PrepareRequestAuth(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	if ctx != nil && ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if _, _, err := e.validateOrdinaryAntigravity(auth); err != nil {
		return nil, err
	}
	return auth, nil
}

func (e *AntigravityExecutor) Refresh(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	if ctx != nil && ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if _, _, err := e.validateOrdinaryAntigravity(auth); err != nil {
		return nil, err
	}
	return nil, statusErr{code: http.StatusBadRequest, msg: "Antigravity refresh requires a separately configured standard OAuth flow", requestScoped: true}
}

func ordinaryAntigravityHasTerminalReason(body []byte) bool {
	for _, path := range []string{"response.candidates.0.finishReason", "candidates.0.finishReason"} {
		reason := ojson.GetBytesNoCopy(body, path)
		if reason.Type == ojson.String && strings.TrimSpace(reason.String()) != "" {
			return true
		}
	}
	return false
}

func ordinaryAntigravityInBandError(body []byte, headers http.Header) error {
	result := ojson.GetBytesNoCopy(body, "error")
	if !result.Exists() || result.Type == ojson.Null {
		return nil
	}
	code := int(result.Get("code").Int())
	if code < 400 || code > 599 {
		switch result.Get("status").String() {
		case "INVALID_ARGUMENT":
			code = http.StatusBadRequest
		case "UNAUTHENTICATED":
			code = http.StatusUnauthorized
		case "PERMISSION_DENIED":
			code = http.StatusForbidden
		case "RESOURCE_EXHAUSTED":
			code = http.StatusTooManyRequests
		case "UNAVAILABLE":
			code = http.StatusServiceUnavailable
		default:
			code = http.StatusBadGateway
		}
	}
	message := strings.TrimSpace(result.Get("message").String())
	if message == "" {
		message = http.StatusText(code)
	}
	return ordinaryAntigravityProviderRefusal(code, message, headers, body)
}

// Preserve explicit upstream credential and policy status for this request.
func ordinaryAntigravityProviderRefusal(code int, message string, headers http.Header, body []byte) statusErr {
	status := statusErr{code: code, msg: message, retryAfter: ordinaryAntigravityRetryHint(code, headers, body)}
	if code == http.StatusUnauthorized {
		status.credentialScoped = true
		return status
	}
	if code != http.StatusForbidden || ojson.GetBytesNoCopy(body, "error.status").String() != "PERMISSION_DENIED" {
		return status
	}
	for _, detail := range ojson.GetBytesNoCopy(body, "error.details").Array() {
		if detail.Get("@type").String() != "type.googleapis.com/google.rpc.ErrorInfo" {
			continue
		}
		switch detail.Get("reason").String() {
		case "VALIDATION_REQUIRED":
			status.credentialScoped = true
		case "TOS_VIOLATION":
			status.credentialScoped = true
			status.tosViolation = true
			return status
		}
	}
	return status
}

// Metadata and empty placeholders are not evidence of model output.
func ordinaryAntigravityHasOutput(body []byte) bool {
	for _, path := range []string{"response.candidates", "candidates"} {
		for _, candidate := range ojson.GetBytesNoCopy(body, path).Array() {
			for _, part := range candidate.Get("content.parts").Array() {
				for _, field := range []string{"text", "functionCall.name", "inlineData.data", "inline_data.data", "executableCode.code", "codeExecutionResult.output"} {
					if strings.TrimSpace(part.Get(field).String()) != "" {
						return true
					}
				}
			}
		}
	}
	return false
}

func ordinaryAntigravityRetryHint(code int, headers http.Header, body []byte) *time.Duration {
	now := time.Now()
	if (code == http.StatusTooManyRequests || code == http.StatusServiceUnavailable) && len(body) <= 64<<10 {
		if delay, err := helps.ParseRetryDelayWithHeaders(headers, body, now); err == nil {
			return delay
		}
	}
	return parseRetryAfterHeader(headers, now)
}

// Normalize only an explicitly set level on the already selected model.
func normalizeOrdinaryAntigravityLevel(doc *ojson.Doc) error {
	for _, path := range []string{
		"request.generationConfig.thinkingConfig.thinkingLevel",
		"request.generationConfig.thinkingConfig.thinking_level",
		"request.generationConfig.thinking_config.thinkingLevel",
		"request.generationConfig.thinking_config.thinking_level",
	} {
		level := strings.TrimSpace(doc.Get(path).String())
		if level != "" {
			return antigravitythinking.NewApplier().ApplyDoc(doc, thinking.ThinkingConfig{Mode: thinking.ModeLevel, Level: thinking.ThinkingLevel(level)}, nil)
		}
	}
	return nil
}
