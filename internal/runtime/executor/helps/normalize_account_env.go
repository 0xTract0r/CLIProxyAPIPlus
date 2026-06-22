package helps

import (
	"crypto/sha256"
	"encoding/binary"
	"regexp"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

// Account env/cwd normalization (requirement ⑦).
//
// Claude Code embeds the developer machine's working directory, real user name,
// home path and host OS into the request body inside an "environment" block. As
// of claude-code 2.1.181 (production, measured) that block is a Markdown section,
// not the older <env> XML form:
//
//	# Environment
//	You have been invoked in the following environment:
//	 - Primary working directory: /tmp/prodsess1
//	 - Is a git repository: false
//	 - Platform: linux
//	 - Shell: bash
//	 - OS Version: Linux 6.8.0-111-generic
//	 ...
//
// Older clients (and historical evidence) instead emit an <env>...</env> /
// <system-reminder>...</system-reminder> XML block carrying the same fields
// ("Working directory: /Users/<user>/..."). Both forms must be normalized.
//
// When several employees share one upstream account, these real paths leak each
// machine's identity and let Anthropic correlate the account back to multiple
// distinct developers. Separately, the INDEPENDENT stabilize-device-profile path
// rewrites the outbound HTTP fingerprint headers (X-Stainless-Os /
// X-Stainless-Arch) to a single per-account baseline OS; only then does a body
// that still reports the real host OS ("Platform: linux" / "OS Version: Linux ...")
// contradict a header that claims MacOS — a self-inconsistent "claims Mac but
// reports Linux" signal Anthropic can cross-check. NormalizeAccountEnv therefore
// rewrites, inside the environment block only:
//   - every real cwd / home path (key-anchored) to a per-account canonical path
//     that is deterministic for a given upstream account, stable across requests,
//     and different between distinct accounts. This is UNCONDITIONAL under
//     normalize-account-env: not leaking the real cwd does not depend on the
//     header path.
//   - the body Platform / OS Version lines to the baseline OS the outbound headers
//     advertise, but ONLY when stabilize-device-profile is also on. The two
//     switches are independent: when stabilize is off the header passes through
//     the real OS, so rewriting the body OS would itself create the body/header
//     contradiction this normalization exists to avoid; the body OS lines are then
//     left untouched.
//
// Scope discipline (the key to not breaking tool calls):
//   - Only the text *inside* an <env> / <system-reminder> span or a
//     "# Environment" Markdown block is touched.
//   - tool_use input args, tool_result content and ordinary conversational text
//     are never modified, even if they happen to contain absolute paths.
//
// Parse failures are a safe no-op: the original payload is returned unchanged so
// the request is never rejected with a 400.

// canonicalHomeRoot is the fixed canonical home prefix every account is mapped
// onto. The per-account distinction lives in the workspace directory suffix, so
// all accounts share an identical, machine-neutral home shape while remaining
// individually consistent.
//
// fork(anticorr): 取 macOS 风格 /Users/agent（而非旧的 Linux 风格 /home/agent）。
// claude/codex 的出站 UA 基线都是 MacOS（X-Stainless-Os=MacOS / codex UA "Mac OS"），
// 路径若用 /home（Linux 形态）会和 UA 自相矛盾，构成"自称 Mac 但路径是 Linux"的
// 反关联信号。改成 /Users 后 body 路径与 header OS 画像一致。这是 claude+codex 共用
// 的 helper，两边出站 cwd / CODEX_HOME 都随之变成 /Users/agent 前缀。
const canonicalHomeRoot = "/Users/agent"

// baselineBodyOS describes how a given Stainless baseline OS name (the value of
// X-Stainless-Os emitted by ResolveClaudeDeviceProfile) is represented inside the
// request body environment block. Real claude-code derives these body fields from
// Node's process.platform (the "Platform:" value) and os.type()+os.release()
// (the "OS Version:" value); we mirror that mapping so body and header describe
// the same OS.
type baselineBodyOS struct {
	// platform is the Node process.platform value (e.g. "darwin", "linux").
	platform string
	// osVersion is the Node `${os.type()} ${os.release()}` value (e.g.
	// "Darwin 24.6.0"). It is a fixed, deterministic per-OS constant so the body
	// fingerprint does not vary per request.
	osVersion string
}

// baselineBodyOSFor maps a Stainless OS name (as carried in the device profile /
// X-Stainless-Os header) to the Platform / OS Version representation used inside
// the request body environment block. The baseline OS is sourced from the same
// defaultClaudeDeviceProfile(cfg) the outbound header path uses, so there is a
// single OS source of truth — this function only translates that one OS into its
// body form, it does not introduce a second OS decision.
//
// The OS Version values are fixed, plausible release constants for each platform
// (chosen, not packet-confirmed):
//   - MacOS  -> Platform "darwin",  OS Version "Darwin 24.6.0" (Apple Silicon
//     macOS 15.x kernel; aligns with the MacOS/arm64 baseline fingerprint).
//   - Linux  -> Platform "linux",   OS Version "Linux 6.8.0-1010-azure".
//   - Windows-> Platform "win32",   OS Version "Windows_NT 10.0.22631".
//   - FreeBSD-> Platform "freebsd", OS Version "FreeBSD 14.0-RELEASE".
//
// An unrecognized OS name yields ok=false, leaving the body OS lines untouched.
func baselineBodyOSFor(stainlessOS string) (baselineBodyOS, bool) {
	switch strings.TrimSpace(stainlessOS) {
	case "MacOS":
		return baselineBodyOS{platform: "darwin", osVersion: "Darwin 24.6.0"}, true
	case "Linux":
		return baselineBodyOS{platform: "linux", osVersion: "Linux 6.8.0-1010-azure"}, true
	case "Windows":
		return baselineBodyOS{platform: "win32", osVersion: "Windows_NT 10.0.22631"}, true
	case "FreeBSD":
		return baselineBodyOS{platform: "freebsd", osVersion: "FreeBSD 14.0-RELEASE"}, true
	default:
		return baselineBodyOS{}, false
	}
}

// xmlEnvBlockPattern matches a full <env>...</env> or
// <system-reminder>...</system-reminder> span (including the tags). It is
// non-greedy and case-insensitive on the tag so only the environment-description
// text is handed to the rewriter; the rest of the surrounding text block is left
// byte-for-byte unchanged.
var xmlEnvBlockPattern = regexp.MustCompile(`(?is)<(env|system-reminder)>.*?</(env|system-reminder)>`)

// markdownEnvBlockPattern matches a real claude-code 2.1.181 "# Environment"
// Markdown block: the "# Environment" heading line followed by the contiguous run
// of subsequent lines that do NOT begin a new Markdown heading ("#..."). The
// trailing run therefore terminates at (but does not consume) the next heading or
// end of text. (?m) makes ^ line-anchored. RE2 has no lookahead, so the block
// boundary is expressed as "lines not starting with #" rather than a lookahead
// terminator; this keeps the match scoped to the environment section while a
// following "# Tools" / "# Memory" heading is left untouched.
var markdownEnvBlockPattern = regexp.MustCompile(`(?m)^#[ \t]+Environment[ \t]*$(?:\n[^#\n].*|\n)*`)

// realPathPattern matches an absolute home path segment of the form
// /Users/<user>/... or /home/<user>/... up to (but not including) the next
// whitespace, quote, angle bracket or end of line. It is retained as a secondary
// sweep for embedded home paths (e.g. CLAUDE.md / memory paths) that are not on a
// recognized key line; unrelated absolute paths (e.g. /usr, /etc) are left alone.
var realPathPattern = regexp.MustCompile(`/(?:Users|home)/[^/\s"'<>` + "`" + `]+(?:/[^\s"'<>` + "`" + `]*)?`)

// cwdKeyValuePattern matches an environment key line whose value is the working
// directory, anchored on the key rather than the path prefix so it normalizes any
// path (including /tmp/... or other non-home roots that the realPathPattern sweep
// deliberately ignores). It covers both the Markdown flat-list form
// (" - Primary working directory: <path>") and the XML/legacy form
// ("Working directory: <path>"). Capture group 1 is everything up to and
// including the ": " separator; group 2 is the value to replace.
var cwdKeyValuePattern = regexp.MustCompile(`(?im)^([ \t]*-?[ \t]*(?:Primary working directory|Working directory|Current working directory):[ \t]*)(\S.*?)[ \t]*$`)

// platformKeyValuePattern matches the body "Platform:" line (Node
// process.platform). Capture group 1 is the key prefix; group 2 is the value.
var platformKeyValuePattern = regexp.MustCompile(`(?im)^([ \t]*-?[ \t]*Platform:[ \t]*)(\S.*?)[ \t]*$`)

// osVersionKeyValuePattern matches the body "OS Version:" line (Node
// os.type()+os.release()). Capture group 1 is the key prefix; group 2 is the
// value.
var osVersionKeyValuePattern = regexp.MustCompile(`(?im)^([ \t]*-?[ \t]*OS Version:[ \t]*)(\S.*?)[ \t]*$`)

// AccountCanonicalCwd derives the per-account canonical working directory from the
// shared account scope key. The result is deterministic for a given upstream
// account, stable across requests, apiKeys and restarts, and different between
// distinct accounts. Unlike the synthetic device_id it intentionally does not mix
// in the server salt: the canonical cwd carries no secret and only needs to be a
// stable, machine-neutral path, so callers (and an optional read-only UI display)
// can reproduce it from the account scope key alone.
func AccountCanonicalCwd(auth *cliproxyauth.Auth, apiKey string) string {
	scopeKey := ClaudeAccountScopeKey(auth, apiKey)
	sum := sha256.Sum256([]byte("cliproxy-canonical-cwd\x00" + scopeKey))
	// Use the first 4 bytes as a stable, opaque per-account workspace id.
	id := binary.BigEndian.Uint32(sum[:4])
	return canonicalHomeRoot + "/workspace-" + uint32ToHex(id)
}

// uint32ToHex renders a uint32 as a fixed-width 8-char lowercase hex string.
func uint32ToHex(v uint32) string {
	const hexDigits = "0123456789abcdef"
	buf := [8]byte{}
	for i := 7; i >= 0; i-- {
		buf[i] = hexDigits[v&0xf]
		v >>= 4
	}
	return string(buf[:])
}

// envRewriteParams bundles the per-account/per-config values used to rewrite an
// environment block: the canonical cwd and the baseline body OS representation.
type envRewriteParams struct {
	canonicalCwd string
	bodyOS       baselineBodyOS
	hasBodyOS    bool
}

// NormalizeAccountEnv rewrites real cwd / home paths and (conditionally) the host
// OS lines inside the environment block (both the "# Environment" Markdown form
// emitted by real claude-code 2.1.181 and the legacy <env> / <system-reminder> XML
// form) of the system field and the messages content. cwd is always rewritten to
// the per-account canonical path. Platform / OS Version are rewritten to the
// baseline OS derived from the same defaultClaudeDeviceProfile(cfg) the outbound
// header path uses ONLY when stabilize-device-profile is enabled — that is the
// only state in which the outbound X-Stainless-Os header is also pinned to that
// baseline, so body and header describe one OS. When stabilize is off the header
// carries the real OS and the body OS lines are left untouched to avoid creating a
// body/header mismatch. It returns the payload unchanged when the body is
// unparseable or contains no matching block.
func NormalizeAccountEnv(payload []byte, auth *cliproxyauth.Auth, apiKey string, cfg *config.Config) []byte {
	if !gjson.ValidBytes(payload) {
		// Never attempt to rewrite an unparseable body; pass it through (no 400).
		return payload
	}

	params := envRewriteParams{canonicalCwd: AccountCanonicalCwd(auth, apiKey)}
	// Body OS rewrite is gated on stabilize-device-profile, which is an INDEPENDENT
	// switch from normalize-account-env. The outbound X-Stainless-Os header is only
	// pinned to defaultClaudeDeviceProfile(cfg).OS when stabilize is on; when
	// stabilize is off the header passes through the REAL host OS. So body and header
	// agree only when stabilize is on:
	//   - stabilize on:  header is pinned to the baseline OS, so rewriting the body
	//     Platform / OS Version to that same baseline keeps body == header (T052).
	//   - stabilize off: header carries the real OS (e.g. Linux); rewriting the body
	//     to the baseline (e.g. MacOS) would create a body(Mac) vs header(Linux)
	//     contradiction, so the body OS lines are left untouched here.
	// cwd normalization above is unconditional under normalize-account-env: not
	// leaking the real cwd is an independent goal that does not depend on stabilize.
	if ClaudeDeviceProfileStabilizationEnabled(cfg) {
		if bodyOS, ok := baselineBodyOSFor(defaultClaudeDeviceProfile(cfg).OS); ok {
			params.bodyOS = bodyOS
			params.hasBodyOS = true
		}
	}

	rewrite := func(text string) string {
		return normalizeEnvText(text, params)
	}

	payload = normalizeSystemEnvBlocks(payload, rewrite)
	payload = normalizeMessagesEnvBlocks(payload, rewrite)
	return payload
}

// normalizeEnvText rewrites every environment block (XML or Markdown) found in
// text, leaving everything outside those blocks untouched.
func normalizeEnvText(text string, params envRewriteParams) string {
	if text == "" {
		return text
	}
	hasXML := strings.Contains(text, "<env>") || strings.Contains(text, "<system-reminder>")
	hasMarkdown := strings.Contains(text, "# Environment")
	if !hasXML && !hasMarkdown {
		return text
	}
	if hasXML {
		text = xmlEnvBlockPattern.ReplaceAllStringFunc(text, func(block string) string {
			return rewriteEnvBlock(block, params)
		})
	}
	if hasMarkdown {
		text = markdownEnvBlockPattern.ReplaceAllStringFunc(text, func(block string) string {
			return rewriteEnvBlock(block, params)
		})
	}
	return text
}

// rewriteEnvBlock normalizes a single environment block (the text inside an
// <env> / <system-reminder> span or a "# Environment" Markdown section):
//   - the working-directory value line(s) are rewritten to the canonical path,
//     anchored on the key so any path root (including /tmp/...) is covered;
//   - any remaining embedded home path (e.g. memory / CLAUDE.md paths not on a
//     recognized key line) is collapsed onto the same canonical path;
//   - the Platform / OS Version lines are rewritten to the baseline body OS so they
//     agree with the outbound X-Stainless-Os header — but only when params.hasBodyOS
//     is set, which the caller gates on stabilize-device-profile (the OS lines are
//     left untouched when stabilize is off, since the header then carries the real OS).
//
// The block no longer leaks the real machine identity, and when stabilize is on it
// no longer contradicts the stabilized header OS.
func rewriteEnvBlock(block string, params envRewriteParams) string {
	// Key-anchored cwd normalization first (covers arbitrary path roots).
	block = cwdKeyValuePattern.ReplaceAllString(block, "${1}"+params.canonicalCwd)
	// Secondary sweep for embedded home-rooted paths not on a cwd key line.
	block = realPathPattern.ReplaceAllString(block, params.canonicalCwd)
	// OS normalization: align body Platform / OS Version with the baseline OS.
	if params.hasBodyOS {
		block = platformKeyValuePattern.ReplaceAllString(block, "${1}"+params.bodyOS.platform)
		block = osVersionKeyValuePattern.ReplaceAllString(block, "${1}"+params.bodyOS.osVersion)
	}
	return block
}

// normalizeSystemEnvBlocks applies the env rewriter to the system field. It reuses
// the obfuscateSystemBlocks traversal shape (array of text blocks or a plain
// string), only touching text whose content actually changes.
func normalizeSystemEnvBlocks(payload []byte, rewrite func(string) string) []byte {
	system := gjson.GetBytes(payload, "system")
	if !system.Exists() {
		return payload
	}

	if system.IsArray() {
		system.ForEach(func(key, value gjson.Result) bool {
			if value.Get("type").String() == "text" {
				text := value.Get("text").String()
				rewritten := rewrite(text)
				if rewritten != text {
					path := "system." + key.String() + ".text"
					payload, _ = sjson.SetBytes(payload, path, rewritten)
				}
			}
			return true
		})
	} else if system.Type == gjson.String {
		text := system.String()
		rewritten := rewrite(text)
		if rewritten != text {
			payload, _ = sjson.SetBytes(payload, "system", rewritten)
		}
	}

	return payload
}

// normalizeMessagesEnvBlocks applies the env rewriter to message text content. It
// reuses the obfuscateMessages traversal shape but, because normalizeEnvText only
// rewrites inside environment blocks, ordinary conversational text and any path
// that is not wrapped in those blocks is left untouched. tool_use and tool_result
// blocks are never of type "text" here, so their args / content are not visited.
func normalizeMessagesEnvBlocks(payload []byte, rewrite func(string) string) []byte {
	messages := gjson.GetBytes(payload, "messages")
	if !messages.Exists() || !messages.IsArray() {
		return payload
	}

	messages.ForEach(func(msgKey, msg gjson.Result) bool {
		content := msg.Get("content")
		if !content.Exists() {
			return true
		}

		msgPath := "messages." + msgKey.String()

		if content.Type == gjson.String {
			text := content.String()
			rewritten := rewrite(text)
			if rewritten != text {
				payload, _ = sjson.SetBytes(payload, msgPath+".content", rewritten)
			}
		} else if content.IsArray() {
			content.ForEach(func(blockKey, block gjson.Result) bool {
				// Only plain text blocks carry environment descriptions. tool_use
				// ("input") and tool_result ("content") blocks are intentionally
				// skipped here.
				if block.Get("type").String() == "text" {
					text := block.Get("text").String()
					rewritten := rewrite(text)
					if rewritten != text {
						path := msgPath + ".content." + blockKey.String() + ".text"
						payload, _ = sjson.SetBytes(payload, path, rewritten)
					}
				}
				return true
			})
		}

		return true
	})

	return payload
}
