package management

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

func newProxyProbeHandler(t *testing.T) *Handler {
	t.Helper()
	gin.SetMode(gin.TestMode)
	manager := coreauth.NewManager(&memoryAuthStore{}, nil, nil)
	return NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)
}

func callProxyProbe(t *testing.T, h *Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, "/v0/management/diagnostics/proxy-connectivity-probe", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	ctx.Request = req
	h.RunProxyConnectivityProbe(ctx)
	return rec
}

func decodeProxyProbe(t *testing.T, rec *httptest.ResponseRecorder) proxyConnectivityProbeResult {
	t.Helper()
	var resp proxyConnectivityProbeResult
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v; body=%s", err, rec.Body.String())
	}
	return resp
}

// failIfProbed installs an inner network seam that fails the test if invoked, so
// format-rejection paths can be proven to perform no network I/O.
func failIfProbed(t *testing.T) {
	t.Helper()
	original := probeProxyExitIP
	t.Cleanup(func() { probeProxyExitIP = original })
	probeProxyExitIP = func(context.Context, *http.Client, string) proxyConnectivityProbeResult {
		t.Fatal("network probe must not run for empty/invalid/direct proxy_url")
		return proxyConnectivityProbeResult{}
	}
}

func TestRunProxyConnectivityProbeEmptyProxyURLDoesNotDial(t *testing.T) {
	failIfProbed(t)
	h := newProxyProbeHandler(t)

	rec := callProxyProbe(t, h, `{"proxy_url":""}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	resp := decodeProxyProbe(t, rec)
	if resp.OK {
		t.Fatalf("ok = true, want false; resp=%#v", resp)
	}
	if resp.Reason != proxyProbeReasonEmpty {
		t.Fatalf("reason = %q, want %q", resp.Reason, proxyProbeReasonEmpty)
	}
	if resp.ExitIP != "" {
		t.Fatalf("exit_ip = %q, want empty", resp.ExitIP)
	}
}

func TestRunProxyConnectivityProbeInvalidProxyURLDoesNotDial(t *testing.T) {
	// Whitespace-only also normalizes to empty, so keep it out of this table.
	cases := map[string]string{
		"bare_number":        "999",
		"malformed_scheme":   "ht!tp://",
		"unsupported_scheme": "ftp://user:pass@host:21",
		"missing_host":       "http://",
		"direct_keyword":     "direct",
		"none_keyword":       "none",
	}
	for name, proxyURL := range cases {
		t.Run(name, func(t *testing.T) {
			failIfProbed(t)
			h := newProxyProbeHandler(t)

			payload, _ := json.Marshal(proxyConnectivityProbeRequest{ProxyURL: proxyURL})
			rec := callProxyProbe(t, h, string(payload))
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
			}
			resp := decodeProxyProbe(t, rec)
			if resp.OK {
				t.Fatalf("ok = true, want false; resp=%#v", resp)
			}
			if resp.Reason != proxyProbeReasonInvalid {
				t.Fatalf("reason = %q, want %q", resp.Reason, proxyProbeReasonInvalid)
			}
			if resp.ExitIP != "" {
				t.Fatalf("exit_ip = %q, want empty", resp.ExitIP)
			}
		})
	}
}

func TestRunProxyConnectivityProbeValidProxyReturnsExitIP(t *testing.T) {
	original := runProxyConnectivityProbe
	t.Cleanup(func() { runProxyConnectivityProbe = original })
	var seenRaw string
	runProxyConnectivityProbe = func(_ context.Context, raw string) proxyConnectivityProbeResult {
		seenRaw = raw
		return proxyConnectivityProbeResult{OK: true, ExitIP: "203.0.113.7", Reason: proxyProbeReasonOK}
	}

	h := newProxyProbeHandler(t)
	rec := callProxyProbe(t, h, `{"proxy_url":"socks5://user:pass@1.2.3.4:1080"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	resp := decodeProxyProbe(t, rec)
	if !resp.OK {
		t.Fatalf("ok = false, want true; resp=%#v", resp)
	}
	if resp.ExitIP != "203.0.113.7" {
		t.Fatalf("exit_ip = %q, want 203.0.113.7", resp.ExitIP)
	}
	if resp.Reason != proxyProbeReasonOK {
		t.Fatalf("reason = %q, want %q", resp.Reason, proxyProbeReasonOK)
	}
	if seenRaw != "socks5://user:pass@1.2.3.4:1080" {
		t.Fatalf("probe received raw = %q, want trimmed proxy_url", seenRaw)
	}
}

func TestRunProxyConnectivityProbeNonJSONBodyReturns400(t *testing.T) {
	failIfProbed(t)
	h := newProxyProbeHandler(t)

	rec := callProxyProbe(t, h, `this is not json`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

func TestRunProxyConnectivityProbeRedactsCredentials(t *testing.T) {
	const secret = "s3cr3tpass"
	const proxyURL = "http://user:" + secret + "@residential.example:8080"

	// Stub the inner network seam so a real dial is not attempted; the outer
	// real function still runs (parse + redacted log) so we can assert the
	// credential never leaks to logs or the response body.
	originalSeam := probeProxyExitIP
	t.Cleanup(func() { probeProxyExitIP = originalSeam })
	probeProxyExitIP = func(context.Context, *http.Client, string) proxyConnectivityProbeResult {
		return proxyConnectivityProbeResult{OK: true, ExitIP: "198.51.100.5", Reason: proxyProbeReasonOK}
	}

	var buf bytes.Buffer
	originalOut := log.StandardLogger().Out
	originalLevel := log.GetLevel()
	log.SetOutput(&buf)
	log.SetLevel(log.DebugLevel)
	t.Cleanup(func() {
		log.SetOutput(originalOut)
		log.SetLevel(originalLevel)
	})

	h := newProxyProbeHandler(t)
	payload, _ := json.Marshal(proxyConnectivityProbeRequest{ProxyURL: proxyURL})
	rec := callProxyProbe(t, h, string(payload))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	if strings.Contains(rec.Body.String(), secret) {
		t.Fatalf("response body leaked proxy credential: %s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), proxyURL) {
		t.Fatalf("response body echoed proxy_url: %s", rec.Body.String())
	}
	logged := buf.String()
	if strings.Contains(logged, secret) {
		t.Fatalf("logs leaked proxy credential: %s", logged)
	}
	if !strings.Contains(logged, "redacted") {
		t.Fatalf("expected redacted proxy marker in logs, got: %s", logged)
	}
}
