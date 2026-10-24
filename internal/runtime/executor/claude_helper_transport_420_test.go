package executor

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

func TestClaude420SafeTimeoutClassificationNegativeControls(t *testing.T) {
	raw := errors.New("https://user:private@proxy.invalid/account-secret")
	configured := cliproxyexecutor.NewConfiguredTLSHandshakeTimeoutError(errors.Join(context.DeadlineExceeded, raw))
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"configured", configured, true}, {"caller cancellation", context.Canceled, false},
		{"caller deadline", context.DeadlineExceeded, false}, {"text spoof", errors.New("tls handshake timeout: context deadline exceeded"), false},
		{"mixed caller", errors.Join(configured, context.Canceled), false}, {"plain secret", raw, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			safe := claudeUpstreamTransportError(tc.err)
			if safe.Error() != claudeClientSafeTransportErrorMessage || strings.Contains(safe.Error(), "private") || errors.Is(safe, raw) {
				t.Fatalf("unsafe error %v", safe)
			}
			if got := cliproxyexecutor.IsConfiguredTLSHandshakeTimeoutError(safe); got != tc.want {
				t.Fatalf("configured=%v want %v", got, tc.want)
			}
			if tc.want && shouldRetryClaudeTransportError(context.Background(), nil, tc.err, 0) {
				t.Fatal("internal configured deadline retried before Manager")
			}
		})
	}
	if !shouldRetryClaudeTransportError(context.Background(), nil, io.EOF, 0) {
		t.Fatal("ordinary transient EOF retry lost")
	}
}

func TestClaude420DefaultRejectsCallerDeviceSpoof(t *testing.T) {
	cfg := claudeNativeHelperExplicit420Config()
	auth := &cliproxyauth.Auth{Attributes: map[string]string{"api_key": "key-420", "fingerprint_profile": "claude-code-cli"}}
	incoming := claudeNativeHelperHeaders(claudeNativeHelperCoreBetas, "identity")
	incoming.Set("User-Agent", "claude-cli/9.9.999 (external, cli)")
	incoming.Set("X-Stainless-Package-Version", "9.9.999")
	incoming.Set("X-Stainless-Runtime-Version", "v99.0.0")
	incoming.Set("X-Stainless-Async", "async")
	req, _ := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", nil)
	if err := applyClaudeHeaders(req, auth, "key-420", false, nil, []byte(`{"model":"claude-opus-5"}`), cfg, incoming, false, claudeNativeHelperSessionID); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"User-Agent": cfg.ClaudeHeaderDefaults.UserAgent, "X-Stainless-Package-Version": "0.112.1", "X-Stainless-Runtime-Version": "v26.3.0", "X-Stainless-Async": ""} {
		if got := req.Header.Get(name); got != want {
			t.Fatalf("%s=%q want pinned %q", name, got, want)
		}
	}
}

func TestClaude420MissingProxyDoesNotReachTransport(t *testing.T) {
	var reached atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reached.Add(1); w.WriteHeader(200) }))
	defer server.Close()
	auth := claudeNativeHelperOAuthAuth(server.URL)
	auth.ProxyURL = ""
	req := cliproxyexecutor.Request{Model: "claude-haiku-4-5-20251001", Payload: []byte(`{"model":"claude-haiku-4-5-20251001","max_tokens":1,"messages":[{"role":"user","content":"probe"}]}`)}
	// 没有注入 transport，空账户代理必须在拨号之前拒绝。
	_, err := NewClaudeExecutor(&config.Config{}).Execute(context.Background(), auth, req, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude})
	if err == nil || err.Error() != claudeClientSafeTransportErrorMessage || reached.Load() != 0 {
		t.Fatalf("missing proxy error=%v", err)
	}
}
