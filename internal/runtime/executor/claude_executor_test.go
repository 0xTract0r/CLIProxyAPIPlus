package executor

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/klauspost/compress/zstd"
	xxHash64 "github.com/pierrec/xxHash/xxHash64"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func resetClaudeDeviceProfileCache() {
	helps.ResetClaudeDeviceProfileCache()
}

func newClaudeHeaderTestRequest(t *testing.T, incoming http.Header) *http.Request {
	t.Helper()

	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginReq := httptest.NewRequest(http.MethodPost, "http://localhost/v1/messages", nil)
	ginReq.Header = incoming.Clone()
	ginCtx.Request = ginReq

	req := httptest.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", nil)
	return req.WithContext(context.WithValue(req.Context(), "gin", ginCtx))
}

func assertClaudeFingerprint(t *testing.T, headers http.Header, userAgent, pkgVersion, runtimeVersion, osName, arch string) {
	t.Helper()

	if got := headers.Get("User-Agent"); got != userAgent {
		t.Fatalf("User-Agent = %q, want %q", got, userAgent)
	}
	if got := headers.Get("X-Stainless-Package-Version"); got != pkgVersion {
		t.Fatalf("X-Stainless-Package-Version = %q, want %q", got, pkgVersion)
	}
	if got := headers.Get("X-Stainless-Runtime-Version"); got != runtimeVersion {
		t.Fatalf("X-Stainless-Runtime-Version = %q, want %q", got, runtimeVersion)
	}
	if got := headers.Get("X-Stainless-Os"); got != osName {
		t.Fatalf("X-Stainless-Os = %q, want %q", got, osName)
	}
	if got := headers.Get("X-Stainless-Arch"); got != arch {
		t.Fatalf("X-Stainless-Arch = %q, want %q", got, arch)
	}
}

func billingVersionFromBody(t *testing.T, body []byte) string {
	t.Helper()
	billingHeader := gjson.GetBytes(body, "system.0.text").String()
	match := regexp.MustCompile(`\bcc_version=([0-9]+\.[0-9]+\.[0-9]+)\.`).FindStringSubmatch(billingHeader)
	if len(match) != 2 {
		t.Fatalf("expected billing cc_version in body, got system.0.text=%q body=%s", billingHeader, string(body))
	}
	return match[1]
}

func billingEntrypointFromBody(t *testing.T, body []byte) string {
	t.Helper()
	billingHeader := gjson.GetBytes(body, "system.0.text").String()
	match := regexp.MustCompile(`cc_entrypoint=([^;]+);`).FindStringSubmatch(billingHeader)
	if len(match) != 2 {
		t.Fatalf("expected billing cc_entrypoint in body, got system.0.text=%q body=%s", billingHeader, string(body))
	}
	return strings.TrimSpace(match[1])
}

func userAgentSuffixEntrypoint(userAgent string) string {
	start := strings.Index(userAgent, "(")
	end := strings.LastIndex(userAgent, ")")
	if start < 0 || end <= start {
		return ""
	}
	parts := strings.Split(userAgent[start+1:end], ",")
	if len(parts) >= 2 {
		return strings.TrimSpace(parts[1])
	}
	return ""
}

func TestApplyClaudeHeaders_UsesConfiguredBaselineFingerprint(t *testing.T) {
	resetClaudeDeviceProfileCache()
	stabilize := true

	cfg := &config.Config{
		ClaudeHeaderDefaults: config.ClaudeHeaderDefaults{
			UserAgent:              "claude-cli/2.1.70 (external, cli)",
			PackageVersion:         "0.80.0",
			RuntimeVersion:         "v24.5.0",
			OS:                     "MacOS",
			Arch:                   "arm64",
			Timeout:                "900",
			StabilizeDeviceProfile: &stabilize,
		},
	}
	auth := &cliproxyauth.Auth{ProxyURL: "direct",
		ID: "auth-baseline",
		Attributes: map[string]string{
			"api_key":                            "key-baseline",
			"header:User-Agent":                  "evil-client/9.9",
			"header:X-Stainless-Os":              "Linux",
			"header:X-Stainless-Arch":            "x64",
			"header:X-Stainless-Package-Version": "9.9.9",
		},
	}
	incoming := http.Header{
		"User-Agent":                  []string{"curl/8.7.1"},
		"X-Stainless-Package-Version": []string{"0.10.0"},
		"X-Stainless-Runtime-Version": []string{"v18.0.0"},
		"X-Stainless-Os":              []string{"Linux"},
		"X-Stainless-Arch":            []string{"x64"},
	}

	req := newClaudeHeaderTestRequest(t, incoming)
	applyClaudeHeaders(req, auth, "key-baseline", false, nil, cfg)

	assertClaudeFingerprint(t, req.Header, "evil-client/9.9", "9.9.9", "v24.5.0", "Linux", "x64")
	if got := req.Header.Get("X-Stainless-Timeout"); got != "900" {
		t.Fatalf("X-Stainless-Timeout = %q, want %q", got, "900")
	}
}

func TestApplyClaudeHeaders_RecordsClientObservationWhenStabilizationDisabled(t *testing.T) {
	resetClaudeDeviceProfileCache()
	stabilize := false

	cfg := &config.Config{
		ClaudeHeaderDefaults: config.ClaudeHeaderDefaults{
			StabilizeDeviceProfile: &stabilize,
		},
	}
	auth := &cliproxyauth.Auth{ProxyURL: "direct",
		ID: "auth-observation-without-stabilization",
		Attributes: map[string]string{
			"api_key": "key-observation-without-stabilization",
		},
	}
	incoming := http.Header{
		"User-Agent":                  []string{"claude-cli/2.1.140 (external, cli)"},
		"X-Stainless-Package-Version": []string{"0.80.0"},
		"X-Stainless-Runtime-Version": []string{"v24.5.0"},
		"X-Stainless-Os":              []string{"darwin"},
		"X-Stainless-Arch":            []string{"arm64"},
	}

	req := newClaudeHeaderTestRequest(t, incoming)
	applyClaudeHeaders(req, auth, "key-observation-without-stabilization", false, nil, cfg)

	observations := helps.ClaudeDeviceProfileObservations(auth, "")
	if len(observations) != 1 {
		t.Fatalf("observations length = %d, want 1: %#v", len(observations), observations)
	}
	if got := observations[0].Version; got != "2.1.140" {
		t.Fatalf("observed version = %q, want 2.1.140: %#v", got, observations[0])
	}
}

func TestResolveClaudeBillingVersionAndApplyHeaders_RecordSingleClientObservation(t *testing.T) {
	resetClaudeDeviceProfileCache()
	stabilize := false

	cfg := &config.Config{
		ClaudeHeaderDefaults: config.ClaudeHeaderDefaults{
			StabilizeDeviceProfile: &stabilize,
		},
	}
	auth := &cliproxyauth.Auth{ProxyURL: "direct",
		ID: "auth-observation-request-cache",
		Attributes: map[string]string{
			"api_key": "key-observation-request-cache",
		},
	}
	incoming := http.Header{
		"User-Agent":                  []string{"claude-cli/2.1.141 (external, cli)"},
		"X-Stainless-Package-Version": []string{"0.80.1"},
		"X-Stainless-Runtime-Version": []string{"v24.5.1"},
		"X-Stainless-Os":              []string{"darwin"},
		"X-Stainless-Arch":            []string{"arm64"},
	}

	req := newClaudeHeaderTestRequest(t, incoming)
	if got := resolveClaudeBillingVersion(req.Context(), cfg, auth, "key-observation-request-cache"); got != "2.1.141" {
		t.Fatalf("billing version = %q, want 2.1.141", got)
	}
	applyClaudeHeaders(req, auth, "key-observation-request-cache", false, nil, cfg)

	observations := helps.ClaudeDeviceProfileObservations(auth, "")
	if len(observations) != 1 {
		t.Fatalf("observations length = %d, want 1: %#v", len(observations), observations)
	}
	if got := observations[0].RequestCount; got != 1 {
		t.Fatalf("request_count = %d, want 1: %#v", got, observations[0])
	}
}

func TestApplyClaudeHeaders_StructuredAccountSettingsKeepsManagedHeadersAuthoritative(t *testing.T) {
	resetClaudeDeviceProfileCache()
	stabilize := true

	cfg := &config.Config{
		ClaudeHeaderDefaults: config.ClaudeHeaderDefaults{
			UserAgent:              "claude-cli/2.9.9 (external, cli)",
			PackageVersion:         "0.99.0",
			RuntimeVersion:         "v30.0.0",
			OS:                     "MacOS",
			Arch:                   "arm64",
			Timeout:                "777",
			StabilizeDeviceProfile: &stabilize,
		},
	}
	auth := &cliproxyauth.Auth{ProxyURL: "direct",
		ID: "auth-structured-account-settings",
		Attributes: map[string]string{
			"api_key":                            "key-structured-account-settings",
			"header:User-Agent":                  "legacy-managed-override/0.1",
			"header:X-Stainless-Package-Version": "legacy-package",
			"header:X-Extra-Debug":               "enabled",
		},
		Metadata: map[string]any{
			"account_settings": map[string]any{
				"schema_version": 1,
				"extra_headers": map[string]any{
					"X-Extra-Debug": "enabled",
				},
			},
		},
	}

	req := newClaudeHeaderTestRequest(t, http.Header{})
	applyClaudeHeaders(req, auth, "key-structured-account-settings", false, nil, cfg)

	if got := req.Header.Get("User-Agent"); got != "claude-cli/2.9.9 (external, cli)" {
		t.Fatalf("User-Agent = %q, want %q", got, "claude-cli/2.9.9 (external, cli)")
	}
	if got := req.Header.Get("X-Stainless-Package-Version"); got != "0.99.0" {
		t.Fatalf("X-Stainless-Package-Version = %q, want %q", got, "0.99.0")
	}
	if got := req.Header.Get("X-Stainless-Timeout"); got != "777" {
		t.Fatalf("X-Stainless-Timeout = %q, want %q", got, "777")
	}
	if got := req.Header.Get("X-Extra-Debug"); got != "enabled" {
		t.Fatalf("X-Extra-Debug = %q, want %q", got, "enabled")
	}
}

func TestApplyClaudeHeaders_TracksHighestClaudeCLIFingerprint(t *testing.T) {
	resetClaudeDeviceProfileCache()
	stabilize := true

	cfg := &config.Config{
		ClaudeHeaderDefaults: config.ClaudeHeaderDefaults{
			UserAgent:              "claude-cli/2.1.60 (external, cli)",
			PackageVersion:         "0.70.0",
			RuntimeVersion:         "v22.0.0",
			OS:                     "MacOS",
			Arch:                   "arm64",
			StabilizeDeviceProfile: &stabilize,
		},
	}
	auth := &cliproxyauth.Auth{ProxyURL: "direct",
		ID: "auth-upgrade",
		Attributes: map[string]string{
			"api_key": "key-upgrade",
		},
	}

	firstReq := newClaudeHeaderTestRequest(t, http.Header{
		"User-Agent":                  []string{"claude-cli/2.1.62 (external, cli)"},
		"X-Stainless-Package-Version": []string{"0.74.0"},
		"X-Stainless-Runtime-Version": []string{"v24.3.0"},
		"X-Stainless-Os":              []string{"Linux"},
		"X-Stainless-Arch":            []string{"x64"},
	})
	applyClaudeHeaders(firstReq, auth, "key-upgrade", false, nil, cfg)
	assertClaudeFingerprint(t, firstReq.Header, "claude-cli/2.1.62 (external, cli)", "0.74.0", "v24.3.0", "MacOS", "arm64")

	thirdPartyReq := newClaudeHeaderTestRequest(t, http.Header{
		"User-Agent":                  []string{"lobe-chat/1.0"},
		"X-Stainless-Package-Version": []string{"0.10.0"},
		"X-Stainless-Runtime-Version": []string{"v18.0.0"},
		"X-Stainless-Os":              []string{"Windows"},
		"X-Stainless-Arch":            []string{"x64"},
	})
	applyClaudeHeaders(thirdPartyReq, auth, "key-upgrade", false, nil, cfg)
	assertClaudeFingerprint(t, thirdPartyReq.Header, "claude-cli/2.1.62 (external, cli)", "0.74.0", "v24.3.0", "MacOS", "arm64")

	higherReq := newClaudeHeaderTestRequest(t, http.Header{
		"User-Agent":                  []string{"claude-cli/2.1.63 (external, cli)"},
		"X-Stainless-Package-Version": []string{"0.75.0"},
		"X-Stainless-Runtime-Version": []string{"v24.4.0"},
		"X-Stainless-Os":              []string{"MacOS"},
		"X-Stainless-Arch":            []string{"arm64"},
	})
	applyClaudeHeaders(higherReq, auth, "key-upgrade", false, nil, cfg)
	assertClaudeFingerprint(t, higherReq.Header, "claude-cli/2.1.63 (external, cli)", "0.75.0", "v24.4.0", "MacOS", "arm64")

	lowerReq := newClaudeHeaderTestRequest(t, http.Header{
		"User-Agent":                  []string{"claude-cli/2.1.61 (external, cli)"},
		"X-Stainless-Package-Version": []string{"0.73.0"},
		"X-Stainless-Runtime-Version": []string{"v24.2.0"},
		"X-Stainless-Os":              []string{"Windows"},
		"X-Stainless-Arch":            []string{"x64"},
	})
	applyClaudeHeaders(lowerReq, auth, "key-upgrade", false, nil, cfg)
	assertClaudeFingerprint(t, lowerReq.Header, "claude-cli/2.1.63 (external, cli)", "0.75.0", "v24.4.0", "MacOS", "arm64")
}

func TestApplyClaudeHeaders_DoesNotDowngradeConfiguredBaselineOnFirstClaudeClient(t *testing.T) {
	resetClaudeDeviceProfileCache()
	stabilize := true

	cfg := &config.Config{
		ClaudeHeaderDefaults: config.ClaudeHeaderDefaults{
			UserAgent:              "claude-cli/2.1.70 (external, cli)",
			PackageVersion:         "0.80.0",
			RuntimeVersion:         "v24.5.0",
			OS:                     "MacOS",
			Arch:                   "arm64",
			StabilizeDeviceProfile: &stabilize,
		},
	}
	auth := &cliproxyauth.Auth{ProxyURL: "direct",
		ID: "auth-baseline-floor",
		Attributes: map[string]string{
			"api_key": "key-baseline-floor",
		},
	}

	olderClaudeReq := newClaudeHeaderTestRequest(t, http.Header{
		"User-Agent":                  []string{"claude-cli/2.1.62 (external, cli)"},
		"X-Stainless-Package-Version": []string{"0.74.0"},
		"X-Stainless-Runtime-Version": []string{"v24.3.0"},
		"X-Stainless-Os":              []string{"Linux"},
		"X-Stainless-Arch":            []string{"x64"},
	})
	applyClaudeHeaders(olderClaudeReq, auth, "key-baseline-floor", false, nil, cfg)
	assertClaudeFingerprint(t, olderClaudeReq.Header, "claude-cli/2.1.70 (external, cli)", "0.80.0", "v24.5.0", "MacOS", "arm64")

	newerClaudeReq := newClaudeHeaderTestRequest(t, http.Header{
		"User-Agent":                  []string{"claude-cli/2.1.71 (external, cli)"},
		"X-Stainless-Package-Version": []string{"0.81.0"},
		"X-Stainless-Runtime-Version": []string{"v24.6.0"},
		"X-Stainless-Os":              []string{"Linux"},
		"X-Stainless-Arch":            []string{"x64"},
	})
	applyClaudeHeaders(newerClaudeReq, auth, "key-baseline-floor", false, nil, cfg)
	assertClaudeFingerprint(t, newerClaudeReq.Header, "claude-cli/2.1.71 (external, cli)", "0.81.0", "v24.6.0", "MacOS", "arm64")
}

func TestApplyClaudeHeaders_UpgradesCachedSoftwareFingerprintWhenBaselineAdvances(t *testing.T) {
	resetClaudeDeviceProfileCache()
	stabilize := true

	oldCfg := &config.Config{
		ClaudeHeaderDefaults: config.ClaudeHeaderDefaults{
			UserAgent:              "claude-cli/2.1.70 (external, cli)",
			PackageVersion:         "0.80.0",
			RuntimeVersion:         "v24.5.0",
			OS:                     "MacOS",
			Arch:                   "arm64",
			StabilizeDeviceProfile: &stabilize,
		},
	}
	newCfg := &config.Config{
		ClaudeHeaderDefaults: config.ClaudeHeaderDefaults{
			UserAgent:              "claude-cli/2.1.77 (external, cli)",
			PackageVersion:         "0.87.0",
			RuntimeVersion:         "v24.8.0",
			OS:                     "MacOS",
			Arch:                   "arm64",
			StabilizeDeviceProfile: &stabilize,
		},
	}
	auth := &cliproxyauth.Auth{ProxyURL: "direct",
		ID: "auth-baseline-reload",
		Attributes: map[string]string{
			"api_key": "key-baseline-reload",
		},
	}

	officialReq := newClaudeHeaderTestRequest(t, http.Header{
		"User-Agent":                  []string{"claude-cli/2.1.71 (external, cli)"},
		"X-Stainless-Package-Version": []string{"0.81.0"},
		"X-Stainless-Runtime-Version": []string{"v24.6.0"},
		"X-Stainless-Os":              []string{"Linux"},
		"X-Stainless-Arch":            []string{"x64"},
	})
	applyClaudeHeaders(officialReq, auth, "key-baseline-reload", false, nil, oldCfg)
	assertClaudeFingerprint(t, officialReq.Header, "claude-cli/2.1.71 (external, cli)", "0.81.0", "v24.6.0", "MacOS", "arm64")

	thirdPartyReq := newClaudeHeaderTestRequest(t, http.Header{
		"User-Agent":                  []string{"curl/8.7.1"},
		"X-Stainless-Package-Version": []string{"0.10.0"},
		"X-Stainless-Runtime-Version": []string{"v18.0.0"},
		"X-Stainless-Os":              []string{"Linux"},
		"X-Stainless-Arch":            []string{"x64"},
	})
	applyClaudeHeaders(thirdPartyReq, auth, "key-baseline-reload", false, nil, newCfg)
	assertClaudeFingerprint(t, thirdPartyReq.Header, "claude-cli/2.1.77 (external, cli)", "0.87.0", "v24.8.0", "MacOS", "arm64")
}

// TestApplyClaudeHeaders_AlignsUserAgentSuffixWithInboundEntrypoint pins the
// anti-correlation fix (T050): the stabilized outbound UA parenthetical suffix
// must mirror the current inbound claude-code client's "(USER_TYPE, ENTRYPOINT)"
// block (same source cc_entrypoint is derived from), while the version stays at
// the high-water mark. The bug: a frozen high-water device profile seeded by a
// "claude --print" / SDK request (UA suffix "(external, sdk-cli)") would keep
// emitting "sdk-cli" on every later interactive request even though that request's
// cc_entrypoint is "cli" — a UA/entrypoint pair real claude-code never produces.
//
// telemetry-farm-ux-hardening T4 scope A: with the default config (cfg.Claude
// here is the zero value, so config.NormalizeSdkCliEntrypointEnabled(cfg) ==
// true), a "sdk-cli" inbound entrypoint is additionally folded to "cli" — see
// the first (seed) request below, which now emits "(external, cli)" instead of
// mirroring "(external, sdk-cli)" verbatim.
func TestApplyClaudeHeaders_AlignsUserAgentSuffixWithInboundEntrypoint(t *testing.T) {
	resetClaudeDeviceProfileCache()
	stabilize := true

	cfg := &config.Config{
		ClaudeHeaderDefaults: config.ClaudeHeaderDefaults{
			UserAgent:              "claude-cli/2.1.60 (external, cli)",
			PackageVersion:         "0.70.0",
			RuntimeVersion:         "v22.0.0",
			OS:                     "MacOS",
			Arch:                   "arm64",
			StabilizeDeviceProfile: &stabilize,
		},
	}
	auth := &cliproxyauth.Auth{ProxyURL: "direct",
		ID: "auth-ua-suffix-align",
		Attributes: map[string]string{
			"api_key": "key-ua-suffix-align",
		},
	}

	// First request is a "claude --print" / SDK invocation: it seeds the per-account
	// high-water device profile with a high version AND (pre-T4) the sdk-cli
	// entrypoint suffix. With the default sdk-cli normalization on, the outbound
	// suffix on this request is folded to "(external, cli)" instead of mirroring
	// the inbound "sdk-cli" verbatim.
	sdkSeedReq := newClaudeHeaderTestRequest(t, http.Header{
		"User-Agent":                  []string{"claude-cli/2.1.180 (external, sdk-cli)"},
		"X-Stainless-Package-Version": []string{"0.90.0"},
		"X-Stainless-Runtime-Version": []string{"v24.9.0"},
		"X-Stainless-Os":              []string{"Linux"},
		"X-Stainless-Arch":            []string{"x64"},
	})
	applyClaudeHeaders(sdkSeedReq, auth, "key-ua-suffix-align", false, nil, cfg)
	// High-water version 2.1.180 adopted; OS/Arch pinned to baseline; suffix folds
	// this request's sdk-cli inbound to cli (T4 scope A normalization).
	assertClaudeFingerprint(t, sdkSeedReq.Header, "claude-cli/2.1.180 (external, cli)", "0.90.0", "v24.9.0", "MacOS", "arm64")

	// Second request is an interactive TUI invocation at a LOWER version with the
	// cli entrypoint. The version stays at the 2.1.180 high-water mark (only-up),
	// but the suffix MUST realign to the current inbound "(external, cli)" so the
	// outbound UA suffix matches cc_entrypoint=cli. OS/Arch stay pinned to baseline.
	interactiveReq := newClaudeHeaderTestRequest(t, http.Header{
		"User-Agent":                  []string{"claude-cli/2.1.63 (external, cli)"},
		"X-Stainless-Package-Version": []string{"0.75.0"},
		"X-Stainless-Runtime-Version": []string{"v24.4.0"},
		"X-Stainless-Os":              []string{"Windows"},
		"X-Stainless-Arch":            []string{"x64"},
	})
	applyClaudeHeaders(interactiveReq, auth, "key-ua-suffix-align", false, nil, cfg)
	assertClaudeFingerprint(t, interactiveReq.Header, "claude-cli/2.1.180 (external, cli)", "0.90.0", "v24.9.0", "MacOS", "arm64")

	// Third request: inbound is a non-claude client (api-key/curl). cc_entrypoint
	// defaults to "cli", so the outbound suffix must default to "(external, cli)"
	// too, while the high-water version/pkg/runtime/OS/Arch stay stable.
	nonClaudeReq := newClaudeHeaderTestRequest(t, http.Header{
		"User-Agent":                  []string{"curl/8.7.1"},
		"X-Stainless-Package-Version": []string{"0.10.0"},
		"X-Stainless-Runtime-Version": []string{"v18.0.0"},
		"X-Stainless-Os":              []string{"Linux"},
		"X-Stainless-Arch":            []string{"x64"},
	})
	applyClaudeHeaders(nonClaudeReq, auth, "key-ua-suffix-align", false, nil, cfg)
	assertClaudeFingerprint(t, nonClaudeReq.Header, "claude-cli/2.1.180 (external, cli)", "0.90.0", "v24.9.0", "MacOS", "arm64")
}

