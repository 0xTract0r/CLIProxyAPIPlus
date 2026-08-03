package executor

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	xxHash64 "github.com/pierrec/xxHash/xxHash64"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// This file pins Stage B / Phase C.B1+B4 as REVISED by Stage C real-machine
// validation: on the REAL serving path (genuine interactive claude-cli,
// helps.ShouldCloak == false) the outbound body's x-anthropic-billing-header
// cc_version <version> segment is floored up to the same account high-water
// version V the outbound User-Agent already uses, so a below-high-water client
// cannot emit UA=V + body cc_version=<lower> (a one-account-two-versions tell).
//
// The <build> fingerprint segment is now RECOMPUTED for V (was: passed through
// byte-for-byte). Stage C account-free capture of genuine claude-cli 2.1.220
// proved the build is a deterministic function of (first non-meta user message,
// version) — computeFingerprint — so a client floored from v to V must emit the
// build V produces over the same first user message, not the build it computed
// for v. The billing-header cch is re-signed exactly once so it still covers the
// rewritten body, and the whole rewrite stays INERT by default
// (config.AlignRealPathBillingVersion nil/false) so the real path is
// byte-for-byte unchanged until an operator opts in after real-machine
// validation.

const (
	realPathClientVersion = "2.1.158" // below the frozen floor 2.1.211
	realPathHighWater     = "2.1.211" // defaultClaudeFingerprintUserAgent floor / V
	realPathBuildSeg      = "a1b"     // arbitrary 3-char INPUT build; now recomputed for V, not preserved
)

// ccVersionBuildPattern captures the <build> segment of cc_version=<v>.<build>;.
var ccVersionBuildPattern = regexp.MustCompile(`\bcc_version=[0-9]+\.[0-9]+\.[0-9]+\.([^;]*)`)

// billingBodyWithVersion builds a real-claude-cli-shaped request body whose
// system[0] carries a verbatim inbound billing header at the given version.build,
// plus a distinct system[1], a user message and a tool so the test can assert
// they are left untouched.
func billingBodyWithVersion(t *testing.T, version, build, cch string) []byte {
	t.Helper()
	header := fmt.Sprintf("x-anthropic-billing-header: cc_version=%s.%s; cc_entrypoint=cli; cch=%s;", version, build, cch)
	body := []byte(`{}`)
	var err error
	body, err = sjson.SetBytes(body, "system.0.type", "text")
	if err != nil {
		t.Fatalf("build body: %v", err)
	}
	body, err = sjson.SetBytes(body, "system.0.text", header)
	if err != nil {
		t.Fatalf("build body: %v", err)
	}
	body, err = sjson.SetBytes(body, "system.1.type", "text")
	if err != nil {
		t.Fatalf("build body: %v", err)
	}
	body, err = sjson.SetBytes(body, "system.1.text", "You are a helpful assistant.")
	if err != nil {
		t.Fatalf("build body: %v", err)
	}
	body, err = sjson.SetBytes(body, "messages.0.role", "user")
	if err != nil {
		t.Fatalf("build body: %v", err)
	}
	body, err = sjson.SetBytes(body, "messages.0.content.0.type", "text")
	if err != nil {
		t.Fatalf("build body: %v", err)
	}
	body, err = sjson.SetBytes(body, "messages.0.content.0.text", "please read the file")
	if err != nil {
		t.Fatalf("build body: %v", err)
	}
	body, err = sjson.SetBytes(body, "tools.0.name", "Read")
	if err != nil {
		t.Fatalf("build body: %v", err)
	}
	body, err = sjson.SetBytes(body, "tools.0.description", "read a file")
	if err != nil {
		t.Fatalf("build body: %v", err)
	}
	return body
}

