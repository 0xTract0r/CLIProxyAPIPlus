package management

import (
	"net/http"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

// TestAPICallTransport_ClaudeUsesUtls is an anti-correlation red line. Before the
// fix, the management api-call tool returned a bare *http.Transport (Go-default
// TLS, JA3 03117a8e) for every account, so the dashboard's periodic
// chatgpt.com / api.anthropic.com probes carried a real account token over a
// distinguishable TLS stack. The claude api-call transport must now route through
// the serving-grade uTLS round tripper (replicated claude-cli ClientHello).
func TestAPICallTransport_ClaudeUsesUtls(t *testing.T) {
	h := &Handler{cfg: &config.Config{}}
	rt := h.apiCallTransport(&coreauth.Auth{Provider: "claude"})
	if _, isStd := rt.(*http.Transport); isStd {
		t.Fatal("claude api-call transport is *http.Transport (Go-default TLS leak); want uTLS round tripper")
	}
	obs, ok := rt.(helps.RuntimeHelloObserver)
	if !ok {
		t.Fatalf("claude api-call transport %T does not implement RuntimeHelloObserver (not a uTLS round tripper)", rt)
	}
	if obs.RuntimeHelloState().ConfiguredHello == "" {
		t.Fatal("claude api-call uTLS ConfiguredHello empty; expected HelloCustom (claude-cli)")
	}
}

// TestAPICallTransport_CodexUsesUtls mirrors the claude red line for codex
// accounts (replicated codex-rs ClientHello).
func TestAPICallTransport_CodexUsesUtls(t *testing.T) {
	h := &Handler{cfg: &config.Config{}}
	rt := h.apiCallTransport(&coreauth.Auth{Provider: "codex"})
	if _, isStd := rt.(*http.Transport); isStd {
		t.Fatal("codex api-call transport is *http.Transport (Go-default TLS leak); want uTLS round tripper")
	}
	obs, ok := rt.(helps.RuntimeHelloObserver)
	if !ok {
		t.Fatalf("codex api-call transport %T is not a uTLS round tripper", rt)
	}
	if obs.RuntimeHelloState().ConfiguredHello == "" {
		t.Fatal("codex api-call uTLS ConfiguredHello empty; expected HelloCustom (codex-rs)")
	}
}

// TestAPICallTransport_OtherProviderKeepsStdLib guards that providers without a
// replicated serving fingerprint (gemini / copilot / api_key) are NOT forced onto
// a claude/codex ClientHello and keep the hardened standard-library transport.
func TestAPICallTransport_OtherProviderKeepsStdLib(t *testing.T) {
	h := &Handler{cfg: &config.Config{}}
	rt := h.apiCallTransport(&coreauth.Auth{Provider: "gemini"})
	if _, ok := rt.(*http.Transport); !ok {
		t.Fatalf("gemini api-call transport = %T, want *http.Transport (hardened std-lib path)", rt)
	}
}

// TestAPICallTransport_NilAuthKeepsStdLib guards the nil-auth fallback.
func TestAPICallTransport_NilAuthKeepsStdLib(t *testing.T) {
	h := &Handler{cfg: &config.Config{}}
	rt := h.apiCallTransport(nil)
	if _, ok := rt.(*http.Transport); !ok {
		t.Fatalf("nil-auth api-call transport = %T, want *http.Transport", rt)
	}
}

// TestAPICallResolvedProxyURL_ClaudeCodexAPIKey keeps the claude/codex api-key proxy
// chain explicitly covered after they moved off the std-lib transport: their proxy is
// resolved by apiCallResolvedProxyURL (same chain the std-lib path uses) from the
// per-key ClaudeKey/CodexKey config, asserted directly by string (survives the uTLS
// switch, which the type-based table assertion could not).
func TestAPICallResolvedProxyURL_ClaudeCodexAPIKey(t *testing.T) {
	h := &Handler{cfg: &config.Config{
		ClaudeKey: []config.ClaudeKey{{APIKey: "claude-key", ProxyURL: "http://claude-proxy.example.com:8080"}},
		CodexKey:  []config.CodexKey{{APIKey: "codex-key", ProxyURL: "http://codex-proxy.example.com:8080"}},
	}}
	if got := h.apiCallResolvedProxyURL(&coreauth.Auth{Provider: "claude", Attributes: map[string]string{"api_key": "claude-key"}}); got != "http://claude-proxy.example.com:8080" {
		t.Fatalf("claude resolved proxy = %q, want http://claude-proxy.example.com:8080", got)
	}
	if got := h.apiCallResolvedProxyURL(&coreauth.Auth{Provider: "codex", Attributes: map[string]string{"api_key": "codex-key"}}); got != "http://codex-proxy.example.com:8080" {
		t.Fatalf("codex resolved proxy = %q, want http://codex-proxy.example.com:8080", got)
	}
}
