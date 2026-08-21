package management

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/fxamacker/cbor/v2"
	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/geminicli"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/proxyutil"
	log "github.com/sirupsen/logrus"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

const defaultAPICallTimeout = 60 * time.Second

// Stage-level timeouts applied to the outbound transport used by APICall and
// its OAuth token-refresh helpers. The outbound chain frequently traverses
// third party proxy providers; without these the dial / TLS handshake phase
// can stall for the full reverse-proxy budget (typically 30s) and surface
// as 502 to the management UI even though the local server is healthy.
//
// These deadlines only apply to the outbound transport. The total wall-clock
// budget for an APICall is still bounded by defaultAPICallTimeout above. We
// keep apiCallResponseHeaderTimeout strictly below 30s so a stalled upstream
// fails fast inside the handler with a structured 502 instead of being
// truncated by the operator-facing reverse proxy.
const (
	apiCallDialTimeout            = 10 * time.Second
	apiCallTLSHandshakeTimeout    = 10 * time.Second
	apiCallResponseHeaderTimeout  = 25 * time.Second
	apiCallExpectContinueTimeout  = 1 * time.Second
	apiCallIdleConnectionTimeout  = 90 * time.Second
	apiCallKeepAliveProbeInterval = 30 * time.Second
)

const (
	geminiOAuthClientID     = "681255809395-oo8ft2oprdrnp9e3aqf6av3hmdib135j.apps.googleusercontent.com"
	geminiOAuthClientSecret = "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"
)

var geminiOAuthScopes = []string{
	"https://www.googleapis.com/auth/cloud-platform",
	"https://www.googleapis.com/auth/userinfo.email",
	"https://www.googleapis.com/auth/userinfo.profile",
}

const (
	antigravityOAuthClientID     = "1071006060591-tmhssin2h21lcre235vtolojh4g403ep.apps.googleusercontent.com"
	antigravityOAuthClientSecret = "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"
)

var antigravityOAuthTokenURL = "https://oauth2.googleapis.com/token"

type apiCallRequest struct {
	AuthIndexSnake  *string           `json:"auth_index"`
	AuthIndexCamel  *string           `json:"authIndex"`
	AuthIndexPascal *string           `json:"AuthIndex"`
	Method          string            `json:"method"`
	URL             string            `json:"url"`
	Header          map[string]string `json:"header"`
	Data            string            `json:"data"`
}

type apiCallResponse struct {
	StatusCode int                 `json:"status_code"`
	Header     map[string][]string `json:"header"`
	Body       string              `json:"body"`
	Quota      *QuotaSnapshots     `json:"quota,omitempty"`
}

