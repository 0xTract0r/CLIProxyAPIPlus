package executor

import (
	"bytes"
	"context"
	"net/http"
	"strings"

	"github.com/MoeForge/moego/ojson"
	claudeauth "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/claude"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	responsesclaude "github.com/router-for-me/CLIProxyAPI/v8/internal/translator/claude/openai/responses"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	log "github.com/sirupsen/logrus"
)

type ClaudeExecutor struct {
	cfg                     *config.Config
	requestLogProvider      string
	upstreamModelNormalizer func(string) string
	// Kept for the old detached tool-state helper while its retirement is reviewed.
}

const defaultModelMaxTokens = 1024

func NewClaudeExecutor(cfg *config.Config) *ClaudeExecutor { return &ClaudeExecutor{cfg: cfg} }

// NewClaudeExecutorWithManager retains service registration compatibility.
func NewClaudeExecutorWithManager(cfg *config.Config, _ *cliproxyauth.Manager) *ClaudeExecutor {
	return NewClaudeExecutor(cfg)
}
func (e *ClaudeExecutor) Identifier() string       { return "claude" }
func (e *ClaudeExecutor) SupportsApplyPatch() bool { return e != nil }

func stripTranslatedDefaultMaxTokens(from sdktranslator.Format, originalPayload, originalTranslated []byte) []byte {
	if len(originalTranslated) == 0 || from == sdktranslator.FormatClaude {
		return originalTranslated
	}
	hasExplicitMaxTokens := func(paths ...string) bool {
		for _, path := range paths {
			if v := ojson.GetBytesNoCopy(originalPayload, path); v.Exists() && v.Type != ojson.Null {
				return true
			}
		}
		return false
	}
	switch from {
	case sdktranslator.FormatOpenAI:
		if hasExplicitMaxTokens("max_tokens", "max_completion_tokens") {
			return originalTranslated
		}
	case sdktranslator.FormatOpenAIResponse:
		if hasExplicitMaxTokens("max_output_tokens") {
			return originalTranslated
		}
	case sdktranslator.FormatGemini:
		if hasExplicitMaxTokens("generationConfig.maxOutputTokens") {
			return originalTranslated
		}
	default:
		return originalTranslated
	}
	stripped, errDelete := ojson.DeleteBytes(originalTranslated, "max_tokens")
	if errDelete != nil {
		return originalTranslated
	}
	return stripped
}

func (e ClaudeExecutor) WithConfig(cfg *config.Config) *ClaudeExecutor {
	e.cfg = cfg
	return &e
}

func (e *ClaudeExecutor) SupportsNativeToolSearch(auth *cliproxyauth.Auth, upstreamModel string) bool {
	return e.supportsNativeToolSearch(responsesclaude.ClientToolSearchEnabled(), auth, upstreamModel)
}

func (e *ClaudeExecutor) supportsNativeToolSearch(toolSearch bool, auth *cliproxyauth.Auth, upstreamModel string) bool {
	if !toolSearch || helps.ClaudeModelLacksToolSearch(upstreamModel) {
		return false
	}
	_, baseURL, _ := ordinaryClaudeCredential(auth)
	if isAnthropicUpstreamBase(baseURL) {
		return true
	}
	if auth != nil && auth.Attributes != nil && strings.EqualFold(strings.TrimSpace(auth.Attributes["native_tool_search"]), "true") {
		return true
	}
	entry := matchOrdinaryClaudeKey(e.cfg, auth)
	return entry != nil && entry.NativeToolSearch
}

// Pin the public tool-search setting for the request and its response translation.
// A reload may occur while the HTTP round trip is in progress.
func pinOrdinaryClaudeToolSearch(ctx context.Context, source sdktranslator.Format) context.Context {
	if source != sdktranslator.FormatOpenAIResponse {
		return ctx
	}
	return responsesclaude.WithClientToolSearch(ctx, responsesclaude.ClientToolSearchEnabledFor(ctx))
}