func TestApplyClaudeHeaders_LearnsOfficialFingerprintAfterCustomBaselineFallback(t *testing.T) {
	resetClaudeDeviceProfileCache()
	stabilize := true

	cfg := &config.Config{
		ClaudeHeaderDefaults: config.ClaudeHeaderDefaults{
			UserAgent:              "my-gateway/1.0",
			PackageVersion:         "custom-pkg",
			RuntimeVersion:         "custom-runtime",
			OS:                     "MacOS",
			Arch:                   "arm64",
			StabilizeDeviceProfile: &stabilize,
		},
	}
	auth := &cliproxyauth.Auth{ProxyURL: "direct",
		ID: "auth-custom-baseline-learning",
		Attributes: map[string]string{
			"api_key": "key-custom-baseline-learning",
		},
	}

	thirdPartyReq := newClaudeHeaderTestRequest(t, http.Header{
		"User-Agent":                  []string{"curl/8.7.1"},
		"X-Stainless-Package-Version": []string{"0.10.0"},
		"X-Stainless-Runtime-Version": []string{"v18.0.0"},
		"X-Stainless-Os":              []string{"Linux"},
		"X-Stainless-Arch":            []string{"x64"},
	})
	applyClaudeHeaders(thirdPartyReq, auth, "key-custom-baseline-learning", false, nil, cfg)
	assertClaudeFingerprint(t, thirdPartyReq.Header, "my-gateway/1.0", "custom-pkg", "custom-runtime", "MacOS", "arm64")

	officialReq := newClaudeHeaderTestRequest(t, http.Header{
		"User-Agent":                  []string{"claude-cli/2.1.77 (external, cli)"},
		"X-Stainless-Package-Version": []string{"0.87.0"},
		"X-Stainless-Runtime-Version": []string{"v24.8.0"},
		"X-Stainless-Os":              []string{"Linux"},
		"X-Stainless-Arch":            []string{"x64"},
	})
	applyClaudeHeaders(officialReq, auth, "key-custom-baseline-learning", false, nil, cfg)
	assertClaudeFingerprint(t, officialReq.Header, "claude-cli/2.1.77 (external, cli)", "0.87.0", "v24.8.0", "MacOS", "arm64")

	postLearningThirdPartyReq := newClaudeHeaderTestRequest(t, http.Header{
		"User-Agent":                  []string{"curl/8.7.1"},
		"X-Stainless-Package-Version": []string{"0.10.0"},
		"X-Stainless-Runtime-Version": []string{"v18.0.0"},
		"X-Stainless-Os":              []string{"Linux"},
		"X-Stainless-Arch":            []string{"x64"},
	})
	applyClaudeHeaders(postLearningThirdPartyReq, auth, "key-custom-baseline-learning", false, nil, cfg)
	assertClaudeFingerprint(t, postLearningThirdPartyReq.Header, "claude-cli/2.1.77 (external, cli)", "0.87.0", "v24.8.0", "MacOS", "arm64")
}

func TestResolveClaudeDeviceProfile_RechecksCacheBeforeStoringCandidate(t *testing.T) {
	resetClaudeDeviceProfileCache()
	stabilize := true

	cfg := &config.Config{
		ClaudeHeaderDefaults: config.ClaudeHeaderDefaults{
			UserAgent:              "claude-cli/2.1.60 (external, cli)",
			PackageVersion:         "0.70.0",
			RuntimeVersion:         "v22.0.0",
			OS:                     "MacOS",
			Arch:                   "arm64",
			StabilizeDeviceProfile: &stabilize,
		},
	}
	auth := &cliproxyauth.Auth{ProxyURL: "direct",
		ID: "auth-racy-upgrade",
		Attributes: map[string]string{
			"api_key": "key-racy-upgrade",
		},
	}

	lowPaused := make(chan struct{})
	releaseLow := make(chan struct{})
	var pauseOnce sync.Once
	var releaseOnce sync.Once

	helps.ClaudeDeviceProfileBeforeCandidateStore = func(candidate helps.ClaudeDeviceProfile) {
		if candidate.UserAgent != "claude-cli/2.1.62 (external, cli)" {
			return
		}
		pauseOnce.Do(func() { close(lowPaused) })
		<-releaseLow
	}
	t.Cleanup(func() {
		helps.ClaudeDeviceProfileBeforeCandidateStore = nil
		releaseOnce.Do(func() { close(releaseLow) })
	})

	lowResultCh := make(chan helps.ClaudeDeviceProfile, 1)
	go func() {
		lowResultCh <- helps.ResolveClaudeDeviceProfile(auth, "key-racy-upgrade", http.Header{
			"User-Agent":                  []string{"claude-cli/2.1.62 (external, cli)"},
			"X-Stainless-Package-Version": []string{"0.74.0"},
			"X-Stainless-Runtime-Version": []string{"v24.3.0"},
			"X-Stainless-Os":              []string{"Linux"},
			"X-Stainless-Arch":            []string{"x64"},
		}, cfg)
	}()

	select {
	case <-lowPaused:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for lower candidate to pause before storing")
	}

	highResult := helps.ResolveClaudeDeviceProfile(auth, "key-racy-upgrade", http.Header{
		"User-Agent":                  []string{"claude-cli/2.1.63 (external, cli)"},
		"X-Stainless-Package-Version": []string{"0.75.0"},
		"X-Stainless-Runtime-Version": []string{"v24.4.0"},
		"X-Stainless-Os":              []string{"MacOS"},
		"X-Stainless-Arch":            []string{"arm64"},
	}, cfg)
	releaseOnce.Do(func() { close(releaseLow) })

	select {
	case lowResult := <-lowResultCh:
		if lowResult.UserAgent != "claude-cli/2.1.63 (external, cli)" {
			t.Fatalf("lowResult.UserAgent = %q, want %q", lowResult.UserAgent, "claude-cli/2.1.63 (external, cli)")
		}
		if lowResult.PackageVersion != "0.75.0" {
			t.Fatalf("lowResult.PackageVersion = %q, want %q", lowResult.PackageVersion, "0.75.0")
		}
		if lowResult.OS != "MacOS" || lowResult.Arch != "arm64" {
			t.Fatalf("lowResult platform = %s/%s, want %s/%s", lowResult.OS, lowResult.Arch, "MacOS", "arm64")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for lower candidate result")
	}

	if highResult.UserAgent != "claude-cli/2.1.63 (external, cli)" {
		t.Fatalf("highResult.UserAgent = %q, want %q", highResult.UserAgent, "claude-cli/2.1.63 (external, cli)")
	}
	if highResult.OS != "MacOS" || highResult.Arch != "arm64" {
		t.Fatalf("highResult platform = %s/%s, want %s/%s", highResult.OS, highResult.Arch, "MacOS", "arm64")
	}

	cached := helps.ResolveClaudeDeviceProfile(auth, "key-racy-upgrade", http.Header{
		"User-Agent": []string{"curl/8.7.1"},
	}, cfg)
	if cached.UserAgent != "claude-cli/2.1.63 (external, cli)" {
		t.Fatalf("cached.UserAgent = %q, want %q", cached.UserAgent, "claude-cli/2.1.63 (external, cli)")
	}
	if cached.PackageVersion != "0.75.0" {
		t.Fatalf("cached.PackageVersion = %q, want %q", cached.PackageVersion, "0.75.0")
	}
	if cached.OS != "MacOS" || cached.Arch != "arm64" {
		t.Fatalf("cached platform = %s/%s, want %s/%s", cached.OS, cached.Arch, "MacOS", "arm64")
	}
}

func TestApplyClaudeHeaders_ThirdPartyBaselineThenOfficialUpgradeKeepsPinnedPlatform(t *testing.T) {
	resetClaudeDeviceProfileCache()
	stabilize := true

	cfg := &config.Config{
		ClaudeHeaderDefaults: config.ClaudeHeaderDefaults{
			UserAgent:              "claude-cli/2.1.70 (external, cli)",
			PackageVersion:         "0.80.0",
			RuntimeVersion:         "v24.5.0",
			OS:                     "MacOS",
			Arch:                   "arm64",
			StabilizeDeviceProfile: &stabilize,
		},
	}
	auth := &cliproxyauth.Auth{ProxyURL: "direct",
		ID: "auth-third-party-then-official",
		Attributes: map[string]string{
			"api_key": "key-third-party-then-official",
		},
	}

	thirdPartyReq := newClaudeHeaderTestRequest(t, http.Header{
		"User-Agent":                  []string{"curl/8.7.1"},
		"X-Stainless-Package-Version": []string{"0.10.0"},
		"X-Stainless-Runtime-Version": []string{"v18.0.0"},
		"X-Stainless-Os":              []string{"Linux"},
		"X-Stainless-Arch":            []string{"x64"},
	})
	applyClaudeHeaders(thirdPartyReq, auth, "key-third-party-then-official", false, nil, cfg)
	assertClaudeFingerprint(t, thirdPartyReq.Header, "claude-cli/2.1.70 (external, cli)", "0.80.0", "v24.5.0", "MacOS", "arm64")

	officialReq := newClaudeHeaderTestRequest(t, http.Header{
		"User-Agent":                  []string{"claude-cli/2.1.77 (external, cli)"},
		"X-Stainless-Package-Version": []string{"0.87.0"},
		"X-Stainless-Runtime-Version": []string{"v24.8.0"},
		"X-Stainless-Os":              []string{"Linux"},
		"X-Stainless-Arch":            []string{"x64"},
	})
	applyClaudeHeaders(officialReq, auth, "key-third-party-then-official", false, nil, cfg)
	assertClaudeFingerprint(t, officialReq.Header, "claude-cli/2.1.77 (external, cli)", "0.87.0", "v24.8.0", "MacOS", "arm64")
}

func TestApplyClaudeHeaders_DisableDeviceProfileStabilization(t *testing.T) {
	resetClaudeDeviceProfileCache()

	stabilize := false
	cfg := &config.Config{
		ClaudeHeaderDefaults: config.ClaudeHeaderDefaults{
			UserAgent:              "claude-cli/2.1.60 (external, cli)",
			PackageVersion:         "0.70.0",
			RuntimeVersion:         "v22.0.0",
			OS:                     "MacOS",
			Arch:                   "arm64",
			StabilizeDeviceProfile: &stabilize,
		},
	}
	auth := &cliproxyauth.Auth{ProxyURL: "direct",
		ID: "auth-disable-stability",
		Attributes: map[string]string{
			"api_key": "key-disable-stability",
		},
	}

	firstReq := newClaudeHeaderTestRequest(t, http.Header{
		"User-Agent":                  []string{"claude-cli/2.1.62 (external, cli)"},
		"X-Stainless-Package-Version": []string{"0.74.0"},
		"X-Stainless-Runtime-Version": []string{"v24.3.0"},
		"X-Stainless-Os":              []string{"Linux"},
		"X-Stainless-Arch":            []string{"x64"},
	})
	applyClaudeHeaders(firstReq, auth, "key-disable-stability", false, nil, cfg)
	assertClaudeFingerprint(t, firstReq.Header, "claude-cli/2.1.62 (external, cli)", "0.74.0", "v24.3.0", "Linux", "x64")

	thirdPartyReq := newClaudeHeaderTestRequest(t, http.Header{
		"User-Agent":                  []string{"lobe-chat/1.0"},
		"X-Stainless-Package-Version": []string{"0.10.0"},
		"X-Stainless-Runtime-Version": []string{"v18.0.0"},
		"X-Stainless-Os":              []string{"Windows"},
		"X-Stainless-Arch":            []string{"x64"},
	})
	applyClaudeHeaders(thirdPartyReq, auth, "key-disable-stability", false, nil, cfg)
	assertClaudeFingerprint(t, thirdPartyReq.Header, "claude-cli/2.1.60 (external, cli)", "0.10.0", "v18.0.0", "Windows", "x64")

	lowerReq := newClaudeHeaderTestRequest(t, http.Header{
		"User-Agent":                  []string{"claude-cli/2.1.61 (external, cli)"},
		"X-Stainless-Package-Version": []string{"0.73.0"},
		"X-Stainless-Runtime-Version": []string{"v24.2.0"},
		"X-Stainless-Os":              []string{"Windows"},
		"X-Stainless-Arch":            []string{"x64"},
	})
	applyClaudeHeaders(lowerReq, auth, "key-disable-stability", false, nil, cfg)
	assertClaudeFingerprint(t, lowerReq.Header, "claude-cli/2.1.61 (external, cli)", "0.73.0", "v24.2.0", "Windows", "x64")
}

func TestApplyClaudeHeaders_LegacyModePreservesConfiguredUserAgentOverrideForClaudeClients(t *testing.T) {
	resetClaudeDeviceProfileCache()

	stabilize := false
	cfg := &config.Config{
		ClaudeHeaderDefaults: config.ClaudeHeaderDefaults{
			UserAgent:              "claude-cli/2.1.60 (external, cli)",
			PackageVersion:         "0.70.0",
			RuntimeVersion:         "v22.0.0",
			StabilizeDeviceProfile: &stabilize,
		},
	}
	auth := &cliproxyauth.Auth{ProxyURL: "direct",
		ID: "auth-legacy-ua-override",
		Attributes: map[string]string{
			"api_key":           "key-legacy-ua-override",
			"header:User-Agent": "config-ua/1.0",
		},
	}

	req := newClaudeHeaderTestRequest(t, http.Header{
		"User-Agent":                  []string{"claude-cli/2.1.62 (external, cli)"},
		"X-Stainless-Package-Version": []string{"0.74.0"},
		"X-Stainless-Runtime-Version": []string{"v24.3.0"},
		"X-Stainless-Os":              []string{"Linux"},
		"X-Stainless-Arch":            []string{"x64"},
	})
	applyClaudeHeaders(req, auth, "key-legacy-ua-override", false, nil, cfg)

	assertClaudeFingerprint(t, req.Header, "config-ua/1.0", "0.74.0", "v24.3.0", "Linux", "x64")
}

func TestApplyClaudeHeaders_LegacyModeFallsBackToRuntimeOSArchWhenMissing(t *testing.T) {
	resetClaudeDeviceProfileCache()

	stabilize := false
	cfg := &config.Config{
		ClaudeHeaderDefaults: config.ClaudeHeaderDefaults{
			UserAgent:              "claude-cli/2.1.60 (external, cli)",
			PackageVersion:         "0.70.0",
			RuntimeVersion:         "v22.0.0",
			OS:                     "MacOS",
			Arch:                   "arm64",
			StabilizeDeviceProfile: &stabilize,
		},
	}
	auth := &cliproxyauth.Auth{ProxyURL: "direct",
		ID: "auth-legacy-runtime-os-arch",
		Attributes: map[string]string{
			"api_key": "key-legacy-runtime-os-arch",
		},
	}

	req := newClaudeHeaderTestRequest(t, http.Header{
		"User-Agent": []string{"curl/8.7.1"},
	})
	applyClaudeHeaders(req, auth, "key-legacy-runtime-os-arch", false, nil, cfg)

	assertClaudeFingerprint(t, req.Header, "claude-cli/2.1.60 (external, cli)", "0.70.0", "v22.0.0", helps.MapStainlessOS(), helps.MapStainlessArch())
}

func TestApplyClaudeHeaders_UnsetStabilizationAlsoUsesLegacyRuntimeOSArchFallback(t *testing.T) {
	resetClaudeDeviceProfileCache()

	cfg := &config.Config{
		ClaudeHeaderDefaults: config.ClaudeHeaderDefaults{
			UserAgent:      "claude-cli/2.1.60 (external, cli)",
			PackageVersion: "0.70.0",
			RuntimeVersion: "v22.0.0",
			OS:             "MacOS",
			Arch:           "arm64",
		},
	}
	auth := &cliproxyauth.Auth{ProxyURL: "direct",
		ID: "auth-unset-runtime-os-arch",
		Attributes: map[string]string{
			"api_key": "key-unset-runtime-os-arch",
		},
	}

	req := newClaudeHeaderTestRequest(t, http.Header{
		"User-Agent": []string{"curl/8.7.1"},
	})
	applyClaudeHeaders(req, auth, "key-unset-runtime-os-arch", false, nil, cfg)

	assertClaudeFingerprint(t, req.Header, "claude-cli/2.1.60 (external, cli)", "0.70.0", "v22.0.0", helps.MapStainlessOS(), helps.MapStainlessArch())
}

func TestClaudeDeviceProfileStabilizationEnabled_DefaultFalse(t *testing.T) {
	if helps.ClaudeDeviceProfileStabilizationEnabled(nil) {
		t.Fatal("expected nil config to default to disabled stabilization")
	}
	if helps.ClaudeDeviceProfileStabilizationEnabled(&config.Config{}) {
		t.Fatal("expected unset stabilize-device-profile to default to disabled stabilization")
	}
}

func TestApplyClaudeToolPrefix(t *testing.T) {
	input := []byte(`{"tools":[{"name":"alpha"},{"name":"proxy_bravo"}],"tool_choice":{"type":"tool","name":"charlie"},"messages":[{"role":"assistant","content":[{"type":"tool_use","name":"delta","id":"t1","input":{}}]}]}`)
	out := applyClaudeToolPrefix(input, "proxy_")

	if got := gjson.GetBytes(out, "tools.0.name").String(); got != "proxy_alpha" {
		t.Fatalf("tools.0.name = %q, want %q", got, "proxy_alpha")
	}
	if got := gjson.GetBytes(out, "tools.1.name").String(); got != "proxy_bravo" {
		t.Fatalf("tools.1.name = %q, want %q", got, "proxy_bravo")
	}
	if got := gjson.GetBytes(out, "tool_choice.name").String(); got != "proxy_charlie" {
		t.Fatalf("tool_choice.name = %q, want %q", got, "proxy_charlie")
	}
	if got := gjson.GetBytes(out, "messages.0.content.0.name").String(); got != "proxy_delta" {
		t.Fatalf("messages.0.content.0.name = %q, want %q", got, "proxy_delta")
	}
}

func TestApplyClaudeToolPrefix_WithToolReference(t *testing.T) {
	input := []byte(`{"tools":[{"name":"alpha"}],"messages":[{"role":"user","content":[{"type":"tool_reference","tool_name":"beta"},{"type":"tool_reference","tool_name":"proxy_gamma"}]}]}`)
	out := applyClaudeToolPrefix(input, "proxy_")

	if got := gjson.GetBytes(out, "messages.0.content.0.tool_name").String(); got != "proxy_beta" {
		t.Fatalf("messages.0.content.0.tool_name = %q, want %q", got, "proxy_beta")
	}
	if got := gjson.GetBytes(out, "messages.0.content.1.tool_name").String(); got != "proxy_gamma" {
		t.Fatalf("messages.0.content.1.tool_name = %q, want %q", got, "proxy_gamma")
	}
}

func TestApplyClaudeToolPrefix_SkipsBuiltinTools(t *testing.T) {
	input := []byte(`{"tools":[{"type":"web_search_20250305","name":"web_search"},{"name":"my_custom_tool","input_schema":{"type":"object"}}]}`)
	out := applyClaudeToolPrefix(input, "proxy_")

	if got := gjson.GetBytes(out, "tools.0.name").String(); got != "web_search" {
		t.Fatalf("built-in tool name should not be prefixed: tools.0.name = %q, want %q", got, "web_search")
	}
	if got := gjson.GetBytes(out, "tools.1.name").String(); got != "proxy_my_custom_tool" {
		t.Fatalf("custom tool should be prefixed: tools.1.name = %q, want %q", got, "proxy_my_custom_tool")
	}
}

func TestApplyClaudeToolPrefix_BuiltinToolSkipped(t *testing.T) {
	body := []byte(`{
		"tools": [
			{"type": "web_search_20250305", "name": "web_search", "max_uses": 5},
			{"name": "Read"}
		],
		"messages": [
			{"role": "user", "content": [
				{"type": "tool_use", "name": "web_search", "id": "ws1", "input": {}},
				{"type": "tool_use", "name": "Read", "id": "r1", "input": {}}
			]}
		]
	}`)
	out := applyClaudeToolPrefix(body, "proxy_")

	if got := gjson.GetBytes(out, "tools.0.name").String(); got != "web_search" {
		t.Fatalf("tools.0.name = %q, want %q", got, "web_search")
	}
	if got := gjson.GetBytes(out, "messages.0.content.0.name").String(); got != "web_search" {
		t.Fatalf("messages.0.content.0.name = %q, want %q", got, "web_search")
	}
	if got := gjson.GetBytes(out, "tools.1.name").String(); got != "proxy_Read" {
		t.Fatalf("tools.1.name = %q, want %q", got, "proxy_Read")
	}
	if got := gjson.GetBytes(out, "messages.0.content.1.name").String(); got != "proxy_Read" {
		t.Fatalf("messages.0.content.1.name = %q, want %q", got, "proxy_Read")
	}
}

func TestApplyClaudeToolPrefix_KnownBuiltinInHistoryOnly(t *testing.T) {
	body := []byte(`{
		"tools": [
			{"name": "Read"}
		],
		"messages": [
			{"role": "user", "content": [
				{"type": "tool_use", "name": "web_search", "id": "ws1", "input": {}}
			]}
		]
	}`)
	out := applyClaudeToolPrefix(body, "proxy_")

	if got := gjson.GetBytes(out, "messages.0.content.0.name").String(); got != "web_search" {
		t.Fatalf("messages.0.content.0.name = %q, want %q", got, "web_search")
	}
	if got := gjson.GetBytes(out, "tools.0.name").String(); got != "proxy_Read" {
		t.Fatalf("tools.0.name = %q, want %q", got, "proxy_Read")
	}
}

func TestApplyClaudeToolPrefix_CustomToolsPrefixed(t *testing.T) {
	body := []byte(`{
		"tools": [{"name": "Read"}, {"name": "Write"}],
		"messages": [
			{"role": "user", "content": [
				{"type": "tool_use", "name": "Read", "id": "r1", "input": {}},
				{"type": "tool_use", "name": "Write", "id": "w1", "input": {}}
			]}
		]
	}`)
	out := applyClaudeToolPrefix(body, "proxy_")

	if got := gjson.GetBytes(out, "tools.0.name").String(); got != "proxy_Read" {
		t.Fatalf("tools.0.name = %q, want %q", got, "proxy_Read")
	}
	if got := gjson.GetBytes(out, "tools.1.name").String(); got != "proxy_Write" {
		t.Fatalf("tools.1.name = %q, want %q", got, "proxy_Write")
	}
	if got := gjson.GetBytes(out, "messages.0.content.0.name").String(); got != "proxy_Read" {
		t.Fatalf("messages.0.content.0.name = %q, want %q", got, "proxy_Read")
	}
	if got := gjson.GetBytes(out, "messages.0.content.1.name").String(); got != "proxy_Write" {
		t.Fatalf("messages.0.content.1.name = %q, want %q", got, "proxy_Write")
	}
}

func TestApplyClaudeToolPrefix_ToolChoiceBuiltin(t *testing.T) {
	body := []byte(`{
		"tools": [
			{"type": "web_search_20250305", "name": "web_search"},
			{"name": "Read"}
		],
		"tool_choice": {"type": "tool", "name": "web_search"}
	}`)
	out := applyClaudeToolPrefix(body, "proxy_")

	if got := gjson.GetBytes(out, "tool_choice.name").String(); got != "web_search" {
		t.Fatalf("tool_choice.name = %q, want %q", got, "web_search")
	}
}

func TestApplyClaudeToolPrefix_KnownFallbackBuiltinsRemainUnprefixed(t *testing.T) {
	for _, builtin := range []string{"web_search", "code_execution", "text_editor", "computer"} {
		t.Run(builtin, func(t *testing.T) {
			input := []byte(fmt.Sprintf(`{
				"tools":[{"name":"Read"}],
				"tool_choice":{"type":"tool","name":%q},
				"messages":[{"role":"assistant","content":[{"type":"tool_use","name":%q,"id":"toolu_1","input":{}},{"type":"tool_reference","tool_name":%q},{"type":"tool_result","tool_use_id":"toolu_1","content":[{"type":"tool_reference","tool_name":%q}]}]}]
			}`, builtin, builtin, builtin, builtin))
			out := applyClaudeToolPrefix(input, "proxy_")

			if got := gjson.GetBytes(out, "tool_choice.name").String(); got != builtin {
				t.Fatalf("tool_choice.name = %q, want %q", got, builtin)
			}
			if got := gjson.GetBytes(out, "messages.0.content.0.name").String(); got != builtin {
				t.Fatalf("messages.0.content.0.name = %q, want %q", got, builtin)
			}
			if got := gjson.GetBytes(out, "messages.0.content.1.tool_name").String(); got != builtin {
				t.Fatalf("messages.0.content.1.tool_name = %q, want %q", got, builtin)
			}
			if got := gjson.GetBytes(out, "messages.0.content.2.content.0.tool_name").String(); got != builtin {
				t.Fatalf("messages.0.content.2.content.0.tool_name = %q, want %q", got, builtin)
			}
			if got := gjson.GetBytes(out, "tools.0.name").String(); got != "proxy_Read" {
				t.Fatalf("tools.0.name = %q, want %q", got, "proxy_Read")
			}
		})
	}
}

func TestStripClaudeToolPrefixFromResponse(t *testing.T) {
	input := []byte(`{"content":[{"type":"tool_use","name":"proxy_alpha","id":"t1","input":{}},{"type":"tool_use","name":"bravo","id":"t2","input":{}}]}`)
	out := stripClaudeToolPrefixFromResponse(input, "proxy_")

	if got := gjson.GetBytes(out, "content.0.name").String(); got != "alpha" {
		t.Fatalf("content.0.name = %q, want %q", got, "alpha")
	}
	if got := gjson.GetBytes(out, "content.1.name").String(); got != "bravo" {
		t.Fatalf("content.1.name = %q, want %q", got, "bravo")
	}
}

func TestStripClaudeToolPrefixFromResponse_WithToolReference(t *testing.T) {
	input := []byte(`{"content":[{"type":"tool_reference","tool_name":"proxy_alpha"},{"type":"tool_reference","tool_name":"bravo"}]}`)
	out := stripClaudeToolPrefixFromResponse(input, "proxy_")

	if got := gjson.GetBytes(out, "content.0.tool_name").String(); got != "alpha" {
		t.Fatalf("content.0.tool_name = %q, want %q", got, "alpha")
	}
	if got := gjson.GetBytes(out, "content.1.tool_name").String(); got != "bravo" {
		t.Fatalf("content.1.tool_name = %q, want %q", got, "bravo")
	}
}

func TestStripClaudeToolPrefixFromStreamLine(t *testing.T) {
	line := []byte(`data: {"type":"content_block_start","content_block":{"type":"tool_use","name":"proxy_alpha","id":"t1"},"index":0}`)
	out := stripClaudeToolPrefixFromStreamLine(line, "proxy_")

	payload := bytes.TrimSpace(out)
	if bytes.HasPrefix(payload, []byte("data:")) {
		payload = bytes.TrimSpace(payload[len("data:"):])
	}
	if got := gjson.GetBytes(payload, "content_block.name").String(); got != "alpha" {
		t.Fatalf("content_block.name = %q, want %q", got, "alpha")
	}
}

func TestStripClaudeToolPrefixFromStreamLine_WithToolReference(t *testing.T) {
	line := []byte(`data: {"type":"content_block_start","content_block":{"type":"tool_reference","tool_name":"proxy_beta"},"index":0}`)
	out := stripClaudeToolPrefixFromStreamLine(line, "proxy_")

	payload := bytes.TrimSpace(out)
	if bytes.HasPrefix(payload, []byte("data:")) {
		payload = bytes.TrimSpace(payload[len("data:"):])
	}
	if got := gjson.GetBytes(payload, "content_block.tool_name").String(); got != "beta" {
		t.Fatalf("content_block.tool_name = %q, want %q", got, "beta")
	}
}

func TestApplyClaudeToolPrefix_NestedToolReference(t *testing.T) {
	input := []byte(`{"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_123","content":[{"type":"tool_reference","tool_name":"mcp__nia__manage_resource"}]}]}]}`)
	out := applyClaudeToolPrefix(input, "proxy_")
	got := gjson.GetBytes(out, "messages.0.content.0.content.0.tool_name").String()
	if got != "proxy_mcp__nia__manage_resource" {
		t.Fatalf("nested tool_reference tool_name = %q, want %q", got, "proxy_mcp__nia__manage_resource")
	}
}

func TestClaudeExecutor_ReusesUserIDAcrossModelsWhenCacheEnabled(t *testing.T) {
	var userIDs []string
	var requestModels []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		userID := gjson.GetBytes(body, "metadata.user_id").String()
		model := gjson.GetBytes(body, "model").String()
		userIDs = append(userIDs, userID)
		requestModels = append(requestModels, model)
		t.Logf("HTTP Server received request: model=%s, user_id=%s, url=%s", model, userID, r.URL.String())
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","model":"claude-3-5-sonnet","role":"assistant","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer server.Close()

	t.Logf("End-to-end test: Fake HTTP server started at %s", server.URL)

	cacheEnabled := true
	executor := NewClaudeExecutor(&config.Config{
		ClaudeKey: []config.ClaudeKey{
			{
				APIKey:  "key-123",
				BaseURL: server.URL,
				Cloak: &config.CloakConfig{
					CacheUserID: &cacheEnabled,
				},
			},
		},
	})
	auth := &cliproxyauth.Auth{ProxyURL: "direct", Attributes: map[string]string{
		"api_key":  "key-123",
		"base_url": server.URL,
	}}

	payload := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`)
	models := []string{"claude-3-5-sonnet", "claude-3-5-haiku"}
	for _, model := range models {
		t.Logf("Sending request for model: %s", model)
		modelPayload, _ := sjson.SetBytes(payload, "model", model)
		if _, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
			Model:   model,
			Payload: modelPayload,
		}, cliproxyexecutor.Options{
			SourceFormat: sdktranslator.FromString("claude"),
		}); err != nil {
			t.Fatalf("Execute(%s) error: %v", model, err)
		}
	}

	if len(userIDs) != 2 {
		t.Fatalf("expected 2 requests, got %d", len(userIDs))
	}
	if userIDs[0] == "" || userIDs[1] == "" {
		t.Fatal("expected user_id to be populated")
	}
	t.Logf("user_id[0] (model=%s): %s", requestModels[0], userIDs[0])
	t.Logf("user_id[1] (model=%s): %s", requestModels[1], userIDs[1])
	// New account-scoped contract: metadata.user_id is a JSON object and the
	// device_id is derived per account, so it stays stable across models/requests
	// regardless of the legacy CacheUserID flag. session_id may differ per request.
	device0 := gjson.GetBytes([]byte(userIDs[0]), "device_id").String()
	device1 := gjson.GetBytes([]byte(userIDs[1]), "device_id").String()
	if device0 == "" || device1 == "" {
		t.Fatalf("expected device_id populated, got %q and %q", userIDs[0], userIDs[1])
	}
	if device0 != device1 {
		t.Fatalf("expected device_id reused across models, got %q and %q", device0, device1)
	}
	t.Logf("✓ End-to-end test passed: Same device_id (%s) was used for both models", device0)
}

// TestClaudeExecutor_DeviceIDIsAccountStableByDefault verifies the account-scoped
// device_id design: with no cloak/cache flag set, the synthetic device_id is derived
// deterministically per upstream account, so it is identical across requests rather
// than randomized per request (the prior behavior, which looked like abuse evasion).
func TestClaudeExecutor_DeviceIDIsAccountStableByDefault(t *testing.T) {
	var userIDs []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		userIDs = append(userIDs, gjson.GetBytes(body, "metadata.user_id").String())
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","model":"claude-3-5-sonnet","role":"assistant","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer server.Close()

	executor := NewClaudeExecutor(&config.Config{AuthDir: t.TempDir()})
	auth := &cliproxyauth.Auth{ProxyURL: "direct",
		FileName: "account-a.json",
		Attributes: map[string]string{
			"api_key":  "key-123",
			"base_url": server.URL,
		},
	}

	payload := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`)

	for i := 0; i < 2; i++ {
		if _, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
			Model:   "claude-3-5-sonnet",
			Payload: payload,
		}, cliproxyexecutor.Options{
			SourceFormat: sdktranslator.FromString("claude"),
		}); err != nil {
			t.Fatalf("Execute call %d error: %v", i, err)
		}
	}

	if len(userIDs) != 2 {
		t.Fatalf("expected 2 requests, got %d", len(userIDs))
	}
	if userIDs[0] == "" || userIDs[1] == "" {
		t.Fatal("expected user_id to be populated")
	}
	device0 := gjson.GetBytes([]byte(userIDs[0]), "device_id").String()
	device1 := gjson.GetBytes([]byte(userIDs[1]), "device_id").String()
	if device0 == "" || device1 == "" {
		t.Fatalf("expected device_id populated, got %q and %q", userIDs[0], userIDs[1])
	}
	if device0 != device1 {
		t.Fatalf("expected device_id stable across requests for the same account, got %q and %q", device0, device1)
	}
}