// APICall makes a generic HTTP request on behalf of the management API caller.
// It is protected by the management middleware.
//
// Endpoint:
//
//	POST /v0/management/api-call
//
// Authentication:
//
//	Same as other management APIs (requires a management key and remote-management rules).
//	You can provide the key via:
//	- Authorization: Bearer <key>
//	- X-Management-Key: <key>
//
// Request JSON (supports both application/json and application/cbor):
//   - auth_index / authIndex / AuthIndex (optional):
//     The credential "auth_index" from GET /v0/management/auth-files (or other endpoints returning it).
//     If omitted or not found, credential-specific proxy/token substitution is skipped.
//   - method (required): HTTP method, e.g. GET, POST, PUT, PATCH, DELETE.
//   - url (required): Absolute URL including scheme and host, e.g. "https://api.example.com/v1/ping".
//   - header (optional): Request headers map.
//     Supports magic variable "$TOKEN$" which is replaced using the selected credential:
//     1) metadata.access_token
//     2) attributes.api_key
//     3) metadata.token / metadata.id_token / metadata.cookie
//     Example: {"Authorization":"Bearer $TOKEN$"}.
//     Note: if you need to override the HTTP Host header, set header["Host"].
//   - data (optional): Raw request body as string (useful for POST/PUT/PATCH).
//
// Proxy selection (highest priority first):
//  1. Selected credential proxy_url
//  2. Global config proxy-url
//  3. Direct connect (environment proxies are not used)
//
// Response (returned with HTTP 200 when the APICall itself succeeds):
//
//	Format matches request Content-Type (application/json or application/cbor)
//	- status_code: Upstream HTTP status code.
//	- header: Upstream response headers.
//	- body: Upstream response body as string.
//	- quota (optional): For GitHub Copilot enterprise accounts, contains quota_snapshots
//	  with details for chat, completions, and premium_interactions.
//
// Example:
//
//	curl -sS -X POST "http://127.0.0.1:8317/v0/management/api-call" \
//	  -H "Authorization: Bearer <MANAGEMENT_KEY>" \
//	  -H "Content-Type: application/json" \
//	  -d '{"auth_index":"<AUTH_INDEX>","method":"GET","url":"https://api.example.com/v1/ping","header":{"Authorization":"Bearer $TOKEN$"}}'
//
//	curl -sS -X POST "http://127.0.0.1:8317/v0/management/api-call" \
//	  -H "Authorization: Bearer 831227" \
//	  -H "Content-Type: application/json" \
//	  -d '{"auth_index":"<AUTH_INDEX>","method":"POST","url":"https://api.example.com/v1/fetchAvailableModels","header":{"Authorization":"Bearer $TOKEN$","Content-Type":"application/json","User-Agent":"cliproxyapi"},"data":"{}"}'
func (h *Handler) APICall(c *gin.Context) {
	// Detect content type
	contentType := strings.ToLower(strings.TrimSpace(c.GetHeader("Content-Type")))
	isCBOR := strings.Contains(contentType, "application/cbor")

	var body apiCallRequest

	// Parse request body based on content type
	if isCBOR {
		rawBody, errRead := io.ReadAll(c.Request.Body)
		if errRead != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "failed to read request body"})
			return
		}
		if errUnmarshal := cbor.Unmarshal(rawBody, &body); errUnmarshal != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid cbor body"})
			return
		}
	} else {
		if errBindJSON := c.ShouldBindJSON(&body); errBindJSON != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
			return
		}
	}

	method := strings.ToUpper(strings.TrimSpace(body.Method))
	if method == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "missing method"})
		return
	}

	urlStr := strings.TrimSpace(body.URL)
	if urlStr == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "missing url"})
		return
	}
	parsedURL, errParseURL := url.Parse(urlStr)
	if errParseURL != nil || parsedURL.Scheme == "" || parsedURL.Host == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid url"})
		return
	}

	authIndex := firstNonEmptyString(body.AuthIndexSnake, body.AuthIndexCamel, body.AuthIndexPascal)
	auth := h.authByIndex(authIndex)

	// Farm supply-atomicity fail-closed gate (R5-3b): reject an api-call targeting
	// an enrolled-but-unprovisioned Claude account before any outbound request is
	// built or token resolved. The management api-call path issues arbitrary
	// authenticated requests (e.g. api.anthropic.com probes) carrying this
	// account's token; running one before the account is bound to a container
	// leaks the synthetic device_id identity. The predicate is nil-safe (auth may
	// be nil when no index matched) and a strict no-op when FARM_REQUIRE_PROVISIONED
	// is off / for non-enrolled accounts, matching the :201-207 token-missing 400s.
	if coreauth.RequireProvisionedBlocked(auth) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "farm account not provisioned: container fail-closed, refusing api-call before device binding"})
		return
	}

	reqHeaders := body.Header
	if reqHeaders == nil {
		reqHeaders = map[string]string{}
	}

	var hostOverride string
	var token string
	var tokenResolved bool
	var tokenErr error
	for key, value := range reqHeaders {
		if !strings.Contains(value, "$TOKEN$") {
			continue
		}
		if !tokenResolved {
			token, tokenErr = h.resolveTokenForAuth(c.Request.Context(), auth)
			tokenResolved = true
		}
		if auth != nil && token == "" {
			if tokenErr != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": "auth token refresh failed"})
				return
			}
			c.JSON(http.StatusBadRequest, gin.H{"error": "auth token not found"})
			return
		}
		if token == "" {
			continue
		}
		reqHeaders[key] = strings.ReplaceAll(value, "$TOKEN$", token)
	}

	// When caller indicates CBOR in request headers, convert JSON string payload to CBOR bytes.
	useCBORPayload := headerContainsValue(reqHeaders, "Content-Type", "application/cbor")

	var requestBody io.Reader
	if body.Data != "" {
		if useCBORPayload {
			cborPayload, errEncode := encodeJSONStringToCBOR(body.Data)
			if errEncode != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": "invalid json data for cbor content-type"})
				return
			}
			requestBody = bytes.NewReader(cborPayload)
		} else {
			requestBody = strings.NewReader(body.Data)
		}
	}

	req, errNewRequest := http.NewRequestWithContext(c.Request.Context(), method, urlStr, requestBody)
	if errNewRequest != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "failed to build request"})
		return
	}

	for key, value := range reqHeaders {
		if strings.EqualFold(key, "host") {
			hostOverride = strings.TrimSpace(value)
			continue
		}
		req.Header.Set(key, value)
	}
	if hostOverride != "" {
		req.Host = hostOverride
	}

	httpClient := &http.Client{
		Timeout: defaultAPICallTimeout,
	}
	httpClient.Transport = h.apiCallTransport(auth)

	resp, errDo := httpClient.Do(req)
	if errDo != nil {
		failureKind := classifyAPICallError(errDo)
		log.WithError(errDo).WithFields(log.Fields{
			"method":       method,
			"url":          urlStr,
			"failure_kind": failureKind,
		}).Warn("management APICall request failed")
		c.JSON(http.StatusBadGateway, gin.H{
			"error":        "request failed",
			"failure_kind": failureKind,
		})
		return
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.Errorf("response body close error: %v", errClose)
		}
	}()

	respBody, errReadAll := io.ReadAll(resp.Body)
	if errReadAll != nil {
		failureKind := classifyAPICallError(errReadAll)
		log.WithError(errReadAll).WithFields(log.Fields{
			"method":       method,
			"url":          urlStr,
			"failure_kind": failureKind,
		}).Warn("management APICall response read failed")
		c.JSON(http.StatusBadGateway, gin.H{
			"error":        "failed to read response",
			"failure_kind": failureKind,
		})
		return
	}

	// For CBOR upstream responses, decode into plain text or JSON string before returning.
	responseBodyText := string(respBody)
	if headerContainsValue(reqHeaders, "Accept", "application/cbor") || strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "application/cbor") {
		if decodedBody, errDecode := decodeCBORBodyToTextOrJSON(respBody); errDecode == nil {
			responseBodyText = decodedBody
		}
	}

	response := apiCallResponse{
		StatusCode: resp.StatusCode,
		Header:     resp.Header,
		Body:       responseBodyText,
	}

	// If this is a GitHub Copilot token endpoint response, try to enrich with quota information
	if resp.StatusCode == http.StatusOK &&
		strings.Contains(urlStr, "copilot_internal") &&
		strings.Contains(urlStr, "/token") {
		response = h.enrichCopilotTokenResponse(c.Request.Context(), response, auth, urlStr)
	}

	// Return response in the same format as the request
	if isCBOR {
		cborData, errMarshal := cbor.Marshal(response)
		if errMarshal != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to encode cbor response"})
			return
		}
		c.Data(http.StatusOK, "application/cbor", cborData)
	} else {
		c.JSON(http.StatusOK, response)
	}
}

func firstNonEmptyString(values ...*string) string {
	for _, v := range values {
		if v == nil {
			continue
		}
		if out := strings.TrimSpace(*v); out != "" {
			return out
		}
	}
	return ""
}

func tokenValueForAuth(auth *coreauth.Auth) string {
	if auth == nil {
		return ""
	}
	if v := tokenValueFromMetadata(auth.Metadata); v != "" {
		return v
	}
	if auth.Attributes != nil {
		if v := strings.TrimSpace(auth.Attributes["api_key"]); v != "" {
			return v
		}
	}
	if shared := geminicli.ResolveSharedCredential(auth.Runtime); shared != nil {
		if v := tokenValueFromMetadata(shared.MetadataSnapshot()); v != "" {
			return v
		}
	}
	return ""
}

func (h *Handler) resolveTokenForAuth(ctx context.Context, auth *coreauth.Auth) (string, error) {
	if auth == nil {
		return "", nil
	}

	provider := strings.ToLower(strings.TrimSpace(auth.Provider))
	if provider == "gemini-cli" {
		token, errToken := h.refreshGeminiOAuthAccessToken(ctx, auth)
		return token, errToken
	}
	if provider == "antigravity" {
		token, errToken := h.refreshAntigravityOAuthAccessToken(ctx, auth)
		return token, errToken
	}

	return tokenValueForAuth(auth), nil
}

func (h *Handler) refreshGeminiOAuthAccessToken(ctx context.Context, auth *coreauth.Auth) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if auth == nil {
		return "", nil
	}

	metadata, updater := geminiOAuthMetadata(auth)
	if len(metadata) == 0 {
		return "", fmt.Errorf("gemini oauth metadata missing")
	}

	base := make(map[string]any)
	if tokenRaw, ok := metadata["token"].(map[string]any); ok && tokenRaw != nil {
		base = cloneMap(tokenRaw)
	}

	var token oauth2.Token
	if len(base) > 0 {
		if raw, errMarshal := json.Marshal(base); errMarshal == nil {
			_ = json.Unmarshal(raw, &token)
		}
	}

	if token.AccessToken == "" {
		token.AccessToken = stringValue(metadata, "access_token")
	}
	if token.RefreshToken == "" {
		token.RefreshToken = stringValue(metadata, "refresh_token")
	}
	if token.TokenType == "" {
		token.TokenType = stringValue(metadata, "token_type")
	}
	if token.Expiry.IsZero() {
		if expiry := stringValue(metadata, "expiry"); expiry != "" {
			if ts, errParseTime := time.Parse(time.RFC3339, expiry); errParseTime == nil {
				token.Expiry = ts
			}
		}
	}

	conf := &oauth2.Config{
		ClientID:     geminiOAuthClientID,
		ClientSecret: geminiOAuthClientSecret,
		Scopes:       geminiOAuthScopes,
		Endpoint:     google.Endpoint,
	}

	ctxToken := ctx
	httpClient := &http.Client{
		Timeout:   defaultAPICallTimeout,
		Transport: h.apiCallTransport(auth),
	}
	ctxToken = context.WithValue(ctxToken, oauth2.HTTPClient, httpClient)

	src := conf.TokenSource(ctxToken, &token)
	currentToken, errToken := src.Token()
	if errToken != nil {
		return "", errToken
	}

	merged := buildOAuthTokenMap(base, currentToken)
	fields := buildOAuthTokenFields(currentToken, merged)
	if updater != nil {
		updater(fields)
	}
	return strings.TrimSpace(currentToken.AccessToken), nil
}

