package executor

import (
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/thinking"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// codexFastEnabled reports whether the Codex priority/fast Responses websocket flow
// is enabled for this credential AND this specific model. It is the per-account &
// per-model gate that parallels codexWebsocketsEnabled: fast is opt-in, never on by
// default, and implies the websocket transport (fast only works over the responses
// websocket, so a fast-enabled request is routed to the ws upstream even when the
// downstream is plain HTTP/SSE).
//
// The allowlist is sourced from auth.Attributes["fast_models"] (a normalized
// comma-separated list written by the config synthesizer) with a Metadata fallback for
// runtimes that carry it there. An entry of "*" enables every model for the account.
func codexFastEnabled(auth *cliproxyauth.Auth, model string) bool {
	if auth == nil {
		return false
	}
	model = strings.ToLower(strings.TrimSpace(model))
	if model == "" {
		return false
	}
	raw := ""
	if len(auth.Attributes) > 0 {
		raw = strings.TrimSpace(auth.Attributes["fast_models"])
	}
	if raw == "" && len(auth.Metadata) > 0 {
		if value, ok := auth.Metadata["fast_models"]; ok {
			switch typed := value.(type) {
			case string:
				raw = strings.TrimSpace(typed)
			case []string:
				raw = strings.Join(typed, ",")
			}
		}
	}
	if raw == "" {
		return false
	}
	for _, entry := range strings.Split(raw, ",") {
		entry = strings.ToLower(strings.TrimSpace(entry))
		if entry == "" {
			continue
		}
		if entry == "*" || entry == model {
			return true
		}
	}
	return false
}

// codexBaseModelName strips any thinking suffix (e.g. "gpt-5.6:high" -> "gpt-5.6")
// so the fast gate compares against the base model name.
func codexBaseModelName(model string) string {
	return thinking.ParseSuffix(model).ModelName
}

// codexFastSessionFallbackID derives a stable per-conversation execution session id
// for a Codex fast request that did not carry an explicit execution_session_id (the
// common case for plain HTTP/SSE downstream). Without a session id the ws executor
// dials and closes a fresh upstream connection per request, which cannot reuse the
// warm connection across turns. This fallback is applied ONLY on the codex fast path
// (see call sites gated by codexFastEnabled); it does not alter session handling for
// any other provider or for non-fast codex traffic.
//
// It prefers the fork's derived-session identity (already computed by session.Enrich
// from the request root) and falls back to the client prompt_cache_key, both of which
// are stable across turns of the same conversation. The "codex-fast:" prefix keeps
// this synthetic id in its own namespace, distinct from real downstream execution
// session ids.
func codexFastSessionFallbackID(opts cliproxyexecutor.Options, req cliproxyexecutor.Request) string {
	if derived := helps.DerivedSessionID(opts.Metadata, req.Metadata); derived != "" {
		return "codex-fast:" + derived
	}
	if promptCacheKey := strings.TrimSpace(gjson.GetBytes(req.Payload, "prompt_cache_key").String()); promptCacheKey != "" {
		return "codex-fast:pck:" + promptCacheKey
	}
	return ""
}

// applyCodexServiceTierPriority injects service_tier=priority into the outbound
// upstream request body. Both the prewarm frame and the main turn derive from the
// same upstream body, so this propagates to both. It is applied to the upstream body
// only (never the client-facing body) and only when fast is enabled.
func applyCodexServiceTierPriority(body []byte) []byte {
	if len(body) == 0 {
		return body
	}
	updated, err := sjson.SetBytes(body, "service_tier", "priority")
	if err != nil || len(updated) == 0 {
		return body
	}
	return updated
}
