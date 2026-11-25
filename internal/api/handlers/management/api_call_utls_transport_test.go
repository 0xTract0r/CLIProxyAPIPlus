package management

import (
	"net/http"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestAPICallTransport_OtherProviderKeepsStdLib(t *testing.T) {
	h := &Handler{cfg: &config.Config{}}
	rt := h.apiCallTransport(&coreauth.Auth{Provider: "gemini"}, "")
	if _, ok := rt.(*http.Transport); !ok {
		t.Fatalf("gemini api-call transport = %T, want *http.Transport (hardened std-lib path)", rt)
	}
}

func TestAPICallTransport_NilAuthKeepsStdLib(t *testing.T) {
	h := &Handler{cfg: &config.Config{}}
	rt := h.apiCallTransport(nil, "")
	if _, ok := rt.(*http.Transport); !ok {
		t.Fatalf("nil-auth api-call transport = %T, want *http.Transport", rt)
	}
}

func TestAPICallResolvedProxyURL_ClaudeCodexAPIKey(t *testing.T) {
	h := &Handler{cfg: &config.Config{
		ClaudeKey: []config.ClaudeKey{{APIKey: "claude-key", ProxyURL: "http://claude-proxy.example.com:8080"}},
		CodexKey:  []config.CodexKey{{APIKey: "codex-key", ProxyURL: "http://codex-proxy.example.com:8080"}},
	}}
	if got := h.apiCallProxy(&coreauth.Auth{Provider: "claude", Attributes: map[string]string{"api_key": "claude-key"}}); got != "http://claude-proxy.example.com:8080" {
		t.Fatalf("claude resolved proxy = %q, want http://claude-proxy.example.com:8080", got)
	}
	if got := h.apiCallProxy(&coreauth.Auth{Provider: "codex", Attributes: map[string]string{"api_key": "codex-key"}}); got != "http://codex-proxy.example.com:8080" {
		t.Fatalf("codex resolved proxy = %q, want http://codex-proxy.example.com:8080", got)
	}
}