func (h *Handler) refreshAntigravityOAuthAccessToken(ctx context.Context, auth *coreauth.Auth) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if auth == nil {
		return "", nil
	}

	metadata := auth.Metadata
	if len(metadata) == 0 {
		return "", fmt.Errorf("antigravity oauth metadata missing")
	}

	current := strings.TrimSpace(tokenValueFromMetadata(metadata))
	if current != "" && !antigravityTokenNeedsRefresh(metadata) {
		return current, nil
	}

	refreshToken := stringValue(metadata, "refresh_token")
	if refreshToken == "" {
		return "", fmt.Errorf("antigravity refresh token missing")
	}

	tokenURL := strings.TrimSpace(antigravityOAuthTokenURL)
	if tokenURL == "" {
		tokenURL = "https://oauth2.googleapis.com/token"
	}
	form := url.Values{}
	form.Set("client_id", antigravityOAuthClientID)
	form.Set("client_secret", antigravityOAuthClientSecret)
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", refreshToken)

	req, errReq := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(form.Encode()))
	if errReq != nil {
		return "", errReq
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	httpClient := &http.Client{
		Timeout:   defaultAPICallTimeout,
		Transport: h.apiCallTransport(auth),
	}
	resp, errDo := httpClient.Do(req)
	if errDo != nil {
		return "", errDo
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.Errorf("response body close error: %v", errClose)
		}
	}()

	bodyBytes, errRead := io.ReadAll(resp.Body)
	if errRead != nil {
		return "", errRead
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return "", fmt.Errorf("antigravity oauth token refresh failed: status %d: %s", resp.StatusCode, strings.TrimSpace(string(bodyBytes)))
	}

	var tokenResp struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
		TokenType    string `json:"token_type"`
	}
	if errUnmarshal := json.Unmarshal(bodyBytes, &tokenResp); errUnmarshal != nil {
		return "", errUnmarshal
	}

	if strings.TrimSpace(tokenResp.AccessToken) == "" {
		return "", fmt.Errorf("antigravity oauth token refresh returned empty access_token")
	}

	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	now := time.Now()
	auth.Metadata["access_token"] = strings.TrimSpace(tokenResp.AccessToken)
	if strings.TrimSpace(tokenResp.RefreshToken) != "" {
		auth.Metadata["refresh_token"] = strings.TrimSpace(tokenResp.RefreshToken)
	}
	if tokenResp.ExpiresIn > 0 {
		auth.Metadata["expires_in"] = tokenResp.ExpiresIn
		auth.Metadata["timestamp"] = now.UnixMilli()
		auth.Metadata["expired"] = now.Add(time.Duration(tokenResp.ExpiresIn) * time.Second).Format(time.RFC3339)
	}
	auth.Metadata["type"] = "antigravity"

	if h != nil && h.authManager != nil {
		auth.LastRefreshedAt = now
		auth.UpdatedAt = now
		_, _ = h.authManager.Update(ctx, auth)
	}

	return strings.TrimSpace(tokenResp.AccessToken), nil
}

func antigravityTokenNeedsRefresh(metadata map[string]any) bool {
	// Refresh a bit early to avoid requests racing token expiry.
	const skew = 30 * time.Second

	if metadata == nil {
		return true
	}
	if expStr, ok := metadata["expired"].(string); ok {
		if ts, errParse := time.Parse(time.RFC3339, strings.TrimSpace(expStr)); errParse == nil {
			return !ts.After(time.Now().Add(skew))
		}
	}
	expiresIn := int64Value(metadata["expires_in"])
	timestampMs := int64Value(metadata["timestamp"])
	if expiresIn > 0 && timestampMs > 0 {
		exp := time.UnixMilli(timestampMs).Add(time.Duration(expiresIn) * time.Second)
		return !exp.After(time.Now().Add(skew))
	}
	return true
}

func int64Value(raw any) int64 {
	switch typed := raw.(type) {
	case int:
		return int64(typed)
	case int32:
		return int64(typed)
	case int64:
		return typed
	case uint:
		return int64(typed)
	case uint32:
		return int64(typed)
	case uint64:
		if typed > uint64(^uint64(0)>>1) {
			return 0
		}
		return int64(typed)
	case float32:
		return int64(typed)
	case float64:
		return int64(typed)
	case json.Number:
		if i, errParse := typed.Int64(); errParse == nil {
			return i
		}
	case string:
		if s := strings.TrimSpace(typed); s != "" {
			if i, errParse := json.Number(s).Int64(); errParse == nil {
				return i
			}
		}
	}
	return 0
}

func geminiOAuthMetadata(auth *coreauth.Auth) (map[string]any, func(map[string]any)) {
	if auth == nil {
		return nil, nil
	}
	if shared := geminicli.ResolveSharedCredential(auth.Runtime); shared != nil {
		snapshot := shared.MetadataSnapshot()
		return snapshot, func(fields map[string]any) { shared.MergeMetadata(fields) }
	}
	return auth.Metadata, func(fields map[string]any) {
		if auth.Metadata == nil {
			auth.Metadata = make(map[string]any)
		}
		for k, v := range fields {
			auth.Metadata[k] = v
		}
	}
}

func stringValue(metadata map[string]any, key string) string {
	if len(metadata) == 0 || key == "" {
		return ""
	}
	if v, ok := metadata[key].(string); ok {
		return strings.TrimSpace(v)
	}
	return ""
}

