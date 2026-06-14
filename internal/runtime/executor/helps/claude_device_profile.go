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
	// observation in claudeFallbackBaseline / claudeOnlineFloorVersion).
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
	// hardcoded floor constant. online-update is consulted later, capped to the
	// real observed high-water, in claudeOnlineFloorVersion.
	return profile
}

// claudeObservedHighWaterVersion returns the highest real first-party claude-cli
// version observed across the supplied observation cache keys (per-account when
// scoped, global when the caller passes the full key set). It only reflects
// versions that a real client actually presented to this proxy, so it never
// fabricates a version that does not exist in the wild for this deployment.
func claudeObservedHighWaterVersion(cacheKeys []string) (claudeCLIVersion, bool) {
	claudeDeviceProfileCacheMu.RLock()
	defer claudeDeviceProfileCacheMu.RUnlock()
	var best claudeCLIVersion
	found := false
	for _, cacheKey := range cacheKeys {
		for _, entry := range claudeDeviceProfileObservations[cacheKey] {
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

// claudeOnlineFloorVersion consults the online registry (npm latest) ONLY as a
// floor reference, and caps it to the supplied real observed high-water ceiling.
// It can never raise the outbound version above what a real client presented.
// When online-update is disabled (the new default) it returns nothing. The
// returned version is the min(npm-latest, observed-ceiling), used only to lift a
// stale static floor toward, but never beyond, real observation.
func claudeOnlineFloorVersion(cfg *config.Config, ceiling claudeCLIVersion, hasCeiling bool) (claudeCLIVersion, bool) {
	if !hasCeiling {
		return claudeCLIVersion{}, false
	}
	online, ok := resolveManagedHeaderOnlineVersion("claude", cfg)
	if !ok {
		return claudeCLIVersion{}, false
	}
	candidateUA := "claude-cli/" + online.Version + " (external, cli)"
	candidateVersion, candidateOK := parseClaudeCLIVersion(candidateUA)
	if !candidateOK {
		return claudeCLIVersion{}, false
	}
	// Cap to the real observed ceiling: npm is never allowed above real.
	if candidateVersion.Compare(ceiling) > 0 {
		return ceiling, true
	}
	return candidateVersion, true
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
//   - The ceiling is the account's real observed first-party high-water mark.
//     When the account itself has no observation yet, we fall back to the global
//     real observed high-water (the highest version any real client presented to
//     this proxy on any account) — never npm latest, which could fabricate a
//     version no real client here has ever sent.
//   - online-update (npm) is consulted only as a floor reference, capped to that
//     real observed ceiling, so it can lift a stale static floor toward, but
//     never beyond, real observation.
//
// The result is monotonic-up only: the returned version is the maximum of the
// static floor and any real-observed ceiling, with npm never able to exceed real.
func claudeFallbackBaseline(auth *cliproxyauth.Auth, apiKey string, cfg *config.Config) ClaudeDeviceProfile {
	baseline := defaultClaudeDeviceProfile(cfg)

	// Real observed ceiling: prefer this account's observed high-water; if the
	// account has none, fall back to the global observed high-water.
	observationKeys := claudeDeviceProfileObservationCacheKeys(auth, apiKey)
	ceiling, hasCeiling := claudeObservedHighWaterVersion(observationKeys)
	if !hasCeiling {
		ceiling, hasCeiling = globalClaudeObservedHighWaterVersion()
	}

	// Lift the baseline floor up to the real observed ceiling (only upward, never
	// below the static/operator floor).
	if hasCeiling && (!baseline.hasVersion || ceiling.Compare(baseline.version) > 0) {
		baseline = withClaudeFloorVersion(baseline, ceiling, observedManagedHeaderProfileSource())
	}

	// online-update may lift the floor further, but only within the real observed
	// ceiling — npm can never raise the outbound version above real observation.
	if onlineVersion, ok := claudeOnlineFloorVersion(cfg, ceiling, hasCeiling); ok {
		if !baseline.hasVersion || onlineVersion.Compare(baseline.version) > 0 {
			source := observedManagedHeaderProfileSource()
			if online, okOnline := resolveManagedHeaderOnlineVersion("claude", cfg); okOnline && onlineVersion.Compare(ceiling) < 0 {
				source = online.ManagedHeaderProfileSource
			}
			baseline = withClaudeFloorVersion(baseline, onlineVersion, source)
		}
	}

	return baseline
}

// withClaudeFloorVersion returns a copy of the profile with its claimed
// claude-cli version (and User-Agent) lifted to the supplied version, keeping the
// rest of the platform/software fingerprint from the baseline. Used to raise the
// fallback floor toward a real observed high-water without inventing other
// header values.
func withClaudeFloorVersion(profile ClaudeDeviceProfile, version claudeCLIVersion, source ManagedHeaderProfileSource) ClaudeDeviceProfile {
	profile.UserAgent = "claude-cli/" + formatClaudeCLIVersion(version) + " (external, cli)"
	profile.version = version
	profile.hasVersion = true
	profile.Source = source
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
