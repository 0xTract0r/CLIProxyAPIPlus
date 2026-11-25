package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/buildinfo"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestBuildInfoHeadersOnOrdinaryRoutes(t *testing.T) {
	cfg := &config.Config{}
	cfg.MaxRequestBodyBytes = 4
	server := newTestServerWithConfig(t, cfg)
	for _, tc := range []struct {
		method, path, body string
		status             int
	}{
		{http.MethodGet, "/healthz", "", http.StatusOK},
		{http.MethodHead, "/healthz", "", http.StatusOK},
		{http.MethodOptions, "/v1/models", "", http.StatusNoContent},
		{http.MethodPost, "/v1/responses", "oversized", http.StatusRequestEntityTooLarge},
		{http.MethodGet, "/missing-build-proof-route", "", http.StatusNotFound},
	} {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
			out := httptest.NewRecorder()
			server.engine.ServeHTTP(out, req)
			if out.Code != tc.status {
				t.Fatalf("status = %d, want %d", out.Code, tc.status)
			}
			for header, want := range map[string]string{
				"X-CPA-Version":    buildinfo.Version,
				"X-CPA-Commit":     buildinfo.Commit,
				"X-CPA-Build-Date": buildinfo.BuildDate,
			} {
				if got := out.Header().Get(header); want == "" || got != want {
					t.Errorf("%s = %q, want %q", header, got, want)
				}
			}
		})
	}
}