func cloneMap(in map[string]any) map[string]any {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func buildOAuthTokenMap(base map[string]any, tok *oauth2.Token) map[string]any {
	merged := cloneMap(base)
	if merged == nil {
		merged = make(map[string]any)
	}
	if tok == nil {
		return merged
	}
	if raw, errMarshal := json.Marshal(tok); errMarshal == nil {
		var tokenMap map[string]any
		if errUnmarshal := json.Unmarshal(raw, &tokenMap); errUnmarshal == nil {
			for k, v := range tokenMap {
				merged[k] = v
			}
		}
	}
	return merged
}

func buildOAuthTokenFields(tok *oauth2.Token, merged map[string]any) map[string]any {
	fields := make(map[string]any, 5)
	if tok != nil && tok.AccessToken != "" {
		fields["access_token"] = tok.AccessToken
	}
	if tok != nil && tok.TokenType != "" {
		fields["token_type"] = tok.TokenType
	}
	if tok != nil && tok.RefreshToken != "" {
		fields["refresh_token"] = tok.RefreshToken
	}
	if tok != nil && !tok.Expiry.IsZero() {
		fields["expiry"] = tok.Expiry.Format(time.RFC3339)
	}
	if len(merged) > 0 {
		fields["token"] = cloneMap(merged)
	}
	return fields
}

func tokenValueFromMetadata(metadata map[string]any) string {
	if len(metadata) == 0 {
		return ""
	}
	if v, ok := metadata["accessToken"].(string); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	if v, ok := metadata["access_token"].(string); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	if tokenRaw, ok := metadata["token"]; ok && tokenRaw != nil {
		switch typed := tokenRaw.(type) {
		case string:
			if v := strings.TrimSpace(typed); v != "" {
				return v
			}
		case map[string]any:
			if v, ok := typed["access_token"].(string); ok && strings.TrimSpace(v) != "" {
				return strings.TrimSpace(v)
			}
			if v, ok := typed["accessToken"].(string); ok && strings.TrimSpace(v) != "" {
				return strings.TrimSpace(v)
			}
		case map[string]string:
			if v := strings.TrimSpace(typed["access_token"]); v != "" {
				return v
			}
			if v := strings.TrimSpace(typed["accessToken"]); v != "" {
				return v
			}
		}
	}
	if v, ok := metadata["token"].(string); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	if v, ok := metadata["id_token"].(string); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	if v, ok := metadata["cookie"].(string); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	return ""
}

func (h *Handler) authByIndex(authIndex string) *coreauth.Auth {
	authIndex = strings.TrimSpace(authIndex)
	if authIndex == "" || h == nil || h.authManager == nil {
		return nil
	}
	auths := h.authManager.List()
	for _, auth := range auths {
		if auth == nil {
			continue
		}
		auth.EnsureIndex()
		if auth.Index == authIndex {
			return auth
		}
	}
	return nil
}

// apiCallTransport returns the outbound RoundTripper used by every management
// APICall (and its OAuth/quota helpers). For claude / codex accounts it routes
// through the SAME per-provider uTLS round tripper that serving and OAuth refresh
// use (helps.NewUtlsRoundTripperForProfile with the provider's ClientHello profile),
// so an account's api-call traffic presents the replicated claude-cli / codex-rs
// ClientHello instead of the
// Go-default TLS fingerprint (JA3 03117a8e). This closes the anti-correlation leak
// where the management dashboard's periodic chatgpt.com / api.anthropic.com usage
// probes carried a real account token over a distinguishable Go TLS stack.
//
// The uTLS round tripper is strict (no-downgrade) for both production profiles and
// handles its own proxy dialing (incl. socks5) and dial timeout, so it bypasses the
// hardenedDialer / socks5 special-casing on the standard-library path. For every
// other provider (gemini, copilot/github, api_key, openai-compatibility) or when
// auth is nil, apiCallUtlsRoundTripper declines and we fall back to the
// existing hardened standard-library transport (which keeps the socks5 DialContext
// handling and the AGENTS.md-allowed timeout exception).
func (h *Handler) apiCallTransport(auth *coreauth.Auth) http.RoundTripper {
	if rt, ok := h.apiCallUtlsRoundTripper(auth); ok && rt != nil {
		return rt
	}
	transport, _ := h.apiCallTransportWithDialer(auth)
	return transport
}

// apiCallUtlsRoundTripper resolves the serving-grade per-provider uTLS round
// tripper for an account whose provider is runtime-enforced (claude / codex). It
// reuses the SAME proxy-candidate priority as apiCallTransportWithDialer
// (auth.ProxyURL -> per-account api-key proxy -> global cfg.ProxyURL) and passes
// the resolved proxy to helps so helps does not re-resolve a different proxy.
// Returns ok=false for non-uTLS providers / nil auth so the caller keeps the
// existing hardened standard-library transport.
func (h *Handler) apiCallUtlsRoundTripper(auth *coreauth.Auth) (http.RoundTripper, bool) {
	if auth == nil {
		return nil, false
	}
	// Select the per-provider ClientHello profile and route through the SAME real
	// uTLS constructor serving/refresh use. Do NOT use BuildRuntimeTransportRoundTripper:
	// its codex branch returns NewCodexTransportRoundTripperForProfile, which ignores
	// the profile and clones a Go-default *http.Transport (NOT uTLS), so codex would
	// keep leaking JA3 03117a8e. NewUtlsRoundTripperForProfile resolves the codex
	// profile to a strict (no-downgrade) HelloCustom codex-rs round tripper, matching
	// codex_executor.go's NewUtlsHTTPClientForProfile(CodexRustlsClientHelloProfileID).
	// Only claude / codex have a serving uTLS fingerprint that api-call must match;
	// unrelated providers (gemini cli-native, copilot, api_key) keep today's transport.
	var profileID string
	switch strings.ToLower(strings.TrimSpace(auth.Provider)) {
	case "claude":
		profileID = helps.ClaudeCLIClientHelloProfileID
	case "codex":
		profileID = helps.CodexRustlsClientHelloProfileID
	default:
		return nil, false
	}
	proxyURL := h.apiCallResolvedProxyURL(auth)
	rt := helps.NewUtlsRoundTripperForProfile(proxyURL, profileID)
	if rt == nil {
		return nil, false
	}
	return rt, true
}

// apiCallResolvedProxyURL computes the same proxy-candidate priority chain used by
// apiCallTransportWithDialer and returns the first non-empty candidate (empty means
// direct egress, preserving api-call's documented "Direct connect" priority-3
// behavior). Factored out so the uTLS and standard-library paths resolve identical
// proxies.
func (h *Handler) apiCallResolvedProxyURL(auth *coreauth.Auth) string {
	if auth != nil {
		if proxyStr := strings.TrimSpace(auth.ProxyURL); proxyStr != "" {
			return proxyStr
		}
		if h != nil && h.cfg != nil {
			if proxyStr := strings.TrimSpace(proxyURLFromAPIKeyConfig(h.cfg, auth)); proxyStr != "" {
				return proxyStr
			}
		}
	}
	if h != nil && h.cfg != nil {
		if proxyStr := strings.TrimSpace(h.cfg.ProxyURL); proxyStr != "" {
			return proxyStr
		}
	}
	return ""
}

// apiCallTransportWithDialer returns the configured outbound transport along
// with the *net.Dialer that drives its dial phase (when applicable). The
// dialer is returned so tests can directly assert that the 10s dial timeout
// is in effect. For SOCKS5 transports the inner dial is delegated to
// proxyutil and we wrap it with a context-bound timeout instead of installing
// a fresh net.Dialer; in that case the returned dialer reflects the
// context-timeout used to bound the SOCKS5 connect, so its Timeout field
// still equals apiCallDialTimeout.
func (h *Handler) apiCallTransportWithDialer(auth *coreauth.Auth) (*http.Transport, *net.Dialer) {
	var proxyCandidates []string
	if auth != nil {
		if proxyStr := strings.TrimSpace(auth.ProxyURL); proxyStr != "" {
			proxyCandidates = append(proxyCandidates, proxyStr)
		}
		if h != nil && h.cfg != nil {
			if proxyStr := strings.TrimSpace(proxyURLFromAPIKeyConfig(h.cfg, auth)); proxyStr != "" {
				proxyCandidates = append(proxyCandidates, proxyStr)
			}
		}
	}
	if h != nil && h.cfg != nil {
		if proxyStr := strings.TrimSpace(h.cfg.ProxyURL); proxyStr != "" {
			proxyCandidates = append(proxyCandidates, proxyStr)
		}
	}

	for _, proxyStr := range proxyCandidates {
		transport, scheme := buildProxyTransport(proxyStr)
		if transport == nil {
			continue
		}
		return hardenAPICallTransport(transport, scheme)
	}

	transport, ok := http.DefaultTransport.(*http.Transport)
	if !ok || transport == nil {
		return hardenAPICallTransport(&http.Transport{Proxy: nil}, "")
	}
	clone := transport.Clone()
	clone.Proxy = nil
	return hardenAPICallTransport(clone, "")
}

// hardenedDialContext is the function-pointer type we use as a sentinel to
// detect whether a transport has already been hardened by this package. We
// rely on equality between function values being unreliable across closures,
// so instead we install a small wrapper closure built from this constructor
// and recognise it via the dialer it owns.
type hardenedDialer struct {
	dialer *net.Dialer
	// wrapped is the original DialContext we replaced (only used for SOCKS5
	// where we must keep proxyutil's inner dial logic intact).
	wrapped func(ctx context.Context, network, addr string) (net.Conn, error)
}

// dialContext is the actual http.Transport DialContext function value. We
// install this closure unconditionally on every harden call; the *net.Dialer
// owned by this hardenedDialer is the authoritative timeout source, so
// tests can pull it out via transportDialerFromContextKey-style indirection
// (we expose it through apiCallTransportWithDialer instead of leaking
// reflection into the test surface).
func (hd *hardenedDialer) dialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	if hd.wrapped != nil {
		// SOCKS5 path: bound the inner proxy dial with our dial timeout so a
		// stalled SOCKS5 connect cannot exceed apiCallDialTimeout. Using
		// context.WithTimeout preserves any tighter caller-supplied deadline.
		dialCtx, cancel := context.WithTimeout(ctx, hd.dialer.Timeout)
		defer cancel()
		return hd.wrapped(dialCtx, network, addr)
	}
	// Direct / HTTP-proxy path: use the freshly-built dialer so its 10s
	// Timeout field is the authoritative bound.
	return hd.dialer.DialContext(ctx, network, addr)
}

