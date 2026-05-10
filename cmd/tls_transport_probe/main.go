package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

type evidence struct {
	EvidenceType      string         `json:"evidence_type"`
	Provider          string         `json:"provider"`
	AuthProfile       map[string]any `json:"auth_profile"`
	URL               string         `json:"url"`
	RequestSummary    map[string]any `json:"request_summary"`
	RuntimeProfile    map[string]any `json:"runtime_profile"`
	TLS               any            `json:"tls,omitempty"`
	JA3               any            `json:"ja3,omitempty"`
	JA4               any            `json:"ja4,omitempty"`
	HTTP2             any            `json:"http2,omitempty"`
	ALPN              any            `json:"alpn,omitempty"`
	RawResponse       map[string]any `json:"raw_response"`
	Limitations       []string       `json:"limitations"`
	GeneratedAt       string         `json:"generated_at"`
	ProbeTransport    string         `json:"probe_transport"`
	ProviderHostClaim string         `json:"provider_host_claim"`
}

func main() {
	modeFlag := flag.String("mode", "echo", "probe mode: echo, provider-sni, or both")
	urlFlag := flag.String("url", "https://tls.peet.ws/api/all", "TLS echo URL")
	outDirFlag := flag.String("out", "build/t029-raw-tls", "output directory")
	proxyFlag := flag.String("proxy", "direct", "proxy URL or direct")
	claudeHostFlag := flag.String("claude-provider-host", "api.anthropic.com", "provider host/SNI for synthetic Claude local capture")
	codexHostFlag := flag.String("codex-provider-host", "chatgpt.com", "provider host/SNI for synthetic Codex local capture")
	flag.Parse()

	if err := os.MkdirAll(*outDirFlag, 0o755); err != nil {
		exitErr(err)
	}

	probes := []struct {
		name string
		auth *cliproxyauth.Auth
	}{
		{name: "claude-utls", auth: syntheticClaudeAuth()},
		{name: "codex-go", auth: syntheticCodexAuth()},
	}

	for _, probe := range probes {
		switch strings.ToLower(strings.TrimSpace(*modeFlag)) {
		case "echo":
			if err := runProbe(*urlFlag, *outDirFlag, *proxyFlag, probe.name, probe.auth); err != nil {
				exitErr(fmt.Errorf("%s: %w", probe.name, err))
			}
		case "provider-sni":
			if err := runProviderSNIProbe(*outDirFlag, probe.name, providerHostForProbe(probe.auth.Provider, *claudeHostFlag, *codexHostFlag), probe.auth); err != nil {
				exitErr(fmt.Errorf("%s: %w", probe.name, err))
			}
		case "both":
			if err := runProbe(*urlFlag, *outDirFlag, *proxyFlag, probe.name, probe.auth); err != nil {
				exitErr(fmt.Errorf("%s echo: %w", probe.name, err))
			}
			if err := runProviderSNIProbe(*outDirFlag, probe.name, providerHostForProbe(probe.auth.Provider, *claudeHostFlag, *codexHostFlag), probe.auth); err != nil {
				exitErr(fmt.Errorf("%s provider-sni: %w", probe.name, err))
			}
		default:
			exitErr(fmt.Errorf("unsupported mode %q", *modeFlag))
		}
	}
}