// recomputeExpectedCCH independently reproduces signAnthropicMessagesBody's hash
// step (normalize the billing-header cch to 00000, then xxHash64 the whole body
// with the shared seed) so a test can prove the emitted cch corresponds to the
// exact bytes of the rewritten body under the real algorithm — not merely that
// re-calling the signer is self-consistent.
func recomputeExpectedCCH(t *testing.T, body []byte) string {
	t.Helper()
	header := gjson.GetBytes(body, "system.0.text").String()
	zeroedHeader := claudeBillingHeaderCCHPattern.ReplaceAllString(header, "cch=00000;")
	zeroedBody, err := sjson.SetBytes(body, "system.0.text", zeroedHeader)
	if err != nil {
		t.Fatalf("zero cch: %v", err)
	}
	return fmt.Sprintf("%05x", xxHash64.Checksum(zeroedBody, claudeCCHSeed)&0xFFFFF)
}

func ccVersionSegment(t *testing.T, body []byte) string {
	t.Helper()
	header := gjson.GetBytes(body, "system.0.text").String()
	m := claudeBillingHeaderVersionPattern.FindStringSubmatch(header)
	if m == nil || len(m) < 2 {
		t.Fatalf("no cc_version in header %q", header)
	}
	return m[1]
}

func ccBuildSegment(t *testing.T, body []byte) string {
	t.Helper()
	header := gjson.GetBytes(body, "system.0.text").String()
	// cc_version=<version>.<build>; — capture the build (everything after the
	// third dotted segment up to the terminating ';').
	m := ccVersionBuildPattern.FindStringSubmatch(header)
	if m == nil || len(m) < 2 {
		t.Fatalf("no cc_version build in header %q", header)
	}
	return m[1]
}

// (a) build RECOMPUTE + untouched fields: flag ON, a below-high-water body
// cc_version 2.1.158.<oldBuild> becomes 2.1.211.<recomputedBuild>. BOTH the
// <version> and <build> segments change: the build is recomputed for V over the
// first user message (computeFingerprint), NOT passed through. cc_entrypoint,
// cch, system[1], messages and tools stay byte-identical.
//
// WHY this replaces the old build-passthrough expectation: Stage C account-free
// capture of genuine claude-cli 2.1.220 proved the build is
// computeFingerprint(firstUserMsg, version). A client floored from v→V must emit
// the build V produces over the same first user message; passing through the old
// v-build would leave a v-build under a V-version, itself a mismatch. This is an
// intentional behavior change, not test-weakening.
func TestAlignRealPathBillingVersion_FloorsVersionRecomputesBuildKeepsOtherFields(t *testing.T) {
	enabled := true
	cfg := &config.Config{Claude: config.ClaudeConfig{AlignRealPathBillingVersion: &enabled}}
	in := billingBodyWithVersion(t, realPathClientVersion, realPathBuildSeg, "11111")

	out, changed := alignRealPathBillingVersion(cfg, bytes.Clone(in), realPathHighWater)
	if !changed {
		t.Fatal("expected changed=true when flooring a below-high-water version")
	}

	if got := ccVersionSegment(t, out); got != realPathHighWater {
		t.Fatalf("cc_version version segment = %q, want %q", got, realPathHighWater)
	}
	// The first user message billingBodyWithVersion writes is "please read the
	// file"; the build must be recomputed for V over it (not the input "a1b").
	firstUserMsg, ok := firstNonMetaUserMessageText(out)
	if !ok {
		t.Fatal("captured body has no first user message")
	}
	wantBuild := computeFingerprint(firstUserMsg, realPathHighWater)
	if wantBuild == realPathBuildSeg {
		t.Fatalf("test setup: recomputed build %q coincidentally equals the input build; pick a different fixture", wantBuild)
	}
	if got := ccBuildSegment(t, out); got != wantBuild {
		t.Fatalf("cc_version build segment = %q, want recomputed %q (build must be RECOMPUTED for V, not passed through)", got, wantBuild)
	}

	// Only the cc_version token changed; every other billing field is preserved.
	outHeader := gjson.GetBytes(out, "system.0.text").String()
	if !strings.Contains(outHeader, "cc_entrypoint=cli;") {
		t.Fatalf("cc_entrypoint not preserved: %q", outHeader)
	}
	if !strings.Contains(outHeader, "cch=11111;") {
		t.Fatalf("cch must be preserved by align (re-sign is a separate step): %q", outHeader)
	}

	// Everything outside system[0] is untouched.
	if a, b := gjson.GetBytes(out, "system.1.text").String(), gjson.GetBytes(in, "system.1.text").String(); a != b {
		t.Fatalf("system[1] changed: %q != %q", a, b)
	}
	if a, b := gjson.GetBytes(out, "messages").Raw, gjson.GetBytes(in, "messages").Raw; a != b {
		t.Fatalf("messages changed:\n%s\n%s", a, b)
	}
	if a, b := gjson.GetBytes(out, "tools").Raw, gjson.GetBytes(in, "tools").Raw; a != b {
		t.Fatalf("tools changed:\n%s\n%s", a, b)
	}
}

