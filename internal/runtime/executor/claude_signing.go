package executor

import (
	"fmt"
	"regexp"
	"strings"

	xxHash64 "github.com/pierrec/xxHash/xxHash64"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const claudeCCHSeed uint64 = 0x6E52736AC806831E

var claudeBillingHeaderCCHPattern = regexp.MustCompile(`\bcch=([0-9a-f]{5});`)

// claudeSdkCliEntrypoint / claudeNormalizedCliEntrypoint are the self-reported
// entrypoint tokens folded by config.NormalizeSdkCliEntrypointEnabled: "sdk-cli"
// (Claude Agent SDK / `claude -p` non-interactive invocations, disallowed by
// Anthropic policy against subscription OAuth) is folded to "cli" (what real
// interactive claude-cli always emits). Shared by parseEntrypointFromUA (the
// UA-derived cloak / count_tokens cc_entrypoint) and normalizeClaudeBillingEntrypoint
// (the verbatim /v1/messages cc_entrypoint) so every fold uses identical tokens.
const (
	claudeSdkCliEntrypoint        = "sdk-cli"
	claudeNormalizedCliEntrypoint = "cli"
)

// claudeBillingHeaderEntrypointPattern captures the cc_entrypoint value of an
// x-anthropic-billing-header text block, e.g. "sdk-cli" in
// "...; cc_entrypoint=sdk-cli; cch=...".
var claudeBillingHeaderEntrypointPattern = regexp.MustCompile(`cc_entrypoint=([^;]*);`)

func signAnthropicMessagesBody(body []byte) []byte {
	billingHeader := gjson.GetBytes(body, "system.0.text").String()
	if !strings.HasPrefix(billingHeader, "x-anthropic-billing-header:") {
		return body
	}
	if !claudeBillingHeaderCCHPattern.MatchString(billingHeader) {
		return body
	}

	unsignedBillingHeader := claudeBillingHeaderCCHPattern.ReplaceAllString(billingHeader, "cch=00000;")
	unsignedBody, err := sjson.SetBytes(body, "system.0.text", unsignedBillingHeader)
	if err != nil {
		return body
	}

	cch := fmt.Sprintf("%05x", xxHash64.Checksum(unsignedBody, claudeCCHSeed)&0xFFFFF)
	signedBillingHeader := claudeBillingHeaderCCHPattern.ReplaceAllString(unsignedBillingHeader, "cch="+cch+";")
	signedBody, err := sjson.SetBytes(unsignedBody, "system.0.text", signedBillingHeader)
	if err != nil {
		return unsignedBody
	}
	return signedBody
}

// normalizeClaudeBillingEntrypoint folds a "sdk-cli" cc_entrypoint inside the
// body's x-anthropic-billing-header (system[0].text) into "cli" when
// config.NormalizeSdkCliEntrypointEnabled(cfg) is true (the default), returning
// the (possibly rewritten) body and whether a fold occurred.
//
// This is the /v1/messages (+ streaming) counterpart of the sdk-cli→cli fold
// applied to the outbound UA parenthetical suffix
// (helps.AlignClaudeDeviceProfileUserAgentSuffix) and to the count_tokens /
// cloak cc_entrypoint (parseEntrypointFromUA). Real interactive claude-cli
// clients bypass cloak system-block regeneration (helps.ShouldCloak is false in
// the default "auto" mode), so a `claude -p` / Agent SDK invocation's inbound
// billing header — self-tagged cc_entrypoint=sdk-cli — is otherwise forwarded
// verbatim by signAnthropicMessagesBody (which only recomputes cch). Without
// this fold the outbound UA suffix (folded to cli) and the body cc_entrypoint
// (sdk-cli) would diverge, a mismatch real claude-code never emits.
//
// Callers must re-sign (signAnthropicMessagesBody) after a reported fold so the
// recomputed cch covers the rewritten body. When disabled, when system[0].text
// is not a billing header, or when cc_entrypoint is not exactly "sdk-cli", the
// body is returned unchanged with changed=false, preserving the previous
// verbatim-mirror behavior (rollback path).
func normalizeClaudeBillingEntrypoint(cfg *config.Config, body []byte) ([]byte, bool) {
	if !config.NormalizeSdkCliEntrypointEnabled(cfg) {
		return body, false
	}
	billingHeader := gjson.GetBytes(body, "system.0.text").String()
	if !strings.HasPrefix(billingHeader, "x-anthropic-billing-header:") {
		return body, false
	}
	folded := claudeBillingHeaderEntrypointPattern.ReplaceAllStringFunc(billingHeader, func(match string) string {
		sub := claudeBillingHeaderEntrypointPattern.FindStringSubmatch(match)
		if len(sub) != 2 || strings.TrimSpace(sub[1]) != claudeSdkCliEntrypoint {
			return match
		}
		return "cc_entrypoint=" + claudeNormalizedCliEntrypoint + ";"
	})
	if folded == billingHeader {
		return body, false
	}
	updated, err := sjson.SetBytes(body, "system.0.text", folded)
	if err != nil {
		return body, false
	}
	return updated, true
}

// alignRealPathBillingVersion rewrites the body's x-anthropic-billing-header
// (system[0].text) cc_version=<version>.<build> token so BOTH segments match the
// account high-water version V: the <version> segment is set to billingVersion
// (V) and the <build> segment is RECOMPUTED as computeFingerprint(firstUserMsg,
// V) over the first non-meta user message in the final outgoing body. Every other
// billing field is preserved byte-for-byte. It returns the (possibly rewritten)
// body plus whether a rewrite occurred.
//
// This is the REAL serving path (helps.ShouldCloak == false, genuine
// interactive claude-cli) counterpart of the cc_version floor the cloaked path
// already applies inside checkSystemInstructionsWithSigningMode. On the real
// path applyCloaking early-returns before that floor, so a below-high-water
// client would send an outbound User-Agent floored up to V (the header side,
// done in the device-profile floor) while its body cc_version stays at the
// lower client version — a "one account, two versions" mismatch. Aligning the
// body version to the same V the UA uses closes that gap. It is the /v1/messages
// (+ streaming) sibling of normalizeClaudeBillingEntrypoint (which folds
// cc_entrypoint); both rewrite the verbatim inbound billing header and require
// the caller to re-sign afterward.
//
// The <build> is RECOMPUTED (not passed through) because Claude Code's build is a
// deterministic function of (first user message, version): a client floored from
// v to V must emit the build V would produce over the same first user message,
// not the build it computed for v. Validated against genuine claude-cli 2.1.220
// captures (Stage C account-free capture) — the build is sha256(salt + msg[4] +
// msg[7] + msg[20] + version)[:3] over the first non-meta user message, indexed
// by UTF-16 code units (see computeFingerprint). Because alignRealPathBillingVersion
// runs at the final re-sign point (after sanitize / oauth tool rename / entrypoint
// fold), the body it hashes is the final outgoing state — the same bytes genuine
// Claude Code hashes. For a genuine client already at V the recompute reproduces
// its own build, so the rewrite is an idempotent no-op.
//
// Callers must re-sign (signAnthropicMessagesBody) after a reported rewrite so
// the recomputed cch covers the rewritten body. When the switch is off (default),
// when billingVersion is empty, when system[0].text is not a billing header, when
// there is no first user message text to hash, when it carries no cc_version
// token, or when the recomputed cc_version already equals the current header, the
// body is returned unchanged with changed=false — the real path then stays
// byte-identical to today (default-safe / idempotent), and no forced re-sign is
// triggered on its behalf. Malformed / non-JSON bodies are a safe no-op
// pass-through (never a panic, never a corrupted build).
func alignRealPathBillingVersion(cfg *config.Config, body []byte, billingVersion string) ([]byte, bool) {
	if !config.AlignRealPathBillingVersionEnabled(cfg) {
		return body, false
	}
	billingVersion = strings.TrimSpace(billingVersion)
	if billingVersion == "" {
		return body, false
	}
	firstText := gjson.GetBytes(body, "system.0.text").String()
	if !strings.HasPrefix(firstText, "x-anthropic-billing-header:") {
		return body, false
	}
	// Recompute the build for V over the first non-meta user message as it appears
	// in the final outgoing body. If there is no first user message text, fall back
	// to a no-op rather than emit a build over nothing (never a corrupt build).
	firstUserMsg, ok := firstNonMetaUserMessageText(body)
	if !ok || firstUserMsg == "" {
		return body, false
	}
	newBuild := computeFingerprint(firstUserMsg, billingVersion)
	updatedText, rewritten := replaceBillingHeaderVersionAndBuild(firstText, billingVersion, newBuild)
	// replaceBillingHeaderVersionAndBuild's bool reports "applicable" (billing
	// header + non-empty version/build + a cc_version token), not "changed": it
	// re-emits an identical string for a genuine client already at V whose build
	// reproduces, or when there is no cc_version token to match. Compare the
	// strings so an already-correct header (idempotent) or a missing cc_version
	// returns changed=false and never forces a redundant re-sign.
	if !rewritten || updatedText == firstText {
		return body, false
	}
	updated, err := sjson.SetBytes(body, "system.0.text", updatedText)
	if err != nil {
		return body, false
	}
	return updated, true
}

func resolveClaudeKeyConfig(cfg *config.Config, auth *cliproxyauth.Auth) *config.ClaudeKey {
	if cfg == nil || auth == nil {
		return nil
	}

	apiKey, baseURL := claudeCreds(auth)
	if apiKey == "" {
		return nil
	}

	for i := range cfg.ClaudeKey {
		entry := &cfg.ClaudeKey[i]
		cfgKey := strings.TrimSpace(entry.APIKey)
		cfgBase := strings.TrimSpace(entry.BaseURL)
		if !strings.EqualFold(cfgKey, apiKey) {
			continue
		}
		if baseURL != "" && cfgBase != "" && !strings.EqualFold(cfgBase, baseURL) {
			continue
		}
		return entry
	}

	return nil
}

// resolveClaudeKeyCloakConfig finds the matching ClaudeKey config and returns its CloakConfig.
func resolveClaudeKeyCloakConfig(cfg *config.Config, auth *cliproxyauth.Auth) *config.CloakConfig {
	entry := resolveClaudeKeyConfig(cfg, auth)
	if entry == nil {
		return nil
	}
	return entry.Cloak
}

func experimentalCCHSigningEnabled(cfg *config.Config, auth *cliproxyauth.Auth) bool {
	entry := resolveClaudeKeyConfig(cfg, auth)
	return entry != nil && entry.ExperimentalCCHSigning
}

func rebuildMidSystemMessageEnabled(cfg *config.Config, auth *cliproxyauth.Auth) bool {
	if auth != nil && auth.Attributes != nil && strings.EqualFold(strings.TrimSpace(auth.Attributes["rebuild_mid_system_message"]), "true") {
		return true
	}
	entry := resolveClaudeKeyConfig(cfg, auth)
	return entry != nil && entry.RebuildMidSystemMessage
}
