package executor

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/MoeForge/moego/ojson"
	"github.com/klauspost/compress/zstd"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/thinking"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

const maxOrdinaryCodexRequestBytes = 32 << 20
const ordinaryCodexDefaultBase = "https://chatgpt.com/backend-api/codex"

func ordinaryCodexOrigin(base string) (*url.URL, error) {
	if base == "" {
		base = ordinaryCodexDefaultBase
	}
	u, err := url.Parse(base)
	if err != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Scheme != "https" && u.Scheme != "http") {
		return nil, statusErr{code: 400, msg: "invalid Codex upstream URL", requestScoped: true}
	}
	return u, nil
}

func ordinaryCodexOriginPort(u *url.URL) string {
	if p := u.Port(); p != "" {
		return p
	}
	if u.Scheme == "https" {
		return "443"
	}
	return "80"
}

// PrepareRequest attaches the selected credential to its configured origin only.
func (e *CodexExecutor) PrepareRequest(req *http.Request, auth *cliproxyauth.Auth) error {
	if req == nil || req.URL == nil {
		return statusErr{code: 400, msg: "invalid Codex request", requestScoped: true}
	}
	if auth != nil && auth.Disabled {
		return statusErr{code: http.StatusForbidden, msg: "Codex credential is disabled", requestScoped: true}
	}
	token, base := ordinaryCodexImageCredential(auth)
	if token == "" {
		return statusErr{code: 401, msg: "missing Codex upstream credential", requestScoped: true}
	}
	u, err := ordinaryCodexOrigin(base)
	if err != nil {
		return err
	}
	if req.URL.User != nil || req.Host != "" && !strings.EqualFold(req.Host, req.URL.Host) || req.URL.Scheme != u.Scheme || !strings.EqualFold(req.URL.Hostname(), u.Hostname()) || ordinaryCodexOriginPort(req.URL) != ordinaryCodexOriginPort(u) {
		return statusErr{code: 403, msg: "Codex credential origin mismatch", requestScoped: true}
	}
	if req.Header == nil {
		req.Header = make(http.Header)
	}
	for name := range req.Header {
		if strings.EqualFold(name, "Authorization") || strings.EqualFold(name, "ChatGPT-Account-ID") {
			delete(req.Header, name)
		}
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if u.Scheme == "https" && strings.EqualFold(u.Hostname(), "chatgpt.com") && ordinaryCodexOriginPort(u) == "443" {
		if account, ok := auth.Metadata["account_id"].(string); ok && strings.TrimSpace(account) != "" {
			req.Header.Set("ChatGPT-Account-ID", strings.TrimSpace(account))
		}
	}
	return nil
}

// Apply administrator model headers only for this selected credential and its
// explicitly configured origin. The allowed headers are non-authenticating.
func (e *CodexExecutor) applyOrdinaryCodexModelHeaders(r *http.Request, auth *cliproxyauth.Auth, model string) {
	if e == nil || auth == nil || auth.Disabled || strings.TrimSpace(auth.ID) == "" {
		return
	}
	entry := e.resolveCodexConfig(auth)
	if entry == nil || strings.TrimSpace(entry.BaseURL) == "" {
		return
	}
	origin, err := ordinaryCodexOrigin(strings.TrimSpace(entry.BaseURL))
	if err != nil || r.URL.Scheme != origin.Scheme || !strings.EqualFold(r.URL.Hostname(), origin.Hostname()) ||
		ordinaryCodexOriginPort(r.URL) != ordinaryCodexOriginPort(origin) || (r.Host != "" && !strings.EqualFold(r.Host, r.URL.Host)) {
		return
	}
	model = thinking.ParseSuffix(model).ModelName
	for _, entry := range registry.GetGlobalRegistry().GetModelsForClient(auth.ID) {
		if entry == nil || entry.Config == nil || !strings.EqualFold(entry.ID, model) {
			continue
		}
		for key, value := range entry.Config.OverrideHeader {
			name := http.CanonicalHeaderKey(strings.TrimSpace(key))
			switch name {
			case "Accept-Language", "X-Correlation-Id":
				value = strings.TrimSpace(value)
				if value != "" && len(value) <= 128 && strings.IndexFunc(value, func(r rune) bool { return r < 32 || r > 126 }) == -1 {
					r.Header.Set(name, value)
				}
			}
		}
		return
	}
}

func (e *CodexExecutor) ordinaryCodexHTTPClient(ctx context.Context, auth *cliproxyauth.Auth) *http.Client {
	client := *helps.NewProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &client
}

func (e *CodexExecutor) HttpRequest(ctx context.Context, auth *cliproxyauth.Auth, req *http.Request) (*http.Response, error) {
	if req == nil {
		return nil, statusErr{code: 400, msg: "invalid Codex request", requestScoped: true}
	}
	if ctx == nil {
		ctx = req.Context()
	}
	if req.ContentLength > maxOrdinaryCodexRequestBytes || req.Body != nil && req.Body != http.NoBody && req.ContentLength <= 0 {
		return nil, statusErr{code: 413, msg: "Codex SDK request body requires a bounded content length", requestScoped: true}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	cloned := req.Clone(ctx)
	if err := e.PrepareRequest(cloned, auth); err != nil {
		return nil, err
	}
	return e.ordinaryCodexHTTPClient(ctx, auth).Do(cloned)
}

func ordinaryCodexPromptCache(ctx context.Context, doc *ojson.Doc, auth *cliproxyauth.Auth, model string, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) {
	tenant := strings.TrimSpace(helps.APIKeyFromContext(ctx))
	token, base := ordinaryCodexImageCredential(auth)
	key := strings.TrimSpace(doc.Get("prompt_cache_key").String())
	if key == "" {
		key = helps.ProviderSessionUUID("codex", opts.Metadata, req.Metadata)
	}
	if tenant == "" || token == "" || key == "" {
		_ = doc.Delete("prompt_cache_key")
		return
	}
	mac := hmac.New(sha256.New, []byte(tenant))
	for _, s := range []string{"codex-prompt-cache-v1", token, base, model, key} {
		_, _ = io.WriteString(mac, s)
		_, _ = mac.Write([]byte{0})
	}
	_ = doc.Set("prompt_cache_key", hex.EncodeToString(mac.Sum(nil)))
}

// The selected credential must be an explicit API key on a non-ChatGPT origin
// to opt into the platform Responses fields. No inbound field chooses the origin.
func ordinaryCodexPlatformIncludes(auth *cliproxyauth.Auth) bool {
	if auth == nil || auth.Disabled || strings.TrimSpace(auth.Attributes["api_key"]) == "" {
		return false
	}
	_, base := ordinaryCodexImageCredential(auth)
	origin, err := ordinaryCodexOrigin(base)
	if err != nil {
		return false
	}
	host := strings.ToLower(strings.TrimRight(origin.Hostname(), "."))
	return host != "chatgpt.com" && !strings.HasSuffix(host, ".chatgpt.com")
}

func restoreOrdinaryCodexResponseIncludes(doc *ojson.Doc, source []byte, platform bool) {
	include := ojson.GetBytesNoCopy(source, "include")
	if !include.IsArray() {
		return
	}
	allowed := [...]string{
		"web_search_call.action.sources", "web_search_call.results",
		"file_search_call.results", "message.input_image.image_url",
		"computer_call_output.output.image_url", "code_interpreter_call.outputs",
		"message.output_text.logprobs",
	}
	var requested [len(allowed)]bool
	for _, item := range include.Array() {
		if item.Type != ojson.String {
			continue
		}
		for i, field := range allowed {
			if (i < 2 || platform) && item.String() == field {
				requested[i] = true
				break
			}
		}
	}
	// Preserve the existing converter byte path when no newly supported field was requested.
	var optIn bool
	for _, yes := range requested[1:] {
		optIn = optIn || yes
	}
	if !optIn {
		return
	}
	values := []string{"reasoning.encrypted_content"}
	for i, field := range allowed {
		if requested[i] {
			values = append(values, field)
		}
	}
	_ = doc.Set("include", values)
}

func (e *CodexExecutor) prepareOrdinaryCodexBody(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, stream, compat bool) ([]byte, error) {
	if len(req.Payload) > maxOrdinaryCodexRequestBytes || len(opts.OriginalRequest) > maxOrdinaryCodexRequestBytes {
		return nil, statusErr{code: 413, msg: "Codex request exceeds size limit", requestScoped: true}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	model := thinking.ParseSuffix(req.Model).ModelName
	original := opts.OriginalRequest
	if len(original) == 0 {
		original = req.Payload
	}
	to := sdktranslator.FromString("codex")
	originalTranslated, body, updatesChanged, err := translateCodexRequestPairWithUpdateIntentContext(ctx, opts.SourceFormat, to, model, original, req.Payload, stream, compat)
	if err != nil {
		return nil, err
	}
	if !ojson.ValidBytes(body) {
		return nil, statusErr{code: 400, msg: "invalid Codex request payload", requestScoped: true}
	}
	doc := ojson.NewDoc(body)

	normalizeCodexOutputBudgetDoc(doc)
	_ = doc.Set("model", model)
	if err = helps.ApplyRequestThinkingDoc(doc, req, opts, opts.SourceFormat.String(), to.String(), e.Identifier(), updatesChanged); err != nil {
		return nil, err
	}
	if opts.SourceFormat == sdktranslator.FormatOpenAIResponse || opts.SourceFormat == sdktranslator.FormatCodex {
		restoreOrdinaryCodexUpdates(doc, req.Payload)
	}
	// Both ordinary HTTP and WebSocket use streaming /responses upstream.
	// The compact endpoint does not use this Responses include contract.
	if stream && opts.SourceFormat == sdktranslator.FormatOpenAIResponse {
		platform := ordinaryCodexPlatformIncludes(auth)
		restoreOrdinaryCodexResponseIncludes(doc, original, platform)
		originalDoc := ojson.NewDoc(originalTranslated)
		restoreOrdinaryCodexResponseIncludes(originalDoc, original, platform)
		originalTranslated = originalDoc.Bytes()
	}
	ctx = helps.WithPayloadFinalizer(ctx, helps.NewPayloadFinalizer(e.cfg, e.Identifier(), model, to.String(), "", originalTranslated, req, opts, auth))
	helps.FinalizePayloadDoc(ctx, doc)
	applyConfiguredCodexTier(doc, auth, model)
	normalizeCodexInstructionsDoc(doc, opts.SourceFormat == to)
	if !codexCompactionSummaryFromContext(ctx) {
		ensureWebSearchToolForHistoryDoc(doc, nil)
	}
	helps.SanitizeCodexInputItemIDsDoc(doc)
	normalizeCodexParallelToolCallsDoc(doc, nil)
	if !doc.Get("parallel_tool_calls").Exists() &&
		(opts.SourceFormat == sdktranslator.FormatOpenAIResponse || opts.SourceFormat == sdktranslator.FormatCodex) &&
		!isCodexResponsesLiteRequestDoc(doc, nil) {
		explicit := ojson.GetBytesNoCopy(req.Payload, "parallel_tool_calls")
		if explicit.Type == ojson.True || explicit.Type == ojson.False {
			for _, item := range doc.Get("input").Array() {
				if item.Get("type").String() == "additional_tools" && len(item.Get("tools").Array()) > 0 {
					_ = doc.Set("parallel_tool_calls", explicit.Bool())
					break
				}
			}
		}
	}
	helps.NormalizeCodexToolSchemasDoc(doc)
	if e.cfg != nil && e.cfg.DisableImageGeneration != config.DisableImageGenerationOff && e.cfg.DisableImageGeneration != config.DisableImageGenerationPassthrough {
		if containsOrdinaryCodexImageTool(doc) {
			return nil, statusErr{code: http.StatusBadRequest, msg: "image generation is disabled", requestScoped: true}
		}
	}
	if err := helps.ValidateCodexHostedImageGenerationDoc(doc, nil, e.cfg, auth != nil && strings.TrimSpace(auth.Attributes["api_key"]) == ""); err != nil {
		return nil, err
	}
	if stream {
		_ = doc.Set("stream", true)
	} else {
		_ = doc.Delete("stream")
	}
	for _, field := range []string{"previous_response_id", "generate", "prompt_cache_retention", "safety_identifier", "stream_options"} {
		_ = doc.Delete(field)
	}
	ordinaryCodexPromptCache(ctx, doc, auth, model, req, opts)
	body = doc.Bytes()
	if len(body) > maxOrdinaryCodexRequestBytes {
		return nil, statusErr{code: 413, msg: "Codex request exceeds size limit", requestScoped: true}
	}
	return body, nil
}

func (e *CodexExecutor) sendOrdinaryCodex(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, endpoint string, stream, compat bool, reporter *helps.UsageReporter) (*http.Response, []byte, error) {
	body, err := e.prepareOrdinaryCodexBody(ctx, auth, req, opts, stream, compat)
	if err != nil {
		return nil, nil, err
	}

	reporter.SetTranslatedRequestMetadata(body, "codex")

	_, base := ordinaryCodexImageCredential(auth)
	origin, err := ordinaryCodexOrigin(base)
	if err != nil {
		return nil, nil, err
	}
	origin.Path = strings.TrimRight(origin.Path, "/") + endpoint
	origin.RawPath = ""
	wireBody := body
	compressed := e.cfg != nil && e.cfg.Codex.UpstreamRequestCompression &&
		len(body) >= e.cfg.Codex.RequestCompressionMinBytes() && auth != nil &&
		strings.TrimSpace(auth.Attributes["api_key"]) == "" &&
		origin.Scheme == "https" && strings.EqualFold(origin.Hostname(), "chatgpt.com") &&
		ordinaryCodexOriginPort(origin) == "443" && strings.HasPrefix(origin.Path, "/backend-api/codex/")
	if compressed {
		encoder, err := zstd.NewWriter(nil, zstd.WithEncoderConcurrency(1))
		if err != nil {
			return nil, nil, err
		}
		wireBody = encoder.EncodeAll(body, nil)
		encoder.Close()
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, origin.String(), bytes.NewReader(wireBody))
	if err != nil {
		return nil, nil, err
	}
	if err = e.PrepareRequest(httpReq, auth); err != nil {
		return nil, nil, err
	}
	e.applyOrdinaryCodexModelHeaders(httpReq, auth, req.Model)
	applyOrdinaryCodexRequestID(httpReq.Header, opts.Headers)
	httpReq.Header.Set("Content-Type", "application/json")
	if compressed {
		httpReq.Header.Set("Content-Encoding", "zstd")
	}
	httpReq.Header.Set("Accept", "application/json")
	if stream {
		httpReq.Header.Set("Accept", "text/event-stream")
		helps.RecordAPIRequest(ctx, e.cfg, helps.UpstreamRequestLog{
			URL: httpReq.URL.String(), Method: httpReq.Method, Headers: codexStreamDiagnosticHeaders(httpReq.Header), Body: body, Provider: e.Identifier(),
		})
	}
	client := reporter.TrackHTTPClient(e.ordinaryCodexHTTPClient(ctx, auth))
	resp, err := client.Do(httpReq)
	if err != nil {
		if stream {
			helps.RecordAPIResponseError(ctx, e.cfg, err)
		}
		return nil, nil, err
	}
	if stream {
		helps.RecordAPIResponseMetadata(ctx, e.cfg, resp.StatusCode, codexStreamDiagnosticHeaders(resp.Header))
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		rawBody := resp.Body
		var raw []byte
		if decoded, errDecode := helps.DecodeResponseBody(rawBody, resp.Header); errDecode == nil {
			raw, _ = io.ReadAll(io.LimitReader(decoded, 64<<10))
			_ = decoded.Close()
		} else {
			_ = rawBody.Close()
		}
		if stream {
			helps.AppendAPIResponseChunk(ctx, e.cfg, raw)
		}
		e.observeOrdinaryPolicyRefusal(ctx, auth, req.Model, raw)
		status := newCodexStatusErrWithHeadersAndCooling(resp.StatusCode, resp.Header, raw, e.modelLevelCooling())
		if (resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden) && len(resp.Header.Values("Retry-After")) == 1 {
			status.retryAfter = parseRetryAfterHeader(resp.Header, time.Now())
		}
		return nil, nil, status
	}
	decoded, err := helps.DecodeResponseBody(resp.Body, resp.Header)
	if err != nil {
		if stream {
			helps.RecordAPIResponseError(ctx, e.cfg, err)
		}
		_ = resp.Body.Close()
		return nil, nil, statusErr{code: http.StatusBadGateway, msg: "invalid Codex response encoding", cause: err}
	}
	resp.Body = decoded
	resp.Header.Del("Content-Encoding")
	resp.Header.Del("Content-Length")
	resp.ContentLength = -1
	return resp, body, nil
}

// codexStreamDiagnosticHeaders copies only the headers needed to diagnose
// wire format. Credential and account headers stay on the actual HTTP exchange.
func codexStreamDiagnosticHeaders(source http.Header) http.Header {
	diagnostic := make(http.Header, 3)
	for _, name := range []string{"Accept", "Content-Type", "Content-Encoding"} {
		if values := source.Values(name); len(values) > 0 {
			diagnostic[name] = append([]string(nil), values...)
		}
	}
	return diagnostic
}

func applyOrdinaryCodexRequestID(target, caller http.Header) {
	if id := caller.Get("X-Client-Request-Id"); validOrdinaryCodexRequestID(id) {
		target.Set("X-Client-Request-Id", id)
	}
}

func containsOrdinaryCodexImageTool(doc *ojson.Doc) bool {
	has := func(tools ojson.Result) bool {
		for _, tool := range tools.Array() {
			if tool.Get("type").String() == "image_generation" {
				return true
			}
		}
		return false
	}
	if has(doc.Get("tools")) {
		return true
	}
	for _, item := range doc.Get("input").Array() {
		if (item.Get("type").String() == "additional_tools" || item.Get("type").String() == "configuration_update") && has(item.Get("tools")) {
			return true
		}
	}
	return false
}

func restoreOrdinaryCodexUpdates(doc *ojson.Doc, original []byte) bool {
	incoming := ojson.GetBytesNoCopy(original, "input")
	translated := doc.Get("input")
	if !incoming.IsArray() || !translated.IsArray() {
		return false
	}
	current := translated.Array()
	for _, item := range current {
		if item.Get("type").String() == "configuration_update" {
			return false
		}
	}
	count := 0
	for _, item := range incoming.Array() {
		if item.Get("type").String() != "configuration_update" {
			count++
		}
	}
	if count != len(current) {
		return false
	}
	merged := make([]json.RawMessage, 0, len(incoming.Array()))
	index := 0
	for _, item := range incoming.Array() {
		if item.Get("type").String() != "configuration_update" {
			merged = append(merged, json.RawMessage(current[index].Raw))
			index++
			continue
		}
		update := ojson.NewDocString(`{"type":"configuration_update"}`)
		if tools := item.Get("tools"); tools.IsArray() {
			_ = update.SetRaw("tools", tools.Raw)
		}
		if effort := item.Get("reasoning.effort"); effort.Type == ojson.String {
			switch effort.String() {
			case "none", "minimal", "low", "medium", "high", "xhigh", "auto":
				_ = update.Set("reasoning.effort", effort.String())
			}
		}
		if update.Get("tools").Exists() || update.Get("reasoning").Exists() {
			merged = append(merged, json.RawMessage(update.Bytes()))
		}
	}
	return len(merged) > len(current) && doc.Set("input", merged) == nil
}