// (b) cch covers the rewritten body exactly once: after align + a single
// signAnthropicMessagesBody, the emitted cch equals an independent xxHash64
// recompute over the rewritten body, differs from the recompute over the
// original (un-floored) body (so it is bound to the rewritten version, not the
// old one), and a second sign is idempotent (proving exactly-once semantics).
func TestAlignRealPathBillingVersion_ReSignCoversRewrittenBodyExactlyOnce(t *testing.T) {
	enabled := true
	cfg := &config.Config{Claude: config.ClaudeConfig{AlignRealPathBillingVersion: &enabled}}
	in := billingBodyWithVersion(t, realPathClientVersion, realPathBuildSeg, "11111")

	aligned, changed := alignRealPathBillingVersion(cfg, bytes.Clone(in), realPathHighWater)
	if !changed {
		t.Fatal("expected changed=true")
	}
	// Mirror the executor: exactly one re-sign after the alignment.
	signed := signAnthropicMessagesBody(aligned)

	emitted := cchFromBody(t, signed)
	if wantOverRewritten := recomputeExpectedCCH(t, signed); emitted != wantOverRewritten {
		t.Fatalf("emitted cch %q != xxHash64 over rewritten body %q", emitted, wantOverRewritten)
	}
	// Bound to the rewritten version, not the original: the cch must NOT equal the
	// hash of the pre-floor (2.1.158) body.
	if overOriginal := recomputeExpectedCCH(t, in); emitted == overOriginal {
		t.Fatalf("emitted cch %q matches the un-floored body hash %q; cch is not bound to the rewritten version", emitted, overOriginal)
	}
	// Exactly-once: re-signing the already-signed body is idempotent.
	if reSigned := cchFromBody(t, signAnthropicMessagesBody(signed)); reSigned != emitted {
		t.Fatalf("signing not idempotent: %q -> %q", emitted, reSigned)
	}
}

// (c) default-safe: the switch is off by default (nil pointer) and when
// explicitly disabled — the body is returned byte-identical with changed=false.
func TestAlignRealPathBillingVersion_DisabledIsByteIdentical(t *testing.T) {
	in := billingBodyWithVersion(t, realPathClientVersion, realPathBuildSeg, "11111")

	disabled := false
	cases := map[string]*config.Config{
		"nil cfg":                nil,
		"zero-value cfg (unset)": {},
		"explicitly disabled":    {Claude: config.ClaudeConfig{AlignRealPathBillingVersion: &disabled}},
	}
	for name, cfg := range cases {
		t.Run(name, func(t *testing.T) {
			out, changed := alignRealPathBillingVersion(cfg, bytes.Clone(in), realPathHighWater)
			if changed {
				t.Fatal("expected changed=false when the switch is off")
			}
			if !bytes.Equal(out, in) {
				t.Fatalf("body must be byte-identical when off.\n got: %s\nwant: %s", out, in)
			}
		})
	}
}

