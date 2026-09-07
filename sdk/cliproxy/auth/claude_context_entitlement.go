package auth

import (
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

const claudeContext1MMetadataKey = "claude_context_1m"

func claudeContext1MRequested(model string, opts cliproxyexecutor.Options) bool {
	if enabled, _ := opts.Metadata[claudeContext1MMetadataKey].(bool); enabled {
		return true
	}
	for _, name := range []string{model, requestedModelMetadataValue(opts.Metadata)} {
		if strings.HasSuffix(strings.ToLower(canonicalModelKey(name)), "[1m]") {
			return true
		}
	}
	for name, values := range opts.Headers {
		if !strings.EqualFold(name, "Anthropic-Beta") {
			continue
		}
		for _, value := range values {
			for _, beta := range strings.Split(value, ",") {
				if strings.EqualFold(strings.TrimSpace(beta), "context-1m-2025-08-07") {
					return true
				}
			}
		}
	}
	return false
}

// Preserve the capability across alias/interceptor rewrites without changing
// the wire request or any persisted account metadata.
func withClaudeContext1M(model string, opts cliproxyexecutor.Options) cliproxyexecutor.Options {
	if !claudeContext1MRequested(model, opts) {
		return opts
	}
	meta := make(map[string]any, len(opts.Metadata)+1)
	for key, value := range opts.Metadata {
		meta[key] = value
	}
	meta[claudeContext1MMetadataKey] = true
	opts.Metadata = meta
	return opts
}

func authAllowsClaudeContext(auth *Auth, model string, opts cliproxyexecutor.Options) bool {
	if auth == nil || !strings.EqualFold(strings.TrimSpace(auth.Provider), "claude") || auth.AuthKind() == AuthKindAPIKey {
		return true
	}
	if !registry.IsClaudeOpusModelID(model) || !claudeContext1MRequested(model, opts) {
		return true
	}
	return registry.ClaudePlanAllowsOpusLongContext(authClaudeSubscriptionPlanType(auth), false)
}

func claudeContextEntitlementError() error {
	return &Error{Code: "auth_not_found", HTTPStatus: http.StatusServiceUnavailable,
		Message: "No eligible Claude subscription for Opus 1M; Pro and unknown plans are excluded (subscription usage only)"}
}

// Resolve aliases for capability inspection without the plan gate hiding an
// ineligible target. Actual execution still uses the normal gated resolver.
func (m *Manager) authAllowsClaudeContextRequest(auth *Auth, model string, opts cliproxyexecutor.Options) bool {
	if auth == nil || !strings.EqualFold(strings.TrimSpace(auth.Provider), "claude") {
		return true
	}
	opts = withClaudeContext1M(model, opts)
	model = rewriteModelForAuth(model, auth)
	if upstream := strings.TrimSpace(auth.Attributes[homeUpstreamModelAttributeKey]); upstream != "" {
		model = upstream
	} else if alias := resolveUpstreamModelFromAliases(OAuthModelAliasesFromAttributes(auth.Attributes), model); alias.UpstreamModel != "" {
		model = alias.UpstreamModel
	} else if table, ok := m.oauthModelAlias.Load().(*oauthModelAliasTable); ok && table != nil {
		if alias, exists := table.reverse["claude"][strings.ToLower(canonicalModelKey(model))]; exists {
			model = alias.upstreamModel
		}
	}
	return authAllowsClaudeContext(auth, model, opts)
}

// Reuse the scheduler's request-local exclusions so priorities, pinning, and
// retry/fallback selection cannot choose a subscription without this capability.
func (m *Manager) excludeClaudeContextAuths(model string, opts cliproxyexecutor.Options, tried map[string]struct{}) map[string]struct{} {
	out := tried
	copied := false
	m.mu.RLock()
	defer m.mu.RUnlock()
	for id, auth := range m.auths {
		if _, exists := out[id]; exists || m.authAllowsClaudeContextRequest(auth, model, opts) {
			continue
		}
		if !copied {
			out = make(map[string]struct{}, len(tried)+1)
			for key := range tried {
				out[key] = struct{}{}
			}
			copied = true
		}
		out[id] = struct{}{}
	}
	return out
}
