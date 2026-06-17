package helps

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

// 反关联修复 B（R5）核心不变式：把 floor 抬到某个真实观测的高水位版本时，
// 三元组（UA/version、X-Stainless-Package-Version、X-Stainless-Runtime-Version）
// 必须整体取自同一次真实观测，绝不出现"新 UA + 旧常量 pkg/runtime"。
//
// 场景：账号 A 观测到一份完整真实三元组（UA=2.1.100 + pkg=0.95.0 + runtime=v25.0.0，
// 三者都不同于 baseline 默认常量 0.74.0 / v24.3.0）；零观测的账号 B 走 fallback floor
// 被抬升到该全局高水位。修复前 B 会得到 UA=2.1.100 但 pkg/runtime 仍是旧常量；
// 修复后 B 必须整组拿到 0.95.0 / v25.0.0。
func TestResolveClaudeDeviceProfile_FloorLiftsVersionTripleAtomically(t *testing.T) {
	ResetClaudeDeviceProfileCache()
	t.Cleanup(ResetClaudeDeviceProfileCache)

	cfg := &config.Config{}

	const (
		highUA      = "claude-cli/2.1.100 (external, cli)"
		highPkg     = "0.95.0"
		highRuntime = "v25.0.0"
	)

	// 账号 A：观测一份完整的真实三元组（pkg/runtime 都带、且都不同于 baseline 默认）。
	observed := ResolveClaudeDeviceProfile(&cliproxyauth.Auth{ProxyURL: "direct",
		ID:       "claude-account-A",
		Provider: "claude",
	}, "", map[string][]string{
		"User-Agent":                  {highUA},
		"X-Stainless-Package-Version": {highPkg},
		"X-Stainless-Runtime-Version": {highRuntime},
		"X-Stainless-Os":              {"Linux"},
		"X-Stainless-Arch":            {"x64"},
	}, cfg)
	if observed.PackageVersion != highPkg || observed.RuntimeVersion != highRuntime {
		t.Fatalf("account A observed triple not captured: ua=%q pkg=%q runtime=%q",
			observed.UserAgent, observed.PackageVersion, observed.RuntimeVersion)
	}

	// 账号 B：零观测，走 fallback floor，被全局高水位抬升。
	fallback := ResolveClaudeDeviceProfile(&cliproxyauth.Auth{ProxyURL: "direct",
		ID:       "claude-account-B-zero-obs",
		Provider: "claude",
	}, "", nil, cfg)

	// UA/version 被抬到高水位。
	if got := fallback.UserAgent; got != highUA {
		t.Fatalf("fallback UserAgent = %q, want lifted high-water %q", got, highUA)
	}
	if got := fallback.VersionString(); got != "2.1.100" {
		t.Fatalf("fallback version = %q, want 2.1.100", got)
	}

	// 关键不变式：pkg/runtime 必须与抬高的 UA 来自同一次真实观测，
	// 绝不是 baseline 旧常量。
	if got := fallback.PackageVersion; got != highPkg {
		t.Fatalf("fallback PackageVersion = %q, want %q (atomic triple); old-constant leak = anti-correlation bug", got, highPkg)
	}
	if got := fallback.RuntimeVersion; got != highRuntime {
		t.Fatalf("fallback RuntimeVersion = %q, want %q (atomic triple); old-constant leak = anti-correlation bug", got, highRuntime)
	}
	// 显式回归断言：决不能出现 baseline 默认常量与高水位 UA 拼接。
	if fallback.PackageVersion == defaultClaudeFingerprintPackageVersion {
		t.Fatalf("fallback emitted high-water UA %q with stale default package version %q — forbidden mismatched triple",
			fallback.UserAgent, defaultClaudeFingerprintPackageVersion)
	}
	if fallback.RuntimeVersion == defaultClaudeFingerprintRuntimeVersion {
		t.Fatalf("fallback emitted high-water UA %q with stale default runtime version %q — forbidden mismatched triple",
			fallback.UserAgent, defaultClaudeFingerprintRuntimeVersion)
	}

	// 平台位（OS/Arch）仍 pin 到本代理 baseline，与软件指纹解耦（既有设计）。
	if fallback.OS != defaultClaudeFingerprintOS || fallback.Arch != defaultClaudeFingerprintArch {
		t.Fatalf("platform should stay pinned to baseline: os=%q arch=%q", fallback.OS, fallback.Arch)
	}
}

// 反关联修复 B（R5）边界：没有任何真实观测时，三元组整体停在内部自洽的 baseline
// （2.1.63 / 0.74.0 / v24.3.0），不得被任何来源拆成不一致组合。
func TestResolveClaudeDeviceProfile_ZeroObservationKeepsConsistentBaselineTriple(t *testing.T) {
	ResetClaudeDeviceProfileCache()
	t.Cleanup(ResetClaudeDeviceProfileCache)

	profile := ResolveClaudeDeviceProfile(&cliproxyauth.Auth{ProxyURL: "direct",
		ID:       "claude-account-zero",
		Provider: "claude",
	}, "", nil, &config.Config{})

	if got := profile.VersionString(); got != "2.1.63" {
		t.Fatalf("zero-observation version = %q, want baseline 2.1.63", got)
	}
	if got := profile.PackageVersion; got != defaultClaudeFingerprintPackageVersion {
		t.Fatalf("zero-observation PackageVersion = %q, want baseline %q", got, defaultClaudeFingerprintPackageVersion)
	}
	if got := profile.RuntimeVersion; got != defaultClaudeFingerprintRuntimeVersion {
		t.Fatalf("zero-observation RuntimeVersion = %q, want baseline %q", got, defaultClaudeFingerprintRuntimeVersion)
	}
}

// 反关联修复 B（R5）回归：当真实观测的高水位版本带有完整、与 UA 同源的 pkg/runtime
// 时，cached profile 在被 normalize/高水位抬升后也必须保持三元组同源。这里直接验证
// 同账号重复观测后输出仍是同一份完整三元组，不会退化成 UA 新、pkg/runtime 旧。
func TestResolveClaudeDeviceProfile_SameAccountKeepsObservedTripleConsistent(t *testing.T) {
	ResetClaudeDeviceProfileCache()
	t.Cleanup(ResetClaudeDeviceProfileCache)

	cfg := &config.Config{}
	auth := &cliproxyauth.Auth{ProxyURL: "direct", ID: "claude-account-consistent", Provider: "claude"}

	headers := map[string][]string{
		"User-Agent":                  {"claude-cli/2.1.120 (external, cli)"},
		"X-Stainless-Package-Version": {"0.92.0"},
		"X-Stainless-Runtime-Version": {"v24.9.0"},
		"X-Stainless-Os":              {"MacOS"},
		"X-Stainless-Arch":            {"arm64"},
	}

	first := ResolveClaudeDeviceProfile(auth, "", headers, cfg)
	second := ResolveClaudeDeviceProfile(auth, "", headers, cfg)

	for name, p := range map[string]ClaudeDeviceProfile{"first": first, "second": second} {
		if p.UserAgent != "claude-cli/2.1.120 (external, cli)" {
			t.Fatalf("%s UA = %q, want observed", name, p.UserAgent)
		}
		if p.PackageVersion != "0.92.0" {
			t.Fatalf("%s PackageVersion = %q, want observed 0.92.0", name, p.PackageVersion)
		}
		if p.RuntimeVersion != "v24.9.0" {
			t.Fatalf("%s RuntimeVersion = %q, want observed v24.9.0", name, p.RuntimeVersion)
		}
	}
}
