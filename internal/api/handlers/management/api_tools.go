package management

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/MoeForge/moego/json"
	"github.com/fxamacker/cbor/v2"
	"github.com/gin-gonic/gin"
	xaiauth "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/xai"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/proxyutil"
	"golang.org/x/net/http/httpguts"
)

const (
	defaultAPICallTimeout   = 60 * time.Second
	maxAPICallRequest       = 1 << 20
	maxAPICallResponse      = 4 << 20
	apiCallTokenPlaceholder = "$TOKEN$"
)

type apiCallRequest struct {
	AuthIndexSnake  *string           `json:"auth_index"`
	AuthIndexCamel  *string           `json:"authIndex"`
	AuthIndexPascal *string           `json:"AuthIndex"`
	Method          string            `json:"method"`
	URL             string            `json:"url"`
	ProxyURL        string            `json:"proxy_url"`
	Header          map[string]string `json:"header"`
	Data            string            `json:"data"`
}

type apiCallResponse struct {
	StatusCode int                 `json:"status_code"`
	Header     map[string][]string `json:"header"`
	Body       string              `json:"body"`
}

var errAPICallPrivateTarget = errors.New("API call destination is not publicly routable")
var errAPICallProxy = errors.New("proxy is unsupported for direct management API calls")

// APICall executes a bounded HTTP(S) request on the management caller's behalf.
// Stored credentials may only be used at their configured HTTPS origin.
func (h *Handler) APICall(c *gin.Context) {
	h.apiCall(c, apiCallPublicTransport, defaultAPICallTimeout)
}