// hardenAPICallTransport tightens the dial / TLS / response-header deadlines
// on the outbound transport used by management APICall. The default
// http.DefaultTransport DialContext is built from a 30s net.Dialer which
// exactly matches a typical reverse-proxy idle budget; when the outbound
// proxy chain stalls during connect, the local server returns 502 instead of
// failing fast with a structured error.
//
// Implementation notes:
//   - For direct / HTTP-proxy transports the underlying DialContext is the
//     stdlib 30s dialer; we *unconditionally* replace it with a fresh 10s
//     net.Dialer. We do NOT guard on `if transport.DialContext == nil`,
//     because http.DefaultTransport.Clone() already populates DialContext
//     and that guard would silently skip the harden step (this is the bug
//     that the first attempt shipped with).
//   - For SOCKS5 transports built by proxyutil the DialContext is a closure
//     that owns the inner SOCKS5 dialer (including auth flow). We must not
//     drop that closure, so we wrap it with context.WithTimeout sized to
//     apiCallDialTimeout. Replacing the closure would discard the SOCKS5
//     auth path.
//   - Idempotency: re-applying this function is safe. Each call installs a
//     fresh hardenedDialer wrapper; the previous wrapper (if any) is
//     discarded. Because the harden function is only invoked from the
//     apiCallTransport construction path and apiCallTransport is called per
//     request, re-application happens at construction time, not per dial.
//   - stage timeouts (TLS handshake / response header / etc.) are set
//     unconditionally to the project-chosen values. The cloned default
//     transport's defaults are either already equal (TLSHandshakeTimeout=10s)
//     or unset; forcing the value ensures uniform behaviour across all
//     proxy paths.
func hardenAPICallTransport(transport *http.Transport, proxyScheme string) (*http.Transport, *net.Dialer) {
	if transport == nil {
		return nil, nil
	}

	dialer := &net.Dialer{
		Timeout:   apiCallDialTimeout,
		KeepAlive: apiCallKeepAliveProbeInterval,
	}

	// Distinguish SOCKS5 (proxyutil installs a custom DialContext closure that
	// must be preserved) from direct / HTTP-proxy (stdlib dialer.DialContext).
	hd := &hardenedDialer{dialer: dialer}
	scheme := strings.ToLower(strings.TrimSpace(proxyScheme))
	if scheme == "socks5" || scheme == "socks5h" {
		hd.wrapped = transport.DialContext
	}
	transport.DialContext = hd.dialContext

	transport.TLSHandshakeTimeout = apiCallTLSHandshakeTimeout
	transport.ResponseHeaderTimeout = apiCallResponseHeaderTimeout
	transport.ExpectContinueTimeout = apiCallExpectContinueTimeout
	transport.IdleConnTimeout = apiCallIdleConnectionTimeout
	return transport, dialer
}

// MERGE-REVIEW: fork-only APICall transport hardening (hardenedDialer / hardenAPICallTransport /
// classifyAPICallError) retained; upstream had no counterpart here. This is an allowed intentional
// timeout exception per AGENTS.md (management APICall timeout in api_tools.go).
// classifyAPICallError maps an outbound transport error returned from
// httpClient.Do into a stable label useful for both structured logging and
// HTTP responses. The handler used to collapse every failure into a generic
// 502 "request failed" which made it impossible to tell whether a stall
// happened during DNS, dial, TLS or response-header phases.
//
// Ordering matters: errors.As walks the wrap chain, so a *net.OpError that
// wraps a *net.DNSError will match both net.Error and *net.DNSError. DNS
// errors must be checked before the generic net.Error timeout branch so we
// don't mislabel a "DNS i/o timeout" as a plain "network_timeout".
func classifyAPICallError(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "context_deadline_exceeded"
	}
	if errors.Is(err, context.Canceled) {
		return "context_canceled"
	}
	// DNS errors (including DNS i/o timeout) must be detected BEFORE the
	// generic net.Error timeout branch — otherwise a wrapped DNS timeout is
	// labelled "network_timeout" and the dns_error signal is lost.
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return "dns_error"
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return "network_timeout"
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return "network_op_error:" + opErr.Op
	}
	return "transport_error"
}

