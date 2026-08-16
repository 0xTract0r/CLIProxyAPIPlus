package helps

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

// validFarmClaudeDeviceID is a well-formed 64-hex claude_device_id override,
// standing in for a real container provisioning binding — the exact marker the
// supply-atomicity gate and auth.ClaudeDeviceIDSource treat as farm-bound.
var validFarmClaudeDeviceID = strings.Repeat("a", 64)

// farmBoundClaudeAuth builds a Claude auth whose attributes carry a valid
// container device_id override, so auth.ClaudeDeviceIDSource reports farmBound=true.
func farmBoundClaudeAuth(id string) *cliproxyauth.Auth {
	return &cliproxyauth.Auth{
		ID:       id,
		Provider: "claude",
		ProxyURL: "direct",
		Attributes: map[string]string{
			cliproxyauth.ClaudeDeviceIDAttributeKey: validFarmClaudeDeviceID,
		},
	}
}

// TestResolveClaudeDeviceProfile_FarmBoundEgressesLinux pins TR3: a farm-bound
// account (real container device_id) egresses the container's Linux platform, so
// the serving header agrees with the container's direct-telemetry OS under one
// device_id. Default farm arch is x64 (pending TR6 on-wire confirmation).
func TestResolveClaudeDeviceProfile_FarmBoundEgressesLinux(t *testing.T) {
	ResetClaudeDeviceProfileCache()
	t.Cleanup(ResetClaudeDeviceProfileCache)

	cfg := &config.Config{}
	profile := ResolveClaudeDeviceProfile(farmBoundClaudeAuth("claude-farm-linux"), "", nil, cfg)

	if profile.OS != "Linux" {
		t.Fatalf("farm-bound OS = %q, want Linux", profile.OS)
	}
	if profile.Arch != "x64" {
		t.Fatalf("farm-bound Arch = %q, want default x64", profile.Arch)
	}

	// The applied wire headers must carry the farm platform.
	r := httptest.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", nil)
	ApplyClaudeDeviceProfileHeaders(r, profile)
	if got := r.Header.Get("X-Stainless-Os"); got != "Linux" {
		t.Fatalf("applied X-Stainless-Os = %q, want Linux", got)
	}
	if got := r.Header.Get("X-Stainless-Arch"); got != "x64" {
		t.Fatalf("applied X-Stainless-Arch = %q, want x64", got)
	}
}

// TestResolveClaudeDeviceProfile_NonFarmKeepsMacOS pins the other half of TR3: a
// normal Claude account (no container binding, only the synthetic device_id) is
// NOT farm-bound and keeps the MacOS/arm64 baseline unchanged.
func TestResolveClaudeDeviceProfile_NonFarmKeepsMacOS(t *testing.T) {
	ResetClaudeDeviceProfileCache()
	t.Cleanup(ResetClaudeDeviceProfileCache)

	cfg := &config.Config{}
	auth := &cliproxyauth.Auth{ID: "claude-normal", Provider: "claude", ProxyURL: "direct"}
	profile := ResolveClaudeDeviceProfile(auth, "", nil, cfg)

	if profile.OS != "MacOS" {
		t.Fatalf("non-farm OS = %q, want MacOS", profile.OS)
	}
	if profile.Arch != "arm64" {
		t.Fatalf("non-farm Arch = %q, want arm64", profile.Arch)
	}
}

// TestResolveClaudeDeviceProfile_FarmArchIsConfigurable proves the farm arch is
// NOT hardcoded: an operator override (claude-header-defaults.farm-arch) wins over
// the x64 default. This is the escape hatch for TR6 once the real container arch is
// confirmed. The farm OS override is exercised at the same time.
func TestResolveClaudeDeviceProfile_FarmArchIsConfigurable(t *testing.T) {
	ResetClaudeDeviceProfileCache()
	t.Cleanup(ResetClaudeDeviceProfileCache)

	cfg := &config.Config{
		ClaudeHeaderDefaults: config.ClaudeHeaderDefaults{
			FarmOS:   "Linux",
			FarmArch: "arm64",
		},
	}
	profile := ResolveClaudeDeviceProfile(farmBoundClaudeAuth("claude-farm-arm"), "", nil, cfg)

	if profile.OS != "Linux" {
		t.Fatalf("configured farm OS = %q, want Linux", profile.OS)
	}
	if profile.Arch != "arm64" {
		t.Fatalf("configured farm Arch = %q, want arm64 (must not be hardcoded x64)", profile.Arch)
	}
}