func TestClaudeExecutor_ExecuteOpenAINonStreamRejectsEmptyClaudeStream(t *testing.T) {
	_, err := executeOpenAIChatCompletionThroughClaude(t, "")
	if err == nil {
		t.Fatal("Execute error = nil, want empty stream error")
	}
	assertStatusErr(t, err, http.StatusBadGateway)
	if !strings.Contains(err.Error(), "empty stream response") {
		t.Fatalf("Execute error = %q, want empty stream response", err.Error())
	}
}

func TestClaudeExecutor_ExecuteOpenAINonStreamRejectsClaudeErrorEvent(t *testing.T) {
	body := `data: {"type":"error","error":{"type":"overloaded_error","message":"upstream overloaded"}}` + "\n"
	_, err := executeOpenAIChatCompletionThroughClaude(t, body)
	if err == nil {
		t.Fatal("Execute error = nil, want upstream error event")
	}
	assertStatusErr(t, err, http.StatusBadGateway)
	if !strings.Contains(err.Error(), "upstream overloaded") {
		t.Fatalf("Execute error = %q, want upstream overloaded", err.Error())
	}
}

func TestClaudeExecutor_ExecuteOpenAINonStreamRejectsIncompleteClaudeStream(t *testing.T) {
	body := strings.Join([]string{
		`data: {"type":"message_start","message":{"id":"msg_123","model":"claude-3-5-sonnet-20241022"}}`,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")

	_, err := executeOpenAIChatCompletionThroughClaude(t, body)
	if err == nil {
		t.Fatal("Execute error = nil, want incomplete stream error")
	}
	assertStatusErr(t, err, http.StatusBadGateway)
	if !strings.Contains(err.Error(), "ended before message completion") {
		t.Fatalf("Execute error = %q, want incomplete stream error", err.Error())
	}
}

func TestClaudeExecutor_ExecuteOpenAINonStreamConvertsValidClaudeStream(t *testing.T) {
	body := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"msg_123","model":"claude-3-5-sonnet-20241022"}}`,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}`,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":2,"output_tokens":1}}`,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")

	resp, err := executeOpenAIChatCompletionThroughClaude(t, body)
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if got := gjson.GetBytes(resp.Payload, "id").String(); got != "msg_123" {
		t.Fatalf("response id = %q, want msg_123; payload=%s", got, string(resp.Payload))
	}
	if got := gjson.GetBytes(resp.Payload, "model").String(); got != "claude-3-5-sonnet-20241022" {
		t.Fatalf("response model = %q, want claude-3-5-sonnet-20241022", got)
	}
	if got := gjson.GetBytes(resp.Payload, "choices.0.message.content").String(); got != "ok" {
		t.Fatalf("response content = %q, want ok", got)
	}
	if got := gjson.GetBytes(resp.Payload, "usage.total_tokens").Int(); got != 3 {
		t.Fatalf("usage.total_tokens = %d, want 3", got)
	}
}

func executeOpenAIChatCompletionThroughClaude(t *testing.T, upstreamBody string) (cliproxyexecutor.Response, error) {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(upstreamBody))
	}))
	defer server.Close()

	executor := NewClaudeExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{ProxyURL: "direct", Attributes: map[string]string{
		"api_key":  "key-123",
		"base_url": server.URL,
	}}
	payload := []byte(`{"model":"claude-3-5-sonnet-20241022","messages":[{"role":"user","content":"hi"}]}`)

	return executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "claude-3-5-sonnet-20241022",
		Payload: payload,
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai"),
	})
}

func assertStatusErr(t *testing.T, err error, want int) {
	t.Helper()

	status, ok := err.(interface{ StatusCode() int })
	if !ok {
		t.Fatalf("error %T does not expose StatusCode", err)
	}
	if got := status.StatusCode(); got != want {
		t.Fatalf("StatusCode() = %d, want %d", got, want)
	}
}

func TestClaudeExecutor_Explicit1MAliasUsesOfficialModelWithoutLegacyContextBeta(t *testing.T) {
	type capturedRequest struct {
		model string
		beta  string
	}
	var captured []capturedRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		captured = append(captured, capturedRequest{
			model: gjson.GetBytes(body, "model").String(),
			beta:  r.Header.Get("Anthropic-Beta"),
		})
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","model":"claude-sonnet-4-6","role":"assistant","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer server.Close()

	executor := NewClaudeExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{ProxyURL: "direct", Attributes: map[string]string{
		"api_key":  "sk-ant-oat-test",
		"base_url": server.URL,
	}}
	payload := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`)

	tests := []struct {
		name           string
		requestedModel string
	}{
		{name: "plain official model", requestedModel: "claude-sonnet-4-6"},
		{name: "explicit 1m alias", requestedModel: "sonnet[1m]"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
				Model:   "claude-sonnet-4-6",
				Payload: payload,
			}, cliproxyexecutor.Options{
				SourceFormat: sdktranslator.FromString("claude"),
				Metadata: map[string]any{
					cliproxyexecutor.RequestedModelMetadataKey: tt.requestedModel,
				},
			})
			if err != nil {
				t.Fatalf("Execute() error = %v", err)
			}
			got := captured[len(captured)-1]
			if got.model != "claude-sonnet-4-6" {
				t.Fatalf("upstream model = %q, want %q", got.model, "claude-sonnet-4-6")
			}
			if strings.Contains(got.beta, "context-1m-2025-08-07") {
				t.Fatalf("Anthropic-Beta should not contain removed context-1m beta; header=%q", got.beta)
			}
		})
	}
}

func TestClaudeExecutor_AlignsBillingVersionWithStabilizedUserAgent(t *testing.T) {
	resetClaudeDeviceProfileCache()
	stabilize := true

	type capturedRequest struct {
		body      []byte
		userAgent string
	}
	var captured capturedRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		captured = capturedRequest{
			body:      bytes.Clone(body),
			userAgent: r.Header.Get("User-Agent"),
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","model":"claude-sonnet-4-6","role":"assistant","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer server.Close()

	executor := NewClaudeExecutor(&config.Config{
		ClaudeHeaderDefaults: config.ClaudeHeaderDefaults{
			UserAgent:              "claude-cli/2.1.70 (external, cli)",
			PackageVersion:         "0.80.0",
			RuntimeVersion:         "v24.5.0",
			OS:                     "MacOS",
			Arch:                   "arm64",
			StabilizeDeviceProfile: &stabilize,
		},
	})
	auth := &cliproxyauth.Auth{ProxyURL: "direct",
		ID: "auth-billing-stabilized",
		Attributes: map[string]string{
			"api_key":     "sk-ant-oat-test",
			"base_url":    server.URL,
			"cloak_mode":  "always",
			"tool_prefix": "disabled",
		},
	}
	ctx := contextWithGinHeaders(map[string]string{
		"User-Agent":                  "claude-cli/2.1.80 (external, cli)",
		"X-Stainless-Package-Version": "0.81.0",
		"X-Stainless-Runtime-Version": "v24.6.0",
		"X-Stainless-Os":              "Linux",
		"X-Stainless-Arch":            "x64",
	})
	payload := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`)

	_, err := executor.Execute(ctx, auth, cliproxyexecutor.Request{
		Model:   "claude-sonnet-4-6",
		Payload: payload,
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("claude")})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if captured.userAgent != "claude-cli/2.1.80 (external, cli)" {
		t.Fatalf("User-Agent = %q, want stabilized incoming version", captured.userAgent)
	}
	if got := billingVersionFromBody(t, captured.body); got != "2.1.80" {
		t.Fatalf("billing cc_version = %q, want %q", got, "2.1.80")
	}
}

