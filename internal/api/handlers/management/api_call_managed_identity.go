package management

import (
	"net"
	"net/http"
	"net/url"
	"strings"

	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

// Fork (fix-oauth-egress-anticorr, vuln 4 / checklist A6): the management
// api-call proxy (dashboard quota lookups against api.anthropic.com and
// chatgpt.com) carries a real claude / codex account token. It must leave
// through that account's own proxy and present the account's serving-managed
// client identity, never a dashboard-hardcoded or Go-default User-Agent.

const apiCallEgressSite = "management_api_call"

// apiCallManagedIdentityHosts lists, per provider, the protected upstream hosts
// for which an api-call must present the account's managed identity headers.
var apiCallManagedIdentityHosts = map[string]map[string]struct{}{
	"claude": {
		"api.anthropic.com": {},
	},
	"codex": {
		"chatgpt.com":     {},
		"auth.openai.com": {},
	},
}

// apiCallManagedIdentityProvider is implemented by the claude / codex executors.
// It returns the account's serving-managed client identity headers for a
// request aimed at target.
type apiCallManagedIdentityProvider interface {
	ManagedIdentityHeaders(target *url.URL, auth *coreauth.Auth) http.Header
}

// apiCallIdentityProviderKey returns the lower-cased provider key when auth is a
// claude / codex account that api-call must harden, or "" otherwise.
func apiCallIdentityProviderKey(auth *coreauth.Auth) string {
	if auth == nil {
		return ""
	}
	provider := strings.ToLower(strings.TrimSpace(auth.Provider))
	if _, ok := apiCallManagedIdentityHosts[provider]; ok {
		return provider
	}
	return ""
}

// apiCallAccountProxyBlockReason reports why a claude / codex account cannot
// carry an api-call: the account-scoped proxy (auth.ProxyURL, then the
// per-key api-key config proxy, i.e. the first candidates apiCallResolvedProxyURL
// will use) is empty or malformed. The core global proxy is deliberately not a
// fallback for these accounts. Explicit "direct"/"none" and valid proxy URLs
// pass. Returns "" for every other provider, which keeps today's behaviour.
func (h *Handler) apiCallAccountProxyBlockReason(auth *coreauth.Auth) string {
	if apiCallIdentityProviderKey(auth) == "" {
		return ""
	}
	accountProxy := strings.TrimSpace(auth.ProxyURL)
	if accountProxy == "" && h != nil && h.cfg != nil {
		accountProxy = strings.TrimSpace(proxyURLFromAPIKeyConfig(h.cfg, auth))
	}
	return oauthEgressProxyBlockReason(accountProxy)
}

// apiCallTargetsManagedIdentityHost reports whether the request is aimed at one
// of the provider's protected upstream hosts, by URL host or by an explicit
// Host header override.
func apiCallTargetsManagedIdentityHost(provider string, target *url.URL, hostOverride string) bool {
	hosts := apiCallManagedIdentityHosts[provider]
	if len(hosts) == 0 {
		return false
	}
	candidates := make([]string, 0, 2)
	if target != nil {
		candidates = append(candidates, target.Hostname())
	}
	if override := strings.TrimSpace(hostOverride); override != "" {
		if host, _, errSplit := net.SplitHostPort(override); errSplit == nil {
			override = host
		}
		candidates = append(candidates, override)
	}
	for _, candidate := range candidates {
		if _, ok := hosts[strings.ToLower(strings.TrimSuffix(strings.TrimSpace(candidate), "."))]; ok {
			return true
		}
	}
	return false
}

// apiCallManagedIdentityHeaders resolves the account's managed identity headers
// from the provider executor registered on the auth manager (the same executor
// serving and quota polling use).
func (h *Handler) apiCallManagedIdentityHeaders(provider string, target *url.URL, auth *coreauth.Auth) http.Header {
	manager := h.currentAuthManager()
	if manager == nil {
		return nil
	}
	exec, ok := manager.Executor(provider)
	if !ok || exec == nil {
		return nil
	}
	identity, ok := exec.(apiCallManagedIdentityProvider)
	if !ok {
		return nil
	}
	return identity.ManagedIdentityHeaders(target, auth)
}

// applyAPICallManagedIdentity replaces every caller-supplied header that the
// managed identity also defines (case-insensitively) with the managed value.
// Business headers outside the identity set are left untouched.
func applyAPICallManagedIdentity(dst http.Header, identity http.Header) {
	for name, values := range identity {
		for existing := range dst {
			if strings.EqualFold(existing, name) {
				delete(dst, existing)
			}
		}
		for _, value := range values {
			dst.Add(name, value)
		}
	}
}
