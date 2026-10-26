package executor

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"

	xxHash64 "github.com/pierrec/xxHash/xxHash64"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	claudeCCHSeed   uint64 = 0x4D659218E32A3268
	claudeCCHLength        = 5
	claudeCCHZero          = "00000"
)

var claudeLegacyCCHPattern = regexp.MustCompile(`\bcch=([0-9a-f]{5});`)

type claudeCCHNormalizationEdit struct {
	start int
	end   int
}

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

type claudeCCHJSONMember struct {
	start       int
	end         int
	commaBefore int
	commaAfter  int
	excluded    bool
}

type claudeCCHJSONScanner struct {
	body  []byte
	pos   int
	edits []claudeCCHNormalizationEdit
}

type claudeCCHUpstreamKind uint8

const (
	claudeCCHUpstreamOther claudeCCHUpstreamKind = iota
	claudeCCHUpstreamAnthropic
	claudeCCHUpstreamVertex
)

func finalizeAnthropicMessagesBodyCCH(body []byte, fallbackBilling string) ([]byte, error) {
	bodyWithPlaceholder, err := ensureClaudeBillingHeaderCCHPlaceholder(body, fallbackBilling)
	if err != nil {
		return nil, err
	}
	return signAnthropicMessagesBody(bodyWithPlaceholder)
}

// claudeBodyNeedsBillingFallback reports whether a confirmed native helper request
// still needs CPA's billing-header fallback.
//
// The measured minimal helper carries no system field at all, which is exactly the
// native wire shape, so injecting a billing header there would be the deviation.
// Keying on "system is absent" rather than "no billing header present" means that
// if anything later in the pipeline (a payload rule, for instance) does attach a
// system prompt, the fallback comes back and the request cannot go upstream with a
// system block that native would never send unsigned.
func claudeBodyNeedsBillingFallback(body []byte) bool {
	return gjson.GetBytes(body, "system").Exists()
}

func ensureClaudeBillingHeaderCCHPlaceholder(body []byte, fallbackBilling string) ([]byte, error) {
	billing := gjson.GetBytes(body, "system.0.text")
	if billing.Type != gjson.String || !strings.HasPrefix(billing.String(), "x-anthropic-billing-header:") {
		if fallbackBilling == "" {
			return body, nil
		}
		var errPrepend error
		body, errPrepend = prependClaudeBillingSystemBlock(body, fallbackBilling)
		if errPrepend != nil {
			return nil, errPrepend
		}
		billing = gjson.GetBytes(body, "system.0.text")
	}
	if _, ok := claudeBillingCCHDigitsOffset(body); ok {
		return body, nil
	}

	billingText := billing.String()
	entrypoint := strings.Index(billingText, "cc_entrypoint=")
	if entrypoint < 0 {
		return body, nil
	}
	entrypointEnd := strings.IndexByte(billingText[entrypoint:], ';')
	if entrypointEnd < 0 {
		return body, nil
	}
	insertAt := entrypoint + entrypointEnd + 1
	billingText = billingText[:insertAt] + " cch=00000;" + billingText[insertAt:]
	updated, err := sjson.SetBytes(body, "system.0.text", billingText)
	if err != nil {
		return nil, fmt.Errorf("insert Claude CCH placeholder: %w", err)
	}
	return updated, nil
}

