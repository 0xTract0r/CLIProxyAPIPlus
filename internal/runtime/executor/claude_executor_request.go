package executor

import (
	"bufio"
	"bytes"
	"compress/flate"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"

	"github.com/andybalholm/brotli"
	"github.com/google/uuid"
	"github.com/klauspost/compress/zstd"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/misc"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/gin-gonic/gin"
)

// extractAndRemoveBetas extracts the "betas" array from the body and removes it.
// Returns the extracted betas as a string slice and the modified body.
func extractAndRemoveBetas(body []byte) ([]string, []byte) {
	betasResult := gjson.GetBytes(body, "betas")
	if !betasResult.Exists() {
		return nil, body
	}
	var betas []string
	if betasResult.IsArray() {
		for _, item := range betasResult.Array() {
			if s := strings.TrimSpace(item.String()); s != "" {
				betas = append(betas, s)
			}
		}
	} else if s := strings.TrimSpace(betasResult.String()); s != "" {
		betas = append(betas, s)
	}
	body, _ = sjson.DeleteBytes(body, "betas")
	return betas, body
}

// disableThinkingIfToolChoiceForced checks if tool_choice forces tool use and disables thinking.
// Anthropic API does not allow thinking when tool_choice is set to "any" or a specific tool.
// See: https://docs.anthropic.com/en/docs/build-with-claude/extended-thinking#important-considerations
func disableThinkingIfToolChoiceForced(body []byte) []byte {
	toolChoiceType := gjson.GetBytes(body, "tool_choice.type").String()
	// "auto" is allowed with thinking, but "any" or "tool" (specific tool) are not
	if toolChoiceType == "any" || toolChoiceType == "tool" {
		// Remove thinking configuration entirely to avoid API error
		body, _ = sjson.DeleteBytes(body, "thinking")
		// Adaptive thinking may also set output_config.effort; remove it to avoid
		// leaking thinking controls when tool_choice forces tool use.
		body, _ = sjson.DeleteBytes(body, "output_config.effort")
		if oc := gjson.GetBytes(body, "output_config"); oc.Exists() && oc.IsObject() && len(oc.Map()) == 0 {
			body, _ = sjson.DeleteBytes(body, "output_config")
		}
	}
	return body
}

// normalizeClaudeTemperatureForThinking keeps Anthropic message requests valid when
// thinking is enabled. Anthropic rejects temperatures other than 1 when
// thinking.type is enabled/adaptive/auto. Unlike the upstream
// normalizeClaudeSamplingForUpstream (which unconditionally strips
// temperature/top_p/top_k), this preserves the client's sampling fields to avoid
// a detectable fingerprint offset, only coercing temperature to 1 where the API
// requires it.
func normalizeClaudeTemperatureForThinking(body []byte) []byte {
	if !gjson.GetBytes(body, "temperature").Exists() {
		return body
	}

	thinkingType := strings.ToLower(strings.TrimSpace(gjson.GetBytes(body, "thinking.type").String()))
	switch thinkingType {
	case "enabled", "adaptive", "auto":
		if temp := gjson.GetBytes(body, "temperature"); temp.Exists() && temp.Type == gjson.Number && temp.Float() == 1 {
			return body
		}
		body, _ = sjson.SetBytes(body, "temperature", 1)
	}
	return body
}

// ensureClaudeThinkingDisplay defaults thinking.display to "summarized" when thinking
// is active and the client did not set display. Without this, Claude backends that
// enable redact-thinking return signature-only thinking blocks (empty thinking text).
// Explicit client values such as "omitted" are preserved.
func ensureClaudeThinkingDisplay(body []byte) []byte {
	thinkingType := strings.ToLower(strings.TrimSpace(gjson.GetBytes(body, "thinking.type").String()))
	switch thinkingType {
	case "enabled", "adaptive", "auto":
	default:
		return body
	}
	if display := strings.TrimSpace(gjson.GetBytes(body, "thinking.display").String()); display != "" {
		return body
	}
	out, err := sjson.SetBytes(body, "thinking.display", "summarized")
	if err != nil {
		return body
	}
	return out
}

type compositeReadCloser struct {
	io.Reader
	closers []func() error
}

