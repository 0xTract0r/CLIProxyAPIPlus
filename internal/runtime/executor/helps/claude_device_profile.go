package helps

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

const (
	defaultClaudeFingerprintUserAgent      = "claude-cli/2.1.63 (external, cli)"
	defaultClaudeFingerprintPackageVersion = "0.74.0"
	defaultClaudeFingerprintRuntimeVersion = "v24.3.0"
	defaultClaudeFingerprintOS             = "MacOS"
	defaultClaudeFingerprintArch           = "arm64"
	claudeDeviceProfileTTL                 = 7 * 24 * time.Hour
	claudeDeviceProfileCleanupPeriod       = time.Hour

	// claudeSanityCeilingMajor / Minor / Patch is the hardcoded, offline,
	// deterministic upper bound on any claude-cli version we are willing to treat
	// as a real first-party observation. It defends against the one threat the
	// high-water model otherwise leaves open: a holder of a valid downstream
	// account key sending a *fabricated* high version User-Agent (e.g.
	// "claude-cli/999.0.0"). Without a ceiling such a forged UA would pass the
	// existing >= static-floor gate, be recorded as a "real observation", and then
	// become the per-account and global high-water that gets applied to other
	// zero-observation accounts (cross-account pollution).
	//
	// The bound is intentionally generous relative to the current real version
	// family (2.1.x as of 2026-06) so it never false-rejects a near-future genuine
	// release — it tolerates a full major bump and a wide minor/patch range — while
	// still rejecting absurd values. Anything strictly greater than this version is
	// refused. When npm "latest" has already been fetched (online-update on or a
	// warm cache) the effective ceiling is raised to that real npm latest (see
	// claudeObservationSanityCeiling); npm is used here ONLY to validate an upper
	// bound, never to push the outbound version up (push stays capped to real
	// observation in claudeFallbackBaseline).
	//
	// When to bump: if claude-cli legitimately reaches a major version at or beyond
	// this constant, raise it (and keep it one major ahead of the live family) so
	// real clients are never rejected. This is a conservative offline backstop, not
	// a precise version pin.
	claudeSanityCeilingMajor = 4
	claudeSanityCeilingMinor = 0
	claudeSanityCeilingPatch = 0
)

var (
	claudeCLIVersionPattern = regexp.MustCompile(`^claude-cli/(\d+)\.(\d+)\.(\d+)`)

	claudeDeviceProfileCache            = make(map[string]claudeDeviceProfileCacheEntry)
	claudeDeviceProfileObservations     = make(map[string][]claudeDeviceProfileObservationEntry)
	claudeDeviceProfileCacheMu          sync.RWMutex
	claudeDeviceProfileCacheCleanupOnce sync.Once

	ClaudeDeviceProfileBeforeCandidateStore func(ClaudeDeviceProfile)
)

type claudeCLIVersion struct {
	major int
	minor int
	patch int
}