// (d) idempotent for a genuine client already at V: when the body is already at
// V AND its build already equals the recompute over the first user message (i.e.
// a genuine client that computed the build the same way), align yields
// changed=false and a byte-identical body (no redundant re-sign is forced).
//
// WHY the fixture build changed from an arbitrary "a1b" to the recomputed value:
// with build RECOMPUTE, "already at V" alone is no longer a no-op — a client
// claiming V with a non-genuine build gets its build corrected. True idempotency
// now requires the build to already match computeFingerprint(firstUserMsg, V),
// which is exactly what a genuine client at V emits.
func TestAlignRealPathBillingVersion_IdempotentAtHighWaterForGenuineBuild(t *testing.T) {
	enabled := true
	cfg := &config.Config{Claude: config.ClaudeConfig{AlignRealPathBillingVersion: &enabled}}
	// billingBodyWithVersion writes first user message "please read the file"; use
	// the build a genuine client at V would compute over it.
	genuineBuild := computeFingerprint("please read the file", realPathHighWater)
	in := billingBodyWithVersion(t, realPathHighWater, genuineBuild, "abcde")

	out, changed := alignRealPathBillingVersion(cfg, bytes.Clone(in), realPathHighWater)
	if changed {
		t.Fatal("expected changed=false when already at V with a build that reproduces")
	}
	if !bytes.Equal(out, in) {
		t.Fatalf("already-at-V genuine body must be byte-identical.\n got: %s\nwant: %s", out, in)
	}
}

// (e) safe no-op on edge cases: empty V, a billing header with no cc_version
// token, a non-billing system[0], and a malformed / non-JSON body must each
// return the input unchanged with changed=false and never panic.
func TestAlignRealPathBillingVersion_SafeNoOpEdgeCases(t *testing.T) {
	enabled := true
	cfg := &config.Config{Claude: config.ClaudeConfig{AlignRealPathBillingVersion: &enabled}}

	noVersion := []byte(`{"system":[{"type":"text","text":"x-anthropic-billing-header: cc_entrypoint=cli; cch=11111;"}],"messages":[]}`)
	notBilling := []byte(`{"system":[{"type":"text","text":"You are Claude."}],"messages":[]}`)
	noSystem := []byte(`{"messages":[{"role":"user","content":"hi"}]}`)
	malformed := []byte(`{"system":[{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1.158.a1b;`) // truncated / invalid JSON
	stringSystem := []byte(`{"system":"plain string system","messages":[]}`)

	cases := map[string]struct {
		body    []byte
		version string
	}{
		"empty V":                        {billingBodyWithVersion(t, realPathClientVersion, realPathBuildSeg, "11111"), ""},
		"billing header no cc_version":   {noVersion, realPathHighWater},
		"system[0] not a billing header": {notBilling, realPathHighWater},
		"no system":                      {noSystem, realPathHighWater},
		"malformed non-JSON body":        {malformed, realPathHighWater},
		"string system":                  {stringSystem, realPathHighWater},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			out, changed := alignRealPathBillingVersion(cfg, bytes.Clone(tc.body), tc.version)
			if changed {
				t.Fatalf("expected changed=false for edge case %q", name)
			}
			if !bytes.Equal(out, tc.body) {
				t.Fatalf("edge case %q must be a byte-identical no-op.\n got: %s\nwant: %s", name, out, tc.body)
			}
		})
	}
}

// --- Real Execute / ExecuteStream serving-path integration ---------------------

