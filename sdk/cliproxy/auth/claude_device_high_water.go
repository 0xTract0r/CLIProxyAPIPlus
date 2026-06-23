package auth

import (
	"regexp"
	"strconv"
	"strings"
)

// ClaudeDeviceHighWaterMetadataKey is the auth.Metadata key under which the
// persisted claude client device-profile high-water mark is stored.
//
// The value is a fresh map[string]any (never an in-place mutation of a nested
// map) so that Auth.Clone — which shallow-copies Metadata top-level entries —
// never shares the nested high-water map between the live auth and a clone. See
// Manager.RaiseClaudeDeviceHighWater for the write side.
const ClaudeDeviceHighWaterMetadataKey = "claude_device_high_water"

// claudeHighWaterVersionPattern parses the "X.Y.Z" version out of a
// "claude-cli/X.Y.Z ..." User-Agent. It mirrors the parser in the helps package
// but is kept self-contained here so the auth package never imports helps
// (helps already imports this package, so the dependency must stay one-way).
var claudeHighWaterVersionPattern = regexp.MustCompile(`^claude-cli/(\d+)\.(\d+)\.(\d+)`)

// ClaudeDeviceHighWater is the serializable, persisted form of a claude client
// device-profile high-water observation. It carries the COMPLETE fingerprint
// triple (UserAgent + PackageVersion + RuntimeVersion) plus the platform pins
// and bookkeeping, all sourced from one real, sanity-ceiling-validated
// observation. The shape is intentionally a superset of the helps
// ClaudeDeviceProfileObservation fields that matter for floor reconstruction.
type ClaudeDeviceHighWater struct {
	UserAgent      string `json:"user_agent,omitempty"`
	Version        string `json:"version,omitempty"`
	PackageVersion string `json:"package_version,omitempty"`
	RuntimeVersion string `json:"runtime_version,omitempty"`
	OS             string `json:"os,omitempty"`
	Arch           string `json:"arch,omitempty"`
	Source         string `json:"source,omitempty"`
	LastSeenAt     string `json:"last_seen_at,omitempty"`
}

// parsedVersion returns the (major, minor, patch) parsed from Version (or, as a
// fallback, from the UserAgent prefix) and whether a version was found.
func (h ClaudeDeviceHighWater) parsedVersion() (claudeHighWaterVersion, bool) {
	if v, ok := parseClaudeHighWaterVersionString(h.Version); ok {
		return v, true
	}
	return parseClaudeHighWaterUserAgent(h.UserAgent)
}

// valid reports whether the high-water carries a usable, internally consistent
// triple: a UserAgent that parses to a version and matching pkg/runtime fields.
func (h ClaudeDeviceHighWater) valid() bool {
	if strings.TrimSpace(h.UserAgent) == "" {
		return false
	}
	_, ok := h.parsedVersion()
	return ok
}

type claudeHighWaterVersion struct {
	major int
	minor int
	patch int
}

func (v claudeHighWaterVersion) compare(other claudeHighWaterVersion) int {
	switch {
	case v.major != other.major:
		if v.major > other.major {
			return 1
		}
		return -1
	case v.minor != other.minor:
		if v.minor > other.minor {
			return 1
		}
		return -1
	case v.patch != other.patch:
		if v.patch > other.patch {
			return 1
		}
		return -1
	default:
		return 0
	}
}

func parseClaudeHighWaterUserAgent(userAgent string) (claudeHighWaterVersion, bool) {
	matches := claudeHighWaterVersionPattern.FindStringSubmatch(strings.TrimSpace(userAgent))
	if len(matches) != 4 {
		return claudeHighWaterVersion{}, false
	}
	return triadFromMatches(matches[1], matches[2], matches[3])
}

func parseClaudeHighWaterVersionString(version string) (claudeHighWaterVersion, bool) {
	parts := strings.SplitN(strings.TrimSpace(version), ".", 3)
	if len(parts) != 3 {
		return claudeHighWaterVersion{}, false
	}
	return triadFromMatches(parts[0], parts[1], parts[2])
}

