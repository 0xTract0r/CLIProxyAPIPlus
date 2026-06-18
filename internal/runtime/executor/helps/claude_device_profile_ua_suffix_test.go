package helps

import (
	"net/http"
	"strings"
	"testing"
)

// TestAlignClaudeDeviceProfileUserAgentSuffix_MirrorsInboundEntrypoint pins the
// anti-correlation invariant: the stabilized outbound UA parenthetical suffix is
// rewritten to mirror the inbound claude-code client's "(USER_TYPE, ENTRYPOINT)"
// block, while the high-water "claude-cli/<version>" prefix is preserved. This is
// the fix for the de-anonymizing mismatch where a frozen device profile seeded by
// "claude --print" emits "(external, sdk-cli)" but cc_entrypoint (derived from the
// same inbound UA) is "cli" — a pair real claude-code never produces.
func TestAlignClaudeDeviceProfileUserAgentSuffix_MirrorsInboundEntrypoint(t *testing.T) {
	cases := []struct {
		name        string
		outboundUA  string
		inboundUA   string
		wantUA      string
		wantEntrypt string
	}{
		{
			name:        "frozen sdk-cli profile realigned to cli inbound keeps high-water version",
			outboundUA:  "claude-cli/2.1.180 (external, sdk-cli)",
			inboundUA:   "claude-cli/2.1.63 (external, cli)",
			wantUA:      "claude-cli/2.1.180 (external, cli)",
			wantEntrypt: "cli",
		},
		{
			name:        "inbound sdk-cli yields sdk-cli outbound suffix",
			outboundUA:  "claude-cli/2.1.180 (external, cli)",
			inboundUA:   "claude-cli/2.1.63 (external, sdk-cli)",
			wantUA:      "claude-cli/2.1.180 (external, sdk-cli)",
			wantEntrypt: "sdk-cli",
		},
		{
			name:        "inbound vscode entrypoint mirrored",
			outboundUA:  "claude-cli/2.1.180 (external, cli)",
			inboundUA:   "claude-cli/2.1.63 (external, vscode)",
			wantUA:      "claude-cli/2.1.180 (external, vscode)",
			wantEntrypt: "vscode",
		},
		{
			name:        "non-claude inbound (api key / curl) falls back to default cli suffix",
			outboundUA:  "claude-cli/2.1.180 (external, sdk-cli)",
			inboundUA:   "curl/8.7.1",
			wantUA:      "claude-cli/2.1.180 (external, cli)",
			wantEntrypt: "cli",
		},
		{
			name:        "empty inbound UA falls back to default cli suffix",
			outboundUA:  "claude-cli/2.1.180 (external, sdk-cli)",
			inboundUA:   "",
			wantUA:      "claude-cli/2.1.180 (external, cli)",
			wantEntrypt: "cli",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &http.Request{Header: http.Header{}}
			r.Header.Set("User-Agent", tc.outboundUA)
			AlignClaudeDeviceProfileUserAgentSuffix(r, tc.inboundUA)
			got := r.Header.Get("User-Agent")
			if got != tc.wantUA {
				t.Fatalf("outbound User-Agent = %q, want %q", got, tc.wantUA)
			}
			// The outbound suffix entrypoint must equal the entrypoint derived from
			// the same inbound UA: outbound suffix == cc_entrypoint must hold.
			gotEntrypt := userAgentEntrypointForTest(got)
			if gotEntrypt != tc.wantEntrypt {
				t.Fatalf("outbound entrypoint = %q, want %q", gotEntrypt, tc.wantEntrypt)
			}
		})
	}
}

// TestAlignClaudeDeviceProfileUserAgentSuffix_NonClaudeOutboundUntouched ensures a
// non-claude outbound UA (e.g. an operator/api-key path that did not emit a
// claude-cli UA) is left as-is so the alignment never fabricates a claude suffix.
func TestAlignClaudeDeviceProfileUserAgentSuffix_NonClaudeOutboundUntouched(t *testing.T) {
	r := &http.Request{Header: http.Header{}}
	r.Header.Set("User-Agent", "my-gateway/1.0")
	AlignClaudeDeviceProfileUserAgentSuffix(r, "claude-cli/2.1.63 (external, cli)")
	if got := r.Header.Get("User-Agent"); got != "my-gateway/1.0" {
		t.Fatalf("non-claude outbound User-Agent = %q, want unchanged", got)
	}
}

// userAgentEntrypointForTest mirrors the executor's parseEntrypointFromUA logic so
// the test can assert "outbound UA suffix == cc_entrypoint" without importing the
// executor package (avoids an import cycle); it is intentionally a tiny copy.
func userAgentEntrypointForTest(userAgent string) string {
	start := strings.Index(userAgent, "(")
	end := strings.LastIndex(userAgent, ")")
	if start < 0 || end <= start {
		return "cli"
	}
	inner := userAgent[start+1 : end]
	parts := strings.Split(inner, ",")
	if len(parts) >= 2 {
		if ep := strings.TrimSpace(parts[1]); ep != "" {
			return ep
		}
	}
	return "cli"
}