func prependClaudeBillingSystemBlock(body []byte, billingText string) ([]byte, error) {
	billingBlock := []byte(buildTextBlock(billingText, nil))
	system := gjson.GetBytes(body, "system")
	var systemArray []byte
	switch {
	case system.Type == gjson.String:
		originalBlock := []byte(buildTextBlock(system.String(), nil))
		systemArray = make([]byte, 0, len(billingBlock)+len(originalBlock)+3)
		systemArray = append(systemArray, '[')
		systemArray = append(systemArray, billingBlock...)
		systemArray = append(systemArray, ',')
		systemArray = append(systemArray, originalBlock...)
		systemArray = append(systemArray, ']')
	case system.IsArray():
		rawSystem := bytes.TrimSpace([]byte(system.Raw))
		if bytes.Equal(rawSystem, []byte("[]")) {
			systemArray = make([]byte, 0, len(billingBlock)+2)
			systemArray = append(systemArray, '[')
			systemArray = append(systemArray, billingBlock...)
			systemArray = append(systemArray, ']')
		} else {
			systemArray = make([]byte, 0, len(billingBlock)+len(rawSystem)+1)
			systemArray = append(systemArray, '[')
			systemArray = append(systemArray, billingBlock...)
			systemArray = append(systemArray, ',')
			systemArray = append(systemArray, rawSystem[1:]...)
		}
	default:
		systemArray = make([]byte, 0, len(billingBlock)+2)
		systemArray = append(systemArray, '[')
		systemArray = append(systemArray, billingBlock...)
		systemArray = append(systemArray, ']')
	}

	updated, err := sjson.SetRawBytes(body, "system", systemArray)
	if err != nil {
		return nil, fmt.Errorf("prepend Claude CCH billing block: %w", err)
	}
	return updated, nil
}

func isKimiAPIEndpoint(endpoint string) bool {
	parsed, err := url.Parse(strings.TrimSpace(endpoint))
	if err != nil {
		return false
	}
	host := parsed.Hostname()
	return strings.EqualFold(host, "api.kimi.com") || strings.EqualFold(host, "api.kimi.ai")
}

func isKimiMessagesUpstream(auth *cliproxyauth.Auth, endpoint string) bool {
	if auth != nil {
		provider := strings.ToLower(strings.TrimSpace(auth.Provider))
		if provider == "kimi" || provider == "kimi-ai" || provider == "kimi.ai" || provider == "kimi.com" {
			return true
		}
	}
	return isKimiAPIEndpoint(endpoint)
}

// stripDefaultKimiClaudeCodeAttribution removes the Claude Code billing/CCH
// attribution block from a Kimi Messages body when the caller did not opt into
// the full CLI profile. Kimi treats the block as prompt text, so forwarding it
// unchanged would leak CPA's attribution into the model's context. Other system
// content is preserved.
func stripDefaultKimiClaudeCodeAttribution(auth *cliproxyauth.Auth, endpoint string, cliFingerprint bool, body []byte) []byte {
	if cliFingerprint || !isKimiMessagesUpstream(auth, endpoint) {
		return body
	}
	return util.StripClaudeCodeAttributionSystem(body)
}

// claudeCCHSigningEnabled applies CPA's CCH policy.
//
// Native gate, identical in Claude Code 2.1.220 through 2.1.234:
//
//	s = (provider === "firstParty" && isFirstPartyBaseURL()) || provider === "vertex"
//	      ? " cch=00000;" : ""
//
// where isFirstPartyBaseURL() is true when ANTHROPIC_BASE_URL is unset or its
// host is api.anthropic.com. Every other backend (bedrock, foundry, mantle,
// anthropicAws, anthropicGoogleCloud, gateway, any custom base URL) sends the
// billing header without cch.
//
// CPA maps that onto two authorities:
//
//   - A real Claude OAuth credential always signs, on every upstream. CPA is the
//     hop that restores the first-party shape: a downstream Claude Code pointed at
//     CPA sees a non-first-party base URL and therefore omits cch itself, so the
//     value has to be regenerated here rather than inherited.
//   - An API key or delegated provider signs only when it explicitly opted into
//     the claude-code-cli profile AND the upstream is one the native gate accepts.
//     On any other gateway the billing header still goes out, but without cch, so
//     a per-request hash cannot bust that gateway's prompt cache.
//
// origin is the concrete upstream URL of the request being built. CPA additionally
// requires https and the default port, which native does not check.
func claudeCCHSigningEnabled(apiKey string, kind claudeCCHUpstreamKind, cliFingerprint bool, origin string) bool {
	if isClaudeOAuthToken(apiKey) {
		return true
	}
	if kind == claudeCCHUpstreamVertex {
		return true
	}
	if !cliFingerprint {
		return false
	}
	return kind == claudeCCHUpstreamAnthropic && isAnthropicUpstreamBase(origin)
}