type apiKeyConfigEntry interface {
	GetAPIKey() string
	GetBaseURL() string
}

func resolveAPIKeyConfig[T apiKeyConfigEntry](entries []T, auth *coreauth.Auth) *T {
	if auth == nil || len(entries) == 0 {
		return nil
	}
	attrKey, attrBase := "", ""
	if auth.Attributes != nil {
		attrKey = strings.TrimSpace(auth.Attributes["api_key"])
		attrBase = strings.TrimSpace(auth.Attributes["base_url"])
	}
	for i := range entries {
		entry := &entries[i]
		cfgKey := strings.TrimSpace((*entry).GetAPIKey())
		cfgBase := strings.TrimSpace((*entry).GetBaseURL())
		if attrKey != "" && attrBase != "" {
			if strings.EqualFold(cfgKey, attrKey) && strings.EqualFold(cfgBase, attrBase) {
				return entry
			}
			continue
		}
		if attrKey != "" && strings.EqualFold(cfgKey, attrKey) {
			if cfgBase == "" || strings.EqualFold(cfgBase, attrBase) {
				return entry
			}
		}
		if attrKey == "" && attrBase != "" && strings.EqualFold(cfgBase, attrBase) {
			return entry
		}
	}
	if attrKey != "" {
		for i := range entries {
			entry := &entries[i]
			if strings.EqualFold(strings.TrimSpace((*entry).GetAPIKey()), attrKey) {
				return entry
			}
		}
	}
	return nil
}

func proxyURLFromAPIKeyConfig(cfg *config.Config, auth *coreauth.Auth) string {
	if cfg == nil || auth == nil {
		return ""
	}
	authKind, authAccount := auth.AccountInfo()
	if !strings.EqualFold(strings.TrimSpace(authKind), "api_key") {
		return ""
	}

	attrs := auth.Attributes
	compatName := ""
	providerKey := ""
	if len(attrs) > 0 {
		compatName = strings.TrimSpace(attrs["compat_name"])
		providerKey = strings.TrimSpace(attrs["provider_key"])
	}
	if compatName != "" || strings.EqualFold(strings.TrimSpace(auth.Provider), "openai-compatibility") {
		return resolveOpenAICompatAPIKeyProxyURL(cfg, auth, strings.TrimSpace(authAccount), providerKey, compatName)
	}

	switch strings.ToLower(strings.TrimSpace(auth.Provider)) {
	case "gemini":
		if entry := resolveAPIKeyConfig(cfg.GeminiKey, auth); entry != nil {
			return strings.TrimSpace(entry.ProxyURL)
		}
	case "gemini-interactions":
		// Absorbed from upstream v7.2.101: native Google Interactions API keys carry
		// their own per-account proxy just like the other providers.
		if entry := resolveAPIKeyConfig(cfg.InteractionsKey, auth); entry != nil {
			return strings.TrimSpace(entry.ProxyURL)
		}
	case "claude":
		if entry := resolveAPIKeyConfig(cfg.ClaudeKey, auth); entry != nil {
			return strings.TrimSpace(entry.ProxyURL)
		}
	case "codex":
		if entry := resolveAPIKeyConfig(cfg.CodexKey, auth); entry != nil {
			return strings.TrimSpace(entry.ProxyURL)
		}
	case "xai":
		// Absorbed from upstream v7.2.101: xAI API keys resolve their per-account
		// proxy from cfg.XAIKey before falling back to the global proxy.
		if entry := resolveAPIKeyConfig(cfg.XAIKey, auth); entry != nil {
			return strings.TrimSpace(entry.ProxyURL)
		}
	}
	return ""
}

func resolveOpenAICompatAPIKeyProxyURL(cfg *config.Config, auth *coreauth.Auth, apiKey, providerKey, compatName string) string {
	if cfg == nil || auth == nil {
		return ""
	}
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		return ""
	}
	candidates := make([]string, 0, 3)
	if v := strings.TrimSpace(compatName); v != "" {
		candidates = append(candidates, v)
	}
	if v := strings.TrimSpace(providerKey); v != "" {
		candidates = append(candidates, v)
	}
	if v := strings.TrimSpace(auth.Provider); v != "" {
		candidates = append(candidates, v)
	}

	for i := range cfg.OpenAICompatibility {
		compat := &cfg.OpenAICompatibility[i]
		if compat.Disabled {
			continue
		}
		for _, candidate := range candidates {
			if candidate != "" && strings.EqualFold(strings.TrimSpace(candidate), compat.Name) {
				for j := range compat.APIKeyEntries {
					entry := &compat.APIKeyEntries[j]
					if strings.EqualFold(strings.TrimSpace(entry.APIKey), apiKey) {
						return strings.TrimSpace(entry.ProxyURL)
					}
				}
				return ""
			}
		}
	}
	return ""
}

// buildProxyTransport returns the configured proxy transport along with the
// proxy URL scheme (e.g. "socks5", "https"). The scheme is required by
// hardenAPICallTransport so it can distinguish a SOCKS5 transport (whose
// inner DialContext closure must be preserved and merely bounded with a
// context timeout) from a direct or HTTP proxy transport (whose stdlib
// 30s DialContext must be replaced outright by a 10s net.Dialer).
func buildProxyTransport(proxyStr string) (*http.Transport, string) {
	setting, errParse := proxyutil.Parse(proxyStr)
	if errParse != nil {
		log.WithError(errParse).Debug("parse proxy setting failed")
		return nil, ""
	}
	transport, _, errBuild := proxyutil.BuildHTTPTransport(proxyStr)
	if errBuild != nil {
		log.WithError(errBuild).Debug("build proxy transport failed")
		return nil, ""
	}
	if transport == nil {
		return nil, ""
	}
	scheme := ""
	if setting.URL != nil {
		scheme = setting.URL.Scheme
	}
	return transport, scheme
}

// headerContainsValue checks whether a header map contains a target value (case-insensitive key and value).
func headerContainsValue(headers map[string]string, targetKey, targetValue string) bool {
	if len(headers) == 0 {
		return false
	}
	for key, value := range headers {
		if !strings.EqualFold(strings.TrimSpace(key), strings.TrimSpace(targetKey)) {
			continue
		}
		if strings.Contains(strings.ToLower(value), strings.ToLower(strings.TrimSpace(targetValue))) {
			return true
		}
	}
	return false
}

// encodeJSONStringToCBOR converts a JSON string payload into CBOR bytes.
func encodeJSONStringToCBOR(jsonString string) ([]byte, error) {
	var payload any
	if errUnmarshal := json.Unmarshal([]byte(jsonString), &payload); errUnmarshal != nil {
		return nil, errUnmarshal
	}
	return cbor.Marshal(payload)
}

