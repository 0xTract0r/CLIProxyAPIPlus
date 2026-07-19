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