func (h *Handler) apiCall(c *gin.Context, transport http.RoundTripper, timeout time.Duration) {
	var input apiCallRequest
	kind, _, _ := mime.ParseMediaType(c.GetHeader("Content-Type"))
	inputCBOR := kind == "application/cbor"
	body, err := io.ReadAll(http.MaxBytesReader(c.Writer, c.Request.Body, maxAPICallRequest))
	if err != nil {
		var limit *http.MaxBytesError
		if errors.As(err, &limit) {
			c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "API call request exceeds size limit"})
			return
		}
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	if inputCBOR {
		err = cbor.Unmarshal(body, &input)
	} else {
		err = json.Unmarshal(body, &input)
	}
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}

	method := strings.ToUpper(strings.TrimSpace(input.Method))
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodOptions:
	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": "unsupported HTTP method"})
		return
	}
	target, err := apiCallURL(input.URL)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	index := firstNonEmptyString(input.AuthIndexSnake, input.AuthIndexCamel, input.AuthIndexPascal)
	for _, alias := range []*string{input.AuthIndexSnake, input.AuthIndexCamel, input.AuthIndexPascal} {
		if alias != nil && strings.TrimSpace(*alias) != "" && strings.TrimSpace(*alias) != index {
			c.JSON(http.StatusBadRequest, gin.H{"error": "conflicting auth index fields"})
			return
		}
	}
	auth := h.authByIndex(index)
	if index != "" && (auth == nil || auth.Disabled || auth.Status == coreauth.StatusDisabled) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "auth credential not found or disabled"})
		return
	}
	effectiveProxy := h.apiCallProxy(auth)
	if !apiCallDirect(input.ProxyURL) || !apiCallDirect(effectiveProxy) {
		c.JSON(http.StatusBadRequest, gin.H{"error": errAPICallProxy.Error()})
		return
	}
	header := make(http.Header, len(input.Header))
	needsToken := strings.Contains(input.Data, apiCallTokenPlaceholder)
	var privateValues []string
	for key, value := range input.Header {
		if !httpguts.ValidHeaderFieldName(key) || !httpguts.ValidHeaderFieldValue(value) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request header"})
			return
		}
		key = http.CanonicalHeaderKey(key)
		if !apiCallAllowedHeader(key) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "unsupported request header"})
			return
		}
		if _, exists := header[key]; exists {
			c.JSON(http.StatusBadRequest, gin.H{"error": "duplicate request header"})
			return
		}
		if strings.Contains(value, apiCallTokenPlaceholder) {
			if key != "Authorization" && key != "X-Api-Key" && key != "X-Goog-Api-Key" {
				c.JSON(http.StatusBadRequest, gin.H{"error": "token placeholder is unsupported in this header"})
				return
			}
			needsToken = true
		}
		if apiCallPrivateHeader(key) {
			if target.Scheme != "https" {
				c.JSON(http.StatusBadRequest, gin.H{"error": "credentials require HTTPS"})
				return
			}
			privateValues = append(privateValues, privateHeaderValues(key, value)...)
		}
		header.Set(key, value)
	}
	if needsToken {
		token, err := ordinaryAPICallToken(auth, target)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		privateValues = append(privateValues, token)
		var headerBytes int
		for key, vals := range header {
			headerBytes += len(key) + expandedAPICallSize(vals[0], token)
			if headerBytes > maxAPICallRequest {
				c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "credential replacement exceeds size limit"})
				return
			}
		}
		for key, vals := range header {
			v := strings.ReplaceAll(vals[0], apiCallTokenPlaceholder, token)
			if !httpguts.ValidHeaderFieldValue(v) {
				c.JSON(http.StatusBadRequest, gin.H{"error": "invalid credential header"})
				return
			}
			header.Set(key, v)
		}
		input.Data, err = replaceAPICallTokenData(input.Data, token)
		if err != nil {
			c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": err.Error()})
			return
		}
	}

	payload := []byte(input.Data)
	ct, _, _ := mime.ParseMediaType(header.Get("Content-Type"))
	if len(payload) > 0 && ct == "application/cbor" {
		var value any
		if err := json.Unmarshal(payload, &value); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid JSON data for CBOR content type"})
			return
		}
		payload, err = cbor.Marshal(value)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid CBOR request"})
			return
		}
	}

	req, err := http.NewRequestWithContext(c.Request.Context(), method, target.String(), bytes.NewReader(payload))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "failed to build request"})
		return
	}
	req.Header = header
	client := boundedAPICallClient(transport, timeout)
	resp, err := client.Do(req)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "request failed", "failure_kind": classifyAPICallError(err)})
		return
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxAPICallResponse+1))
	if err != nil || len(respBody) > maxAPICallResponse {
		reason := "response_too_large"
		if err != nil {
			reason = classifyAPICallError(err)
		}
		c.JSON(http.StatusBadGateway, gin.H{"error": "failed to read response", "failure_kind": reason})
		return
	}
	bodyText := string(respBody)
	ct, _, _ = mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if len(respBody) > 0 && (ct == "application/cbor" || strings.Contains(header.Get("Accept"), "application/cbor")) {
		if decoded, errDecode := decodeAPICallCBOR(respBody); errDecode == nil {
			bodyText = decoded
		}
	}
	redactor := apiCallRedactor(privateValues)
	outHeaders := make(http.Header, len(resp.Header))
	for key, vals := range resp.Header {
		if apiCallPrivateHeader(key) {
			continue
		}
		for _, val := range vals {
			outHeaders.Add(key, redactor.Replace(val))
		}
	}
	out := apiCallResponse{
		StatusCode: resp.StatusCode,
		Header:     outHeaders,
		Body:       redactor.Replace(bodyText),
	}
	if inputCBOR {
		encoded, err := cbor.Marshal(out)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to encode CBOR response"})
			return
		}
		c.Data(http.StatusOK, "application/cbor", encoded)
		return
	}
	c.JSON(http.StatusOK, out)
}

