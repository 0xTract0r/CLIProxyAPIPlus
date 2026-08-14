package executor

import (
	"bytes"
	"context"
	"fmt"
	"strings"

	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/thinking"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// codexFastPrewarmWarmupText is the neutral warmup message used by the generate:false
// prewarm frame. It keeps the frame valid and cheap even if the upstream ignores
// generate:false (it would then run on this neutral message, not double-run the real
// prompt). This mirrors the real-machine probe flow that reproduced the 1.55x priority
// speedup.
const codexFastPrewarmWarmupText = "<session warmup>"

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

// buildCodexWebsocketPrewarmBody builds the generate:false prewarm frame from the
// already-normalized upstream body. It reuses every identity-normalized field (model,
// instructions, client_metadata, prompt_cache_key, service_tier, reasoning, ...) so the
// prewarm inherits the SAME anti-correlation normalization as the main turn and never
// opens a second un-normalized outbound. Only the input is swapped for a minimal neutral
// warmup message, generate is forced false, and any inherited previous_response_id is
// dropped because the prewarm STARTS the turn chain.
func buildCodexWebsocketPrewarmBody(upstreamBody []byte) []byte {
	if len(upstreamBody) == 0 {
		return nil
	}
	body := bytes.Clone(upstreamBody)
	if updated, err := sjson.DeleteBytes(body, "previous_response_id"); err == nil {
		body = updated
	}
	warmupInput := []byte(`[{"type":"message","role":"user","content":[{"type":"input_text","text":"` + codexFastPrewarmWarmupText + `"}]}]`)
	if updated, err := sjson.SetRawBytes(body, "input", warmupInput); err == nil && len(updated) > 0 {
		body = updated
	}
	if updated, err := sjson.SetBytes(body, "generate", false); err == nil && len(updated) > 0 {
		body = updated
	}
	// buildCodexWebsocketRequestBody sets type=response.create and sanitizes input ids
	// (the warmup input has none, so sanitize is a no-op) and preserves generate:false.
	return buildCodexWebsocketRequestBody(body)
}

// buildCodexWebsocketFastMainBody builds the main turn frame linked to the prewarm
// response via previous_response_id (generate defaults true). It keeps the REAL user
// input from the upstream body and, when a prewarm id is present, overwrites any
// client-supplied previous_response_id with it — codex fast requires the main turn to
// link to the prewarm on the same connection.
//
// Note: codex-cli uses store:false and resends the full transcript each turn, so
// overwriting previous_response_id does not drop conversation context. Clients that
// rely on server-stored previous_response_id are out of scope for fast (Phase 5).
func buildCodexWebsocketFastMainBody(upstreamBody []byte, previousResponseID string) []byte {
	if len(upstreamBody) == 0 {
		return buildCodexWebsocketRequestBody(upstreamBody)
	}
	body := bytes.Clone(upstreamBody)
	// The main turn must generate; strip any stray generate flag inherited from the body.
	if updated, err := sjson.DeleteBytes(body, "generate"); err == nil {
		body = updated
	}
	if id := strings.TrimSpace(previousResponseID); id != "" {
		if updated, err := sjson.SetBytes(body, "previous_response_id", id); err == nil && len(updated) > 0 {
			body = updated
		}
	}
	return buildCodexWebsocketRequestBody(body)
}

// runCodexFastPrewarm sends the generate:false prewarm frame on the established upstream
// connection and reads until the upstream returns a terminal response, returning the
// real upstream response id used to link the main turn via previous_response_id
// (codex-rs responses websocket v2 turn semantics). Prewarm and main run on the SAME
// connection sequentially; only after the prewarm terminal is read does the caller send
// the main frame, so their frames never interleave.
//
// Fail-closed: any write/read/upstream error, or a terminal completion without a
// response id, aborts the fast turn with an error rather than silently downgrading to a
// path that would leak identity or mischarge.
func (e *CodexWebsocketsExecutor) runCodexFastPrewarm(
	ctx context.Context,
	sess *codexWebsocketSession,
	conn *websocket.Conn,
	readCh chan codexWebsocketRead,
	upstreamBody []byte,
	identityState codexIdentityConfuseState,
) (string, error) {
	prewarmBody := buildCodexWebsocketPrewarmBody(upstreamBody)
	if len(prewarmBody) == 0 {
		return "", fmt.Errorf("codex websockets executor: fast prewarm body is empty")
	}
	if errSend := writeCodexWebsocketMessage(sess, conn, prewarmBody); errSend != nil {
		return "", mapCodexWebsocketWriteError(sess, conn, errSend)
	}

	var prewarmID string
	for {
		if ctx != nil && ctx.Err() != nil {
			return "", ctx.Err()
		}
		msgType, payload, errRead := readCodexWebsocketMessage(ctx, sess, conn, readCh)
		if errRead != nil {
			return "", mapCodexWebsocketReadError(errRead)
		}
		if msgType != websocket.TextMessage {
			if msgType == websocket.BinaryMessage {
				return "", fmt.Errorf("codex websockets executor: unexpected binary message during fast prewarm")
			}
			continue
		}
		payload = bytes.TrimSpace(payload)
		if len(payload) == 0 {
			continue
		}
		// Extract the raw upstream response id BEFORE any identity transform: the
		// previous_response_id echoed upstream must be the exact id the server issued.
		if id := strings.TrimSpace(gjson.GetBytes(payload, "response.id").String()); id != "" {
			prewarmID = id
		}
		if wsErr, ok := parseCodexWebsocketError(payload); ok {
			return "", wsErr
		}
		if streamErr, _, ok := codexTerminalFailureErr(payload); ok {
			return "", streamErr
		}
		eventType := gjson.GetBytes(payload, "type").String()
		// Mirror the main loop's logging discipline (log the confused view, never the
		// real identity fields).
		helps.AppendAPIWebsocketResponse(ctx, e.cfg, applyCodexIdentityConfuseResponsePayload(payload, identityState))
		switch eventType {
		case "response.completed", "response.done":
			if prewarmID == "" {
				return "", fmt.Errorf("codex websockets executor: fast prewarm completed without a response id")
			}
			return prewarmID, nil
		}
	}
}
