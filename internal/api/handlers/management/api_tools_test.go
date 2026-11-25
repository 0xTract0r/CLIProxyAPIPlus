package management

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/fxamacker/cbor/v2"
	"github.com/gin-gonic/gin"
	xaiauth "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/xai"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

type apiCallFixtureRoundTripper func(*http.Request) (*http.Response, error)

func (f apiCallFixtureRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

const apiCallFixtureAdminKey = "synthetic-api-call-admin"

func apiCallFixtureHandler(t *testing.T, item *coreauth.Auth, proxy string) (*Handler, string) {
	t.Helper()
	t.Setenv("MANAGEMENT_PASSWORD", apiCallFixtureAdminKey)
	manager := coreauth.NewManager(nil, nil, nil)
	index := ""
	if item != nil {
		registered, err := manager.Register(context.Background(), item)
		if err != nil {
			t.Fatal(err)
		}
		index = registered.EnsureIndex()
	}
	return NewHandlerWithoutConfigFilePath(&config.Config{SDKConfig: sdkconfig.SDKConfig{ProxyURL: proxy}}, manager), index
}

func apiCallFixtureRequest(t *testing.T, h *Handler, transport apiCallFixtureRoundTripper, timeout time.Duration, input []byte, contentType, key string) *httptest.ResponseRecorder {
	t.Helper()
	router := gin.New()
	router.POST("/v0/management/api-call", h.Middleware(), func(c *gin.Context) { h.apiCall(c, transport, timeout) })
	req := httptest.NewRequest(http.MethodPost, "/v0/management/api-call", strings.NewReader(string(input)))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Content-Type", contentType)
	if key != "" {
		req.Header.Set("X-Management-Key", key)
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

func apiCallJSON(t *testing.T, h *Handler, transport apiCallFixtureRoundTripper, timeout time.Duration, input any) *httptest.ResponseRecorder {
	t.Helper()
	b, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	return apiCallFixtureRequest(t, h, transport, timeout, b, "application/json", apiCallFixtureAdminKey)
}

func apiCallFixtureResponse(status int, headers http.Header, text string) *http.Response {
	return &http.Response{StatusCode: status, Header: headers, Body: io.NopCloser(strings.NewReader(text))}
}

func TestAPICallKeepsUpstreamStatusAndDoesNotFollowRedirects(t *testing.T) {
	h, _ := apiCallFixtureHandler(t, nil, "")
	var calls int
	rt := func(req *http.Request) (*http.Response, error) {
		calls++
		if req.Method != http.MethodGet || req.URL.Host != "service.example.invalid" || req.URL.Query().Get("page") != "2" ||
			req.Header.Get("X-Management-Key") != "" {
			t.Fatal("wrong upstream request")
		}
		return apiCallFixtureResponse(http.StatusTemporaryRedirect, http.Header{
			"Location": {"https://next.example.invalid/"},
		}, "redirect"), nil
	}
	input := []byte(`{"method":"GET","url":"https://service.example.invalid/list?page=2"}`)
	if denied := apiCallFixtureRequest(t, h, rt, time.Second, input, "application/json", ""); denied.Code != http.StatusUnauthorized || calls != 0 {
		t.Fatalf("unauthenticated caller returned %d with %d outbound calls", denied.Code, calls)
	}
	w := apiCallFixtureRequest(t, h, rt, time.Second, input, "application/json", apiCallFixtureAdminKey)
	var value apiCallResponse
	if err := json.Unmarshal(w.Body.Bytes(), &value); err != nil || w.Code != 200 || calls != 1 ||
		value.StatusCode != http.StatusTemporaryRedirect || value.Body != "redirect" ||
		http.Header(value.Header).Get("Location") != "https://next.example.invalid/" {
		t.Fatalf("redirect/status shape: http=%d requests=%d decode=%v", w.Code, calls, err)
	}
}

func TestAPICallReplacesTokenInBodyData(t *testing.T) {
	const secret = "synthetic-stored-credential"
	h, id := apiCallFixtureHandler(t, &coreauth.Auth{
		ID: "fixture-xai.json", Provider: "xai", Status: coreauth.StatusActive,
		Metadata: map[string]any{"access_token": secret, "id_token": "unused-id-fixture"},
	}, "")
	test := func(req *http.Request) (*http.Response, error) {
		var body map[string]string
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil || body["token"] != secret ||
			req.Header.Get("Authorization") != "Bearer "+secret || req.URL.Host != "api.x.ai" {
			t.Fatal("stored access token went to the wrong destination or payload")
		}
		return apiCallFixtureResponse(http.StatusTooManyRequests, http.Header{
			"Set-Cookie": {"session=" + secret}, "X-Echo": {secret}, "Retry-After": {"12"},
		}, "echo: "+secret), nil
	}
	r := apiCallJSON(t, h, test, time.Second, apiCallRequest{
		Method: http.MethodPost, URL: "https://api.x.ai/example", AuthIndexSnake: &id,
		Header: map[string]string{"Authorization": "Bearer $TOKEN$", "Content-Type": "application/json"},
		Data:   `{"token":"$TOKEN$"}`,
	})
	var out apiCallResponse
	if err := json.Unmarshal(r.Body.Bytes(), &out); err != nil || r.Code != 200 ||
		out.StatusCode != http.StatusTooManyRequests || http.Header(out.Header).Get("Retry-After") != "12" ||
		strings.Contains(r.Body.String(), secret) || http.Header(out.Header).Get("Set-Cookie") != "" ||
		out.Body != "echo: [redacted]" {
		t.Fatal("status, useful response metadata or credential isolation failed")
	}
}

func TestAPICallRejectsUnresolvedTokenPlaceholder(t *testing.T) {
	h, id := apiCallFixtureHandler(t, &coreauth.Auth{
		ID: "fixture-codex.json", Provider: "codex", Status: coreauth.StatusActive,
		Metadata: map[string]any{"id_token": "synthetic-id-only", "refresh_token": "synthetic-refresh-only"},
	}, "")
	failTransport := func(*http.Request) (*http.Response, error) {
		t.Fatal("refused request reached HTTP transport")
		return nil, nil
	}
	noToken := map[string]string{"Authorization": "Bearer $TOKEN$"}
	blank := ""
	for _, input := range []apiCallRequest{
		{Method: "GET", URL: "https://chatgpt.com/example", AuthIndexCamel: &id, Header: noToken},
		{Method: "GET", URL: "https://chatgpt.com/example", AuthIndexCamel: &blank, Header: noToken},
		{Method: "GET", URL: "https://service.example.invalid/example", AuthIndexCamel: &id, Header: noToken},
		{Method: "GET", URL: "http://chatgpt.com/example", AuthIndexCamel: &id, Header: noToken},
		{Method: "GET", URL: "https://chatgpt.com/example", AuthIndexCamel: &id, Header: map[string]string{"X-Unbound": "$TOKEN$"}},
		{Method: "GET", URL: "https://service.example.invalid/example", Header: map[string]string{"Host": "override.example.invalid"}},
		{Method: "GET", URL: "https://service.example.invalid/example?token=%24TOKEN%24"},
		{Method: "CONNECT", URL: "https://service.example.invalid/example"},
		{Method: "GET", URL: "https://service.example.invalid/example", ProxyURL: "http://proxy.example.invalid"},
	} {
		r := apiCallJSON(t, h, failTransport, time.Second, input)
		if r.Code != 400 || strings.Contains(r.Body.String(), "synthetic-id-only") {
			t.Fatalf("invalid request status=%d", r.Code)
		}
	}
}

func TestAPICallRejectsConfiguredProxyWithoutSilentDirectFallback(t *testing.T) {
	h, _ := apiCallFixtureHandler(t, nil, "http://proxy.example.invalid")
	w := apiCallJSON(t, h, func(*http.Request) (*http.Response, error) {
		t.Fatal("configured proxy was silently bypassed")
		return nil, nil
	}, time.Second, apiCallRequest{Method: "GET", URL: "https://service.example.invalid/", ProxyURL: "direct"})
	if w.Code != 400 || !strings.Contains(w.Body.String(), "proxy is unsupported") {
		t.Fatalf("proxy rejection status=%d", w.Code)
	}
}

func TestAPICallErrorResponseIncludesFailureKind(t *testing.T) {
	h, _ := apiCallFixtureHandler(t, nil, "")
	for _, check := range []struct {
		name, kind string
		rt         apiCallFixtureRoundTripper
	}{
		{"actual client timeout", "context_deadline_exceeded", func(req *http.Request) (*http.Response, error) {
			<-req.Context().Done()
			return nil, req.Context().Err()
		}},
		{"DNS timeout", "dns_error", func(*http.Request) (*http.Response, error) {
			return nil, &net.DNSError{Err: "synthetic-private-error", Name: "finite.example.invalid", IsTimeout: true}
		}},
		{"limited body", "response_too_large", func(*http.Request) (*http.Response, error) {
			return apiCallFixtureResponse(http.StatusOK, nil, strings.Repeat("x", maxAPICallResponse+1)), nil
		}},
	} {
		t.Run(check.name, func(t *testing.T) {
			w := apiCallJSON(t, h, check.rt, 100*time.Millisecond, apiCallRequest{Method: "GET", URL: "https://service.example.invalid/"})
			if w.Code != 502 || !strings.Contains(w.Body.String(), check.kind) || strings.Contains(w.Body.String(), "synthetic-private-error") {
				t.Fatalf("transport error status=%d", w.Code)
			}
		})
	}
}

func TestAPICallPublicAddressBoundary(t *testing.T) {
	for _, target := range []string{"http://localhost/", "http://127.0.0.1/", "https://[::1]/", "http://169.254.169.254/",
		"http://service.internal/", "https://user:password@service.example.invalid/"} {
		if _, err := apiCallURL(target); err == nil {
			t.Fatalf("invalid destination was accepted: %s", target)
		}
	}
	open := netip.MustParseAddr("93.184.216.34")
	private := netip.MustParseAddr("10.1.2.3")
	if !allPublicAPICallIPs([]netip.Addr{open}) || allPublicAPICallIPs([]netip.Addr{open, private}) || allPublicAPICallIPs(nil) {
		t.Fatal("DNS result filtering must reject mixed or empty results")
	}
}

func TestAPICallCBORCompatibility(t *testing.T) {
	h, _ := apiCallFixtureHandler(t, nil, "")
	input, err := cbor.Marshal(apiCallRequest{
		Method: "POST", URL: "https://service.example.invalid/", Data: `{"value":"ordinary"}`,
		Header: map[string]string{"Content-Type": "application/cbor"},
	})
	if err != nil {
		t.Fatal(err)
	}
	rt := func(req *http.Request) (*http.Response, error) {
		b, err := io.ReadAll(req.Body)
		var payload map[string]string
		if err != nil || cbor.Unmarshal(b, &payload) != nil || payload["value"] != "ordinary" {
			return nil, fmt.Errorf("invalid CBOR test payload")
		}
		data, err := cbor.Marshal(map[string]string{"result": "ok"})
		if err != nil {
			return nil, err
		}
		return apiCallFixtureResponse(http.StatusCreated, http.Header{"Content-Type": {"application/cbor"}}, string(data)), nil
	}
	w := apiCallFixtureRequest(t, h, rt, time.Second, input, "application/cbor", apiCallFixtureAdminKey)
	var output apiCallResponse
	if err := cbor.Unmarshal(w.Body.Bytes(), &output); err != nil || w.Code != 200 ||
		output.StatusCode != http.StatusCreated || output.Body != `{"result":"ok"}` {
		t.Fatalf("invalid CBOR envelope: status=%d err=%v", w.Code, err)
	}
}

func TestAPICallTransportDirectBypassesGlobalProxy(t *testing.T) {
	t.Parallel()

	h := &Handler{
		cfg: &config.Config{
			SDKConfig: sdkconfig.SDKConfig{ProxyURL: "http://global-proxy.example.com:8080"},
		},
	}

	transport := h.apiCallTransport(&coreauth.Auth{ProxyURL: "direct"}, "")
	httpTransport, ok := transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport type = %T, want *http.Transport", transport)
	}
	if httpTransport.Proxy != nil {
		t.Fatal("expected direct transport to disable proxy function")
	}
}

func TestAPICallTransportAPIKeyAuthFallsBackToConfigProxyURL(t *testing.T) {
	t.Parallel()

	h := &Handler{
		cfg: &config.Config{
			SDKConfig: sdkconfig.SDKConfig{ProxyURL: "http://global-proxy.example.com:8080"},
			GeminiKey: []config.GeminiKey{{
				APIKey:   "gemini-key",
				ProxyURL: "http://gemini-proxy.example.com:8080",
			}},
			ClaudeKey: []config.ClaudeKey{{
				APIKey:   "claude-key",
				ProxyURL: "http://claude-proxy.example.com:8080",
			}},
			CodexKey: []config.CodexKey{{
				APIKey:   "codex-key",
				ProxyURL: "http://codex-proxy.example.com:8080",
			}},
			XAIKey: []config.XAIKey{{
				APIKey:   "xai-key",
				ProxyURL: "http://xai-proxy.example.com:8080",
			}},
			MetaKey: []config.MetaKey{{
				APIKey:   "meta-key",
				ProxyURL: "http://meta-proxy.example.com:8080",
			}},
			OpenAICompatibility: []config.OpenAICompatibility{{
				Name:    "bohe",
				BaseURL: "https://bohe.example.com",
				APIKeyEntries: []config.OpenAICompatibilityAPIKey{{
					APIKey:   "compat-key",
					ProxyURL: "http://compat-proxy.example.com:8080",
				}},
			}},
		},
	}

	cases := []struct {
		name      string
		auth      *coreauth.Auth
		wantProxy string
	}{
		{
			name: "gemini",
			auth: &coreauth.Auth{
				Provider:   "gemini",
				Attributes: map[string]string{"api_key": "gemini-key"},
			},
			wantProxy: "http://gemini-proxy.example.com:8080",
		},
		{
			name: "claude",
			auth: &coreauth.Auth{
				Provider:   "claude",
				Attributes: map[string]string{"api_key": "claude-key"},
			},
			wantProxy: "http://claude-proxy.example.com:8080",
		},
		{
			name: "codex",
			auth: &coreauth.Auth{
				Provider:   "codex",
				Attributes: map[string]string{"api_key": "codex-key"},
			},
			wantProxy: "http://codex-proxy.example.com:8080",
		},
		{
			name: "xai",
			auth: &coreauth.Auth{
				Provider:   "xai",
				Attributes: map[string]string{"api_key": "xai-key"},
			},
			wantProxy: "http://xai-proxy.example.com:8080",
		},
		{
			name: "meta",
			auth: &coreauth.Auth{
				Provider:   "meta",
				Attributes: map[string]string{"api_key": "meta-key"},
			},
			wantProxy: "http://meta-proxy.example.com:8080",
		},
		{
			name: "openai-compatibility",
			auth: &coreauth.Auth{
				Provider: "bohe",
				Attributes: map[string]string{
					"api_key":      "compat-key",
					"compat_name":  "bohe",
					"provider_key": "bohe",
				},
			},
			wantProxy: "http://compat-proxy.example.com:8080",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			transport := h.apiCallTransport(tc.auth, "")
			httpTransport, ok := transport.(*http.Transport)
			if !ok {
				t.Fatalf("transport type = %T, want *http.Transport", transport)
			}

			req, errRequest := http.NewRequest(http.MethodGet, "https://example.com", nil)
			if errRequest != nil {
				t.Fatalf("http.NewRequest returned error: %v", errRequest)
			}

			proxyURL, errProxy := httpTransport.Proxy(req)
			if errProxy != nil {
				t.Fatalf("httpTransport.Proxy returned error: %v", errProxy)
			}
			if proxyURL == nil || proxyURL.String() != tc.wantProxy {
				t.Fatalf("proxy URL = %v, want %s", proxyURL, tc.wantProxy)
			}
		})
	}
}

func TestAuthByIndexDistinguishesSharedAPIKeysAcrossProviders(t *testing.T) {
	t.Parallel()

	manager := coreauth.NewManager(nil, nil, nil)
	geminiAuth := &coreauth.Auth{
		ID:       "gemini:apikey:123",
		Provider: "gemini",
		Attributes: map[string]string{
			"api_key": "shared-key",
		},
	}
	compatAuth := &coreauth.Auth{
		ID:       "openai-compatibility:bohe:456",
		Provider: "bohe",
		Label:    "bohe",
		Attributes: map[string]string{
			"api_key":      "shared-key",
			"compat_name":  "bohe",
			"provider_key": "bohe",
		},
	}

	if _, errRegister := manager.Register(context.Background(), geminiAuth); errRegister != nil {
		t.Fatalf("register gemini auth: %v", errRegister)
	}
	if _, errRegister := manager.Register(context.Background(), compatAuth); errRegister != nil {
		t.Fatalf("register compat auth: %v", errRegister)
	}

	geminiIndex := geminiAuth.EnsureIndex()
	compatIndex := compatAuth.EnsureIndex()
	if geminiIndex == compatIndex {
		t.Fatalf("shared api key produced duplicate auth_index %q", geminiIndex)
	}

	h := &Handler{authManager: manager}

	gotGemini := h.authByIndex(geminiIndex)
	if gotGemini == nil {
		t.Fatal("expected gemini auth by index")
	}
	if gotGemini.ID != geminiAuth.ID {
		t.Fatalf("authByIndex(gemini) returned %q, want %q", gotGemini.ID, geminiAuth.ID)
	}

	gotCompat := h.authByIndex(compatIndex)
	if gotCompat == nil {
		t.Fatal("expected compat auth by index")
	}
	if gotCompat.ID != compatAuth.ID {
		t.Fatalf("authByIndex(compat) returned %q, want %q", gotCompat.ID, compatAuth.ID)
	}
}

func TestAPICallUsesXAITokenStorageWithoutRefresh(t *testing.T) {
	const token = "synthetic-xai-storage"
	h, id := apiCallFixtureHandler(t, &coreauth.Auth{
		ID: "fixture-xai.json", Provider: "xai", Status: coreauth.StatusActive,
		Metadata: map[string]any{"id_token": "synthetic-unusable-id"},
		Storage:  &xaiauth.TokenStorage{AccessToken: token},
	}, "")
	w := apiCallJSON(t, h, func(req *http.Request) (*http.Response, error) {
		if req.Header.Get("Authorization") != "Bearer "+token {
			t.Fatal("ordinary stored access token not selected")
		}
		return apiCallFixtureResponse(http.StatusUnauthorized, make(http.Header), "expired upstream session"), nil
	}, time.Second, apiCallRequest{Method: "GET", URL: "https://api.x.ai/example", AuthIndexPascal: &id,
		Header: map[string]string{"Authorization": "Bearer $TOKEN$"}})
	var result apiCallResponse
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || w.Code != 200 || result.StatusCode != http.StatusUnauthorized {
		t.Fatal("normal stored token request did not return the real upstream status")
	}
}

func TestAPICallPluginQuotaBindsCredentialAndDisablesRedirect(t *testing.T) {
	const token = "synthetic-configured-api-key"
	item := &coreauth.Auth{
		ID: "ordinary-plugin-key", Provider: "openai-compatibility", Status: coreauth.StatusActive,
		Attributes: map[string]string{"api_key": token, "base_url": "https://quota.example.invalid/v1"},
	}
	h, _ := apiCallFixtureHandler(t, item, "")
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/v0/management/quota/fetch", nil)
	probe := map[string]any{
		"url":    "https://quota.example.invalid/v1/quota",
		"header": map[string]any{"Authorization": "Bearer $TOKEN$"},
	}
	var calls int
	get := apiCallFixtureRoundTripper(func(req *http.Request) (*http.Response, error) {
		calls++
		if req.URL.Host != "quota.example.invalid" || req.Header.Get("Authorization") != "Bearer "+token {
			t.Fatal("plugin quota request lost configured HTTPS credential binding")
		}
		return apiCallFixtureResponse(http.StatusOK, nil, `{"subscription":{"plan":"fixture-plan"}}`), nil
	})
	value, used, err := h.executeQuotaProbeWithTransport(c, item, probe, get)
	if err != nil || !used || value.Subscription == nil || value.Subscription.Plan != "fixture-plan" || calls != 1 {
		t.Fatalf("approved plugin quota shape used=%t calls=%d: %v", used, calls, err)
	}
	probe["url"] = "https://unbound.example.invalid/v1/quota"
	if _, _, err := h.executeQuotaProbeWithTransport(c, item, probe, get); err == nil || calls != 1 {
		t.Fatal("unknown plugin quota origin was not rejected before the transport")
	}
	probe["url"] = "https://quota.example.invalid/v1/quota"
	redirect := apiCallFixtureRoundTripper(func(*http.Request) (*http.Response, error) {
		calls++
		return apiCallFixtureResponse(http.StatusTemporaryRedirect, http.Header{"Location": {"https://unbound.example.invalid/"}}, token), nil
	})
	if _, _, err := h.executeQuotaProbeWithTransport(c, item, probe, redirect); err == nil ||
		!strings.Contains(err.Error(), "307") || strings.Contains(err.Error(), token) || calls != 2 {
		t.Fatal("credential-bound plugin request did not preserve status with redirection blocked")
	}
}

func TestAPICallPluginQuotaKeepsNonCredentialTransport(t *testing.T) {
	const configuredProxy = "http://ordinary-proxy.example.invalid:9999"
	h, _ := apiCallFixtureHandler(t, nil, configuredProxy)
	req := httptest.NewRequest("GET", "https://quota.example.invalid/", nil)
	transport, ok := h.apiCallTransport(nil, "").(*http.Transport)
	if !ok || transport.Proxy == nil {
		t.Fatal("ordinary non-credential plugin lost configured transport")
	}
	proxy, err := transport.Proxy(req)
	if err != nil || proxy.String() != configuredProxy {
		t.Fatal("non-credential plugin transport changed its configured proxy")
	}
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = req
	value, used, err := h.executeQuotaProbeWithTransport(c, nil, map[string]any{"url": req.URL.String()},
		apiCallFixtureRoundTripper(func(req *http.Request) (*http.Response, error) {
			if req.Header.Get("Authorization") != "" {
				t.Fatal("non-credential plugin request unexpectedly included authorization")
			}
			return apiCallFixtureResponse(200, nil, `{"subscription":{"plan":"public-plan"}}`), nil
		}))
	if err != nil || !used || value.Subscription == nil || value.Subscription.Plan != "public-plan" {
		t.Fatalf("normal non-credential plugin result: %v", err)
	}
}