func boundedAPICallClient(transport http.RoundTripper, timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout:       timeout,
		Transport:     transport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

func apiCallAllowedHeader(key string) bool {
	switch key {
	case "Host", "Connection", "Upgrade", "Te", "Trailer", "Transfer-Encoding", "Content-Length",
		"Keep-Alive", "Proxy-Connection", "Proxy-Authenticate", "Proxy-Authorization", "Forwarded",
		"X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto", "X-Real-Ip":
		return false
	}
	return true
}

func replaceAPICallTokenData(text, token string) (string, error) {
	if !strings.Contains(text, apiCallTokenPlaceholder) {
		return text, nil
	}
	replacement := token
	if json.Valid([]byte(text)) {
		encoded, err := json.Marshal(token)
		if err != nil {
			return "", errors.New("invalid request body token")
		}
		replacement = string(encoded[1 : len(encoded)-1])
	}
	if expandedAPICallSize(text, replacement) > maxAPICallRequest {
		return "", errors.New("credential replacement exceeds size limit")
	}
	return strings.ReplaceAll(text, apiCallTokenPlaceholder, replacement), nil
}

func apiCallPrivateHeader(key string) bool {
	switch http.CanonicalHeaderKey(key) {
	case "Authorization", "Cookie", "Set-Cookie", "Proxy-Authorization", "X-Api-Key", "X-Goog-Api-Key", "X-Auth-Token", "X-Access-Token", "X-Session-Token", "Authentication-Info", "Proxy-Authentication-Info":
		return true
	}
	return false
}

func privateHeaderValues(key, value string) []string {
	switch http.CanonicalHeaderKey(key) {
	case "Authorization":
		if _, secret, ok := strings.Cut(value, " "); ok {
			return []string{value, secret}
		}
	case "Cookie":
		result := []string{value}
		for _, part := range strings.Split(value, ";") {
			if _, secret, ok := strings.Cut(part, "="); ok {
				result = append(result, strings.TrimSpace(secret))
			}
		}
		return result
	}
	return []string{value}
}

func expandedAPICallSize(text, replacement string) int {
	return len(text) + strings.Count(text, apiCallTokenPlaceholder)*(len(replacement)-len(apiCallTokenPlaceholder))
}

func apiCallRedactor(secrets []string) *strings.Replacer {
	var pairs []string
	for _, secret := range secrets {
		if secret == "" || secret == apiCallTokenPlaceholder {
			continue
		}
		pairs = append(pairs, secret, "[redacted]", url.QueryEscape(secret), "[redacted]", url.PathEscape(secret), "[redacted]")
		if encoded, err := json.Marshal(secret); err == nil {
			pairs = append(pairs, string(encoded[1:len(encoded)-1]), "[redacted]")
		}
	}
	return strings.NewReplacer(pairs...)
}

func apiCallURL(raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Hostname() == "" || u.User != nil || u.Fragment != "" || u.Opaque != "" ||
		(u.Scheme != "http" && u.Scheme != "https") || strings.Contains(u.Host, "%") {
		return nil, errors.New("invalid HTTP(S) URL")
	}
	if p := u.Port(); p != "" {
		n, err := strconv.ParseUint(p, 10, 16)
		if err != nil || n == 0 {
			return nil, errors.New("invalid HTTP(S) port")
		}
	}
	query, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return nil, errors.New("invalid HTTP query")
	}
	containsToken := strings.Contains(u.Path, apiCallTokenPlaceholder)
	for key, vals := range query {
		containsToken = containsToken || strings.Contains(key, apiCallTokenPlaceholder)
		for _, val := range vals {
			containsToken = containsToken || strings.Contains(val, apiCallTokenPlaceholder)
		}
	}
	if containsToken {
		return nil, errors.New("token placeholder is unsupported in URLs")
	}
	if !validAPICallHost(u.Hostname()) {
		return nil, errAPICallPrivateTarget
	}
	return u, nil
}

func validAPICallHost(host string) bool {
	if addr, err := netip.ParseAddr(host); err == nil {
		return publicAPICallIP(addr)
	}
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	return strings.Contains(host, ".") && !strings.HasSuffix(host, ".localhost") &&
		!strings.HasSuffix(host, ".local") && !strings.HasSuffix(host, ".internal") && !strings.HasSuffix(host, ".home.arpa")
}

var apiCallNonPublicNetworks = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"), netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"), netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"), netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"), netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("64:ff9b::/96"), netip.MustParsePrefix("64:ff9b:1::/48"),
	netip.MustParsePrefix("2001::/23"), netip.MustParsePrefix("2001:db8::/32"), netip.MustParsePrefix("2002::/16"),
}

func publicAPICallIP(addr netip.Addr) bool {
	addr = addr.Unmap()
	if addr.Zone() != "" || !addr.IsGlobalUnicast() || addr.IsPrivate() || addr.IsLoopback() || addr.IsLinkLocalUnicast() {
		return false
	}
	for _, network := range apiCallNonPublicNetworks {
		if network.Contains(addr) {
			return false
		}
	}
	return true
}

func allPublicAPICallIPs(addrs []netip.Addr) bool {
	for _, addr := range addrs {
		if !publicAPICallIP(addr) {
			return false
		}
	}
	return len(addrs) > 0
}

