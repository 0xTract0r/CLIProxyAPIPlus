package api

import (
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	configaccess "github.com/router-for-me/CLIProxyAPI/v8/internal/access/config_access"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	sdkaccess "github.com/router-for-me/CLIProxyAPI/v8/sdk/access"
	"golang.org/x/net/http/httpguts"
)

type ampForwardState struct {
	proxy         *httputil.ReverseProxy
	localhostOnly bool
	serviceKey    string
	bindings      map[string]string
	tenantControl bool
}

var ampForwardTransport = &http.Transport{
	DialContext:           (&net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
	ForceAttemptHTTP2:     true,
	MaxIdleConns:          64,
	MaxIdleConnsPerHost:   8,
	IdleConnTimeout:       90 * time.Second,
	TLSHandshakeTimeout:   10 * time.Second,
	ExpectContinueTimeout: time.Second,
}

var ampForwardMethods = []string{http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete}

var ampForwardAPIBranches = []string{"/internal", "/user", "/auth", "/meta", "/telemetry", "/threads", "/otel", "/tab"}
var ampForwardAPIPaths = []string{"/ads", "/thread-actors"}
var ampForwardRootBranches = []string{"/threads", "/docs", "/settings", "/auth"}
var ampForwardRootPaths = []string{"/threads.rss", "/news.rss"}

func (s *Server) reloadAmpForwarding(settings config.AmpCode, clientKeys []string) error {
	current := s.ampForward.Load()
	next := &ampForwardState{
		localhostOnly: settings.RestrictManagementToLocalhost,
		bindings:      ampClientBindings(settings.UpstreamAPIKeys, clientKeys),
		tenantControl: settings.RequireUpstreamAPIKeyBinding || len(settings.UpstreamAPIKeys) > 0 ||
			(current != nil && current.tenantControl),
	}
	rawURL := strings.TrimSpace(settings.UpstreamURL)
	if rawURL == "" {
		s.ampForward.Store(next)
		return nil
	}
	target, err := url.Parse(rawURL)
	if err != nil || target.Hostname() == "" || target.User != nil || target.RawQuery != "" || target.ForceQuery || target.Fragment != "" ||
		(target.Scheme != "http" && target.Scheme != "https") {
		// Retain the working target. An invalid target must neither defer a revocation
		// nor authorize a new credential against the previous service.
		if current != nil {
			for caller, key := range next.bindings {
				if current.bindings[caller] != key {
					delete(next.bindings, caller)
				}
			}
			next.proxy = current.proxy
			next.serviceKey = current.serviceKey
			next.localhostOnly = next.localhostOnly || current.localhostOnly
			s.ampForward.Store(next)
		}
		return errors.New("invalid Amp upstream URL (expected an HTTP(S) service URL)")
	}
	next.serviceKey = strings.TrimSpace(settings.UpstreamAPIKey)
	next.proxy = &httputil.ReverseProxy{
		Transport: ampForwardTransport,
		// Body-copy errors may contain arbitrary upstream-provided strings.
		ErrorLog: log.New(io.Discard, "", 0),
		Rewrite: func(req *httputil.ProxyRequest) {
			req.SetURL(target)
			for _, header := range []string{
				"Authorization", "X-Api-Key", "X-Goog-Api-Key", "Proxy-Authorization", "Cookie",
				"X-Auth-Token", "X-Goog-Auth-Token", "X-Serverless-Authorization", "X-Goog-Iam-Authorization-Token",
				"Forwarded", "X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto", "X-Real-Ip",
			} {
				req.Out.Header.Del(header)
			}
			query := req.Out.URL.Query()
			for name := range query {
				switch strings.ToLower(name) {
				case "key", "auth_token", "access_token", "oauth_token", "authorization", "api_key", "api-key", "apikey", "x-api-key", "x-goog-api-key":
					query.Del(name)
				}
			}
			req.Out.URL.RawQuery = query.Encode()
		},
		ModifyResponse: func(resp *http.Response) error {
			removeAmpCORSHeaders(resp.Header)
			resp.Header.Del("Set-Cookie")
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, _ error) {
			if r.Context().Err() == nil {
				http.Error(w, "Amp upstream unavailable", http.StatusBadGateway)
			}
		},
	}
	s.ampForward.Store(next)
	return nil
}

func ampClientBindings(entries []config.AmpUpstreamAPIKeyEntry, clientKeys []string) map[string]string {
	active := make(map[string]struct{}, len(clientKeys))
	for _, caller := range clientKeys {
		active[strings.TrimSpace(caller)] = struct{}{}
	}
	bindings := make(map[string]string)
	for _, entry := range entries {
		key := strings.TrimSpace(entry.UpstreamAPIKey)
		if !httpguts.ValidHeaderFieldValue(key) {
			key = ""
		}
		for _, caller := range entry.APIKeys {
			caller = strings.TrimSpace(caller)
			if _, exists := active[caller]; !exists || caller == "" {
				continue
			}
			if previous, exists := bindings[caller]; exists && previous != key {
				// An ambiguous binding denies this caller regardless of entry order.
				bindings[caller] = ""
			} else {
				bindings[caller] = key
			}
		}
	}
	return bindings
}

func isAmpForwardBranch(path, branch string) bool {
	return path == branch || strings.HasPrefix(path, branch+"/")
}

func ampForwardPath(path string) bool {
	for _, branch := range ampForwardAPIBranches {
		if isAmpForwardBranch(path, "/api"+branch) {
			return true
		}
	}
	for _, exact := range ampForwardAPIPaths {
		if path == "/api"+exact {
			return true
		}
	}
	for _, branch := range ampForwardRootBranches {
		if isAmpForwardBranch(path, branch) {
			return true
		}
	}
	for _, exact := range ampForwardRootPaths {
		if path == exact {
			return true
		}
	}
	return false
}

func ampForwardPreflight(c *gin.Context) {
	if c.Request.Method == http.MethodOptions && ampForwardPath(c.Request.URL.Path) {
		c.AbortWithStatus(http.StatusNoContent)
		return
	}
	c.Next()
}

func removeAmpCORSHeaders(h http.Header) {
	for name := range h {
		if strings.HasPrefix(strings.ToLower(name), "access-control-") {
			delete(h, name)
		}
	}
}

func readyAmpForward(c *gin.Context, state *ampForwardState) bool {
	removeAmpCORSHeaders(c.Writer.Header())
	if state == nil || state.proxy == nil {
		logging.SkipGinRequestLogging(c)
		c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{"error": "Amp upstream unavailable"})
		return false
	}
	if state.localhostOnly {
		host, _, err := net.SplitHostPort(c.Request.RemoteAddr)
		if err != nil {
			host = c.Request.RemoteAddr
		}
		if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "Amp management is restricted to localhost"})
			return false
		}
	}
	return true
}

