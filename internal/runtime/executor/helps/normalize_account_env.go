package helps

import (
	"crypto/sha256"
	"encoding/binary"
	"regexp"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

// Account env/cwd normalization (requirement ⑦).
//
// Claude Code embeds the developer machine's working directory, real user name
// and home path into the request body in two distinct places:
//   - system.*.text: the <env> section ("Working directory: /Users/<user>/...").
//   - messages.*.content.*.text: <system-reminder> / <env> blocks emitted by
//     CLAUDE.md, hooks and dynamic reminders (also carrying absolute paths).
//
// When several employees share one upstream account, these real paths leak each
// machine's identity and let Anthropic correlate the account back to multiple
// distinct developers. NormalizeAccountEnv rewrites every real cwd / home path
// inside those tagged environment blocks to a per-account canonical path that is
// deterministic for a given upstream account (derived from the same account scope
// key as the synthetic device_id), stable across requests, and different between
// distinct accounts.
//
// Scope discipline (the key to not breaking tool calls):
//   - Only the text *inside* <env>...</env> and <system-reminder>...</system-reminder>
//     spans is touched.
//   - tool_use input args, tool_result content and ordinary conversational text
//     are never modified, even if they happen to contain absolute paths.
//
// Parse failures are a safe no-op: the original payload is returned unchanged so
// the request is never rejected with a 400.

// canonicalHomeRoot is the fixed canonical home prefix every account is mapped
// onto. The per-account distinction lives in the workspace directory suffix, so
// all accounts share an identical, machine-neutral home shape while remaining
// individually consistent.
const canonicalHomeRoot = "/home/agent"

// envBlockPattern matches a full <env>...</env> or <system-reminder>...</system-reminder>
// span (including the tags). It is non-greedy and case-insensitive on the tag so
// only the environment-description text is handed to the path rewriter; the rest
// of the surrounding text block is left byte-for-byte unchanged.
var envBlockPattern = regexp.MustCompile(`(?is)<(env|system-reminder)>.*?</(env|system-reminder)>`)

// realPathPattern matches an absolute home path segment of the form
// /Users/<user>/... or /home/<user>/... up to (but not including) the next
// whitespace, quote, angle bracket or end of line. Only these home-rooted
// absolute paths are rewritten; unrelated absolute paths (e.g. /usr, /etc, /tmp)
// are deliberately left untouched.
var realPathPattern = regexp.MustCompile(`/(?:Users|home)/[^/\s"'<>` + "`" + `]+(?:/[^\s"'<>` + "`" + `]*)?`)

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

// NormalizeAccountEnv rewrites real cwd / home paths inside <env> and
// <system-reminder> blocks of both the system field and the messages content to
// the per-account canonical path. It returns the payload unchanged when the body
// is unparseable or contains no matching block.
func NormalizeAccountEnv(payload []byte, auth *cliproxyauth.Auth, apiKey string) []byte {
	if !gjson.ValidBytes(payload) {
		// Never attempt to rewrite an unparseable body; pass it through (no 400).
		return payload
	}

	canonicalCwd := AccountCanonicalCwd(auth, apiKey)
	rewrite := func(text string) string {
		return normalizeEnvText(text, canonicalCwd)
	}

	payload = normalizeSystemEnvBlocks(payload, rewrite)
	payload = normalizeMessagesEnvBlocks(payload, rewrite)
	return payload
}

// normalizeEnvText rewrites the real paths inside every <env> / <system-reminder>
// span found in text, leaving everything outside those spans untouched.
func normalizeEnvText(text, canonicalCwd string) string {
	if text == "" {
		return text
	}
	if !strings.Contains(text, "<env>") && !strings.Contains(text, "<system-reminder>") {
		return text
	}
	return envBlockPattern.ReplaceAllStringFunc(text, func(block string) string {
		return rewritePathsInBlock(block, canonicalCwd)
	})
}

// rewritePathsInBlock replaces every home-rooted absolute path inside a single
// <env> / <system-reminder> span with the canonical workspace path. The
// "Working directory:" (and "Primary working directory:") value lines and any
// other embedded home paths all collapse onto the same per-account canonical
// directory, so the block no longer leaks the real machine identity.
func rewritePathsInBlock(block, canonicalCwd string) string {
	return realPathPattern.ReplaceAllString(block, canonicalCwd)
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
// rewrites inside <env> / <system-reminder> spans, ordinary conversational text
// and any path that is not wrapped in those tags is left untouched. tool_use and
// tool_result blocks are never of type "text" here, so their args / content are
// not visited.
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
				// Only plain text blocks carry <env> / <system-reminder>
				// environment descriptions. tool_use ("input") and tool_result
				// ("content") blocks are intentionally skipped here.
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
