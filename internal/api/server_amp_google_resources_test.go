package api

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestOrdinaryAmpGoogleResourceMethodsRequireBoundCaller(t *testing.T) {
	const prefix = "/api/provider/google/v1beta1"
	const allow = "synthetic-google-a"
	const deny = "synthetic-google-b"
	const upstream = "synthetic-google-upstream-a"
	const legacy = "synthetic-google-upstream-shared"
	const target = "https://amp-google-resource.example.invalid/service"
	cfg := &config.Config{
		AmpCode: config.AmpCode{
			UpstreamURL: target, UpstreamAPIKey: legacy,
			UpstreamAPIKeys: []config.AmpUpstreamAPIKeyEntry{{APIKeys: []string{allow}, UpstreamAPIKey: upstream}},
		},
	}
	cfg.APIKeys = []string{allow, deny}
	s := newTestServerWithConfig(t, cfg, WithIsolatedUsage())
	type request struct{ method, path, body string }
	var want request
	calls := 0
	fake := ampTenantTransport(func(req *http.Request) (*http.Response, error) {
		calls++
		body := ""
		if req.Body != nil {
			raw, err := io.ReadAll(req.Body)
			if err != nil {
				t.Fatal(err)
			}
			_ = req.Body.Close()
			body = string(raw)
		}
		if req.URL.Scheme != "https" || req.URL.Host != "amp-google-resource.example.invalid" ||
			req.URL.Path != "/service"+want.path || req.Method != want.method || body != want.body ||
			req.URL.Query().Encode() != "pageSize=2" || req.Header.Get("Authorization") != "Bearer "+upstream ||
			req.Header.Get("X-Api-Key") != upstream || req.Header.Get("Cookie") != "" ||
			req.Header.Get("X-Serverless-Authorization") != "" {
			t.Fatal("method, resource, body, filter, or caller binding changed at the pinned upstream")
		}
		header := make(http.Header)
		status := http.StatusNoContent
		if want.method == http.MethodPatch {
			status = http.StatusForbidden
			header.Set("Retry-After", "20")
		}
		return &http.Response{StatusCode: status, Header: header, Body: http.NoBody}, nil
	})
	s.ampForward.Load().proxy.Transport = fake

	call := func(method, endpoint, principal, body string, status int, forwards bool) {
		t.Helper()
		before := calls
		want = request{method, endpoint, body}
		req := ampFixtureRequest(http.MethodGet, endpoint+"?pageSize=2&key=ignored&OAuth_Token=ignored", body, "127.0.0.1:9001", principal)
		req.Method = method
		req.Header.Set("Cookie", "browser-login=synthetic")
		req.Header.Set("X-Serverless-Authorization", "Bearer synthetic-alternative")
		ctx, cancel := context.WithCancel(req.Context())
		defer cancel()
		resp := httptest.NewRecorder()
		s.Handler().ServeHTTP(resp, req.WithContext(ctx))
		allowedCalls := 0
		if forwards {
			allowedCalls = 1
		}
		if resp.Code != status || calls-before != allowedCalls {
			t.Fatalf("%s %s returned %d (%d upstream calls), want %d (%d upstream calls)", method, endpoint, resp.Code, calls-before, status, allowedCalls)
		}
		if status == http.StatusForbidden && forwards && resp.Header().Get("Retry-After") != "20" {
			t.Fatal("upstream refusal and response metadata were lost")
		}
		if strings.Contains(resp.Body.String(), upstream) || strings.Contains(resp.Body.String(), legacy) {
			t.Fatal("credential exposed to caller")
		}
	}
	files := prefix + "/projects/fixture/locations/fixture/files"
	call(http.MethodGet, files, allow, "", http.StatusNoContent, true)
	call(http.MethodHead, prefix+"/publishers/google/models", allow, "", http.StatusNoContent, true)
	call(http.MethodPost, files, allow, `{"displayName":"ordinary upload"}`, http.StatusNoContent, true)
	call(http.MethodPost, prefix+"/projects/fixture/models/resource:upload", allow, `{"file":"fixture"}`, http.StatusNoContent, true)
	call(http.MethodPut, files+"/sample", allow, `{"value":"updated"}`, http.StatusNoContent, true)
	call(http.MethodPatch, files+"/sample", allow, `{"value":"refused"}`, http.StatusForbidden, true)
	call(http.MethodDelete, files+"/sample", allow, "", http.StatusNoContent, true)
	call(http.MethodGet, files, deny, "", http.StatusForbidden, false)
	call(http.MethodGet, files, "", "", http.StatusUnauthorized, false)
	call(http.MethodConnect, files, allow, "", http.StatusNotFound, false)
	call(http.MethodTrace, files, allow, "", http.StatusNotFound, false)
	call(http.MethodGet, "/api/provider/anthropic/v1beta1/projects/fixture/files", allow, "", http.StatusNotFound, false)
	call(http.MethodGet, prefix+"/projects/../private", allow, "", http.StatusBadRequest, false)

	next := s.getConfig().CloneForRuntime()
	next.AmpCode.UpstreamAPIKeys = nil
	if !s.UpdateClientsContext(context.Background(), next) {
		t.Fatal("reload failed")
	}
	s.ampForward.Load().proxy.Transport = fake
	call(http.MethodPost, files, allow, `{"displayName":"revoked"}`, http.StatusForbidden, false)
}