func serveAmpForward(c *gin.Context, state *ampForwardState, requireBinding bool) {
	caller := c.GetString("userApiKey")
	if caller == "" {
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "Amp forwarding requires a configured API key"})
		return
	}
	if !configaccess.RequestPresentsInlineKey(c.Request, caller) {
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "Amp forwarding requires possession of the authenticated API key"})
		return
	}
	key := state.serviceKey
	if requireBinding || state.tenantControl {
		key = state.bindings[caller]
		if key == "" || c.GetString("accessProvider") != sdkaccess.DefaultAccessProviderName {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "Amp upstream access is not configured for this API key"})
			return
		}
	}
	proxy := *state.proxy
	if key != "" {
		proxy.Transport = ampProtectedTransport{RoundTripper: state.proxy.Transport, caller: caller, upstreamKey: key}
		proxy.Rewrite = func(req *httputil.ProxyRequest) {
			state.proxy.Rewrite(req)
			req.Out.Header.Set("Authorization", "Bearer "+key)
			req.Out.Header.Set("X-Api-Key", key)
			req.Out.Header.Set("Accept-Encoding", "identity")
		}
		proxy.ModifyResponse = func(resp *http.Response) error {
			if err := state.proxy.ModifyResponse(resp); err != nil {
				return err
			}
			return protectAmpForwardResponse(resp, caller, key)
		}
	}
	defer func() {
		if recovered := recover(); recovered != nil && recovered != http.ErrAbortHandler {
			panic(recovered)
		}
	}()
	proxy.ServeHTTP(c.Writer, c.Request)
}

func (s *Server) registerAmpForwardRoutes() {
	auth := AuthMiddleware(s.accessManager)
	surface := func(c *gin.Context) {
		if readyAmpForward(c, s.ampForward.Load()) {
			c.Next()
		}
	}
	proxy := func(c *gin.Context) {
		state := s.ampForward.Load()
		if readyAmpForward(c, state) {
			serveAmpForward(c, state, false)
		}
	}
	api := s.engine.Group("/api", surface, auth, proxy)
	for _, method := range ampForwardMethods {
		for _, path := range ampForwardAPIBranches {
			api.Handle(method, path)
			api.Handle(method, path+"/*path")
		}
		for _, path := range ampForwardAPIPaths {
			api.Handle(method, path)
		}
	}
	for _, path := range ampForwardRootBranches {
		if path == "/auth" {
			for _, method := range ampForwardMethods {
				s.engine.Handle(method, path, surface, auth, proxy)
				s.engine.Handle(method, path+"/*path", surface, auth, proxy)
			}
		} else {
			s.engine.GET(path, surface, auth, proxy)
			s.engine.GET(path+"/*path", surface, auth, proxy)
		}
	}
	for _, path := range ampForwardRootPaths {
		s.engine.GET(path, surface, auth, proxy)
	}
}