func apiCallDial(ctx context.Context, network, address string) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	host, port, err := net.SplitHostPort(address)
	if err != nil || !validAPICallHost(host) {
		return nil, errAPICallPrivateTarget
	}
	if addr, err := netip.ParseAddr(host); err == nil {
		return (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, network, net.JoinHostPort(addr.String(), port))
	}
	addrs, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return nil, err
	}
	if !allPublicAPICallIPs(addrs) {
		return nil, errAPICallPrivateTarget
	}
	dial := net.Dialer{Timeout: 10 * time.Second}
	var last error
	for _, addr := range addrs {
		conn, err := dial.DialContext(ctx, network, net.JoinHostPort(addr.String(), port))
		if err == nil {
			return conn, nil
		}
		last = err
	}
	return nil, last
}

var apiCallPublicTransport = &http.Transport{
	DialContext: apiCallDial, ForceAttemptHTTP2: true, MaxIdleConns: 32, MaxIdleConnsPerHost: 4,
	IdleConnTimeout: 60 * time.Second, TLSHandshakeTimeout: 10 * time.Second,
	MaxResponseHeaderBytes: 1 << 20,
}

func firstNonEmptyString(values ...*string) string {
	for _, v := range values {
		if v != nil && strings.TrimSpace(*v) != "" {
			return strings.TrimSpace(*v)
		}
	}
	return ""
}

func ordinaryAPICallToken(auth *coreauth.Auth, target *url.URL) (string, error) {
	if auth == nil {
		return "", errors.New("stored credential is required for the token placeholder")
	}
	if target.Scheme != "https" || !apiCallOriginAllowed(auth, target) {
		return "", errors.New("credential is not authorized for this HTTPS origin")
	}
	metadata := auth.Metadata
	key := strings.TrimSpace(auth.Attributes["api_key"])
	if key != "" {
		return checkedAPICallCredential(key)
	}
	tokenMeta, _ := metadata["token"].(map[string]any)
	tokenMetaStr, _ := metadata["token"].(map[string]string)
	for _, name := range []string{"access_token", "accessToken"} {
		for _, value := range []any{metadata[name], tokenMeta[name], tokenMetaStr[name]} {
			if text, ok := value.(string); ok && strings.TrimSpace(text) != "" {
				return checkedAPICallCredential(strings.TrimSpace(text))
			}
		}
	}
	if strings.EqualFold(strings.TrimSpace(auth.Provider), "xai") {
		if storage, ok := auth.Storage.(*xaiauth.TokenStorage); ok && storage != nil && strings.TrimSpace(storage.AccessToken) != "" {
			return checkedAPICallCredential(strings.TrimSpace(storage.AccessToken))
		}
	}
	if key, ok := metadata["api_key"].(string); ok && strings.TrimSpace(key) != "" {
		return checkedAPICallCredential(strings.TrimSpace(key))
	}
	return "", errors.New("stored access token is unavailable; refresh and delegated tokens are unsupported here")
}

func checkedAPICallCredential(token string) (string, error) {
	if len(token) > 16<<10 {
		return "", errors.New("stored credential exceeds size limit")
	}
	return token, nil
}

func apiCallOriginAllowed(auth *coreauth.Auth, target *url.URL) bool {
	port := target.Port()
	if port == "" {
		port = "443"
	}
	provider := strings.ToLower(strings.TrimSpace(auth.Provider))
	switch provider {
	case "claude", "anthropic", "codex", "openai", "openai-compatibility", "xai", "gemini", "gemini-interactions":
	default:
		return false
	}
	host := strings.ToLower(target.Hostname())
	if base := strings.TrimSpace(auth.Attributes["base_url"]); base != "" && strings.TrimSpace(auth.Attributes["api_key"]) != "" {
		configured, err := apiCallURL(base)
		if err != nil || configured.Scheme != "https" {
			return false
		}
		basePort := configured.Port()
		if basePort == "" {
			basePort = "443"
		}
		return strings.EqualFold(configured.Hostname(), host) && basePort == port
	}
	if port != "443" {
		return false
	}
	switch provider {
	case "claude", "anthropic":
		return host == "api.anthropic.com"
	case "codex":
		if strings.TrimSpace(auth.Attributes["api_key"]) != "" {
			return host == "api.openai.com"
		}
		return host == "chatgpt.com"
	case "openai":
		return host == "api.openai.com"
	case "xai":
		return host == "api.x.ai"
	case "gemini", "gemini-interactions":
		return host == "generativelanguage.googleapis.com"
	}
	return false
}

