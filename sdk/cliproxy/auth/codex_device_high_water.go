package auth

import (
	"regexp"
	"strconv"
	"strings"
)

// CodexDeviceHighWaterMetadataKey is the auth.Metadata key under which the
// persisted codex client device-profile high-water mark is stored.
//
// The value is a fresh map[string]any (never an in-place mutation of a nested
// map) so that Auth.Clone — which shallow-copies Metadata top-level entries —
// never shares the nested high-water map between the live auth and a clone. See
// Manager.RaiseCodexDeviceHighWater for the write side.
//
// 与 claude 的 ClaudeDeviceHighWaterMetadataKey 对称：codex 侧此前只有「读种子」
// （codexClientProfileFromAuth 读 persisted headers/attributes + floor 0.140.0），
// 运行时观测到的更高 CLI 版本没有写回 auth，重启即丢、回落 floor。这里补上写回侧
// 的持久化 schema，让运行时观测的高水位版本能落盘、重启被读种子拿到，闭环。
const CodexDeviceHighWaterMetadataKey = "codex_device_high_water"

// codexHighWaterVersionPattern 从 codex UA（如 "codex_cli_rs/0.140.0 (...)"）里抽出
// product 后面的 "X.Y.Z..." 版本前缀。保持 auth 包自包含，不 import helps（helps 已
// 单向 import auth）。
var codexHighWaterVersionPattern = regexp.MustCompile(`/(\d+(?:\.\d+)*)`)

// codexHighWaterNumericPattern 抽取版本字符串里的连续数字段，用于把 "0.140.0" 解析成
// 可比较的整数序列。codex 版本有 Desktop（year.day.build）与 CLI（0.x.y）两个家族，
// 第一段不可线性比较；持久化只承载「已过 serving 侧家族 + ceiling gate 的合法 CLI 观测」，
// 因此这里只做同家族的逐段数值比较即可。
var codexHighWaterNumericPattern = regexp.MustCompile(`\d+`)

// CodexDeviceHighWater is the serializable, persisted form of a codex client
// device-profile high-water observation. It carries the outbound-relevant
// identity fields (UserAgent + Version + Originator) plus bookkeeping, all
// sourced from one real, family/ceiling-validated serving observation.
//
// 比 claude 的三元组简单：codex 出站版本只体现在 UA 里（无独立 pkg/runtime header），
// 因此持久化只需 UA + Version + Originator + 来源/时间戳即可重建读种子的版本 floor。
type CodexDeviceHighWater struct {
	UserAgent  string `json:"user_agent,omitempty"`
	Version    string `json:"version,omitempty"`
	Originator string `json:"originator,omitempty"`
	Source     string `json:"source,omitempty"`
	LastSeenAt string `json:"last_seen_at,omitempty"`
}

// parsedVersion 返回从 Version（其次 UserAgent 前缀）解析出的可比较版本序列，以及是否
// 解析成功。
func (h CodexDeviceHighWater) parsedVersion() (codexHighWaterVersion, bool) {
	if v, ok := parseCodexHighWaterVersionString(h.Version); ok {
		return v, true
	}
	return parseCodexHighWaterUserAgent(h.UserAgent)
}

// valid 报告 high-water 是否承载一个可用、内部自洽的版本：UA 或 Version 至少一项能解析
// 出版本。
func (h CodexDeviceHighWater) valid() bool {
	if strings.TrimSpace(h.UserAgent) == "" && strings.TrimSpace(h.Version) == "" {
		return false
	}
	_, ok := h.parsedVersion()
	return ok
}

// codexHighWaterVersion 是按段比较的版本序列（如 0.140.0 -> [0,140,0]）。
type codexHighWaterVersion struct {
	parts []int
	valid bool
}

