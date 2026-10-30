package management

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

type latestReleaseRoundTripFunc func(*http.Request) (*http.Response, error)

func (f latestReleaseRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestGetLatestVersionUsesCanonicalUpstream(t *testing.T) {
	// 此测试只替换默认 transport，不访问网络，也不改变生产 handler 的依赖结构。
	originalTransport := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = originalTransport })

	cases := []struct {
		name         string
		upstreamCode int
		body         string
		requestError error
		wantCode     int
		wantVersion  string
		wantError    string
	}{
		{name: "tag", upstreamCode: http.StatusOK, body: `{"tag_name":" v8.0.47 ","name":"ignored"}`, wantCode: http.StatusOK, wantVersion: "v8.0.47"},
		{name: "name fallback", upstreamCode: http.StatusOK, body: `{"tag_name":" ","name":" v8.0.48 "}`, wantCode: http.StatusOK, wantVersion: "v8.0.48"},
		{name: "missing version", upstreamCode: http.StatusOK, body: `{}`, wantCode: http.StatusBadGateway, wantError: "invalid_response"},
		{name: "invalid JSON", upstreamCode: http.StatusOK, body: `{`, wantCode: http.StatusBadGateway, wantError: "decode_failed"},
		{name: "upstream not found", upstreamCode: http.StatusNotFound, body: `{"message":"Not Found"}`, wantCode: http.StatusBadGateway, wantError: "unexpected_status"},
		{name: "transport error", requestError: errors.New("upstream unavailable"), wantCode: http.StatusBadGateway, wantError: "request_failed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			called := false
			http.DefaultTransport = latestReleaseRoundTripFunc(func(req *http.Request) (*http.Response, error) {
				called = true
				if req.URL.String() != "https://api.github.com/repos/router-for-me/CLIProxyAPI/releases/latest" {
					t.Fatalf("release URL = %q, want current upstream instead of closed CLIProxyAPIPlus repository", req.URL)
				}
				if req.Method != http.MethodGet || req.Header.Get("Accept") != "application/vnd.github+json" || req.Header.Get("User-Agent") == "" {
					t.Fatalf("unexpected release request method/headers: %s %v", req.Method, req.Header)
				}
				if tc.requestError != nil {
					return nil, tc.requestError
				}
				return &http.Response{
					StatusCode: tc.upstreamCode,
					Header:     make(http.Header),
					Body:       io.NopCloser(strings.NewReader(tc.body)),
				}, nil
			})

			recorder := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(recorder)
			ctx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/latest-version", nil)
			(&Handler{}).GetLatestVersion(ctx)
			if !called {
				t.Fatal("release lookup did not use the configured HTTP transport")
			}
			if recorder.Code != tc.wantCode {
				t.Fatalf("status = %d, want %d; body=%s", recorder.Code, tc.wantCode, recorder.Body)
			}
			var result map[string]string
			if err := json.Unmarshal(recorder.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if result["latest-version"] != tc.wantVersion || result["error"] != tc.wantError {
				t.Fatalf("response = %v, want version=%q error=%q", result, tc.wantVersion, tc.wantError)
			}
			if tc.upstreamCode == http.StatusNotFound && !strings.Contains(result["message"], "status 404") {
				t.Fatalf("upstream status should remain available for diagnosis: %v", result)
			}
		})
	}
}