// signClaudeHTTPRequestCCH selects the compatibility contract from the actual
// outbound fingerprint after native and managed header rules have completed.
// The body cannot choose its own algorithm. Keep retries and logs on these same
// final bytes rather than retaining the unsigned NewRequest reader.
func signClaudeHTTPRequestCCH(req *http.Request, body []byte) ([]byte, error) {
	version, _ := helps.ClaudeVersionFromUserAgent(req.Header.Get("User-Agent"))
	signed, err := signAnthropicMessagesBodyForVersion(body, version)
	if err != nil {
		return nil, err
	}
	if req.Body != nil {
		_ = req.Body.Close()
	}
	req.Body = io.NopCloser(bytes.NewReader(signed))
	req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(signed)), nil }
	req.ContentLength = int64(len(signed))
	if req.Header.Get("Content-Length") != "" {
		req.Header.Set("Content-Length", strconv.FormatInt(req.ContentLength, 10))
	}
	return signed, nil
}

func signAnthropicMessagesBodyForVersion(body []byte, version string) ([]byte, error) {
	parts := strings.Split(version, ".")
	if len(parts) == 3 {
		major, e1 := strconv.Atoi(parts[0])
		minor, e2 := strconv.Atoi(parts[1])
		patch, e3 := strconv.Atoi(parts[2])
		if e1 == nil && e2 == nil && e3 == nil && major >= 0 && minor >= 0 && patch >= 0 && (major > 2 || (major == 2 && (minor > 1 || (minor == 1 && patch >= 220)))) {
			return signAnthropicMessagesBody(body)
		}
	}
	// Preserve the fork's existing seed and raw-body algorithm for unknown or
	// older fingerprints. This is source-contract compatibility, not a claim
	// that a native 2.1.211 capture proved this algorithm.
	const legacySeed uint64 = 0x6E52736AC806831E
	header := gjson.GetBytes(body, "system.0.text").String()
	if !strings.HasPrefix(header, "x-anthropic-billing-header:") {
		return body, nil
	}
	pattern := claudeLegacyCCHPattern
	if !pattern.MatchString(header) {
		return body, nil
	}
	unsignedHeader := pattern.ReplaceAllString(header, "cch=00000;")
	unsigned, err := sjson.SetBytes(body, "system.0.text", unsignedHeader)
	if err != nil {
		return nil, fmt.Errorf("zero legacy Claude CCH: %w", err)
	}
	digest := fmt.Sprintf("%05x", xxHash64.Checksum(unsigned, legacySeed)&0xFFFFF)
	signedHeader := pattern.ReplaceAllString(unsignedHeader, "cch="+digest+";")
	signed, err := sjson.SetBytes(unsigned, "system.0.text", signedHeader)
	if err != nil {
		return nil, fmt.Errorf("write legacy Claude CCH: %w", err)
	}
	return signed, nil
}

// signAnthropicMessagesBody reproduces Claude Code 2.1.220's final-body CCH.
// It changes only the five CCH digits in the outgoing body.
func signAnthropicMessagesBody(body []byte) ([]byte, error) {
	cchOffset, ok := claudeBillingCCHDigitsOffset(body)
	if !ok {
		return body, nil
	}

	unsignedBody := bytes.Clone(body)
	copy(unsignedBody[cchOffset:cchOffset+claudeCCHLength], claudeCCHZero)
	normalizedBody, err := normalizeClaudeCCHInput(unsignedBody)
	if err != nil {
		return nil, fmt.Errorf("normalize Claude CCH input: %w", err)
	}

	hasher := xxHash64.New(claudeCCHSeed)
	if _, err = hasher.Write(normalizedBody); err != nil {
		return nil, fmt.Errorf("hash Claude CCH input: %w", err)
	}
	cch := fmt.Sprintf("%05x", hasher.Sum64()&0xFFFFF)
	copy(unsignedBody[cchOffset:cchOffset+claudeCCHLength], cch)
	return unsignedBody, nil
}