// TestResolveClaudeDeviceProfile_NonClaudeProviderUnaffected pins the scope guard:
// the farm platform split is Claude-only. A non-Claude provider auth is never
// farm-bound (even if it carries a claude_device_id attribute), so its baseline
// platform is untouched.
func TestResolveClaudeDeviceProfile_NonClaudeProviderUnaffected(t *testing.T) {
	ResetClaudeDeviceProfileCache()
	t.Cleanup(ResetClaudeDeviceProfileCache)

	cfg := &config.Config{}
	auth := &cliproxyauth.Auth{
		ID:       "codex-acct",
		Provider: "codex",
		ProxyURL: "direct",
		Attributes: map[string]string{
			cliproxyauth.ClaudeDeviceIDAttributeKey: validFarmClaudeDeviceID,
		},
	}
	profile := ResolveClaudeDeviceProfile(auth, "", nil, cfg)

	if profile.OS != "MacOS" {
		t.Fatalf("non-claude provider OS = %q, want MacOS (farm split is claude-only)", profile.OS)
	}
	if profile.Arch != "arm64" {
		t.Fatalf("non-claude provider Arch = %q, want arm64", profile.Arch)
	}
}

// TestResolveClaudeDeviceProfile_FarmBoundVersionRaiseKeepsLinux guards the
// ordering invariant: the farm platform override runs before the version
// high-water raise (withClaudeFloorProfile), which replaces UA/pkg/runtime but
// preserves OS/Arch. So a farm account whose version is raised by a real observed
// high-water still egresses Linux, not a platform smuggled in from the observation.
func TestResolveClaudeDeviceProfile_FarmBoundVersionRaiseKeepsLinux(t *testing.T) {
	ResetClaudeDeviceProfileCache()
	t.Cleanup(ResetClaudeDeviceProfileCache)

	cfg := &config.Config{}
	auth := farmBoundClaudeAuth("claude-farm-highwater")

	// A higher real observation (reported on MacOS inbound) ratchets the version
	// high-water up; the outbound platform must still be pinned to farm Linux.
	headers := http.Header{}
	headers.Set("User-Agent", "claude-cli/2.1.240 (external, cli)")
	headers.Set("X-Stainless-Package-Version", "0.94.0")
	headers.Set("X-Stainless-Runtime-Version", "v26.3.0")
	headers.Set("X-Stainless-Os", "MacOS")
	headers.Set("X-Stainless-Arch", "arm64")

	profile := ResolveClaudeDeviceProfile(auth, "", headers, cfg)

	if profile.OS != "Linux" {
		t.Fatalf("farm-bound (version-raised) OS = %q, want Linux", profile.OS)
	}
	if profile.Arch != "x64" {
		t.Fatalf("farm-bound (version-raised) Arch = %q, want x64", profile.Arch)
	}
	if got := profile.VersionString(); got != "2.1.240" {
		t.Fatalf("version high-water = %q, want 2.1.240 (version raise must still apply)", got)
	}
}

// TestApplyClaudeLegacyDeviceHeaders_FarmAgnostic pins "stabilize off 行为不变":
// the legacy emitter used when stabilize-device-profile is disabled takes no auth,
// so the TR3 farm platform split can never reach it. It always emits the
// runtime-derived platform, identical to pre-TR3 behavior. Asserting against the
// same runtime mappers (not a hardcoded OS) keeps this deterministic regardless of
// the CI host GOOS/GOARCH.
func TestApplyClaudeLegacyDeviceHeaders_FarmAgnostic(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", nil)
	ApplyClaudeLegacyDeviceHeaders(r, nil, &config.Config{})

	if got, want := r.Header.Get("X-Stainless-Os"), MapStainlessOS(); got != want {
		t.Fatalf("legacy X-Stainless-Os = %q, want runtime-derived %q (must ignore farm binding)", got, want)
	}
	if got, want := r.Header.Get("X-Stainless-Arch"), MapStainlessArch(); got != want {
		t.Fatalf("legacy X-Stainless-Arch = %q, want runtime-derived %q", got, want)
	}
}
