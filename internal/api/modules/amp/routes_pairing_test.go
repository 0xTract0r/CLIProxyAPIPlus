package amp

import (
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers"
)

// ampAnyMethods mirrors the fixed method fan-out gin's RouterGroup.Any performs
// (gin v1.10.1, see vendored routergroup.go var anyMethods). Every path in
// ampManagementAnyPaths below is registered via Any(), so each one expands
// into exactly this many distinct (method, path) entries in engine.Routes().
// If gin ever changes this list, or if routes.go narrows one of these
// registrations away from Any() to fewer explicit methods, the per-method
// existence checks and the total-count assertion below both go red.
var ampAnyMethods = []string{
	http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch,
	http.MethodHead, http.MethodOptions, http.MethodDelete, http.MethodConnect,
	http.MethodTrace,
}

// ampManagementAnyPaths is the exhaustive list of paths registerManagementRoutes
// (routes.go) wires through gin's catch-all Any(): the Amp control-plane proxy
// routes under /api/*, the Gemini v1beta1 bridge, and the root-level /auth
// alias. Deleting any entry here, or the route it corresponds to in routes.go,
// must fail TestAmpRoutesHandlersPairingSurvives.
var ampManagementAnyPaths = []string{
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
	"/api/provider/google/v1beta1/*path",
	"/auth",
	"/auth/*path",
}

// ampManagementGetOnlyPaths is the exhaustive list of root-level GET-only
// aliases registerManagementRoutes exposes so Amp CLI can hit /threads,
// /docs, /settings, etc. without the /api prefix.
var ampManagementGetOnlyPaths = []string{
	"/threads",
	"/threads/*path",
	"/docs",
	"/docs/*path",
	"/settings",
	"/settings/*path",
	"/threads.rss",
	"/news.rss",
}

// ampProviderAliasRoutes is the exhaustive (method, path) surface
// registerProviderAliases (routes.go) wires under /api/provider/:provider.
// Unlike the management routes above, none of these go through Any(), so each
// entry maps to exactly one gin route.
var ampProviderAliasRoutes = []struct {
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

// routeSet builds a "METHOD path" lookup set from gin's registered routes.
func routeSet(routes gin.RoutesInfo) map[string]bool {
	set := make(map[string]bool, len(routes))
	for _, r := range routes {
		set[r.Method+" "+r.Path] = true
	}
	return set
}

// TestAmpRoutesHandlersPairingSurvives guards the fork-only Amp route surface
// (routes.go registerManagementRoutes + registerProviderAliases) against
// silent shrinkage during upstream restructures. It enumerates every route
// gin actually registers and asserts the full (method, path) set survives
// intact: removing a single route, or narrowing an Any() registration to
// fewer methods, drops the observed route count below the expected total and
// fails the test.
func TestAmpRoutesHandlersPairingSurvives(t *testing.T) {
	gin.SetMode(gin.TestMode)
	base := &handlers.BaseAPIHandler{}

	t.Run("management routes", func(t *testing.T) {
		engine := gin.New()
		m := &AmpModule{restrictToLocalhost: false}
		m.registerManagementRoutes(engine, base, nil)

		got := routeSet(engine.Routes())

		for _, path := range ampManagementAnyPaths {
			for _, method := range ampAnyMethods {
				key := method + " " + path
				if !got[key] {
					t.Fatalf("management route %s missing (Any() registration narrowed or the route was dropped)", key)
				}
			}
		}
		for _, path := range ampManagementGetOnlyPaths {
			key := http.MethodGet + " " + path
			if !got[key] {
				t.Fatalf("management route %s missing", key)
			}
		}

		wantTotal := len(ampManagementAnyPaths)*len(ampAnyMethods) + len(ampManagementGetOnlyPaths)
		if gotTotal := len(engine.Routes()); gotTotal != wantTotal {
			t.Fatalf("registerManagementRoutes registered %d routes, want %d; update the expected path lists above if this route surface change is intentional", gotTotal, wantTotal)
		}
	})

	t.Run("provider alias routes", func(t *testing.T) {
		engine := gin.New()
		noopAuth := func(c *gin.Context) {}
		m := &AmpModule{}
		m.registerProviderAliases(engine, base, noopAuth)

		got := routeSet(engine.Routes())

		for _, r := range ampProviderAliasRoutes {
			key := r.method + " " + r.path
			if !got[key] {
				t.Fatalf("provider alias route %s missing (route or handler dropped)", key)
			}
		}

		if gotTotal := len(engine.Routes()); gotTotal != len(ampProviderAliasRoutes) {
			t.Fatalf("registerProviderAliases registered %d routes, want %d; update ampProviderAliasRoutes above if this route surface change is intentional", gotTotal, len(ampProviderAliasRoutes))
		}
	})
}