func TestClaudeExecutor_RewritesStaleBillingVersionToStabilizedUserAgent(t *testing.T) {
	resetClaudeDeviceProfileCache()
	stabilize := true

	var captured capturedRequestForBilling
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		captured = capturedRequestForBilling{
			body:      bytes.Clone(body),
			userAgent: r.Header.Get("User-Agent"),
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","model":"claude-sonnet-4-6","role":"assistant","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer server.Close()

	executor := NewClaudeExecutor(&config.Config{
		ClaudeHeaderDefaults: config.ClaudeHeaderDefaults{
			UserAgent:              "claude-cli/2.1.70 (external, cli)",
			PackageVersion:         "0.80.0",
			RuntimeVersion:         "v24.5.0",
			OS:                     "MacOS",
			Arch:                   "arm64",
			StabilizeDeviceProfile: &stabilize,
		},
	})
	auth := &cliproxyauth.Auth{ProxyURL: "direct",
		ID: "auth-billing-stale-execute",
		Attributes: map[string]string{
			"api_key":     "sk-ant-oat-test",
			"base_url":    server.URL,
			"cloak_mode":  "always",
			"tool_prefix": "disabled",
		},
	}
	ctx := contextWithGinHeaders(map[string]string{
		"User-Agent":                  "claude-cli/2.1.83 (external, cli)",
		"X-Stainless-Package-Version": "0.81.0",
		"X-Stainless-Runtime-Version": "v24.6.0",
		"X-Stainless-Os":              "Linux",
		"X-Stainless-Arch":            "x64",
	})
	payload := []byte(`{"system":[{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1.63.abc; cc_entrypoint=cli; cch=12345;"},{"type":"text","text":"existing"}],"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`)

	_, err := executor.Execute(ctx, auth, cliproxyexecutor.Request{
		Model:   "claude-sonnet-4-6",
		Payload: payload,
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("claude")})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if captured.userAgent != "claude-cli/2.1.83 (external, cli)" {
		t.Fatalf("User-Agent = %q, want stabilized incoming version", captured.userAgent)
	}
	if got := billingVersionFromBody(t, captured.body); got != "2.1.83" {
		t.Fatalf("billing cc_version = %q, want %q", got, "2.1.83")
	}
}

// TestClaudeExecutor_OutboundUserAgentSuffixMatchesCCEntrypoint is the
// end-to-end (full Execute) regression for the T050 anti-correlation bug: with a
// device profile high-water frozen on the sdk-cli entrypoint, an interactive
// inbound request (cc_entrypoint=cli) must produce an outbound UA whose
// parenthetical suffix is "cli" — i.e. the outbound UA suffix and the billing
// cc_entrypoint must reference the same inbound-derived entrypoint and never
// diverge. The high-water version is still emitted in the UA.
func TestClaudeExecutor_OutboundUserAgentSuffixMatchesCCEntrypoint(t *testing.T) {
	resetClaudeDeviceProfileCache()
	stabilize := true

	var captured capturedRequestForBilling
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		captured = capturedRequestForBilling{
			body:      bytes.Clone(body),
			userAgent: r.Header.Get("User-Agent"),
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","model":"claude-sonnet-4-6","role":"assistant","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer server.Close()

	cfg := &config.Config{
		ClaudeHeaderDefaults: config.ClaudeHeaderDefaults{
			UserAgent:              "claude-cli/2.1.70 (external, cli)",
			PackageVersion:         "0.80.0",
			RuntimeVersion:         "v24.5.0",
			OS:                     "MacOS",
			Arch:                   "arm64",
			StabilizeDeviceProfile: &stabilize,
		},
	}
	executor := NewClaudeExecutor(cfg)
	auth := &cliproxyauth.Auth{ProxyURL: "direct",
		ID: "auth-ua-entrypoint-e2e",
		Attributes: map[string]string{
			"api_key":     "sk-ant-oat-test",
			"base_url":    server.URL,
			"cloak_mode":  "always",
			"tool_prefix": "disabled",
		},
	}
	payload := []byte(`{"system":[{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1.63.abc; cc_entrypoint=cli; cch=12345;"},{"type":"text","text":"existing"}],"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`)

	// Seed the high-water device profile with an SDK ("claude --print") request:
	// high version + sdk-cli entrypoint suffix get frozen.
	sdkCtx := contextWithGinHeaders(map[string]string{
		"User-Agent":                  "claude-cli/2.1.180 (external, sdk-cli)",
		"X-Stainless-Package-Version": "0.90.0",
		"X-Stainless-Runtime-Version": "v24.9.0",
		"X-Stainless-Os":              "Linux",
		"X-Stainless-Arch":            "x64",
	})
	if _, err := executor.Execute(sdkCtx, auth, cliproxyexecutor.Request{
		Model:   "claude-sonnet-4-6",
		Payload: payload,
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("claude")}); err != nil {
		t.Fatalf("Execute() sdk-seed error = %v", err)
	}

	// Now an interactive TUI request: inbound entrypoint is cli, lower version.
	cliCtx := contextWithGinHeaders(map[string]string{
		"User-Agent":                  "claude-cli/2.1.63 (external, cli)",
		"X-Stainless-Package-Version": "0.75.0",
		"X-Stainless-Runtime-Version": "v24.4.0",
		"X-Stainless-Os":              "Windows",
		"X-Stainless-Arch":            "x64",
	})
	if _, err := executor.Execute(cliCtx, auth, cliproxyexecutor.Request{
		Model:   "claude-sonnet-4-6",
		Payload: payload,
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("claude")}); err != nil {
		t.Fatalf("Execute() cli error = %v", err)
	}

	// Outbound UA keeps the 2.1.180 high-water version but the suffix realigns to
	// the inbound cli entrypoint.
	if captured.userAgent != "claude-cli/2.1.180 (external, cli)" {
		t.Fatalf("outbound User-Agent = %q, want %q", captured.userAgent, "claude-cli/2.1.180 (external, cli)")
	}
	uaEntrypoint := userAgentSuffixEntrypoint(captured.userAgent)
	ccEntrypoint := billingEntrypointFromBody(t, captured.body)
	if uaEntrypoint != "cli" {
		t.Fatalf("outbound UA suffix entrypoint = %q, want cli", uaEntrypoint)
	}
	if ccEntrypoint != "cli" {
		t.Fatalf("billing cc_entrypoint = %q, want cli", ccEntrypoint)
	}
	// The core invariant: outbound UA suffix == cc_entrypoint (no divergence).
	if uaEntrypoint != ccEntrypoint {
		t.Fatalf("UA suffix entrypoint %q != cc_entrypoint %q; anti-correlation mismatch", uaEntrypoint, ccEntrypoint)
	}
	// High-water version is still emitted in the billing header.
	if got := billingVersionFromBody(t, captured.body); got != "2.1.180" {
		t.Fatalf("billing cc_version = %q, want %q", got, "2.1.180")
	}
}

// TestClaudeExecutor_MessagesEntrypointMatchesUserAgentInAutoCloakMode is the
// end-to-end (full Execute) regression for the T4 scope A HIGH gap that the
// "always" cloak-mode test above masked: in the DEFAULT "auto" cloak mode a real
// claude-cli client bypasses cloak system-block regeneration (helps.ShouldCloak
// is false), so its inbound x-anthropic-billing-header — self-tagged
// cc_entrypoint=sdk-cli by a `claude -p` / Agent SDK invocation — used to be
// forwarded verbatim (signAnthropicMessagesBody only recomputes cch) while the
// outbound UA suffix was folded to "cli". That divergence (UA=cli, body=sdk-cli)
// is a pair real claude-code never emits. With normalizeClaudeBillingEntrypoint
// wired into the messages path the outbound UA suffix and the body cc_entrypoint
// must both be "cli"; disabling the switch restores the (self-consistent) pre-T4
// verbatim behavior.
func TestClaudeExecutor_MessagesEntrypointMatchesUserAgentInAutoCloakMode(t *testing.T) {
	stabilize := true
	// Inbound "claude -p" traffic: both the UA suffix and the body billing header
	// self-report the disallowed "sdk-cli" entrypoint.
	inboundUA := "claude-cli/2.1.180 (external, sdk-cli)"
	payload := []byte(`{"system":[{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1.63.abc; cc_entrypoint=sdk-cli; cch=12345;"},{"type":"text","text":"existing"}],"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`)

	runOnce := func(t *testing.T, cfg *config.Config, authID string) capturedRequestForBilling {
		t.Helper()
		resetClaudeDeviceProfileCache()
		var captured capturedRequestForBilling
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			captured = capturedRequestForBilling{body: bytes.Clone(body), userAgent: r.Header.Get("User-Agent")}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","model":"claude-sonnet-4-6","role":"assistant","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`))
		}))
		defer server.Close()

		executor := NewClaudeExecutor(cfg)
		auth := &cliproxyauth.Auth{ProxyURL: "direct",
			ID: authID,
			Attributes: map[string]string{
				"api_key":     "sk-ant-oat-test",
				"base_url":    server.URL,
				"cloak_mode":  "auto", // real claude-cli clients are intentionally NOT cloaked here
				"tool_prefix": "disabled",
			},
		}
		ctx := contextWithGinHeaders(map[string]string{
			"User-Agent":                  inboundUA,
			"X-Stainless-Package-Version": "0.90.0",
			"X-Stainless-Runtime-Version": "v24.9.0",
			"X-Stainless-Os":              "Linux",
			"X-Stainless-Arch":            "x64",
		})
		if _, err := executor.Execute(ctx, auth, cliproxyexecutor.Request{
			Model:   "claude-sonnet-4-6",
			Payload: payload,
		}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("claude")}); err != nil {
			t.Fatalf("Execute() error = %v", err)
		}
		return captured
	}

	baselineHeaders := config.ClaudeHeaderDefaults{
		UserAgent:              "claude-cli/2.1.70 (external, cli)",
		PackageVersion:         "0.80.0",
		RuntimeVersion:         "v24.5.0",
		OS:                     "MacOS",
		Arch:                   "arm64",
		StabilizeDeviceProfile: &stabilize,
	}

	// Default config (config.NormalizeSdkCliEntrypointEnabled == true): the
	// verbatim messages path folds the inbound sdk-cli billing header to cli.
	captured := runOnce(t, &config.Config{ClaudeHeaderDefaults: baselineHeaders}, "auth-auto-entrypoint-e2e")
	uaEntrypoint := userAgentSuffixEntrypoint(captured.userAgent)
	ccEntrypoint := billingEntrypointFromBody(t, captured.body)
	if uaEntrypoint != "cli" {
		t.Fatalf("outbound UA suffix entrypoint = %q, want cli (UA=%q)", uaEntrypoint, captured.userAgent)
	}
	if ccEntrypoint != "cli" {
		t.Fatalf("billing cc_entrypoint = %q, want cli (verbatim messages path not folded)", ccEntrypoint)
	}
	// The core invariant: outbound UA suffix == cc_entrypoint on the auto-mode
	// messages path (no divergence).
	if uaEntrypoint != ccEntrypoint {
		t.Fatalf("UA suffix entrypoint %q != cc_entrypoint %q; anti-correlation mismatch on auto-mode messages path", uaEntrypoint, ccEntrypoint)
	}

	// Rollback: with normalization disabled the messages path mirrors the inbound
	// sdk-cli entrypoint verbatim on BOTH the UA suffix and the body, restoring the
	// pre-T4 self-consistent behavior.
	disabled := false
	cfgOff := &config.Config{ClaudeHeaderDefaults: baselineHeaders, Claude: config.ClaudeConfig{NormalizeSdkCliEntrypoint: &disabled}}
	capturedOff := runOnce(t, cfgOff, "auth-auto-entrypoint-e2e-off")
	uaOff := userAgentSuffixEntrypoint(capturedOff.userAgent)
	ccOff := billingEntrypointFromBody(t, capturedOff.body)
	if ccOff != "sdk-cli" {
		t.Fatalf("normalization disabled: billing cc_entrypoint = %q, want verbatim sdk-cli", ccOff)
	}
	if uaOff != "sdk-cli" {
		t.Fatalf("normalization disabled: UA suffix entrypoint = %q, want verbatim sdk-cli", uaOff)
	}
	if uaOff != ccOff {
		t.Fatalf("normalization disabled: UA suffix %q != cc_entrypoint %q", uaOff, ccOff)
	}
}

func TestClaudeExecutor_AlignsBillingVersionWithSavedManagedUserAgent(t *testing.T) {
	var captured capturedRequestForBilling
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		captured = capturedRequestForBilling{
			body:      bytes.Clone(body),
			userAgent: r.Header.Get("User-Agent"),
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","model":"claude-sonnet-4-6","role":"assistant","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer server.Close()

	executor := NewClaudeExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{ProxyURL: "direct",
		ID: "auth-billing-saved-header",
		Attributes: map[string]string{
			"api_key":    "sk-ant-oat-test",
			"base_url":   server.URL,
			"cloak_mode": "always",
		},
		Metadata: map[string]any{
			"headers": map[string]any{
				"User-Agent": "claude-cli/2.1.77 (external, cli)",
			},
		},
	}
	payload := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`)

	_, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "claude-sonnet-4-6",
		Payload: payload,
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("claude")})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if captured.userAgent != "claude-cli/2.1.77 (external, cli)" {
		t.Fatalf("User-Agent = %q, want saved managed header", captured.userAgent)
	}
	if got := billingVersionFromBody(t, captured.body); got != "2.1.77" {
		t.Fatalf("billing cc_version = %q, want %q", got, "2.1.77")
	}
}

type capturedRequestForBilling struct {
	body      []byte
	userAgent string
}

func TestClaudeExecutorPrepareRequest_RecordsDirectClientVersionObservation(t *testing.T) {
	resetClaudeDeviceProfileCache()

	executor := NewClaudeExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{ProxyURL: "direct",
		FileName: "claude-direct-auth.json",
		Provider: "claude",
		Attributes: map[string]string{
			"api_key": "sk-ant-oat-test",
		},
	}
	req := httptest.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages?beta=true", nil)
	req.Header.Set("User-Agent", "claude-cli/2.1.142 (external, sdk-cli)")
	req.Header.Set("X-Stainless-Package-Version", "0.94.0")
	req.Header.Set("X-Stainless-Runtime-Version", "v24.3.0")

	if err := executor.PrepareRequest(req, auth); err != nil {
		t.Fatalf("PrepareRequest() error = %v", err)
	}
	observations := helps.ClaudeDeviceProfileObservations(auth, "")
	if len(observations) != 1 {
		t.Fatalf("observations length = %d, want 1: %#v", len(observations), observations)
	}
	if got := observations[0].Version; got != "2.1.142" {
		t.Fatalf("observation version = %q, want 2.1.142", got)
	}
}

// TestClaudeExecutorPrepareRequest_AppliesFullManagedAnthropicHeaderSet covers
// the quota/oauth snapshot egress path (quota_snapshots.go fetchQuotaJSON ->
// exec.HttpRequest -> ClaudeExecutor.PrepareRequest, used for GET
// /api/oauth/profile and /api/oauth/usage). Before the fix, PrepareRequest only
// applied 5 device-profile headers (UA/package-version/runtime-version/os/arch),
// leaving quota egress with a half-managed subset of real claude-cli's header
// set. This asserts PrepareRequest now fills in the rest of the managed
// Anthropic/stainless protocol headers to match real serving, while preserving
// the caller-set Accept and quota-specific anthropic-beta, and never attaching a
// client session id (quota is a sessionless background lookup).
func TestClaudeExecutorPrepareRequest_AppliesFullManagedAnthropicHeaderSet(t *testing.T) {
	resetClaudeDeviceProfileCache()

	executor := NewClaudeExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		ProxyURL: "direct",
		Provider: "claude",
		Attributes: map[string]string{
			"api_key": "sk-ant-oat-test",
		},
	}

	// Mirrors quota_snapshots.go fetchQuotaJSON: Accept and the quota-specific
	// oauth beta are set by the caller before the request reaches PrepareRequest.
	req, err := http.NewRequest(http.MethodGet, "https://api.anthropic.com/api/oauth/profile", nil)
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("anthropic-beta", "oauth-2025-04-20")

	if err := executor.PrepareRequest(req, auth); err != nil {
		t.Fatalf("PrepareRequest() error = %v", err)
	}

	wantSet := map[string]string{
		"Anthropic-Version":       "2023-06-01",
		"X-App":                   "cli",
		"X-Stainless-Lang":        "js",
		"X-Stainless-Runtime":     "node",
		"X-Stainless-Retry-Count": "0",
		"X-Stainless-Timeout":     "600",
		"Connection":              "keep-alive",
	}
	for name, want := range wantSet {
		if got := req.Header.Get(name); got != want {
			t.Fatalf("%s = %q, want %q", name, got, want)
		}
	}

	if got := req.Header.Get("x-client-request-id"); got == "" {
		t.Fatalf("x-client-request-id must be set for first-party api.anthropic.com requests")
	} else if _, errParse := uuid.Parse(got); errParse != nil {
		t.Fatalf("x-client-request-id = %q, want a uuid: %v", got, errParse)
	}

	// The quota-specific beta set by the caller must survive untouched;
	// PrepareRequest must never replace it with serving's own beta set.
	if got := req.Header.Get("anthropic-beta"); got != "oauth-2025-04-20" {
		t.Fatalf("anthropic-beta = %q, want %q (must be preserved)", got, "oauth-2025-04-20")
	}
	if got := req.Header.Get("Accept"); got != "application/json" {
		t.Fatalf("Accept = %q, want %q (must be preserved)", got, "application/json")
	}

	// Quota is a sessionless background call: a client session id must never be
	// attached, unlike real serving which always attaches one.
	if got := req.Header.Get("X-Claude-Code-Session-Id"); got != "" {
		t.Fatalf("X-Claude-Code-Session-Id = %q, want empty (quota must not gain a session correlation anchor)", got)
	}
}

// TestClaudeExecutorPrepareRequest_FreshClientRequestIDPerCall asserts
// x-client-request-id is a new UUID on every call, matching real claude-cli's
// per-request id semantics (not a stable/cached value).
func TestClaudeExecutorPrepareRequest_FreshClientRequestIDPerCall(t *testing.T) {
	resetClaudeDeviceProfileCache()

	executor := NewClaudeExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		ProxyURL: "direct",
		Provider: "claude",
		Attributes: map[string]string{
			"api_key": "sk-ant-oat-test",
		},
	}

	newReq := func() *http.Request {
		req, err := http.NewRequest(http.MethodGet, "https://api.anthropic.com/api/oauth/usage", nil)
		if err != nil {
			t.Fatalf("NewRequest() error = %v", err)
		}
		return req
	}

	first := newReq()
	if err := executor.PrepareRequest(first, auth); err != nil {
		t.Fatalf("PrepareRequest() error = %v", err)
	}
	second := newReq()
	if err := executor.PrepareRequest(second, auth); err != nil {
		t.Fatalf("PrepareRequest() error = %v", err)
	}

	firstID := first.Header.Get("x-client-request-id")
	secondID := second.Header.Get("x-client-request-id")
	if firstID == "" || secondID == "" {
		t.Fatalf("x-client-request-id must be set on both requests: first=%q second=%q", firstID, secondID)
	}
	if firstID == secondID {
		t.Fatalf("x-client-request-id must be fresh per request, got same value %q on both calls", firstID)
	}
}

// TestClaudeExecutorPrepareRequest_SkipsManagedProtocolHeadersForNonAnthropicHost
// guards the host gate. PrepareRequest also implements the generic
// RequestPreparer hook reachable via Manager.InjectCredentials/
// PrepareHttpRequest for arbitrary requests an SDK embedder may build (not just
// quota's fixed api.anthropic.com targets). It must not inject
// Anthropic-specific managed protocol headers onto a request targeting a
// non-Anthropic base_url/proxy host.
func TestClaudeExecutorPrepareRequest_SkipsManagedProtocolHeadersForNonAnthropicHost(t *testing.T) {
	resetClaudeDeviceProfileCache()

	executor := NewClaudeExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		ProxyURL: "direct",
		Provider: "claude",
		Attributes: map[string]string{
			"api_key":  "sk-ant-oat-test",
			"base_url": "https://compat.example.com",
		},
	}

	req, err := http.NewRequest(http.MethodGet, "https://compat.example.com/api/oauth/profile", nil)
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}

	if err := executor.PrepareRequest(req, auth); err != nil {
		t.Fatalf("PrepareRequest() error = %v", err)
	}

	for _, name := range []string{
		"Anthropic-Version",
		"X-Stainless-Lang",
		"X-Stainless-Runtime",
		"X-Stainless-Retry-Count",
		"X-Stainless-Timeout",
		"x-client-request-id",
	} {
		if got := req.Header.Get(name); got != "" {
			t.Fatalf("%s = %q, want empty for non-anthropic host", name, got)
		}
	}
}

func TestClaudeExecutor_UsesOptionsHeadersForClientVersionObservation(t *testing.T) {
	resetClaudeDeviceProfileCache()
	stabilize := true

	var captured capturedRequestForBilling
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		captured = capturedRequestForBilling{
			body:      bytes.Clone(body),
			userAgent: r.Header.Get("User-Agent"),
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","model":"claude-sonnet-4-6","role":"assistant","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer server.Close()

	executor := NewClaudeExecutor(&config.Config{
		ClaudeHeaderDefaults: config.ClaudeHeaderDefaults{
			StabilizeDeviceProfile: &stabilize,
		},
	})
	auth := &cliproxyauth.Auth{ProxyURL: "direct",
		FileName: "claude-file-auth.json",
		Provider: "claude",
		Attributes: map[string]string{
			"api_key":     "sk-ant-oat-test",
			"base_url":    server.URL,
			"tool_prefix": "disabled",
		},
	}
	payload := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`)
	headers := http.Header{
		"User-Agent":                  []string{"claude-cli/2.1.142 (external, cli)"},
		"X-Stainless-Package-Version": []string{"0.94.0"},
		"X-Stainless-Runtime-Version": []string{"v24.3.0"},
	}

	_, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "claude-sonnet-4-6",
		Payload: payload,
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("claude"), Headers: headers})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if captured.userAgent != "claude-cli/2.1.142 (external, cli)" {
		t.Fatalf("User-Agent = %q, want options header version", captured.userAgent)
	}
	if got := billingVersionFromBody(t, captured.body); got != "2.1.142" {
		t.Fatalf("billing cc_version = %q, want %q", got, "2.1.142")
	}
	observations := helps.ClaudeDeviceProfileObservations(auth, "")
	if len(observations) != 1 {
		t.Fatalf("observations length = %d, want 1: %#v", len(observations), observations)
	}
	if got := observations[0].Version; got != "2.1.142" {
		t.Fatalf("observation version = %q, want 2.1.142", got)
	}
}

func TestClaudeExecutorStream_AlignsBillingVersionWithStabilizedUserAgent(t *testing.T) {
	resetClaudeDeviceProfileCache()
	stabilize := true

	var captured capturedRequestForBilling
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		captured = capturedRequestForBilling{
			body:      bytes.Clone(body),
			userAgent: r.Header.Get("User-Agent"),
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"type\":\"message\",\"model\":\"claude-sonnet-4-6\",\"role\":\"assistant\",\"content\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\n"))
	}))
	defer server.Close()

	executor := NewClaudeExecutor(&config.Config{
		ClaudeHeaderDefaults: config.ClaudeHeaderDefaults{
			UserAgent:              "claude-cli/2.1.70 (external, cli)",
			PackageVersion:         "0.80.0",
			RuntimeVersion:         "v24.5.0",
			OS:                     "MacOS",
			Arch:                   "arm64",
			StabilizeDeviceProfile: &stabilize,
		},
	})
	auth := &cliproxyauth.Auth{ProxyURL: "direct",
		ID: "auth-billing-stream",
		Attributes: map[string]string{
			"api_key":     "sk-ant-oat-test",
			"base_url":    server.URL,
			"cloak_mode":  "always",
			"tool_prefix": "disabled",
		},
	}
	ctx := contextWithGinHeaders(map[string]string{
		"User-Agent":                  "claude-cli/2.1.81 (external, cli)",
		"X-Stainless-Package-Version": "0.81.0",
		"X-Stainless-Runtime-Version": "v24.6.0",
		"X-Stainless-Os":              "Linux",
		"X-Stainless-Arch":            "x64",
	})
	payload := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`)

	stream, err := executor.ExecuteStream(ctx, auth, cliproxyexecutor.Request{
		Model:   "claude-sonnet-4-6",
		Payload: payload,
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("claude")})
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}
	for chunk := range stream.Chunks {
		if chunk.Err != nil {
			t.Fatalf("stream chunk error = %v", chunk.Err)
		}
	}
	if captured.userAgent != "claude-cli/2.1.81 (external, cli)" {
		t.Fatalf("User-Agent = %q, want stabilized incoming version", captured.userAgent)
	}
	if got := billingVersionFromBody(t, captured.body); got != "2.1.81" {
		t.Fatalf("billing cc_version = %q, want %q", got, "2.1.81")
	}
}

