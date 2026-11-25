package executor

import (
	"context"
	"fmt"
	"net/http"
	"net/url"

	xaiauth "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/xai"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

var (
	xaiDataTag  = []byte("data:")
	xaiEventTag = []byte("event:")
)

const (
	xaiImageHandlerType        = "openai-image"
	xaiVideoHandlerType        = "openai-video"
	xaiSpeechHandlerType       = "openai-speech"
	xaiCustomToolType          = "custom"
	xaiFunctionToolType        = "function"
	xaiImageGenerationToolType = "image_generation"
	xaiNamespaceToolType       = "namespace"
	xaiToolSearchType          = "tool_search"
	xaiWebSearchToolType       = "web_search"
	xaiClientWebSearchAlias    = "clientfn_web_search"
	xaiXSearchToolType         = "x_search"
	xaiMaxTools                = 200
	// Codex Desktop injects codex_app.automation_update with a large oneOf+$ref
	// schema. xAI's free/build Responses path accepts the HTTP request but never
	// emits SSE when that schema is present, so Desktop hangs on "thinking".
	xaiCodexAppNamespaceName    = "codex_app"
	xaiAutomationUpdateToolName = "automation_update"
	// Permissive placeholder schema: keeps the tool callable without the hang.
	xaiSafeFunctionParameters     = `{"type":"object","properties":{},"additionalProperties":true}`
	xaiImagesGenerationsPath      = "/images/generations"
	xaiImagesEditsPath            = "/images/edits"
	xaiDefaultImageEndpointPath   = xaiImagesGenerationsPath
	xaiVideosGenerationsPath      = "/videos/generations"
	xaiVideosEditsPath            = "/videos/edits"
	xaiVideosExtensionsPath       = "/videos/extensions"
	xaiVideosPath                 = "/videos"
	xaiTTSPath                    = "/tts"
	xaiIdempotencyKeyMetaKey      = "idempotency_key"
	xaiComposerModelPrefix        = "grok-composer-"
	xaiTokenAuthHeader            = "X-XAI-Token-Auth"
	xaiClientVersionHeader        = "x-grok-client-version"
	xaiClientIdentifierHeader     = "x-grok-client-identifier"
	xaiAuthenticateResponseHeader = "x-authenticateresponse"
	// xaiUsingAPIAttr enables the official API path for HTTP chat and media.
	xaiUsingAPIAttr = "using_api"
)

// xaiXSearchToolJSON is the native X Search tool injected when enabled by config.
// Internal subtool traces are still filtered downstream when this tool is present.
var xaiXSearchToolJSON = []byte(`{"type":"x_search"}`)

// XAIExecutor is a stateless executor for xAI Grok's Responses API.
type XAIExecutor struct {
	cfg *config.Config
}

// NewXAIExecutor creates a new xAI executor.
func NewXAIExecutor(cfg *config.Config) *XAIExecutor {
	return &XAIExecutor{cfg: cfg}
}

// StartXAIVersionUpdater is retained for callers built against earlier releases.
// Standard API requests identify this service directly.
func StartXAIVersionUpdater(context.Context, string) {}

func xaiOriginPort(u *url.URL) string {
	if port := u.Port(); port != "" {
		return port
	}
	if u.Scheme == "https" {
		return "443"
	}
	return "80"
}

func xaiDestinationAllowed(auth *cliproxyauth.Auth, u *url.URL) error {
	if u == nil || u.Hostname() == "" || u.User != nil || (u.Scheme != "https" && u.Scheme != "http") {
		return statusErr{code: http.StatusBadRequest, msg: "invalid xAI destination"}
	}
	if strings.EqualFold(strings.TrimSuffix(u.Hostname(), "."), "cli-chat-proxy.grok.com") {
		return statusErr{code: http.StatusUnprocessableEntity, msg: "this xAI endpoint requires an unsupported client profile"}
	}
	_, base := xaiCreds(auth)
	if base == "" {
		base = xaiauth.DefaultAPIBaseURL
	}
	target, err := url.Parse(base)
	if err != nil || target.User != nil || target.Scheme != u.Scheme || !strings.EqualFold(target.Hostname(), u.Hostname()) || xaiOriginPort(target) != xaiOriginPort(u) {
		return statusErr{code: http.StatusForbidden, msg: "xAI credential is bound to a different origin"}
	}
	return nil
}

func doXAIHTTPRequest(client *http.Client, req *http.Request, auth *cliproxyauth.Auth) (*http.Response, error) {
	if err := xaiDestinationAllowed(auth, req.URL); err != nil {
		return nil, err
	}
	if req.Host != "" && !strings.EqualFold(req.Host, req.URL.Host) {
		return nil, statusErr{code: http.StatusForbidden, msg: "xAI request host does not match its origin"}
	}
	bound := *client
	bound.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return bound.Do(req)
}

// Identifier returns the provider identifier.
func (e *XAIExecutor) Identifier() string {
	return "xai"
}

// PrepareRequest injects xAI credentials into the outgoing HTTP request.
func (e *XAIExecutor) PrepareRequest(req *http.Request, auth *cliproxyauth.Auth) error {
	if req == nil {
		return nil
	}
	if err := xaiDestinationAllowed(auth, req.URL); err != nil {
		return err
	}
	if req.Host != "" && !strings.EqualFold(req.Host, req.URL.Host) {
		return statusErr{code: http.StatusForbidden, msg: "xAI request host does not match its origin"}
	}
	token, _ := xaiCreds(auth)
	if strings.TrimSpace(token) != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	} else {
		req.Header.Del("Authorization")
	}
	ensureXAIUserAgent(req)
	var attrs map[string]string
	if auth != nil {
		attrs = auth.Attributes
	}
	util.ApplyCustomHeadersFromAttrs(req, attrs)
	stripXAIPrivateHeaders(req.Header)
	return nil
}

// HttpRequest injects xAI credentials into the request and executes it.
func (e *XAIExecutor) HttpRequest(ctx context.Context, auth *cliproxyauth.Auth, req *http.Request) (*http.Response, error) {
	if req == nil {
		return nil, fmt.Errorf("xai executor: request is nil")
	}
	if ctx == nil {
		ctx = req.Context()
	}
	httpReq := req.Clone(ctx)
	if errPrepare := e.PrepareRequest(httpReq, auth); errPrepare != nil {
		return nil, errPrepare
	}
	httpClient := helps.NewProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	return doXAIHTTPRequest(httpClient, httpReq, auth)
}

// SupportsApplyPatch reports the actual executor contract, independent of its provider name.
func (e *XAIExecutor) SupportsApplyPatch() bool { return e != nil }