// decodeCBORBodyToTextOrJSON decodes CBOR bytes to plain text (for string payloads) or JSON string.
func decodeCBORBodyToTextOrJSON(raw []byte) (string, error) {
	if len(raw) == 0 {
		return "", nil
	}

	var payload any
	if errUnmarshal := cbor.Unmarshal(raw, &payload); errUnmarshal != nil {
		return "", errUnmarshal
	}

	jsonCompatible := cborValueToJSONCompatible(payload)
	switch typed := jsonCompatible.(type) {
	case string:
		return typed, nil
	case []byte:
		return string(typed), nil
	default:
		jsonBytes, errMarshal := json.Marshal(jsonCompatible)
		if errMarshal != nil {
			return "", errMarshal
		}
		return string(jsonBytes), nil
	}
}

// cborValueToJSONCompatible recursively converts CBOR-decoded values into JSON-marshalable values.
func cborValueToJSONCompatible(value any) any {
	switch typed := value.(type) {
	case map[any]any:
		out := make(map[string]any, len(typed))
		for key, item := range typed {
			out[fmt.Sprint(key)] = cborValueToJSONCompatible(item)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(typed))
		for key, item := range typed {
			out[key] = cborValueToJSONCompatible(item)
		}
		return out
	case []any:
		out := make([]any, len(typed))
		for i, item := range typed {
			out[i] = cborValueToJSONCompatible(item)
		}
		return out
	default:
		return typed
	}
}

// QuotaDetail represents quota information for a specific resource type
type QuotaDetail struct {
	Entitlement      float64 `json:"entitlement"`
	OverageCount     float64 `json:"overage_count"`
	OveragePermitted bool    `json:"overage_permitted"`
	PercentRemaining float64 `json:"percent_remaining"`
	QuotaID          string  `json:"quota_id"`
	QuotaRemaining   float64 `json:"quota_remaining"`
	Remaining        float64 `json:"remaining"`
	Unlimited        bool    `json:"unlimited"`
}

// QuotaSnapshots contains quota details for different resource types
type QuotaSnapshots struct {
	Chat                QuotaDetail `json:"chat"`
	Completions         QuotaDetail `json:"completions"`
	PremiumInteractions QuotaDetail `json:"premium_interactions"`
}

// CopilotUsageResponse represents the GitHub Copilot usage information
type CopilotUsageResponse struct {
	AccessTypeSKU         string         `json:"access_type_sku"`
	AnalyticsTrackingID   string         `json:"analytics_tracking_id"`
	AssignedDate          string         `json:"assigned_date"`
	CanSignupForLimited   bool           `json:"can_signup_for_limited"`
	ChatEnabled           bool           `json:"chat_enabled"`
	CopilotPlan           string         `json:"copilot_plan"`
	OrganizationLoginList []interface{}  `json:"organization_login_list"`
	OrganizationList      []interface{}  `json:"organization_list"`
	QuotaResetDate        string         `json:"quota_reset_date"`
	QuotaSnapshots        QuotaSnapshots `json:"quota_snapshots"`
}

type copilotQuotaRequest struct {
	AuthIndexSnake  *string `json:"auth_index"`
	AuthIndexCamel  *string `json:"authIndex"`
	AuthIndexPascal *string `json:"AuthIndex"`
}

// GetCopilotQuota fetches GitHub Copilot quota information from the /copilot_internal/user endpoint.
//
// Endpoint:
//
//	GET /v0/management/copilot-quota
//
// Query Parameters (optional):
//   - auth_index: The credential "auth_index" from GET /v0/management/auth-files.
//     If omitted, uses the first available GitHub Copilot credential.
//
// Response:
//
//	Returns the CopilotUsageResponse with quota_snapshots containing detailed quota information
//	for chat, completions, and premium_interactions.
//
// Example:
//
//	curl -sS -X GET "http://127.0.0.1:8317/v0/management/copilot-quota?auth_index=<AUTH_INDEX>" \
//	  -H "Authorization: Bearer <MANAGEMENT_KEY>"
func (h *Handler) GetCopilotQuota(c *gin.Context) {
	authIndex := strings.TrimSpace(c.Query("auth_index"))
	if authIndex == "" {
		authIndex = strings.TrimSpace(c.Query("authIndex"))
	}
	if authIndex == "" {
		authIndex = strings.TrimSpace(c.Query("AuthIndex"))
	}

	auth := h.findCopilotAuth(authIndex)
	if auth == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "no github copilot credential found"})
		return
	}

	token, tokenErr := h.resolveTokenForAuth(c.Request.Context(), auth)
	if tokenErr != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "failed to refresh copilot token"})
		return
	}
	if token == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "copilot token not found"})
		return
	}

	apiURL := "https://api.github.com/copilot_internal/user"
	req, errNewRequest := http.NewRequestWithContext(c.Request.Context(), http.MethodGet, apiURL, nil)
	if errNewRequest != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to build request"})
		return
	}

	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("User-Agent", "CLIProxyAPIPlus")
	req.Header.Set("Accept", "application/json")

	httpClient := &http.Client{
		Timeout:   defaultAPICallTimeout,
		Transport: h.apiCallTransport(auth),
	}

	resp, errDo := httpClient.Do(req)
	if errDo != nil {
		failureKind := classifyAPICallError(errDo)
		log.WithError(errDo).WithFields(log.Fields{
			"url":          apiURL,
			"failure_kind": failureKind,
		}).Warn("copilot quota request failed")
		c.JSON(http.StatusBadGateway, gin.H{
			"error":        "request failed",
			"failure_kind": failureKind,
		})
		return
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.Errorf("response body close error: %v", errClose)
		}
	}()

	respBody, errReadAll := io.ReadAll(resp.Body)
	if errReadAll != nil {
		failureKind := classifyAPICallError(errReadAll)
		log.WithError(errReadAll).WithFields(log.Fields{
			"url":          apiURL,
			"failure_kind": failureKind,
		}).Warn("copilot quota response read failed")
		c.JSON(http.StatusBadGateway, gin.H{
			"error":        "failed to read response",
			"failure_kind": failureKind,
		})
		return
	}

	if resp.StatusCode != http.StatusOK {
		c.JSON(http.StatusBadGateway, gin.H{
			"error":       "github api request failed",
			"status_code": resp.StatusCode,
			"body":        string(respBody),
		})
		return
	}

	var usage CopilotUsageResponse
	if errUnmarshal := json.Unmarshal(respBody, &usage); errUnmarshal != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to parse response"})
		return
	}

	c.JSON(http.StatusOK, usage)
}

// findCopilotAuth locates a GitHub Copilot credential by auth_index or returns the first available one
func (h *Handler) findCopilotAuth(authIndex string) *coreauth.Auth {
	if h == nil || h.authManager == nil {
		return nil
	}

	auths := h.authManager.List()
	var firstCopilot *coreauth.Auth

	for _, auth := range auths {
		if auth == nil {
			continue
		}

		provider := strings.ToLower(strings.TrimSpace(auth.Provider))
		if provider != "copilot" && provider != "github" && provider != "github-copilot" {
			continue
		}

		if firstCopilot == nil {
			firstCopilot = auth
		}

		if authIndex != "" {
			auth.EnsureIndex()
			if auth.Index == authIndex {
				return auth
			}
		}
	}

	return firstCopilot
}