func runProbe(rawURL, outDir, proxyURL, name string, auth *cliproxyauth.Auth) error {
	rt, profile, diagnosticLimitation, err := helps.BuildTLSEvidenceProbeRoundTripper(proxyURL, auth)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, rawURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	headerSummary := applySyntheticManagedHeaders(req, auth)

	client := &http.Client{Transport: rt, Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return err
	}

	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		decoded = map[string]any{"parse_error": err.Error()}
	}

	limitations := []string{
		"evidence is from the selected transport builder against the echo host, not proof of provider-observed JA3/JA4",
		"synthetic auth metadata was used; no tokens were read",
	}
	if diagnosticLimitation != "" {
		limitations = append(limitations, diagnosticLimitation)
	}

	ev := evidence{
		EvidenceType:   "echo-host",
		Provider:       auth.Provider,
		AuthProfile:    accountSettings(auth),
		URL:            rawURL,
		RequestSummary: headerSummary,
		RuntimeProfile: profileSummary(profile),
		TLS:            decoded["tls"],
		JA3:            tlsSummary(decoded, "ja3", "ja3_hash"),
		JA4:            tlsSummary(decoded, "ja4", "ja4_r"),
		HTTP2:          firstPresent(decoded, "http2", "http2_settings"),
		ALPN:           firstPresent(decoded, "alpn", "http_version"),
		RawResponse: map[string]any{
			"status":       resp.Status,
			"status_code":  resp.StatusCode,
			"body_sha256":  sha256Hex(body),
			"body_prefix":  stringPrefix(string(body), 2000),
			"content_type": resp.Header.Get("Content-Type"),
		},
		Limitations:       limitations,
		GeneratedAt:       time.Now().UTC().Format(time.RFC3339),
		ProbeTransport:    fmt.Sprintf("%T", rt),
		ProviderHostClaim: "not-claimed: echo service request did not go to provider host",
	}

	outPath := filepath.Join(outDir, name+".json")
	data, err := json.MarshalIndent(ev, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(outPath, append(data, '\n'), 0o644); err != nil {
		return err
	}
	fmt.Println(outPath)
	return nil
}

func runProviderSNIProbe(outDir, name, providerHost string, auth *cliproxyauth.Auth) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	ev, err := helps.CaptureSyntheticProviderSNIEvidence(ctx, auth, providerHost)
	if err != nil {
		return err
	}
	data, err := helps.MarshalSyntheticProviderSNIEvidence(ev)
	if err != nil {
		return err
	}
	outPath := filepath.Join(outDir, "provider-sni-"+name+".json")
	if err := os.WriteFile(outPath, append(data, '\n'), 0o644); err != nil {
		return err
	}
	fmt.Println(outPath)
	return nil
}

func providerHostForProbe(provider, claudeHost, codexHost string) string {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "claude":
		return claudeHost
	case "codex":
		return codexHost
	default:
		return ""
	}
}

func syntheticClaudeAuth() *cliproxyauth.Auth {
	return &cliproxyauth.Auth{
		ID:       "synthetic-claude-tls-probe",
		Provider: "claude",
		Label:    "synthetic claude uTLS probe",
		Metadata: map[string]any{
			"headers": map[string]any{
				"User-Agent":                  "claude-cli/2.1.63 (external, cli)",
				"X-Stainless-Package-Version": "0.74.0",
				"X-Stainless-Runtime-Version": "v24.3.0",
				"X-Stainless-Os":              "MacOS",
				"X-Stainless-Arch":            "arm64",
			},
			"account_settings": map[string]any{
				"transport_profile": map[string]any{
					"provider":   "claude",
					"family":     "utls",
					"profile_id": "claude_utls_chrome_133",
				},
				"tls_profile": map[string]any{
					"provider":   "claude",
					"family":     "utls",
					"profile_id": "claude_utls_chrome_133",
				},
			},
		},
	}
}

func syntheticCodexAuth() *cliproxyauth.Auth {
	return &cliproxyauth.Auth{
		ID:       "synthetic-codex-tls-probe",
		Provider: "codex",
		Label:    "synthetic codex Go transport probe",
		Metadata: map[string]any{
			"headers": map[string]any{
				"User-Agent": "codex_cli_rs/0.124.0 (Mac OS 26.3.1; arm64) iTerm.app/3.6.9 (codex_cli_rs; 0.124.0)",
				"Version":    "0.124.0",
				"Originator": "codex_cli_rs",
			},
			"account_settings": map[string]any{
				"transport_profile": map[string]any{
					"provider":   "codex",
					"family":     "standard",
					"profile_id": "codex_managed_transport_v1",
					"alpn":       []any{"h2", "http/1.1"},
				},
				"tls_profile": map[string]any{
					"provider":   "codex",
					"family":     "go-tls",
					"profile_id": "codex_go_http11_v1",
				},
			},
		},
	}
}

