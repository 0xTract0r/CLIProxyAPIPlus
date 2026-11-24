package api

import (
	"errors"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
)

type ampForwardState struct {
	proxy         *httputil.ReverseProxy
	localhostOnly bool
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

var ampForwardAPIBranches = []string{"/internal", "/user", "/auth", "/meta", "/telemetry", "/threads", "/otel", "/tab"}
var ampForwardAPIPaths = []string{"/ads", "/thread-actors"}
var ampForwardRootBranches = []string{"/threads", "/docs", "/settings", "/auth"}
var ampForwardRootPaths = []string{"/threads.rss", "/news.rss"}

func (s *Server) reloadAmpForwarding(settings config.AmpCode) error {
	next := &ampForwardState{localhostOnly: settings.RestrictManagementToLocalhost}
	rawURL := strings.TrimSpace(settings.UpstreamURL)
	if rawURL == "" {
		s.ampForward.Store(next)
		return nil
	}
	target, err := url.Parse(rawURL)
	if err != nil || target.Hostname() == "" || target.User != nil || target.RawQuery != "" || target.ForceQuery || target.Fragment != "" ||
		(target.Scheme != "http" && target.Scheme != "https") {
		// Keep the working target and allow an invalid reload to tighten local access.
		if current := s.ampForward.Load(); current != nil && next.localhostOnly && !current.localhostOnly {
			s.ampForward.Store(&ampForwardState{proxy: current.proxy, localhostOnly: true})
		}
		return errors.New("invalid Amp upstream URL (expected an HTTP(S) service URL)")
	}
	serviceKey := strings.TrimSpace(settings.UpstreamAPIKey)
	next.proxy = &httputil.ReverseProxy{
		Transport: ampForwardTransport,
		Rewrite: func(req *httputil.ProxyRequest) {
			req.SetURL(target)
			for _, header := range []string{
				"Authorization", "X-Api-Key", "X-Goog-Api-Key", "Proxy-Authorization", "Cookie",
				"Forwarded", "X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto", "X-Real-Ip",
			} {
				req.Out.Header.Del(header)
			}
			query := req.Out.URL.Query()
			for _, name := range []string{"key", "auth_token", "api_key", "access_token"} {
				query.Del(name)
			}
			req.Out.URL.RawQuery = query.Encode()
			if serviceKey != "" {
				req.Out.Header.Set("Authorization", "Bearer "+serviceKey)
				req.Out.Header.Set("X-Api-Key", serviceKey)
			}
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

func (s *Server) registerAmpForwardRoutes() {
	auth := AuthMiddleware(s.accessManager)
	surface := func(c *gin.Context) {
		removeAmpCORSHeaders(c.Writer.Header())
		state := s.ampForward.Load()
		if state == nil || state.proxy == nil {
			logging.SkipGinRequestLogging(c)
			c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{"error": "Amp upstream unavailable"})
			return
		}
		if state.localhostOnly {
			host, _, err := net.SplitHostPort(c.Request.RemoteAddr)
			if err != nil {
				host = c.Request.RemoteAddr
			}
			if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
				c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "Amp management is restricted to localhost"})
				return
			}
		}
		// Keep the same target and locality decision for this request across hot reload.
		c.Set("amp-forward-state", state)
		c.Next()
	}
	proxy := func(c *gin.Context) {
		if c.GetString("userApiKey") == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "Amp forwarding requires a configured API key"})
			return
		}
		state := c.MustGet("amp-forward-state").(*ampForwardState)
		defer func() {
			if recovered := recover(); recovered != nil && recovered != http.ErrAbortHandler {
				panic(recovered)
			}
		}()
		state.proxy.ServeHTTP(c.Writer, c.Request)
	}
	api := s.engine.Group("/api", surface, auth, proxy)
	for _, path := range ampForwardAPIBranches {
		api.Any(path)
		api.Any(path + "/*path")
	}
	for _, path := range ampForwardAPIPaths {
		api.Any(path)
	}
	for _, path := range ampForwardRootBranches {
		if path == "/auth" {
			s.engine.Any(path, surface, auth, proxy)
			s.engine.Any(path+"/*path", surface, auth, proxy)
		} else {
			s.engine.GET(path, surface, auth, proxy)
			s.engine.GET(path+"/*path", surface, auth, proxy)
		}
	}
	for _, path := range ampForwardRootPaths {
		s.engine.GET(path, surface, auth, proxy)
	}
}