func TestClaudeExecutorStream_RewritesStaleBillingVersionToStabilizedUserAgent(t *testing.T) {
	resetClaudeDeviceProfileCache()
	stabilize := true

	var captured capturedRequestForBilling
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		captured = capturedRequestForBilling{
			body:      bytes.Clone(body),
			userAgent: r.Header.Get("User-Agent"),
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"type\":\"message\",\"model\":\"claude-sonnet-4-6\",\"role\":\"assistant\",\"content\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\n"))
	}))
	defer server.Close()

	executor := NewClaudeExecutor(&config.Config{
		ClaudeHeaderDefaults: config.ClaudeHeaderDefaults{
			UserAgent:              "claude-cli/2.1.70 (external, cli)",
			PackageVersion:         "0.80.0",
			RuntimeVersion:         "v24.5.0",
			OS:                     "MacOS",
			Arch:                   "arm64",
			StabilizeDeviceProfile: &stabilize,
		},
	})
	auth := &cliproxyauth.Auth{ProxyURL: "direct",
		ID: "auth-billing-stale-stream",
		Attributes: map[string]string{
			"api_key":     "sk-ant-oat-test",
			"base_url":    server.URL,
			"cloak_mode":  "always",
			"tool_prefix": "disabled",
		},
	}
	ctx := contextWithGinHeaders(map[string]string{
		"User-Agent":                  "claude-cli/2.1.84 (external, cli)",
		"X-Stainless-Package-Version": "0.81.0",
		"X-Stainless-Runtime-Version": "v24.6.0",
		"X-Stainless-Os":              "Linux",
		"X-Stainless-Arch":            "x64",
	})
	payload := []byte(`{"system":[{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1.63.abc; cc_entrypoint=cli; cch=12345;"}],"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`)

	stream, err := executor.ExecuteStream(ctx, auth, cliproxyexecutor.Request{
		Model:   "claude-sonnet-4-6",
		Payload: payload,
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("claude")})
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}
	for chunk := range stream.Chunks {
		if chunk.Err != nil {
			t.Fatalf("stream chunk error = %v", chunk.Err)
		}
	}
	if captured.userAgent != "claude-cli/2.1.84 (external, cli)" {
		t.Fatalf("User-Agent = %q, want stabilized incoming version", captured.userAgent)
	}
	if got := billingVersionFromBody(t, captured.body); got != "2.1.84" {
		t.Fatalf("billing cc_version = %q, want %q", got, "2.1.84")
	}
}

func TestClaudeExecutorCountTokens_AlignsBillingVersionWithStabilizedUserAgent(t *testing.T) {
	resetClaudeDeviceProfileCache()
	stabilize := true

	var captured capturedRequestForBilling
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		captured = capturedRequestForBilling{
			body:      bytes.Clone(body),
			userAgent: r.Header.Get("User-Agent"),
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"input_tokens":1}`))
	}))
	defer server.Close()

	executor := NewClaudeExecutor(&config.Config{
		ClaudeHeaderDefaults: config.ClaudeHeaderDefaults{
			UserAgent:              "claude-cli/2.1.70 (external, cli)",
			PackageVersion:         "0.80.0",
			RuntimeVersion:         "v24.5.0",
			OS:                     "MacOS",
			Arch:                   "arm64",
			StabilizeDeviceProfile: &stabilize,
		},
	})
	auth := &cliproxyauth.Auth{ProxyURL: "direct",
		ID: "auth-billing-count",
		Attributes: map[string]string{
			"api_key":  "sk-ant-oat-test",
			"base_url": server.URL,
		},
	}
	ctx := contextWithGinHeaders(map[string]string{
		"User-Agent":                  "claude-cli/2.1.82 (external, cli)",
		"X-Stainless-Package-Version": "0.81.0",
		"X-Stainless-Runtime-Version": "v24.6.0",
		"X-Stainless-Os":              "Linux",
		"X-Stainless-Arch":            "x64",
	})
	payload := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`)

	_, err := executor.CountTokens(ctx, auth, cliproxyexecutor.Request{
		Model:   "claude-sonnet-4-6",
		Payload: payload,
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("claude")})
	if err != nil {
		t.Fatalf("CountTokens() error = %v", err)
	}
	if captured.userAgent != "claude-cli/2.1.82 (external, cli)" {
		t.Fatalf("User-Agent = %q, want stabilized incoming version", captured.userAgent)
	}
	if got := billingVersionFromBody(t, captured.body); got != "2.1.82" {
		t.Fatalf("billing cc_version = %q, want %q", got, "2.1.82")
	}
}

func TestClaudeExecutorCountTokens_RewritesStaleBillingVersionToStabilizedUserAgent(t *testing.T) {
	resetClaudeDeviceProfileCache()
	stabilize := true

	var captured capturedRequestForBilling
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		captured = capturedRequestForBilling{
			body:      bytes.Clone(body),
			userAgent: r.Header.Get("User-Agent"),
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"input_tokens":1}`))
	}))
	defer server.Close()

	executor := NewClaudeExecutor(&config.Config{
		ClaudeHeaderDefaults: config.ClaudeHeaderDefaults{
			UserAgent:              "claude-cli/2.1.70 (external, cli)",
			PackageVersion:         "0.80.0",
			RuntimeVersion:         "v24.5.0",
			OS:                     "MacOS",
			Arch:                   "arm64",
			StabilizeDeviceProfile: &stabilize,
		},
	})
	auth := &cliproxyauth.Auth{ProxyURL: "direct",
		ID: "auth-billing-stale-count",
		Attributes: map[string]string{
			"api_key":  "sk-ant-oat-test",
			"base_url": server.URL,
		},
	}
	ctx := contextWithGinHeaders(map[string]string{
		"User-Agent":                  "claude-cli/2.1.85 (external, cli)",
		"X-Stainless-Package-Version": "0.81.0",
		"X-Stainless-Runtime-Version": "v24.6.0",
		"X-Stainless-Os":              "Linux",
		"X-Stainless-Arch":            "x64",
	})
	payload := []byte(`{"system":[{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1.63.abc; cc_entrypoint=cli; cch=12345;"}],"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`)

	_, err := executor.CountTokens(ctx, auth, cliproxyexecutor.Request{
		Model:   "claude-sonnet-4-6",
		Payload: payload,
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("claude")})
	if err != nil {
		t.Fatalf("CountTokens() error = %v", err)
	}
	if captured.userAgent != "claude-cli/2.1.85 (external, cli)" {
		t.Fatalf("User-Agent = %q, want stabilized incoming version", captured.userAgent)
	}
	if got := billingVersionFromBody(t, captured.body); got != "2.1.85" {
		t.Fatalf("billing cc_version = %q, want %q", got, "2.1.85")
	}
}

func TestApplyClaudeHeaders_DoesNotInjectRemoved1MContextBeta(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", nil)
	req = req.WithContext(contextWithGinHeaders(map[string]string{
		"X-CPA-CLAUDE-1M": "1",
	}))
	auth := &cliproxyauth.Auth{ProxyURL: "direct", Attributes: map[string]string{"api_key": "sk-ant-oat-test"}}

	applyClaudeHeaders(req, auth, "sk-ant-oat-test", true, nil, nil)

	if got := req.Header.Get("Anthropic-Beta"); strings.Contains(got, "context-1m-2025-08-07") {
		t.Fatalf("Anthropic-Beta should not contain removed context-1m beta; header=%q", got)
	}
}

func TestStripClaudeToolPrefixFromResponse_NestedToolReference(t *testing.T) {
	input := []byte(`{"content":[{"type":"tool_result","tool_use_id":"toolu_123","content":[{"type":"tool_reference","tool_name":"proxy_mcp__nia__manage_resource"}]}]}`)
	out := stripClaudeToolPrefixFromResponse(input, "proxy_")
	got := gjson.GetBytes(out, "content.0.content.0.tool_name").String()
	if got != "mcp__nia__manage_resource" {
		t.Fatalf("nested tool_reference tool_name = %q, want %q", got, "mcp__nia__manage_resource")
	}
}

func TestApplyClaudeToolPrefix_NestedToolReferenceWithStringContent(t *testing.T) {
	// tool_result.content can be a string - should not be processed
	input := []byte(`{"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_123","content":"plain string result"}]}]}`)
	out := applyClaudeToolPrefix(input, "proxy_")
	got := gjson.GetBytes(out, "messages.0.content.0.content").String()
	if got != "plain string result" {
		t.Fatalf("string content should remain unchanged = %q", got)
	}
}

func TestApplyClaudeToolPrefix_SkipsBuiltinToolReference(t *testing.T) {
	input := []byte(`{"tools":[{"type":"web_search_20250305","name":"web_search"}],"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":[{"type":"tool_reference","tool_name":"web_search"}]}]}]}`)
	out := applyClaudeToolPrefix(input, "proxy_")
	got := gjson.GetBytes(out, "messages.0.content.0.content.0.tool_name").String()
	if got != "web_search" {
		t.Fatalf("built-in tool_reference should not be prefixed, got %q", got)
	}
}

func TestApplyClaudeHeaders_PrefersSavedManagedHeadersOverGinHeaders(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", nil)
	req = req.WithContext(contextWithGinHeaders(map[string]string{
		"User-Agent":                  "curl/8.1.2",
		"X-App":                       "browser",
		"X-Stainless-Package-Version": "9.9.9",
		"X-Stainless-Runtime-Version": "v1.0.0",
		"X-Stainless-Timeout":         "1",
	}))

	auth := &cliproxyauth.Auth{ProxyURL: "direct",
		Attributes: map[string]string{
			"api_key":      "sk-ant-oat-test",
			"header:X-App": "cli",
		},
		Metadata: map[string]any{
			"headers": map[string]any{
				"User-Agent":                  "claude-cli/2.1.63 (external, sdk-cli)",
				"X-Stainless-Package-Version": "0.74.0",
				"X-Stainless-Runtime-Version": "v22.20.0",
				"X-Stainless-Timeout":         "600",
			},
		},
	}

	applyClaudeHeaders(req, auth, "sk-ant-oat-test", true, nil, nil)

	if got := req.Header.Get("User-Agent"); got != "claude-cli/2.1.63 (external, sdk-cli)" {
		t.Fatalf("User-Agent = %q, want %q", got, "claude-cli/2.1.63 (external, sdk-cli)")
	}
	if got := req.Header.Get("X-App"); got != "cli" {
		t.Fatalf("X-App = %q, want %q", got, "cli")
	}
	if got := req.Header.Get("X-Stainless-Package-Version"); got != "0.74.0" {
		t.Fatalf("X-Stainless-Package-Version = %q, want %q", got, "0.74.0")
	}
	if got := req.Header.Get("X-Stainless-Runtime-Version"); got != "v22.20.0" {
		t.Fatalf("X-Stainless-Runtime-Version = %q, want %q", got, "v22.20.0")
	}
	if got := req.Header.Get("X-Stainless-Timeout"); got != "600" {
		t.Fatalf("X-Stainless-Timeout = %q, want %q", got, "600")
	}
	if got := req.Header.Get("Accept-Encoding"); got != "identity" {
		t.Fatalf("Accept-Encoding = %q, want %q", got, "identity")
	}
}

func TestApplyClaudeHeaders_PreservesClaudeCloakingDefaultsWithoutSavedHeaders(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", nil)
	req = req.WithContext(contextWithGinHeaders(map[string]string{
		"User-Agent": "curl/8.1.2",
	}))

	auth := &cliproxyauth.Auth{ProxyURL: "direct",
		Attributes: map[string]string{
			"api_key": "sk-ant-oat-test",
		},
	}

	applyClaudeHeaders(req, auth, "sk-ant-oat-test", true, nil, nil)

	if got := req.Header.Get("User-Agent"); got != "claude-cli/2.1.63 (external, cli)" {
		t.Fatalf("User-Agent = %q, want %q", got, "claude-cli/2.1.63 (external, cli)")
	}
	if got := req.Header.Get("X-App"); got != "cli" {
		t.Fatalf("X-App = %q, want %q", got, "cli")
	}
	if got := req.Header.Get("X-Stainless-Runtime-Version"); got != "v24.3.0" {
		t.Fatalf("X-Stainless-Runtime-Version = %q, want %q", got, "v24.3.0")
	}
	if got := req.Header.Get("Accept-Encoding"); got != "identity" {
		t.Fatalf("Accept-Encoding = %q, want %q", got, "identity")
	}
}

func TestNormalizeCacheControlTTL_DowngradesLaterOneHourBlocks(t *testing.T) {
	payload := []byte(`{
		"tools": [{"name":"t1","cache_control":{"type":"ephemeral","ttl":"1h"}}],
		"system": [{"type":"text","text":"s1","cache_control":{"type":"ephemeral"}}],
		"messages": [{"role":"user","content":[{"type":"text","text":"u1","cache_control":{"type":"ephemeral","ttl":"1h"}}]}]
	}`)

	out := normalizeCacheControlTTL(payload)

	if got := gjson.GetBytes(out, "tools.0.cache_control.ttl").String(); got != "1h" {
		t.Fatalf("tools.0.cache_control.ttl = %q, want %q", got, "1h")
	}
	if gjson.GetBytes(out, "messages.0.content.0.cache_control.ttl").Exists() {
		t.Fatalf("messages.0.content.0.cache_control.ttl should be removed after a default-5m block")
	}
}

func TestNormalizeCacheControlTTL_PreservesOriginalBytesWhenNoChange(t *testing.T) {
	// Payload where no TTL normalization is needed (all blocks use 1h with no
	// preceding 5m block). The text intentionally contains HTML chars (<, >, &)
	// that json.Marshal would escape to \u003c etc., altering byte identity.
	payload := []byte(`{"tools":[{"name":"t1","cache_control":{"type":"ephemeral","ttl":"1h"}}],"system":[{"type":"text","text":"<system-reminder>foo & bar</system-reminder>","cache_control":{"type":"ephemeral","ttl":"1h"}}],"messages":[{"role":"user","content":[{"type":"text","text":"hello"}]}]}`)

	out := normalizeCacheControlTTL(payload)

	if !bytes.Equal(out, payload) {
		t.Fatalf("normalizeCacheControlTTL altered bytes when no change was needed.\noriginal: %s\ngot:      %s", payload, out)
	}
}

func TestNormalizeCacheControlTTL_PreservesKeyOrderWhenModified(t *testing.T) {
	payload := []byte(`{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"u1","cache_control":{"type":"ephemeral","ttl":"1h"}}]}],"tools":[{"name":"t1","cache_control":{"type":"ephemeral"}}],"system":[{"type":"text","text":"s1","cache_control":{"type":"ephemeral"}}]}`)

	out := normalizeCacheControlTTL(payload)

	if gjson.GetBytes(out, "messages.0.content.0.cache_control.ttl").Exists() {
		t.Fatalf("messages.0.content.0.cache_control.ttl should be removed after a default-5m block")
	}

	outStr := string(out)
	idxModel := strings.Index(outStr, `"model"`)
	idxMessages := strings.Index(outStr, `"messages"`)
	idxTools := strings.Index(outStr, `"tools"`)
	idxSystem := strings.Index(outStr, `"system"`)
	if idxModel == -1 || idxMessages == -1 || idxTools == -1 || idxSystem == -1 {
		t.Fatalf("failed to locate top-level keys in output: %s", outStr)
	}
	if !(idxModel < idxMessages && idxMessages < idxTools && idxTools < idxSystem) {
		t.Fatalf("top-level key order changed:\noriginal: %s\ngot:      %s", payload, out)
	}
}

func TestEnforceCacheControlLimit_StripsNonLastToolBeforeMessages(t *testing.T) {
	payload := []byte(`{
		"tools": [
			{"name":"t1","cache_control":{"type":"ephemeral"}},
			{"name":"t2","cache_control":{"type":"ephemeral"}}
		],
		"system": [{"type":"text","text":"s1","cache_control":{"type":"ephemeral"}}],
		"messages": [
			{"role":"user","content":[{"type":"text","text":"u1","cache_control":{"type":"ephemeral"}}]},
			{"role":"user","content":[{"type":"text","text":"u2","cache_control":{"type":"ephemeral"}}]}
		]
	}`)

	out := enforceCacheControlLimit(payload, 4)

	if got := countCacheControls(out); got != 4 {
		t.Fatalf("cache_control count = %d, want 4", got)
	}
	if gjson.GetBytes(out, "tools.0.cache_control").Exists() {
		t.Fatalf("tools.0.cache_control should be removed first (non-last tool)")
	}
	if !gjson.GetBytes(out, "tools.1.cache_control").Exists() {
		t.Fatalf("tools.1.cache_control (last tool) should be preserved")
	}
	if !gjson.GetBytes(out, "messages.0.content.0.cache_control").Exists() || !gjson.GetBytes(out, "messages.1.content.0.cache_control").Exists() {
		t.Fatalf("message cache_control blocks should be preserved when non-last tool removal is enough")
	}
}

func TestEnforceCacheControlLimit_PreservesKeyOrderWhenModified(t *testing.T) {
	payload := []byte(`{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"u1","cache_control":{"type":"ephemeral"}},{"type":"text","text":"u2","cache_control":{"type":"ephemeral"}}]}],"tools":[{"name":"t1","cache_control":{"type":"ephemeral"}},{"name":"t2","cache_control":{"type":"ephemeral"}}],"system":[{"type":"text","text":"s1","cache_control":{"type":"ephemeral"}}]}`)

	out := enforceCacheControlLimit(payload, 4)

	if got := countCacheControls(out); got != 4 {
		t.Fatalf("cache_control count = %d, want 4", got)
	}
	if gjson.GetBytes(out, "tools.0.cache_control").Exists() {
		t.Fatalf("tools.0.cache_control should be removed first (non-last tool)")
	}

	outStr := string(out)
	idxModel := strings.Index(outStr, `"model"`)
	idxMessages := strings.Index(outStr, `"messages"`)
	idxTools := strings.Index(outStr, `"tools"`)
	idxSystem := strings.Index(outStr, `"system"`)
	if idxModel == -1 || idxMessages == -1 || idxTools == -1 || idxSystem == -1 {
		t.Fatalf("failed to locate top-level keys in output: %s", outStr)
	}
	if !(idxModel < idxMessages && idxMessages < idxTools && idxTools < idxSystem) {
		t.Fatalf("top-level key order changed:\noriginal: %s\ngot:      %s", payload, out)
	}
}

func TestEnforceCacheControlLimit_ToolOnlyPayloadStillRespectsLimit(t *testing.T) {
	payload := []byte(`{
		"tools": [
			{"name":"t1","cache_control":{"type":"ephemeral"}},
			{"name":"t2","cache_control":{"type":"ephemeral"}},
			{"name":"t3","cache_control":{"type":"ephemeral"}},
			{"name":"t4","cache_control":{"type":"ephemeral"}},
			{"name":"t5","cache_control":{"type":"ephemeral"}}
		]
	}`)

	out := enforceCacheControlLimit(payload, 4)

	if got := countCacheControls(out); got != 4 {
		t.Fatalf("cache_control count = %d, want 4", got)
	}
	if gjson.GetBytes(out, "tools.0.cache_control").Exists() {
		t.Fatalf("tools.0.cache_control should be removed to satisfy max=4")
	}
	if !gjson.GetBytes(out, "tools.4.cache_control").Exists() {
		t.Fatalf("last tool cache_control should be preserved when possible")
	}
}

func TestClaudeExecutor_CountTokens_AppliesCacheControlGuards(t *testing.T) {
	var seenBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		seenBody = bytes.Clone(body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"input_tokens":42}`))
	}))
	defer server.Close()

	executor := NewClaudeExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{ProxyURL: "direct", Attributes: map[string]string{
		"api_key":  "key-123",
		"base_url": server.URL,
	}}

	payload := []byte(`{
		"tools": [
			{"name":"t1","cache_control":{"type":"ephemeral","ttl":"1h"}},
			{"name":"t2","cache_control":{"type":"ephemeral"}}
		],
		"system": [
			{"type":"text","text":"s1","cache_control":{"type":"ephemeral","ttl":"1h"}},
			{"type":"text","text":"s2","cache_control":{"type":"ephemeral","ttl":"1h"}}
		],
		"messages": [
			{"role":"user","content":[{"type":"text","text":"u1","cache_control":{"type":"ephemeral","ttl":"1h"}}]},
			{"role":"user","content":[{"type":"text","text":"u2","cache_control":{"type":"ephemeral","ttl":"1h"}}]}
		]
	}`)

	_, err := executor.CountTokens(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "claude-3-5-haiku-20241022",
		Payload: payload,
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("claude")})
	if err != nil {
		t.Fatalf("CountTokens error: %v", err)
	}

	if len(seenBody) == 0 {
		t.Fatal("expected count_tokens request body to be captured")
	}
	if got := countCacheControls(seenBody); got > 4 {
		t.Fatalf("count_tokens body has %d cache_control blocks, want <= 4", got)
	}
	if hasTTLOrderingViolation(seenBody) {
		t.Fatalf("count_tokens body still has ttl ordering violations: %s", string(seenBody))
	}
}

func TestClaudeExecutor_ExecuteSanitizesSignaturesBeforeUpstream(t *testing.T) {
	var seenBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		seenBody = bytes.Clone(body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","model":"claude-sonnet-4-5","role":"assistant","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer server.Close()

	executor := NewClaudeExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{ProxyURL: "direct", Attributes: map[string]string{
		"api_key":  "key-123",
		"base_url": server.URL,
	}}

	payload := []byte(`{
		"model": "claude-sonnet-4-5",
		"max_tokens": 16,
		"messages": [
			{"role":"assistant","content":[
				{"type":"thinking","thinking":"drop this","signature":""},
				{"type":"text","text":"I will run git status."},
				{"type":"tool_use","id":"Bash-1","name":"Bash","input":{"command":"git status"},"signature":"bad","thoughtSignature":"bad2","model":"claude-opus-4-1"}
			]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"Bash-1","content":"ok"}]}
		]
	}`)

	if _, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "claude-sonnet-4-5",
		Payload: payload,
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("claude"),
		Stream:       false,
	}); err != nil {
		t.Fatalf("Execute error: %v", err)
	}

	parts := gjson.GetBytes(seenBody, "messages.0.content").Array()
	if len(parts) != 2 {
		t.Fatalf("messages.0.content length = %d, want 2; body=%s", len(parts), seenBody)
	}
	if parts[0].Get("type").String() != "text" {
		t.Fatalf("first remaining part = %s, want text", parts[0].Raw)
	}
	toolUse := parts[1]
	if toolUse.Get("type").String() != "tool_use" {
		t.Fatalf("second remaining part = %s, want tool_use", toolUse.Raw)
	}
	for _, path := range []string{"signature", "thoughtSignature", "model"} {
		if toolUse.Get(path).Exists() {
			t.Fatalf("tool_use.%s should be removed before upstream: %s", path, seenBody)
		}
	}
}

