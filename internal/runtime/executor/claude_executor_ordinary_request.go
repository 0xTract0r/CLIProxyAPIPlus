package executor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"slices"
	"sort"
	"strings"
	"sync/atomic"

	"github.com/MoeForge/moego/ojson"
	claudeauth "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/claude"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"golang.org/x/net/http/httpguts"
)

const ordinaryClaudeMaxRequestBytes = 32 << 20

func ordinaryClaudeCredential(auth *cliproxyauth.Auth) (key, base string, bearer bool) {
	base = "https://api.anthropic.com"
	if auth == nil {
		return
	}
	if configured := strings.TrimSpace(auth.Attributes["base_url"]); configured != "" {
		base = configured
	}
	key = strings.TrimSpace(auth.Attributes["api_key"])
	if key != "" {
		// Preserve old OAuth credentials stored in the API-key attribute.
		bearer = strings.HasPrefix(key, "sk-ant-oat")
	} else if !ordinaryClaudeExplicitAPIKey(auth) {
		key = strings.TrimSpace(claudeauth.ReadMetadataString(&auth.Metadata, "access_token"))
		bearer = true
	}
	return
}

func ordinaryClaudeExplicitAPIKey(auth *cliproxyauth.Auth) bool {
	if auth == nil {
		return false
	}
	kind := strings.TrimSpace(auth.Attributes["auth_kind"])
	if kind == "" {
		kind = claudeauth.ReadMetadataString(&auth.Metadata, "auth_kind")
	}
	switch strings.ToLower(kind) {
	case "apikey", "api_key", "api-key":
		return true
	default:
		return false
	}
}

func parseOrdinaryClaudeBase(base string) (*url.URL, error) {
	u, err := url.Parse(base)
	if err != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" || (u.Scheme != "https" && u.Scheme != "http") {
		return nil, statusErr{code: http.StatusBadRequest, msg: "invalid Claude upstream base URL", requestScoped: true}
	}
	return u, nil
}

func newOrdinaryClaudeRequest(ctx context.Context, auth *cliproxyauth.Auth, path string, payload []byte, stream bool) (*http.Request, error) {
	if len(payload) > ordinaryClaudeMaxRequestBytes {
		return nil, statusErr{code: http.StatusRequestEntityTooLarge, msg: "Claude request exceeds size limit", requestScoped: true}
	}
	if ctx != nil && ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if auth != nil && auth.Disabled {
		return nil, statusErr{code: http.StatusForbidden, msg: "Claude credential is disabled", requestScoped: true}
	}
	key, base, bearer := ordinaryClaudeCredential(auth)
	if key == "" {
		return nil, statusErr{code: http.StatusUnauthorized, msg: "missing upstream credential", requestScoped: true}
	}
	u, err := parseOrdinaryClaudeBase(base)
	if err != nil {
		return nil, err
	}
	u.Path = strings.TrimRight(u.Path, "/") + path
	u.RawPath = ""
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Anthropic-Version", "2023-06-01")
	req.Header.Set("Accept", "application/json")
	if stream {
		req.Header.Set("Accept", "text/event-stream")
	}
	if bearer {
		req.Header.Set("Authorization", "Bearer "+key)
	} else {
		req.Header.Set("X-Api-Key", key)
	}
	return req, nil
}

func (e *ClaudeExecutor) sendOrdinaryClaudeRequest(ctx context.Context, auth *cliproxyauth.Auth, path string, body []byte, stream bool, headers http.Header, reporter *helps.UsageReporter) (*http.Response, error) {
	claude := sdktranslator.FromString("claude")
	var betas []string
	seen := make(map[string]bool)
	add := func(value string) {
		for _, beta := range strings.Split(value, ",") {
			beta = strings.TrimSpace(beta)
			if !validOrdinaryClaudeBeta(beta) || seen[beta] {
				continue
			}
			seen[beta] = true
			betas = append(betas, beta)
		}
	}
	for key, values := range headers {
		if strings.EqualFold(key, "Anthropic-Beta") {
			for _, value := range values {
				add(value)
			}
		}
	}
	doc := ojson.NewDoc(body)
	changed := false
	if field := doc.Get("betas"); field.Exists() {
		for _, value := range field.Array() {
			if value.Type == ojson.String {
				add(value.String())
			}
		}
		changed = doc.Delete("betas") == nil
	}
	if sanitizeClaudeWebSearchDomainsDoc(doc) || changed {
		body = doc.Bytes()
	}
	httpReq, err := newOrdinaryClaudeRequest(ctx, auth, path, body, stream)
	if err != nil {
		return nil, err
	}
	gatewayBeta, hasGatewayBeta := applyOrdinaryClaudeConfiguredHeaders(httpReq, auth)
	if hasGatewayBeta {
		betas = nil
		clear(seen)
		add(gatewayBeta)
	} else if e.cfg != nil {
		defaults := e.cfg.ClaudeHeaderDefaults
		strip := make(map[string]bool, len(defaults.AnthropicBetaStrip))
		for _, beta := range defaults.AnthropicBetaStrip {
			strip[strings.TrimSpace(beta)] = true
		}
		betas = slices.DeleteFunc(betas, func(beta string) bool {
			if !strip[beta] {
				return false
			}
			delete(seen, beta)
			return true
		})
		for _, beta := range defaults.AnthropicBetaAppend {
			add(beta)
		}
	}
	if len(betas) > 0 {
		httpReq.Header.Set("Anthropic-Beta", strings.Join(betas, ","))
	}
	client := *helps.NewProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	if reporter != nil {
		reporter.SetTranslatedReasoningEffort(body, claude.String())
		client = *reporter.TrackHTTPClient(&client)
	}
	// Never forward either Authorization or X-API-Key to a redirected host.
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := doOrdinaryClaudeMessagesRequest(&client, httpReq)
	if err == nil {
		return resp, nil
	}
	if errors.Is(err, context.Canceled) {
		return nil, context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return nil, context.DeadlineExceeded
	}
	// Transport diagnostics can contain proxy userinfo and internal endpoints.
	return nil, statusErr{code: http.StatusBadGateway, msg: "Claude upstream transport request failed", requestScoped: true}
}

