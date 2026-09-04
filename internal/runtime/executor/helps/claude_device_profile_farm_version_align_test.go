package helps

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

// 反关联 A1：农场号 serving 出站版本自动对齐容器入站真实版本。
//
// 现状（改前）：农场号 serving 版本 = max(硬编码 floor, 跨账号全局观测高水位, 持久化
// 高水位)，容器入站真实版本只有严格高于高水位才被采纳、否则被 floor-up 丢弃 → serving
// 版本可高于容器遥测版本（同一 device_id 两个版本 = 反关联信号），且全局高水位跨账号、
// 一个号顶高污染所有农场号。
//
// 改后：农场号（farmBound）直出容器入站真实版本、不 floor-up；农场观测不进跨账号全局
// 高水位池。普通号（共享上游账号）保持 unify-to-high-water 行为不变。

// claudeInboundHeaders builds a full inbound claude-cli device triple (UA + pkg +
// runtime + platform), the shape extractClaudeDeviceProfile parses into a candidate.
// The inbound platform is intentionally MacOS/arm64: a farm container's outbound
// platform is always pinned to Linux regardless, so these version tests keep the
// inbound platform distinct to also confirm the pin is not smuggled from inbound.
func claudeInboundHeaders(version string) http.Header {
	h := http.Header{}
	h.Set("User-Agent", "claude-cli/"+version+" (external, cli)")
	h.Set("X-Stainless-Package-Version", "0.94.0")
	h.Set("X-Stainless-Runtime-Version", "v26.3.0")
	h.Set("X-Stainless-Os", "MacOS")
	h.Set("X-Stainless-Arch", "arm64")
	return h
}

// nonFarmClaudeAuth builds a normal (non farm-bound) claude account: no container
// device_id attribute, so auth.ClaudeDeviceIDSource reports farmBound=false.
func nonFarmClaudeAuth(id string) *cliproxyauth.Auth {
	return &cliproxyauth.Auth{ID: id, Provider: "claude", ProxyURL: "direct"}
}

// farmFloorVersion returns the farm-bound fallback version (no candidate, no
// observation → baseline floor), derived dynamically so the assertions do not
// hardcode the floor constant.
func farmFloorVersion(t *testing.T, cfg *config.Config) string {
	t.Helper()
	return ResolveClaudeDeviceProfile(farmBoundClaudeAuth("farm-floor-probe"), "", nil, cfg).VersionString()
}

// (a) A farm candidate BELOW the cross-account global high-water egresses the
// candidate verbatim, NOT the global high-water. Under the pre-A1 behavior the farm
// baseline folded in the global observed high-water (2.1.300 here) and floored the
// 2.1.230 candidate up to it; A1 excludes global from the farm baseline so serving
// tracks the container's real version.
func TestFarmVersionAlign_EmitsCandidateBelowGlobalHighWater(t *testing.T) {
	ResetClaudeDeviceProfileCache()
	t.Cleanup(ResetClaudeDeviceProfileCache)
	cfg := &config.Config{}

	// A different NON-farm account observes 2.1.300 → sets the cross-account global.
	ResolveClaudeDeviceProfile(nonFarmClaudeAuth("normal-sets-global"), "", claudeInboundHeaders("2.1.300"), cfg)

	// Farm account presents a real container version well below that global.
	got := ResolveClaudeDeviceProfile(farmBoundClaudeAuth("farm-below-global"), "", claudeInboundHeaders("2.1.230"), cfg).VersionString()
	if got != "2.1.230" {
		t.Fatalf("farm serving version = %q, want 2.1.230 (container real version, NOT floored up to global 2.1.300)", got)
	}
}

// (a2) A farm candidate BELOW the hardcoded floor still egresses verbatim (older
// frozen container). The pre-A1 floor-up gate would discard a below-floor candidate
// and emit the floor; A1 keeps it so serving equals the container's real version.
func TestFarmVersionAlign_EmitsCandidateBelowFloor(t *testing.T) {
	ResetClaudeDeviceProfileCache()
	t.Cleanup(ResetClaudeDeviceProfileCache)
	cfg := &config.Config{}

	floor := farmFloorVersion(t, cfg)
	if floor != "2.1.211" {
		t.Logf("note: farm floor is %q (test picks a below-floor candidate accordingly)", floor)
	}

	// 2.1.205 is below the 2.1.211 floor.
	got := ResolveClaudeDeviceProfile(farmBoundClaudeAuth("farm-below-floor"), "", claudeInboundHeaders("2.1.205"), cfg).VersionString()
	if got != "2.1.205" {
		t.Fatalf("farm serving version = %q, want 2.1.205 (below-floor container version emitted verbatim, NOT floored to %q)", got, floor)
	}
}

// (b) Regression lock: a NORMAL (non-farm) account still floors up to the global
// high-water. A1 must not change shared-account unify-to-high-water behavior.
func TestFarmVersionAlign_NonFarmStillFloorsUpToGlobal(t *testing.T) {
	ResetClaudeDeviceProfileCache()
	t.Cleanup(ResetClaudeDeviceProfileCache)
	cfg := &config.Config{}

	// Account X observes 2.1.300 → global high-water 2.1.300.
	ResolveClaudeDeviceProfile(nonFarmClaudeAuth("normal-x"), "", claudeInboundHeaders("2.1.300"), cfg)

	// Account Y (zero own observations) presents 2.1.230 → must be floored up to 2.1.300.
	got := ResolveClaudeDeviceProfile(nonFarmClaudeAuth("normal-y"), "", claudeInboundHeaders("2.1.230"), cfg).VersionString()
	if got != "2.1.300" {
		t.Fatalf("non-farm serving version = %q, want 2.1.300 (unify-to-high-water unchanged for shared accounts)", got)
	}
}