func hasTTLOrderingViolation(payload []byte) bool {
	seen5m := false
	violates := false

	checkCC := func(cc gjson.Result) {
		if !cc.Exists() || violates {
			return
		}
		ttl := cc.Get("ttl").String()
		if ttl != "1h" {
			seen5m = true
			return
		}
		if seen5m {
			violates = true
		}
	}

	tools := gjson.GetBytes(payload, "tools")
	if tools.IsArray() {
		tools.ForEach(func(_, tool gjson.Result) bool {
			checkCC(tool.Get("cache_control"))
			return !violates
		})
	}

	system := gjson.GetBytes(payload, "system")
	if system.IsArray() {
		system.ForEach(func(_, item gjson.Result) bool {
			checkCC(item.Get("cache_control"))
			return !violates
		})
	}

	messages := gjson.GetBytes(payload, "messages")
	if messages.IsArray() {
		messages.ForEach(func(_, msg gjson.Result) bool {
			content := msg.Get("content")
			if content.IsArray() {
				content.ForEach(func(_, item gjson.Result) bool {
					checkCC(item.Get("cache_control"))
					return !violates
				})
			}
			return !violates
		})
	}

	return violates
}

func TestClaudeExecutor_Execute_InvalidGzipErrorBodyReturnsDecodeMessage(t *testing.T) {
	testClaudeExecutorInvalidCompressedErrorBody(t, func(executor *ClaudeExecutor, auth *cliproxyauth.Auth, payload []byte) error {
		_, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
			Model:   "claude-3-5-sonnet-20241022",
			Payload: payload,
		}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("claude")})
		return err
	})
}

func TestClaudeExecutor_ExecuteStream_InvalidGzipErrorBodyReturnsDecodeMessage(t *testing.T) {
	testClaudeExecutorInvalidCompressedErrorBody(t, func(executor *ClaudeExecutor, auth *cliproxyauth.Auth, payload []byte) error {
		_, err := executor.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
			Model:   "claude-3-5-sonnet-20241022",
			Payload: payload,
		}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("claude")})
		return err
	})
}

func TestClaudeExecutor_CountTokens_InvalidGzipErrorBodyReturnsDecodeMessage(t *testing.T) {
	testClaudeExecutorInvalidCompressedErrorBody(t, func(executor *ClaudeExecutor, auth *cliproxyauth.Auth, payload []byte) error {
		_, err := executor.CountTokens(context.Background(), auth, cliproxyexecutor.Request{
			Model:   "claude-3-5-sonnet-20241022",
			Payload: payload,
		}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("claude")})
		return err
	})
}

func testClaudeExecutorInvalidCompressedErrorBody(
	t *testing.T,
	invoke func(executor *ClaudeExecutor, auth *cliproxyauth.Auth, payload []byte) error,
) {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "gzip")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte("not-a-valid-gzip-stream"))
	}))
	defer server.Close()

	executor := NewClaudeExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{ProxyURL: "direct", Attributes: map[string]string{
		"api_key":  "key-123",
		"base_url": server.URL,
	}}
	payload := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`)

	err := invoke(executor, auth, payload)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "failed to decode error response body") {
		t.Fatalf("expected decode failure message, got: %v", err)
	}
	if statusProvider, ok := err.(interface{ StatusCode() int }); !ok || statusProvider.StatusCode() != http.StatusBadRequest {
		t.Fatalf("expected status code 400, got: %v", err)
	}
}

func TestEnsureModelMaxTokens_UsesRegisteredMaxCompletionTokens(t *testing.T) {
	reg := registry.GetGlobalRegistry()
	clientID := "test-claude-max-completion-tokens-client"
	modelID := "test-claude-max-completion-tokens-model"
	reg.RegisterClient(clientID, "claude", []*registry.ModelInfo{{
		ID:                  modelID,
		Type:                "claude",
		OwnedBy:             "anthropic",
		Object:              "model",
		Created:             time.Now().Unix(),
		MaxCompletionTokens: 4096,
		UserDefined:         true,
	}})
	defer reg.UnregisterClient(clientID)

	input := []byte(`{"model":"test-claude-max-completion-tokens-model","messages":[{"role":"user","content":"hi"}]}`)
	out := ensureModelMaxTokens(input, modelID)

	if got := gjson.GetBytes(out, "max_tokens").Int(); got != 4096 {
		t.Fatalf("max_tokens = %d, want %d", got, 4096)
	}
}

func TestEnsureModelMaxTokens_DefaultsMissingValue(t *testing.T) {
	reg := registry.GetGlobalRegistry()
	clientID := "test-claude-default-max-tokens-client"
	modelID := "test-claude-default-max-tokens-model"
	reg.RegisterClient(clientID, "claude", []*registry.ModelInfo{{
		ID:          modelID,
		Type:        "claude",
		OwnedBy:     "anthropic",
		Object:      "model",
		Created:     time.Now().Unix(),
		UserDefined: true,
	}})
	defer reg.UnregisterClient(clientID)

	input := []byte(`{"model":"test-claude-default-max-tokens-model","messages":[{"role":"user","content":"hi"}]}`)
	out := ensureModelMaxTokens(input, modelID)

	if got := gjson.GetBytes(out, "max_tokens").Int(); got != defaultModelMaxTokens {
		t.Fatalf("max_tokens = %d, want %d", got, defaultModelMaxTokens)
	}
}

func TestEnsureModelMaxTokens_PreservesExplicitValue(t *testing.T) {
	reg := registry.GetGlobalRegistry()
	clientID := "test-claude-preserve-max-tokens-client"
	modelID := "test-claude-preserve-max-tokens-model"
	reg.RegisterClient(clientID, "claude", []*registry.ModelInfo{{
		ID:                  modelID,
		Type:                "claude",
		OwnedBy:             "anthropic",
		Object:              "model",
		Created:             time.Now().Unix(),
		MaxCompletionTokens: 4096,
		UserDefined:         true,
	}})
	defer reg.UnregisterClient(clientID)

	input := []byte(`{"model":"test-claude-preserve-max-tokens-model","max_tokens":2048,"messages":[{"role":"user","content":"hi"}]}`)
	out := ensureModelMaxTokens(input, modelID)

	if got := gjson.GetBytes(out, "max_tokens").Int(); got != 2048 {
		t.Fatalf("max_tokens = %d, want %d", got, 2048)
	}
}

func TestEnsureModelMaxTokens_SkipsUnregisteredModel(t *testing.T) {
	input := []byte(`{"model":"test-claude-unregistered-model","messages":[{"role":"user","content":"hi"}]}`)
	out := ensureModelMaxTokens(input, "test-claude-unregistered-model")

	if gjson.GetBytes(out, "max_tokens").Exists() {
		t.Fatalf("max_tokens should remain unset, got %s", gjson.GetBytes(out, "max_tokens").Raw)
	}
}

// TestClaudeExecutor_ExecuteStream_SetsIdentityAcceptEncoding verifies that streaming
// requests use Accept-Encoding: identity so the upstream cannot respond with a
// compressed SSE body that would silently break the line scanner.
func TestClaudeExecutor_ExecuteStream_SetsIdentityAcceptEncoding(t *testing.T) {
	var gotEncoding, gotAccept string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotEncoding = r.Header.Get("Accept-Encoding")
		gotAccept = r.Header.Get("Accept")
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"message_stop\"}\n\n"))
	}))
	defer server.Close()

	executor := NewClaudeExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{ProxyURL: "direct", Attributes: map[string]string{
		"api_key":  "key-123",
		"base_url": server.URL,
	}}
	payload := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`)

	result, err := executor.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "claude-3-5-sonnet-20241022",
		Payload: payload,
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("claude"),
	})
	if err != nil {
		t.Fatalf("ExecuteStream error: %v", err)
	}
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("unexpected chunk error: %v", chunk.Err)
		}
	}

	if gotEncoding != "identity" {
		t.Errorf("Accept-Encoding = %q, want %q", gotEncoding, "identity")
	}
	if gotAccept != "text/event-stream" {
		t.Errorf("Accept = %q, want %q", gotAccept, "text/event-stream")
	}
}

// TestClaudeExecutor_Execute_SetsCompressedAcceptEncoding verifies that non-streaming
// requests keep the full accept-encoding to allow response compression (which
// decodeResponseBody handles correctly).
func TestClaudeExecutor_Execute_SetsCompressedAcceptEncoding(t *testing.T) {
	var gotEncoding, gotAccept string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotEncoding = r.Header.Get("Accept-Encoding")
		gotAccept = r.Header.Get("Accept")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","model":"claude-3-5-sonnet-20241022","role":"assistant","content":[{"type":"text","text":"hi"}],"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer server.Close()

	executor := NewClaudeExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{ProxyURL: "direct", Attributes: map[string]string{
		"api_key":  "key-123",
		"base_url": server.URL,
	}}
	payload := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`)

	_, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "claude-3-5-sonnet-20241022",
		Payload: payload,
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("claude"),
	})
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}

	if gotEncoding != "gzip, deflate, br, zstd" {
		t.Errorf("Accept-Encoding = %q, want %q", gotEncoding, "gzip, deflate, br, zstd")
	}
	if gotAccept != "application/json" {
		t.Errorf("Accept = %q, want %q", gotAccept, "application/json")
	}
}

// TestClaudeExecutor_ExecuteStream_GzipSuccessBodyDecoded verifies that a streaming
// HTTP 200 response with Content-Encoding: gzip is correctly decompressed before
// the line scanner runs, so SSE chunks are not silently dropped.
func TestClaudeExecutor_ExecuteStream_GzipSuccessBodyDecoded(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	_, _ = gz.Write([]byte("data: {\"type\":\"message_stop\"}\n"))
	_ = gz.Close()
	compressedBody := buf.Bytes()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Content-Encoding", "gzip")
		_, _ = w.Write(compressedBody)
	}))
	defer server.Close()

	executor := NewClaudeExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{ProxyURL: "direct", Attributes: map[string]string{
		"api_key":  "key-123",
		"base_url": server.URL,
	}}
	payload := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`)

	result, err := executor.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "claude-3-5-sonnet-20241022",
		Payload: payload,
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("claude"),
	})
	if err != nil {
		t.Fatalf("ExecuteStream error: %v", err)
	}

	var combined strings.Builder
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("chunk error: %v", chunk.Err)
		}
		combined.Write(chunk.Payload)
	}

	if combined.Len() == 0 {
		t.Fatal("expected at least one chunk from gzip-encoded SSE body, got none (body was not decompressed)")
	}
	if !strings.Contains(combined.String(), "message_stop") {
		t.Errorf("expected SSE content in chunks, got: %q", combined.String())
	}
}

// TestDecodeResponseBody_MagicByteGzipNoHeader verifies that decodeResponseBody
// detects gzip-compressed content via magic bytes even when Content-Encoding is absent.
func TestDecodeResponseBody_MagicByteGzipNoHeader(t *testing.T) {
	const plaintext = "data: {\"type\":\"message_stop\"}\n"

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	_, _ = gz.Write([]byte(plaintext))
	_ = gz.Close()

	rc := io.NopCloser(&buf)
	decoded, err := decodeResponseBody(rc, "")
	if err != nil {
		t.Fatalf("decodeResponseBody error: %v", err)
	}
	defer decoded.Close()

	got, err := io.ReadAll(decoded)
	if err != nil {
		t.Fatalf("ReadAll error: %v", err)
	}
	if string(got) != plaintext {
		t.Errorf("decoded = %q, want %q", got, plaintext)
	}
}

// TestDecodeResponseBody_MagicByteZstdNoHeader verifies that decodeResponseBody
// detects zstd-compressed content via magic bytes even when Content-Encoding is absent.
func TestDecodeResponseBody_MagicByteZstdNoHeader(t *testing.T) {
	const plaintext = "data: {\"type\":\"message_stop\"}\n"

	var buf bytes.Buffer
	enc, err := zstd.NewWriter(&buf)
	if err != nil {
		t.Fatalf("zstd.NewWriter: %v", err)
	}
	_, _ = enc.Write([]byte(plaintext))
	_ = enc.Close()

	rc := io.NopCloser(&buf)
	decoded, err := decodeResponseBody(rc, "")
	if err != nil {
		t.Fatalf("decodeResponseBody error: %v", err)
	}
	defer decoded.Close()

	got, err := io.ReadAll(decoded)
	if err != nil {
		t.Fatalf("ReadAll error: %v", err)
	}
	if string(got) != plaintext {
		t.Errorf("decoded = %q, want %q", got, plaintext)
	}
}

// TestDecodeResponseBody_PlainTextNoHeader verifies that decodeResponseBody returns
// plain text untouched when Content-Encoding is absent and no magic bytes match.
func TestDecodeResponseBody_PlainTextNoHeader(t *testing.T) {
	const plaintext = "data: {\"type\":\"message_stop\"}\n"
	rc := io.NopCloser(strings.NewReader(plaintext))
	decoded, err := decodeResponseBody(rc, "")
	if err != nil {
		t.Fatalf("decodeResponseBody error: %v", err)
	}
	defer decoded.Close()

	got, err := io.ReadAll(decoded)
	if err != nil {
		t.Fatalf("ReadAll error: %v", err)
	}
	if string(got) != plaintext {
		t.Errorf("decoded = %q, want %q", got, plaintext)
	}
}

// TestClaudeExecutor_ExecuteStream_GzipNoContentEncodingHeader verifies the full
// pipeline: when the upstream returns a gzip-compressed SSE body WITHOUT setting
// Content-Encoding (a misbehaving upstream), the magic-byte sniff in
// decodeResponseBody still decompresses it, so chunks reach the caller.
func TestClaudeExecutor_ExecuteStream_GzipNoContentEncodingHeader(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	_, _ = gz.Write([]byte("data: {\"type\":\"message_stop\"}\n"))
	_ = gz.Close()
	compressedBody := buf.Bytes()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		// Intentionally omit Content-Encoding to simulate misbehaving upstream.
		_, _ = w.Write(compressedBody)
	}))
	defer server.Close()

	executor := NewClaudeExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{ProxyURL: "direct", Attributes: map[string]string{
		"api_key":  "key-123",
		"base_url": server.URL,
	}}
	payload := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`)

	result, err := executor.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "claude-3-5-sonnet-20241022",
		Payload: payload,
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("claude"),
	})
	if err != nil {
		t.Fatalf("ExecuteStream error: %v", err)
	}

	var combined strings.Builder
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("chunk error: %v", chunk.Err)
		}
		combined.Write(chunk.Payload)
	}

	if combined.Len() == 0 {
		t.Fatal("expected chunks from gzip body without Content-Encoding header, got none (magic-byte sniff failed)")
	}
	if !strings.Contains(combined.String(), "message_stop") {
		t.Errorf("unexpected chunk content: %q", combined.String())
	}
}

// TestClaudeExecutor_Execute_GzipErrorBodyNoContentEncodingHeader verifies that the
// error path (4xx) correctly decompresses a gzip body even when the upstream omits
// the Content-Encoding header.  This closes the gap left by PR #1771, which only
// fixed header-declared compression on the error path.
func TestClaudeExecutor_Execute_GzipErrorBodyNoContentEncodingHeader(t *testing.T) {
	const errJSON = `{"type":"error","error":{"type":"invalid_request_error","message":"test error"}}`

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	_, _ = gz.Write([]byte(errJSON))
	_ = gz.Close()
	compressedBody := buf.Bytes()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Intentionally omit Content-Encoding to simulate misbehaving upstream.
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write(compressedBody)
	}))
	defer server.Close()

	executor := NewClaudeExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{ProxyURL: "direct", Attributes: map[string]string{
		"api_key":  "key-123",
		"base_url": server.URL,
	}}
	payload := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`)

	_, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "claude-3-5-sonnet-20241022",
		Payload: payload,
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("claude"),
	})
	if err == nil {
		t.Fatal("expected an error for 400 response, got nil")
	}
	if !strings.Contains(err.Error(), "test error") {
		t.Errorf("error message should contain decompressed JSON, got: %q", err.Error())
	}
}

// TestClaudeExecutor_ExecuteStream_GzipErrorBodyNoContentEncodingHeader verifies
// the same for the streaming executor: 4xx gzip body without Content-Encoding is
// decoded and the error message is readable.
func TestClaudeExecutor_ExecuteStream_GzipErrorBodyNoContentEncodingHeader(t *testing.T) {
	const errJSON = `{"type":"error","error":{"type":"invalid_request_error","message":"stream test error"}}`

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	_, _ = gz.Write([]byte(errJSON))
	_ = gz.Close()
	compressedBody := buf.Bytes()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Intentionally omit Content-Encoding to simulate misbehaving upstream.
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write(compressedBody)
	}))
	defer server.Close()

	executor := NewClaudeExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{ProxyURL: "direct", Attributes: map[string]string{
		"api_key":  "key-123",
		"base_url": server.URL,
	}}
	payload := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`)

	_, err := executor.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "claude-3-5-sonnet-20241022",
		Payload: payload,
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("claude"),
	})
	if err == nil {
		t.Fatal("expected an error for 400 response, got nil")
	}
	if !strings.Contains(err.Error(), "stream test error") {
		t.Errorf("error message should contain decompressed JSON, got: %q", err.Error())
	}
}

// TestClaudeExecutor_ExecuteStream_AcceptEncodingOverrideCannotBypassIdentity verifies that the
// streaming executor enforces Accept-Encoding: identity regardless of auth.Attributes override.
func TestClaudeExecutor_ExecuteStream_AcceptEncodingOverrideCannotBypassIdentity(t *testing.T) {
	var gotEncoding string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotEncoding = r.Header.Get("Accept-Encoding")
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"message_stop\"}\n\n"))
	}))
	defer server.Close()

	executor := NewClaudeExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{ProxyURL: "direct", Attributes: map[string]string{
		"api_key":                "key-123",
		"base_url":               server.URL,
		"header:Accept-Encoding": "gzip, deflate, br, zstd",
	}}
	payload := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`)

	result, err := executor.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "claude-3-5-sonnet-20241022",
		Payload: payload,
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("claude"),
	})
	if err != nil {
		t.Fatalf("ExecuteStream error: %v", err)
	}
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("unexpected chunk error: %v", chunk.Err)
		}
	}

	if gotEncoding != "identity" {
		t.Errorf("Accept-Encoding = %q; stream path must enforce identity regardless of auth.Attributes override", gotEncoding)
	}
}

func TestClaudeExecutor_ExecuteStream_RepairsClaudeCodeTextInvokeToToolUse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(strings.Join([]string{
			"event: message_start\n",
			"data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"type\":\"message\",\"model\":\"claude-opus-4-8\",\"role\":\"assistant\",\"content\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\n",
			"event: content_block_start\n",
			"data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n",
			"event: content_block_delta\n",
			"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"准备执行。\\n\\n<invoke name=\\\"\"}}\n\n",
			"event: content_block_delta\n",
			"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"Bash\\\">\\n<parameter name=\\\"command\\\">echo hi</parameter>\\n<parameter name=\\\"description\\\">Run echo</parameter>\\n<parameter name=\\\"dangerouslyDisableSandbox\\\">true</parameter>\\n</invoke>\"}}\n\n",
			"event: content_block_stop\n",
			"data: {\"type\":\"content_block_stop\",\"index\":0}\n\n",
			"event: message_delta\n",
			"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\",\"stop_sequence\":null},\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}\n\n",
			"event: message_stop\n",
			"data: {\"type\":\"message_stop\"}\n\n",
		}, "")))
	}))
	defer server.Close()

	executor := NewClaudeExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{ProxyURL: "direct", Attributes: map[string]string{
		"api_key":  "key-123",
		"base_url": server.URL,
	}}
	payload := []byte(`{"stream":true,"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}],"tools":[{"name":"Bash","description":"Run shell","input_schema":{"type":"object"}}]}`)

	result, err := executor.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "claude-opus-4-8",
		Payload: payload,
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("claude"),
		Headers: http.Header{
			"User-Agent": []string{"claude-cli/2.1.158 (external, cli)"},
			"X-App":      []string{"cli"},
		},
	})
	if err != nil {
		t.Fatalf("ExecuteStream error: %v", err)
	}
	var got strings.Builder
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("unexpected chunk error: %v", chunk.Err)
		}
		got.Write(chunk.Payload)
	}
	body := got.String()
	if strings.Contains(body, "<invoke") {
		t.Fatalf("text invoke leaked to client: %s", body)
	}
	for _, want := range []string{
		`"type":"tool_use"`,
		`"name":"Bash"`,
		`"type":"input_json_delta"`,
		`echo hi`,
		`dangerouslyDisableSandbox`,
		`"stop_reason":"tool_use"`,
		`准备执行。`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("repaired stream missing %q: %s", want, body)
		}
	}
	if strings.Contains(body, `"stop_reason":"end_turn"`) {
		t.Fatalf("end_turn stop reason should be replaced after repaired tool use: %s", body)
	}
}