func classifyAPICallError(err error) string {
	switch {
	case errors.Is(err, errAPICallPrivateTarget):
		return "destination_blocked"
	case errors.Is(err, context.DeadlineExceeded):
		return "context_deadline_exceeded"
	case errors.Is(err, context.Canceled):
		return "context_canceled"
	}
	var dns *net.DNSError
	if errors.As(err, &dns) {
		return "dns_error"
	}
	var timed net.Error
	if errors.As(err, &timed) && timed.Timeout() {
		return "network_timeout"
	}
	var operation *net.OpError
	if errors.As(err, &operation) {
		switch operation.Op {
		case "dial", "read", "write":
			return "network_op_error:" + operation.Op
		}
	}
	return "transport_error"
}

func decodeAPICallCBOR(raw []byte) (string, error) {
	var value any
	mode, err := (cbor.DecOptions{MaxNestedLevels: 16, MaxArrayElements: 8192, MaxMapPairs: 8192}).DecMode()
	if err != nil {
		return "", err
	}
	if err := mode.Unmarshal(raw, &value); err != nil {
		return "", err
	}
	switch data := value.(type) {
	case string:
		return data, nil
	case []byte:
		return string(data), nil
	default:
		encoded, err := json.Marshal(cborValueToJSONCompatible(value))
		return string(encoded), err
	}
}

func (h *Handler) authByIndex(index string) *coreauth.Auth {
	if h == nil || strings.TrimSpace(index) == "" {
		return nil
	}
	h.mu.Lock()
	manager := h.authManager
	h.mu.Unlock()
	if manager == nil {
		return nil
	}
	for _, item := range manager.List() {
		if item != nil && item.EnsureIndex() == index {
			return item
		}
	}
	return nil
}

func (h *Handler) apiCallProxy(auth *coreauth.Auth) string {
	if auth != nil && strings.TrimSpace(auth.ProxyURL) != "" {
		return strings.TrimSpace(auth.ProxyURL)
	}
	if cfg := h.configSnapshot(); cfg != nil {
		if value := proxyURLFromAPIKeyConfig(cfg, auth); value != "" {
			return value
		}
		return strings.TrimSpace(cfg.ProxyURL)
	}
	return ""
}

func apiCallDirect(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "direct", "none":
		return true
	}
	return false
}

// Plugin quota has its own configured HTTP client; the generic API call does
// not delegate to it or share its outbound proxy policy.
func (h *Handler) apiCallTransport(auth *coreauth.Auth, requestProxyURL string) http.RoundTripper {
	proxy := strings.TrimSpace(requestProxyURL)
	if proxy == "" {
		proxy = h.apiCallProxy(auth)
	}
	if apiCallDirect(proxy) {
		return directAPICallTransport()
	}
	if result := buildProxyTransport(proxy); result != nil {
		return result
	}
	return failedAPICallTransport{}
}

type failedAPICallTransport struct{}

func (failedAPICallTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errAPICallProxy
}

func directAPICallTransport() http.RoundTripper {
	if transport, ok := http.DefaultTransport.(*http.Transport); ok && transport != nil {
		clone := transport.Clone()
		clone.Proxy = nil
		return clone
	}
	return &http.Transport{}
}

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
			if cfgKey == attrKey && strings.EqualFold(cfgBase, attrBase) {
				return entry
			}
			continue
		}
		if attrKey != "" && cfgKey == attrKey {
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
			if strings.TrimSpace((*entry).GetAPIKey()) == attrKey {
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
		if entry := resolveAPIKeyConfig(cfg.XAIKey, auth); entry != nil {
			return strings.TrimSpace(entry.ProxyURL)
		}
	case "meta":
		if entry := resolveAPIKeyConfig(cfg.MetaKey, auth); entry != nil {
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
					if strings.TrimSpace(entry.APIKey) == apiKey {
						return strings.TrimSpace(entry.ProxyURL)
					}
				}
				return ""
			}
		}
	}
	return ""
}

func buildProxyTransport(proxy string) *http.Transport {
	transport, _, err := proxyutil.BuildHTTPTransport(proxy)
	if err != nil {
		return nil
	}
	return transport
}
