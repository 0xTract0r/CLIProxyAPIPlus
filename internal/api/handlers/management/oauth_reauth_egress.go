package management

import (
	"errors"
	"fmt"
	"strings"

	"github.com/gin-gonic/gin"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/proxyutil"
)

// Fork (fix-oauth-egress-anticorr): egress guards for the built-in claude /
// codex OAuth token exchange entry points. Every exchange must leave through
// the proxy of the account it authenticates, with the provider's official
// ClientHello, and must never fall back to the core global proxy or a direct
// connection.

// oauthEgressProxyBlockReason mirrors the serving-side account proxy guard
// (helps.accountProxyBlockReason): an empty or malformed proxy setting cannot
// carry an OAuth token exchange. The explicit "direct"/"none" sentinels and any
// valid proxy URL are accepted as a deliberate operator decision.
func oauthEgressProxyBlockReason(proxyURL string) string {
	trimmed := strings.TrimSpace(proxyURL)
	if trimmed == "" {
		return "empty_proxy_url"
	}
	setting, err := proxyutil.Parse(trimmed)
	if err != nil || setting.Mode == proxyutil.ModeInvalid {
		return "invalid_proxy_url"
	}
	return ""
}

// resolveOAuthReauthTarget resolves the card re-auth target named by the
// auth_name query parameter. It returns a nil target and nil error when
// auth_name is absent. Every failure is a client error: callers answer 400
// before generating an authorization URL, so a rejected re-auth never opens a
// session, never dials out and never falls back to the core global proxy.
func (h *Handler) resolveOAuthReauthTarget(c *gin.Context, provider string) (*coreauth.Auth, string, error) {
	name := strings.TrimSpace(c.Query("auth_name"))
	if name == "" {
		return nil, "", nil
	}
	if isUnsafeAuthFileName(name) {
		return nil, name, errors.New("invalid auth_name")
	}
	target := h.findAuthByNameOrID(name)
	if target == nil {
		return nil, name, errors.New("auth_name target not found")
	}
	if !strings.EqualFold(providerKey(target), provider) {
		return nil, name, fmt.Errorf("auth_name must reference a %s auth file", provider)
	}
	if reason := oauthEgressProxyBlockReason(authProxyURL(target)); reason != "" {
		return nil, name, fmt.Errorf("auth_name target has no usable proxy_url (%s); configure the account proxy before re-authenticating", reason)
	}
	return h.syncAuthManagedHeaderState(c.Request.Context(), target), name, nil
}

// checkOAuthStartEgress enforces that an OAuth start without a re-auth target
// has a proxy to exchange through: a new-account setup must carry its own
// valid proxy_url, and a request without any setup needs a usable global
// proxy-url (otherwise the exchange would leave over a direct connection).
func (h *Handler) checkOAuthStartEgress(setup *oauthAccountSetup) error {
	if setup != nil {
		if reason := oauthEgressProxyBlockReason(setup.ProxyURL); reason != "" {
			return fmt.Errorf("proxy_url is required for a new OAuth account (%s)", reason)
		}
		return nil
	}
	globalProxyURL := ""
	if h != nil && h.cfg != nil {
		globalProxyURL = h.cfg.ProxyURL
	}
	if reason := oauthEgressProxyBlockReason(globalProxyURL); reason != "" {
		return fmt.Errorf("no proxy available for the OAuth token exchange (%s): pass proxy_url for a new account, auth_name for a re-auth, or configure the global proxy-url", reason)
	}
	return nil
}
