package kiro

import (
	"net/http"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

// TestNewSSOOIDCClientWithProxyURL_OverrideTakesPrecedence verifies the
// per-account proxy override applied at Refresh time wins over a globally
// configured cfg.ProxyURL for the AWS SSO OIDC refresh path. Regression
// coverage for the bug where KiroExecutor.Refresh dropped to cfg.ProxyURL.
func TestNewSSOOIDCClientWithProxyURL_OverrideTakesPrecedence(t *testing.T) {
	cfg := &config.Config{SDKConfig: config.SDKConfig{ProxyURL: "http://global.example.com:8080"}}
	client := NewSSOOIDCClientWithProxyURL(cfg, "http://override.example.com:8081")

	transport, ok := client.httpClient.Transport.(*http.Transport)
	if !ok || transport == nil {
		t.Fatalf("expected *http.Transport, got %T", client.httpClient.Transport)
	}
	req, errReq := http.NewRequest(http.MethodGet, "https://oidc.us-east-1.amazonaws.com", nil)
	if errReq != nil {
		t.Fatalf("new request: %v", errReq)
	}
	proxyURL, errProxy := transport.Proxy(req)
	if errProxy != nil {
		t.Fatalf("proxy func: %v", errProxy)
	}
	if proxyURL == nil || proxyURL.String() != "http://override.example.com:8081" {
		t.Fatalf("proxy URL = %v, want http://override.example.com:8081", proxyURL)
	}
}

// TestNewSSOOIDCClientWithProxyURL_EmptyOverrideFallsBackToCfg verifies that
// when the account does not specify a proxy override the constructor falls
// back to the global cfg.ProxyURL, preserving the historical default
// behavior.
func TestNewSSOOIDCClientWithProxyURL_EmptyOverrideFallsBackToCfg(t *testing.T) {
	cfg := &config.Config{SDKConfig: config.SDKConfig{ProxyURL: "http://global.example.com:8080"}}
	client := NewSSOOIDCClientWithProxyURL(cfg, "")

	transport, ok := client.httpClient.Transport.(*http.Transport)
	if !ok || transport == nil {
		t.Fatalf("expected *http.Transport, got %T", client.httpClient.Transport)
	}
	req, errReq := http.NewRequest(http.MethodGet, "https://oidc.us-east-1.amazonaws.com", nil)
	if errReq != nil {
		t.Fatalf("new request: %v", errReq)
	}
	proxyURL, errProxy := transport.Proxy(req)
	if errProxy != nil {
		t.Fatalf("proxy func: %v", errProxy)
	}
	if proxyURL == nil || proxyURL.String() != "http://global.example.com:8080" {
		t.Fatalf("proxy URL = %v, want http://global.example.com:8080", proxyURL)
	}
}

// TestNewSSOOIDCClient_DefaultStillUsesCfgProxyURL guards backward
// compatibility for the legacy NewSSOOIDCClient ctor used by first-time
// login paths without auth context.
func TestNewSSOOIDCClient_DefaultStillUsesCfgProxyURL(t *testing.T) {
	cfg := &config.Config{SDKConfig: config.SDKConfig{ProxyURL: "http://global.example.com:8080"}}
	client := NewSSOOIDCClient(cfg)

	transport, ok := client.httpClient.Transport.(*http.Transport)
	if !ok || transport == nil {
		t.Fatalf("expected *http.Transport, got %T", client.httpClient.Transport)
	}
	req, errReq := http.NewRequest(http.MethodGet, "https://oidc.us-east-1.amazonaws.com", nil)
	if errReq != nil {
		t.Fatalf("new request: %v", errReq)
	}
	proxyURL, errProxy := transport.Proxy(req)
	if errProxy != nil {
		t.Fatalf("proxy func: %v", errProxy)
	}
	if proxyURL == nil || proxyURL.String() != "http://global.example.com:8080" {
		t.Fatalf("proxy URL = %v, want http://global.example.com:8080", proxyURL)
	}
}

// TestNewKiroOAuthWithProxyURL_OverrideTakesPrecedence verifies the
// per-account proxy override wins over cfg.ProxyURL for the Kiro social-auth
// (Google/GitHub) refresh path.
func TestNewKiroOAuthWithProxyURL_OverrideTakesPrecedence(t *testing.T) {
	cfg := &config.Config{SDKConfig: config.SDKConfig{ProxyURL: "http://global.example.com:8080"}}
	oauth := NewKiroOAuthWithProxyURL(cfg, "http://override.example.com:8081")

	transport, ok := oauth.httpClient.Transport.(*http.Transport)
	if !ok || transport == nil {
		t.Fatalf("expected *http.Transport, got %T", oauth.httpClient.Transport)
	}
	req, errReq := http.NewRequest(http.MethodGet, "https://prod.us-east-1.auth.desktop.kiro.dev", nil)
	if errReq != nil {
		t.Fatalf("new request: %v", errReq)
	}
	proxyURL, errProxy := transport.Proxy(req)
	if errProxy != nil {
		t.Fatalf("proxy func: %v", errProxy)
	}
	if proxyURL == nil || proxyURL.String() != "http://override.example.com:8081" {
		t.Fatalf("proxy URL = %v, want http://override.example.com:8081", proxyURL)
	}
}

// TestNewKiroOAuthWithProxyURL_EmptyOverrideFallsBackToCfg verifies the empty
// override falls back to cfg.ProxyURL.
func TestNewKiroOAuthWithProxyURL_EmptyOverrideFallsBackToCfg(t *testing.T) {
	cfg := &config.Config{SDKConfig: config.SDKConfig{ProxyURL: "http://global.example.com:8080"}}
	oauth := NewKiroOAuthWithProxyURL(cfg, "")

	transport, ok := oauth.httpClient.Transport.(*http.Transport)
	if !ok || transport == nil {
		t.Fatalf("expected *http.Transport, got %T", oauth.httpClient.Transport)
	}
	req, errReq := http.NewRequest(http.MethodGet, "https://prod.us-east-1.auth.desktop.kiro.dev", nil)
	if errReq != nil {
		t.Fatalf("new request: %v", errReq)
	}
	proxyURL, errProxy := transport.Proxy(req)
	if errProxy != nil {
		t.Fatalf("proxy func: %v", errProxy)
	}
	if proxyURL == nil || proxyURL.String() != "http://global.example.com:8080" {
		t.Fatalf("proxy URL = %v, want http://global.example.com:8080", proxyURL)
	}
}

// TestNewKiroOAuth_DefaultStillUsesCfgProxyURL guards backward compatibility
// for the legacy NewKiroOAuth ctor.
func TestNewKiroOAuth_DefaultStillUsesCfgProxyURL(t *testing.T) {
	cfg := &config.Config{SDKConfig: config.SDKConfig{ProxyURL: "http://global.example.com:8080"}}
	oauth := NewKiroOAuth(cfg)

	transport, ok := oauth.httpClient.Transport.(*http.Transport)
	if !ok || transport == nil {
		t.Fatalf("expected *http.Transport, got %T", oauth.httpClient.Transport)
	}
	req, errReq := http.NewRequest(http.MethodGet, "https://prod.us-east-1.auth.desktop.kiro.dev", nil)
	if errReq != nil {
		t.Fatalf("new request: %v", errReq)
	}
	proxyURL, errProxy := transport.Proxy(req)
	if errProxy != nil {
		t.Fatalf("proxy func: %v", errProxy)
	}
	if proxyURL == nil || proxyURL.String() != "http://global.example.com:8080" {
		t.Fatalf("proxy URL = %v, want http://global.example.com:8080", proxyURL)
	}
}
