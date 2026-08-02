package executor

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

// This file pins scenario D.1(1): on the REAL serving path (genuine interactive
// claude-cli, helps.ShouldCloak == false) with stabilize-device-profile ON and
// align-real-path-billing-version ON, the OUTBOUND User-Agent header version and
// the OUTBOUND body x-anthropic-billing-header cc_version <version> segment are BOTH
// floored to the SAME account high-water V. The two segments are captured from the
// SAME wire request, so the assertion proves end-to-end header/body CONSISTENCY —
// not merely each side in isolation, which the pre-existing real-path test does
// (it captures only the body). A header floored to V while the body cc_version stays
// at the client's own lower version is the "one account, two versions" tell this
// alignment closes.

// outboundClaudeCLIUAVersionPattern extracts "X.Y.Z" from an outbound
// "claude-cli/X.Y.Z (...)" User-Agent header.
var outboundClaudeCLIUAVersionPattern = regexp.MustCompile(`claude-cli/([0-9]+\.[0-9]+\.[0-9]+)`)

func outboundClaudeCLIUAVersion(t *testing.T, header http.Header) string {
	t.Helper()
	ua := header.Get("User-Agent")
	m := outboundClaudeCLIUAVersionPattern.FindStringSubmatch(ua)
	if len(m) != 2 {
		t.Fatalf("outbound User-Agent %q has no claude-cli/<version> prefix", ua)
	}
	return m[1]
}

// runRealPathServingCapture drives the real Execute serving path (genuine
// interactive claude-cli UA => helps.ShouldCloak=false) against a capturing
// upstream and returns BOTH the outbound request headers and the exact body bytes
// that reach Anthropic, so a test can compare the outbound User-Agent version with
// the body cc_version version on the same wire request. resetClaudeDeviceProfileCache
// clears the shared observation/high-water map so a prior test's global observation
// cannot lift this account's zero-observation floor above V.
func runRealPathServingCapture(t *testing.T, stabilize, align bool, inboundUA, apiKey string, payload []byte) (http.Header, []byte) {
	t.Helper()
	resetClaudeDeviceProfileCache()

	var seenHeader http.Header
	var seenBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenHeader = r.Header.Clone()
		b, _ := io.ReadAll(r.Body)
		seenBody = bytes.Clone(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","model":"claude-3-5-sonnet","role":"assistant","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer server.Close()

	cfg := &config.Config{
		ClaudeHeaderDefaults: config.ClaudeHeaderDefaults{StabilizeDeviceProfile: &stabilize},
		Claude:               config.ClaudeConfig{AlignRealPathBillingVersion: &align},
	}
	executor := NewClaudeExecutor(cfg)
	auth := &cliproxyauth.Auth{ProxyURL: "direct", ID: "realpath-consistency", Attributes: map[string]string{
		"api_key":  apiKey,
		"base_url": server.URL,
	}}
	ctx := ginContextWithUA(inboundUA)
	if _, err := executor.Execute(ctx, auth, cliproxyexecutor.Request{
		Model:   "claude-3-5-sonnet-20241022",
		Payload: payload,
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("claude")}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(seenBody) == 0 {
		t.Fatal("no upstream body captured")
	}
	if seenHeader == nil {
		t.Fatal("no upstream headers captured")
	}
	return seenHeader, seenBody
}

// Scenario D.1(1): a genuine interactive claude-cli at 2.1.209 (BELOW the frozen
// floor 2.1.211) and exactly at the floor (2.1.211) each egress with the outbound
// User-Agent version AND the body cc_version <version> segment BOTH equal to the
// account high-water V (2.1.211). Both are read from the same captured wire request,
// so the test proves header and body AGREE (no "one account, two versions" tell).
func TestClaudeExecutorExecute_RealPathOutboundUAVersionMatchesBodyCCVersionAtHighWater(t *testing.T) {
	cases := []struct {
		name        string
		inboundUA   string
		bodyVersion string // the inbound body's own cc_version (a real client mirrors its UA)
	}{
		{"below floor 2.1.209 floors header+body to V", "claude-cli/2.1.209 (external, cli)", "2.1.209"},
		{"at floor 2.1.211 header+body stay at V", "claude-cli/2.1.211 (external, cli)", "2.1.211"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			payload := billingBodyWithVersion(t, tc.bodyVersion, realPathBuildSeg, "11111")
			header, body := runRealPathServingCapture(t, true /* stabilize */, true /* align */, tc.inboundUA, "sk-ant-oat-consistency", payload)

			uaVersion := outboundClaudeCLIUAVersion(t, header)
			bodyVersion := ccVersionSegment(t, body)

			if uaVersion != realPathHighWater {
				t.Fatalf("outbound UA version = %q, want account high-water V %q", uaVersion, realPathHighWater)
			}
			if bodyVersion != realPathHighWater {
				t.Fatalf("body cc_version version = %q, want account high-water V %q", bodyVersion, realPathHighWater)
			}
			// The load-bearing consistency invariant: the two segments captured from the
			// same wire request are identical.
			if uaVersion != bodyVersion {
				t.Fatalf("outbound header/body version mismatch: UA=%q body cc_version=%q (one-account-two-versions tell)", uaVersion, bodyVersion)
			}
			// The single re-sign covers the aligned body exactly, so the emitted cch is
			// valid over the exact captured bytes (a rewritten body with a stale cch
			// would itself be a detectable tell).
			if emitted, want := cchFromBody(t, body), recomputeExpectedCCH(t, body); emitted != want {
				t.Fatalf("emitted cch %q != recompute over captured body %q", emitted, want)
			}
		})
	}
}

// Negative control / flag-gating guard for scenario D.1(1): with stabilize ON but
// align-real-path-billing-version OFF (the documented default), the outbound UA is
// still floored to V (that is the stabilize device-profile floor, independent of
// this flag) while the body cc_version stays at the client's own lower version —
// i.e. header and body DIVERGE. This is precisely the "one account, two versions"
// tell, and it proves (a) the alignment is what closes it, and (b) the consistency
// test above is not vacuously passing because the body was never floored. It also
// pins that the alignment is gated OFF by default (real path byte-behavior unchanged
// until an operator opts in).
func TestClaudeExecutorExecute_RealPathDefaultOffLeavesHeaderBodyVersionDivergent(t *testing.T) {
	align := false // explicit default-off; nil behaves identically (AlignRealPathBillingVersionEnabled == false)
	payload := billingBodyWithVersion(t, "2.1.209", realPathBuildSeg, "11111")
	header, body := runRealPathServingCapture(t, true /* stabilize */, align, "claude-cli/2.1.209 (external, cli)", "sk-ant-oat-consistency", payload)

	uaVersion := outboundClaudeCLIUAVersion(t, header)
	bodyVersion := ccVersionSegment(t, body)

	if uaVersion != realPathHighWater {
		t.Fatalf("outbound UA version = %q, want stabilize floor V %q (UA floor is independent of the align flag)", uaVersion, realPathHighWater)
	}
	if bodyVersion != "2.1.209" {
		t.Fatalf("body cc_version version = %q, want client version 2.1.209 (align off must leave the body un-floored)", bodyVersion)
	}
	if uaVersion == bodyVersion {
		t.Fatalf("expected header/body divergence with align OFF, but both were %q; the align flag is what unifies them", uaVersion)
	}
}