// compare 逐段比较两个版本序列；缺位段按 0 处理。返回 1/0/-1。
func (v codexHighWaterVersion) compare(other codexHighWaterVersion) int {
	if !v.valid && !other.valid {
		return 0
	}
	if v.valid && !other.valid {
		return 1
	}
	if !v.valid && other.valid {
		return -1
	}
	limit := len(v.parts)
	if len(other.parts) > limit {
		limit = len(other.parts)
	}
	for idx := 0; idx < limit; idx++ {
		left := 0
		right := 0
		if idx < len(v.parts) {
			left = v.parts[idx]
		}
		if idx < len(other.parts) {
			right = other.parts[idx]
		}
		switch {
		case left > right:
			return 1
		case left < right:
			return -1
		}
	}
	return 0
}

func parseCodexHighWaterUserAgent(userAgent string) (codexHighWaterVersion, bool) {
	matches := codexHighWaterVersionPattern.FindStringSubmatch(strings.TrimSpace(userAgent))
	if len(matches) != 2 {
		return codexHighWaterVersion{}, false
	}
	return parseCodexHighWaterVersionString(matches[1])
}

func parseCodexHighWaterVersionString(version string) (codexHighWaterVersion, bool) {
	matches := codexHighWaterNumericPattern.FindAllString(strings.TrimSpace(version), -1)
	if len(matches) == 0 {
		return codexHighWaterVersion{}, false
	}
	parts := make([]int, 0, len(matches))
	for _, match := range matches {
		n, err := strconv.Atoi(match)
		if err != nil {
			return codexHighWaterVersion{}, false
		}
		parts = append(parts, n)
	}
	return codexHighWaterVersion{parts: parts, valid: true}, true
}

// CodexDeviceHighWaterFromMetadata reads the persisted high-water entry from an
// auth.Metadata map. It tolerates both the in-process map[string]any shape (just
// written this run) and the map[string]string / decoded-from-JSON shape that the
// token store round-trips back on restart. Returns false when no usable entry is
// present.
func CodexDeviceHighWaterFromMetadata(metadata map[string]any) (CodexDeviceHighWater, bool) {
	if len(metadata) == 0 {
		return CodexDeviceHighWater{}, false
	}
	raw, ok := metadata[CodexDeviceHighWaterMetadataKey]
	if !ok || raw == nil {
		return CodexDeviceHighWater{}, false
	}
	hw := CodexDeviceHighWater{}
	switch m := raw.(type) {
	case CodexDeviceHighWater:
		hw = m
	case map[string]any:
		hw = CodexDeviceHighWater{
			UserAgent:  codexStringFromAny(m["user_agent"]),
			Version:    codexStringFromAny(m["version"]),
			Originator: codexStringFromAny(m["originator"]),
			Source:     codexStringFromAny(m["source"]),
			LastSeenAt: codexStringFromAny(m["last_seen_at"]),
		}
	case map[string]string:
		hw = CodexDeviceHighWater{
			UserAgent:  m["user_agent"],
			Version:    m["version"],
			Originator: m["originator"],
			Source:     m["source"],
			LastSeenAt: m["last_seen_at"],
		}
	default:
		return CodexDeviceHighWater{}, false
	}
	if !hw.valid() {
		return CodexDeviceHighWater{}, false
	}
	return hw, true
}

func codexStringFromAny(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return strings.TrimSpace(s)
	}
	return ""
}

// codexDeviceHighWaterToMetadataMap builds the fresh, serializable map written
// into auth.Metadata. A brand-new map is always allocated so that, after the
// owning auth is cloned, the original and the clone never share this nested map.
func codexDeviceHighWaterToMetadataMap(hw CodexDeviceHighWater) map[string]any {
	out := make(map[string]any, 5)
	if v := strings.TrimSpace(hw.UserAgent); v != "" {
		out["user_agent"] = v
	}
	if v := strings.TrimSpace(hw.Version); v != "" {
		out["version"] = v
	}
	if v := strings.TrimSpace(hw.Originator); v != "" {
		out["originator"] = v
	}
	if v := strings.TrimSpace(hw.Source); v != "" {
		out["source"] = v
	}
	if v := strings.TrimSpace(hw.LastSeenAt); v != "" {
		out["last_seen_at"] = v
	}
	return out
}