func applySyntheticManagedHeaders(req *http.Request, auth *cliproxyauth.Auth) map[string]any {
	headers := make(map[string]string)
	if auth.Provider == "claude" {
		profile := helps.ResolveClaudeDeviceProfile(auth, "", nil, &config.Config{})
		helps.ApplyClaudeDeviceProfileHeaders(req, profile)
		headers = map[string]string{
			"User-Agent":                  profile.UserAgent,
			"X-Stainless-Package-Version": profile.PackageVersion,
			"X-Stainless-Runtime-Version": profile.RuntimeVersion,
			"X-Stainless-Os":              profile.OS,
			"X-Stainless-Arch":            profile.Arch,
		}
	} else if auth.Provider == "codex" {
		profile := helps.ResolveCodexClientProfile(auth, nil, &config.Config{})
		headers = helps.CodexManagedHeaders(profile)
		for key, value := range headers {
			req.Header.Set(key, value)
		}
	}
	return summarizeHeaders(headers)
}

func summarizeHeaders(headers map[string]string) map[string]any {
	keys := make([]string, 0, len(headers))
	for key := range headers {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	out := map[string]any{"managed_header_keys": keys}
	for _, key := range keys {
		if strings.EqualFold(key, "User-Agent") || strings.EqualFold(key, "Originator") {
			out[strings.ToLower(strings.ReplaceAll(key, "-", "_"))] = headers[key]
		}
	}
	return out
}

func accountSettings(auth *cliproxyauth.Auth) map[string]any {
	if auth == nil || auth.Metadata == nil {
		return nil
	}
	if settings, ok := auth.Metadata["account_settings"].(map[string]any); ok {
		return settings
	}
	return nil
}

func profileSummary(profile *helps.RuntimeTransportProfile) map[string]any {
	if profile == nil {
		return nil
	}
	return map[string]any{
		"provider":                  profile.Provider,
		"family":                    profile.Family,
		"profile_id":                profile.ProfileID,
		"transport_configured":      profile.TransportConfigured,
		"transport_status":          profile.TransportStatus,
		"tls_family":                profile.TLSFamily,
		"tls_profile_id":            profile.TLSProfileID,
		"tls_configured":            profile.TLSConfigured,
		"tls_status":                profile.TLSStatus,
		"alpn":                      profile.ALPN,
		"force_http11":              profile.ForceHTTP11,
		"supports_transport":        profile.SupportsTransportRuntime(),
		"supports_tls":              profile.SupportsTLSRuntime(),
		"provider_mismatch":         profile.ProviderMismatch,
		"runtime_profile_cache_key": profileCacheToken(profile),
	}
}

func profileCacheToken(profile *helps.RuntimeTransportProfile) string {
	auth := &cliproxyauth.Auth{
		Provider: profile.Provider,
		Metadata: map[string]any{
			"account_settings": map[string]any{
				"transport_profile": map[string]any{
					"provider":   profile.Provider,
					"family":     profile.Family,
					"profile_id": profile.ProfileID,
					"alpn":       profile.ALPN,
				},
				"tls_profile": map[string]any{
					"provider":     profile.Provider,
					"family":       profile.TLSFamily,
					"profile_id":   profile.TLSProfileID,
					"force_http11": profile.ForceHTTP11,
				},
			},
		},
	}
	return helps.RuntimeTransportProfileToken(auth)
}

func firstPresent(values map[string]any, keys ...string) any {
	for _, key := range keys {
		if value, ok := values[key]; ok {
			return value
		}
	}
	return nil
}

func tlsSummary(values map[string]any, keys ...string) any {
	rawTLS, ok := values["tls"].(map[string]any)
	if !ok {
		return firstPresent(values, keys...)
	}
	out := make(map[string]any)
	for _, key := range keys {
		if value, ok := rawTLS[key]; ok {
			out[key] = value
		}
	}
	if len(out) == 0 {
		return firstPresent(values, keys...)
	}
	return out
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func stringPrefix(value string, max int) string {
	if len(value) <= max {
		return value
	}
	return value[:max]
}

func exitErr(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
