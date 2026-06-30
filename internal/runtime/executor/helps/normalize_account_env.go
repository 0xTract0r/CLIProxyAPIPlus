package helps

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"regexp"
	"sort"
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

// memoryPathPattern matches the claude-code "auto memory" sentence that embeds
// the real on-disk memory directory, e.g.:
//
//	You have a persistent, file-based memory system at `/Users/<user>/.claude/projects/<encoded-cwd>/memory/`.
//
// This sentence lives under a SEPARATE "# auto memory" Markdown heading, not the
// "# Environment" block, so the env-block rewriter (markdownEnvBlockPattern,
// which terminates at the next "#" heading) never reaches it. The leak is real in
// production: the path root is the real home (/Users/<user>/ or /home/<user>/),
// and even when the HOME is an isolated /tmp dir, the projects subdirectory name
// URL-encodes the real working directory ("-Users-corylin-Project-..."), so the
// real machine user name leaks regardless of the root.
//
// Because this normalization is anchored on the literal "memory system at `...`"
// phrase (not on a path prefix), it cannot touch tool_use args, tool_result
// content or arbitrary conversational paths the way a blind /Users|/home full-text
// sweep would — preserving the scope discipline the realPathPattern comment warns
// about. Capture group 1 is the text up to and including the opening backtick;
// group 2 is the backtick-quoted path body (any root, including /tmp/...); group 3
// is the closing backtick. The path body is collapsed onto a single per-account
// canonical memory path.
var memoryPathPattern = regexp.MustCompile("(?i)(file-based memory system at\\s+`)([^`]*)(`)")

// cwdKeyValuePattern matches an environment key line whose value is the working
// directory, anchored on the key rather than the path prefix so it normalizes any
// path (including /tmp/... or other non-home roots that the realPathPattern sweep
// deliberately ignores). It covers both the Markdown flat-list form
// (" - Primary working directory: <path>") and the XML/legacy form
// ("Working directory: <path>"). Capture group 1 is everything up to and
// including the ": " separator; group 2 is the value to replace.
var cwdKeyValuePattern = regexp.MustCompile(`(?im)^([ \t]*-?[ \t]*(?:Primary working directory|Working directory|Current working directory):[ \t]*)(\S.*?)[ \t]*$`)

// additionalDirsHeadingPattern matches the claude-code "Additional working
// directories:" heading line. Unlike the cwd key lines above, the real paths it
// introduces are NOT on this line: they live on the subsequent indented Markdown
// list items ("  - <path>"), which carry no key and would therefore be missed by
// cwdKeyValuePattern and, when their root is not /Users|/home (e.g. /tmp, /var),
// also by the realPathPattern home-only sweep. additionalDirItemPattern matches
// each such list item so every additional directory — at any root — is normalized.
// Capture group 1 is the "- " bullet prefix (with indentation); group 2 is the
// directory path value.
//
// additionalDirItemPattern deliberately requires the list-item value to be an
// ABSOLUTE path (POSIX "/..." or Windows "<drive>:\..."). Sibling env fields in the
// same flat Markdown list ("- Is a git repository: false", "- Platform: linux") are
// also "- <text>" lines but are NOT absolute paths, so they do not match and the
// additional-directories run terminates at the first such field — never rewriting a
// non-path env field as if it were a directory.
var additionalDirsHeadingPattern = regexp.MustCompile(`(?im)^[ \t]*-?[ \t]*Additional working directories:[ \t]*$`)
var additionalDirItemPattern = regexp.MustCompile(`(?m)^([ \t]*-[ \t]*)((?:/|[A-Za-z]:\\)\S.*?)[ \t]*$`)

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