// enrichCopilotTokenResponse fetches quota information and adds it to the Copilot token response body
func (h *Handler) enrichCopilotTokenResponse(ctx context.Context, response apiCallResponse, auth *coreauth.Auth, originalURL string) apiCallResponse {
	if auth == nil || response.Body == "" {
		return response
	}

	// Parse the token response to check if it's enterprise (null limited_user_quotas)
	var tokenResp map[string]interface{}
	if err := json.Unmarshal([]byte(response.Body), &tokenResp); err != nil {
		log.WithError(err).Debug("enrichCopilotTokenResponse: failed to parse copilot token response")
		return response
	}

	// Get the GitHub token to call the copilot_internal/user endpoint
	token, tokenErr := h.resolveTokenForAuth(ctx, auth)
	if tokenErr != nil {
		log.WithError(tokenErr).Debug("enrichCopilotTokenResponse: failed to resolve token")
		return response
	}
	if token == "" {
		return response
	}

	// Fetch quota information from /copilot_internal/user
	// Derive the base URL from the original token request to support proxies and test servers
	parsedURL, errParse := url.Parse(originalURL)
	if errParse != nil {
		log.WithError(errParse).Debug("enrichCopilotTokenResponse: failed to parse URL")
		return response
	}
	quotaURL := fmt.Sprintf("%s://%s/copilot_internal/user", parsedURL.Scheme, parsedURL.Host)

	req, errNewRequest := http.NewRequestWithContext(ctx, http.MethodGet, quotaURL, nil)
	if errNewRequest != nil {
		log.WithError(errNewRequest).Debug("enrichCopilotTokenResponse: failed to build request")
		return response
	}

	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("User-Agent", "CLIProxyAPIPlus")
	req.Header.Set("Accept", "application/json")

	httpClient := &http.Client{
		Timeout:   defaultAPICallTimeout,
		Transport: h.apiCallTransport(auth),
	}

	quotaResp, errDo := httpClient.Do(req)
	if errDo != nil {
		log.WithError(errDo).Debug("enrichCopilotTokenResponse: quota fetch HTTP request failed")
		return response
	}

	defer func() {
		if errClose := quotaResp.Body.Close(); errClose != nil {
			log.Errorf("quota response body close error: %v", errClose)
		}
	}()

	if quotaResp.StatusCode != http.StatusOK {
		return response
	}

	quotaBody, errReadAll := io.ReadAll(quotaResp.Body)
	if errReadAll != nil {
		log.WithError(errReadAll).Debug("enrichCopilotTokenResponse: failed to read response")
		return response
	}

	// Parse the quota response
	var quotaData CopilotUsageResponse
	if err := json.Unmarshal(quotaBody, &quotaData); err != nil {
		log.WithError(err).Debug("enrichCopilotTokenResponse: failed to parse response")
		return response
	}

	// Check if this is an enterprise account by looking for quota_snapshots in the response
	// Enterprise accounts have quota_snapshots, non-enterprise have limited_user_quotas
	var quotaRaw map[string]interface{}
	if err := json.Unmarshal(quotaBody, &quotaRaw); err == nil {
		if _, hasQuotaSnapshots := quotaRaw["quota_snapshots"]; hasQuotaSnapshots {
			// Enterprise account - has quota_snapshots
			tokenResp["quota_snapshots"] = quotaData.QuotaSnapshots
			tokenResp["access_type_sku"] = quotaData.AccessTypeSKU
			tokenResp["copilot_plan"] = quotaData.CopilotPlan

			// Add quota reset date for enterprise (quota_reset_date_utc)
			if quotaResetDateUTC, ok := quotaRaw["quota_reset_date_utc"]; ok {
				tokenResp["quota_reset_date"] = quotaResetDateUTC
			} else if quotaData.QuotaResetDate != "" {
				tokenResp["quota_reset_date"] = quotaData.QuotaResetDate
			}
		} else {
			// Non-enterprise account - build quota from limited_user_quotas and monthly_quotas
			var quotaSnapshots QuotaSnapshots

			// Get monthly quotas (total entitlement) and limited_user_quotas (remaining)
			monthlyQuotas, hasMonthly := quotaRaw["monthly_quotas"].(map[string]interface{})
			limitedQuotas, hasLimited := quotaRaw["limited_user_quotas"].(map[string]interface{})

			// Process chat quota
			if hasMonthly && hasLimited {
				if chatTotal, ok := monthlyQuotas["chat"].(float64); ok {
					chatRemaining := chatTotal // default to full if no limited quota
					if chatLimited, ok := limitedQuotas["chat"].(float64); ok {
						chatRemaining = chatLimited
					}
					percentRemaining := 0.0
					if chatTotal > 0 {
						percentRemaining = (chatRemaining / chatTotal) * 100.0
					}
					quotaSnapshots.Chat = QuotaDetail{
						Entitlement:      chatTotal,
						Remaining:        chatRemaining,
						QuotaRemaining:   chatRemaining,
						PercentRemaining: percentRemaining,
						QuotaID:          "chat",
						Unlimited:        false,
					}
				}

				// Process completions quota
				if completionsTotal, ok := monthlyQuotas["completions"].(float64); ok {
					completionsRemaining := completionsTotal // default to full if no limited quota
					if completionsLimited, ok := limitedQuotas["completions"].(float64); ok {
						completionsRemaining = completionsLimited
					}
					percentRemaining := 0.0
					if completionsTotal > 0 {
						percentRemaining = (completionsRemaining / completionsTotal) * 100.0
					}
					quotaSnapshots.Completions = QuotaDetail{
						Entitlement:      completionsTotal,
						Remaining:        completionsRemaining,
						QuotaRemaining:   completionsRemaining,
						PercentRemaining: percentRemaining,
						QuotaID:          "completions",
						Unlimited:        false,
					}
				}
			}

			// Premium interactions don't exist for non-enterprise, leave as zero values
			quotaSnapshots.PremiumInteractions = QuotaDetail{
				QuotaID:   "premium_interactions",
				Unlimited: false,
			}

			// Add quota_snapshots to the token response
			tokenResp["quota_snapshots"] = quotaSnapshots
			tokenResp["access_type_sku"] = quotaData.AccessTypeSKU
			tokenResp["copilot_plan"] = quotaData.CopilotPlan

			// Add quota reset date for non-enterprise (limited_user_reset_date)
			if limitedResetDate, ok := quotaRaw["limited_user_reset_date"]; ok {
				tokenResp["quota_reset_date"] = limitedResetDate
			}
		}
	}

	// Re-serialize the enriched response
	enrichedBody, errMarshal := json.Marshal(tokenResp)
	if errMarshal != nil {
		log.WithError(errMarshal).Debug("failed to marshal enriched response")
		return response
	}

	response.Body = string(enrichedBody)

	return response
}