// runRealPathExecuteCapture drives the real Execute serving path (claude-cli UA
// => ShouldCloak=false) against a capturing upstream and returns the exact bytes
// that would reach Anthropic.
func runRealPathExecuteCapture(t *testing.T, align *bool, apiKey string, payload []byte) []byte {
	t.Helper()
	resetClaudeDeviceProfileCache()
	var seen []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		seen = bytes.Clone(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","model":"claude-3-5-sonnet","role":"assistant","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer server.Close()

	cfg := &config.Config{Claude: config.ClaudeConfig{AlignRealPathBillingVersion: align}}
	executor := NewClaudeExecutor(cfg)
	auth := &cliproxyauth.Auth{ProxyURL: "direct", ID: "align-real-path-exec", Attributes: map[string]string{
		"api_key":  apiKey,
		"base_url": server.URL,
	}}
	ctx := ginContextWithUA("claude-cli/" + realPathClientVersion + " (external, cli)")
	if _, err := executor.Execute(ctx, auth, cliproxyexecutor.Request{
		Model:   "claude-3-5-sonnet-20241022",
		Payload: payload,
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("claude")}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(seen) == 0 {
		t.Fatal("no upstream body captured")
	}
	return seen
}

// Flag ON, genuine OAuth claude-cli, below-high-water inbound billing header:
// the upstream body cc_version is floored to V, the build is preserved, the cch
// is valid over the rewritten body (exactly-once), and the user text / system[1]
// survive.
func TestClaudeExecutorExecute_RealPathFloorsBodyBillingVersionWhenEnabled(t *testing.T) {
	enabled := true
	payload := billingBodyWithVersion(t, realPathClientVersion, realPathBuildSeg, "11111")
	seen := runRealPathExecuteCapture(t, &enabled, "sk-ant-oat-real-path", payload)

	header := gjson.GetBytes(seen, "system.0.text").String()
	if !strings.HasPrefix(header, "x-anthropic-billing-header:") {
		t.Fatalf("upstream system.0.text is not a billing header: %q\nbody: %s", header, seen)
	}
	if got := ccVersionSegment(t, seen); got != realPathHighWater {
		t.Fatalf("upstream cc_version = %q, want floored %q", got, realPathHighWater)
	}
	// Build is RECOMPUTED for V over the first user message as it appears in the
	// final captured body (robust to any message transformation on the path).
	firstUserMsg, ok := firstNonMetaUserMessageText(seen)
	if !ok {
		t.Fatal("captured upstream body has no first user message")
	}
	if got, want := ccBuildSegment(t, seen), computeFingerprint(firstUserMsg, realPathHighWater); got != want {
		t.Fatalf("upstream cc_version build = %q, want recomputed %q", got, want)
	}
	// cch covers the rewritten upstream body under the real xxHash64 algorithm.
	if emitted, want := cchFromBody(t, seen), recomputeExpectedCCH(t, seen); emitted != want {
		t.Fatalf("upstream cch %q != recompute over rewritten body %q", emitted, want)
	}
	if strings.Contains(header, "cch=11111;") {
		t.Fatalf("stale client cch forwarded verbatim: %q", header)
	}
	// System instructions / user content preserved (real path: no cloak injection).
	if got := gjson.GetBytes(seen, "system.1.text").String(); got != "You are a helpful assistant." {
		t.Fatalf("system[1] not preserved: %q", got)
	}
	if got := gjson.GetBytes(seen, "messages.0.content.0.text").String(); got != "please read the file" {
		t.Fatalf("user message text not preserved: %q", got)
	}
}

// Flag OFF (default): the real path is inert — the upstream body cc_version
// stays at the client version (today's behavior), proving the rewrite is gated.
func TestClaudeExecutorExecute_RealPathBillingVersionInertWhenDisabled(t *testing.T) {
	seen := runRealPathExecuteCapture(t, nil /* unset => default off */, "sk-ant-oat-real-path", billingBodyWithVersion(t, realPathClientVersion, realPathBuildSeg, "11111"))

	if got := ccVersionSegment(t, seen); got != realPathClientVersion {
		t.Fatalf("flag off must leave cc_version at the client version; got %q want %q", got, realPathClientVersion)
	}
	if got := ccBuildSegment(t, seen); got != realPathBuildSeg {
		t.Fatalf("build segment changed while flag off: %q", got)
	}
}

// Streaming serving path parity: the same real-path floor + valid cch applies on
// ExecuteStream (the main conversation stream path).
func TestClaudeExecutorExecuteStream_RealPathFloorsBodyBillingVersionWhenEnabled(t *testing.T) {
	resetClaudeDeviceProfileCache()
	enabled := true
	var seen []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		seen = bytes.Clone(b)
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		_, _ = w.Write([]byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"model\":\"claude-3-5-sonnet\"}}\n\n"))
		_, _ = w.Write([]byte("event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":1}}\n\n"))
		_, _ = w.Write([]byte("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"))
		if flusher != nil {
			flusher.Flush()
		}
	}))
	defer server.Close()

	cfg := &config.Config{Claude: config.ClaudeConfig{AlignRealPathBillingVersion: &enabled}}
	executor := NewClaudeExecutor(cfg)
	auth := &cliproxyauth.Auth{ProxyURL: "direct", ID: "align-real-path-stream", Attributes: map[string]string{
		"api_key":  "sk-ant-oat-real-path-stream",
		"base_url": server.URL,
	}}
	ctx := ginContextWithUA("claude-cli/" + realPathClientVersion + " (external, cli)")
	result, err := executor.ExecuteStream(ctx, auth, cliproxyexecutor.Request{
		Model:   "claude-3-5-sonnet-20241022",
		Payload: billingBodyWithVersion(t, realPathClientVersion, realPathBuildSeg, "11111"),
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("claude")})
	if err != nil {
		t.Fatalf("ExecuteStream: %v", err)
	}
	if result != nil {
		for range result.Chunks {
		}
	}
	if len(seen) == 0 {
		t.Fatal("no upstream body captured")
	}

	if got := ccVersionSegment(t, seen); got != realPathHighWater {
		t.Fatalf("stream upstream cc_version = %q, want floored %q", got, realPathHighWater)
	}
	// Build is RECOMPUTED for V over the first user message in the final body.
	firstUserMsg, ok := firstNonMetaUserMessageText(seen)
	if !ok {
		t.Fatal("captured stream upstream body has no first user message")
	}
	if got, want := ccBuildSegment(t, seen), computeFingerprint(firstUserMsg, realPathHighWater); got != want {
		t.Fatalf("stream upstream build = %q, want recomputed %q", got, want)
	}
	if emitted, want := cchFromBody(t, seen), recomputeExpectedCCH(t, seen); emitted != want {
		t.Fatalf("stream upstream cch %q != recompute over rewritten body %q", emitted, want)
	}
}

