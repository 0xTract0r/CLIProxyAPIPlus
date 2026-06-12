package helps

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"testing"

	tls "github.com/refraction-networking/utls"
)

// expectedClaudeCLIJA3 is the structural JA3 string of the real claude-cli
// ClientHello captured against an IP (no SNI), per
// docs/fingerprint/cpa-reqs/03-tls-target.md. Its md5 is the replication
// target. The production spec additionally adds server_name(0) for real-host
// handshakes, which is excluded here when validating the structural match.
const (
	expectedClaudeCLIJA3    = "771,4865-4866-4867-49195-49199-49196-49200-52393-52392-49161-49171-49162-49172-156-157-47-53,23-65281-10-11-35-16-5-13-18-51-45-43,29-23-24,0"
	expectedClaudeCLIJA3MD5 = "e97f5146a7009cc2918b50e903b6ff8d"
)

func TestResolveClaudeClientHelloIDCustomProfile(t *testing.T) {
	// P1.5: the new profile resolves to HelloCustom.
	for _, id := range []string{
		"claude_cli_clienthello_v1",
		"claude_cli_clienthello",
		"claude_node_openssl_v1",
		"CLAUDE_CLI_CLIENTHELLO_V1",
	} {
		got, ok := resolveClaudeClientHelloID(id)
		if !ok {
			t.Fatalf("resolveClaudeClientHelloID(%q) not resolved", id)
		}
		if got.Str() != tls.HelloCustom.Str() {
			t.Fatalf("resolveClaudeClientHelloID(%q) = %s, want HelloCustom", id, got.Str())
		}
	}

	// The prior Chrome presets remain resolvable for backward compatibility.
	if got, ok := resolveClaudeClientHelloID("claude_utls_chrome_133"); !ok || got.Str() != tls.HelloChrome_133.Str() {
		t.Fatalf("claude_utls_chrome_133 = (%v, %v), want HelloChrome_133", got.Str(), ok)
	}
}

func TestClaudeCLIClientHelloProfileResolvesToCustom(t *testing.T) {
	// P1.6: the replicated claude-cli profile resolves to HelloCustom. This
	// profile is wired into the claude->anthropic core-managed default outbound
	// path (transport_profile.go), not into NewUtlsHTTPClient.
	if claudeCLIClientHelloProfileID != "claude_cli_clienthello_v1" {
		t.Fatalf("claude-cli profile id = %q", claudeCLIClientHelloProfileID)
	}
	got, ok := resolveClaudeClientHelloID(claudeCLIClientHelloProfileID)
	if !ok || got.Str() != tls.HelloCustom.Str() {
		t.Fatalf("claude-cli profile resolves to %s, want HelloCustom", got.Str())
	}
}

// TestNewUtlsHTTPClientDefaultDoesNotUseClaudeCLIClientHello guards the B1
// regression: NewUtlsHTTPClient is consumed only by the codex executor
// (chatgpt.com), so its default must stay the Chrome-like preset and must NOT
// emit the claude-cli (HelloCustom) ClientHello, which would misrepresent the
// codex client.
func TestNewUtlsHTTPClientDefaultDoesNotUseClaudeCLIClientHello(t *testing.T) {
	if utlsHTTPClientDefaultProfileID == claudeCLIClientHelloProfileID {
		t.Fatalf("codex-facing NewUtlsHTTPClient default = %q, must not be the claude-cli ClientHello profile", utlsHTTPClientDefaultProfileID)
	}
	got, ok := resolveClaudeClientHelloID(utlsHTTPClientDefaultProfileID)
	if !ok {
		t.Fatalf("codex-facing default profile %q does not resolve", utlsHTTPClientDefaultProfileID)
	}
	if got.Str() == tls.HelloCustom.Str() {
		t.Fatalf("codex-facing default profile %q resolves to HelloCustom (claude-cli); want Chrome-like", utlsHTTPClientDefaultProfileID)
	}
	if got.Str() != tls.HelloChrome_133.Str() {
		t.Fatalf("codex-facing default ClientHello = %s, want HelloChrome_133", got.Str())
	}

	client := NewUtlsHTTPClient(context.Background(), nil, nil, 0)
	fallback, ok := client.Transport.(*fallbackRoundTripper)
	if !ok {
		t.Fatalf("NewUtlsHTTPClient transport = %T, want *fallbackRoundTripper", client.Transport)
	}
	utlsRT, ok := fallback.utls.(*utlsRoundTripper)
	if !ok {
		t.Fatalf("NewUtlsHTTPClient protected transport = %T, want *utlsRoundTripper", fallback.utls)
	}
	if utlsRT.clientHello.Str() == tls.HelloCustom.Str() {
		t.Fatalf("NewUtlsHTTPClient ClientHello = HelloCustom (claude-cli); codex outbound must not use it")
	}
}