func claudeBillingCCHDigitsOffset(body []byte) (int, bool) {
	billing := gjson.GetBytes(body, "system.0.text")
	if billing.Type != gjson.String || !strings.HasPrefix(billing.String(), "x-anthropic-billing-header:") {
		return 0, false
	}

	raw := []byte(billing.Raw)
	for searchFrom := 0; searchFrom < len(raw); {
		relative := bytes.Index(raw[searchFrom:], []byte("cch="))
		if relative < 0 {
			return 0, false
		}
		prefix := searchFrom + relative
		digits := prefix + len("cch=")
		end := digits + claudeCCHLength
		if end < len(raw) && raw[end] == ';' && isLowerHex(raw[digits:end]) {
			return billing.Index + digits, true
		}
		searchFrom = prefix + len("cch=")
	}
	return 0, false
}

func isLowerHex(value []byte) bool {
	if len(value) != claudeCCHLength {
		return false
	}
	for _, character := range value {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}

// normalizeClaudeCCHInput builds the hash view without reserializing JSON.
// Model string values are emptied, while dispatch-only members are omitted.
func normalizeClaudeCCHInput(body []byte) ([]byte, error) {
	if !json.Valid(body) {
		return nil, fmt.Errorf("invalid JSON body")
	}

	scanner := claudeCCHJSONScanner{
		body:  body,
		edits: make([]claudeCCHNormalizationEdit, 0),
	}
	if err := scanner.parseValue(true); err != nil {
		return nil, err
	}
	scanner.skipWhitespace()
	if scanner.pos != len(body) {
		return nil, fmt.Errorf("unexpected JSON data at byte %d", scanner.pos)
	}

	sort.Slice(scanner.edits, func(i, j int) bool {
		return scanner.edits[i].start < scanner.edits[j].start
	})
	normalized := make([]byte, 0, len(body))
	last := 0
	for _, edit := range scanner.edits {
		if edit.start < last || edit.end > len(body) {
			return nil, fmt.Errorf("overlapping CCH normalization edit at byte %d", edit.start)
		}
		normalized = append(normalized, body[last:edit.start]...)
		last = edit.end
	}
	normalized = append(normalized, body[last:]...)
	return normalized, nil
}

func (scanner *claudeCCHJSONScanner) parseValue(collect bool) error {
	scanner.skipWhitespace()
	if scanner.pos >= len(scanner.body) {
		return fmt.Errorf("missing JSON value at byte %d", scanner.pos)
	}

	switch scanner.body[scanner.pos] {
	case '{':
		return scanner.parseObject(collect)
	case '[':
		return scanner.parseArray(collect)
	case '"':
		_, _, err := scanner.parseString()
		return err
	default:
		start := scanner.pos
		for scanner.pos < len(scanner.body) {
			switch scanner.body[scanner.pos] {
			case ',', '}', ']', ' ', '\t', '\r', '\n':
				if scanner.pos == start {
					return fmt.Errorf("missing JSON value at byte %d", start)
				}
				return nil
			default:
				scanner.pos++
			}
		}
		if scanner.pos == start {
			return fmt.Errorf("missing JSON value at byte %d", start)
		}
		return nil
	}
}

func (scanner *claudeCCHJSONScanner) parseObject(collect bool) error {
	scanner.pos++
	scanner.skipWhitespace()
	if scanner.consume('}') {
		return nil
	}

	members := make([]claudeCCHJSONMember, 0)
	commaBefore := -1
	for {
		scanner.skipWhitespace()
		memberStart := scanner.pos
		keyStart, keyEnd, err := scanner.parseString()
		if err != nil {
			return err
		}
		scanner.skipWhitespace()
		if !scanner.consume(':') {
			return fmt.Errorf("missing object colon at byte %d", scanner.pos)
		}
		scanner.skipWhitespace()

		key := scanner.body[keyStart:keyEnd]
		excluded := collect && isClaudeCCHExcludedKey(key)
		if collect && bytes.Equal(key, []byte(`"model"`)) && scanner.pos < len(scanner.body) && scanner.body[scanner.pos] == '"' {
			valueStart, valueEnd, errString := scanner.parseString()
			if errString != nil {
				return errString
			}
			scanner.addEdit(valueStart+1, valueEnd-1)
		} else if err = scanner.parseValue(collect && !excluded); err != nil {
			return err
		}
		memberEnd := scanner.pos
		scanner.skipWhitespace()

		commaAfter := -1
		if scanner.consume(',') {
			commaAfter = scanner.pos - 1
		}
		members = append(members, claudeCCHJSONMember{
			start:       memberStart,
			end:         memberEnd,
			commaBefore: commaBefore,
			commaAfter:  commaAfter,
			excluded:    excluded,
		})
		if commaAfter >= 0 {
			commaBefore = commaAfter
			continue
		}
		if !scanner.consume('}') {
			return fmt.Errorf("missing object end at byte %d", scanner.pos)
		}
		break
	}

	if collect {
		scanner.addExcludedMemberEdits(members)
	}
	return nil
}

func (scanner *claudeCCHJSONScanner) parseArray(collect bool) error {
	scanner.pos++
	scanner.skipWhitespace()
	if scanner.consume(']') {
		return nil
	}

	for {
		if err := scanner.parseValue(collect); err != nil {
			return err
		}
		scanner.skipWhitespace()
		if scanner.consume(',') {
			continue
		}
		if !scanner.consume(']') {
			return fmt.Errorf("missing array end at byte %d", scanner.pos)
		}
		return nil
	}
}

func (scanner *claudeCCHJSONScanner) parseString() (start, end int, err error) {
	if scanner.pos >= len(scanner.body) || scanner.body[scanner.pos] != '"' {
		return 0, 0, fmt.Errorf("missing JSON string at byte %d", scanner.pos)
	}

	start = scanner.pos
	scanner.pos++
	for scanner.pos < len(scanner.body) {
		switch scanner.body[scanner.pos] {
		case '\\':
			scanner.pos += 2
		case '"':
			scanner.pos++
			return start, scanner.pos, nil
		default:
			scanner.pos++
		}
	}
	return 0, 0, fmt.Errorf("unterminated JSON string at byte %d", start)
}

func (scanner *claudeCCHJSONScanner) addExcludedMemberEdits(members []claudeCCHJSONMember) {
	for start := 0; start < len(members); {
		if !members[start].excluded {
			start++
			continue
		}

		end := start
		for end+1 < len(members) && members[end+1].excluded {
			end++
		}
		switch {
		case end+1 < len(members):
			scanner.addEdit(members[start].start, members[end].commaAfter+1)
		case start > 0 && end > start:
			// Claude Code 2.1.220 leaves the preceding comma in its hash view
			// when an object ends with multiple consecutive dispatch members.
			scanner.addEdit(members[start].start, members[end].end)
		case start > 0:
			scanner.addEdit(members[start].commaBefore, members[end].end)
		default:
			scanner.addEdit(members[start].start, members[end].end)
		}
		start = end + 1
	}
}

func (scanner *claudeCCHJSONScanner) addEdit(start, end int) {
	if start >= end {
		return
	}
	scanner.edits = append(scanner.edits, claudeCCHNormalizationEdit{start: start, end: end})
}

func (scanner *claudeCCHJSONScanner) skipWhitespace() {
	for scanner.pos < len(scanner.body) {
		switch scanner.body[scanner.pos] {
		case ' ', '\t', '\r', '\n':
			scanner.pos++
		default:
			return
		}
	}
}

func (scanner *claudeCCHJSONScanner) consume(character byte) bool {
	if scanner.pos >= len(scanner.body) || scanner.body[scanner.pos] != character {
		return false
	}
	scanner.pos++
	return true
}

func isClaudeCCHExcludedKey(key []byte) bool {
	switch string(key) {
	case `"max_tokens"`, `"fallbacks"`, `"fallback_credit_token"`:
		return true
	default:
		return false
	}
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

func rebuildMidSystemMessageEnabled(cfg *config.Config, auth *cliproxyauth.Auth) bool {
	if auth != nil && auth.Attributes != nil && strings.EqualFold(strings.TrimSpace(auth.Attributes["rebuild_mid_system_message"]), "true") {
		return true
	}
	entry := resolveClaudeKeyConfig(cfg, auth)
	return entry != nil && entry.RebuildMidSystemMessage
}

func experimentalCCHSigningEnabled(cfg *config.Config, auth *cliproxyauth.Auth) bool {
	entry := resolveClaudeKeyConfig(cfg, auth)
	return entry != nil && entry.ExperimentalCCHSigning
}