func TestClaudeExecutor_ExecuteStream_DoesNotRepairUnknownTextInvokeTool(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(strings.Join([]string{
			"event: message_start\n",
			"data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"type\":\"message\",\"model\":\"claude-opus-4-8\",\"role\":\"assistant\",\"content\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\n",
			"event: content_block_start\n",
			"data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n",
			"event: content_block_delta\n",
			"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"<invoke name=\\\"UnknownTool\\\"><parameter name=\\\"command\\\">echo hi</parameter></invoke>\"}}\n\n",
			"event: content_block_stop\n",
			"data: {\"type\":\"content_block_stop\",\"index\":0}\n\n",
			"event: message_delta\n",
			"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\",\"stop_sequence\":null},\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}\n\n",
			"event: message_stop\n",
			"data: {\"type\":\"message_stop\"}\n\n",
		}, "")))
	}))
	defer server.Close()

	executor := NewClaudeExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{ProxyURL: "direct", Attributes: map[string]string{
		"api_key":  "key-123",
		"base_url": server.URL,
	}}
	payload := []byte(`{"stream":true,"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}],"tools":[{"name":"Bash","description":"Run shell","input_schema":{"type":"object"}}]}`)

	result, err := executor.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "claude-opus-4-8",
		Payload: payload,
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("claude"),
		Headers: http.Header{
			"User-Agent": []string{"claude-cli/2.1.158 (external, cli)"},
			"X-App":      []string{"cli"},
		},
	})
	if err != nil {
		t.Fatalf("ExecuteStream error: %v", err)
	}
	var got strings.Builder
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("unexpected chunk error: %v", chunk.Err)
		}
		got.Write(chunk.Payload)
	}
	body := got.String()
	if !strings.Contains(body, `UnknownTool`) {
		t.Fatalf("unknown invoke should remain text: %s", body)
	}
	if strings.Contains(body, `"stop_reason":"tool_use"`) {
		t.Fatalf("unknown invoke must not force tool_use stop reason: %s", body)
	}
	if !strings.Contains(body, `"stop_reason":"end_turn"`) {
		t.Fatalf("unknown invoke should keep original end_turn: %s", body)
	}
}

func TestClaudeExecutor_ExecuteStream_DoesNotRepairInvokeWithTrailingText(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(strings.Join([]string{
			"event: message_start\n",
			"data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"type\":\"message\",\"model\":\"claude-opus-4-8\",\"role\":\"assistant\",\"content\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\n",
			"event: content_block_start\n",
			"data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n",
			"event: content_block_delta\n",
			"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"<invoke name=\\\"Bash\\\"><parameter name=\\\"command\\\">echo hi</parameter></invoke> then explain it\"}}\n\n",
			"event: content_block_stop\n",
			"data: {\"type\":\"content_block_stop\",\"index\":0}\n\n",
			"event: message_delta\n",
			"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\",\"stop_sequence\":null},\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}\n\n",
			"event: message_stop\n",
			"data: {\"type\":\"message_stop\"}\n\n",
		}, "")))
	}))
	defer server.Close()

	executor := NewClaudeExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{ProxyURL: "direct", Attributes: map[string]string{
		"api_key":  "key-123",
		"base_url": server.URL,
	}}
	payload := []byte(`{"stream":true,"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}],"tools":[{"name":"Bash","description":"Run shell","input_schema":{"type":"object"}}]}`)

	result, err := executor.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "claude-opus-4-8",
		Payload: payload,
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("claude"),
		Headers: http.Header{
			"User-Agent": []string{"claude-cli/2.1.158 (external, cli)"},
			"X-App":      []string{"cli"},
		},
	})
	if err != nil {
		t.Fatalf("ExecuteStream error: %v", err)
	}
	var got strings.Builder
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("unexpected chunk error: %v", chunk.Err)
		}
		got.Write(chunk.Payload)
	}
	body := got.String()
	if !strings.Contains(body, `then explain it`) {
		t.Fatalf("trailing text should remain in text stream: %s", body)
	}
	if strings.Contains(body, `"stop_reason":"tool_use"`) {
		t.Fatalf("trailing text invoke must not force tool_use stop reason: %s", body)
	}
}

func TestClaudeExecutor_ExecuteStream_DoesNotRepairInvokeWithTrailingTextInLaterDelta(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(strings.Join([]string{
			"event: message_start\n",
			"data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"type\":\"message\",\"model\":\"claude-opus-4-8\",\"role\":\"assistant\",\"content\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\n",
			"event: content_block_start\n",
			"data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n",
			"event: content_block_delta\n",
			"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"<invoke name=\\\"Bash\\\"><parameter name=\\\"command\\\">echo hi</parameter></invoke>\"}}\n\n",
			"event: content_block_delta\n",
			"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\" then explain it\"}}\n\n",
			"event: content_block_stop\n",
			"data: {\"type\":\"content_block_stop\",\"index\":0}\n\n",
			"event: message_delta\n",
			"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\",\"stop_sequence\":null},\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}\n\n",
			"event: message_stop\n",
			"data: {\"type\":\"message_stop\"}\n\n",
		}, "")))
	}))
	defer server.Close()

	executor := NewClaudeExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{ProxyURL: "direct", Attributes: map[string]string{
		"api_key":  "key-123",
		"base_url": server.URL,
	}}
	payload := []byte(`{"stream":true,"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}],"tools":[{"name":"Bash","description":"Run shell","input_schema":{"type":"object"}}]}`)

	result, err := executor.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "claude-opus-4-8",
		Payload: payload,
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("claude"),
		Headers: http.Header{
			"User-Agent": []string{"claude-cli/2.1.158 (external, cli)"},
			"X-App":      []string{"cli"},
		},
	})
	if err != nil {
		t.Fatalf("ExecuteStream error: %v", err)
	}
	var got strings.Builder
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("unexpected chunk error: %v", chunk.Err)
		}
		got.Write(chunk.Payload)
	}
	body := got.String()
	if !strings.Contains(body, `Bash`) || !strings.Contains(body, `echo hi`) || !strings.Contains(body, `then explain it`) {
		t.Fatalf("split trailing text invoke should remain text: %s", body)
	}
	if strings.Contains(body, `"stop_reason":"tool_use"`) || strings.Contains(body, `toolu_repaired_`) {
		t.Fatalf("split trailing text invoke must not be repaired to tool_use: %s", body)
	}
	if !strings.Contains(body, `"stop_reason":"end_turn"`) {
		t.Fatalf("split trailing text invoke should keep original end_turn: %s", body)
	}
}

func TestClaudeExecutor_ExecuteStream_DoesNotRepairNonClaudeCodeCliHeader(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(strings.Join([]string{
			"event: message_start\n",
			"data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"type\":\"message\",\"model\":\"claude-opus-4-8\",\"role\":\"assistant\",\"content\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\n",
			"event: content_block_start\n",
			"data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n",
			"event: content_block_delta\n",
			"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"<invoke name=\\\"Bash\\\"><parameter name=\\\"command\\\">echo hi</parameter></invoke>\"}}\n\n",
			"event: content_block_stop\n",
			"data: {\"type\":\"content_block_stop\",\"index\":0}\n\n",
			"event: message_delta\n",
			"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\",\"stop_sequence\":null},\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}\n\n",
			"event: message_stop\n",
			"data: {\"type\":\"message_stop\"}\n\n",
		}, "")))
	}))
	defer server.Close()

	executor := NewClaudeExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{ProxyURL: "direct", Attributes: map[string]string{
		"api_key":  "key-123",
		"base_url": server.URL,
	}}
	payload := []byte(`{"stream":true,"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}],"tools":[{"name":"Bash","description":"Run shell","input_schema":{"type":"object"}}]}`)

	result, err := executor.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "claude-opus-4-8",
		Payload: payload,
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("claude"),
		Headers: http.Header{
			"User-Agent": []string{"curl/8.7.1"},
			"X-App":      []string{"cli"},
		},
	})
	if err != nil {
		t.Fatalf("ExecuteStream error: %v", err)
	}
	var got strings.Builder
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("unexpected chunk error: %v", chunk.Err)
		}
		got.Write(chunk.Payload)
	}
	body := got.String()
	if !strings.Contains(body, `<invoke name=\"Bash\"`) {
		t.Fatalf("non Claude Code cli header should not be repaired: %s", body)
	}
	if strings.Contains(body, `"stop_reason":"tool_use"`) || strings.Contains(body, `toolu_repaired_`) {
		t.Fatalf("non Claude Code cli header must not force tool_use: %s", body)
	}
	if !strings.Contains(body, `"stop_reason":"end_turn"`) {
		t.Fatalf("non Claude Code cli header should keep original end_turn: %s", body)
	}
}

func expectedClaudeCodeStaticPrompt() string {
	return strings.Join([]string{
		helps.ClaudeCodeIntro,
		helps.ClaudeCodeSystem,
		helps.ClaudeCodeDoingTasks,
		helps.ClaudeCodeToneAndStyle,
		helps.ClaudeCodeOutputEfficiency,
	}, "\n\n")
}

func expectedForwardedSystemReminder(text string) string {
	return fmt.Sprintf(`<system-reminder>
As you answer the user's questions, you can use the following context from the system:
%s

IMPORTANT: this context may or may not be relevant to your tasks. You should not respond to this context unless it is highly relevant to your task.
</system-reminder>
`, text)
}

// Test case 1: String system prompt is preserved by forwarding it to the first user message
func TestCheckSystemInstructionsWithMode_StringSystemPreserved(t *testing.T) {
	payload := []byte(`{"system":"You are a helpful assistant.","messages":[{"role":"user","content":"hi"}]}`)

	out := checkSystemInstructionsWithMode(payload, false)

	system := gjson.GetBytes(out, "system")
	if !system.IsArray() {
		t.Fatalf("system should be an array, got %s", system.Type)
	}

	blocks := system.Array()
	if len(blocks) != 3 {
		t.Fatalf("expected 3 system blocks, got %d", len(blocks))
	}

	if !strings.HasPrefix(blocks[0].Get("text").String(), "x-anthropic-billing-header:") {
		t.Fatalf("blocks[0] should be billing header, got %q", blocks[0].Get("text").String())
	}
	if blocks[1].Get("text").String() != "You are Claude Code, Anthropic's official CLI for Claude." {
		t.Fatalf("blocks[1] should be agent block, got %q", blocks[1].Get("text").String())
	}
	if blocks[2].Get("text").String() != expectedClaudeCodeStaticPrompt() {
		t.Fatalf("blocks[2] should be static Claude Code prompt, got %q", blocks[2].Get("text").String())
	}
	if blocks[2].Get("cache_control").Exists() {
		t.Fatalf("blocks[2] should not have cache_control, got %s", blocks[2].Get("cache_control").Raw)
	}

	if got := gjson.GetBytes(out, "messages.0.content").String(); got != expectedForwardedSystemReminder("You are a helpful assistant.")+"hi" {
		t.Fatalf("messages[0].content should include forwarded system prompt, got %q", got)
	}
}

// Test case 2: Strict mode keeps only the injected Claude Code system blocks
func TestCheckSystemInstructionsWithMode_StringSystemStrict(t *testing.T) {
	payload := []byte(`{"system":"You are a helpful assistant.","messages":[{"role":"user","content":"hi"}]}`)

	out := checkSystemInstructionsWithMode(payload, true)

	blocks := gjson.GetBytes(out, "system").Array()
	if len(blocks) != 3 {
		t.Fatalf("strict mode should produce 3 injected blocks, got %d", len(blocks))
	}
	if got := gjson.GetBytes(out, "messages.0.content").String(); got != "hi" {
		t.Fatalf("strict mode should not forward system prompt into messages, got %q", got)
	}
}

// Test case 3: Empty string system prompt does not alter the first user message
func TestCheckSystemInstructionsWithMode_EmptyStringSystemIgnored(t *testing.T) {
	payload := []byte(`{"system":"","messages":[{"role":"user","content":"hi"}]}`)

	out := checkSystemInstructionsWithMode(payload, false)

	blocks := gjson.GetBytes(out, "system").Array()
	if len(blocks) != 3 {
		t.Fatalf("empty string system should still produce 3 injected blocks, got %d", len(blocks))
	}
	if got := gjson.GetBytes(out, "messages.0.content").String(); got != "hi" {
		t.Fatalf("empty string system should not alter messages, got %q", got)
	}
}

// Test case 4: Array system prompt is forwarded to the first user message
func TestCheckSystemInstructionsWithMode_ArraySystemStillWorks(t *testing.T) {
	payload := []byte(`{"system":[{"type":"text","text":"Be concise."}],"messages":[{"role":"user","content":"hi"}]}`)

	out := checkSystemInstructionsWithMode(payload, false)

	blocks := gjson.GetBytes(out, "system").Array()
	if len(blocks) != 3 {
		t.Fatalf("expected 3 system blocks, got %d", len(blocks))
	}
	if blocks[2].Get("text").String() != expectedClaudeCodeStaticPrompt() {
		t.Fatalf("blocks[2] should be static Claude Code prompt, got %q", blocks[2].Get("text").String())
	}
	if got := gjson.GetBytes(out, "messages.0.content").String(); got != expectedForwardedSystemReminder("Be concise.")+"hi" {
		t.Fatalf("messages[0].content should include forwarded array system prompt, got %q", got)
	}
}

// Test case 5: Special characters in string system prompt survive forwarding
func TestCheckSystemInstructionsWithMode_StringWithSpecialChars(t *testing.T) {
	payload := []byte(`{"system":"Use <xml> tags & \"quotes\" in output.","messages":[{"role":"user","content":"hi"}]}`)

	out := checkSystemInstructionsWithMode(payload, false)

	blocks := gjson.GetBytes(out, "system").Array()
	if len(blocks) != 3 {
		t.Fatalf("expected 3 system blocks, got %d", len(blocks))
	}
	if got := gjson.GetBytes(out, "messages.0.content").String(); got != expectedForwardedSystemReminder(`Use <xml> tags & "quotes" in output.`)+"hi" {
		t.Fatalf("forwarded system prompt text mangled, got %q", got)
	}
}

func TestClaudeExecutor_ExperimentalCCHSigningDisabledByDefaultKeepsLegacyHeader(t *testing.T) {
	var seenBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		seenBody = bytes.Clone(body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","model":"claude-3-5-sonnet","role":"assistant","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer server.Close()

	executor := NewClaudeExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{ProxyURL: "direct", Attributes: map[string]string{
		"api_key":  "key-123",
		"base_url": server.URL,
	}}
	payload := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`)

	_, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "claude-3-5-sonnet-20241022",
		Payload: payload,
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("claude")})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if len(seenBody) == 0 {
		t.Fatal("expected request body to be captured")
	}

	billingHeader := gjson.GetBytes(seenBody, "system.0.text").String()
	if !strings.HasPrefix(billingHeader, "x-anthropic-billing-header:") {
		t.Fatalf("system.0.text = %q, want billing header", billingHeader)
	}
	if strings.Contains(billingHeader, "cch=00000;") {
		t.Fatalf("legacy mode should not forward cch placeholder, got %q", billingHeader)
	}
}

func TestClaudeExecutor_ExperimentalCCHSigningOptInSignsFinalBody(t *testing.T) {
	var seenBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		seenBody = bytes.Clone(body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","model":"claude-3-5-sonnet","role":"assistant","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer server.Close()

	executor := NewClaudeExecutor(&config.Config{
		ClaudeKey: []config.ClaudeKey{{
			APIKey:                 "key-123",
			BaseURL:                server.URL,
			ExperimentalCCHSigning: true,
		}},
	})
	auth := &cliproxyauth.Auth{ProxyURL: "direct", Attributes: map[string]string{
		"api_key":  "key-123",
		"base_url": server.URL,
	}}
	const messageText = "please keep literal cch=00000 in this message"
	payload := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"please keep literal cch=00000 in this message"}]}]}`)

	_, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "claude-3-5-sonnet-20241022",
		Payload: payload,
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("claude")})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if len(seenBody) == 0 {
		t.Fatal("expected request body to be captured")
	}
	if got := gjson.GetBytes(seenBody, "messages.0.content.0.text").String(); got != messageText {
		t.Fatalf("message text = %q, want %q", got, messageText)
	}

	billingPattern := regexp.MustCompile(`(x-anthropic-billing-header:[^"]*?\bcch=)([0-9a-f]{5})(;)`)
	match := billingPattern.FindSubmatch(seenBody)
	if match == nil {
		t.Fatalf("expected signed billing header in body: %s", string(seenBody))
	}
	actualCCH := string(match[2])
	unsignedBody := billingPattern.ReplaceAll(seenBody, []byte(`${1}00000${3}`))
	wantCCH := fmt.Sprintf("%05x", xxHash64.Checksum(unsignedBody, 0x6E52736AC806831E)&0xFFFFF)
	if actualCCH != wantCCH {
		t.Fatalf("cch = %q, want %q\nbody: %s", actualCCH, wantCCH, string(seenBody))
	}
}

func TestApplyCloaking_PreservesConfiguredStrictModeAndSensitiveWordsWhenModeOmitted(t *testing.T) {
	cfg := &config.Config{
		ClaudeKey: []config.ClaudeKey{{
			APIKey: "key-123",
			Cloak: &config.CloakConfig{
				StrictMode:     true,
				SensitiveWords: []string{"proxy"},
			},
		}},
	}
	auth := &cliproxyauth.Auth{ProxyURL: "direct", Attributes: map[string]string{"api_key": "key-123"}}
	payload := []byte(`{"system":"proxy rules","messages":[{"role":"user","content":[{"type":"text","text":"proxy access"}]}]}`)

	out := applyCloaking(context.Background(), cfg, auth, payload, "claude-3-5-sonnet-20241022", "key-123", "2.1.63")

	blocks := gjson.GetBytes(out, "system").Array()
	if len(blocks) != 3 {
		t.Fatalf("expected strict mode to keep the 3 injected Claude Code system blocks, got %d", len(blocks))
	}
	if got := gjson.GetBytes(out, "messages.0.content.#").Int(); got != 1 {
		t.Fatalf("strict mode should not prepend a forwarded system reminder block, got %d content blocks", got)
	}
	if got := gjson.GetBytes(out, "messages.0.content.0.text").String(); !strings.Contains(got, "\u200B") {
		t.Fatalf("expected configured sensitive word obfuscation to apply, got %q", got)
	}
}

// ctxWithUserAgent builds a context carrying a gin request with the given
// User-Agent so applyCloaking can resolve the client type via getClientUserAgent.
func ctxWithUserAgent(userAgent string) context.Context {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginReq := httptest.NewRequest(http.MethodPost, "http://localhost/v1/messages", nil)
	ginReq.Header.Set("User-Agent", userAgent)
	ginCtx.Request = ginReq
	req := httptest.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", nil)
	return context.WithValue(req.Context(), "gin", ginCtx)
}

// TestApplyCloaking_InjectsDeviceIDForClaudeCLIButNotSystemBlocks covers P1.2:
// real claude-cli clients are excluded from the broader cloak (no injected system
// blocks), yet the account-scoped synthetic device_id must still be applied.
func TestApplyCloaking_InjectsDeviceIDForClaudeCLIButNotSystemBlocks(t *testing.T) {
	cfg := &config.Config{AuthDir: t.TempDir()}
	auth := &cliproxyauth.Auth{ProxyURL: "direct", FileName: "account-a.json", Attributes: map[string]string{"api_key": "key-123"}}
	// metadata.user_id is sent by claude-cli as a JSON *string* (not an object);
	// Anthropic validates it as an opaque string.
	payload := []byte(`{"system":"original system","metadata":{"user_id":"{\"device_id\":\"realdevice\",\"account_uuid\":\"\",\"session_id\":\"sess-1\"}"},"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`)

	ctx := ctxWithUserAgent("claude-cli/2.1.60 (external, cli)")
	out := applyCloaking(ctx, cfg, auth, payload, "claude-3-5-sonnet-20241022", "key-123", "2.1.63")

	// metadata.user_id must stay a JSON string (object form would 400 at Anthropic).
	userIDField := gjson.GetBytes(out, "metadata.user_id")
	if userIDField.Type != gjson.String {
		t.Fatalf("expected metadata.user_id to remain a JSON string, got type=%v raw=%q", userIDField.Type, userIDField.Raw)
	}
	inner := userIDField.String()
	// device_id must be rewritten to a synthetic 64-hex value.
	device := gjson.Get(inner, "device_id").String()
	if device == "realdevice" || len(device) != 64 {
		t.Fatalf("expected synthetic 64-hex device_id for claude-cli, got %q", device)
	}
	// session_id preserved.
	if got := gjson.Get(inner, "session_id").String(); got != "sess-1" {
		t.Fatalf("expected session_id preserved, got %q", got)
	}
	// The original system string must NOT be replaced with injected Claude Code
	// system blocks (claude-cli is excluded from the broader cloak).
	system := gjson.GetBytes(out, "system")
	if system.IsArray() {
		t.Fatalf("expected claude-cli system to remain the original (no injected cloak blocks), got array: %s", system.Raw)
	}
	if got := system.String(); got != "original system" {
		t.Fatalf("expected claude-cli system left untouched, got %q", got)
	}
}

// TestApplyCloaking_DeviceIDDiffersBetweenAccounts asserts the anti-correlation
// invariant at the cloak layer: distinct accounts derive distinct device IDs.
func TestApplyCloaking_DeviceIDDiffersBetweenAccounts(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{AuthDir: dir}
	payload := []byte(`{"metadata":{"user_id":"{\"device_id\":\"x\",\"account_uuid\":\"\",\"session_id\":\"s\"}"},"messages":[]}`)
	ctx := ctxWithUserAgent("claude-cli/2.1.60 (external, cli)")

	authA := &cliproxyauth.Auth{ProxyURL: "direct", FileName: "account-a.json", Attributes: map[string]string{"api_key": "key-a"}}
	authB := &cliproxyauth.Auth{ProxyURL: "direct", FileName: "account-b.json", Attributes: map[string]string{"api_key": "key-b"}}

	outA := applyCloaking(ctx, cfg, authA, payload, "claude-3-5-sonnet-20241022", "key-a", "2.1.63")
	outB := applyCloaking(ctx, cfg, authB, payload, "claude-3-5-sonnet-20241022", "key-b", "2.1.63")

	// metadata.user_id must remain a JSON string on both paths; device_id is read
	// from the inner JSON text.
	fieldA := gjson.GetBytes(outA, "metadata.user_id")
	fieldB := gjson.GetBytes(outB, "metadata.user_id")
	if fieldA.Type != gjson.String || fieldB.Type != gjson.String {
		t.Fatalf("expected metadata.user_id to remain a JSON string, got typeA=%v typeB=%v", fieldA.Type, fieldB.Type)
	}
	devA := gjson.Get(fieldA.String(), "device_id").String()
	devB := gjson.Get(fieldB.String(), "device_id").String()
	if devA == "" || devB == "" || devA == devB {
		t.Fatalf("expected distinct device IDs per account, got %q and %q", devA, devB)
	}
}