// derivedFakeWorkspaceRoot derives a deterministic, machine-neutral fake root for
// an ADDITIONAL real working directory (one that is distinct from the primary cwd).
// The primary cwd maps to AccountCanonicalCwd (stable per account); a request may,
// however, declare several distinct additional directories, each of which is a
// different real path that must map to its OWN fake root so the response side can
// restore each one back to the correct real directory. We seed the derivation on
// the real path itself (not the account) so the same real additional directory
// always yields the same fake root within and across requests, while leaking no
// real-path information: only the sha256 of the real path is used. The shape mirrors
// AccountCanonicalCwd (canonicalHomeRoot + "/workspace-<hex8>") so an upstream
// observer sees a uniform, machine-neutral workspace path indistinguishable from the
// primary one.
func derivedFakeWorkspaceRoot(realPath string) string {
	sum := sha256.Sum256([]byte("cliproxy-canonical-additional-cwd\x00" + realPath))
	id := binary.BigEndian.Uint32(sum[:4])
	return canonicalHomeRoot + "/workspace-" + uint32ToHex(id)
}

// accountCanonicalMemoryPath derives the canonical replacement for the claude-code
// "auto memory" directory. The real path is
// <home>/.claude/projects/<url-encoded-cwd>/memory/, where both the home root and
// the encoded-cwd segment leak the real machine identity. We collapse it onto a
// machine-neutral, per-account path that is derived from the same canonical cwd
// (so it stays consistent with the rewritten Environment cwd) and carries the
// canonical workspace name in the projects segment instead of the real one. The
// result is deterministic for a given account, stable across requests, and free of
// any real user name. canonicalCwd is e.g. "/Users/agent/workspace-<id>"; the
// encoded projects segment mirrors claude-code's own "-"-joined path encoding.
func accountCanonicalMemoryPath(canonicalCwd string) string {
	encoded := strings.ReplaceAll(strings.TrimPrefix(canonicalCwd, "/"), "/", "-")
	return canonicalCwd + "/.claude/projects/-" + encoded + "/memory/"
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
// environment block: the canonical cwd, the canonical "auto memory" path and the
// baseline body OS representation.
type envRewriteParams struct {
	canonicalCwd       string
	canonicalMemoryDir string
	bodyOS             baselineBodyOS
	hasBodyOS          bool
	// collector, when non-nil, records the fake→real cwd mapping captured while
	// rewriting cwd key lines, for response-side restoration. It is never used to
	// alter the outbound rewrite itself.
	collector *CwdRestoreCollector
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
	return normalizeAccountEnv(payload, auth, apiKey, cfg, nil)
}

// NormalizeAccountEnvWithRestore behaves like NormalizeAccountEnv but additionally
// captures the fake→real cwd mapping it applied into the CwdRestoreCollector
// attached to ctx (if any). The captured fake root is the per-account canonical
// cwd; the real root is the working directory probed from the request body. The
// response side later uses this mapping to restore tool-call path arguments. When
// no collector is attached the behavior is identical to NormalizeAccountEnv.
func NormalizeAccountEnvWithRestore(ctx context.Context, payload []byte, auth *cliproxyauth.Auth, apiKey string, cfg *config.Config) []byte {
	return normalizeAccountEnv(payload, auth, apiKey, cfg, CwdRestoreCollectorFromContext(ctx))
}

func normalizeAccountEnv(payload []byte, auth *cliproxyauth.Auth, apiKey string, cfg *config.Config, collector *CwdRestoreCollector) []byte {
	if !gjson.ValidBytes(payload) {
		// Never attempt to rewrite an unparseable body; pass it through (no 400).
		return payload
	}

	canonicalCwd := AccountCanonicalCwd(auth, apiKey)
	params := envRewriteParams{
		canonicalCwd:       canonicalCwd,
		canonicalMemoryDir: accountCanonicalMemoryPath(canonicalCwd),
		collector:          collector,
	}
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
	// CONTROLLED EXCEPTION to the "tool_result is never rewritten" scope rule.
	// The env-block passes above have populated the collector with the exact real
	// cwd literals seen in THIS request (env declaration blocks) and their fake
	// roots. A tool_result for `pwd` / `git rev-parse --show-toplevel` / `ls` / an
	// error message echoes those same real cwds straight back upstream every turn,
	// re-leaking the real machine path/user the env-block rewrite just hid. We close
	// that one channel by rewriting real→fake inside tool_result content, but ONLY
	// for paths that exactly match (by path-prefix) a real cwd the collector already
	// captured. This is safe and minimal: it never does a generalized /Users sweep,
	// only the already-known real→fake mappings are applied, so an unrelated absolute
	// path in a tool_result is left untouched. The model is already fed the fake root,
	// so making the tool_result agree with it improves consistency without breaking
	// the agent. symlink divergence (/tmp vs /private/tmp) is accepted best-effort.
	payload = normalizeToolResultKnownCwds(payload, collector)
	return payload
}

// normalizeToolResultKnownCwds rewrites real→fake cwd literals inside tool_result
// content, restricted to the real cwds the collector captured while rewriting this
// request's env blocks. This is the CONTROLLED exception documented at the call
// site: it is the inverse of the response-side restore (fake→real) and uses the same
// pair set, so only known mappings are touched — never an arbitrary path. When the
// collector is nil or empty (no env block was rewritten, so no real cwd is known)
// it is a no-op and tool_result is left byte-for-byte unchanged.
func normalizeToolResultKnownCwds(payload []byte, collector *CwdRestoreCollector) []byte {
	pairs := collector.Pairs()
	if len(pairs) == 0 {
		return payload
	}
	messages := gjson.GetBytes(payload, "messages")
	if !messages.Exists() || !messages.IsArray() {
		return payload
	}
	messages.ForEach(func(msgKey, msg gjson.Result) bool {
		content := msg.Get("content")
		if !content.Exists() || !content.IsArray() {
			return true
		}
		msgPath := "messages." + msgKey.String()
		content.ForEach(func(blockKey, block gjson.Result) bool {
			if block.Get("type").String() != "tool_result" {
				return true
			}
			// tool_result.content is either a plain string or an array of content
			// blocks (each typically {type:"text", text:...}). Rewrite real→fake only
			// inside those string values; everything else is left untouched.
			tr := block.Get("content")
			blockBase := msgPath + ".content." + blockKey.String()
			if tr.Type == gjson.String {
				rewritten := rewriteKnownRealToFake(pairs, tr.String())
				if rewritten != tr.String() {
					payload, _ = sjson.SetBytes(payload, blockBase+".content", rewritten)
				}
			} else if tr.IsArray() {
				tr.ForEach(func(innerKey, inner gjson.Result) bool {
					t := inner.Get("text")
					if t.Type != gjson.String {
						return true
					}
					rewritten := rewriteKnownRealToFake(pairs, t.String())
					if rewritten != t.String() {
						payload, _ = sjson.SetBytes(payload, blockBase+".content."+innerKey.String()+".text", rewritten)
					}
					return true
				})
			}
			return true
		})
		return true
	})
	return payload
}

// rewriteKnownRealToFake replaces each captured real cwd root with its fake root
// inside s, by path-prefix-safe literal substitution (so "<real>/sub" becomes
// "<fake>/sub"). Only the exact captured real roots are matched; no generalized path
// replacement is performed. Pairs are applied longest-real-first so a more specific
// (longer) real root wins over a shorter one that is its prefix.
func rewriteKnownRealToFake(pairs []CwdRestorePair, s string) string {
	if len(pairs) == 0 || s == "" {
		return s
	}
	ordered := make([]CwdRestorePair, len(pairs))
	copy(ordered, pairs)
	sort.SliceStable(ordered, func(i, j int) bool {
		return len(ordered[i].Real) > len(ordered[j].Real)
	})
	for _, p := range ordered {
		if p.Fake == "" || p.Real == "" || p.Fake == p.Real {
			continue
		}
		s = replaceRealPrefix(s, p.Real, p.Fake)
	}
	return s
}

// replaceRealPrefix replaces every occurrence of the real cwd root with the fake
// root, but only when the match is a whole path segment boundary: the byte after
// the match is either the end of string, a path separator ("/"), or a non-path
// character (whitespace, quote, control char, etc.). This makes "<real>" and
// "<real>/sub" match while "<real>extra" (e.g. /Users/corylin/Project vs
// /Users/corylin/ProjectOther) does NOT, so the substitution stays scoped to the
// captured directory and never bleeds into an unrelated sibling path. This is the
// path-prefix-safe rule the controlled tool_result exception requires; it is
// deliberately stricter than the literal whole-buffer swap the (response-side)
// restore uses.
func replaceRealPrefix(s, real, fake string) string {
	if !strings.Contains(s, real) {
		return s
	}
	var b strings.Builder
	for {
		idx := strings.Index(s, real)
		if idx < 0 {
			b.WriteString(s)
			break
		}
		end := idx + len(real)
		// Boundary check: the char immediately after the match must not be a path
		// continuation char (an unbroken filename byte), otherwise this is a longer
		// unrelated path that merely starts with the real root's text.
		boundary := end >= len(s) || !isPathContinuationByte(s[end])
		b.WriteString(s[:idx])
		if boundary {
			b.WriteString(fake)
		} else {
			b.WriteString(real)
		}
		s = s[end:]
	}
	return b.String()
}

// isPathContinuationByte reports whether c can be part of the same path segment
// immediately following a directory root (i.e. would make "<real>c" a different,
// longer filename rather than a child path). A following "/" is a child path and is
// allowed (treated as a boundary); only bytes that extend the final segment of the
// root itself block the match.
func isPathContinuationByte(c byte) bool {
	switch {
	case c >= 'a' && c <= 'z':
		return true
	case c >= 'A' && c <= 'Z':
		return true
	case c >= '0' && c <= '9':
		return true
	case c == '-' || c == '_' || c == '.':
		return true
	default:
		return false
	}
}

// normalizeEnvText rewrites every environment block (XML or Markdown) found in
// text plus the "# auto memory" path that lives under its own heading, leaving
// everything else untouched.
func normalizeEnvText(text string, params envRewriteParams) string {
	if text == "" {
		return text
	}
	hasXML := strings.Contains(text, "<env>") || strings.Contains(text, "<system-reminder>")
	hasMarkdown := strings.Contains(text, "# Environment")
	// The "auto memory" sentence is matched independently of the env block: it
	// lives under a separate "# auto memory" heading, so it is NOT inside an <env>
	// span or the "# Environment" Markdown block and would otherwise leak through.
	hasMemory := strings.Contains(text, "memory system at")
	if !hasXML && !hasMarkdown && !hasMemory {
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
	if hasMemory {
		// Anchored on the literal "memory system at `...`" phrase: only the
		// backtick-quoted path body (group 2) is replaced, so tool_use args,
		// tool_result content and ordinary conversational paths are never touched.
		text = memoryPathPattern.ReplaceAllString(text, "${1}"+params.canonicalMemoryDir+"${3}")
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
	// Key-anchored cwd normalization first (covers arbitrary path roots). The
	// matched value (group 2) is the real working directory; capture the
	// canonicalCwd→realCwd mapping for response-side restoration before replacing.
	block = cwdKeyValuePattern.ReplaceAllStringFunc(block, func(line string) string {
		m := cwdKeyValuePattern.FindStringSubmatch(line)
		if len(m) == 3 {
			params.collector.Add(params.canonicalCwd, strings.TrimSpace(m[2]))
			return m[1] + params.canonicalCwd
		}
		return line
	})
	// "Additional working directories:" heading + its indented list items. The
	// real paths are on the list items (no key), so cwdKeyValuePattern misses them
	// and the home-only realPathPattern sweep below misses any non-/Users|/home root
	// (e.g. /tmp, /var). Each additional directory is a DISTINCT real path, so each
	// is mapped to its OWN deterministic fake root (derivedFakeWorkspaceRoot) and the
	// fake→real pair recorded for response-side restoration of that directory.
	block = rewriteAdditionalWorkingDirs(block, params)
	// Secondary sweep for embedded home-rooted paths not on a cwd key line. Paths
	// already under canonicalHomeRoot are left alone: they are the canonical cwd and
	// the per-directory derived fake roots produced just above, which must NOT be
	// collapsed onto canonicalCwd (that would destroy the distinct additional-dir
	// mappings the response side needs). canonicalCwd is itself under canonicalHomeRoot,
	// so a re-matched canonical cwd is also (harmlessly) left unchanged.
	block = realPathPattern.ReplaceAllStringFunc(block, func(p string) string {
		if strings.HasPrefix(p, canonicalHomeRoot+"/") || p == canonicalHomeRoot {
			return p
		}
		return params.canonicalCwd
	})
	// OS normalization: align body Platform / OS Version with the baseline OS.
	if params.hasBodyOS {
		block = platformKeyValuePattern.ReplaceAllString(block, "${1}"+params.bodyOS.platform)
		block = osVersionKeyValuePattern.ReplaceAllString(block, "${1}"+params.bodyOS.osVersion)
	}
	return block
}

// rewriteAdditionalWorkingDirs normalizes the directories listed under an
// "Additional working directories:" heading inside an environment block. claude-code
// emits the heading followed by an indented Markdown list, one directory per
// "  - <path>" item; those items carry no key, so the key-anchored cwd pattern does
// not reach them, and the home-only realPathPattern sweep misses non-/Users|/home
// roots (e.g. /tmp, /var). Each item is a DISTINCT real directory, so each gets its
// own deterministic fake root via derivedFakeWorkspaceRoot and a fake→real pair is
// recorded for response-side restoration of that specific directory.
//
// Scope: only the contiguous run of list items immediately following the heading is
// rewritten. The run terminates at the first line that is not an indented list item
// (a blank line, a new key line, or a heading), so unrelated content is untouched.
func rewriteAdditionalWorkingDirs(block string, params envRewriteParams) string {
	loc := additionalDirsHeadingPattern.FindStringIndex(block)
	if loc == nil {
		return block
	}
	head := block[:loc[1]]
	rest := block[loc[1]:]
	// rest begins right after the heading line (before its trailing newline). Walk
	// it line by line, rewriting consecutive list items and stopping at the first
	// non-list-item line.
	var out strings.Builder
	out.WriteString(head)
	i := 0
	stopped := false
	for i < len(rest) {
		nl := strings.IndexByte(rest[i:], '\n')
		var line string
		var sep string
		if nl < 0 {
			line = rest[i:]
			i = len(rest)
		} else {
			line = rest[i : i+nl]
			sep = "\n"
			i += nl + 1
		}
		if !stopped {
			if m := additionalDirItemPattern.FindStringSubmatch(line); len(m) == 3 {
				realPath := strings.TrimSpace(m[2])
				fake := derivedFakeWorkspaceRoot(realPath)
				params.collector.Add(fake, realPath)
				out.WriteString(m[1])
				out.WriteString(fake)
				out.WriteString(sep)
				continue
			}
			if strings.TrimSpace(line) == "" {
				// Blank line (including the empty fragment immediately after the
				// heading, since FindStringIndex stops before the heading's newline)
				// does not terminate the list run; emit it and keep scanning.
				out.WriteString(line)
				out.WriteString(sep)
				continue
			}
			// First non-blank, non-list line ends the additional-directories run;
			// everything after it is emitted verbatim.
			stopped = true
		}
		out.WriteString(line)
		out.WriteString(sep)
	}
	return out.String()
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