func (e *ClaudeExecutor) applyNativeToolSearchFallback(ctx context.Context, auth *cliproxyauth.Auth, from sdktranslator.Format, upstreamModel string, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (context.Context, cliproxyexecutor.Request, cliproxyexecutor.Options) {
	if from != sdktranslator.FormatOpenAIResponse {
		return ctx, req, opts
	}
	toolSearch := responsesclaude.ClientToolSearchEnabledFor(ctx)
	ctx = responsesclaude.WithClientToolSearch(ctx, toolSearch)
	if !toolSearch {
		return ctx, req, opts
	}
	if !helps.ResponsesHasToolSearch(req.Payload) && !helps.ResponsesHasToolSearch(opts.OriginalRequest) {
		return ctx, req, opts
	}
	if e.supportsNativeToolSearch(toolSearch, auth, upstreamModel) {
		return ctx, req, opts
	}
	req.Payload = helps.StripResponsesToolSearch(req.Payload)
	if len(opts.OriginalRequest) > 0 {
		opts.OriginalRequest = helps.StripResponsesToolSearch(opts.OriginalRequest)
	}
	log.Debugf("claude executor: native tool search unavailable, removed tool_search from request (model=%s)", upstreamModel)
	return ctx, req, opts
}

func (e *ClaudeExecutor) modelLevelCooling() bool {
	return e != nil && e.cfg != nil && e.cfg.Claude.ModelLevelCooling
}

func (e *ClaudeExecutor) upstreamRequestLogProvider() string {
	if provider := strings.TrimSpace(e.requestLogProvider); provider != "" {
		return provider
	}
	return e.Identifier()
}

func (e *ClaudeExecutor) upstreamModel(baseModel string) string {
	if e.upstreamModelNormalizer != nil {
		return e.upstreamModelNormalizer(baseModel)
	}
	return baseModel
}

func (e *ClaudeExecutor) restoreResponseModel(payload []byte, model string) []byte {
	if e.upstreamModelNormalizer == nil || strings.TrimSpace(model) == "" {
		return payload
	}
	return restoreClaudeResponseModel(payload, model)
}

func restoreClaudeResponseModel(payload []byte, model string) []byte {
	if updated, changed := setClaudeResponseModel(payload, model); changed {
		return updated
	}

	trimmed := bytes.TrimSpace(payload)
	if !bytes.HasPrefix(trimmed, []byte("data:")) {
		return payload
	}
	dataIndex := bytes.Index(payload, []byte("data:"))
	if dataIndex < 0 {
		return payload
	}
	rawJSON := bytes.TrimSpace(payload[dataIndex+len("data:"):])
	updated, changed := setClaudeResponseModel(rawJSON, model)
	if !changed {
		return payload
	}
	rebuilt := make([]byte, 0, dataIndex+len("data: ")+len(updated))
	rebuilt = append(rebuilt, payload[:dataIndex]...)
	rebuilt = append(rebuilt, []byte("data: ")...)
	rebuilt = append(rebuilt, updated...)
	return rebuilt
}

func setClaudeResponseModel(payload []byte, model string) ([]byte, bool) {
	if !ojson.ValidBytes(payload) {
		return payload, false
	}
	doc := ojson.NewDoc(payload)
	changed := false
	for _, path := range []string{"model", "message.model"} {
		if !doc.Get(path).Exists() {
			continue
		}
		if errSet := doc.Set(path, model); errSet == nil {
			changed = true
		}
	}
	if !changed {
		return payload, false
	}
	return doc.Bytes(), true
}

func (e *ClaudeExecutor) PrepareRequest(req *http.Request, auth *cliproxyauth.Auth) error {
	if req == nil || req.URL == nil {
		return statusErr{code: http.StatusBadRequest, msg: "invalid Claude SDK request", requestScoped: true}
	}
	if err := e.rejectUnsupportedClaudeRequest(auth); err != nil {
		return err
	}
	if auth != nil && auth.Disabled {
		return statusErr{code: http.StatusForbidden, msg: "Claude credential is disabled", requestScoped: true}
	}
	key, base, bearer := ordinaryClaudeCredential(auth)
	if key == "" {
		return statusErr{code: http.StatusUnauthorized, msg: "missing upstream credential", requestScoped: true}
	}
	configured, err := parseOrdinaryClaudeBase(base)
	if err != nil {
		return err
	}
	if req.URL.User != nil || req.Host != "" && !strings.EqualFold(req.Host, req.URL.Host) || req.URL.Scheme != configured.Scheme || !strings.EqualFold(req.URL.Hostname(), configured.Hostname()) || ordinaryClaudeOriginPort(req.URL.Scheme, req.URL.Port()) != ordinaryClaudeOriginPort(configured.Scheme, configured.Port()) {
		return statusErr{code: http.StatusForbidden, msg: "Claude request origin does not match the configured credential origin", requestScoped: true}
	}
	if req.Header == nil {
		req.Header = make(http.Header)
	}
	clearOrdinaryClaudeCredentials(req.Header)
	if bearer {
		req.Header.Set("Authorization", "Bearer "+key)
	} else {
		req.Header.Set("X-Api-Key", key)
	}
	if req.Header.Get("Anthropic-Version") == "" {
		req.Header.Set("Anthropic-Version", "2023-06-01")
	}
	return nil
}

func ordinaryClaudeOriginPort(scheme, port string) string {
	if port != "" {
		return port
	}
	if scheme == "https" {
		return "443"
	}
	return "80"
}

func (e *ClaudeExecutor) HttpRequest(ctx context.Context, auth *cliproxyauth.Auth, req *http.Request) (*http.Response, error) {
	if req == nil {
		return nil, statusErr{code: http.StatusBadRequest, msg: "invalid Claude SDK request", requestScoped: true}
	}
	if ctx == nil {
		ctx = req.Context()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if req.ContentLength > ordinaryClaudeMaxRequestBytes || req.Body != nil && req.Body != http.NoBody && req.ContentLength <= 0 {
		return nil, statusErr{code: http.StatusRequestEntityTooLarge, msg: "Claude request exceeds size limit", requestScoped: true}
	}
	httpReq := req.Clone(ctx)
	if err := e.PrepareRequest(httpReq, auth); err != nil {
		return nil, err
	}
	client := *helps.NewProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return doOrdinaryClaudeMessagesRequest(&client, httpReq)
}

func clearOrdinaryClaudeCredentials(headers http.Header) {
	for name := range headers {
		if strings.EqualFold(name, "authorization") || strings.EqualFold(name, "x-api-key") {
			delete(headers, name)
		}
	}
}

func matchOrdinaryClaudeKey(cfg *config.Config, auth *cliproxyauth.Auth) *config.ClaudeKey {
	if cfg == nil || auth == nil {
		return nil
	}
	key := strings.TrimSpace(auth.Attributes["api_key"])
	if key == "" {
		return nil
	}
	_, base, _ := ordinaryClaudeCredential(auth)
	for i := range cfg.ClaudeKey {
		entry := &cfg.ClaudeKey[i]
		entryBase := strings.TrimSpace(entry.BaseURL)
		if entryBase == "" {
			entryBase = "https://api.anthropic.com"
		}
		if strings.TrimSpace(entry.APIKey) == key && strings.TrimRight(entryBase, "/") == strings.TrimRight(base, "/") {
			return entry
		}
	}
	return nil
}

func (e *ClaudeExecutor) rejectUnsupportedClaudeRequest(auth *cliproxyauth.Auth) error {
	profile := ""
	mode := ""
	if auth != nil {
		profile = strings.TrimSpace(auth.Attributes["fingerprint_profile"])
		if profile == "" {
			profile = strings.TrimSpace(claudeauth.ReadMetadataString(&auth.Metadata, "fingerprint_profile"))
		}
		mode = strings.TrimSpace(auth.Attributes["cloak_mode"])
		if mode == "" {
			mode = strings.TrimSpace(claudeauth.ReadMetadataString(&auth.Metadata, "cloak_mode"))
		}
	}
	if entry := matchOrdinaryClaudeKey(e.cfg, auth); entry != nil {
		if profile == "" {
			profile = strings.TrimSpace(entry.FingerprintProfile)
		}
		if mode == "" && entry.Cloak != nil {
			mode = strings.TrimSpace(entry.Cloak.Mode)
			if mode == "" {
				mode = "auto"
			}
		}
	}
	if profile != "" && !strings.EqualFold(profile, "none") || mode != "" && !strings.EqualFold(mode, "never") && !strings.EqualFold(mode, "uncloak") {
		return statusErr{code: http.StatusBadRequest, msg: "Claude client identity emulation is unsupported", requestScoped: true}
	}
	return nil
}
