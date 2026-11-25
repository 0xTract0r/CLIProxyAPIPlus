package api

import (
	"net/http"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

var ampExpectedProxyPaths = []string{
	"/api/internal",
	"/api/internal/*path",
	"/api/user",
	"/api/user/*path",
	"/api/auth",
	"/api/auth/*path",
	"/api/meta",
	"/api/meta/*path",
	"/api/ads",
	"/api/telemetry",
	"/api/telemetry/*path",
	"/api/threads",
	"/api/threads/*path",
	"/api/thread-actors",
	"/api/otel",
	"/api/otel/*path",
	"/api/tab",
	"/api/tab/*path",
	"/api/provider/:provider/v1beta1/*action",
	"/auth",
	"/auth/*path",
}

// Amp CLI also uses these root-level GET aliases.
var ampExpectedGetOnlyPaths = []string{
	"/threads",
	"/threads/*path",
	"/docs",
	"/docs/*path",
	"/settings",
	"/settings/*path",
	"/threads.rss",
	"/news.rss",
}

// Provider aliases that must survive the built-in route registration.
var ampExpectedProviderRoutes = []struct {
	method string
	path   string
}{
	{http.MethodGet, "/api/provider/:provider/models"},
	{http.MethodPost, "/api/provider/:provider/chat/completions"},
	{http.MethodPost, "/api/provider/:provider/completions"},
	{http.MethodPost, "/api/provider/:provider/responses"},
	{http.MethodGet, "/api/provider/:provider/v1/models"},
	{http.MethodPost, "/api/provider/:provider/v1/chat/completions"},
	{http.MethodPost, "/api/provider/:provider/v1/completions"},
	{http.MethodPost, "/api/provider/:provider/v1/responses"},
	{http.MethodPost, "/api/provider/:provider/v1/messages"},
	{http.MethodPost, "/api/provider/:provider/v1/messages/count_tokens"},
	{http.MethodGet, "/api/provider/:provider/v1beta/models"},
	{http.MethodPost, "/api/provider/:provider/v1beta/models/*action"},
	{http.MethodGet, "/api/provider/:provider/v1beta/models/*action"},
}

func TestOrdinaryAmpRegisteredRoutesHaveHandlers(t *testing.T) {
	s := newAmpForwardingFixture(t, config.AmpCode{})
	actual := make(map[string]bool)
	for _, route := range s.engine.Routes() {
		actual[route.Method+" "+route.Path] = true
	}
	for _, path := range ampExpectedProxyPaths {
		for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
			if !actual[method+" "+path] {
				t.Errorf("missing Amp forwarding route: %s %s", method, path)
			}
		}
		for _, method := range []string{http.MethodConnect, http.MethodTrace} {
			if actual[method+" "+path] {
				t.Errorf("unwanted Amp method: %s %s", method, path)
			}
		}
	}
	for _, path := range ampExpectedGetOnlyPaths {
		if !actual[http.MethodGet+" "+path] {
			t.Errorf("missing Amp read route: GET %s", path)
		}
	}
	for _, route := range ampExpectedProviderRoutes {
		if !actual[route.method+" "+route.path] {
			t.Errorf("missing Amp provider alias: %s %s", route.method, route.path)
		}
	}
}
