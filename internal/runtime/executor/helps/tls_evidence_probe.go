package helps

import (
	"fmt"
	"net/http"
	"strings"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

// BuildTLSEvidenceProbeRoundTripper builds a diagnostic transport from the
// same account transport/tls profile resolution used by runtime executors.
//
// Claude production runtime only applies uTLS on Anthropic API hosts. TLS echo
// probes intentionally connect to non-provider hosts, so this diagnostic helper
// uses the resolved Claude uTLS ClientHello directly while reporting that host
// override in the returned limitation string.
func BuildTLSEvidenceProbeRoundTripper(proxyURL string, auth *cliproxyauth.Auth) (http.RoundTripper, *RuntimeTransportProfile, string, error) {
	profile := ResolveRuntimeTransportProfile(auth)
	if profile == nil || !profile.SupportsRuntime() {
		return nil, profile, "", fmt.Errorf("runtime transport profile is not configured or unsupported")
	}

	switch profile.Provider {
	case "claude":
		clientHelloProfile := profile.TLSProfileID
		if clientHelloProfile == "" {
			clientHelloProfile = profile.ProfileID
		}
		clientHello, ok := resolveClaudeClientHelloID(clientHelloProfile)
		if !ok {
			clientHello, _ = resolveClaudeClientHelloID("claude_utls_chrome_133")
		}
		limitation := "diagnostic echo probe: reuses resolved Claude uTLS ClientHello on non-Anthropic host; production runtime gates uTLS to api.anthropic.com"
		return newUtlsRoundTripper(proxyURL, clientHello), profile, limitation, nil
	case "codex":
		return NewCodexTransportRoundTripperForProfile(proxyURL, profile.ProfileID, profile.ALPN, profile.ForceHTTP11), profile, "", nil
	default:
		return nil, profile, "", fmt.Errorf("unsupported provider %q", strings.TrimSpace(profile.Provider))
	}
}
