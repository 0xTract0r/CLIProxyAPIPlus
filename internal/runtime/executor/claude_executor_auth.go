package executor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	claudeauth "github.com/router-for-me/CLIProxyAPI/v7/internal/auth/claude"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

// errFarmUnprovisionedRefreshProxyMissing is returned by ClaudeExecutor.Refresh
// when the farm supply-atomicity fail-closed gate matches an account
// (FARM_REQUIRE_PROVISIONED armed + farm-enrolled + unprovisioned Claude) that
// resolves to no proxy at all. Falling through to NewClaudeAuthWithProxyURL with
// an empty proxy builds a direct OAuth transport (NewAnthropicHttpClient ->
// proxyutil.NewDirectTransport) that would expose the real server IP to
// api.anthropic.com under only the synthetic device_id. This sentinel is a new
// executor-local error (not the helps package's egress guard) so the fail-closed
// reason is specific to the farm supply-atomicity policy rather than the generic
// serving egress guard, and callers can errors.Is it distinctly.
var errFarmUnprovisionedRefreshProxyMissing = errors.New("farm account not provisioned and no proxy configured: refusing direct OAuth refresh to prevent IP exposure")

func (e *ClaudeExecutor) Refresh(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	log.Debugf("claude executor: refresh called")
	if refreshed, handled, err := helps.RefreshAuthViaHome(ctx, e.cfg, auth); handled {
		return refreshed, err
	}
	if auth == nil {
		return nil, fmt.Errorf("claude executor: auth is nil")
	}
	// Honor operator-controlled refresh_disabled / refresh_enabled=false flags
	// at the executor layer so unaware request paths cannot bypass the
	// auto-refresh scheduler.
	if auth.RefreshDisabled() {
		log.Debugf("claude executor: refresh skipped because refresh_disabled=true")
		return auth, nil
	}
	var refreshToken string
	if auth.Metadata != nil {
		if v, ok := auth.Metadata["refresh_token"].(string); ok && v != "" {
			refreshToken = v
		}
	}
	if refreshToken == "" {
		return auth, nil
	}
	// Farm supply-atomicity fail-closed gate (R5-3d): when the gate matches this
	// account AND neither the account nor the global config carries a proxy, refuse
	// to refresh instead of falling through to NewClaudeAuthWithProxyURL, whose
	// empty-proxy path builds a direct transport that would expose the real server
	// IP to the token endpoint. The auto-refresh scheduler (R5-3e) already unschedules
	// these accounts; this is the defense-in-depth executor-layer guard for any
	// other refresh caller. Strict no-op when FARM_REQUIRE_PROVISIONED is off / for
	// non-enrolled or provisioned accounts (predicate returns false), so refresh
	// behaviour stays byte-identical today. An account WITH a resolved proxy still
	// refreshes normally (egress is not direct), so this never fail-closes a
	// properly-proxied enrolled account.
	if cliproxyauth.RequireProvisionedBlocked(auth) {
		cfgProxyURL := ""
		if e.cfg != nil {
			cfgProxyURL = strings.TrimSpace(e.cfg.ProxyURL)
		}
		if strings.TrimSpace(auth.ProxyURL) == "" && cfgProxyURL == "" {
			log.Warnf("claude executor: refusing OAuth refresh for farm-unprovisioned auth %q with no proxy (fail-closed)", auth.ID)
			return nil, errFarmUnprovisionedRefreshProxyMissing
		}
	}
	svc := claudeauth.NewClaudeAuthWithProxyURL(e.cfg, auth.ProxyURL)
	// Raise the OAuth refresh User-Agent to this account's persisted
	// device-profile high-water mark (when present) so background token
	// refresh matches the same claude-cli identity this account's serving
	// requests present, instead of the generic claudeOAuthUserAgent floor.
	// The high-water suffix entrypoint is folded (sdk-cli -> cli, gated by
	// config.NormalizeSdkCliEntrypointEnabled) so the token-endpoint UA suffix
	// matches the serving outbound UA suffix aligned by
	// helps.AlignClaudeDeviceProfileUserAgentSuffix.
	if refreshUA := claudeRefreshHighWaterUserAgent(e.cfg, auth); refreshUA != "" {
		svc = svc.WithUserAgent(refreshUA)
	}
	td, err := svc.RefreshTokensWithRetry(ctx, refreshToken, 3)
	if err != nil {
		return nil, err
	}
	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	auth.Metadata["access_token"] = td.AccessToken
	if td.RefreshToken != "" {
		auth.Metadata["refresh_token"] = td.RefreshToken
	}
	auth.Metadata["email"] = td.Email
	auth.Metadata["expired"] = td.Expire
	auth.Metadata["type"] = "claude"
	now := time.Now().Format(time.RFC3339)
	auth.Metadata["last_refresh"] = now
	return auth, nil
}

// claudeRefreshHighWaterUserAgent returns the OAuth-refresh User-Agent for auth:
// the account's persisted device-profile high-water User-Agent with its suffix
// entrypoint folded (sdk-cli -> cli, gated by config.NormalizeSdkCliEntrypointEnabled)
// so the token-endpoint refresh identity matches the serving outbound UA suffix.
// It returns "" when auth has no usable high-water User-Agent, in which case the
// caller leaves the constructor's claudeOAuthUserAgent floor untouched.
func claudeRefreshHighWaterUserAgent(cfg *config.Config, auth *cliproxyauth.Auth) string {
	if auth == nil {
		return ""
	}
	hw, ok := cliproxyauth.ClaudeDeviceHighWaterFromMetadata(auth.Metadata)
	if !ok || strings.TrimSpace(hw.UserAgent) == "" {
		return ""
	}
	return helps.NormalizeClaudeUserAgentEntrypoint(cfg, hw.UserAgent)
}