func (v claudeCLIVersion) Compare(other claudeCLIVersion) int {
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

type ClaudeDeviceProfile struct {
	UserAgent      string
	PackageVersion string
	RuntimeVersion string
	OS             string
	Arch           string
	Source         ManagedHeaderProfileSource
	version        claudeCLIVersion
	hasVersion     bool
}

type ClaudeDeviceProfileObservation struct {
	UserAgent      string                     `json:"user_agent,omitempty"`
	Version        string                     `json:"version,omitempty"`
	PackageVersion string                     `json:"package_version,omitempty"`
	RuntimeVersion string                     `json:"runtime_version,omitempty"`
	OS             string                     `json:"os,omitempty"`
	Arch           string                     `json:"arch,omitempty"`
	Source         ManagedHeaderProfileSource `json:"source,omitempty"`
	FirstSeenAt    string                     `json:"first_seen_at,omitempty"`
	LastSeenAt     string                     `json:"last_seen_at,omitempty"`
	RequestCount   int                        `json:"request_count,omitempty"`
}

type claudeDeviceProfileCacheEntry struct {
	profile ClaudeDeviceProfile
	expire  time.Time
}

type claudeDeviceProfileObservationEntry struct {
	profile   ClaudeDeviceProfile
	firstSeen time.Time
	lastSeen  time.Time
	count     int
}

func ClaudeDeviceProfileStabilizationEnabled(cfg *config.Config) bool {
	if cfg == nil || cfg.ClaudeHeaderDefaults.StabilizeDeviceProfile == nil {
		return false
	}
	return *cfg.ClaudeHeaderDefaults.StabilizeDeviceProfile
}

// ClaudeDeviceProfileStaleGuardActive reports whether the runtime is in the
// only remaining stale-prone state under the high-water model (requirement ⑥,
// plan A): stabilize is enabled, no operator baseline User-Agent is configured,
// AND no real first-party claude-cli version has been observed yet on ANY account
// (so the global observed high-water fallback is also empty). In that narrow
// window the only floor left is the hardcoded defaultClaudeFingerprintUserAgent
// constant, which can drift stale relative to live clients until the first real
// client request is seen. The guard self-heals: once any real first-party client
// is observed, the fallback ceiling becomes that real observed version.
//
// online-update is intentionally NOT part of this predicate. Under plan A npm is
// no longer a ceiling (it can fabricate a version no real client here has sent),
// so enabling it does not resolve the stale window and is not recommended as a
// remedy. An operator-configured baseline UA remains an explicit, authoritative
// floor and suppresses the guard.
func ClaudeDeviceProfileStaleGuardActive(cfg *config.Config) bool {
	if !ClaudeDeviceProfileStabilizationEnabled(cfg) {
		return false
	}
	// An operator-configured baseline UA is an explicit, authoritative floor;
	// it is not the stale hardcoded constant, so the guard does not apply.
	if cfg != nil && strings.TrimSpace(cfg.ClaudeHeaderDefaults.UserAgent) != "" {
		return false
	}
	// Any real first-party observation anywhere provides a non-stale fallback
	// ceiling, so the guard only applies before the first real client is seen.
	if _, hasGlobal := globalClaudeObservedHighWaterVersion(); hasGlobal {
		return false
	}
	return true
}

func ResetClaudeDeviceProfileCache() {
	claudeDeviceProfileCacheMu.Lock()
	claudeDeviceProfileCache = make(map[string]claudeDeviceProfileCacheEntry)
	claudeDeviceProfileObservations = make(map[string][]claudeDeviceProfileObservationEntry)
	claudeDeviceProfileCacheMu.Unlock()
}

func MapStainlessOS() string {
	return mapStainlessOS()
}

func MapStainlessArch() string {
	return mapStainlessArch()
}

func defaultClaudeDeviceProfile(cfg *config.Config) ClaudeDeviceProfile {
	hdrDefault := func(cfgVal, fallback string) string {
		if strings.TrimSpace(cfgVal) != "" {
			return strings.TrimSpace(cfgVal)
		}
		return fallback
	}

	var hd config.ClaudeHeaderDefaults
	if cfg != nil {
		hd = cfg.ClaudeHeaderDefaults
	}

	profile := ClaudeDeviceProfile{
		UserAgent:      hdrDefault(hd.UserAgent, defaultClaudeFingerprintUserAgent),
		PackageVersion: hdrDefault(hd.PackageVersion, defaultClaudeFingerprintPackageVersion),
		RuntimeVersion: hdrDefault(hd.RuntimeVersion, defaultClaudeFingerprintRuntimeVersion),
		OS:             hdrDefault(hd.OS, defaultClaudeFingerprintOS),
		Arch:           hdrDefault(hd.Arch, defaultClaudeFingerprintArch),
		Source:         defaultManagedHeaderProfileSource(),
	}
	if version, ok := parseClaudeCLIVersion(profile.UserAgent); ok {
		profile.version = version
		profile.hasVersion = true
	}
	// High-water model: the outbound version ceiling is the account's real
	// observed first-party claude-cli high-water mark, and we must never claim a
	// version higher than a real client actually presented. The online registry
	// (npm "latest") is therefore NOT a ceiling and is intentionally not injected
	// into the baseline floor here. npm latest can be ahead of every client this
	// deployment has ever seen; using it as a floor/ceiling would fabricate an
	// "old body + newer-than-real UA" mismatch on zero-/low-observation accounts.
	// The baseline returned here is only the absolute lower bound: the operator
	// configured claude-header-defaults.user-agent when set, otherwise the
	// hardcoded floor constant. 反关联修复 B（R5）后，npm online-update 不再用于单独
	// 抬升 outbound version——因为它只产出版本号、没有配套真实 pkg/runtime，抬 UA 会
	// 造出不存在的三元组；floor 只在有同一次真实观测的完整三元组时才整体抬升。
	return profile
}

// globalClaudeObservedHighWaterVersion returns the highest real first-party
// claude-cli version observed across ALL accounts. It is the zero-observation
// fallback ceiling: a version that some real client genuinely presented to this
// proxy (just not on this account), which is always safer than npm latest
// because it is guaranteed to be a version that actually exists in the wild.
func globalClaudeObservedHighWaterVersion() (claudeCLIVersion, bool) {
	claudeDeviceProfileCacheMu.RLock()
	defer claudeDeviceProfileCacheMu.RUnlock()
	var best claudeCLIVersion
	found := false
	for _, entries := range claudeDeviceProfileObservations {
		for _, entry := range entries {
			if !entry.profile.hasVersion {
				continue
			}
			if !found || entry.profile.version.Compare(best) > 0 {
				best = entry.profile.version
				found = true
			}
		}
	}
	return best, found
}

// 反关联修复 B（R5）：版本三元组（UA/version、X-Stainless-Package-Version、
// X-Stainless-Runtime-Version）必须始终取自同一真实来源。此前 high-water 只回传
// 一个 version 数字，withClaudeFloorVersion 据此合成 UA 却保留 baseline 的旧常量
// pkg/runtime（0.74.0 / v24.3.0），抬高 floor 时会发出"新 UA + 旧 pkg/runtime"这种
// 真实世界不存在的三元组。
//
// claudeObservedHighWaterProfile 返回最高版本那一次观测的**完整 profile**——即
// 同一次带完整 pkg/runtime 头的真实观测——使三元组可以作为原子单元一起抬升。
// 当多条观测同为最高版本时取最近 lastSeen（entries 已按 lastSeen 倒序排过），
// 保证取到一份内部自洽的真实三元组。
func claudeObservedHighWaterProfile(cacheKeys []string) (ClaudeDeviceProfile, bool) {
	claudeDeviceProfileCacheMu.RLock()
	defer claudeDeviceProfileCacheMu.RUnlock()
	var best ClaudeDeviceProfile
	found := false
	for _, cacheKey := range cacheKeys {
		for _, entry := range claudeDeviceProfileObservations[cacheKey] {
			if !entry.profile.hasVersion {
				continue
			}
			if !found || entry.profile.version.Compare(best.version) > 0 {
				best = entry.profile
				found = true
			}
		}
	}
	return best, found
}

// globalClaudeObservedHighWaterProfile 是 claudeObservedHighWaterProfile 的全局版本：
// 当某账号自身零观测时，回退到任一真实客户端在本代理上呈现过的最高版本**完整三元组**，
// 同样保证 UA/pkg/runtime 来自同一次真实观测，而不是把抬高的 UA 与旧常量拼接。
func globalClaudeObservedHighWaterProfile() (ClaudeDeviceProfile, bool) {
	claudeDeviceProfileCacheMu.RLock()
	defer claudeDeviceProfileCacheMu.RUnlock()
	var best ClaudeDeviceProfile
	found := false
	for _, entries := range claudeDeviceProfileObservations {
		for _, entry := range entries {
			if !entry.profile.hasVersion {
				continue
			}
			if !found || entry.profile.version.Compare(best.version) > 0 {
				best = entry.profile
				found = true
			}
		}
	}
	return best, found
}

// claudePersistedHighWaterProfile reconstructs a ClaudeDeviceProfile from the
// high-water triple persisted into auth.Metadata by
// Manager.RaiseClaudeDeviceHighWater. It is the read-seed half of claude version
// high-water persistence: after a restart the in-memory observation maps are
// empty, so claudeFallbackBaseline consults this persisted triple (and takes the
// max with any live observation) instead of falling back to the static floor.
//
// The returned profile carries the COMPLETE triple (UA/version + pkg + runtime)
// from the same real observation that was persisted, so it can be adopted as an
// atomic floor unit just like a live observation. A persisted entry whose
// UserAgent does not parse to a version is rejected (returns false).
func claudePersistedHighWaterProfile(auth *cliproxyauth.Auth) (ClaudeDeviceProfile, bool) {
	if auth == nil {
		return ClaudeDeviceProfile{}, false
	}
	hw, ok := cliproxyauth.ClaudeDeviceHighWaterFromMetadata(auth.Metadata)
	if !ok {
		return ClaudeDeviceProfile{}, false
	}
	version, ok := parseClaudeCLIVersion(hw.UserAgent)
	if !ok {
		return ClaudeDeviceProfile{}, false
	}
	profile := ClaudeDeviceProfile{
		UserAgent:      strings.TrimSpace(hw.UserAgent),
		PackageVersion: strings.TrimSpace(hw.PackageVersion),
		RuntimeVersion: strings.TrimSpace(hw.RuntimeVersion),
		OS:             strings.TrimSpace(hw.OS),
		Arch:           strings.TrimSpace(hw.Arch),
		Source:         observedManagedHeaderProfileSource(),
		version:        version,
		hasVersion:     true,
	}
	return profile, true
}

// SeedClaudeObservedHighWaterFromAuth re-seeds the in-memory observation map from
// an auth's persisted claude_device_high_water triple. It is the startup/auth-load
// half of high-water persistence on the *observation* side.
//
// Background: two code paths consume the persisted high-water but read different
// sources. The outbound floor path (claudeFallbackBaseline) already reads the
// persisted triple from auth.Metadata directly, so the outbound version is correct
// from the very first request after a restart. The operator-facing stale-guard
// warning predicate (ClaudeDeviceProfileStaleGuardActive ->
// globalClaudeObservedHighWaterVersion) instead consults ONLY the in-memory
// observation map, which is empty right after a restart. That mismatch makes the
// guard emit a "no real claude-cli observed, falling back to frozen floor 2.1.63"
// warning on the first request even though the real outbound UA is the (correct)
// persisted version — a misleading false positive that self-heals only after the
// first live observation lands.
//
// Seeding the persisted triple back into the in-memory observation map makes the
// warning predicate's view consistent with the disk/outbound view, eliminating the
// false-positive log without changing outbound timing. The persisted triple was
// already sanity-ceiling-validated when it was first observed (the write side runs
// the gate before RaiseClaudeDeviceHighWater), so re-seeding cannot fabricate a
// forged high version. recordClaudeDeviceProfileObservation is additive/dedup and
// globalClaudeObservedHighWaterVersion always takes the max, so this stays strictly
// only-up: re-seeding a value that is lower than a live observation does not lower
// the high-water. The triple is recorded under the shared "global" observation key
// so it acts as the zero-observation global fallback ceiling, exactly as a live
// cross-account observation would. Returns false when the auth carries no usable
// persisted triple.
func SeedClaudeObservedHighWaterFromAuth(auth *cliproxyauth.Auth) bool {
	profile, ok := claudePersistedHighWaterProfile(auth)
	if !ok {
		return false
	}
	sum := sha256.Sum256([]byte("global"))
	globalKey := hex.EncodeToString(sum[:])
	claudeDeviceProfileCacheMu.Lock()
	recordClaudeDeviceProfileObservation(globalKey, profile, time.Now())
	claudeDeviceProfileCacheMu.Unlock()
	return true
}

// ClaudeObservedHighWaterForAuth returns the account's current in-memory observed
// high-water mark as a serializable triple, suitable for passing to
// Manager.RaiseClaudeDeviceHighWater. It returns the per-account observed
// high-water when present, otherwise the global observed high-water.
//
// Because only candidates that already passed the sanity-ceiling gate are ever
// recorded into the observation maps (ResolveClaudeDeviceProfile drops forged
// high versions before recording), the returned triple is guaranteed to be a
// real, sanity-validated observation — a forged "claude-cli/999.0.0" can never
// surface here. Returns false when no observation exists yet.
func ClaudeObservedHighWaterForAuth(auth *cliproxyauth.Auth, apiKey string) (cliproxyauth.ClaudeDeviceHighWater, bool) {
	observationKeys := claudeDeviceProfileObservationCacheKeys(auth, apiKey)
	profile, ok := claudeObservedHighWaterProfile(observationKeys)
	if !ok {
		profile, ok = globalClaudeObservedHighWaterProfile()
	}
	if !ok || profile.UserAgent == "" || !profile.hasVersion {
		return cliproxyauth.ClaudeDeviceHighWater{}, false
	}
	return cliproxyauth.ClaudeDeviceHighWater{
		UserAgent:      profile.UserAgent,
		Version:        profile.VersionString(),
		PackageVersion: profile.PackageVersion,
		RuntimeVersion: profile.RuntimeVersion,
		OS:             profile.OS,
		Arch:           profile.Arch,
		Source:         profile.Source.Source,
		LastSeenAt:     time.Now().UTC().Format(time.RFC3339),
	}, true
}

// claudeStaticSanityCeiling returns the hardcoded, offline upper bound on any
// claude-cli version we will treat as a real first-party observation.
func claudeStaticSanityCeiling() claudeCLIVersion {
	return claudeCLIVersion{
		major: claudeSanityCeilingMajor,
		minor: claudeSanityCeilingMinor,
		patch: claudeSanityCeilingPatch,
	}
}

// claudeObservationSanityCeiling returns the effective upper bound used to reject
// fabricated high-version inbound User-Agents before they can pollute the
// per-account or global observed high-water mark.
//
// The ceiling is max(hardcoded static sanity ceiling, npm latest when already
// available). The static constant guarantees a deterministic offline bound even
// when online-update is disabled (the default). npm "latest" — only when it has
// already been fetched/cached — is consulted purely to RAISE the validation
// ceiling toward the real newest release, so a genuine bleeding-edge client is
// never false-rejected. npm is never used here to push the outbound version up;
// that remains capped to real observation elsewhere.
func claudeObservationSanityCeiling(cfg *config.Config) claudeCLIVersion {
	ceiling := claudeStaticSanityCeiling()
	if online, ok := resolveManagedHeaderOnlineVersion("claude", cfg); ok {
		candidateUA := "claude-cli/" + online.Version + " (external, cli)"
		if npmVersion, npmOK := parseClaudeCLIVersion(candidateUA); npmOK && npmVersion.Compare(ceiling) > 0 {
			ceiling = npmVersion
		}
	}
	return ceiling
}

// claudeObservationWithinSanityCeiling reports whether a candidate observation's
// version is at or below the effective sanity ceiling. A candidate that exceeds
// the ceiling is treated as fabricated and must not be adopted (neither recorded
// into the per-account/global high-water nor emitted as the outbound version).
func claudeObservationWithinSanityCeiling(candidate ClaudeDeviceProfile, cfg *config.Config) bool {
	if !candidate.hasVersion {
		return true
	}
	return candidate.version.Compare(claudeObservationSanityCeiling(cfg)) <= 0
}

// mapStainlessOS maps runtime.GOOS to Stainless SDK OS names.
func mapStainlessOS() string {
	switch runtime.GOOS {
	case "darwin":
		return "MacOS"
	case "windows":
		return "Windows"
	case "linux":
		return "Linux"
	case "freebsd":
		return "FreeBSD"
	default:
		return "Other::" + runtime.GOOS
	}
}

// mapStainlessArch maps runtime.GOARCH to Stainless SDK architecture names.
func mapStainlessArch() string {
	switch runtime.GOARCH {
	case "amd64":
		return "x64"
	case "arm64":
		return "arm64"
	case "386":
		return "x86"
	default:
		return "other::" + runtime.GOARCH
	}
}

func parseClaudeCLIVersion(userAgent string) (claudeCLIVersion, bool) {
	matches := claudeCLIVersionPattern.FindStringSubmatch(strings.TrimSpace(userAgent))
	if len(matches) != 4 {
		return claudeCLIVersion{}, false
	}
	major, err := strconv.Atoi(matches[1])
	if err != nil {
		return claudeCLIVersion{}, false
	}
	minor, err := strconv.Atoi(matches[2])
	if err != nil {
		return claudeCLIVersion{}, false
	}
	patch, err := strconv.Atoi(matches[3])
	if err != nil {
		return claudeCLIVersion{}, false
	}
	return claudeCLIVersion{major: major, minor: minor, patch: patch}, true
}

func formatClaudeCLIVersion(version claudeCLIVersion) string {
	return strconv.Itoa(version.major) + "." + strconv.Itoa(version.minor) + "." + strconv.Itoa(version.patch)
}

func ClaudeVersionFromUserAgent(userAgent string) (string, bool) {
	version, ok := parseClaudeCLIVersion(userAgent)
	if !ok {
		return "", false
	}
	return formatClaudeCLIVersion(version), true
}

func (profile ClaudeDeviceProfile) VersionString() string {
	if !profile.hasVersion {
		return ""
	}
	return formatClaudeCLIVersion(profile.version)
}

func shouldUpgradeClaudeDeviceProfile(candidate, current ClaudeDeviceProfile) bool {
	if candidate.UserAgent == "" || !candidate.hasVersion {
		return false
	}
	if current.UserAgent == "" || !current.hasVersion {
		return true
	}
	return candidate.version.Compare(current.version) > 0
}

func pinClaudeDeviceProfilePlatform(profile, baseline ClaudeDeviceProfile) ClaudeDeviceProfile {
	profile.OS = baseline.OS
	profile.Arch = baseline.Arch
	return profile
}

// normalizeClaudeDeviceProfile keeps stabilized profiles pinned to the current
// baseline platform and enforces the baseline software fingerprint as a floor.
func normalizeClaudeDeviceProfile(profile, baseline ClaudeDeviceProfile) ClaudeDeviceProfile {
	profile = pinClaudeDeviceProfilePlatform(profile, baseline)
	if profile.UserAgent == "" || !profile.hasVersion || shouldUpgradeClaudeDeviceProfile(baseline, profile) {
		profile.UserAgent = baseline.UserAgent
		profile.PackageVersion = baseline.PackageVersion
		profile.RuntimeVersion = baseline.RuntimeVersion
		profile.Source = baseline.Source
		profile.version = baseline.version
		profile.hasVersion = baseline.hasVersion
	}
	profile.Source = withManagedHeaderProfileSource(profile.Source, baseline.Source)
	return profile
}

func extractClaudeDeviceProfile(headers http.Header, cfg *config.Config) (ClaudeDeviceProfile, bool) {
	if headers == nil {
		return ClaudeDeviceProfile{}, false
	}

	userAgent := strings.TrimSpace(headers.Get("User-Agent"))
	version, ok := parseClaudeCLIVersion(userAgent)
	if !ok {
		return ClaudeDeviceProfile{}, false
	}

	baseline := defaultClaudeDeviceProfile(cfg)
	profile := ClaudeDeviceProfile{
		UserAgent:      userAgent,
		PackageVersion: firstNonEmptyHeader(headers, "X-Stainless-Package-Version", baseline.PackageVersion),
		RuntimeVersion: firstNonEmptyHeader(headers, "X-Stainless-Runtime-Version", baseline.RuntimeVersion),
		OS:             firstNonEmptyHeader(headers, "X-Stainless-Os", baseline.OS),
		Arch:           firstNonEmptyHeader(headers, "X-Stainless-Arch", baseline.Arch),
		Source:         observedManagedHeaderProfileSource(),
		version:        version,
		hasVersion:     true,
	}
	return profile, true
}

func firstNonEmptyHeader(headers http.Header, name, fallback string) string {
	if headers == nil {
		return fallback
	}
	if value := strings.TrimSpace(headers.Get(name)); value != "" {
		return value
	}
	return fallback
}

// ClaudeAccountScopeKey exposes the per-account scope key used by the device
// profile cache so that other identity-rewriting paths (e.g. the cloak path that
// derives a synthetic device_id) can scope state to the same upstream account.
//
// The scope precedence is FileName > ID > Label > (auth present -> global) > apiKey
// > global. It is deterministic for a given auth/apiKey and intentionally avoids
// volatile material (such as OAuth tokens), so derived values stay stable across
// requests and refreshes while differing between distinct upstream accounts.
func ClaudeAccountScopeKey(auth *cliproxyauth.Auth, apiKey string) string {
	return claudeDeviceProfileScopeKey(auth, apiKey)
}

func claudeDeviceProfileScopeKey(auth *cliproxyauth.Auth, apiKey string) string {
	switch {
	case auth != nil && strings.TrimSpace(auth.FileName) != "":
		return "file:" + strings.TrimSpace(auth.FileName)
	case auth != nil && strings.TrimSpace(auth.ID) != "":
		return "auth:" + strings.TrimSpace(auth.ID)
	case auth != nil && strings.TrimSpace(auth.Label) != "":
		return "label:" + strings.TrimSpace(auth.Label)
	case auth != nil:
		return "global"
	case strings.TrimSpace(apiKey) != "":
		return "api_key:" + strings.TrimSpace(apiKey)
	default:
		return "global"
	}
}

func claudeDeviceProfileCacheKey(auth *cliproxyauth.Auth, apiKey string) string {
	sum := sha256.Sum256([]byte(claudeDeviceProfileScopeKey(auth, apiKey)))
	return hex.EncodeToString(sum[:])
}

func claudeDeviceProfileObservationCacheKeys(auth *cliproxyauth.Auth, apiKey string) []string {
	rawKeys := []string{claudeDeviceProfileScopeKey(auth, apiKey)}
	if auth != nil {
		if fileName := strings.TrimSpace(auth.FileName); fileName != "" {
			rawKeys = append(rawKeys, "file:"+fileName, "auth:"+fileName)
		}
		if id := strings.TrimSpace(auth.ID); id != "" {
			rawKeys = append(rawKeys, "auth:"+id, "file:"+id)
		}
		if label := strings.TrimSpace(auth.Label); label != "" {
			rawKeys = append(rawKeys, "label:"+label, "auth:"+label)
		}
	}
	if key := strings.TrimSpace(apiKey); key != "" {
		rawKeys = append(rawKeys, "api_key:"+key)
	}
	rawKeys = append(rawKeys, "global")
	seen := make(map[string]bool, len(rawKeys))
	out := make([]string, 0, len(rawKeys))
	for _, rawKey := range rawKeys {
		rawKey = strings.TrimSpace(rawKey)
		if rawKey == "" || seen[rawKey] {
			continue
		}
		seen[rawKey] = true
		sum := sha256.Sum256([]byte(rawKey))
		out = append(out, hex.EncodeToString(sum[:]))
	}
	return out
}

func startClaudeDeviceProfileCacheCleanup() {
	go func() {
		ticker := time.NewTicker(claudeDeviceProfileCleanupPeriod)
		defer ticker.Stop()
		for range ticker.C {
			purgeExpiredClaudeDeviceProfiles()
		}
	}()
}

func purgeExpiredClaudeDeviceProfiles() {
	now := time.Now()
	claudeDeviceProfileCacheMu.Lock()
	for key, entry := range claudeDeviceProfileCache {
		if !entry.expire.After(now) {
			delete(claudeDeviceProfileCache, key)
			delete(claudeDeviceProfileObservations, key)
		}
	}
	claudeDeviceProfileCacheMu.Unlock()
}

func recordClaudeDeviceProfileObservation(cacheKey string, profile ClaudeDeviceProfile, now time.Time) {
	if cacheKey == "" || profile.UserAgent == "" || !profile.hasVersion {
		return
	}
	entries := claudeDeviceProfileObservations[cacheKey]
	version := profile.VersionString()
	for i := range entries {
		if entries[i].profile.UserAgent == profile.UserAgent && entries[i].profile.VersionString() == version {
			entries[i].lastSeen = now
			entries[i].count++
			entries[i].profile = profile
			claudeDeviceProfileObservations[cacheKey] = sortClaudeDeviceProfileObservationEntries(entries)
			return
		}
	}
	entries = append(entries, claudeDeviceProfileObservationEntry{
		profile:   profile,
		firstSeen: now,
		lastSeen:  now,
		count:     1,
	})
	claudeDeviceProfileObservations[cacheKey] = sortClaudeDeviceProfileObservationEntries(entries)
}

func sortClaudeDeviceProfileObservationEntries(entries []claudeDeviceProfileObservationEntry) []claudeDeviceProfileObservationEntry {
	sort.SliceStable(entries, func(i, j int) bool {
		return entries[i].lastSeen.After(entries[j].lastSeen)
	})
	if len(entries) > 8 {
		entries = entries[:8]
	}
	return entries
}

func ClaudeDeviceProfileObservations(auth *cliproxyauth.Auth, apiKey string) []ClaudeDeviceProfileObservation {
	cacheKeys := claudeDeviceProfileObservationCacheKeys(auth, apiKey)
	claudeDeviceProfileCacheMu.RLock()
	var entries []claudeDeviceProfileObservationEntry
	for _, cacheKey := range cacheKeys {
		entries = append(entries, claudeDeviceProfileObservations[cacheKey]...)
	}
	claudeDeviceProfileCacheMu.RUnlock()
	if len(entries) == 0 {
		return nil
	}
	merged := make(map[string]claudeDeviceProfileObservationEntry, len(entries))
	for _, entry := range entries {
		version := entry.profile.VersionString()
		if entry.profile.UserAgent == "" || version == "" {
			continue
		}
		key := entry.profile.UserAgent + "\x00" + version
		existing, ok := merged[key]
		if !ok {
			merged[key] = entry
			continue
		}
		if entry.firstSeen.Before(existing.firstSeen) {
			existing.firstSeen = entry.firstSeen
		}
		if entry.lastSeen.After(existing.lastSeen) {
			existing.lastSeen = entry.lastSeen
			existing.profile = entry.profile
		}
		existing.count += entry.count
		merged[key] = existing
	}
	entries = entries[:0]
	for _, entry := range merged {
		entries = append(entries, entry)
	}
	entries = sortClaudeDeviceProfileObservationEntries(entries)
	out := make([]ClaudeDeviceProfileObservation, 0, len(entries))
	for _, entry := range entries {
		version := entry.profile.VersionString()
		if version == "" {
			continue
		}
		out = append(out, ClaudeDeviceProfileObservation{
			UserAgent:      entry.profile.UserAgent,
			Version:        version,
			PackageVersion: entry.profile.PackageVersion,
			RuntimeVersion: entry.profile.RuntimeVersion,
			OS:             entry.profile.OS,
			Arch:           entry.profile.Arch,
			Source:         entry.profile.Source,
			FirstSeenAt:    entry.firstSeen.UTC().Format(time.RFC3339),
			LastSeenAt:     entry.lastSeen.UTC().Format(time.RFC3339),
			RequestCount:   entry.count,
		})
	}
	return out
}

// claudeFallbackBaseline computes the effective floor profile used when an
// account has no per-request candidate and no valid cached high-water entry.
//
// High-water model (requirement ⑥, plan A):
//   - The absolute lower bound is the static/operator-configured baseline
//     (defaultClaudeDeviceProfile). The hardcoded floor constant guarantees a
//     parseable claude-cli version.
//   - The ceiling is the account's real observed first-party high-water mark,
//     surfaced as a COMPLETE triple (UA/version + pkg + runtime) from the single
//     real observation that carried it. When the account itself has no observation
//     yet, we fall back to the global real observed high-water triple — never npm
//     latest, which could fabricate a version no real client here has ever sent.
//   - 反关联修复 B（R5）：三元组作为原子单元抬升。npm online-update 只产出版本号、
//     没有配套真实 pkg/runtime，因此不再用它单独抬 UA（会造出不存在的三元组）；
//     没有完整真实三元组时，三元组整体停在 baseline。
//
// The result is monotonic-up only: the returned version is the maximum of the
// static floor and any real-observed ceiling, and the emitted triple is always
// internally consistent (all three fields from the same real source).
func claudeFallbackBaseline(auth *cliproxyauth.Auth, apiKey string, cfg *config.Config) ClaudeDeviceProfile {
	baseline := defaultClaudeDeviceProfile(cfg)

	// 反关联修复 B（R5）：抬高 floor 时把版本三元组当原子单元处理。
	// 优先取本账号观测到的最高版本**完整三元组**；本账号零观测时回退全局最高版本
	// **完整三元组**。三元组（UA/version + pkg + runtime）全部来自同一次真实观测，
	// 绝不把抬高的 UA 与 baseline 旧常量 pkg/runtime 拼接。
	observationKeys := claudeDeviceProfileObservationCacheKeys(auth, apiKey)
	ceilingProfile, hasCeiling := claudeObservedHighWaterProfile(observationKeys)
	if !hasCeiling {
		ceilingProfile, hasCeiling = globalClaudeObservedHighWaterProfile()
	}

	// claude 版本高水位持久化：重启/部署后内存观测被清空，会回落到 floor 2.1.63。
	// 这里把上一次 persist 进 auth.Metadata 的高水位三元组作为额外的 ceiling 候选，
	// 与内存观测取 max。persisted 三元组本身来自上一进程里已过 sanity-ceiling gate 的
	// 真实观测（写回点在 RaiseClaudeDeviceHighWater 之前已做 sanity 校验），因此不会
	// 固化伪造超高版本。第一笔请求即可从 persisted 值起步，而非回 floor。
	if persisted, hasPersisted := claudePersistedHighWaterProfile(auth); hasPersisted {
		if !hasCeiling || persisted.version.Compare(ceilingProfile.version) > 0 {
			ceilingProfile = persisted
			hasCeiling = true
		}
	}

	// 只有当存在同一次真实观测的完整三元组、且其版本高于 baseline 时才整体抬升；
	// 否则三元组整体停在 baseline（baseline 自身是内部自洽的真实发布三元组：
	// 2.1.63 / 0.74.0 / v24.3.0）。
	if hasCeiling && (!baseline.hasVersion || ceilingProfile.version.Compare(baseline.version) > 0) {
		baseline = withClaudeFloorProfile(baseline, ceilingProfile, observedManagedHeaderProfileSource())
	}

	// online-update（npm latest）只产出一个 version 数字，没有与之配套的真实
	// pkg/runtime 观测。若据此单独抬 UA，必然造出"新 UA + 旧 pkg/runtime"的不存在
	// 三元组——这正是 R5 要消除的反关联信号。因此在没有完整真实三元组可依据时，
	// 不再用 npm 抬 UA；三元组整体停在 baseline。这与 online-update 默认关、不引入
	// 对外网版本映射拉取的设计一致。
	return baseline
}

// withClaudeFloorProfile 返回 profile 的一份拷贝，将其版本三元组
// （User-Agent/version、PackageVersion、RuntimeVersion）整体替换为 source 这一份
// 真实观测三元组，平台位（OS/Arch）保持 baseline 不变（由 pin 流程另行统一）。
// 反关联修复 B（R5）：三元组作为原子单元一起抬升，杜绝"新 UA + 旧常量 pkg/runtime"。
func withClaudeFloorProfile(profile, source ClaudeDeviceProfile, profileSource ManagedHeaderProfileSource) ClaudeDeviceProfile {
	profile.UserAgent = source.UserAgent
	profile.version = source.version
	profile.hasVersion = source.hasVersion
	if strings.TrimSpace(source.PackageVersion) != "" {
		profile.PackageVersion = source.PackageVersion
	}
	if strings.TrimSpace(source.RuntimeVersion) != "" {
		profile.RuntimeVersion = source.RuntimeVersion
	}
	profile.Source = profileSource
	return profile
}

func ResolveClaudeDeviceProfile(auth *cliproxyauth.Auth, apiKey string, headers http.Header, cfg *config.Config) ClaudeDeviceProfile {
	claudeDeviceProfileCacheCleanupOnce.Do(startClaudeDeviceProfileCacheCleanup)

	cacheKey := claudeDeviceProfileCacheKey(auth, apiKey)
	now := time.Now()
	baseline := claudeFallbackBaseline(auth, apiKey, cfg)
	candidate, hasCandidate := extractClaudeDeviceProfile(headers, cfg)
	if hasCandidate {
		candidate = pinClaudeDeviceProfilePlatform(candidate, baseline)
	}
	// Sanity-ceiling gate (source-level rejection): a candidate whose claimed
	// claude-cli version exceeds the effective sanity ceiling is treated as a
	// fabricated inbound User-Agent. Drop it here, before any observation is
	// recorded, so a forged high version (e.g. claude-cli/999.0.0 from a holder of
	// a valid downstream key) can never enter the per-account or global observed
	// high-water and can never become the outbound version applied to other
	// accounts. This check runs for both upgrade and non-upgrade candidates,
	// because a forged version is precisely the case that would otherwise look
	// like an "upgrade" over the real baseline.
	if hasCandidate && !claudeObservationWithinSanityCeiling(candidate, cfg) {
		hasCandidate = false
	}
	if hasCandidate && !shouldUpgradeClaudeDeviceProfile(candidate, baseline) {
		staticBaselineVersion, _ := parseClaudeCLIVersion(defaultClaudeFingerprintUserAgent)
		allowObservedFirstParty := candidate.hasVersion &&
			candidate.version.Compare(staticBaselineVersion) >= 0
		if !allowObservedFirstParty {
			hasCandidate = false
		}
	}

	claudeDeviceProfileCacheMu.RLock()
	entry, hasCached := claudeDeviceProfileCache[cacheKey]
	cachedValid := hasCached && entry.expire.After(now) && entry.profile.UserAgent != ""
	claudeDeviceProfileCacheMu.RUnlock()

	if hasCandidate {
		if ClaudeDeviceProfileBeforeCandidateStore != nil {
			ClaudeDeviceProfileBeforeCandidateStore(candidate)
		}

		claudeDeviceProfileCacheMu.Lock()
		recordClaudeDeviceProfileObservation(cacheKey, candidate, now)
		entry, hasCached = claudeDeviceProfileCache[cacheKey]
		cachedValid = hasCached && entry.expire.After(now) && entry.profile.UserAgent != ""
		if cachedValid {
			entry.profile = normalizeClaudeDeviceProfile(entry.profile, baseline)
		}
		if cachedValid && !shouldUpgradeClaudeDeviceProfile(candidate, entry.profile) {
			entry.expire = now.Add(claudeDeviceProfileTTL)
			claudeDeviceProfileCache[cacheKey] = entry
			claudeDeviceProfileCacheMu.Unlock()
			return entry.profile
		}

		claudeDeviceProfileCache[cacheKey] = claudeDeviceProfileCacheEntry{
			profile: candidate,
			expire:  now.Add(claudeDeviceProfileTTL),
		}
		claudeDeviceProfileCacheMu.Unlock()
		return candidate
	}

	if cachedValid {
		claudeDeviceProfileCacheMu.Lock()
		entry = claudeDeviceProfileCache[cacheKey]
		if entry.expire.After(now) && entry.profile.UserAgent != "" {
			entry.profile = normalizeClaudeDeviceProfile(entry.profile, baseline)
			entry.expire = now.Add(claudeDeviceProfileTTL)
			claudeDeviceProfileCache[cacheKey] = entry
			claudeDeviceProfileCacheMu.Unlock()
			return entry.profile
		}
		claudeDeviceProfileCacheMu.Unlock()
	}

	return baseline
}

func ApplyClaudeDeviceProfileHeaders(r *http.Request, profile ClaudeDeviceProfile) {
	if r == nil {
		return
	}
	for _, headerName := range []string{
		"User-Agent",
		"X-Stainless-Package-Version",
		"X-Stainless-Runtime-Version",
		"X-Stainless-Os",
		"X-Stainless-Arch",
	} {
		r.Header.Del(headerName)
	}
	r.Header.Set("User-Agent", profile.UserAgent)
	r.Header.Set("X-Stainless-Package-Version", profile.PackageVersion)
	r.Header.Set("X-Stainless-Runtime-Version", profile.RuntimeVersion)
	r.Header.Set("X-Stainless-Os", profile.OS)
	r.Header.Set("X-Stainless-Arch", profile.Arch)
}

// defaultClaudeFingerprintUserAgentSuffix is the parenthetical suffix
// "(external, cli)" used by real interactive claude-cli. It is also the suffix
// implied by the default cc_entrypoint ("cli") derived from a non-claude-code
// inbound client. Outbound UA suffix and cc_entrypoint must reference the same
// inbound-derived entrypoint, so this default is shared by both.
const defaultClaudeFingerprintUserAgentSuffix = "(external, cli)"

// claudeUserAgentSuffixPattern captures the first parenthetical block of a
// claude-cli User-Agent, e.g. the "(external, cli)" in
// "claude-cli/2.1.63 (external, cli)".
var claudeUserAgentSuffixPattern = regexp.MustCompile(`\([^)]*\)`)

// claudeClientUserAgentSuffix returns the parenthetical "(USER_TYPE, ENTRYPOINT)"
// block of an inbound claude-code client User-Agent. When the inbound client is
// not a claude-code client, or carries no parenthetical block, it returns the
// default "(external, cli)" suffix — which is exactly the suffix that matches the
// default cc_entrypoint ("cli") produced for such clients. This keeps the
// outbound UA suffix and the billing cc_entrypoint derived from the same source.
func claudeClientUserAgentSuffix(clientUA string) string {
	clientUA = strings.TrimSpace(clientUA)
	if !isClaudeCodeClient(clientUA) {
		return defaultClaudeFingerprintUserAgentSuffix
	}
	if match := claudeUserAgentSuffixPattern.FindString(clientUA); match != "" {
		return match
	}
	return defaultClaudeFingerprintUserAgentSuffix
}

// AlignClaudeDeviceProfileUserAgentSuffix rewrites the parenthetical suffix of the
// stabilized outbound User-Agent so it mirrors the inbound claude-code client's
// "(USER_TYPE, ENTRYPOINT)" block, while preserving the high-water
// "claude-cli/<version>" prefix and all other stabilized fingerprint fields.
//
// Anti-correlation invariant: the outbound UA suffix and the billing
// cc_entrypoint are both derived from the same inbound client User-Agent
// (parseEntrypointFromUA in the executor reads the same source). Without this
// alignment the outbound UA suffix comes from a frozen high-water device profile
// (which a single "claude --print" can seed to "sdk-cli") while cc_entrypoint is
// derived per request, producing a UA/entrypoint pair (e.g. "(external, sdk-cli)"
// + cc_entrypoint=cli) that real claude-code never emits and that Anthropic can
// detect. After this call the suffix and cc_entrypoint can no longer diverge.
//
// Only the parenthetical suffix is rewritten; the version, package version,
// runtime version, OS and arch fields stay at their stabilized high-water values.
func AlignClaudeDeviceProfileUserAgentSuffix(r *http.Request, clientUA string) {
	if r == nil {
		return
	}
	outboundUA := strings.TrimSpace(r.Header.Get("User-Agent"))
	if outboundUA == "" || !isClaudeCodeClient(outboundUA) {
		return
	}
	desiredSuffix := claudeClientUserAgentSuffix(clientUA)
	if claudeUserAgentSuffixPattern.MatchString(outboundUA) {
		aligned := claudeUserAgentSuffixPattern.ReplaceAllString(outboundUA, desiredSuffix)
		r.Header.Set("User-Agent", aligned)
		return
	}
	// Outbound UA has no parenthetical block (unexpected for stabilized profiles,
	// but be defensive): append the desired suffix so it stays paired with
	// cc_entrypoint.
	r.Header.Set("User-Agent", outboundUA+" "+desiredSuffix)
}

// DefaultClaudeVersion returns the version string (e.g. "2.1.63") from the
// current baseline device profile. It extracts the version from the User-Agent.
func DefaultClaudeVersion(cfg *config.Config) string {
	profile := defaultClaudeDeviceProfile(cfg)
	if version := profile.VersionString(); version != "" {
		return version
	}
	return "2.1.63"
}

func ApplyClaudeLegacyDeviceHeaders(r *http.Request, ginHeaders http.Header, cfg *config.Config) {
	if r == nil {
		return
	}
	profile := defaultClaudeDeviceProfile(cfg)
	miscEnsure := func(name, fallback string) {
		if strings.TrimSpace(r.Header.Get(name)) != "" {
			return
		}
		if strings.TrimSpace(ginHeaders.Get(name)) != "" {
			r.Header.Set(name, strings.TrimSpace(ginHeaders.Get(name)))
			return
		}
		r.Header.Set(name, fallback)
	}

	miscEnsure("X-Stainless-Runtime-Version", profile.RuntimeVersion)
	miscEnsure("X-Stainless-Package-Version", profile.PackageVersion)
	miscEnsure("X-Stainless-Os", mapStainlessOS())
	miscEnsure("X-Stainless-Arch", mapStainlessArch())

	// Legacy mode preserves per-auth custom header overrides. By the time we get
	// here, ApplyCustomHeadersFromAttrs has already populated r.Header.
	if strings.TrimSpace(r.Header.Get("User-Agent")) != "" {
		return
	}

	clientUA := ""
	if ginHeaders != nil {
		clientUA = strings.TrimSpace(ginHeaders.Get("User-Agent"))
	}
	if isClaudeCodeClient(clientUA) {
		r.Header.Set("User-Agent", clientUA)
		return
	}
	r.Header.Set("User-Agent", profile.UserAgent)
}
