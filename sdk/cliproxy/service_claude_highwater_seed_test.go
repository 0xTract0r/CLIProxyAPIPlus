package cliproxy

import (
	"context"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

// TestSeedClaudeObservedHighWaterFromLoadedAuths_WarmsGlobalFallbackAcrossRestart
// is the Phase B startup-aggregation regression. After a (re)start the in-memory
// device-profile observation map is empty, but every account's persisted
// claude_device_high_water triple survives on disk in auth.Metadata. The startup
// pass must aggregate those persisted triples into the global observed high-water so
// a brand-new zero-observation account inherits this deployment's real current
// version from the very first request, instead of collapsing to the hardcoded floor.
func TestSeedClaudeObservedHighWaterFromLoadedAuths_WarmsGlobalFallbackAcrossRestart(t *testing.T) {
	helps.ResetClaudeDeviceProfileCache()
	t.Cleanup(helps.ResetClaudeDeviceProfileCache)

	// A real current-generation version, comfortably above the hardcoded floor.
	const persistedVersion = "2.1.230"

	service := &Service{
		cfg:         &config.Config{},
		coreManager: coreauth.NewManager(nil, nil, nil),
	}

	authID := "claude-restart-highwater-auth"
	t.Cleanup(func() { GlobalModelRegistry().UnregisterClient(authID) })

	if _, err := service.coreManager.Register(context.Background(), &coreauth.Auth{
		ID:       authID,
		Provider: "claude",
		Status:   coreauth.StatusActive,
		Metadata: map[string]any{
			coreauth.ClaudeDeviceHighWaterMetadataKey: map[string]string{
				"user_agent":      "claude-cli/" + persistedVersion + " (external, cli)",
				"version":         persistedVersion,
				"package_version": "0.95.0",
				"runtime_version": "v26.5.0",
				"os":              "MacOS",
				"arch":            "arm64",
				"source":          "observed",
			},
		},
	}); err != nil {
		t.Fatalf("Register() error = %v", err)
	}

	// Simulate a fresh process: clear the in-memory observation map that would
	// normally be empty right after a restart, while the auth still carries the
	// persisted triple.
	helps.ResetClaudeDeviceProfileCache()

	// Precondition: before the startup seed, a brand-new zero-observation account
	// falls back to the hardcoded floor (there is no live/global observation yet).
	before := helps.ResolveClaudeDeviceProfile(
		&coreauth.Auth{ProxyURL: "direct", ID: "claude-zero-obs-before", Provider: "claude"},
		"", nil, &config.Config{},
	)
	if got := before.VersionString(); got != helps.DefaultClaudeVersion(nil) {
		t.Fatalf("precondition zero-observation version = %q, want hardcoded floor %q (map must be cold before seed)", got, helps.DefaultClaudeVersion(nil))
	}

	// Startup aggregation over all loaded accounts.
	service.seedClaudeObservedHighWaterFromLoadedAuths()

	// After the seed, a brand-new zero-observation account inherits the persisted
	// current version as its fallback ceiling, not the hardcoded floor.
	after := helps.ResolveClaudeDeviceProfile(
		&coreauth.Auth{ProxyURL: "direct", ID: "claude-zero-obs-after", Provider: "claude"},
		"", nil, &config.Config{},
	)
	if got := after.VersionString(); got != persistedVersion {
		t.Fatalf("post-seed zero-observation version = %q, want persisted current version %q (startup seed must warm the global fallback across restart)", got, persistedVersion)
	}
	// The whole triple is inherited atomically from the same persisted observation,
	// never spliced with stale baseline constants.
	if after.PackageVersion != "0.95.0" || after.RuntimeVersion != "v26.5.0" {
		t.Fatalf("post-seed triple = %s/%s/%s, want atomic persisted triple (pkg 0.95.0 / runtime v26.5.0)", after.UserAgent, after.PackageVersion, after.RuntimeVersion)
	}
}