// applyOrdinaryClaudeConfiguredHeaders accepts static per-credential headers
// without delegating authority, authentication, or client identity to config.
// The provider itself owns its Beta policy; an explicitly configured gateway
// may supply its own Beta header instead, as in the legacy configuration.
func applyOrdinaryClaudeConfiguredHeaders(req *http.Request, auth *cliproxyauth.Auth) (string, bool) {
	if auth == nil {
		return "", false
	}
	var keys []string
	for key := range auth.Attributes {
		if strings.HasPrefix(key, "header:") {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	gateway := !helps.IsAnthropicUpstreamURL(req.URL)
	var beta string
	var override bool
	for _, key := range keys {
		name := strings.TrimSpace(strings.TrimPrefix(key, "header:"))
		value := strings.TrimSpace(auth.Attributes[key])
		if value == "" || strings.HasPrefix(value, "$") || !httpguts.ValidHeaderFieldName(name) || !httpguts.ValidHeaderFieldValue(value) {
			continue
		}
		if strings.EqualFold(name, "Anthropic-Beta") {
			if gateway {
				beta, override = value, true
			}
			continue
		}
		if validOrdinaryClaudeConfiguredHeader(name) {
			req.Header.Set(name, value)
		}
	}
	return beta, override
}

func validOrdinaryClaudeConfiguredHeader(name string) bool {
	lower := strings.ToLower(name)
	switch lower {
	case "accept-language", "cache-control", "prefer":
		return true
	case "x-api-key", "x-app", "x-real-ip", "x-remote-user", "x-host", "x-original-url", "x-rewrite-url",
		"x-http-method", "x-http-method-override", "x-method-override", "x-access-token", "x-auth-token":
		return false
	}
	if !strings.HasPrefix(lower, "x-") {
		return false
	}
	for _, reserved := range []string{"x-anthropic-", "x-claude-", "x-stainless-", "x-forwarded-", "x-proxy-", "x-auth-",
		"x-client-version", "x-client-platform", "x-client-device", "x-device-"} {
		if strings.HasPrefix(lower, reserved) {
			return false
		}
	}
	for _, reserved := range []string{"-api-key", "-access-token", "-authorization", "-session-id", "-account-id", "-subscription-key", "-fingerprint"} {
		if strings.HasSuffix(lower, reserved) {
			return false
		}
	}

	return true
}

func doOrdinaryClaudeMessagesRequest(client *http.Client, req *http.Request) (*http.Response, error) {
	// Only a typed dial failure before any connection was acquired is eligible
	// for one retry. A response, a write or a read error is never replayed.
	var connected atomic.Bool
	trace := &httptrace.ClientTrace{GotConn: func(httptrace.GotConnInfo) { connected.Store(true) }}
	first := req.WithContext(httptrace.WithClientTrace(req.Context(), trace))
	resp, err := client.Do(first)
	var call *net.OpError
	if err == nil || resp != nil || connected.Load() || req.Context().Err() != nil || req.GetBody == nil || !errors.As(err, &call) || call.Op != "dial" {
		return resp, err
	}
	body, replayErr := req.GetBody()
	if replayErr != nil {
		return nil, err
	}
	retry := req.Clone(req.Context())
	retry.Body = body
	return client.Do(retry)
}

func validOrdinaryClaudeBeta(beta string) bool {
	if beta == "" || len(beta) > 128 || strings.HasPrefix(strings.ToLower(beta), "claude-code-") {
		return false
	}
	for _, r := range beta {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.' {
			continue
		}
		return false
	}
	return true
}

func sanitizeClaudeWebSearchDomainsDoc(doc *ojson.Doc) bool {
	if doc == nil {
		return false
	}
	tools := doc.Get("tools")
	if !tools.Exists() || !tools.IsArray() {
		return false
	}
	changed := false
	tools.ForEach(func(index, tool ojson.Result) bool {
		if !strings.HasPrefix(tool.Get("type").String(), "web_search_") {
			return true
		}
		for _, field := range []string{"allowed_domains", "blocked_domains"} {
			value := tool.Get(field)
			if value.Exists() && value.IsArray() && len(value.Array()) == 0 {
				path := fmt.Sprintf("tools.%d.%s", index.Int(), field)
				if errDelete := doc.Delete(path); errDelete == nil {
					changed = true
				}
			}
		}
		return true
	})
	return changed
}