func TestNormalizeClaudeTemperatureForThinking_AdaptiveCoercesToOne(t *testing.T) {
	payload := []byte(`{"temperature":0,"thinking":{"type":"adaptive"},"output_config":{"effort":"max"}}`)
	out := normalizeClaudeTemperatureForThinking(payload)

	if got := gjson.GetBytes(out, "temperature").Float(); got != 1 {
		t.Fatalf("temperature = %v, want 1", got)
	}
}

func TestNormalizeClaudeTemperatureForThinking_EnabledCoercesToOne(t *testing.T) {
	payload := []byte(`{"temperature":0.2,"thinking":{"type":"enabled","budget_tokens":2048}}`)
	out := normalizeClaudeTemperatureForThinking(payload)

	if got := gjson.GetBytes(out, "temperature").Float(); got != 1 {
		t.Fatalf("temperature = %v, want 1", got)
	}
}

func TestNormalizeClaudeTemperatureForThinking_NoThinkingLeavesTemperatureAlone(t *testing.T) {
	payload := []byte(`{"temperature":0,"messages":[{"role":"user","content":"hi"}]}`)
	out := normalizeClaudeTemperatureForThinking(payload)

	if got := gjson.GetBytes(out, "temperature").Float(); got != 0 {
		t.Fatalf("temperature = %v, want 0", got)
	}
}

func TestNormalizeClaudeTemperatureForThinking_AfterForcedToolChoiceKeepsOriginalTemperature(t *testing.T) {
	payload := []byte(`{"temperature":0,"thinking":{"type":"adaptive"},"output_config":{"effort":"max"},"tool_choice":{"type":"any"}}`)
	out := disableThinkingIfToolChoiceForced(payload)
	out = normalizeClaudeTemperatureForThinking(out)

	if gjson.GetBytes(out, "thinking").Exists() {
		t.Fatalf("thinking should be removed when tool_choice forces tool use")
	}
	if got := gjson.GetBytes(out, "temperature").Float(); got != 0 {
		t.Fatalf("temperature = %v, want 0", got)
	}
}

func TestRemapOAuthToolNames_TitleCase_NoReverseNeeded(t *testing.T) {
	body := []byte(`{"tools":[{"name":"Bash","description":"Run shell commands","input_schema":{"type":"object","properties":{"cmd":{"type":"string"}}}}],"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`)

	out, reverseMap := remapOAuthToolNames(body)
	if len(reverseMap) != 0 {
		t.Fatalf("reverseMap = %v, want empty", reverseMap)
	}
	if got := gjson.GetBytes(out, "tools.0.name").String(); got != "Bash" {
		t.Fatalf("tools.0.name = %q, want %q", got, "Bash")
	}

	resp := []byte(`{"content":[{"type":"tool_use","id":"toolu_01","name":"Bash","input":{"cmd":"ls"}}]}`)
	reversed := reverseRemapOAuthToolNames(resp, reverseMap)
	if got := gjson.GetBytes(reversed, "content.0.name").String(); got != "Bash" {
		t.Fatalf("content.0.name = %q, want %q", got, "Bash")
	}
}

func TestRemapOAuthToolNames_Lowercase_ReverseApplied(t *testing.T) {
	body := []byte(`{"tools":[{"name":"bash","description":"Run shell commands","input_schema":{"type":"object","properties":{"cmd":{"type":"string"}}}}],"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`)

	out, reverseMap := remapOAuthToolNames(body)
	if reverseMap["Bash"] != "bash" {
		t.Fatalf("reverseMap = %v, want entry Bash->bash", reverseMap)
	}
	if got := gjson.GetBytes(out, "tools.0.name").String(); got != "Bash" {
		t.Fatalf("tools.0.name = %q, want %q", got, "Bash")
	}

	resp := []byte(`{"content":[{"type":"tool_use","id":"toolu_01","name":"Bash","input":{"cmd":"ls"}}]}`)
	reversed := reverseRemapOAuthToolNames(resp, reverseMap)
	if got := gjson.GetBytes(reversed, "content.0.name").String(); got != "bash" {
		t.Fatalf("content.0.name = %q, want %q", got, "bash")
	}
}

// TestRemapOAuthToolNames_MixedCase_OnlyRenamedToolsReversed is the regression
// test for a case where a single request contains both a TitleCase tool (which
// must pass through unchanged) and a lowercase tool that we forward-rename.
// Before the fix, triggering ANY forward rename caused the reverse pass to
// lowercase every TitleCase tool in the response using a global reverse map,
// corrupting tool names the client originally sent in TitleCase (notably Amp
// CLI's `Bash`, which its registry lookup cannot find as `bash`).
func TestRemapOAuthToolNames_MixedCase_OnlyRenamedToolsReversed(t *testing.T) {
	body := []byte(`{"tools":[` +
		`{"name":"Bash","input_schema":{"type":"object","properties":{"cmd":{"type":"string"}}}},` +
		`{"name":"glob","input_schema":{"type":"object","properties":{"filePattern":{"type":"string"}}}}` +
		`]}`)

	out, reverseMap := remapOAuthToolNames(body)

	// Forward: TitleCase `Bash` is not a forward-map key, must pass through.
	if got := gjson.GetBytes(out, "tools.0.name").String(); got != "Bash" {
		t.Fatalf("tools.0.name = %q, want %q (TitleCase tool must not be renamed)", got, "Bash")
	}
	// Forward: `glob` is a forward-map key, upstream sees `Glob`.
	if got := gjson.GetBytes(out, "tools.1.name").String(); got != "Glob" {
		t.Fatalf("tools.1.name = %q, want %q", got, "Glob")
	}

	// Reverse map records ONLY the rename that happened.
	if len(reverseMap) != 1 || reverseMap["Glob"] != "glob" {
		t.Fatalf("reverseMap = %v, want {Glob:glob}", reverseMap)
	}

	// Upstream responds with a `Bash` tool_use. Since we never renamed `Bash`,
	// reverseRemap MUST leave it alone.
	bashResp := []byte(`{"content":[{"type":"tool_use","id":"toolu_01","name":"Bash","input":{"cmd":"ls"}}]}`)
	reversed := reverseRemapOAuthToolNames(bashResp, reverseMap)
	if got := gjson.GetBytes(reversed, "content.0.name").String(); got != "Bash" {
		t.Fatalf("content.0.name = %q, want %q (Bash must be preserved; was never forward-renamed)", got, "Bash")
	}

	// Upstream responds with a `Glob` tool_use. Since we renamed `glob`→`Glob`,
	// reverseRemap MUST restore the original `glob`.
	globResp := []byte(`{"content":[{"type":"tool_use","id":"toolu_02","name":"Glob","input":{"filePattern":"**/*.go"}}]}`)
	reversed = reverseRemapOAuthToolNames(globResp, reverseMap)
	if got := gjson.GetBytes(reversed, "content.0.name").String(); got != "glob" {
		t.Fatalf("content.0.name = %q, want %q (Glob must be restored to client's original `glob`)", got, "glob")
	}
}

// TestReverseRemapOAuthToolNamesFromStreamLine_HonorsPerRequestMap guards the
// SSE streaming code path against the same mixed-case bug.
func TestReverseRemapOAuthToolNamesFromStreamLine_HonorsPerRequestMap(t *testing.T) {
	reverseMap := map[string]string{"Glob": "glob"}

	// Bash block was never renamed, must pass through as-is.
	bashLine := []byte(`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_01","name":"Bash","input":{}}}`)
	out := reverseRemapOAuthToolNamesFromStreamLine(bashLine, reverseMap)
	if !bytes.Contains(out, []byte(`"name":"Bash"`)) {
		t.Fatalf("Bash should be preserved, got: %s", string(out))
	}
	if bytes.Contains(out, []byte(`"name":"bash"`)) {
		t.Fatalf("Bash must not be lowercased, got: %s", string(out))
	}

	// Glob block IS in the reverseMap, must be restored to `glob`.
	globLine := []byte(`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_02","name":"Glob","input":{}}}`)
	out = reverseRemapOAuthToolNamesFromStreamLine(globLine, reverseMap)
	if !bytes.Contains(out, []byte(`"name":"glob"`)) {
		t.Fatalf("Glob should be restored to glob, got: %s", string(out))
	}
}

func TestPrepareClaudeOAuthToolNamesForUpstream_MixedCaseWithPrefix(t *testing.T) {
	body := []byte(`{"tools":[` +
		`{"name":"Bash","input_schema":{"type":"object","properties":{"cmd":{"type":"string"}}}},` +
		`{"name":"glob","input_schema":{"type":"object","properties":{"filePattern":{"type":"string"}}}}` +
		`],"messages":[{"role":"assistant","content":[` +
		`{"type":"tool_use","id":"toolu_01","name":"Bash","input":{}},` +
		`{"type":"tool_use","id":"toolu_02","name":"glob","input":{}}` +
		`]}]}`)

	out, reverseMap := prepareClaudeOAuthToolNamesForUpstream(body, "proxy_", false)

	if got := gjson.GetBytes(out, "tools.0.name").String(); got != "proxy_Bash" {
		t.Fatalf("tools.0.name = %q, want %q", got, "proxy_Bash")
	}
	if got := gjson.GetBytes(out, "tools.1.name").String(); got != "proxy_Glob" {
		t.Fatalf("tools.1.name = %q, want %q", got, "proxy_Glob")
	}
	if got := gjson.GetBytes(out, "messages.0.content.0.name").String(); got != "proxy_Bash" {
		t.Fatalf("messages.0.content.0.name = %q, want %q", got, "proxy_Bash")
	}
	if got := gjson.GetBytes(out, "messages.0.content.1.name").String(); got != "proxy_Glob" {
		t.Fatalf("messages.0.content.1.name = %q, want %q", got, "proxy_Glob")
	}
	if len(reverseMap) != 1 || reverseMap["Glob"] != "glob" {
		t.Fatalf("reverseMap = %v, want {Glob:glob}", reverseMap)
	}
}

func TestRestoreClaudeOAuthToolNamesFromResponse_MixedCaseWithPrefix(t *testing.T) {
	reverseMap := map[string]string{"Glob": "glob"}
	resp := []byte(`{"content":[` +
		`{"type":"tool_use","id":"toolu_01","name":"proxy_Bash","input":{}},` +
		`{"type":"tool_use","id":"toolu_02","name":"proxy_Glob","input":{}}` +
		`]}`)

	out := restoreClaudeOAuthToolNamesFromResponse(resp, "proxy_", false, reverseMap)

	if got := gjson.GetBytes(out, "content.0.name").String(); got != "Bash" {
		t.Fatalf("content.0.name = %q, want %q", got, "Bash")
	}
	if got := gjson.GetBytes(out, "content.1.name").String(); got != "glob" {
		t.Fatalf("content.1.name = %q, want %q", got, "glob")
	}
}

func TestRestoreClaudeOAuthToolNamesFromStreamLine_MixedCaseWithPrefix(t *testing.T) {
	reverseMap := map[string]string{"Glob": "glob"}

	bashLine := []byte(`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_01","name":"proxy_Bash","input":{}}}`)
	out := restoreClaudeOAuthToolNamesFromStreamLine(bashLine, "proxy_", false, reverseMap)
	if !bytes.Contains(out, []byte(`"name":"Bash"`)) {
		t.Fatalf("Bash should be preserved, got: %s", string(out))
	}
	if bytes.Contains(out, []byte(`"name":"bash"`)) {
		t.Fatalf("Bash must not be lowercased, got: %s", string(out))
	}

	globLine := []byte(`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_02","name":"proxy_Glob","input":{}}}`)
	out = restoreClaudeOAuthToolNamesFromStreamLine(globLine, "proxy_", false, reverseMap)
	if !bytes.Contains(out, []byte(`"name":"glob"`)) {
		t.Fatalf("Glob should be restored to glob, got: %s", string(out))
	}
}

// betaSetFromHeader splits an Anthropic-Beta header into a presence set.
func betaSetFromHeader(header string) map[string]bool {
	set := make(map[string]bool)
	for _, b := range strings.Split(header, ",") {
		if name := strings.TrimSpace(b); name != "" {
			set[name] = true
		}
	}
	return set
}

// TestApplyClaudeHeaders_ForcesXAppCliRegardlessOfClient verifies A6.1: a
// client-supplied X-App value (e.g. "browser") never leaks to the upstream;
// x-app is always forced to "cli".
func TestApplyClaudeHeaders_ForcesXAppCliRegardlessOfClient(t *testing.T) {
	resetClaudeDeviceProfileCache()

	req := newClaudeHeaderTestRequest(t, http.Header{
		"X-App": []string{"foo"},
	})
	auth := &cliproxyauth.Auth{ProxyURL: "direct",
		Attributes: map[string]string{"api_key": "key-xapp"},
	}
	applyClaudeHeaders(req, auth, "key-xapp", false, nil, nil)

	if got := req.Header.Get("X-App"); got != "cli" {
		t.Fatalf("X-App = %q, want %q (client value must not leak)", got, "cli")
	}
}

// TestApplyClaudeHeaders_ManagedXAppStillWins confirms A6.1 does not break the
// intentional per-account managed X-App override path: header:X-App is applied
// after the forced default and remains authoritative.
func TestApplyClaudeHeaders_ManagedXAppStillWins(t *testing.T) {
	resetClaudeDeviceProfileCache()

	req := newClaudeHeaderTestRequest(t, http.Header{
		"X-App": []string{"browser"},
	})
	auth := &cliproxyauth.Auth{ProxyURL: "direct",
		Attributes: map[string]string{
			"api_key":      "key-xapp-managed",
			"header:X-App": "cli",
		},
	}
	applyClaudeHeaders(req, auth, "key-xapp-managed", false, nil, nil)

	if got := req.Header.Get("X-App"); got != "cli" {
		t.Fatalf("X-App = %q, want %q", got, "cli")
	}
}

// TestApplyClaudeHeaders_AnthropicBetaUnionsClientWithFloor verifies A6.2:
// baseBetas is unioned with the client's real anthropic-beta set (not replaced),
// so baseBetas-only floor entries survive, client-only entries are preserved,
// strong-fill entries are present, and the set only grows (never-down).
func TestApplyClaudeHeaders_AnthropicBetaUnionsClientWithFloor(t *testing.T) {
	resetClaudeDeviceProfileCache()

	// Client sends a narrow beta set that includes one client-only beta and
	// omits several floor betas.
	req := newClaudeHeaderTestRequest(t, http.Header{
		"Anthropic-Beta": []string{"claude-code-20250219,fine-grained-tool-streaming-2025-05-14"},
	})
	auth := &cliproxyauth.Auth{ProxyURL: "direct",
		Attributes: map[string]string{"api_key": "key-beta"},
	}
	applyClaudeHeaders(req, auth, "key-beta", false, nil, nil)

	got := betaSetFromHeader(req.Header.Get("Anthropic-Beta"))

	// Floor (baseBetas-only) entries must not be dropped by a narrow client set.
	for _, floorBeta := range []string{
		"claude-code-20250219",
		"interleaved-thinking-2025-05-14",
		"thinking-token-count-2026-05-13",
		"context-management-2025-06-27",
		"prompt-caching-scope-2026-01-05",
		"mid-conversation-system-2026-04-07",
	} {
		if !got[floorBeta] {
			t.Fatalf("Anthropic-Beta missing floor beta %q; header=%q", floorBeta, req.Header.Get("Anthropic-Beta"))
		}
	}
	// Client-only beta is preserved.
	if !got["fine-grained-tool-streaming-2025-05-14"] {
		t.Fatalf("Anthropic-Beta dropped client-only beta; header=%q", req.Header.Get("Anthropic-Beta"))
	}
	// Strong-fill oauth beta present.
	if !got["oauth-2025-04-20"] {
		t.Fatalf("Anthropic-Beta missing strong-fill oauth-2025-04-20; header=%q", req.Header.Get("Anthropic-Beta"))
	}
	// We must NOT inject betas real claude-cli never sends.
	for _, forbidden := range []string{
		"structured-outputs-2025-12-15",
		"fast-mode-2026-02-01",
		"redact-thinking-2026-02-12",
		"token-efficient-tools-2026-03-28",
		"context-1m-2025-08-07",
	} {
		if got[forbidden] {
			t.Fatalf("Anthropic-Beta should not contain %q; header=%q", forbidden, req.Header.Get("Anthropic-Beta"))
		}
	}
}

// TestApplyClaudeHeaders_AnthropicBetaFloorWithoutClient verifies A6.2 keeps the
// full real-claude-cli-aligned floor when the client sends no anthropic-beta.
func TestApplyClaudeHeaders_AnthropicBetaFloorWithoutClient(t *testing.T) {
	resetClaudeDeviceProfileCache()

	req := newClaudeHeaderTestRequest(t, http.Header{})
	auth := &cliproxyauth.Auth{ProxyURL: "direct",
		Attributes: map[string]string{"api_key": "key-beta-floor"},
	}
	applyClaudeHeaders(req, auth, "key-beta-floor", false, nil, nil)

	got := betaSetFromHeader(req.Header.Get("Anthropic-Beta"))
	for _, floorBeta := range []string{
		"claude-code-20250219",
		"oauth-2025-04-20",
		"interleaved-thinking-2025-05-14",
		"thinking-token-count-2026-05-13",
		"context-management-2025-06-27",
		"prompt-caching-scope-2026-01-05",
		"mid-conversation-system-2026-04-07",
	} {
		if !got[floorBeta] {
			t.Fatalf("Anthropic-Beta missing floor beta %q; header=%q", floorBeta, req.Header.Get("Anthropic-Beta"))
		}
	}
}

// TestClaudeDeviceProfileStaleGuardActive_DetectsStaleProneConfig verifies the
// high-water model's only remaining stale-prone state: stabilize on, no operator
// baseline UA, and no real first-party claude-cli observed on any account yet.
// Any of: stabilize off, a configured baseline UA, or a real global observation,
// disarms the guard. online-update is irrelevant under plan A (npm is no longer a
// ceiling), so toggling it must not change the guard.
func TestClaudeDeviceProfileStaleGuardActive_DetectsStaleProneConfig(t *testing.T) {
	resetClaudeDeviceProfileCache()
	t.Cleanup(resetClaudeDeviceProfileCache)

	stabilize := true
	online := true
	offline := false

	staleCfg := &config.Config{
		ClaudeHeaderDefaults: config.ClaudeHeaderDefaults{
			StabilizeDeviceProfile: &stabilize,
		},
		ManagedHeaderProfile: config.ManagedHeaderProfileConfig{OnlineUpdate: &offline},
	}
	if !helps.ClaudeDeviceProfileStaleGuardActive(staleCfg) {
		t.Fatalf("expected guard active for stabilize+no-baseline+no-observation")
	}

	// online-update is no longer part of the predicate under plan A; toggling it
	// must not change the guard while there is still no real observation.
	onlineCfg := &config.Config{
		ClaudeHeaderDefaults: config.ClaudeHeaderDefaults{StabilizeDeviceProfile: &stabilize},
		ManagedHeaderProfile: config.ManagedHeaderProfileConfig{OnlineUpdate: &online},
	}
	if !helps.ClaudeDeviceProfileStaleGuardActive(onlineCfg) {
		t.Fatalf("guard must stay active regardless of online-update when no real client observed")
	}

	// Configured baseline UA is an explicit authoritative floor; guard off.
	baselineCfg := &config.Config{
		ClaudeHeaderDefaults: config.ClaudeHeaderDefaults{
			StabilizeDeviceProfile: &stabilize,
			UserAgent:              "claude-cli/2.1.158 (external, cli)",
		},
		ManagedHeaderProfile: config.ManagedHeaderProfileConfig{OnlineUpdate: &offline},
	}
	if helps.ClaudeDeviceProfileStaleGuardActive(baselineCfg) {
		t.Fatalf("guard must be off when an operator baseline UA is configured")
	}

	// Stabilize off disarms the guard.
	if helps.ClaudeDeviceProfileStaleGuardActive(&config.Config{}) {
		t.Fatalf("guard must be off when stabilize is disabled")
	}

	// A real first-party observation anywhere provides a non-stale fallback
	// ceiling and disarms the guard.
	_ = helps.ResolveClaudeDeviceProfile(&cliproxyauth.Auth{ProxyURL: "direct", ID: "stale-guard-seed", Provider: "claude"}, "", map[string][]string{
		"User-Agent": {"claude-cli/2.1.158 (external, cli)"},
	}, &config.Config{})
	if helps.ClaudeDeviceProfileStaleGuardActive(staleCfg) {
		t.Fatalf("guard must be off once a real first-party client has been observed")
	}
}

// TestApplyClaudeHeaders_StaleGuardOffPreservesObservedNewerClient confirms A6.3
// does not overwrite a newer real first-party client value with the stale frozen
// baseline when online-update is off and the cache is empty: the observed newer
// value wins (only-up, never the stale floor).
func TestApplyClaudeHeaders_StaleGuardOffPreservesObservedNewerClient(t *testing.T) {
	resetClaudeDeviceProfileCache()
	stabilize := true
	offline := false

	cfg := &config.Config{
		ClaudeHeaderDefaults: config.ClaudeHeaderDefaults{
			OS:                     "MacOS",
			Arch:                   "arm64",
			StabilizeDeviceProfile: &stabilize,
		},
		ManagedHeaderProfile: config.ManagedHeaderProfileConfig{OnlineUpdate: &offline},
	}
	auth := &cliproxyauth.Auth{ProxyURL: "direct",
		ID:         "auth-stale-guard",
		Attributes: map[string]string{"api_key": "key-stale-guard"},
	}

	// Real client far newer than the frozen built-in baseline (2.1.63).
	req := newClaudeHeaderTestRequest(t, http.Header{
		"User-Agent":                  []string{"claude-cli/2.1.158 (external, cli)"},
		"X-Stainless-Package-Version": []string{"0.94.0"},
		"X-Stainless-Runtime-Version": []string{"v24.3.0"},
		"X-Stainless-Os":              []string{"MacOS"},
		"X-Stainless-Arch":            []string{"arm64"},
	})
	applyClaudeHeaders(req, auth, "key-stale-guard", false, nil, cfg)
	assertClaudeFingerprint(t, req.Header, "claude-cli/2.1.158 (external, cli)", "0.94.0", "v24.3.0", "MacOS", "arm64")
}
