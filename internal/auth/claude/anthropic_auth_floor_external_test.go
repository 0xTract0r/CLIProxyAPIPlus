package claude_test

import (
	"context"
	claude "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/claude"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	"io"
	"net/http"
	"strings"
	"testing"
)

type floorCaptureTransport func(*http.Request) (*http.Response, error)

func (f floorCaptureTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// The external test observes the real OAuth request without reversing the
// production helps -> claude dependency or exposing private auth internals.
func TestClaudeOAuthUserAgentMatchesDeviceProfileFloor(t *testing.T) {
	var captured string
	service := claude.NewClaudeAuthWithHTTPClient(&http.Client{Transport: floorCaptureTransport(func(req *http.Request) (*http.Response, error) {
		if req.Method == http.MethodPost && req.URL.Path == "/v1/oauth/token" {
			captured = req.Header.Get("User-Agent")
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"access_token":"floor-access","refresh_token":"floor-refresh","expires_in":3600}`)), Request: req}, nil
	})})
	if _, err := service.RefreshTokens(context.Background(), "external-floor-guard"); err != nil {
		t.Fatal(err)
	}
	got, ok := helps.ClaudeVersionFromUserAgent(captured)
	if !ok {
		t.Fatalf("OAuth User-Agent %q is not a claude-cli identity", captured)
	}
	want := helps.DefaultClaudeVersion(nil)
	if got != want {
		t.Fatalf("OAuth version %q != device-profile floor %q", got, want)
	}
	if wantUA := "claude-cli/" + want + " (external, cli)"; captured != wantUA {
		t.Fatalf("OAuth User-Agent %q != %q", captured, wantUA)
	}
}
