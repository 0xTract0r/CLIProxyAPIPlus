package executor

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// cchBillingPattern extracts the five-digit billing-header CCH.
var cchBillingPattern = regexp.MustCompile(`(x-anthropic-billing-header:[^"]*?\bcch=)([0-9a-f]{5})(;)`)

func cchFromBody(t *testing.T, body []byte) string {
	t.Helper()
	match := cchBillingPattern.FindSubmatch(body)
	if match == nil {
		t.Fatalf("expected billing header with cch in body: %s", string(body))
	}
	return string(match[2])
}

// Preserve the historical C1 guard entry points while moving their duties to
// the approved protocol: unsigned first-party Count, local custom-gateway Count,
// and signed Messages. The real first-party wire matrix is covered separately.
func TestClaudeExecutorCountTokens_OAuthSignsCCHWithXXHash64(t *testing.T) {
	body := captureClaudeGatewayAfterLocalCount(t, true, false)
	if got, want := cchFromBody(t, body), recomputeExpectedCCH(t, body); got != want || got == "00000" {
		t.Fatalf("gateway Messages CCH=%s want legacy final-byte hash %s", got, want)
	}
}

// Same-body signature stability belongs to Messages, the endpoint that still
// sends CCH. Custom Count must also retain caller guidance in its estimate.
func TestClaudeExecutorCountTokens_SameBodyYieldsSameCCHAsMessages(t *testing.T) {
	body := captureClaudeGatewayAfterLocalCount(t, true, true)
	signed := mustSignClaudeMessagesForIntegration(t, body)
	if !bytes.Equal(body, signed) || !bytes.Equal(signed, mustSignClaudeMessagesForIntegration(t, signed)) {
		t.Fatal("Messages same-body signature changed on repeat signing")
	}
	changed, err := sjson.SetBytes(body, "messages.0.content.0.text", "changed final request")
	if err != nil {
		t.Fatal(err)
	}
	if cchFromBody(t, mustSignClaudeMessagesForIntegration(t, changed)) == cchFromBody(t, body) {
		t.Fatal("legacy Messages signature ignored changed final request bytes")
	}
}

// Preserve the API-key guard against OAuth signing. The upstream gateway gate
// omits CCH; this explicitly replaces the old SHA256 shape rather than claiming
// that an absent CCH retains SHA256.
func TestClaudeExecutorCountTokens_NonOAuthKeepsSha256(t *testing.T) {
	body := captureClaudeGatewayAfterLocalCount(t, false, false)
	if bytes.Contains(body, []byte("cch=")) {
		t.Fatalf("ordinary API-key gateway gained CCH: %s", body)
	}
	if !bytes.Equal(body, mustSignClaudeMessagesForIntegration(t, body)) {
		t.Fatal("unsigned API-key request unexpectedly entered CCH signing")
	}
}

func captureClaudeGatewayAfterLocalCount(t *testing.T, oauth, expandedSystem bool) []byte {
	t.Helper()
	resetClaudeDeviceProfileCache()
	captured := make(chan []byte, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			t.Errorf("custom gateway count must remain local; got %s", r.URL.Path)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		captured <- bytes.Clone(body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg-local","type":"message","role":"assistant","model":"claude-opus-5","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1},"stop_reason":"end_turn"}`)
	}))
	defer server.Close()
	key := "synthetic-gateway-key"
	if oauth {
		key = "sk-ant-oat-synthetic-gateway"
	}
	auth := &cliproxyauth.Auth{ID: t.Name(), Provider: "claude", ProxyURL: "direct", Attributes: map[string]string{"api_key": key, "base_url": server.URL}, Metadata: claudeOAuthTestMetadata()}
	e := NewClaudeExecutor(&config.Config{})
	req := cliproxyexecutor.Request{Model: "claude-opus-5", Payload: []byte(`{"model":"claude-opus-5","messages":[{"role":"user","content":[{"type":"text","text":"hello world"}]}]}`)}
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude}
	counted, err := e.CountTokens(context.Background(), auth, req, opts)
	if err != nil {
		t.Fatal(err)
	}
	baseCount := gjson.GetBytes(counted.Payload, "input_tokens").Int()
	if baseCount <= 0 {
		t.Fatalf("local count missing: %s", counted.Payload)
	}
	if expandedSystem {
		req.Payload, err = sjson.SetBytes(req.Payload, "system", strings.Repeat("Keep all caller guidance in the custom gateway estimate. ", 64))
		if err != nil {
			t.Fatal(err)
		}
		extra, err := e.CountTokens(context.Background(), auth, req, opts)
		if err != nil {
			t.Fatal(err)
		}
		if gjson.GetBytes(extra.Payload, "input_tokens").Int() <= baseCount {
			t.Fatal("local count discarded caller system")
		}
	}
	select {
	case body := <-captured:
		t.Fatalf("Count unexpectedly dialed custom gateway: %s", body)
	default:
	}
	if _, err := e.Execute(context.Background(), auth, req, opts); err != nil {
		t.Fatal(err)
	}
	return <-captured
}

func mustSignClaudeMessagesForIntegration(t *testing.T, body []byte) []byte {
	t.Helper()
	signed, err := signAnthropicMessagesBodyForVersion(body, "2.1.211")
	if err != nil {
		t.Fatalf("signAnthropicMessagesBody: %v", err)
	}
	return signed
}