func TestClaudeCLIClientHelloSpecAppliesAndMatchesJA3(t *testing.T) {
	// P1.5: the custom ClientHello spec builds and is applicable via ApplyPreset.
	spec, err := newClaudeCLIClientHelloSpec()
	if err != nil {
		t.Fatalf("newClaudeCLIClientHelloSpec: %v", err)
	}

	uConn := tls.UClient(nil, &tls.Config{ServerName: "api.anthropic.com"}, tls.HelloCustom)
	if errApply := uConn.ApplyPreset(spec); errApply != nil {
		t.Fatalf("ApplyPreset: %v", errApply)
	}

	// P1.6: ALPN advertises only http/1.1.
	alpn := alpnProtocols(spec)
	if len(alpn) != 1 || alpn[0] != "http/1.1" {
		t.Fatalf("ALPN = %v, want [http/1.1]", alpn)
	}

	// No GREASE in ciphers or extensions (Node/OpenSSL form).
	for _, c := range spec.CipherSuites {
		if isGREASEClientHello(c) {
			t.Fatalf("cipher suite list contains GREASE value 0x%04x", c)
		}
	}

	// Structural JA3 (excluding the SNI(0) extension that is only added for
	// real-host handshakes) must equal the captured target md5.
	ja3 := buildStructuralJA3(t, spec)
	if ja3 != expectedClaudeCLIJA3 {
		t.Fatalf("JA3 string mismatch:\n got: %s\nwant: %s", ja3, expectedClaudeCLIJA3)
	}
	sum := md5.Sum([]byte(ja3))
	if got := hex.EncodeToString(sum[:]); got != expectedClaudeCLIJA3MD5 {
		t.Fatalf("JA3 md5 = %s, want %s", got, expectedClaudeCLIJA3MD5)
	}
}

func alpnProtocols(spec *tls.ClientHelloSpec) []string {
	for _, ext := range spec.Extensions {
		if alpn, ok := ext.(*tls.ALPNExtension); ok {
			return alpn.AlpnProtocols
		}
	}
	return nil
}

func isGREASEClientHello(v uint16) bool {
	return (v&0x0f0f) == 0x0a0a && (v>>8) == (v&0xff)
}

// buildStructuralJA3 derives the JA3 string from the spec, excluding GREASE and
// the SNI(0) extension (which the spec adds only for real-host handshakes,
// while the captured target was taken against an IP without SNI).
func buildStructuralJA3(t *testing.T, spec *tls.ClientHelloSpec) string {
	t.Helper()

	const tlsVersion = 771 // TLS 1.2 record version (0x0303)

	ciphers := make([]string, 0, len(spec.CipherSuites))
	for _, c := range spec.CipherSuites {
		if isGREASEClientHello(c) {
			continue
		}
		ciphers = append(ciphers, strconv.Itoa(int(c)))
	}

	exts := make([]string, 0, len(spec.Extensions))
	var curves, points []string
	for _, ext := range spec.Extensions {
		id, ok := clientHelloExtensionID(ext)
		if !ok {
			t.Fatalf("unmapped extension type %T", ext)
		}
		if isGREASEClientHello(id) {
			continue
		}
		if id == 0 { // server_name: only present for real-host handshakes
			continue
		}
		exts = append(exts, strconv.Itoa(int(id)))

		switch e := ext.(type) {
		case *tls.SupportedCurvesExtension:
			for _, cv := range e.Curves {
				if isGREASEClientHello(uint16(cv)) {
					continue
				}
				curves = append(curves, strconv.Itoa(int(cv)))
			}
		case *tls.SupportedPointsExtension:
			for _, p := range e.SupportedPoints {
				points = append(points, strconv.Itoa(int(p)))
			}
		}
	}

	return fmt.Sprintf("%d,%s,%s,%s,%s",
		tlsVersion,
		strings.Join(ciphers, "-"),
		strings.Join(exts, "-"),
		strings.Join(curves, "-"),
		strings.Join(points, "-"),
	)
}

// clientHelloExtensionID maps a uTLS extension struct to its IANA extension
// number, for the extensions used by the claude-cli ClientHello spec.
func clientHelloExtensionID(ext tls.TLSExtension) (uint16, bool) {
	switch ext.(type) {
	case *tls.SNIExtension:
		return 0, true
	case *tls.StatusRequestExtension:
		return 5, true
	case *tls.SupportedCurvesExtension:
		return 10, true
	case *tls.SupportedPointsExtension:
		return 11, true
	case *tls.SignatureAlgorithmsExtension:
		return 13, true
	case *tls.ALPNExtension:
		return 16, true
	case *tls.SCTExtension:
		return 18, true
	case *tls.ExtendedMasterSecretExtension:
		return 23, true
	case *tls.SessionTicketExtension:
		return 35, true
	case *tls.SupportedVersionsExtension:
		return 43, true
	case *tls.PSKKeyExchangeModesExtension:
		return 45, true
	case *tls.KeyShareExtension:
		return 51, true
	case *tls.RenegotiationInfoExtension:
		return 65281, true
	default:
		return 0, false
	}
}