func triadFromMatches(rawMajor, rawMinor, rawPatch string) (claudeHighWaterVersion, bool) {
	major, err := strconv.Atoi(strings.TrimSpace(rawMajor))
	if err != nil {
		return claudeHighWaterVersion{}, false
	}
	minor, err := strconv.Atoi(strings.TrimSpace(rawMinor))
	if err != nil {
		return claudeHighWaterVersion{}, false
	}
	patch, err := strconv.Atoi(strings.TrimSpace(rawPatch))
	if err != nil {
		return claudeHighWaterVersion{}, false
	}
	return claudeHighWaterVersion{major: major, minor: minor, patch: patch}, true
}

// ClaudeDeviceHighWaterFromMetadata reads the persisted high-water triple from
// an auth.Metadata map. It tolerates both the in-process map[string]any shape
// (just written this run) and the map[string]string / decoded-from-JSON shape
// that the token store round-trips back on restart. Returns false when no usable
// triple is present.
func ClaudeDeviceHighWaterFromMetadata(metadata map[string]any) (ClaudeDeviceHighWater, bool) {
	if len(metadata) == 0 {
		return ClaudeDeviceHighWater{}, false
	}
	raw, ok := metadata[ClaudeDeviceHighWaterMetadataKey]
	if !ok || raw == nil {
		return ClaudeDeviceHighWater{}, false
	}
	hw := ClaudeDeviceHighWater{}
	switch m := raw.(type) {
	case ClaudeDeviceHighWater:
		hw = m
	case map[string]any:
		hw = ClaudeDeviceHighWater{
			UserAgent:      stringFromAny(m["user_agent"]),
			Version:        stringFromAny(m["version"]),
			PackageVersion: stringFromAny(m["package_version"]),
			RuntimeVersion: stringFromAny(m["runtime_version"]),
			OS:             stringFromAny(m["os"]),
			Arch:           stringFromAny(m["arch"]),
			Source:         stringFromAny(m["source"]),
			LastSeenAt:     stringFromAny(m["last_seen_at"]),
		}
	case map[string]string:
		hw = ClaudeDeviceHighWater{
			UserAgent:      m["user_agent"],
			Version:        m["version"],
			PackageVersion: m["package_version"],
			RuntimeVersion: m["runtime_version"],
			OS:             m["os"],
			Arch:           m["arch"],
			Source:         m["source"],
			LastSeenAt:     m["last_seen_at"],
		}
	default:
		return ClaudeDeviceHighWater{}, false
	}
	if !hw.valid() {
		return ClaudeDeviceHighWater{}, false
	}
	return hw, true
}

func stringFromAny(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return strings.TrimSpace(s)
	}
	return ""
}

// claudeDeviceHighWaterToMetadataMap builds the fresh, serializable map written
// into auth.Metadata. A brand-new map is always allocated so that, after the
// owning auth is cloned, the original and the clone never share this nested map.
func claudeDeviceHighWaterToMetadataMap(hw ClaudeDeviceHighWater) map[string]any {
	out := make(map[string]any, 8)
	if v := strings.TrimSpace(hw.UserAgent); v != "" {
		out["user_agent"] = v
	}
	if v := strings.TrimSpace(hw.Version); v != "" {
		out["version"] = v
	}
	if v := strings.TrimSpace(hw.PackageVersion); v != "" {
		out["package_version"] = v
	}
	if v := strings.TrimSpace(hw.RuntimeVersion); v != "" {
		out["runtime_version"] = v
	}
	if v := strings.TrimSpace(hw.OS); v != "" {
		out["os"] = v
	}
	if v := strings.TrimSpace(hw.Arch); v != "" {
		out["arch"] = v
	}
	if v := strings.TrimSpace(hw.Source); v != "" {
		out["source"] = v
	}
	if v := strings.TrimSpace(hw.LastSeenAt); v != "" {
		out["last_seen_at"] = v
	}
	return out
}