// (c) Checkpoint 1: the sanity-ceiling gate still protects farm accounts. A forged
// super-high inbound UA (major far above the ceiling) is dropped BEFORE emission, so
// the farm account falls back to its floor rather than egressing the fabricated
// version.
func TestFarmVersionAlign_SanityCeilingStillBlocksForged(t *testing.T) {
	ResetClaudeDeviceProfileCache()
	t.Cleanup(ResetClaudeDeviceProfileCache)
	cfg := &config.Config{}

	floor := farmFloorVersion(t, cfg)
	got := ResolveClaudeDeviceProfile(farmBoundClaudeAuth("farm-forged"), "", claudeInboundHeaders("99.0.0"), cfg).VersionString()
	if got == "99.0.0" {
		t.Fatalf("farm serving version = %q, forged super-high UA must NOT be egressed (sanity ceiling breached)", got)
	}
	if got != floor {
		t.Fatalf("farm serving version = %q, want farm floor %q after forged candidate dropped", got, floor)
	}
}

// (d) Checkpoint 3: farm cold-start fallback uses the account's OWN last observation
// (cache), never the cross-account global. A farm account first emits 2.1.240, then a
// request with no parseable candidate must fall back to 2.1.240 — not the global
// 2.1.300 set by another account.
func TestFarmVersionAlign_ColdStartFallsBackToOwnNotGlobal(t *testing.T) {
	ResetClaudeDeviceProfileCache()
	t.Cleanup(ResetClaudeDeviceProfileCache)
	cfg := &config.Config{}

	// Another account sets a higher global.
	ResolveClaudeDeviceProfile(nonFarmClaudeAuth("normal-high-global"), "", claudeInboundHeaders("2.1.300"), cfg)

	farm := farmBoundClaudeAuth("farm-coldstart")
	// Farm first observes its real container version 2.1.240 (cached).
	if v := ResolveClaudeDeviceProfile(farm, "", claudeInboundHeaders("2.1.240"), cfg).VersionString(); v != "2.1.240" {
		t.Fatalf("farm first observation = %q, want 2.1.240", v)
	}
	// A later request with no parseable inbound UA falls back to the account's own
	// last profile, NOT the global 2.1.300.
	got := ResolveClaudeDeviceProfile(farm, "", nil, cfg).VersionString()
	if got != "2.1.240" {
		t.Fatalf("farm cold-start fallback = %q, want own 2.1.240 (must NOT jump to global 2.1.300)", got)
	}
}

// (e) The outbound UA header and the resolved version are the same source, so the
// body cc_version (derived from VersionString elsewhere) can never disagree with the
// UA. Assert the resolved UA string carries exactly the resolved version.
func TestFarmVersionAlign_UAAndVersionShareSameResolvedVersion(t *testing.T) {
	ResetClaudeDeviceProfileCache()
	t.Cleanup(ResetClaudeDeviceProfileCache)
	cfg := &config.Config{}

	profile := ResolveClaudeDeviceProfile(farmBoundClaudeAuth("farm-ua-version"), "", claudeInboundHeaders("2.1.230"), cfg)
	if profile.VersionString() != "2.1.230" {
		t.Fatalf("resolved version = %q, want 2.1.230", profile.VersionString())
	}
	if !strings.Contains(profile.UserAgent, "2.1.230") {
		t.Fatalf("resolved UserAgent = %q, want it to carry the same 2.1.230 version (UA and cc_version must agree)", profile.UserAgent)
	}
	// Applied wire header carries the same version.
	r := httptest.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", nil)
	ApplyClaudeDeviceProfileHeaders(r, profile)
	if ua := r.Header.Get("User-Agent"); !strings.Contains(ua, "2.1.230") {
		t.Fatalf("applied User-Agent = %q, want it to carry 2.1.230", ua)
	}
}

// (f) Checkpoint 4: a farm account's observation must NOT pollute the cross-account
// global high-water. After a farm account observes 2.1.240, a fresh NON-farm account
// with zero own observations must fall back to its floor — proving the farm version
// never entered the global pool consumed by normal accounts.
func TestFarmVersionAlign_DoesNotPolluteGlobalHighWater(t *testing.T) {
	ResetClaudeDeviceProfileCache()
	t.Cleanup(ResetClaudeDeviceProfileCache)
	cfg := &config.Config{}

	// Farm account observes 2.1.240 (marked as farm, excluded from global).
	ResolveClaudeDeviceProfile(farmBoundClaudeAuth("farm-only-observer"), "", claudeInboundHeaders("2.1.240"), cfg)

	// A fresh non-farm account with no observations and no candidate: its baseline is
	// max(floor, own(none), global). If the farm 2.1.240 leaked into global, this would
	// be 2.1.240; it must instead be the non-farm floor.
	nonFarmFloor := ResolveClaudeDeviceProfile(nonFarmClaudeAuth("normal-zero-obs-1"), "", nil, cfg).VersionString()
	got := ResolveClaudeDeviceProfile(nonFarmClaudeAuth("normal-zero-obs-2"), "", nil, cfg).VersionString()
	if got == "2.1.240" {
		t.Fatalf("non-farm zero-obs version = %q, farm observation leaked into the global high-water", got)
	}
	if got != nonFarmFloor {
		t.Fatalf("non-farm zero-obs version = %q, want floor %q (farm must not raise global)", got, nonFarmFloor)
	}
}