// (f) cloaked-path guard: with ShouldCloak=true (non-claude-cli UA), applyCloaking
// still performs its full existing behavior — it injects the Claude Code system
// blocks (billing header + "You are Claude Code..." agent block) and floors the
// injected cc_version to V. This change never touches that path; the assertion is
// a regression guard, not a rewrite of cloak logic.
func TestApplyCloaking_CloakedPathStillInjectsAndFloors(t *testing.T) {
	resetClaudeDeviceProfileCache()
	cfg := &config.Config{AuthDir: t.TempDir()}
	auth := &cliproxyauth.Auth{ProxyURL: "direct", FileName: "cloak-guard.json", Attributes: map[string]string{"api_key": "key-cloak"}}
	// A non-claude-cli client under the default "auto" mode => ShouldCloak=true.
	payload := []byte(`{"system":"third party system","messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`)
	ctx := ctxWithUserAgent("some-third-party-agent/1.0")

	out := applyCloaking(ctx, cfg, auth, payload, "claude-3-5-sonnet-20241022", "key-cloak", realPathHighWater)

	system := gjson.GetBytes(out, "system")
	if !system.IsArray() {
		t.Fatalf("cloaked path must inject the Claude Code system block array, got: %s", system.Raw)
	}
	header := gjson.GetBytes(out, "system.0.text").String()
	if !strings.HasPrefix(header, "x-anthropic-billing-header:") {
		t.Fatalf("cloaked path must inject a billing header at system[0], got %q", header)
	}
	if got := ccVersionSegment(t, out); got != realPathHighWater {
		t.Fatalf("cloaked-path injected cc_version = %q, want %q", got, realPathHighWater)
	}
	if got := gjson.GetBytes(out, "system.1.text").String(); got != "You are Claude Code, Anthropic's official CLI for Claude." {
		t.Fatalf("cloaked path must inject the Claude Code agent block at system[1], got %q", got)
	}
}