func (c *compositeReadCloser) Close() error {
	var firstErr error
	for i := range c.closers {
		if c.closers[i] == nil {
			continue
		}
		if err := c.closers[i](); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// peekableBody wraps a bufio.Reader around the original ReadCloser so that
// magic bytes can be inspected without consuming them from the stream.
type peekableBody struct {
	*bufio.Reader
	closer io.Closer
}

func (p *peekableBody) Close() error {
	return p.closer.Close()
}

func decodeResponseBody(body io.ReadCloser, contentEncoding string) (io.ReadCloser, error) {
	if body == nil {
		return nil, fmt.Errorf("response body is nil")
	}
	if contentEncoding == "" {
		// No Content-Encoding header.  Attempt best-effort magic-byte detection to
		// handle misbehaving upstreams that compress without setting the header.
		// Only gzip (1f 8b) and zstd (28 b5 2f fd) have reliable magic sequences;
		// br and deflate have none and are left as-is.
		// The bufio wrapper preserves unread bytes so callers always see the full
		// stream regardless of whether decompression was applied.
		pb := &peekableBody{Reader: bufio.NewReader(body), closer: body}
		magic, peekErr := pb.Peek(4)
		if peekErr == nil || (peekErr == io.EOF && len(magic) >= 2) {
			switch {
			case len(magic) >= 2 && magic[0] == 0x1f && magic[1] == 0x8b:
				gzipReader, gzErr := gzip.NewReader(pb)
				if gzErr != nil {
					_ = pb.Close()
					return nil, fmt.Errorf("magic-byte gzip: failed to create reader: %w", gzErr)
				}
				return &compositeReadCloser{
					Reader: gzipReader,
					closers: []func() error{
						gzipReader.Close,
						pb.Close,
					},
				}, nil
			case len(magic) >= 4 && magic[0] == 0x28 && magic[1] == 0xb5 && magic[2] == 0x2f && magic[3] == 0xfd:
				decoder, zdErr := zstd.NewReader(pb)
				if zdErr != nil {
					_ = pb.Close()
					return nil, fmt.Errorf("magic-byte zstd: failed to create reader: %w", zdErr)
				}
				return &compositeReadCloser{
					Reader: decoder,
					closers: []func() error{
						func() error { decoder.Close(); return nil },
						pb.Close,
					},
				}, nil
			}
		}
		return pb, nil
	}
	encodings := strings.Split(contentEncoding, ",")
	for _, raw := range encodings {
		encoding := strings.TrimSpace(strings.ToLower(raw))
		switch encoding {
		case "", "identity":
			continue
		case "gzip":
			gzipReader, err := gzip.NewReader(body)
			if err != nil {
				_ = body.Close()
				return nil, fmt.Errorf("failed to create gzip reader: %w", err)
			}
			return &compositeReadCloser{
				Reader: gzipReader,
				closers: []func() error{
					gzipReader.Close,
					func() error { return body.Close() },
				},
			}, nil
		case "deflate":
			deflateReader := flate.NewReader(body)
			return &compositeReadCloser{
				Reader: deflateReader,
				closers: []func() error{
					deflateReader.Close,
					func() error { return body.Close() },
				},
			}, nil
		case "br":
			return &compositeReadCloser{
				Reader: brotli.NewReader(body),
				closers: []func() error{
					func() error { return body.Close() },
				},
			}, nil
		case "zstd":
			decoder, err := zstd.NewReader(body)
			if err != nil {
				_ = body.Close()
				return nil, fmt.Errorf("failed to create zstd reader: %w", err)
			}
			return &compositeReadCloser{
				Reader: decoder,
				closers: []func() error{
					func() error { decoder.Close(); return nil },
					func() error { return body.Close() },
				},
			}, nil
		default:
			continue
		}
	}
	return body, nil
}

func authAttrs(auth *cliproxyauth.Auth) map[string]string {
	if auth == nil {
		return nil
	}
	return auth.Attributes
}

var claudeManagedHeaderNames = []string{
	"User-Agent",
	"X-App",
	"X-Stainless-Package-Version",
	"X-Stainless-Runtime-Version",
	"X-Stainless-Timeout",
}

func applyClaudeManagedHeaders(r *http.Request, auth *cliproxyauth.Auth, snapshot map[string]string) {
	if r == nil || auth == nil {
		return
	}
	if cliproxyauth.HasStructuredAccountSettingsMetadata(auth) {
		applyManagedHeaderSnapshot(r.Header, snapshot)
		return
	}
	for _, headerName := range claudeManagedHeaderNames {
		// X-App is a low-entropy de-anonymization anchor: real claude-cli always
		// sends "cli". The structured path keeps it pinned to "cli" (its snapshot
		// is captured after the forced Set above), so the non-structured path must
		// match and never let an operator header:X-App override leak a non-cli
		// value. Skip it here; it is re-pinned to "cli" below. Other managed
		// headers may still be overridden by the operator.
		if strings.EqualFold(headerName, "X-App") {
			continue
		}
		if value := claudeManagedHeaderValue(auth, headerName); value != "" {
			r.Header.Set(headerName, value)
		}
	}
	// Re-pin X-App to "cli" only when the operator actually configured a
	// header:X-App override: ApplyCustomHeadersFromAttrs (run before this) would
	// otherwise have applied that override (e.g. "browser") to the outgoing
	// request, which must not survive on the de-anonymization anchor. When no
	// header:X-App override exists, leave the current X-App untouched so callers
	// that do not pre-force "cli" (e.g. PrepareRequest passthrough) keep their
	// prior behavior. This matches the structured snapshot path, where the
	// snapshot is captured after X-App was forced to "cli".
	if strings.TrimSpace(claudeManagedHeaderValue(auth, "X-App")) != "" {
		r.Header.Set("X-App", "cli")
	}
}

func claudeManagedHeaderValue(auth *cliproxyauth.Auth, headerName string) string {
	if auth == nil {
		return ""
	}
	if value := claudeManagedHeaderValueFromMetadata(auth.Metadata, headerName); value != "" {
		return value
	}
	return claudeManagedHeaderValueFromAttrs(auth.Attributes, headerName)
}

func claudeManagedHeaderValueFromMetadata(metadata map[string]any, headerName string) string {
	if len(metadata) == 0 {
		return ""
	}
	rawHeaders, ok := metadata["headers"]
	if !ok {
		return ""
	}
	switch headers := rawHeaders.(type) {
	case map[string]string:
		for key, value := range headers {
			if strings.EqualFold(key, headerName) {
				return strings.TrimSpace(value)
			}
		}
		return ""
	case map[string]any:
		for key, rawValue := range headers {
			if !strings.EqualFold(key, headerName) {
				continue
			}
			if value, ok := rawValue.(string); ok {
				return strings.TrimSpace(value)
			}
		}
		return ""
	default:
		return ""
	}
}

func claudeManagedHeaderValueFromAttrs(attrs map[string]string, headerName string) string {
	if len(attrs) == 0 {
		return ""
	}
	targetKey := "header:" + headerName
	for key, value := range attrs {
		if !strings.EqualFold(key, targetKey) {
			continue
		}
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func ginHeadersFromContext(ctx context.Context) http.Header {
	ginCtx := ginContextFromContext(ctx)
	if ginCtx != nil && ginCtx.Request != nil {
		return ginCtx.Request.Header
	}
	if headers, ok := ctx.Value(claudeInboundHeadersContextKey).(http.Header); ok {
		return headers
	}
	return nil
}

func ginContextFromContext(ctx context.Context) *gin.Context {
	if ginCtx, ok := ctx.Value("gin").(*gin.Context); ok && ginCtx != nil {
		return ginCtx
	}
	return nil
}

const claudeDeviceProfileContextKey = "claude_device_profile"

type claudeInboundHeadersContextKeyType struct{}

var claudeInboundHeadersContextKey = claudeInboundHeadersContextKeyType{}

func contextWithClaudeInboundHeaders(ctx context.Context, headers http.Header) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if ginContextFromContext(ctx) != nil || len(headers) == 0 {
		return ctx
	}
	return context.WithValue(ctx, claudeInboundHeadersContextKey, headers.Clone())
}

var claudeDeviceProfileStaleGuardWarnOnce sync.Once

// warnClaudeDeviceProfileStaleGuard emits a single operator-facing warning when
// the runtime is in the only remaining stale-prone state under the high-water
// model: stabilize on, no operator baseline UA, and no real first-party
// claude-cli version observed on any account yet. In that narrow window the floor
// is the frozen hardcoded claude-cli version constant until the first real client
// is seen. Enabling online-update is NOT a remedy here because npm latest is no
// longer used as a ceiling (it could claim a version no real client has sent);
// the guard self-heals once any real first-party client is observed.
func warnClaudeDeviceProfileStaleGuard(cfg *config.Config) {
	if !helps.ClaudeDeviceProfileStaleGuardActive(cfg) {
		return
	}
	claudeDeviceProfileStaleGuardWarnOnce.Do(func() {
		log.Warn("claude device profile: stabilize-device-profile is enabled, no claude-header-defaults.user-agent baseline is configured, and no real claude-cli client has been observed yet; the device fingerprint falls back to a frozen built-in version constant until the first real client is seen. Set claude-header-defaults.user-agent to a current claude-cli version to provide an explicit floor, or send one real first-party claude-cli request to seed the observed high-water mark.")
	})
}

func resolveClaudeDeviceProfileForRequest(ctx context.Context, auth *cliproxyauth.Auth, apiKey string, headers http.Header, cfg *config.Config) helps.ClaudeDeviceProfile {
	// Re-seed the in-memory observation map from this auth's persisted
	// high-water before the stale-guard predicate runs. After a restart the
	// in-memory observation map is empty while auth.Metadata still carries the
	// persisted high-water triple; the outbound floor path already reads that
	// triple directly (so the outbound UA is correct), but the stale-guard
	// warning predicate only inspects the in-memory map and would otherwise emit
	// a misleading "falls back to frozen floor" warning on the first request.
	// Seeding aligns the warning's view with the disk/outbound view without
	// changing outbound timing; it is only-up (the persisted triple was already
	// sanity-validated and the global high-water always takes the max).
	helps.SeedClaudeObservedHighWaterFromAuth(auth)
	warnClaudeDeviceProfileStaleGuard(cfg)
	ginCtx := ginContextFromContext(ctx)
	if ginCtx != nil {
		if cached, ok := ginCtx.Get(claudeDeviceProfileContextKey); ok {
			if profile, okProfile := cached.(helps.ClaudeDeviceProfile); okProfile {
				return profile
			}
		}
	}
	profile := helps.ResolveClaudeDeviceProfile(auth, apiKey, headers, cfg)
	if ginCtx != nil {
		ginCtx.Set(claudeDeviceProfileContextKey, profile)
	}
	return profile
}

func resolveClaudeBillingVersion(ctx context.Context, cfg *config.Config, auth *cliproxyauth.Auth, apiKey string) string {
	if auth != nil && !cliproxyauth.HasStructuredAccountSettingsMetadata(auth) {
		if version, ok := helps.ClaudeVersionFromUserAgent(claudeManagedHeaderValue(auth, "User-Agent")); ok {
			return version
		}
	}

	ginHeaders := ginHeadersFromContext(ctx)
	if version := resolveClaudeDeviceProfileForRequest(ctx, auth, apiKey, ginHeaders, cfg).VersionString(); version != "" {
		return version
	}

	if version, ok := helps.ClaudeVersionFromUserAgent(strings.TrimSpace(ginHeaders.Get("User-Agent"))); ok {
		return version
	}
	return helps.DefaultClaudeVersion(cfg)
}

// applyClaudeManagedProtocolHeaders sets the managed Anthropic/stainless-SDK
// protocol headers real claude-cli always sends: Anthropic-Version, X-App, the
// stainless client fingerprint (lang/runtime/retry-count/timeout), a fresh
// per-request client id (first-party api.anthropic.com only), and
// Connection: keep-alive. It is shared by two callers:
//
//   - applyClaudeHeaders, used by the real /v1/messages serving path
//     (Execute/ExecuteStream/CountTokens).
//   - ClaudeExecutor.PrepareRequest, used by the background quota/oauth
//     snapshot lookups (GET /api/oauth/profile, /api/oauth/usage via
//     exec.HttpRequest in quota_snapshots.go). Before this extraction,
//     PrepareRequest only applied the 5 device-profile headers
//     (UA/package-version/runtime-version/os/arch) via
//     ApplyClaudeDeviceProfileHeaders, so quota egress carried a
//     distinguishable "half-managed" header set compared to real serving.
//
// includeSessionID controls whether X-Claude-Code-Session-Id is attached. Real
// serving always passes true (a client session exists). The quota/oauth path
// is a sessionless background lookup with no client session context and must
// pass false: attaching a session id there would itself become a new
// cross-account correlation anchor, not a fingerprint fix.
//
// isAnthropicBase gates x-client-request-id (first-party API only, matching
// real claude-cli) and must be computed by the caller from the actual request
// host — PrepareRequest is a generic RequestPreparer hook that can in principle
// be reached for non-Anthropic base_url/proxy targets, so this function must
// never assume every caller targets api.anthropic.com.
//
// X-App is a low-entropy A-class identity field: real claude-cli always sends
// "cli". It is forced (Set, not EnsureHeader) so a client-supplied or
// operator-configured X-App override (e.g. "browser") can never leak through
// and de-anonymize the account; both callers snapshot/reapply managed headers
// afterward (applyClaudeManagedHeaders) and re-pin it the same way.
func applyClaudeManagedProtocolHeaders(r *http.Request, ginHeaders http.Header, cfg *config.Config, apiKey string, isAnthropicBase bool, includeSessionID bool) {
	hdrDefault := func(cfgVal, fallback string) string {
		if cfgVal != "" {
			return cfgVal
		}
		return fallback
	}
	var hd config.ClaudeHeaderDefaults
	if cfg != nil {
		hd = cfg.ClaudeHeaderDefaults
	}
	misc.EnsureHeader(r.Header, ginHeaders, "Anthropic-Version", "2023-06-01")
	r.Header.Set("X-App", "cli")
	misc.EnsureHeader(r.Header, ginHeaders, "X-Stainless-Retry-Count", "0")
	misc.EnsureHeader(r.Header, ginHeaders, "X-Stainless-Runtime", "node")
	misc.EnsureHeader(r.Header, ginHeaders, "X-Stainless-Lang", "js")
	misc.EnsureHeader(r.Header, ginHeaders, "X-Stainless-Timeout", hdrDefault(hd.Timeout, "600"))
	if includeSessionID {
		// Session ID: stable per auth/apiKey, matches Claude Code's X-Claude-Code-Session-Id header.
		misc.EnsureHeader(r.Header, ginHeaders, "X-Claude-Code-Session-Id", helps.CachedSessionID(apiKey))
	}
	// Per-request UUID, matches Claude Code's x-client-request-id for first-party API.
	if isAnthropicBase {
		misc.EnsureHeader(r.Header, ginHeaders, "x-client-request-id", uuid.New().String())
	}
	r.Header.Set("Connection", "keep-alive")
}

func applyClaudeHeaders(r *http.Request, auth *cliproxyauth.Auth, apiKey string, stream bool, extraBetas []string, cfg *config.Config) {
	useAPIKey := auth != nil && auth.Attributes != nil && strings.TrimSpace(auth.Attributes["api_key"]) != ""
	isAnthropicBase := r.URL != nil && strings.EqualFold(r.URL.Scheme, "https") && strings.EqualFold(r.URL.Host, "api.anthropic.com")
	if isAnthropicBase && useAPIKey {
		r.Header.Del("Authorization")
		r.Header.Set("x-api-key", apiKey)
	} else {
		r.Header.Set("Authorization", "Bearer "+apiKey)
	}
	r.Header.Set("Content-Type", "application/json")

	ginHeaders := ginHeadersFromContext(r.Context())
	stabilizeDeviceProfile := helps.ClaudeDeviceProfileStabilizationEnabled(cfg)
	deviceProfile := resolveClaudeDeviceProfileForRequest(r.Context(), auth, apiKey, ginHeaders, cfg)

	// baseBetas is the manually maintained floor, aligned to the real
	// claude-cli anthropic-beta set captured in
	// docs/fingerprint/cpa-reqs/04-traffic-ref.md (claude-cli/2.1.158):
	//   claude-code-20250219, context-1m-2025-08-07,
	//   interleaved-thinking-2025-05-14, thinking-token-count-2026-05-13,
	//   context-management-2025-06-27, prompt-caching-scope-2026-01-05,
	//   mid-conversation-system-2026-04-07.
	// We intentionally do NOT inject betas real claude-cli never sends
	// (e.g. structured-outputs / fast-mode / redact-thinking /
	// token-efficient-tools) because injecting stale/foreign betas can
	// corrupt tool_use JSON on newer models (OmniRoute #3415). We also do
	// NOT synthesize context-1m-2025-08-07 here: Claude 1M is GA and Claude
	// Code selects it via the model suffix, not a beta flag. oauth-2025-04-20
	// is kept as a strong-fill because it is required by the OAuth path and
	// is not visible in the cpa-mediated capture.
	baseBetas := "claude-code-20250219,oauth-2025-04-20,interleaved-thinking-2025-05-14,thinking-token-count-2026-05-13,context-management-2025-06-27,prompt-caching-scope-2026-01-05,mid-conversation-system-2026-04-07"

	// Union baseBetas with the client's real anthropic-beta set instead of
	// replacing it. Replacing would drop baseBetas-only floor entries when a
	// client sends a narrower set; union keeps the per-account beta set
	// monotonically non-decreasing (only-up, never-down). Client-only betas
	// are preserved so the header stays self-consistent with the forwarded
	// body capabilities.
	betaSet := make(map[string]bool)
	appendBeta := func(list string) {
		for _, b := range strings.Split(list, ",") {
			betaName := strings.TrimSpace(b)
			if betaName != "" && !betaSet[betaName] {
				betaSet[betaName] = true
				if baseBetas == "" {
					baseBetas = betaName
				} else {
					baseBetas += "," + betaName
				}
			}
		}
	}
	// Seed the set with the floor that is already in baseBetas.
	for _, b := range strings.Split(baseBetas, ",") {
		if betaName := strings.TrimSpace(b); betaName != "" {
			betaSet[betaName] = true
		}
	}
	if val := strings.TrimSpace(ginHeaders.Get("Anthropic-Beta")); val != "" {
		appendBeta(val)
	}
	if !betaSet["oauth-2025-04-20"] {
		appendBeta("oauth-2025-04-20")
	}
	if !strings.Contains(baseBetas, "interleaved-thinking") {
		appendBeta("interleaved-thinking-2025-05-14")
	}

	// Merge extra betas from request body. Do not synthesize the removed
	// context-1m beta; Claude 1M is GA and Claude Code uses model suffixes.
	if len(extraBetas) > 0 {
		for _, beta := range extraBetas {
			appendBeta(beta)
		}
	}
	r.Header.Set("Anthropic-Beta", baseBetas)

	// Only set browser access header for API key mode; real Claude Code CLI does not send it.
	if useAPIKey {
		misc.EnsureHeader(r.Header, ginHeaders, "Anthropic-Dangerous-Direct-Browser-Access", "true")
	}
	applyClaudeManagedProtocolHeaders(r, ginHeaders, cfg, apiKey, isAnthropicBase, true)
	if stream {
		r.Header.Set("Accept", "text/event-stream")
		r.Header.Set("Accept-Encoding", "identity")
	} else {
		r.Header.Set("Accept", "application/json")
		r.Header.Set("Accept-Encoding", "gzip, deflate, br, zstd")
	}
	// Legacy mode keeps OS/Arch runtime-derived; stabilized mode pins OS/Arch
	// to the configured baseline while still allowing newer official
	// User-Agent/package/runtime tuples to upgrade the software fingerprint.
	if stabilizeDeviceProfile {
		helps.ApplyClaudeDeviceProfileHeaders(r, deviceProfile)
		// Align the outbound UA parenthetical suffix "(USER_TYPE, ENTRYPOINT)" with
		// the inbound claude-code client UA, keeping the high-water
		// "claude-cli/<version>" prefix. cc_entrypoint is derived from the same
		// inbound UA (parseEntrypointFromUA(getClientUserAgent(ctx))), so without
		// this the frozen device-profile UA suffix (which one "claude --print" can
		// seed to "sdk-cli") can diverge from cc_entrypoint and produce a
		// UA/entrypoint pair real claude-code never emits. ginHeaders.Get reads the
		// same inbound request header source as getClientUserAgent.
		helps.AlignClaudeDeviceProfileUserAgentSuffix(cfg, r, ginHeaders.Get("User-Agent"))
	} else {
		helps.ApplyClaudeLegacyDeviceHeaders(r, ginHeaders, cfg)
	}
	managedHeaderSnapshot := captureManagedHeaderSnapshot(r.Header, claudeManagedHeaderNames)
	util.ApplyCustomHeadersFromAttrs(r, authAttrs(auth))
	applyClaudeManagedHeaders(r, auth, managedHeaderSnapshot)
	if stream {
		r.Header.Set("Accept-Encoding", "identity")
	}
}

func claudeCreds(a *cliproxyauth.Auth) (apiKey, baseURL string) {
	if a == nil {
		return "", ""
	}
	if a.Attributes != nil {
		apiKey = a.Attributes["api_key"]
		baseURL = a.Attributes["base_url"]
	}
	if apiKey == "" && a.Metadata != nil {
		if v, ok := a.Metadata["access_token"].(string); ok {
			apiKey = v
		}
	}
	return
}

func checkSystemInstructions(payload []byte) []byte {
	return checkSystemInstructionsWithSigningMode(payload, false, false, false, "2.1.63", "", "")
}

func rebuildMidSystemMessagesToTopLevel(payload []byte) []byte {
	messages := gjson.GetBytes(payload, "messages")
	if !messages.IsArray() {
		return payload
	}

	var movedSystemParts []string
	keptMessages := make([]string, 0, int(messages.Get("#").Int()))
	messages.ForEach(func(_, message gjson.Result) bool {
		if strings.EqualFold(strings.TrimSpace(message.Get("role").String()), "system") {
			movedSystemParts = append(movedSystemParts, claudeSystemTextParts(message.Get("content"))...)
			return true
		}
		keptMessages = append(keptMessages, message.Raw)
		return true
	})
	if len(movedSystemParts) == 0 {
		return payload
	}

	systemParts := claudeSystemTextParts(gjson.GetBytes(payload, "system"))
	systemParts = append(systemParts, movedSystemParts...)
	if len(systemParts) > 0 {
		if updated, errSetSystem := sjson.SetRawBytes(payload, "system", rawJSONArray(systemParts)); errSetSystem == nil {
			payload = updated
		}
	}
	if updated, errSetMessages := sjson.SetRawBytes(payload, "messages", rawJSONArray(keptMessages)); errSetMessages == nil {
		payload = updated
	}
	return payload
}

func claudeSystemTextParts(content gjson.Result) []string {
	if !content.Exists() {
		return nil
	}
	if content.Type == gjson.String {
		text := content.String()
		if strings.TrimSpace(text) == "" {
			return nil
		}
		block := []byte(`{"type":"text","text":""}`)
		block, _ = sjson.SetBytes(block, "text", text)
		return []string{string(block)}
	}
	if !content.IsArray() {
		return nil
	}

	var parts []string
	content.ForEach(func(_, item gjson.Result) bool {
		if item.Type == gjson.String {
			text := item.String()
			if strings.TrimSpace(text) != "" {
				block := []byte(`{"type":"text","text":""}`)
				block, _ = sjson.SetBytes(block, "text", text)
				parts = append(parts, string(block))
			}
			return true
		}
		if item.IsObject() && item.Get("type").String() == "text" && strings.TrimSpace(item.Get("text").String()) != "" {
			parts = append(parts, item.Raw)
		}
		return true
	})
	return parts
}

func rawJSONArray(items []string) []byte {
	if len(items) == 0 {
		return []byte("[]")
	}
	var builder strings.Builder
	builder.WriteByte('[')
	for i, item := range items {
		if i > 0 {
			builder.WriteByte(',')
		}
		builder.WriteString(item)
	}
	builder.WriteByte(']')
	return []byte(builder.String())
}

func isClaudeOAuthToken(apiKey string) bool {
	return strings.Contains(apiKey, "sk-ant-oat")
}

// prepareClaudeOAuthToolNamesForUpstream applies the Claude OAuth tool-name
// transforms in the same order across request paths. Remap runs before prefixing
// so any future non-empty prefix still composes correctly with the per-request
// reverse map.
func prepareClaudeOAuthToolNamesForUpstream(body []byte, prefix string, prefixDisabled bool) ([]byte, map[string]string) {
	body, reverseMap := remapOAuthToolNames(body)
	if !prefixDisabled {
		body = applyClaudeToolPrefix(body, prefix)
	}
	return body, reverseMap
}

// restoreClaudeOAuthToolNamesFromResponse undoes the Claude OAuth tool-name
// transforms for non-stream responses in reverse order.
func restoreClaudeOAuthToolNamesFromResponse(body []byte, prefix string, prefixDisabled bool, reverseMap map[string]string) []byte {
	if !prefixDisabled {
		body = stripClaudeToolPrefixFromResponse(body, prefix)
	}
	return reverseRemapOAuthToolNames(body, reverseMap)
}

// restoreClaudeOAuthToolNamesFromStreamLine undoes the Claude OAuth tool-name
// transforms for SSE lines in reverse order.
func restoreClaudeOAuthToolNamesFromStreamLine(line []byte, prefix string, prefixDisabled bool, reverseMap map[string]string) []byte {
	if !prefixDisabled {
		line = stripClaudeToolPrefixFromStreamLine(line, prefix)
	}
	return reverseRemapOAuthToolNamesFromStreamLine(line, reverseMap)
}

// remapOAuthToolNames renames third-party tool names to Claude Code equivalents
// and removes tools without an official counterpart. This prevents Anthropic from
// fingerprinting the request as a third-party client via tool naming patterns.
//
// It operates on: tools[].name, tool_choice.name, and all tool_use/tool_reference
// references in messages. Removed tools' corresponding tool_result blocks are preserved
// (they just become orphaned, which is safe for Claude).
//
// The returned map is keyed on the upstream (TitleCase) name and maps to the
// client-supplied original name. Callers MUST pass this map to the reverse
// functions so only names the client actually caused us to rewrite are restored
// on the response. A global reverse map (the previous implementation) incorrectly
// rewrote names the client originally sent in TitleCase (e.g. `Bash`)
// when any OTHER tool in the same request triggered a forward rename (e.g.
// `glob` -> `Glob`), because the global reverse map contained `Bash` -> `bash`
// regardless of what the client originally sent.
func remapOAuthToolNames(body []byte) ([]byte, map[string]string) {
	reverseMap := make(map[string]string, len(oauthToolRenameMap))
	recordRename := func(original, renamed string) {
		// Preserve the first-seen original name if the same upstream name is
		// produced from multiple call sites; they all map back identically.
		if _, exists := reverseMap[renamed]; !exists {
			reverseMap[renamed] = original
		}
	}

	// 1. Rewrite tools array in a single pass (if present).
	// IMPORTANT: do not mutate names first and then rebuild from an older gjson
	// snapshot. gjson results are snapshots of the original bytes; rebuilding from a
	// stale snapshot will preserve removals but overwrite renamed names back to their
	// original lowercase values.
	tools := gjson.GetBytes(body, "tools")
	toolsNeedRewrite := false
	if tools.Exists() && tools.IsArray() {
		tools.ForEach(func(_, tool gjson.Result) bool {
			if tool.Get("type").Exists() && tool.Get("type").String() != "" {
				return true
			}
			name := tool.Get("name").String()
			toolsNeedRewrite = oauthToolsToRemove[name]
			if !toolsNeedRewrite {
				newName, ok := oauthToolRenameMap[name]
				toolsNeedRewrite = ok && newName != name
			}
			return !toolsNeedRewrite
		})
	}
	if toolsNeedRewrite {
		var toolsJSON strings.Builder
		toolsJSON.WriteByte('[')
		toolCount := 0
		tools.ForEach(func(_, tool gjson.Result) bool {
			// Keep Anthropic built-in tools (web_search, code_execution, etc.) unchanged.
			if tool.Get("type").Exists() && tool.Get("type").String() != "" {
				if toolCount > 0 {
					toolsJSON.WriteByte(',')
				}
				toolsJSON.WriteString(tool.Raw)
				toolCount++
				return true
			}

			name := tool.Get("name").String()
			if oauthToolsToRemove[name] {
				return true
			}

			toolJSON := tool.Raw
			if newName, ok := oauthToolRenameMap[name]; ok && newName != name {
				updatedTool, err := sjson.Set(toolJSON, "name", newName)
				if err == nil {
					toolJSON = updatedTool
					recordRename(name, newName)
				}
			}

			if toolCount > 0 {
				toolsJSON.WriteByte(',')
			}
			toolsJSON.WriteString(toolJSON)
			toolCount++
			return true
		})
		toolsJSON.WriteByte(']')
		body, _ = sjson.SetRawBytes(body, "tools", []byte(toolsJSON.String()))
	}

	// 2. Rename tool_choice if it references a known tool
	toolChoiceType := gjson.GetBytes(body, "tool_choice.type").String()
	if toolChoiceType == "tool" {
		tcName := gjson.GetBytes(body, "tool_choice.name").String()
		if oauthToolsToRemove[tcName] {
			// The chosen tool was removed from the tools array, so drop tool_choice to
			// keep the payload internally consistent and fall back to normal auto tool use.
			body, _ = sjson.DeleteBytes(body, "tool_choice")
		} else if newName, ok := oauthToolRenameMap[tcName]; ok && newName != tcName {
			body, _ = sjson.SetBytes(body, "tool_choice.name", newName)
			recordRename(tcName, newName)
		}
	}

	// 3. Rename tool references in messages
	messages := gjson.GetBytes(body, "messages")
	if messages.Exists() && messages.IsArray() {
		messages.ForEach(func(msgIndex, msg gjson.Result) bool {
			content := msg.Get("content")
			if !content.Exists() || !content.IsArray() {
				return true
			}
			content.ForEach(func(contentIndex, part gjson.Result) bool {
				partType := part.Get("type").String()
				switch partType {
				case "tool_use":
					name := part.Get("name").String()
					if newName, ok := oauthToolRenameMap[name]; ok && newName != name {
						path := fmt.Sprintf("messages.%d.content.%d.name", msgIndex.Int(), contentIndex.Int())
						body, _ = sjson.SetBytes(body, path, newName)
						recordRename(name, newName)
					}
				case "tool_reference":
					toolName := part.Get("tool_name").String()
					if newName, ok := oauthToolRenameMap[toolName]; ok && newName != toolName {
						path := fmt.Sprintf("messages.%d.content.%d.tool_name", msgIndex.Int(), contentIndex.Int())
						body, _ = sjson.SetBytes(body, path, newName)
						recordRename(toolName, newName)
					}
				case "tool_result":
					// Handle nested tool_reference blocks inside tool_result.content[]
					toolID := part.Get("tool_use_id").String()
					_ = toolID // tool_use_id stays as-is
					nestedContent := part.Get("content")
					if nestedContent.Exists() && nestedContent.IsArray() {
						nestedContent.ForEach(func(nestedIndex, nestedPart gjson.Result) bool {
							if nestedPart.Get("type").String() == "tool_reference" {
								nestedToolName := nestedPart.Get("tool_name").String()
								if newName, ok := oauthToolRenameMap[nestedToolName]; ok && newName != nestedToolName {
									nestedPath := fmt.Sprintf("messages.%d.content.%d.content.%d.tool_name", msgIndex.Int(), contentIndex.Int(), nestedIndex.Int())
									body, _ = sjson.SetBytes(body, nestedPath, newName)
									recordRename(nestedToolName, newName)
								}
							}
							return true
						})
					}
				}
				return true
			})
			return true
		})
	}

	return body, reverseMap
}

// reverseRemapOAuthToolNames reverses the tool name mapping for non-stream responses
// using the per-request map produced by remapOAuthToolNames. Names the client sent
// that were NOT forward-renamed are passed through unchanged.
func reverseRemapOAuthToolNames(body []byte, reverseMap map[string]string) []byte {
	if len(reverseMap) == 0 {
		return body
	}
	content := gjson.GetBytes(body, "content")
	if !content.Exists() || !content.IsArray() {
		return body
	}
	content.ForEach(func(index, part gjson.Result) bool {
		partType := part.Get("type").String()
		switch partType {
		case "tool_use":
			name := part.Get("name").String()
			if origName, ok := reverseMap[name]; ok {
				path := fmt.Sprintf("content.%d.name", index.Int())
				body, _ = sjson.SetBytes(body, path, origName)
			}
		case "tool_reference":
			toolName := part.Get("tool_name").String()
			if origName, ok := reverseMap[toolName]; ok {
				path := fmt.Sprintf("content.%d.tool_name", index.Int())
				body, _ = sjson.SetBytes(body, path, origName)
			}
		}
		return true
	})
	return body
}

// reverseRemapOAuthToolNamesFromStreamLine reverses the tool name mapping for SSE
// stream lines, using the per-request reverseMap produced by remapOAuthToolNames.
func reverseRemapOAuthToolNamesFromStreamLine(line []byte, reverseMap map[string]string) []byte {
	if len(reverseMap) == 0 {
		return line
	}
	payload := helps.JSONPayload(line)
	if len(payload) == 0 || !gjson.ValidBytes(payload) {
		return line
	}

	contentBlock := gjson.GetBytes(payload, "content_block")
	if !contentBlock.Exists() {
		return line
	}

	blockType := contentBlock.Get("type").String()
	var updated []byte
	var err error

	switch blockType {
	case "tool_use":
		name := contentBlock.Get("name").String()
		if origName, ok := reverseMap[name]; ok {
			updated, err = sjson.SetBytes(payload, "content_block.name", origName)
			if err != nil {
				return line
			}
		} else {
			return line
		}
	case "tool_reference":
		toolName := contentBlock.Get("tool_name").String()
		if origName, ok := reverseMap[toolName]; ok {
			updated, err = sjson.SetBytes(payload, "content_block.tool_name", origName)
			if err != nil {
				return line
			}
		} else {
			return line
		}
	default:
		return line
	}

	trimmed := bytes.TrimSpace(line)
	if bytes.HasPrefix(trimmed, []byte("data:")) {
		return append([]byte("data: "), updated...)
	}
	return updated
}

func applyClaudeToolPrefix(body []byte, prefix string) []byte {
	if prefix == "" {
		return body
	}

	// Collect built-in tool names from the authoritative fallback seed list and
	// augment it with any typed built-ins present in the current request body.
	builtinTools := helps.AugmentClaudeBuiltinToolRegistry(body, nil)

	if tools := gjson.GetBytes(body, "tools"); tools.Exists() && tools.IsArray() {
		tools.ForEach(func(index, tool gjson.Result) bool {
			// Skip built-in tools (web_search, code_execution, etc.) which have
			// a "type" field and require their name to remain unchanged.
			if tool.Get("type").Exists() && tool.Get("type").String() != "" {
				if n := tool.Get("name").String(); n != "" {
					builtinTools[n] = true
				}
				return true
			}
			name := tool.Get("name").String()
			if name == "" || strings.HasPrefix(name, prefix) {
				return true
			}
			path := fmt.Sprintf("tools.%d.name", index.Int())
			body, _ = sjson.SetBytes(body, path, prefix+name)
			return true
		})
	}

	if gjson.GetBytes(body, "tool_choice.type").String() == "tool" {
		name := gjson.GetBytes(body, "tool_choice.name").String()
		if name != "" && !strings.HasPrefix(name, prefix) && !builtinTools[name] {
			body, _ = sjson.SetBytes(body, "tool_choice.name", prefix+name)
		}
	}

	if messages := gjson.GetBytes(body, "messages"); messages.Exists() && messages.IsArray() {
		messages.ForEach(func(msgIndex, msg gjson.Result) bool {
			content := msg.Get("content")
			if !content.Exists() || !content.IsArray() {
				return true
			}
			content.ForEach(func(contentIndex, part gjson.Result) bool {
				partType := part.Get("type").String()
				switch partType {
				case "tool_use":
					name := part.Get("name").String()
					if name == "" || strings.HasPrefix(name, prefix) || builtinTools[name] {
						return true
					}
					path := fmt.Sprintf("messages.%d.content.%d.name", msgIndex.Int(), contentIndex.Int())
					body, _ = sjson.SetBytes(body, path, prefix+name)
				case "tool_reference":
					toolName := part.Get("tool_name").String()
					if toolName == "" || strings.HasPrefix(toolName, prefix) || builtinTools[toolName] {
						return true
					}
					path := fmt.Sprintf("messages.%d.content.%d.tool_name", msgIndex.Int(), contentIndex.Int())
					body, _ = sjson.SetBytes(body, path, prefix+toolName)
				case "tool_result":
					// Handle nested tool_reference blocks inside tool_result.content[]
					nestedContent := part.Get("content")
					if nestedContent.Exists() && nestedContent.IsArray() {
						nestedContent.ForEach(func(nestedIndex, nestedPart gjson.Result) bool {
							if nestedPart.Get("type").String() == "tool_reference" {
								nestedToolName := nestedPart.Get("tool_name").String()
								if nestedToolName != "" && !strings.HasPrefix(nestedToolName, prefix) && !builtinTools[nestedToolName] {
									nestedPath := fmt.Sprintf("messages.%d.content.%d.content.%d.tool_name", msgIndex.Int(), contentIndex.Int(), nestedIndex.Int())
									body, _ = sjson.SetBytes(body, nestedPath, prefix+nestedToolName)
								}
							}
							return true
						})
					}
				}
				return true
			})
			return true
		})
	}

	return body
}

func stripClaudeToolPrefixFromResponse(body []byte, prefix string) []byte {
	if prefix == "" {
		return body
	}
	content := gjson.GetBytes(body, "content")
	if !content.Exists() || !content.IsArray() {
		return body
	}
	content.ForEach(func(index, part gjson.Result) bool {
		partType := part.Get("type").String()
		switch partType {
		case "tool_use":
			name := part.Get("name").String()
			if !strings.HasPrefix(name, prefix) {
				return true
			}
			path := fmt.Sprintf("content.%d.name", index.Int())
			body, _ = sjson.SetBytes(body, path, strings.TrimPrefix(name, prefix))
		case "tool_reference":
			toolName := part.Get("tool_name").String()
			if !strings.HasPrefix(toolName, prefix) {
				return true
			}
			path := fmt.Sprintf("content.%d.tool_name", index.Int())
			body, _ = sjson.SetBytes(body, path, strings.TrimPrefix(toolName, prefix))
		case "tool_result":
			// Handle nested tool_reference blocks inside tool_result.content[]
			nestedContent := part.Get("content")
			if nestedContent.Exists() && nestedContent.IsArray() {
				nestedContent.ForEach(func(nestedIndex, nestedPart gjson.Result) bool {
					if nestedPart.Get("type").String() == "tool_reference" {
						nestedToolName := nestedPart.Get("tool_name").String()
						if strings.HasPrefix(nestedToolName, prefix) {
							nestedPath := fmt.Sprintf("content.%d.content.%d.tool_name", index.Int(), nestedIndex.Int())
							body, _ = sjson.SetBytes(body, nestedPath, strings.TrimPrefix(nestedToolName, prefix))
						}
					}
					return true
				})
			}
		}
		return true
	})
	return body
}

func stripClaudeToolPrefixFromStreamLine(line []byte, prefix string) []byte {
	if prefix == "" {
		return line
	}
	payload := helps.JSONPayload(line)
	if len(payload) == 0 || !gjson.ValidBytes(payload) {
		return line
	}
	contentBlock := gjson.GetBytes(payload, "content_block")
	if !contentBlock.Exists() {
		return line
	}

	blockType := contentBlock.Get("type").String()
	var updated []byte
	var err error

	switch blockType {
	case "tool_use":
		name := contentBlock.Get("name").String()
		if !strings.HasPrefix(name, prefix) {
			return line
		}
		updated, err = sjson.SetBytes(payload, "content_block.name", strings.TrimPrefix(name, prefix))
		if err != nil {
			return line
		}
	case "tool_reference":
		toolName := contentBlock.Get("tool_name").String()
		if !strings.HasPrefix(toolName, prefix) {
			return line
		}
		updated, err = sjson.SetBytes(payload, "content_block.tool_name", strings.TrimPrefix(toolName, prefix))
		if err != nil {
			return line
		}
	default:
		return line
	}

	trimmed := bytes.TrimSpace(line)
	if bytes.HasPrefix(trimmed, []byte("data:")) {
		return append([]byte("data: "), updated...)
	}
	return updated
}