func TestOrdinaryAmpExplicitMissingTargetNeverUsesGoogleUpstream(t *testing.T) {
	s := newAmpForwardingFixture(t, config.AmpCode{
		UpstreamURL:     "https://amp-google-resource.example.invalid",
		UpstreamAPIKeys: []config.AmpUpstreamAPIKeyEntry{{APIKeys: []string{ampInboundFixture}, UpstreamAPIKey: "synthetic-google-upstream"}},
		ModelMappings:   []config.AmpModelMapping{{From: "missing-local-alias", To: "missing-local-target"}},
	})
	s.ampForward.Load().proxy.Transport = ampTenantTransport(func(*http.Request) (*http.Response, error) {
		t.Fatal("explicit alias to unavailable target escaped to upstream")
		return nil, nil
	})
	req := ampFixtureRequest(http.MethodPost, "/api/provider/google/v1beta1/publishers/google/models/missing-local-alias:generateContent",
		`{"contents":[{"parts":[{"text":"fixture"}]}]}`, "127.0.0.1:9001", ampInboundFixture)
	resp := httptest.NewRecorder()
	s.Handler().ServeHTTP(resp, req)
	if resp.Code < 400 {
		t.Fatal("unavailable local mapping was accepted")
	}
}

func TestOrdinaryAmpGoogleGzipResponseProtectsBoundKey(t *testing.T) {
	const secret = "synthetic-compressed-upstream-key"
	s := newAmpForwardingFixture(t, config.AmpCode{
		UpstreamURL:     "https://amp-google-resource.example.invalid",
		UpstreamAPIKeys: []config.AmpUpstreamAPIKeyEntry{{APIKeys: []string{ampInboundFixture}, UpstreamAPIKey: secret}},
	})
	var compressed bytes.Buffer
	z := gzip.NewWriter(&compressed)
	_, _ = io.WriteString(z, `{"resource":"fixture","reflected":"`+secret+`"}`)
	if err := z.Close(); err != nil {
		t.Fatal(err)
	}
	s.ampForward.Load().proxy.Transport = ampTenantTransport(func(req *http.Request) (*http.Response, error) {
		if req.Header.Get("X-Api-Key") != secret {
			t.Fatal("wrong key for the selected upstream")
		}
		headers := make(http.Header)
		headers.Set("Content-Type", "application/json")
		headers.Set("Content-Encoding", "gzip")
		return &http.Response{StatusCode: http.StatusOK, Header: headers,
			Body: io.NopCloser(bytes.NewReader(compressed.Bytes())), ContentLength: int64(compressed.Len())}, nil
	})
	req := ampFixtureRequest(http.MethodGet, "/api/provider/google/v1beta1/projects/fixture/files", "", "127.0.0.1:9001", ampInboundFixture)
	ctx, cancel := context.WithCancel(req.Context())
	defer cancel()
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req.WithContext(ctx))
	if w.Code != http.StatusOK || w.Header().Get("Content-Encoding") != "" ||
		!strings.Contains(w.Body.String(), `"resource":"fixture"`) || strings.Contains(w.Body.String(), secret) {
		t.Fatal("compressed authorized response was lost or reflected its upstream key")
	}
}
